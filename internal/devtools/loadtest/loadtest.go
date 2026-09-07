// Package loadtest 把「本地网关模拟器 + 我们的网关状态机 + 事件入口」接成一个可跑闭环，
// 产出吞吐、延迟分位、内存与崩溃点。
//
// 这不是并发正确性测试（那是 -race 的事），是性能量纲：一条消息从模拟器写出
// 到业务处理器看到，花多少时间、单线程能吃多少条每秒、灌到什么速率开始排队。
package loadtest

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xingta/internal/devtools/qqsim"
	"xingta/internal/qq"
)

type Profile struct {
	// Total 是要灌的事件条数。必填。
	Total int

	// RatePerSec 是目标推送速率；0 表示尽力灌（闭环上限，用来找天花板）。
	RatePerSec int

	// HandlerCPU 模拟业务处理器的纯 CPU 耗时（查库/渲染/排版）。
	// 0 表示空处理器，量出来的是「基础设施净成本」；
	// 真实功能上线前应该拿一个有代表性的值再跑一遍，否则数字会过分好看。
	HandlerCPU time.Duration

	// DedupKeys 去重表容量，默认走 qq 包默认值。
	DedupKeys int

	// HeartbeatMS 模拟网关下发的心跳间隔，默认 45000（与真平台同量级，
	// 免得心跳本身混进延迟样本）。
	HeartbeatMS int64

	// Verbose 打开后每 200ms 打一行实时进度。
	Verbose bool
}

func (p Profile) validate() error {
	if p.Total <= 0 {
		return fmt.Errorf("loadtest: Total 必须 > 0")
	}
	if p.RatePerSec < 0 {
		return fmt.Errorf("loadtest: RatePerSec 不能为负")
	}
	return nil
}

type Percentiles struct {
	Count int
	P50   time.Duration
	P90   time.Duration
	P99   time.Duration
	P999  time.Duration
	Max   time.Duration
	Mean  time.Duration
}

// Sample 从升序切片里取分位。空切片返回全零。
func sampleLatencies(v []int64) Percentiles {
	if len(v) == 0 {
		return Percentiles{}
	}
	at := func(q float64) time.Duration {
		i := int(q * float64(len(v)-1))
		return time.Duration(v[i])
	}
	var sum int64
	for _, x := range v {
		sum += x
	}
	return Percentiles{
		Count: len(v),
		P50:   at(0.50), P90: at(0.90), P99: at(0.99), P999: at(0.999),
		Max:  time.Duration(v[len(v)-1]),
		Mean: time.Duration(sum / int64(len(v))),
	}
}

type Mem struct {
	// RSSKB 是常驻内存。Linux/Android 读 /proc，Windows 拿不到就为 -1，
	// 此时只能用 HeapInuse 近似 —— 报告里必须标清楚是哪个。
	RSSKB       int64
	HeapInUseKB uint64
	SysKB       uint64
	GC          uint32
	RSSSource   string
}

type Result struct {
	Profile Profile

	Offered    int // 模拟器写出的事件帧
	Received   int // 客户端事件循环看到
	Admitted   int // 放行到业务处理器
	DupDropped int
	DecodeFail int
	Backlog    int // Offered - Received，收尾时还没进客户端的条数

	Elapsed    time.Duration
	Throughput float64 // Admitted / Elapsed

	// Queue = 服务端写出 → 客户端事件循环看到（含 socket、读协程、frames 通道排队）
	Queue Percentiles
	// Total = 服务端写出 → 业务处理器返回
	Total Percentiles

	Mem       Mem
	Goroutine int
	Host      string

	// ClockQuantum 是启动时采到的 time.Now 最小正步进：低于它的延迟分位都被抹平。
	ClockQuantum time.Duration
}

