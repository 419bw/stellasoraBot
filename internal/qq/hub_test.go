package qq_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	. "xingta/internal/qq"
)

func groupEvent(t *testing.T, seq int64) Event {
	t.Helper()
	return Event{Seq: seq, Type: EventGroupAtMessage, Data: loadFixture(t, "group_at_message_create.json")}
}

func TestHubDispatchesDecodedMessage(t *testing.T) {
	h := NewHub(nil, nil)
	var got []*Message
	h.OnMessage(EventGroupAtMessage, func(_ context.Context, m *Message) error {
		got = append(got, m)
		return nil
	})

	if err := h.Handle(context.Background(), groupEvent(t, 7)); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("处理器跑了 %d 次，想要 1 次", len(got))
	}
	m := got[0]
	if m.Kind != EventGroupAtMessage || m.ID == "" {
		t.Errorf("处理器收到的消息不完整: kind=%q id=%q", m.Kind, head(m.ID, 12))
	}
	if m.Text() != "你好" {
		t.Errorf("Text() = %q", m.Text())
	}
	s := h.Stats()
	if s.Admitted != 1 || s.DupDropped != 0 || s.DecodeFail != 0 || s.HandlerErr != 0 {
		t.Errorf("Stats = %+v，想要只有 Admitted=1", s)
	}
	if s.ByType[EventGroupAtMessage] != 1 {
		t.Errorf("ByType = %v", s.ByType)
	}
}

// 这条是去重层存在的理由：resume 补发同一条报文时，业务处理器只该跑一次。
func TestHubDropsReplayedDuplicate(t *testing.T) {
	h := NewHub(nil, nil)
	var calls int
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error {
		calls++
		return nil
	})

	// 原始投递 seq=7，补发的那条 id 完全相同、seq 更大。
	for _, seq := range []int64{7, 9, 10} {
		if err := h.Handle(context.Background(), groupEvent(t, seq)); err != nil {
			t.Fatalf("Handle 返回错误: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("处理器跑了 %d 次，去重应当只放行 1 次", calls)
	}
	s := h.Stats()
	if s.Admitted != 1 || s.DupDropped != 2 {
		t.Errorf("Stats = %+v，想要 admitted=1 dropped=2", s)
	}
}

func TestHubDoesNotLeakHandlerErrorToTransport(t *testing.T) {
	// 处理器报错如果被当成传输层错误返回，网关会断开重连，
	// 于是「一个业务 bug」演变成「机器人反复掉线」。这里必须钉住不外传。
	var logs []string
	h := NewHub(nil, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	boom := errors.New("boom")
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error { return boom })

	if err := h.Handle(context.Background(), groupEvent(t, 7)); err != nil {
		t.Fatalf("Handle 把处理器错误外传了: %v", err)
	}
	if s := h.Stats(); s.HandlerErr != 1 {
		t.Errorf("HandlerErr = %d，想要 1", s.HandlerErr)
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "boom") {
		t.Errorf("错误没进日志: %v", logs)
	}
}

func TestHubContinuesAfterFailingHandler(t *testing.T) {
	h := NewHub(nil, nil)
	var second int
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error { return errors.New("x") })
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error { second++; return nil })

	h.Handle(context.Background(), groupEvent(t, 7))
	if second != 1 {
		t.Errorf("前一个处理器报错后，第二个跑了 %d 次，应当照常执行", second)
	}
}

func TestHubDecodeFailureIsCountedAndNotDispatched(t *testing.T) {
	h := NewHub(nil, func(string, ...any) {})
	var calls int
	h.OnMessage(EventC2CMessage, func(context.Context, *Message) error { calls++; return nil })

	// 缺 id 的报文：可能是平台改了结构，不能当正常消息喂给业务。
	err := h.Handle(context.Background(), Event{
		Seq: 3, Type: EventC2CMessage, Data: []byte(`{"content":"hi"}`),
	})
	if err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}
	if calls != 0 {
		t.Error("解码失败的报文不该进业务处理器")
	}
	if s := h.Stats(); s.DecodeFail != 1 || s.Admitted != 0 {
		t.Errorf("Stats = %+v，想要 DecodeFail=1", s)
	}
}

