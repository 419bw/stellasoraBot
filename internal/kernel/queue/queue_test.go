package queue

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedSend struct {
	at     time.Time
	target string
	ids    []string
	text   string
	media  string // 非空表示这一条发的是图，值是版本键
}

type recSink struct {
	mu      sync.Mutex
	sends   []recordedSend
	failFor int // 前 N 次 Send 返回可重试错误
	calls   int
	latency time.Duration
}

func (s *recSink) Send(ctx context.Context, b *Batch) error {
	if s.latency > 0 {
		select {
		case <-time.After(s.latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failFor {
		return errors.New("sink: 临时失败")
	}
	ids := make([]string, 0, len(b.Items))
	for _, it := range b.Items {
		ids = append(ids, it.ID)
	}
	s.sends = append(s.sends, recordedSend{
		at: time.Now(), target: b.Target, ids: ids, text: b.Text,
		media: mediaKey(b),
	})
	return nil
}

func mediaKey(b *Batch) string {
	if b.Media == nil {
		return ""
	}
	return b.Media.Key
}

func (s *recSink) snapshot() []recordedSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedSend(nil), s.sends...)
}

func (s *recSink) delivered() map[string]bool {
	out := make(map[string]bool)
	for _, send := range s.snapshot() {
		for _, id := range send.ids {
			out[id] = true
		}
	}
	return out
}

func startDispatcher(t *testing.T, d *Dispatcher) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("Run 返回意外错误: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run 未在取消后退出")
		}
	})
	return cancel
}

func fastPolicy() Policy {
	p := DefaultPolicy()
	p.MergeWindow = 30 * time.Millisecond
	p.Deadline = 5 * time.Second
	p.BackoffBase = 10 * time.Millisecond
	p.TargetPerMin = 6000
	p.TargetBurst = 50
	p.GlobalPerMin = 6000
	p.GlobalBurst = 50
	p.TargetPerDay = 0
	p.QueueSize = 4096
	return p
}

