package qq

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// MessageHandler 处理一条已解码、已确认不是重复投递的消息。
type MessageHandler func(ctx context.Context, m *Message) error

// RawHandler 处理非消息类事件（READY / RESUMED / 平台以后新增的类型）。
type RawHandler func(ctx context.Context, ev Event) error

// Hub 是统一事件入口：websocket 网关和 webhook 回调两条前端把原始事件交给它，
// 解码、判重、分发在这里做完，后端功能不再关心自己收到的是哪种连接。
//
// 两条前端只在「谁产生 Event」这一步不同，从这里往后是同一份代码——
// 这就是「一个后端、两个前端」能成立的地方。
type Hub struct {
	dedup *Deduper
	logf  func(string, ...any)

	mu  sync.RWMutex
	msg map[string][]MessageHandler
	raw map[string][]RawHandler

	statMu     sync.Mutex
	admitted   uint64
	dupDropped uint64
	forgotten  uint64
	decodeFail uint64
	handlerErr uint64
	byType     map[string]uint64
}

func NewHub(dedup *Deduper, logf func(string, ...any)) *Hub {
	if dedup == nil {
		dedup = NewDeduper(DedupConfig{})
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Hub{
		dedup:  dedup,
		logf:   logf,
		msg:    make(map[string][]MessageHandler),
		raw:    make(map[string][]RawHandler),
		byType: make(map[string]uint64),
	}
}

// OnMessage 订阅某个消息事件类型。重复注册同一个类型会有多个处理器，按注册顺序依次调用。
func (h *Hub) OnMessage(kind string, fn MessageHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msg[kind] = append(h.msg[kind], fn)
}

// OnEvent 订阅非消息类事件。没注册处理器的类型只计数，不做处理。
func (h *Hub) OnEvent(kind string, fn RawHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.raw[kind] = append(h.raw[kind], fn)
}

func (h *Hub) Dedup() *Deduper { return h.dedup }

// Handle 是 websocket 前端的入口，签名正好是 Gateway.Run 的 onEvent。
//
// 返回值刻意不含业务错误：在这里返回 error 会让网关判定链路出问题并断开重连，
// 于是「某个处理器报错」会演变成「机器人反复掉线」。处理失败一律就地记日志 + 计数，
// 由 Stats 暴露出去。WS 这条链路平台不会重投，报错也没有重试可言。
//
// 处理器按到达顺序同步执行，不额外起 goroutine：并发派发会打乱同一会话里多条消息的
// 先后顺序，还会让去重判定和实际处理之间出现竞态。需要慢操作请丢给自己的队列。
func (h *Hub) Handle(ctx context.Context, ev Event) error {
	return h.dispatch(ctx, ev, false)
}

// WebhookHandler 是 webhook 前端的入口，直接交给 NewCallbackHandler 用。
//
// 与 WS 那条的唯一区别是失败处理：回调侧回 op 12 且 d=1 时平台会重投，
// 所以这里要把处理器错误往上抛，并且先把这条消息从去重表里撤掉 ——
// 不撤的话重投回来会被自己的去重记录挡掉，重试通道就被堵死了。
// 去重仍按报文里的消息 id，不按回调的 event id：官方要求的就是「相同 msg_id 可能
// 多次推送」，而 event id 只在回调信封上，两种前端拿到的不是同一个东西。
func (h *Hub) WebhookHandler() EventHandler {
	return func(ctx context.Context, eventType, eventID string, data json.RawMessage) error {
		return h.dispatch(ctx, Event{Type: eventType, Data: data}, true)
	}
}

func (h *Hub) dispatch(ctx context.Context, ev Event, retryOnFail bool) error {
	var out outcome
	out.kind = ev.Type
	defer func() { h.record(out) }()

	if !IsMessageEvent(ev.Type) {
		for _, fn := range h.rawHandlers(ev.Type) {
			if err := fn(ctx, ev); err != nil && ctx.Err() == nil {
				out.handlerErr++
				h.logf("qq: 事件 %s 的处理器返回错误: %v", ev.Type, err)
			}
		}
		return nil
	}

	m, err := DecodeMessage(ev.Type, ev.Data)
	if err != nil {
		// 这里刻意不回错误给平台：解不开是我们的结构体跟不上，重投一百次也一样解不开，
		// 返回错误只会让 webhook 侧无限重试。计数 + 日志留给人来看。
		out.decodeFail++
		h.logf("qq: 事件 %s 解码失败: %v", ev.Type, err)
		return nil
	}

	if !h.dedup.See(m.Kind, m.ID) {
		// 只计数不打日志：补发风暴恰恰是条数最多的时候，逐条打反而吃掉入口吞吐
		// （实测光是准备一条日志的参数就多 ~1.7µs / 5 次分配）。要看就读 Stats.DupDropped。
		out.dupDropped++
		return nil
	}

	out.admitted++
	var failed error
	for _, fn := range h.msgHandlers(ev.Type) {
		if err := fn(ctx, m); err != nil && ctx.Err() == nil {
			out.handlerErr++
			failed = err
			h.logf("qq: 事件 %s 的处理器返回错误: %v", ev.Type, err)
		}
	}
	if failed != nil && retryOnFail {
		h.dedup.Forget(m.Kind, m.ID)
		out.forgotten++
		return fmt.Errorf("qq: 处理 %s 失败，已撤销去重登记等待平台重投: %w", m.Kind, failed)
	}
	return nil
}

// outcome 是单个事件的处理结果，攒起来一次性记账，避免每个事件抢多次锁。
type outcome struct {
	kind       string
	admitted   uint64
	dupDropped uint64
	forgotten  uint64
	decodeFail uint64
	handlerErr uint64
}

func (h *Hub) record(o outcome) {
	h.statMu.Lock()
	defer h.statMu.Unlock()
	h.byType[o.kind]++
	h.admitted += o.admitted
	h.forgotten += o.forgotten
	h.dupDropped += o.dupDropped
	h.decodeFail += o.decodeFail
	h.handlerErr += o.handlerErr
}

func (h *Hub) msgHandlers(kind string) []MessageHandler {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.msg[kind]
}

func (h *Hub) rawHandlers(kind string) []RawHandler {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.raw[kind]
}

// HubStats 是入口侧的累计计数，给日志、压测报告和自检断言用。
type HubStats struct {
	Admitted   uint64            // 放行给业务处理器的消息数
	Forgotten  uint64            // 处理失败后撤销去重登记的条数（仅 webhook 前端）
	DupDropped uint64            // 因重复投递被挡下的消息数
	DecodeFail uint64            // 解码失败的事件数
	HandlerErr uint64            // 处理器返回错误的次数
	ByType     map[string]uint64 // 按事件类型计数（含未订阅的类型）
	Dedup      DedupStats
}

func (h *Hub) Stats() HubStats {
	h.statMu.Lock()
	byType := make(map[string]uint64, len(h.byType))
	for k, v := range h.byType {
		byType[k] = v
	}
	s := HubStats{
		Admitted:   h.admitted,
		Forgotten:  h.forgotten,
		DupDropped: h.dupDropped,
		DecodeFail: h.decodeFail,
		HandlerErr: h.handlerErr,
		ByType:     byType,
	}
	h.statMu.Unlock()
	s.Dedup = h.dedup.Stats()
	return s
}
