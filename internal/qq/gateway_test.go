package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeGateway 是一个只按官方 op 序列应答的本地对端。
//
// 边界必须说清：它验证的是状态机（identify / resume / 心跳 / ack 判死 / op7 / op9）
// 与退避决策，不验证业务事件的报文形状 —— 后者照文档猜就是我们上次踩的坑，
// 只能靠 cmd/qqwatch 真连采样本。
type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	frames   []map[string]any // 客户端发来的报文
	conns    int              // 被建联过多少次
	sessions int
	resumes  int
	lastSeq  int64

	// 行为开关
	heartbeatMS    int64
	suppressAck    bool // 收心跳但不回 op 11 → 触发假活判定
	closeAfterGo   bool // READY 之后立刻关连接 → 逼出 resume
	rejectResume   bool // resume 一律以 4006 关闭
	op9Immediately bool // hello 之后直接判会话无效
	sessionID      string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	f := &fakeGateway{t: t, heartbeatMS: 20, sessionID: "sess-abcdef0123456789"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TokenSource 与 WS 复用同一个 base，所以这里也得充当 token 端点
		if r.URL.Path == "/app/getAppAccessToken" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"test-token","expires_in":"7200"}`))
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		defer conn.CloseNow()
		f.serve(conn)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGateway) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeGateway) serve(conn *websocket.Conn) {
	ctx := context.Background()
	send := func(m map[string]any) {
		b, _ := json.Marshal(m)
		_ = conn.Write(ctx, websocket.MessageText, b)
	}

	send(map[string]any{"op": OpHello, "d": map[string]any{"heartbeat_interval": f.heartbeatMS}})

	// op 9：告诉客户端会话无效，必须重新 identify
	if f.op9Immediately {
		send(map[string]any{"op": OpInvalidSession, "d": 4000})
		return
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Op int             `json:"op"`
			D  json.RawMessage `json:"d"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		var fields map[string]any
		_ = json.Unmarshal(data, &fields)
		f.mu.Lock()
		f.frames = append(f.frames, fields)
		f.mu.Unlock()

		switch msg.Op {
		case OpIdentify:
			f.mu.Lock()
			f.sessions++
			f.mu.Unlock()
			send(map[string]any{
				"op": OpDispatch, "s": 7, "t": "READY",
				"d": map[string]any{"session_id": f.sessionID, "version": 1},
			})
			if f.closeAfterGo {
				conn.Close(websocket.StatusGoingAway, "bye")
				return
			}
		case OpResume:
			f.mu.Lock()
			f.resumes++
			reject := f.rejectResume
			f.mu.Unlock()
			if reject {
				conn.Close(websocket.StatusCode(codeInvalidSession), "resume refused")
				return
			}
			send(map[string]any{"op": OpDispatch, "s": 8, "t": "RESUMED", "d": ""})
		case OpHeartbeat:
			var d json.Number
			_ = json.Unmarshal(msg.D, &d)
			if n, err := d.Int64(); err == nil {
				f.mu.Lock()
				if n > f.lastSeq {
					f.lastSeq = n
				}
				f.mu.Unlock()
			}
			if !f.suppressAck {
				send(map[string]any{"op": OpHeartbeatAck})
			}
		}
	}
}

// waitFor 轮询直到 cond 为真，返回是否等到。
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

func (f *fakeGateway) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeGateway) countOf(op int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.frames {
		if v, ok := m["op"].(float64); ok && int(v) == op {
			n++
		}
	}
	return n
}

func (f *fakeGateway) firstOf(op int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.frames {
		if v, ok := m["op"].(float64); ok && int(v) == op {
			return m
		}
	}
	return nil
}