func TestHubNonMessageEventsSkipDedup(t *testing.T) {
	h := NewHub(nil, nil)
	var ready, resumed int
	h.OnEvent(EventReady, func(context.Context, Event) error { ready++; return nil })
	h.OnEvent(EventResumed, func(context.Context, Event) error { resumed++; return nil })

	// READY 报文没有消息 id；RESUMED 的 d 甚至是空字符串。若误走消息路径会当场解码失败。
	for _, ev := range []Event{
		{Seq: 1, Type: EventReady, Data: []byte(`{"version":1,"session_id":"sess-x"}`)},
		{Seq: 2, Type: EventResumed, Data: []byte(`""`)},
		{Seq: 3, Type: EventResumed, Data: []byte(`""`)},
	} {
		if err := h.Handle(context.Background(), ev); err != nil {
			t.Fatalf("Handle(%s) 返回错误: %v", ev.Type, err)
		}
	}
	if ready != 1 || resumed != 2 {
		t.Errorf("ready=%d resumed=%d，想要 1 和 2", ready, resumed)
	}
	s := h.Stats()
	if s.DecodeFail != 0 || s.DupDropped != 0 || s.Admitted != 0 {
		t.Errorf("非消息事件不该动消息计数: %+v", s)
	}
	if s.Dedup.Live != 0 {
		t.Errorf("非消息事件不该进去重表: %+v", s.Dedup)
	}
}

func TestHubIgnoresUnsubscribedTypes(t *testing.T) {
	// 平台以后加新事件类型时，入口必须只是计数而不是报错。
	h := NewHub(nil, func(string, ...any) {})
	err := h.Handle(context.Background(), Event{
		Seq: 1, Type: "SOME_FUTURE_EVENT", Data: []byte(`{"whatever":true}`),
	})
	if err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}
	s := h.Stats()
	if s.ByType["SOME_FUTURE_EVENT"] != 1 || s.DecodeFail != 0 {
		t.Errorf("Stats = %+v", s)
	}
}

func TestHubRunsHandlersInArrivalOrder(t *testing.T) {
	h := NewHub(nil, nil)
	var mu sync.Mutex
	var order []string
	h.OnMessage(EventGroupAtMessage, func(_ context.Context, m *Message) error {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, m.Text())
		return nil
	})

	texts := []string{"第一条", "第二条", "第三条"}
	for i, txt := range texts {
		payload := fmt.Sprintf(`{"id":"m%d","content":%q,"timestamp":"2026-09-04T12:00:00+08:00",`+
			`"message_type":0,"group_openid":"G1","author":{"id":"U1","username":"迪西","bot":false}}`, i, txt)
		if err := h.Handle(context.Background(), Event{
			Seq: int64(i + 1), Type: EventGroupAtMessage, Data: []byte(payload),
		}); err != nil {
			t.Fatalf("Handle 返回错误: %v", err)
		}
	}
	if strings.Join(order, ",") != strings.Join(texts, ",") {
		t.Errorf("处理器收到顺序 = %v，想要投递顺序 %v", order, texts)
	}
}

func TestHubConcurrentHandle(t *testing.T) {
	// webhook 前端可能并发回调，入口不能在计数和去重上出竞态。
	h := NewHub(nil, func(string, ...any) {})
	var admitted int64
	var mu sync.Mutex
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error {
		mu.Lock()
		admitted++
		mu.Unlock()
		return nil
	})

	const workers = 16
	const each = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				payload := fmt.Sprintf(`{"id":"w%d-m%d","content":"x",`+
					`"timestamp":"2026-09-04T12:00:00+08:00","message_type":0,"group_openid":"G1",`+
					`"author":{"id":"U1","username":"n","bot":false}}`, w, i)
				_ = h.Handle(context.Background(), Event{
					Seq: int64(i), Type: EventGroupAtMessage, Data: []byte(payload),
				})
			}
		}(w)
	}
	wg.Wait()

	if admitted != workers*each {
		t.Errorf("放行 %d 条，想要 %d 条（不许丢也不许重）", admitted, workers*each)
	}
	s := h.Stats()
	if s.Admitted != workers*each || s.DupDropped != 0 || s.DecodeFail != 0 {
		t.Errorf("Stats = %+v", s)
	}
}

func TestHubSharesDedupAcrossFrontends(t *testing.T) {
	// 同一条消息从 websocket 和 webhook 两边各来一次，也只能处理一次。
	d := NewDeduper(DedupConfig{})
	a := NewHub(d, nil)
	b := NewHub(d, nil)
	var n int
	var mu sync.Mutex
	fn := func(context.Context, *Message) error { mu.Lock(); n++; mu.Unlock(); return nil }
	a.OnMessage(EventGroupAtMessage, fn)
	b.OnMessage(EventGroupAtMessage, fn)

	_ = a.Handle(context.Background(), groupEvent(t, 7))
	_ = b.Handle(context.Background(), groupEvent(t, 7))
	if n != 1 {
		t.Errorf("两个前端合计处理 %d 次，共用去重器时应为 1 次", n)
	}
}