// Summary 返回一段可直接贴给人看的报告。
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "机型/环境: %s (%s/%s, %d 核, %s)\n",
		r.Host, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	fmt.Fprintf(&b, "灌入 %d 条，目标速率 %s，业务处理器耗时 %v\n",
		r.Offered, rateText(r.Profile.RatePerSec), r.Profile.HandlerCPU)
	fmt.Fprintf(&b, "收到 %d，放行 %d，判重 %d，解码失败 %d，收尾积压 %d\n",
		r.Received, r.Admitted, r.DupDropped, r.DecodeFail, r.Backlog)
	fmt.Fprintf(&b, "耗时 %v，吞吐 %.0f msg/s\n", r.Elapsed.Round(time.Millisecond), r.Throughput)
	fmt.Fprintf(&b, "排队延迟   p50=%v p90=%v p99=%v p999=%v max=%v\n",
		r.Queue.P50.Round(time.Microsecond), r.Queue.P90.Round(time.Microsecond),
		r.Queue.P99.Round(time.Microsecond), r.Queue.P999.Round(time.Microsecond), r.Queue.Max.Round(time.Millisecond))
	fmt.Fprintf(&b, "端到端延迟 p50=%v p90=%v p99=%v p999=%v max=%v\n",
		r.Total.P50.Round(time.Microsecond), r.Total.P90.Round(time.Microsecond),
		r.Total.P99.Round(time.Microsecond), r.Total.P999.Round(time.Microsecond), r.Total.Max.Round(time.Millisecond))
	fmt.Fprintf(&b, "内存 RSS=%s (%s)，HeapInUse=%dKB Sys=%dKB GC=%d，goroutine=%d\n",
		kbText(r.Mem.RSSKB), r.Mem.RSSSource, r.Mem.HeapInUseKB, r.Mem.SysKB, r.Mem.GC, r.Goroutine)
	fmt.Fprintf(&b, "本机 time.Now 步进 ≈ %v", r.ClockQuantum)
	if r.ClockQuantum > 100*time.Microsecond {
		fmt.Fprintf(&b, " —— 大于 100µs：上面的延迟分位只能看量级，低于步进的值都被抹平了，换 Linux/Android 重跑\n")
	} else {
		fmt.Fprintf(&b, "，延迟分位可用\n")
	}
	if r.DecodeFail != 0 {
		fmt.Fprintf(&b, "!! 有 %d 条解码失败，这批数据不可信\n", r.DecodeFail)
	}
	if r.Backlog > 0 {
		fmt.Fprintf(&b, "!! 收尾仍有 %d 条没进客户端：这个速率已经跑不动了\n", r.Backlog)
	}
	return b.String()
}

func rateText(n int) string {
	if n <= 0 {
		return "不限（尽力灌）"
	}
	return fmt.Sprintf("%d/s", n)
}

func kbText(kb int64) string {
	if kb < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%dMB", kb/1024)
}