func waitForStats(t *testing.T, d *Dispatcher, cond func(Stats) bool, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond(d.Stats()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("超时: %s (stats=%+v backlog=%d)", what, d.Stats(), d.Backlog())
}

func TestSendsEveryItem(t *testing.T) {
	sink := &recSink{}
	d := New(sink, fastPolicy())
	startDispatcher(t, d)

	const n = 60
	for i := 0; i < n; i++ {
		if err := d.Submit(Item{ID: itoa(i), Target: "g1", Text: "t"}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	waitForStats(t, d, func(s Stats) bool { return s.Sent == n }, 5*time.Second, "并非全部消息都发出")

	got := sink.delivered()
	if len(got) != n {
		t.Errorf("实际送达 %d 条, want %d（丢失或重复）", len(got), n)
	}
}

// 合并只在同 Target + 同 Topic 且标记 Mergeable 时发生。
func TestMergesSameTopicIntoOneSend(t *testing.T) {
	sink := &recSink{}
	d := New(sink, fastPolicy())
	startDispatcher(t, d)

	for i := 0; i < 4; i++ {
		d.Submit(Item{ID: itoa(i), Target: "g1", Topic: "ending", Mergeable: true, Text: "活动" + itoa(i) + "结束"})
	}
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 4 }, 5*time.Second, "合并后的消息未全部记为已发送")

	sends := sink.snapshot()
	if len(sends) != 1 {
		t.Fatalf("发送次数 = %d, want 1（应合并成一条）", len(sends))
	}
	if got := strings.Count(sends[0].text, "\n"); got != 3 {
		t.Errorf("合并文本行数 = %d, want 3: %q", got, sends[0].text)
	}
	if st := d.Stats(); st.Merged != 3 || st.Batches != 1 {
		t.Errorf("Merged/Batches = %d/%d, want 3/1", st.Merged, st.Batches)
	}
}

// OnDelivered 是"这条真的发出去了"的队列侧权威时刻：整批成功后逐条回调（合并批
// 一条消息装多个条目，各记各的账）；失败重试与最终丢弃一律不回调——功能拿它记账，
// 这两条边界就是"只有发送成功才记账"的机制保证。
func TestOnDeliveredFiresPerItemOnSuccessOnly(t *testing.T) {
	var mu sync.Mutex
	fired := map[string]int{}

	sink := &recSink{failFor: 1} // 第一次 Send 失败（可重试），第二次成功
	d := New(sink, fastPolicy())
	startDispatcher(t, d)

	d.Submit(Item{ID: "m1", Target: "g1", Topic: "t", Mergeable: true, Text: "甲",
		OnDelivered: func() { mu.Lock(); fired["m1"]++; mu.Unlock() }})
	d.Submit(Item{ID: "m2", Target: "g1", Topic: "t", Mergeable: true, Text: "乙",
		OnDelivered: func() { mu.Lock(); fired["m2"]++; mu.Unlock() }})

	waitForStats(t, d, func(s Stats) bool { return s.Sent == 2 }, 5*time.Second, "两条合并消息未送达")
	sends := sink.snapshot()
	if len(sends) != 1 {
		t.Fatalf("成功发送次数 = %d, want 1（recSink 只记成功的批次）", len(sends))
	}
	if got := len(sends[0].ids); got != 2 {
		t.Fatalf("成功批的条目数 = %d, want 2（合并批）", got)
	}
	waitForStats(t, d, func(s Stats) bool { return s.Retries >= 2 && s.Sent == 2 }, 5*time.Second, "重试未完成")
	time.Sleep(50 * time.Millisecond) // 给"不该发生的回调"留出暴露窗口

	mu.Lock()
	defer mu.Unlock()
	if fired["m1"] != 1 || fired["m2"] != 1 {
		t.Errorf("回执回调次数 = %v, want 各恰好 1 次（失败那批不许回调）", fired)
	}
}

func TestDifferentTopicsAreNotMerged(t *testing.T) {
	sink := &recSink{}
	d := New(sink, fastPolicy())
	startDispatcher(t, d)

	d.Submit(Item{ID: "a", Target: "g1", Topic: "ending", Mergeable: true, Text: "A"})
	d.Submit(Item{ID: "b", Target: "g1", Topic: "start", Mergeable: true, Text: "B"})
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 2 }, 5*time.Second, "两条都未发出")

	if got := len(sink.snapshot()); got != 2 {
		t.Errorf("发送次数 = %d, want 2（不同 Topic 被错误合并）", got)
	}
}

// TestPosterItemsNeverMerge：图不能并成一条。这里故意把 Mergeable 也设上，
// 挡的就是"调用方标错一个字段，图就被拼进文字里"。
func TestPosterItemsNeverMerge(t *testing.T) {
	sink := &recSink{}
	d := New(sink, fastPolicy())
	startDispatcher(t, d)

	for i := 0; i < 3; i++ {
		d.Submit(Item{ID: "p" + itoa(i), Target: "g1", Topic: "poster", Mergeable: true,
			Media: &Media{Kind: "poster", Key: "202609080000"}})
	}
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 3 }, 5*time.Second, "三张图未全部发出")

	sends := sink.snapshot()
	if len(sends) != 3 {
		t.Fatalf("发送次数 = %d, want 3（图被合并了）", len(sends))
	}
	for i, s := range sends {
		if s.media == "" {
			t.Errorf("第 %d 条不是图: %+v", i, s)
		}
		if len(s.ids) != 1 {
			t.Errorf("第 %d 条带了 %d 个 item，want 1", i, len(s.ids))
		}
	}
	if st := d.Stats(); st.Merged != 0 {
		t.Errorf("Merged = %d, want 0", st.Merged)
	}
}

// TestPosterNotAbsorbedIntoTextBatch 盯的是合并只看"头"的 Mergeable、不看被吸收那一侧
// 的那个洞：图被并进文字批次后只剩一个空 Text，图本身静默消失。
func TestPosterNotAbsorbedIntoTextBatch(t *testing.T) {
	sink := &recSink{}
	d := New(sink, fastPolicy())
	startDispatcher(t, d)

	d.Submit(Item{ID: "text", Target: "g1", Topic: "ending", Mergeable: true, Text: "活动结束了"})
	d.Submit(Item{ID: "pic", Target: "g1", Topic: "ending", Mergeable: true,
		Media: &Media{Kind: "poster", Key: "202609080000"}})
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 2 }, 5*time.Second, "文字与图未都发出")

	sends := sink.snapshot()
	if len(sends) != 2 {
		t.Fatalf("发送次数 = %d, want 2: %+v", len(sends), sends)
	}
	var media, text int
	for _, s := range sends {
		if s.media != "" {
			media++
			if s.text != "" {
				t.Errorf("图那条批次上还挂了文字 %q", s.text)
			}
			continue
		}
		text++
		if s.text != "活动结束了" {
			t.Errorf("文字那条内容 = %q", s.text)
		}
	}
	if media != 1 || text != 1 {
		t.Errorf("media/text = %d/%d, want 1/1", media, text)
	}
}

