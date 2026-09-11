// Package calexpiry 是「活动到期提醒」功能：活动快结束时主动往目标群/私聊发一条。
//
// 分层契约的执行点：这个功能**自己决定**要存什么——"哪些活动已经提醒过"写进
// store.Doc 的 "calexpiry" 命名空间，基础设施既不认识"提醒"这个概念，也不为它
// 预建 schema。没有这份记录，每次重启都会把还在窗口里的活动重新提醒一遍。
//
// 它不注册任何命令，所以构造时也拿不到命令注册面：功能拿到什么能力由 main 注入决定。
package calexpiry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"xingta/internal/feature/text"
	"xingta/internal/kernel"
	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/target"
	"xingta/internal/store"
)

// NS 是这个功能在 store.Doc 里占的命名空间。键是活动 ID，值是提醒时刻。
const NS = "calexpiry"

// Config 的零值必须可用（除了 Targets：空 = 只记日志不发送）。
type Config struct {
	Lead  time.Duration // 提前多久提醒，默认 48h
	Every time.Duration // 扫描间隔，默认 10m

	// Targets 是主动消息的投递目标，形如 "g:<group_openid>" / "u:<user_openid>"，
	// 由发送侧（main 里的 Sink）解释前缀。空表示只记日志——还没决定往哪发时，
	// 提醒链路照样能跑通并留下痕迹，不会静默地什么都不做。
	Targets []string

	Zone *time.Location
	Now  func() time.Time
	Logf func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.Lead <= 0 {
		c.Lead = 48 * time.Hour
	}
	if c.Every <= 0 {
		c.Every = 10 * time.Minute
	}
	if c.Zone == nil {
		c.Zone = time.FixedZone("CST", 8*60*60)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

type feature struct {
	doc store.Doc
	cfg Config

	mu    sync.Mutex
	sent  map[string]time.Time // 活动 ID → 提醒时刻，重启后从 doc 恢复
	armed map[string]bool      // 已经排上期、还没触发的活动 ID
}

func New(doc store.Doc, cfg Config) kernel.Feature {
	c := cfg.withDefaults()
	return &feature{
		doc:   doc,
		cfg:   c,
		sent:  make(map[string]time.Time),
		armed: make(map[string]bool),
	}
}

func (f *feature) Name() string { return "calexpiry" }

// Start 非阻塞（kernel.Feature 契约）：扫描循环自己起 goroutine 并尊重 ctx。
func (f *feature) Start(ctx context.Context, api kernel.API) error {
	if f.doc == nil {
		return errors.New("calexpiry: doc 不能为空（要靠它记住已提醒过的活动）")
	}
	if api == nil {
		return errors.New("calexpiry: 需要 kernel.API 才能排期与投递")
	}
	_ = api.RegisterTopic(target.Topic{
		Key:  "expiry",
		Name: "活动到期提醒",
		Desc: "活动结束前48小时文字提醒",
	})
	if err := f.loadSent(); err != nil {
		return fmt.Errorf("calexpiry: 读已提醒记录: %w", err)
	}
	go f.loop(ctx, api)
	return nil
}

// loadSent 把上一次进程的提醒记录捞回来。
//
// 值解不开时按"已提醒"处理：键在就说明这个活动确实发过一次，重发只会刷屏，
// 而漏发的那一次已经漏了。这是"功能自决存储"要交的税——Doc 的 JSON 值没有
// 编译期 schema 约束，改结构体字段时旧数据只能这样容错。
func (f *feature) loadSent() error {
	return f.doc.Scan(NS, "", func(key string, raw []byte) error {
		var at time.Time
		if err := json.Unmarshal(raw, &at); err != nil {
			f.cfg.Logf("calexpiry: 已提醒记录 %s 解不开，按已提醒处理: %v", key, err)
			f.sent[key] = time.Time{}
			return nil
		}
		f.sent[key] = at
		return nil
	})
}

func (f *feature) loop(ctx context.Context, api kernel.API) {
	f.scan(api) // 立刻扫一遍：重启后仍在窗口里的活动马上补上提醒
	t := time.NewTicker(f.cfg.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.scan(api)
		}
	}
}

// scan 把"已进入提醒窗口且没提醒过"的活动排上期，并撤掉已经离开窗口的排期。
//
// EndingWithin 只返回正在进行、且 End 在 now+Lead 之内的活动，所以 End-Lead 必然
// 不晚于 now —— 排期几乎总是立刻触发。仍然走 Schedule 而不是直接 Submit，是为了
// 拿到两件事：按 ID 幂等（每 10 分钟扫一次不会重复排），以及活动改期时自动重排。
func (f *feature) scan(api kernel.API) {
	now := f.cfg.Now()
	list := api.Calendar().EndingWithin(now, f.cfg.Lead)

	seen := make(map[string]bool, len(list))
	for _, a := range list {
		seen[a.ID] = true
		if f.done(a.ID) {
			continue
		}
		act := a
		api.Schedule(taskID(act.ID), act.End.Add(-f.cfg.Lead), func(ctx context.Context) error {
			return f.remind(ctx, api, act)
		})
		f.arm(act.ID)
	}
	f.disarmGone(api, seen)
	f.pruneExpired(api, now)
}

// remind 是排期触发时跑的：投递到所有目标，全部成功后才记下"已提醒"。
//
// 中途失败就不记，下一轮扫描会重新排期重投——宁可极小概率重复一条，
// 也不要因为队列满而静默漏掉一个到期提醒。
func (f *feature) remind(ctx context.Context, api kernel.API, a calendar.Activity) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := f.cfg.Now()
	msg := f.message(a, now)

	targets := f.targets(api)
	if len(targets) == 0 {
		f.cfg.Logf("calexpiry: 未配置提醒目标，只记日志 → %s", text.OneLine(msg))
		f.markSent(a.ID, now)
		return nil
	}

	for _, target := range targets {
		// Topic 相同 + Mergeable：同一轮里多个活动一起到期时，队列把它们合成一条发出去
		item := queue.Item{
			ID:        "expiry:" + a.ID + "@" + target,
			Target:    target,
			Text:      msg,
			Topic:     "expiry",
			Mergeable: true,
		}
		if err := api.Submit(item); err != nil {
			return fmt.Errorf("calexpiry: 投递 %s 到 %s 失败: %w", a.ID, target, err)
		}
	}
	f.markSent(a.ID, now)
	f.cfg.Logf("calexpiry: 已提醒 %s（%s），投给 %d 个目标", a.ID, a.Title, len(targets))
	return nil
}

