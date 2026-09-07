package qq

import (
	"sync"
	"time"
)

// 入站消息去重。
//
// 为什么必须有：官方 websocket 文档写明「断开之后短时间内重连会补发中间遗漏的事件」，
// resume 会带上 last seq，补发回来的就是原样重复的报文。这是机制，不是偶发故障，
// 所以去重必须常开。
//
// 为什么必须挡在回复之前：被动回复要求 msg_id + msg_seq 组合不重复，我们的回复器
// 每次自动递增 seq。于是一条被补发的重复消息不但会被再处理一遍，还会因为 seq 换了值
// 而真的再发出一条用户可见的回复——去重漏在上游，下游的 msg_seq 递增会把它放大成两倍。
//
// 为什么键里要带事件类型：入站报文里没有 msg_seq 可用来区分同一 id 的两次推送，
// 只有 (事件类型, 消息 id) 这对组合能既挡住重复、又不让同 id 的不同事件互相吞掉。
type dedupKey struct {
	kind string
	id   string
}

type DedupStats struct {
	Admitted  uint64 // 首次见到、放行处理
	Dropped   uint64 // 窗口内重复、已挡下
	Evicted   uint64 // 因容量上限被淘汰（>0 说明窗口或容量不够用）
	Forgotten uint64 // 被 Forget 主动撤掉的条数
	Live      int    // 当前表内键数
}

// DedupConfig 去重参数。零值表示用默认值。
type DedupConfig struct {
	// Window 是「多久之内的同 id 重复算重复」。默认 5 分钟。
	//
	// 这个数的依据是补发的来源：只有 resume 会补发，而我们的 resume 重连预算是
	// 1s+2s+4s+8s ≈ 15s，之后就只能重新 identify 拿新 session、不再补发。
	// 5 分钟相对最坏情况留了 20 倍余量。它不是从平台文档里查到的回复窗口，
	// 如果以后要让去重覆盖被动回复时效，得先去一手文档核实那个数字再改这里。
	Window time.Duration

	// MaxKeys 是表内键数上限，超出后按插入顺序淘汰最老的。默认 8192。
	//
	// 淘汰意味着极端流量下老键可能提前出局、让一条迟到重复漏过去。
	// 这是用有界内存换来的代价，Evicted 计数会把这种情况暴露出来。
	MaxKeys int

	Now func() time.Time
}

const (
	defaultDedupWindow  = 5 * time.Minute
	defaultDedupMaxKeys = 8192
)

type Deduper struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time

	seen map[dedupKey]dedupEntry
	ring []dedupKey // 插入顺序环，用于 O(1) 淘汰最老的
	head int

	admitted, dropped, evicted, forgotten uint64
}

// dedupEntry 记下键所在的环形槽位：Forget 要能把那个槽清空，
// 否则环形缓冲绕回来时会把「后来重新登记的同一条」当成老键误删。
type dedupEntry struct {
	at   time.Time
	slot int
}

func NewDeduper(cfg DedupConfig) *Deduper {
	if cfg.Window <= 0 {
		cfg.Window = defaultDedupWindow
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = defaultDedupMaxKeys
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Deduper{
		window: cfg.Window,
		now:    cfg.Now,
		seen:   make(map[dedupKey]dedupEntry, cfg.MaxKeys),
		ring:   make([]dedupKey, cfg.MaxKeys),
	}
}

// See 返回 true 表示这条消息第一次见到、应当继续处理；false 表示窗口内的重复投递。
//
// 语义是「见到即登记」，调用后不要指望再拿到一次 true。没有 id 的事件不参与去重，
// 一律放行——否则会把这些事件全吞了。
func (d *Deduper) See(kind, id string) bool {
	if id == "" {
		return true
	}
	k := dedupKey{kind: kind, id: id}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	if e, ok := d.seen[k]; ok {
		if now.Sub(e.at) < d.window {
			d.dropped++
			return false
		}
		// 窗口已过，原地刷新时间、沿用原槽位，不占新位置。
		d.seen[k] = dedupEntry{at: now, slot: e.slot}
		d.admitted++
		return true
	}

	slot := d.head
	if old := d.ring[slot]; old != (dedupKey{}) {
		if _, live := d.seen[old]; live {
			delete(d.seen, old)
			d.evicted++
		}
	}
	d.ring[slot] = k
	d.head = (d.head + 1) % len(d.ring)
	d.seen[k] = dedupEntry{at: now, slot: slot}
	d.admitted++
	return true
}

// Forget 撤掉一条登记，让这条消息的重投能重新被放行。
//
// 为什么必须有：webhook 前端的失败恢复靠的是「回 d=1 让平台重投」，
// 但如果处理器已经失败、登记还留在表里，重投回来会被判成重复而挡掉 ——
// 那是「去重把自己的重试通道堵死」，消息就此永久丢失。
// 所以「要让平台重试」的路径必须先 Forget 再返回错误。
func (d *Deduper) Forget(kind, id string) {
	if id == "" {
		return
	}
	k := dedupKey{kind: kind, id: id}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.seen[k]
	if !ok {
		return
	}
	delete(d.seen, k)
	if d.ring[e.slot] == k {
		d.ring[e.slot] = dedupKey{}
	}
	d.forgotten++
}

func (d *Deduper) Stats() DedupStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return DedupStats{
		Admitted:  d.admitted,
		Dropped:   d.dropped,
		Evicted:   d.evicted,
		Forgotten: d.forgotten,
		Live:      len(d.seen),
	}
}

// messageEvents 是需要解码 + 去重的事件类型集合。
var messageEvents = map[string]bool{
	EventGroupAtMessage: true,
	EventGroupMessage:   true,
	EventC2CMessage:     true,
}

// IsMessageEvent 判断这个事件类型是否携带一条用户消息。
//
// 其余类型（READY、RESUMED、 guild 类、平台以后新增的）都不带 id 字段，
// 走消息解码路径会当场报错，必须分开走。
func IsMessageEvent(kind string) bool { return messageEvents[kind] }