func TestRetryWithBackoffEventuallySends(t *testing.T) {
	sink := &recSink{failFor: 2}
	p := fastPolicy()
	d := New(sink, p)
	startDispatcher(t, d)

	d.Submit(Item{ID: "x", Target: "g1", Text: "hi"})
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 1 }, 5*time.Second, "重试后仍未送达")

	st := d.Stats()
	// failFor=2 → 前两次 Send 各产生一次重试，第三次才成功
	if st.Retries != 2 {
		t.Errorf("Retries = %d, want 2", st.Retries)
	}
	if st.DroppedFailed != 0 {
		t.Errorf("DroppedFailed = %d, want 0", st.DroppedFailed)
	}
}

// 一旦平台给出"用户关闭了主动发送"这类不可重试错误，绝不能继续退避重刷。
func TestNonRetryableErrorDropsImmediately(t *testing.T) {
	sink := &recSink{failFor: 99}
	d := New(sink, fastPolicy())
	var seenErrs atomicInt
	d.SetRetryClassifier(func(error) bool { return false })
	d.OnError = func(Item, error) { seenErrs.add(1) }
	startDispatcher(t, d)

	d.Submit(Item{ID: "x", Target: "g1", Text: "hi"})
	waitForStats(t, d, func(s Stats) bool { return s.DroppedFailed == 1 }, 5*time.Second, "不可重试错误未被立刻丢弃")

	st := d.Stats()
	if st.Retries != 0 {
		t.Errorf("Retries = %d, want 0（不该重试）", st.Retries)
	}
	if st.Sent != 0 {
		t.Errorf("Sent = %d, want 0", st.Sent)
	}
	if seenErrs.get() != 1 {
		t.Errorf("OnError 调用 %d 次, want 1", seenErrs.get())
	}
}

// gateSink：Send 进入即报信、然后卡住直到放行，用于把 worker 钉在原地、
// 让条目稳定滞留在 pending 里触发积压溢出。
type gateSink struct {
	entered chan struct{}
	release chan struct{}

	mu      sync.Mutex
	sentIDs []string
}

func (g *gateSink) Send(ctx context.Context, b *Batch) error {
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, it := range b.Items {
		g.sentIDs = append(g.sentIDs, it.ID)
	}
	return nil
}

func (g *gateSink) delivered() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]bool, len(g.sentIDs))
	for _, id := range g.sentIDs {
		out[id] = true
	}
	return out
}