func testConfig(f *fakeGateway, tweak func(*GatewayConfig)) GatewayConfig {
	cfg := GatewayConfig{
		Tokens:  NewTokenSource("app", "secret", f.srv.URL, &http.Client{}),
		URL:     f.wsURL(),
		Intents: IntentPublicMessages,
		// Shard 刻意不填：让所有网关测试都走 NewGateway 的默认分片兜底，
		// 端到端那条 shard 断言才有校验对象。
		AckMisses:       2,
		MinHealthy:      time.Hour, // 测试期间不让退避计数归零
		ResumeBackoff:   []time.Duration{5 * time.Millisecond},
		IdentifyBackoff: []time.Duration{5 * time.Millisecond},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return cfg
}

func TestGatewayIdentifyThenHeartbeat(t *testing.T) {
	f := newFakeGateway(t)
	gotEvents := make(chan Event, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_ = NewGateway(testConfig(f, nil)).Run(ctx, func(_ context.Context, ev Event) error {
			gotEvents <- ev
			return nil
		})
	}()

	if !waitFor(2*time.Second, func() bool {
		return f.countOf(OpIdentify) > 0 && f.countOf(OpHeartbeat) > 0
	}) {
		t.Fatalf("没看到 identify/heartbeat，实际 identify=%d heartbeat=%d",
			f.countOf(OpIdentify), f.countOf(OpHeartbeat))
	}

	d, _ := f.firstOf(OpIdentify)["d"].(map[string]any)
	if d["intents"] != float64(IntentPublicMessages) {
		t.Errorf("intents = %v, 期望 %d", d["intents"], IntentPublicMessages)
	}
	if tok, _ := d["token"].(string); tok != "QQBot test-token" {
		t.Errorf("token = %q, 期望 \"QQBot test-token\"（验证 token 确实一路透传到 identify）", tok)
	}
	shard, _ := d["shard"].([]any)
	if len(shard) != 2 {
		t.Errorf("shard = %v, 期望两个整数 [shard_id, num_shards]", d["shard"])
	}

	f.mu.Lock()
	seq := f.lastSeq
	f.mu.Unlock()
	if seq != 7 {
		t.Errorf("心跳携带的 seq = %d, 期望是 READY 里的 7", seq)
	}

	select {
	case ev := <-gotEvents:
		if ev.Type != "READY" {
			t.Errorf("READY 不该作为业务事件上抛，得到 %q", ev.Type)
		}
	default:
		// READY 被状态机自己消化掉了，正是期望行为
	}
}

// 手机休眠后 socket 假活，读不会报错；只有 ack 看门狗能把连接救回来。
func TestGatewayDetectsDeadLinkWithoutAck(t *testing.T) {
	f := newFakeGateway(t)
	f.suppressAck = true

	var (
		mu   sync.Mutex
		logs []string
	)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	started := time.Now()
	// Run 的职责是持续重连，不会把判死当返回值抛出来，
	// 所以这里断言"确实因判死重连过"，而不是断言它报错退出。
	_ = NewGateway(testConfig(f, func(c *GatewayConfig) {
		c.Logf = func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		}
	})).Run(ctx, func(context.Context, Event) error { return nil })

	elapsed := time.Since(started)
	if got := f.connections(); got < 2 {
		t.Fatalf("心跳长期无应答却没有重连，建联次数=%d", got)
	}
	// AckMisses=2、心跳 20ms → 判死应当在百毫秒量级，而不是撑满整个窗口
	if elapsed < 100*time.Millisecond {
		t.Errorf("判死过快（%v），怀疑没等够两个心跳周期就断了", elapsed)
	}

	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "假活") {
		t.Errorf("重连原因应当是心跳判死，实际日志：\n%s", joined)
	}
}

// READY 之后对端关连接：下一次必须 resume，不能白烧一次建联配额。
func TestGatewayResumesAfterDrop(t *testing.T) {
	f := newFakeGateway(t)
	f.closeAfterGo = true

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_ = NewGateway(testConfig(f, nil)).Run(ctx, func(context.Context, Event) error { return nil })
	}()

	if !waitFor(2*time.Second, func() bool { return f.countOf(OpResume) > 0 }) {
		t.Fatalf("掉线后没有尝试 resume，identify 次数=%d", f.countOf(OpIdentify))
	}
	if f.countOf(OpIdentify) != 1 {
		t.Errorf("掉线重连不该重新 identify（会白烧建联配额），identify 次数=%d", f.countOf(OpIdentify))
	}

	d, _ := f.firstOf(OpResume)["d"].(map[string]any)
	if d["session_id"] != f.sessionID {
		t.Errorf("resume 的 session_id = %v, 期望 %q", d["session_id"], f.sessionID)
	}
	if s, _ := d["seq"].(float64); s == 0 {
		t.Error("resume 必须带 seq，否则平台不知道从哪儿补发")
	}
	cancel()
}

