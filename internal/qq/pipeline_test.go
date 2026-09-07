package qq_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	. "xingta/internal/qq"

	"xingta/internal/devtools/qqsim"
)

// 端到端：本地网关模拟器 → 我们的 Gateway 状态机 → Hub 解码去重 → 业务处理器。
// 这一段跑通才说明「去重」不是只在单元测试里成立 —— 重复是从协议层进来的，
// 必须在协议层之后仍然被挡住。
// 压测/自检里显式给去重容量，不去引用包内私有默认值：
// 断言 "Live <= 容量" 时这个数必须是我们自己定的，否则换默认值就会静默失去意义。
const simDedupKeys = 2048

type pipeline struct {
	t      *testing.T
	sim    *qqsim.Server
	hub    *Hub
	gw     *Gateway
	cancel context.CancelFunc
	logs   *safeBuf
	done   chan error
}

type safeBuf struct {
	mu sync.Mutex
	sb []byte
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sb = append(b.sb, p...)
	return len(p), nil
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.sb)
}

func runPipeline(t *testing.T, cfg qqsim.Config) *pipeline {
	t.Helper()
	sim, err := qqsim.New(cfg)
	if err != nil {
		t.Fatalf("起模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })

	client := NewClientAt("app-id", "app-secret", sim.BaseURL())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	p := &pipeline{t: t, sim: sim, cancel: cancel, logs: &safeBuf{}}

	info, err := client.Gateway(ctx)
	if err != nil {
		cancel()
		t.Fatalf("向模拟器取网关地址失败: %v", err)
	}
	if info.URL != sim.WSURL() {
		cancel()
		t.Fatalf("网关地址 = %q，想要 %q", info.URL, sim.WSURL())
	}

	p.hub = NewHub(NewDeduper(DedupConfig{MaxKeys: simDedupKeys}), func(f string, a ...any) {
		fmt.Fprintf(p.logs, f+"\n", a...)
	})
	p.gw = NewGateway(GatewayConfig{
		Tokens: client.Tokens(),
		URL:    info.URL,
		// 用真 intents 值，顺带验证我们下发的位掩码与平台要求一致。
		Intents:         IntentPublicMessages,
		AckMisses:       2,
		MinHealthy:      30 * time.Millisecond,
		ResumeBackoff:   []time.Duration{time.Millisecond, 2 * time.Millisecond},
		IdentifyBackoff: []time.Duration{2 * time.Millisecond, 5 * time.Millisecond},
		Logf: func(f string, a ...any) {
			fmt.Fprintf(p.logs, f+"\n", a...)
		},
	})

	p.done = make(chan error, 1)
	go func() { p.done <- p.gw.Run(ctx, p.hub.Handle) }()

	if !waitFor(3*time.Second, func() bool { return sim.Stats().Sessions >= 1 }) {
		cancel()
		t.Fatalf("网关没能完成 identify：%s", p.logs.String())
	}
	t.Cleanup(cancel)
	return p
}

// waitEvents 等 hub 处理满 n 个事件（含被挡下的重复）。
func (p *pipeline) waitProcessed(n int) HubStats {
	p.t.Helper()
	ok := waitFor(5*time.Second, func() bool {
		s := p.hub.Stats()
		return int(s.Admitted+s.DupDropped) >= n
	})
	s := p.hub.Stats()
	if !ok {
		p.t.Fatalf("等待处理 %d 条超时，当前 %+v\n日志：\n%s", n, s, p.logs.String())
	}
	return s
}

func TestPipelineIdentifyPayload(t *testing.T) {
	p := runPipeline(t, qqsim.Config{})

	st := p.sim.Stats()
	if len(st.Identifies) == 0 {
		t.Fatal("模拟器没收到 identify")
	}
	id := st.Identifies[0]
	if id.Token != "QQBot sim-token" {
		t.Errorf("token = %q，想要 \"QQBot sim-token\"", id.Token)
	}
	if id.Intents != IntentPublicMessages {
		t.Errorf("intents = %d，想要 %d", id.Intents, IntentPublicMessages)
	}
	if id.Shard != [2]uint{0, 1} {
		t.Errorf("shard = %v，想要 [0,1]", id.Shard)
	}
}

func TestPipelineDecodesSimulatedGroupMessage(t *testing.T) {
	p := runPipeline(t, qqsim.Config{})

	var mu sync.Mutex
	var got []*Message
	p.hub.OnMessage(EventGroupAtMessage, func(_ context.Context, m *Message) error {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return nil
	})

	gen := qqsim.DefaultGenerator()
	t1, raw, id := gen.NextGroupAt()
	if _, err := p.sim.Push(t1, raw); err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	p.waitProcessed(1)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("处理器收到 %d 条，想要 1 条", len(got))
	}
	m := got[0]
	if m.ID != id {
		t.Errorf("id 不一致: %q vs %q", m.ID, id)
	}
	if m.GroupOpenID != gen.GroupOpenID {
		t.Errorf("group_openid = %q", m.GroupOpenID)
	}
	if m.Author.Username == "" || m.Text() == "" {
		t.Errorf("昵称/正文没解出来: %q %q", m.Author.Username, m.Text())
	}
	if !m.IsGroup() {
		t.Error("群消息被判成单聊")
	}
	if s := p.hub.Stats(); s.DecodeFail != 0 {
		t.Errorf("解码失败 %d 次：\n%s", s.DecodeFail, p.logs.String())
	}
}

// 这条是整个去重层要防的场景：掉线 → resume → 平台补发同一批消息。
func TestPipelineSuppressesResumeReplay(t *testing.T) {
	p := runPipeline(t, qqsim.Config{ReplayOnResume: true})

	var mu sync.Mutex
	handled := map[string]int{}
	p.hub.OnMessage(EventGroupAtMessage, func(_ context.Context, m *Message) error {
		mu.Lock()
		handled[m.ID]++
		mu.Unlock()
		return nil
	})

	gen := qqsim.DefaultGenerator()
	const n = 6
	for i := 0; i < n; i++ {
		t1, raw, _ := gen.NextGroupAt()
		if _, err := p.sim.Push(t1, raw); err != nil {
			t.Fatalf("推送失败: %v", err)
		}
	}
	before := p.waitProcessed(n)
	if before.Admitted != n {
		t.Fatalf("首轮放行 %d 条，想要 %d 条", before.Admitted, n)
	}

	// 掐线，逼出 resume；模拟器会按平台语义把推过的帧原样补发。
	p.sim.Reconnect()
	if !waitFor(5*time.Second, func() bool { return p.sim.Stats().Resumes >= 1 }) {
		t.Fatalf("掉线后没有 resume：\n%s", p.logs.String())
	}
	after := p.waitProcessed(2 * n)

	if rep := p.sim.Stats().Replayed; rep < n {
		t.Fatalf("模拟器只补发了 %d 条，想要至少 %d 条 —— 补发没发生，上面那些断言就是空跑",
			rep, n)
	}
	if after.Admitted != n {
		t.Errorf("补发后放行总数 = %d，想要仍是 %d（一条都不该重复处理）", after.Admitted, n)
	}
	if after.DupDropped < n {
		t.Errorf("挡下的重复 = %d，想要至少 %d", after.DupDropped, n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handled) != n {
		t.Errorf("业务看到 %d 个不同 id，想要 %d", len(handled), n)
	}
	for id, c := range handled {
		if c != 1 {
			t.Errorf("消息 %s 被业务处理 %d 次，去重失败", head(id, 20), c)
		}
	}
	// 掉线重连不该重新 identify：那会白烧建联配额。
	if got := p.sim.Stats().Sessions; got != 1 {
		t.Errorf("identify 次数 = %d，resume 成功时应当仍是 1", got)
	}
}

// 对照实验：把补发关掉，同样的断言就必须失败 —— 否则上面那条测试可能是假绿。
func TestPipelineReplayControlOff(t *testing.T) {
	p := runPipeline(t, qqsim.Config{ReplayOnResume: false})

	var mu sync.Mutex
	var count int
	p.hub.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})

	gen := qqsim.DefaultGenerator()
	for i := 0; i < 4; i++ {
		t1, raw, _ := gen.NextGroupAt()
		_, _ = p.sim.Push(t1, raw)
	}
	p.waitProcessed(4)

	p.sim.Reconnect()
	if !waitFor(3*time.Second, func() bool { return p.sim.Stats().Resumes >= 1 }) {
		t.Fatalf("掉线后没有 resume")
	}
	// 补发关掉时，重连后不该再看到任何消息事件。
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 4 {
		t.Errorf("关掉补发后业务仍看到 %d 条，想要 4 条", count)
	}
	if s := p.hub.Stats(); s.DupDropped != 0 {
		t.Errorf("关掉补发却有 %d 条被判成重复，说明去重误伤", s.DupDropped)
	}
}