// 溢出丢弃不能只留在 DroppedOverflw 计数里：它和过期/终态失败同属静默丢消息，
// 必须走 OnError 交代。丢弃决策在锁内（enqueue），回调必须在锁外（Run 循环）。
//
// 确定性构造（Workers=1、积压上限 1）：a 被 worker 卡在 Send 里；b 被 pump 塞进
// 满容量 jobs 缓冲；c 留在 pending；d 到来时把最老的 c 挤出溢出。Run 按 FIFO 逐条
// 处理且每条都先 enqueue 再 pump，所以无论测试侧提交多快，c 必然是溢出对象。
func TestOverflowDroppedItemReportsOnError(t *testing.T) {
	gate := &gateSink{entered: make(chan struct{}, 8), release: make(chan struct{})}
	p := fastPolicy()
	p.Workers = 1
	p.MaxPendingPerTarget = 1
	d := New(gate, p)

	var mu sync.Mutex
	var overflowed []Item
	var causes []error
	d.OnError = func(it Item, err error) {
		_ = d.Stats() // 回调可能回访 Dispatcher：持锁调用会自死锁（纪律同 OnDelivered）
		mu.Lock()
		defer mu.Unlock()
		overflowed = append(overflowed, it)
		causes = append(causes, err)
	}
	startDispatcher(t, d)

	if err := d.Submit(Item{ID: "a", Target: "g1", Text: "1"}); err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	<-gate.entered // a 已被 worker 取走并卡在 Send 里

	for _, id := range []string{"b", "c", "d"} {
		if err := d.Submit(Item{ID: id, Target: "g1", Text: id}); err != nil {
			t.Fatalf("Submit %s: %v", id, err)
		}
	}

	waitForStats(t, d, func(s Stats) bool { return s.DroppedOverflw == 1 }, 3*time.Second, "溢出丢弃未计数")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(overflowed)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if len(overflowed) != 1 {
		t.Fatalf("溢出丢弃未回调 OnError（回调 %d 次）", len(overflowed))
	}
	if overflowed[0].ID != "c" {
		t.Errorf("溢出丢的是 %q, want \"c\"（pending 里最老的）", overflowed[0].ID)
	}
	if !errors.Is(causes[0], ErrOverflow) {
		t.Errorf("溢出回调的错误 = %v, want ErrOverflow", causes[0])
	}
	mu.Unlock()

	close(gate.release)
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 3 }, 3*time.Second, "放行后 a/b/d 未发出")
	got := gate.delivered()
	for _, id := range []string{"a", "b", "d"} {
		if !got[id] {
			t.Errorf("条目 %s 应已送达", id)
		}
	}
	if got["c"] {
		t.Error("被溢出丢弃的 c 不应送达")
	}
}

func TestDailyQuotaBlocksFurtherSends(t *testing.T) {
	sink := &recSink{}
	p := fastPolicy()
	p.TargetPerDay = 3
	d := New(sink, p)
	startDispatcher(t, d)

	for i := 0; i < 10; i++ {
		d.Submit(Item{ID: itoa(i), Target: "g1", Text: "t"})
	}
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 3 && s.DailyBlocked > 0 }, 3*time.Second, "日额度未生效")
}

func TestExpiredMessagesAreDroppedNotSent(t *testing.T) {
	sink := &recSink{}
	p := fastPolicy()
	p.Deadline = 30 * time.Millisecond
	p.TargetPerMin = 1 // 一分钟一个，故意让它排在后面
	p.TargetBurst = 1
	d := New(sink, p)
	var expired int
	var mu sync.Mutex
	d.OnError = func(Item, error) { mu.Lock(); expired++; mu.Unlock() }
	startDispatcher(t, d)

	for i := 0; i < 5; i++ {
		d.Submit(Item{ID: itoa(i), Target: "g1", Text: "t"})
	}
	waitForStats(t, d, func(s Stats) bool { return s.DroppedExpired >= 3 }, 3*time.Second, "超期消息未被丢弃")
	mu.Lock()
	defer mu.Unlock()
	if expired == 0 {
		t.Error("丢弃超期消息没有回调 OnError")
	}
}

