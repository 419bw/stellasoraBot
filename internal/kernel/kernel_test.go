package kernel

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
)

// ---------- 压测用 Sink：记录送达、注入延迟与失败 ----------

type loadSink struct {
	mu       sync.Mutex
	attempts int
	records  []sendRecord
	ids      map[string]int
}

type sendRecord struct {
	at     time.Time
	target string
	n      int
}

func newLoadSink() *loadSink {
	return &loadSink{ids: make(map[string]int)}
}

// failEvery 非零时，每第 N 次 Send 返回一次临时错误。
// 注意：真实网络下"返回失败但其实已送达"是可能发生的，这里不模拟那种情况；
// 因此"恰好一次"的断言只在进程内成立，跨网络要靠 msg_id 幂等兜。
func (s *loadSink) Send(ctx context.Context, b *queue.Batch, failEvery int, latency time.Duration) error {
	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if failEvery > 0 && s.attempts%failEvery == 0 {
		return errors.New("loadSink: 临时失败")
	}
	s.records = append(s.records, sendRecord{at: time.Now(), target: b.Target, n: len(b.Items)})
	for _, it := range b.Items {
		s.ids[it.ID]++
	}
	return nil
}

func (s *loadSink) snapshot() ([]sendRecord, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := append([]sendRecord(nil), s.records...)
	ids := make(map[string]int, len(s.ids))
	for k, v := range s.ids {
		ids[k] = v
	}
	return recs, ids
}

type flakySink struct {
	sink      *loadSink
	failEvery int
	latency   time.Duration
}

func (f flakySink) Send(ctx context.Context, b *queue.Batch) error {
	return f.sink.Send(ctx, b, f.failEvery, f.latency)
}

// ---------- 功能接缝 ----------