func (f *feature) targets(api kernel.API) []string {
	if api != nil {
		if t := api.TargetsFor("expiry"); len(t) > 0 {
			return t
		}
	}
	return f.cfg.Targets
}

func (f *feature) message(a calendar.Activity, now time.Time) string {
	s := fmt.Sprintf("到期提醒：「%s」%s 结束，剩 %s",
		a.Title, text.Clock(a.End, now, f.cfg.Zone), text.Human(a.End.Sub(now)))
	if a.URL != "" {
		s += "\n" + a.URL
	}
	return s
}

// disarmGone 撤掉已经不在窗口里的排期：活动被隐藏、被同步删掉，或者干脆已经结束。
// 任务已经触发过的话 Cancel 返回 false，这是常态，不值得记一笔。
func (f *feature) disarmGone(api kernel.API, seen map[string]bool) {
	f.mu.Lock()
	var gone []string
	for id := range f.armed {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	for _, id := range gone {
		delete(f.armed, id)
	}
	f.mu.Unlock()

	for _, id := range gone {
		if api.Cancel(taskID(id)) {
			f.cfg.Logf("calexpiry: 活动 %s 已离开提醒窗口，撤销排期", id)
		}
	}
}

// pruneExpired 清理已经真正到期的活动记录：
// 活动一旦彻底结束，EndingWithin 绝不会再吐出它，留在 sent 和 doc 里的记录已无防重价值。
func (f *feature) pruneExpired(api kernel.API, now time.Time) {
	cal := api.Calendar()
	if cal == nil {
		return
	}

	f.mu.Lock()
	var toDelete []string
	for id, sentAt := range f.sent {
		if act, ok := cal.Get(id); ok {
			if now.After(act.End) {
				toDelete = append(toDelete, id)
			}
		} else {
			if now.Sub(sentAt) > f.cfg.Lead+24*time.Hour {
				toDelete = append(toDelete, id)
			}
		}
	}
	for _, id := range toDelete {
		delete(f.sent, id)
	}
	f.mu.Unlock()

	for _, id := range toDelete {
		if err := f.doc.Delete(NS, id); err != nil {
			f.cfg.Logf("calexpiry: 删已到期记录 %s 失败: %v", id, err)
		} else {
			f.cfg.Logf("calexpiry: 活动 %s 已到期，清理提醒账本记录", id)
		}
	}
}

func (f *feature) done(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sent[id]
	return ok
}

func (f *feature) arm(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed[id] = true
}

// markSent 先落盘再改内存：落盘失败时内存里也记上，避免这一轮反复刷屏，
// 但重启后可能重发一次——日志里必须留下这条失败，否则重发查不出原因。
func (f *feature) markSent(id string, at time.Time) {
	if err := f.doc.Put(NS, id, at); err != nil {
		f.cfg.Logf("calexpiry: 写已提醒记录 %s 失败（重启后可能重发一次）: %v", id, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[id] = at
	delete(f.armed, id)
}

func taskID(activityID string) string { return "expiry:" + activityID }