// webhook 前端的失败恢复靠平台重投，所以处理器报错时必须把去重登记撤掉，
// 否则重投回来会被自己的记录挡掉 —— 那是去重把重试通道堵死。
func TestWebhookHandlerForgetsOnFailure(t *testing.T) {
	h := NewHub(nil, func(string, ...any) {})
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error {
		return errors.New("渲染失败")
	})

	ev := groupEvent(t, 7)
	if err := h.WebhookHandler()(context.Background(), ev.Type, "", ev.Data); err == nil {
		t.Fatal("webhook 路径的处理器错误必须外传，否则平台不会重投")
	}
	s := h.Stats()
	if s.Forgotten != 1 || s.Dedup.Forgotten != 1 {
		t.Errorf("Stats = %+v，想要 Forgotten=1", s)
	}
	// 重投必须能重新处理一次。
	if err := h.WebhookHandler()(context.Background(), ev.Type, "", ev.Data); err == nil {
		t.Error("重投没能重新进入处理器")
	}
	if got := h.Stats().Admitted; got != 2 {
		t.Errorf("Admitted = %d，想要 2（原始一次 + 重投一次）", got)
	}
}

func TestWebhookHandlerKeepsDedupOnSuccess(t *testing.T) {
	h := NewHub(nil, func(string, ...any) {})
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error { return nil })

	ev := groupEvent(t, 7)
	for i := 0; i < 2; i++ {
		if err := h.WebhookHandler()(context.Background(), ev.Type, "", ev.Data); err != nil {
			t.Fatalf("第 %d 次回调返回错误: %v", i+1, err)
		}
	}
	s := h.Stats()
	if s.Admitted != 1 || s.DupDropped != 1 || s.Forgotten != 0 {
		t.Errorf("Stats = %+v，想要 放行 1 判重 1 撤销 0", s)
	}
}

func TestWebsocketHandlerDoesNotPropagateError(t *testing.T) {
	// 与 webhook 相反：WS 路径报错不外传（外传会让网关断线重连），
	// 但也不该撤销登记 —— 那条链路没有重投，撤销只会让同一条消息再被处理一次。
	h := NewHub(nil, func(string, ...any) {})
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error {
		return errors.New("boom")
	})
	ev := groupEvent(t, 7)
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("WS 路径把错误外传了: %v", err)
	}
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("第二次: %v", err)
	}
	s := h.Stats()
	if s.Forgotten != 0 || s.DupDropped != 1 {
		t.Errorf("Stats = %+v，WS 路径应当保持登记", s)
	}
}

func TestBothFrontendsShareOneHandler(t *testing.T) {
	// 「一个后端、两个前端」的实际校验：同一个 Hub 同时挂 WS 和 webhook，
	// 两边各来一条同 id 的消息，业务处理器只跑一次。
	h := NewHub(nil, func(string, ...any) {})
	var n int
	var mu sync.Mutex
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error {
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	})
	ev := groupEvent(t, 7)

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("WS 入口失败: %v", err)
	}
	if err := h.WebhookHandler()(context.Background(), ev.Type, "", ev.Data); err != nil {
		t.Fatalf("webhook 入口失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Errorf("业务处理器跑了 %d 次，共用入口时应为 1 次", n)
	}
}

// 处理器 panic 在 hub 边界就地吸收：Handle 返回 nil（外抛会让网关断连重连）、
// 同事件后续处理器与后续事件照常、计数单列不混入 HandlerErr、日志带栈。
// 变异负对照：注释掉 invokeHandler 的 recover，本测试必红（进程炸）。
func TestHubAbsorbsHandlerPanic(t *testing.T) {
	var logs []string
	h := NewHub(nil, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	var after []string
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error {
		panic("处理器炸了")
	})
	h.OnMessage(EventGroupAtMessage, func(_ context.Context, m *Message) error {
		after = append(after, m.Text())
		return nil
	})

	if err := h.Handle(context.Background(), groupEvent(t, 1)); err != nil {
		t.Fatalf("Handle 对处理器 panic 必须返回 nil，得到: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("panic 后同事件的后续处理器未执行: %v", after)
	}

	// 派发循环还活着：下一事件照常处理（c2c 的 Kind 不同，不会被去重挡下）
	c2cEv := Event{Seq: 2, Type: EventC2CMessage, Data: loadFixture(t, "c2c_message_create.json")}
	h.OnMessage(EventC2CMessage, func(_ context.Context, m *Message) error {
		after = append(after, m.Text())
		return nil
	})
	if err := h.Handle(context.Background(), c2cEv); err != nil {
		t.Fatalf("panic 后 Handle 返回错误: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("panic 后续事件未照常处理: %v", after)
	}

	s := h.Stats()
	if s.HandlerPanic != 1 {
		t.Errorf("HandlerPanic = %d, want 1（第二条事件不 panic）", s.HandlerPanic)
	}
	if s.HandlerErr != 0 {
		t.Errorf("panic 不得计入 HandlerErr: %+v", s)
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "panic") || !strings.Contains(joined, "处理器炸了") {
		t.Errorf("panic 日志缺标记或原值: %q", joined)
	}
	if !strings.Contains(joined, "goroutine ") {
		t.Error("panic 日志缺调用栈")
	}
}