// Run 跑一轮。它自己起模拟器、自己连、自己收尾，调用方只需要给 Profile。
func Run(ctx context.Context, p Profile) (*Result, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	hb := p.HeartbeatMS
	if hb <= 0 {
		hb = 45000
	}
	sim, err := qqsim.New(qqsim.Config{HeartbeatMS: hb})
	if err != nil {
		return nil, err
	}
	defer sim.Close()

	client := qq.NewClientAt("load-app", "load-secret", sim.BaseURL())
	info, err := client.Gateway(ctx)
	if err != nil {
		return nil, fmt.Errorf("loadtest: 取网关地址失败: %w", err)
	}

	dedupCfg := qq.DedupConfig{}
	if p.DedupKeys > 0 {
		dedupCfg.MaxKeys = p.DedupKeys
	}
	hub := qq.NewHub(qq.NewDeduper(dedupCfg), nil)

	var mu sync.Mutex
	r := &Result{Profile: p, Host: hostname(), ClockQuantum: clockQuantum()}
	queue := make([]int64, 0, p.Total)
	total := make([]int64, 0, p.Total)
	var gorPeak int

	hub.OnMessage(qq.EventGroupAtMessage, func(_ context.Context, m *qq.Message) error {
		sent, ok := qqsim.ParseSent(m.ID)
		if !ok {
			return nil
		}
		done := time.Now()
		if p.HandlerCPU > 0 {
			burn(p.HandlerCPU)
			done = time.Now()
		}
		mu.Lock()
		total = append(total, int64(done.Sub(sent)))
		mu.Unlock()
		return nil
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- qq.NewGateway(qq.GatewayConfig{
			Tokens:  client.Tokens(),
			URL:     info.URL,
			Intents: qq.IntentPublicMessages,
			// 压测里不想让心跳判死干扰计时，把判死阈值放宽。
			AckMisses:  4,
			MinHealthy: time.Hour,
		}).Run(runCtx, func(ctx context.Context, ev qq.Event) error {
			sent, ok := qqsim.ParseSent(evIDOf(ev))
			if ok {
				mu.Lock()
				queue = append(queue, int64(time.Since(sent)))
				if g := runtime.NumGoroutine(); g > gorPeak {
					gorPeak = g
				}
				mu.Unlock()
			}
			return hub.Handle(ctx, ev)
		})
	}()

	if !waitFor(5*time.Second, func() bool { return sim.Stats().Sessions >= 1 }) {
		return nil, fmt.Errorf("loadtest: 压测客户端没能建联")
	}

	gen := qqsim.DefaultGenerator()
	start := time.Now()
	interval := time.Duration(0)
	if p.RatePerSec > 0 {
		interval = time.Second / time.Duration(p.RatePerSec)
	}
	next := start
	var offered int
	for offered < p.Total {
		batch := 1
		if interval > 0 {
			// 目标速率下每条的间隔太短时，攒一小批一起发：
			// time.Sleep 的粒度做不到微秒级，逐条 sleep 会让实际速率低于目标。
			batch = int(2 * time.Millisecond / interval)
			if batch < 1 {
				batch = 1
			}
			if offered+batch > p.Total {
				batch = p.Total - offered
			}
		}
		for i := 0; i < batch; i++ {
			t, raw, _ := gen.NextGroupAt()
			if _, err := sim.Push(t, raw); err != nil {
				cancel()
				return nil, fmt.Errorf("loadtest: 第 %d 条推送失败: %w", offered+i, err)
			}
		}
		offered += batch

		if p.Verbose && offered%(2500) < batch {
			s := hub.Stats()
			fmt.Printf("  已灌 %6d/%d  放行 %6d  判重 %5d  积压 %6d  %.0f%%\n",
				offered, p.Total, s.Admitted, s.DupDropped, offered-int(s.Admitted),
				100*float64(offered)/float64(p.Total))
		}
		if interval > 0 {
			next = next.Add(time.Duration(batch) * interval)
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			}
		}
	}
	pushDur := time.Since(start)

	// 等排空：给一个与规模成正比的宽限，超时就认定这一轮跑不动。
	drainTimeout := 10*time.Second + time.Duration(p.Total)*time.Millisecond
	if p.HandlerCPU > 0 {
		drainTimeout += time.Duration(p.Total) * p.HandlerCPU * 2
	}
	if drainTimeout > 5*time.Minute {
		drainTimeout = 5 * time.Minute
	}
	deadline := time.Now().Add(drainTimeout)
	for time.Now().Before(deadline) {
		s := hub.Stats()
		if int(s.Admitted+s.DupDropped) >= offered {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	elapsed := time.Since(start)
	s := hub.Stats()
	cancel()
	<-done

	mu.Lock()
	sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
	sort.Slice(total, func(i, j int) bool { return total[i] < total[j] })
	r.Queue = sampleLatencies(queue)
	r.Total = sampleLatencies(total)
	mu.Unlock()

	r.Offered = offered
	r.Received = r.Queue.Count
	r.Admitted = int(s.Admitted)
	r.DupDropped = int(s.DupDropped)
	r.DecodeFail = int(s.DecodeFail)
	r.Backlog = offered - r.Received
	r.Elapsed = elapsed
	r.Goroutine = gorPeak
	// 吞吐按「排空后的总耗时」算，不是按推送耗时：只看 push 循环会低估排队代价。
	if elapsed.Seconds() > 0 && r.Admitted > 0 {
		r.Throughput = float64(r.Admitted) / elapsed.Seconds()
	}
	r.Mem = readMem()
	_ = pushDur
	return r, nil
}

// evIDOf 从事件原文里抠出消息 id，用于在解码之前就打卡。
func evIDOf(ev qq.Event) string {
	i := strings.Index(string(ev.Data), `"id":"`)
	if i < 0 {
		return ""
	}
	rest := ev.Data[i+6:]
	j := strings.IndexByte(string(rest), '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

// burn 用忙等模拟 CPU 耗时。time.Sleep 在 Windows 上粒度约 1ms，
// 用它模拟亚毫秒的业务耗时会严重失真，所以宁可烧 CPU。
func burn(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
	}
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// clockQuantum 采样 time.Now 的最小正步进。Windows 的计时粒度最粗可到 15ms，
// 不把这个量纲报出来，压测数字看起来会比实际可信。
func clockQuantum() time.Duration {
	prev := time.Now()
	for i := 0; i < 1<<20; i++ {
		now := time.Now()
		if d := now.Sub(prev); d > 0 {
			return d
		}
		prev = now
	}
	return 0
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// readMem 优先读 /proc（Linux 与 Termux 都有），拿不到就退回 runtime.MemStats 并标明来源。
func readMem() Mem {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m := Mem{
		HeapInUseKB: ms.HeapInuse / 1024,
		SysKB:       ms.Sys / 1024,
		GC:          uint32(ms.NumGC),
		RSSKB:       -1,
		RSSSource:   "不可用（非 Linux）",
	}
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		m.RSSSource = "不可用（读 /proc/self/statm 失败: " + err.Error() + "）"
		return m
	}
	// statm: size resident shared text lib data dtex，单位是页
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return m
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return m
	}
	m.RSSKB = pages * int64(os.Getpagesize()) / 1024
	m.RSSSource = "/proc/self/statm"
	return m
}