func TestSubmitRejectsWhenBufferFull(t *testing.T) {
	p := fastPolicy()
	p.QueueSize = 2
	d := New(&recSink{}, p) // 故意不 Run，in 通道不会被消费

	var rejected int
	for i := 0; i < 20; i++ {
		if errors.Is(d.Submit(Item{ID: itoa(i), Target: "g1"}), ErrQueueFull) {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("队列已满却没有任何 Submit 被拒")
	}
	if st := d.Stats(); st.Rejected != rejected {
		t.Errorf("Rejected 统计 = %d, 实际拒绝 %d", st.Rejected, rejected)
	}
}

// 慢消费者不能阻塞生产者：调度线程只做非阻塞投递。
func TestProducerNotBlockedBySlowSink(t *testing.T) {
	sink := &recSink{latency: 25 * time.Millisecond}
	d := New(sink, fastPolicy())
	startDispatcher(t, d)

	start := time.Now()
	for i := 0; i < 200; i++ {
		if err := d.Submit(Item{ID: itoa(i), Target: "g1", Text: "t"}); err != nil {
			t.Fatalf("Submit 被阻塞/拒绝: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("200 次 Submit 花了 %v，生产者被慢 Sink 拖住了", elapsed)
	}
}

// 按平台真实数字（单群 20/qpm）校验：任意 60 秒滑动窗口内某群收到的条数不得越界。
func TestRollingWindowRespectsPlatformQuota(t *testing.T) {
	const perTargetPerMin = 20.0
	sink := &recSink{}
	p := DefaultPolicy()
	p.MergeWindow = 5 * time.Millisecond
	p.TargetPerMin = perTargetPerMin
	p.TargetBurst = 1
	p.GlobalPerMin = 600 // 放开全局，单独观察单群配额这一维
	p.GlobalBurst = 50
	p.QueueSize = 4096
	d := New(sink, p)
	startDispatcher(t, d)

	const groups = 6
	const perGroup = 40
	for g := 0; g < groups; g++ {
		target := "g" + itoa(g)
		for i := 0; i < perGroup; i++ {
			if err := d.Submit(Item{ID: target + "-" + itoa(i), Target: target, Text: "t"}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
	}

	const runFor = 3 * time.Second
	time.Sleep(runFor)

	// 令牌桶是线性的：这段时间内单群上限 = 突发额度 + runFor*20/60，再留一格边界余量
	maxPerTarget := int(float64(p.TargetBurst)+runFor.Seconds()*perTargetPerMin/60) + 1

	counts := map[string]int{}
	for _, s := range sink.snapshot() {
		counts[s.target] += len(s.ids)
	}
	assertRollingLimit(t, sink.snapshot(), time.Minute, perTargetPerMin)

	for g := 0; g < groups; g++ {
		target := "g" + itoa(g)
		if counts[target] > maxPerTarget {
			t.Errorf("群 %s 在 %v 内收到 %d 条，超过单群配额推算上限 %d", target, runFor, counts[target], maxPerTarget)
		}
		if counts[target] == 0 {
			t.Errorf("群 %s 一条都没收到，配额分配存在饿死", target)
		}
	}
}

// assertRollingLimit 的标尺来自平台文档数字，而不是本包的实现常量。
func assertRollingLimit(t *testing.T, sends []recordedSend, window time.Duration, limit float64) {
	t.Helper()
	// 展平成 (target, at) 序列后做滑动窗口
	type ev struct {
		at     time.Time
		target string
		n      int
	}
	var evs []ev
	for _, s := range sends {
		evs = append(evs, ev{at: s.at, target: s.target, n: len(s.ids)})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })

	for i, e := range evs {
		count := 0
		for j := i; j < len(evs) && evs[j].at.Sub(e.at) <= window; j++ {
			if evs[j].target == e.target {
				count += evs[j].n
			}
		}
		if float64(count) > limit+0.5 {
			t.Errorf("群 %s 在 %v 窗口内收到 %d 条（自第 %d 条起算），超出平台配额 %.0f",
				e.target, window, count, i, limit)
			return
		}
	}
}

type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) add(n int) {
	a.mu.Lock()
	a.n += n
	a.mu.Unlock()
}

func (a *atomicInt) get() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// 永久失败必须在 MaxAttempts 之后停下。
// 早先 requeue 会重建一个 attempts 归零的副本，导致重试次数失去上界。
func TestPermanentFailureStopsAfterMaxAttempts(t *testing.T) {
	sink := &recSink{failFor: 1 << 30}
	p := fastPolicy()
	p.MaxAttempts = 3
	d := New(sink, p)
	startDispatcher(t, d)

	d.Submit(Item{ID: "x", Target: "g1", Text: "hi"})
	waitForStats(t, d, func(s Stats) bool { return s.DroppedFailed == 1 }, 3*time.Second, "永久失败未被丢弃")

	time.Sleep(200 * time.Millisecond) // 给可能的多余重试留时间
	st := d.Stats()
	if st.Retries != 2 {
		t.Errorf("Retries = %d, want 2", st.Retries)
	}
	sink.mu.Lock()
	calls := sink.calls
	sink.mu.Unlock()
	if calls != 3 {
		t.Errorf("Send 共被调用 %d 次, want 3 —— 重试上界失守", calls)
	}
}

// OnError 是用户代码，它可能反过来读 Stats；在持锁状态下回调会自死锁。
func TestOnErrorMayReadStatsWithoutDeadlock(t *testing.T) {
	sink := &recSink{failFor: 1 << 30}
	p := fastPolicy()
	p.MaxAttempts = 1
	d := New(sink, p)

	var mu sync.Mutex
	seen := 0
	d.OnError = func(Item, error) {
		mu.Lock()
		defer mu.Unlock()
		_ = d.Stats()
		_ = d.Backlog()
		seen++
	}
	startDispatcher(t, d)

	d.Submit(Item{ID: "x", Target: "g1", Text: "hi"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := seen
		mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("OnError 未按预期触发（可能死锁）")
}

// panicSink 对指定目标 panic N 次（其余正常送达），用于钉死 Sink 边界保险丝。
type panicSink struct {
	panics map[string]int // target → 剩余 panic 次数
	mu     sync.Mutex
	sent   []string
}

func (s *panicSink) Send(ctx context.Context, b *Batch) error {
	s.mu.Lock()
	if s.panics[b.Target] > 0 {
		s.panics[b.Target]--
		s.mu.Unlock()
		panic("sink: 目标 " + b.Target + " 炸了")
	}
	s.mu.Unlock()
	s.mu.Lock()
	s.sent = append(s.sent, b.Target)
	s.mu.Unlock()
	return nil
}

func (s *panicSink) sentFor(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.sent {
		if t == target {
			return true
		}
	}
	return false
}

// Sink panic 不带走 worker：降级为投递错误，走终态丢弃 + OnError，
// worker 存活继续送后续消息。变异负对照：注释掉 sendGuarded 的 recover，本测试必红。
func TestSinkPanicIsDowngradedToFailure(t *testing.T) {
	sink := &panicSink{panics: map[string]int{"g:boom": 1 << 30}}
	p := fastPolicy()
	p.MaxAttempts = 1
	d := New(sink, p)
	var mu sync.Mutex
	var errText string
	d.OnError = func(_ Item, err error) {
		mu.Lock()
		defer mu.Unlock()
		errText = err.Error()
	}
	startDispatcher(t, d)

	d.Submit(Item{ID: "doomed", Target: "g:boom", Text: "必炸"})
	waitForStats(t, d, func(s Stats) bool { return s.DroppedFailed == 1 }, 5*time.Second, "panic 未走终态丢弃")

	mu.Lock()
	if !strings.Contains(errText, "panic") || !strings.Contains(errText, "g:boom") {
		t.Errorf("OnError 收到错误 %q，want 含 panic 标记与目标", errText)
	}
	mu.Unlock()

	// worker 还活着：后续消息照常送达，在途账目回收干净
	d.Submit(Item{ID: "alive", Target: "g:ok", Text: "还活着"})
	waitForStats(t, d, func(s Stats) bool { return s.Sent == 1 }, 5*time.Second, "panic 后 worker 未存活")
	if n := d.InFlight(); n != 0 {
		t.Errorf("InFlight = %d, want 0 —— panic 不得泄漏在途计数", n)
	}
}

// panic 只发生一次时走既有重试管道：重试成功、回执照常、无终态丢弃。
func TestSinkPanicOnceRetriesSucceed(t *testing.T) {
	sink := &panicSink{panics: map[string]int{"g1": 1}}
	d := New(sink, fastPolicy())
	var delivered atomicInt
	d.OnError = func(Item, error) { t.Error("panic 后重试成功不应终态丢弃") }
	startDispatcher(t, d)

	d.Submit(Item{ID: "x", Target: "g1", Text: "hi", OnDelivered: func() { delivered.add(1) }})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && delivered.get() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if delivered.get() != 1 {
		t.Fatalf("panic 一次后未重试成功（OnDelivered %d 次）: stats=%+v", delivered.get(), d.Stats())
	}
	st := d.Stats()
	if st.Retries != 1 || st.Sent != 1 || st.DroppedFailed != 0 {
		t.Errorf("Stats = %+v, want Retries=1 Sent=1 DroppedFailed=0", st)
	}
	if n := d.InFlight(); n != 0 {
		t.Errorf("InFlight = %d, want 0", n)
	}
}