// resume 被拒（4006）后必须降级 identify，而不是死磕 resume。
func TestGatewayFallsBackToIdentifyWhenResumeRefused(t *testing.T) {
	f := newFakeGateway(t)
	f.rejectResume = true
	f.closeAfterGo = true

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_ = NewGateway(testConfig(f, nil)).Run(ctx, func(context.Context, Event) error { return nil })
	}()

	if !waitFor(2*time.Second, func() bool { return f.countOf(OpIdentify) >= 2 }) {
		t.Fatalf("resume 被拒后没有退回 identify，identify=%d resume=%d",
			f.countOf(OpIdentify), f.countOf(OpResume))
	}
	cancel()
}

// op 9 判定会话无效后，必须丢弃会话、作废 token 并重新 identify；
// 反复被判无效时要有尽头，不能无限白烧建联配额。
func TestGatewayHandlesInvalidSession(t *testing.T) {
	f := newFakeGateway(t)
	f.op9Immediately = true

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := NewGateway(testConfig(f, func(c *GatewayConfig) {
		c.IdentifyBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	})).Run(ctx, func(context.Context, Event) error { return nil })

	if !errors.Is(err, ErrGiveUp) {
		t.Fatalf("反复 op 9 应当以 ErrGiveUp 收摊，实际 %v", err)
	}
	if got := f.connections(); got < 3 {
		t.Errorf("建联次数 = %d, 期望至少重试 3 次（说明会话没被正确丢弃，或退避没走满）", got)
	}
}

// 退避决策：会话在就走短的 resume 档；预算用尽必须丢弃会话而不是无限白等。
func TestBackoffTiering(t *testing.T) {
	g := NewGateway(GatewayConfig{
		ResumeBackoff:   []time.Duration{time.Second},
		IdentifyBackoff: []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Millisecond},
		MinHealthy:      time.Hour,
	})

	if g.canResume() {
		t.Error("没有 session_id 时不该能 resume")
	}
	if d, _ := g.nextDelay(); d != 10*time.Second {
		t.Errorf("首次 identify 退避 = %v, 期望 10s", d)
	}
	if d, _ := g.nextDelay(); d != 20*time.Second {
		t.Errorf("第二次 = %v, 期望翻倍到 20s", d)
	}

	g.sessionID, g.seq = "s", 42
	if !g.canResume() {
		t.Error("有 session_id 和 seq 时应优先 resume，省一次建联配额")
	}
	if d, _ := g.nextDelay(); d != time.Second {
		t.Errorf("能 resume 时应走更短的 resume 档, 得到 %v", d)
	}
	if d, _ := g.nextDelay(); d != 40*time.Millisecond {
		t.Errorf("resume 档用尽后应降级到 identify 档, 得到 %v", d)
	}
	if g.canResume() {
		t.Error("resume 预算用尽后必须丢弃会话")
	}
	if _, ok := g.nextDelay(); ok {
		t.Error("identify 预算也用尽后必须报 ErrGiveUp 而不是继续")
	}
}

// 连上就被踢不该清零退避计数 —— 这正是他 Java 版失效的地方。
func TestFailureCountOnlyResetsAfterMinHealthy(t *testing.T) {
	f := newFakeGateway(t)
	f.closeAfterGo = true
	f.rejectResume = true

	g := NewGateway(testConfig(f, func(c *GatewayConfig) {
		c.MinHealthy = time.Hour // 秒断永远不算健康
		c.IdentifyBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = g.Run(ctx, func(context.Context, Event) error { return nil })

	if g.identifyFailures < 2 {
		t.Errorf("反复秒断后 identifyFailures = %d, 期望持续增长而不是被清零", g.identifyFailures)
	}
}