func TestFeatureSeamWiresThrough(t *testing.T) {
	sink := newLoadSink()
	p := queue.DefaultPolicy()
	p.MergeWindow = time.Millisecond
	r := NewRuntime(flakySink{sink: sink}, p, calendar.NewStore(), nil)

	ran := make(chan struct{})
	r.Register(NewFeature("demo", func(ctx context.Context, api API) error {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			if err := api.Submit(queue.Item{ID: "direct", Target: "g1", Text: "立即"}); err != nil {
				t.Errorf("Submit: %v", err)
			}
			api.Schedule("reminder-1", time.Now().Add(60*time.Millisecond), func(context.Context) error {
				close(ran)
				return api.Submit(queue.Item{ID: "timed", Target: "g1", Text: "定时"})
			})
		}()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("调度器没有触发功能注册的任务")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ids := sink.snapshot(); len(ids) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, ids := sink.snapshot()
	for _, want := range []string{"direct", "timed"} {
		if ids[want] != 1 {
			t.Errorf("消息 %q 送达 %d 次, want 1", want, ids[want])
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run 返回意外错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Run 未退出")
	}
}

func TestFeatureStartErrorAbortsRun(t *testing.T) {
	r := NewRuntime(flakySink{sink: newLoadSink()}, queue.DefaultPolicy(), calendar.NewStore(), nil)
	r.Register(NewFeature("broken", func(context.Context, API) error {
		return errors.New("配置缺失")
	}))
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "配置缺失") {
		t.Errorf("Run 错误 = %v, 期望包含功能名与原因", err)
	}
}

// ---------- 压测 ----------

// 场景一：额度充足时灌 3000 条，要求零丢失、零重复、协程回落基线。
func TestFloodNoLossNoDuplicate(t *testing.T) {
	before := goroutines(t)

	const groups = 40
	const perGroup = 75 // 共 3000 条
	sink := newLoadSink()

	p := queue.DefaultPolicy()
	p.MergeWindow = time.Millisecond
	p.TargetPerMin = 60000
	p.TargetBurst = 500
	p.GlobalPerMin = 60000
	p.GlobalBurst = 500
	p.TargetPerDay = 0
	p.QueueSize = 8192

	r := NewRuntime(flakySink{sink: sink, latency: 200 * time.Microsecond}, p, calendar.NewStore(), nil)
	r.Register(NewFeature("flood", func(ctx context.Context, api API) error {
		go func() {
			for g := 0; g < groups; g++ {
				target := fmt.Sprintf("g%d", g)
				for i := 0; i < perGroup; i++ {
					id := fmt.Sprintf("%s-%d", target, i)
					for {
						if err := api.Submit(queue.Item{ID: id, Target: target, Text: id}); err == nil {
							break
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}
			}
		}()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	want := groups * perGroup
	deadline := time.Now().Add(30 * time.Second)
	st := r.Stats()
	for time.Now().Before(deadline) {
		st = r.Stats()
		if st.Sent == want {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("场景一：Sent=%d Batches=%d Retries=%d Rejected=%d 积压=%d in-flight=%d",
		st.Sent, st.Batches, st.Retries, st.Rejected, r.Backlog(), r.InFlight())
	assertConservation(t, want, st, r.Backlog())

	_, ids := sink.snapshot()
	if len(ids) != want {
		t.Fatalf("送达去重后 %d 条, want %d（丢失）", len(ids), want)
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("消息 %s 送达 %d 次, want 1（重复）", id, n)
		}
	}
	if st.Sent != want {
		t.Errorf("Stats.Sent = %d, want %d", st.Sent, want)
	}

	cancel()
	<-done
	waitGoroutines(t, before)
}

// 注入故障时不再要求零丢失，但要求"每一条都有交代"：
// 要么送达，要么被显式计入丢弃，且任何一条都不能重复送达。
func TestFloodWithFailuresIsFullyAccounted(t *testing.T) {
	const groups, perGroup = 30, 40 // 1200 条
	sink := newLoadSink()

	p := generousPolicy()
	p.MaxAttempts = 4
	r := NewRuntime(flakySink{sink: sink, failEvery: 7, latency: time.Millisecond}, p, calendar.NewStore(), nil)
	r.Register(NewFeature("flood-fail", func(ctx context.Context, api API) error {
		go func() {
			for g := 0; g < groups; g++ {
				target := fmt.Sprintf("g%d", g)
				for i := 0; i < perGroup; i++ {
					id := fmt.Sprintf("%s-%d", target, i)
					for {
						if err := api.Submit(queue.Item{ID: id, Target: target, Text: id}); err == nil {
							break
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}
			}
		}()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	want := groups * perGroup
	deadline := time.Now().Add(30 * time.Second)
	var st queue.Stats
	for time.Now().Before(deadline) {
		st = r.Stats()
		if st.Sent+st.DroppedFailed >= want && r.Backlog() == 0 && r.InFlight() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	recs, ids := sink.snapshot()
	t.Logf("故障注入：Sent=%d DroppedFailed=%d Retries=%d sink发送次数=%d 去重送达=%d",
		st.Sent, st.DroppedFailed, st.Retries, len(recs), len(ids))

	if st.Sent != len(ids) {
		t.Errorf("Stats.Sent=%d 与 sink 实收 %d 不一致", st.Sent, len(ids))
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("消息 %s 送达 %d 次，重试路径造成重复发送", id, n)
		}
	}
	assertConservation(t, want, st, r.Backlog())
	// 瞬时失败应当被重试全部吸收；只有在重试耗尽时才允许丢，
	// 那条路径由 queue 包的 TestPermanentFailureStopsAfterMaxAttempts 专门覆盖。
	if st.Retries == 0 {
		t.Error("注入了 1/7 失败率却没有任何重试，失败注入没生效")
	}
	// 每条消息最多尝试 MaxAttempts 次，发送调用总数必须有上界
	if len(recs) > want*p.MaxAttempts {
		t.Errorf("Send 调用 %d 次，超过 %d 条 × %d 次的上界，重试失控", len(recs), want, p.MaxAttempts)
	}
}

// 场景二：直接用平台真实配额灌爆，要求不越权、不丢账、积压有界。
func TestPlatformQuotaUnderFlood(t *testing.T) {
	before := goroutines(t)

	const groups = 50
	const perGroup = 30 // 共 1500 条，远超 60/qpm 的全局额度
	sink := newLoadSink()

	p := queue.DefaultPolicy() // 单群 20/qpm、全局 60/qpm、每群每天 1000
	p.MergeWindow = time.Millisecond
	p.Deadline = time.Second // 发不出去的很快就该被丢弃
	p.QueueSize = 8192

	r := NewRuntime(flakySink{sink: sink}, p, calendar.NewStore(), nil)
	r.Register(NewFeature("quota-flood", func(ctx context.Context, api API) error {
		go func() {
			for g := 0; g < groups; g++ {
				target := fmt.Sprintf("g%d", g)
				for i := 0; i < perGroup; i++ {
					id := fmt.Sprintf("%s-%d", target, i)
					for {
						if err := api.Submit(queue.Item{ID: id, Target: target, Text: id}); err == nil {
							break
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}
			}
		}()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// 等到队列彻底静默（要么发出、要么过期丢弃）
	deadline := time.Now().Add(30 * time.Second)
	var st queue.Stats
	for time.Now().Before(deadline) {
		st = r.Stats()
		if r.Backlog() == 0 && r.InFlight() == 0 &&
			st.Sent+st.DroppedExpired+st.DroppedFailed >= groups*perGroup {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st = r.Stats()

	recs, ids := sink.snapshot()
	t.Logf("压测结果：送达 %d 条 / 批次数 %d / 过期丢弃 %d / 失败丢弃 %d / 重试 %d / 拒绝 %d",
		st.Sent, st.Batches, st.DroppedExpired, st.DroppedFailed, st.Retries, st.Rejected)

	assertConservation(t, groups*perGroup, st, r.Backlog())

	if st.Sent == 0 {
		t.Fatal("一条都没发出去，配额逻辑把流量全堵死了")
	}
	if st.DroppedExpired == 0 {
		t.Error("积压 1500 条却没有一条超时丢弃，说明截止时间路径没生效")
	}
	// 平台数字：全局 60/qpm。整个压测窗口只有几秒，发出量必须远低于日额度
	if float64(st.Sent) > 60*1.5 {
		t.Errorf("共发出 %d 条，超过全局 60/qpm 在压测时长内的合理上界", st.Sent)
	}
	assertRollingLimit(t, recs, time.Minute, "g0", 20)
	for g := 0; g < groups; g++ {
		assertRollingLimit(t, recs, time.Minute, fmt.Sprintf("g%d", g), 20)
	}
	assertGlobalRollingLimit(t, recs, time.Minute, 60)

	if len(ids) != st.Sent {
		t.Errorf("sink 记录 %d 条与 Stats.Sent=%d 不一致", len(ids), st.Sent)
	}
	cancel()
	<-done
	waitGoroutines(t, before)
}

// ---------- 日历查询延迟 ----------

func TestCalendarQueryLatency(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	const total = 20000
	store := calendar.NewStore()
	list := make([]calendar.Activity, 0, total)
	for i := 0; i < total; i++ {
		start := base.Add(time.Duration(i) * 30 * time.Minute) // 跨约 3.4 年
		list = append(list, calendar.Activity{
			ID: fmt.Sprintf("a%d", i), Game: "gs", Title: "活动",
			Start: start, End: start.Add(14 * 24 * time.Hour),
		})
	}
	store.BulkUpsert(list)
	if store.Len() != total {
		t.Fatalf("载入 %d, want %d", store.Len(), total)
	}

	const batch = 500
	sample := func(name string, f func(time.Time) []calendar.Activity) {
		var perQuery []time.Duration
		for round := 0; round < 20; round++ {
			start := time.Now()
			for i := 0; i < batch; i++ {
				at := base.Add(time.Duration((round*batch+i)*7%300000) * time.Minute)
				_ = f(at)
			}
			perQuery = append(perQuery, time.Since(start)/batch)
		}
		p50, p99 := percentiles(perQuery)
		t.Logf("%-14s n=%d 单次查询 P50=%v P99=%v（%d 次/样本 × %d 样本）",
			name, total, p50, p99, batch, len(perQuery))
		if p99 > 2*time.Millisecond {
			t.Errorf("%s P99 = %v, 超过 2ms（用户 @机器人 查询要求毫秒级）", name, p99)
		}
	}
	sample("Active", store.Active)
	sample("Upcoming", func(at time.Time) []calendar.Activity { return store.Upcoming(at, 7*24*time.Hour) })
	sample("EndingWithin", func(at time.Time) []calendar.Activity { return store.EndingWithin(at, 3*24*time.Hour) })
}

// ---------- 提醒触发抖动 ----------

func TestReminderJitterUnderLoad(t *testing.T) {
	r := NewRuntime(flakySink{sink: newLoadSink(), latency: time.Millisecond}, generousPolicy(), calendar.NewStore(), nil)
	apiCh := make(chan API, 1)
	var deltas []time.Duration
	var mu sync.Mutex
	const n = 800

	r.Register(NewFeature("jitter", func(ctx context.Context, api API) error {
		apiCh <- api
		go func() {
			base := time.Now().Add(100 * time.Millisecond)
			for i := 0; i < n; i++ {
				want := base.Add(time.Duration(i) * time.Millisecond)
				id := fmt.Sprintf("t%d", i)
				api.Schedule(id, want, func(context.Context) error {
					mu.Lock()
					deltas = append(deltas, time.Since(want))
					mu.Unlock()
					return api.Submit(queue.Item{ID: id, Target: "g0", Text: "提醒"})
				})
			}
		}()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	api := <-apiCh
	_ = api

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(deltas)
		mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deltas) != n {
		t.Fatalf("只触发 %d/%d 个提醒", len(deltas), n)
	}
	d := make([]time.Duration, len(deltas))
	copy(d, deltas)
	p50, p99 := percentiles(d)
	t.Logf("提醒抖动 n=%d 中位数=%v P99=%v 跨度=%v", len(d), p50, p99,
		func() time.Duration {
			sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
			return d[len(d)-1] - d[0]
		}())
	if p99 > 60*time.Millisecond {
		t.Errorf("提醒 P99 偏差 = %v, 超过 60ms", p99)
	}
}

// ---------- 断言工具 ----------

func generousPolicy() queue.Policy {
	p := queue.DefaultPolicy()
	p.MergeWindow = time.Millisecond
	p.TargetPerMin = 60000
	p.TargetBurst = 1000
	p.GlobalPerMin = 60000
	p.GlobalBurst = 1000
	p.TargetPerDay = 0
	p.QueueSize = 8192
	return p
}

// assertConservation 检查账目守恒：提交数 = 拒绝 + 送达 + 过期丢 + 失败丢 + 溢出丢（静默时积压为 0）。
// 这个恒等式是按平台"不允许无声吞消息"的要求独立推导的，不依赖本包实现细节。
func assertConservation(t *testing.T, submitted int, st queue.Stats, backlog int) {
	t.Helper()
	if backlog != 0 {
		t.Errorf("静默后仍有积压 %d", backlog)
	}
	accounted := st.Rejected + st.Sent + st.DroppedExpired + st.DroppedFailed + st.DroppedOverflw
	if accounted != st.Submitted {
		t.Errorf("账目不守恒：已提交 %d，可解释 %d（拒绝 %d 送达 %d 过期 %d 失败 %d 溢出 %d）→ 丢了 %d 条",
			st.Submitted, accounted, st.Rejected, st.Sent, st.DroppedExpired, st.DroppedFailed,
			st.DroppedOverflw, st.Submitted-accounted)
	}
	if st.Submitted != submitted {
		t.Errorf("Stats.Submitted = %d, 实际投递 %d", st.Submitted, submitted)
	}
}

func assertRollingLimit(t *testing.T, recs []sendRecord, window time.Duration, target string, limit float64) {
	t.Helper()
	var times []time.Time
	for _, r := range recs {
		if r.target != target {
			continue
		}
		for i := 0; i < r.n; i++ {
			times = append(times, r.at)
		}
	}
	if exceedMax(times, window, int(limit)) {
		t.Errorf("群 %s 在 %v 窗口内超过 %v 条", target, window, limit)
	}
}

func assertGlobalRollingLimit(t *testing.T, recs []sendRecord, window time.Duration, limit float64) {
	t.Helper()
	var times []time.Time
	for _, r := range recs {
		for i := 0; i < r.n; i++ {
			times = append(times, r.at)
		}
	}
	if exceedMax(times, window, int(limit)) {
		t.Errorf("Bot 维度在 %v 窗口内超过 %v 条", window, limit)
	}
}

// exceedMax 用双指针检查任意滑动窗口内的最大计数。
func exceedMax(sorted []time.Time, window time.Duration, limit int) bool {
	if len(sorted) == 0 {
		return false
	}
	j := 0
	for i := range sorted {
		for sorted[i].Sub(sorted[j]) > window {
			j++
		}
		if i-j+1 > limit {
			return true
		}
	}
	return false
}

func goroutines(t *testing.T) int {
	t.Helper()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	return runtime.NumGoroutine()
}

// waitGoroutines 确认内核没有留下悬空协程：手机内存与句柄都经不起泄漏。
func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		last = runtime.NumGoroutine()
		if last <= baseline+1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("协程数未回落：基线 %d，当前 %d", baseline, last)
}

func percentiles(d []time.Duration) (p50, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)*50/100], c[min(len(c)*99/100, len(c)-1)]
}

type dummyTargetView struct{ list []string }

func (d dummyTargetView) Targets() []string     { return d.list }
func (d dummyTargetView) Has(target string) bool { return true }

func TestAPITargetsSeam(t *testing.T) {
	tv := dummyTargetView{list: []string{"g:1", "u:2"}}
	r := NewRuntime(flakySink{sink: newLoadSink()}, queue.DefaultPolicy(), calendar.NewStore(), tv)

	got := make(chan []string, 1)
	r.Register(NewFeature("target-check", func(ctx context.Context, api API) error {
		got <- api.Targets()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	select {
	case targets := <-got:
		if len(targets) != 2 || targets[0] != "g:1" || targets[1] != "u:2" {
			t.Errorf("api.Targets() = %v, 期望 [g:1 u:2]", targets)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 api.Targets() 超时")
	}
}