func TestPipelineSoakNoLoss(t *testing.T) {
	// 中等规模跑一遍，只断言「不丢、不重、内存有界」，不看吞吐（吞吐交给压测）。
	// 注意 dedup 表容量是默认 8192，这里 20000 条会触发淘汰，所以断言按淘汰后口径算。
	p := runPipeline(t, qqsim.Config{})

	var mu sync.Mutex
	seen := map[string]int{}
	p.hub.OnMessage(EventGroupAtMessage, func(_ context.Context, m *Message) error {
		mu.Lock()
		seen[m.ID]++
		mu.Unlock()
		return nil
	})

	gen := qqsim.DefaultGenerator()
	const n = 20000
	start := time.Now()
	for i := 0; i < n; i++ {
		t1, raw, _ := gen.NextGroupAt()
		if _, err := p.sim.Push(t1, raw); err != nil {
			t.Fatalf("第 %d 条推送失败: %v", i, err)
		}
	}
	pushDur := time.Since(start)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if p.hub.Stats().Admitted >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := p.hub.Stats()
	elapsed := time.Since(start)

	if s.DecodeFail != 0 {
		t.Errorf("解码失败 %d 次", s.DecodeFail)
	}
	if s.DupDropped != 0 {
		t.Errorf("没有补发的情况下被判成重复 %d 条", s.DupDropped)
	}
	if s.Admitted != n {
		t.Errorf("放行 %d 条，想要 %d 条（丢了 %d）", s.Admitted, n, n-s.Admitted)
	}
	mu.Lock()
	repeats := 0
	for _, c := range seen {
		if c > 1 {
			repeats++
		}
	}
	mu.Unlock()
	if repeats != 0 {
		t.Errorf("%d 条消息被重复处理", repeats)
	}
	if d := s.Dedup; d.Live > simDedupKeys {
		t.Errorf("去重表 Live=%d 超出显式容量 %d", d.Live, simDedupKeys)
	}
	t.Logf("%d 条：推送 %v，全链路 %v，%.0f msg/s，去重表 %d 槽位淘汰 %d 次",
		n, pushDur.Round(time.Millisecond), elapsed.Round(time.Millisecond),
		float64(n)/elapsed.Seconds(), s.Dedup.Live, s.Dedup.Evicted)
}
