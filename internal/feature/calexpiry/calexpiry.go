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

// NS 是这个功能在 store.Doc 里占的命名空间。键是活动 ID，值是提醒账本记录。
const NS = "calexpiry"

// remindRec 是账本记录：键=活动 ID（裸键，不带目标），值=哪些目标收到了、是否全局了结。
// 写入纪律与 calposter/biliwatch 一致：锁内以内存镜像为准整条序列化 + Put，
// 不在盘上读改写——回执可能从两个队列 worker 并发回来。
type remindRec struct {
	SettledAt time.Time            `json:"settled_at,omitempty"` // 全局了结：全员收齐/无目标只记日志的轮次
	Targets   map[string]time.Time `json:"targets,omitempty"`    // 目标 → 回执时刻
}

// newest 返回记录里最新的时间戳，孤立记录的保护期以它为准。
func (r *remindRec) newest() time.Time {
	t := r.SettledAt
	for _, at := range r.Targets {
		if at.After(t) {
			t = at
		}
	}
	return t
}

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

	api   kernel.API // Start 之后才有：回执收敛时查当前订阅目标
	mu    sync.Mutex
	sent  map[string]*remindRec // 活动 ID → 账（内存镜像，权威；磁盘是它的投影）
	armed map[string]bool       // 已经排上期、还没触发的活动 ID
}

func New(doc store.Doc, cfg Config) kernel.Feature {
	c := cfg.withDefaults()
	return &feature{
		doc:   doc,
		cfg:   c,
		sent:  make(map[string]*remindRec),
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
	f.api = api
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
// 值解不开时按"已提醒"兜底：重发只会刷屏，而漏发的那一次已经漏了。这是"功能
// 自决存储"要交的税——Doc 的 JSON 值没有编译期 schema 约束，坏记录或换格式时
// 只能这样容错。不认识任何旧格式（开发期不做迁移）。
func (f *feature) loadSent() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.doc.Scan(NS, "", func(key string, raw []byte) error {
		var rec remindRec
		if err := json.Unmarshal(raw, &rec); err != nil {
			f.cfg.Logf("calexpiry: 已提醒记录 %s 解不开，按已提醒处理: %v", key, err)
			rec.SettledAt = time.Now()
		}
		f.sent[key] = &rec
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
		if f.done(api, a.ID) {
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

// remind 是排期触发时跑的：把还没收到提醒的目标投进队列，回执到了才记账。
//
// 旧实现是"Submit 全部成功就算提醒过"——崩溃窗口（落账后、发送前进程退出）与
// 队列静默丢弃（超期/重试耗尽/积压溢出）两条路都通向永久漏提醒。现在账只在
// 回执（OnDelivered）里落：没送到的不记，下一轮扫描用同一任务 ID 重排补投——
// 宁可极小概率重复，也不要静默漏掉一个到期提醒。
func (f *feature) remind(ctx context.Context, api kernel.API, a calendar.Activity) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := f.cfg.Now()
	msg := f.message(a, now)

	targets := f.targets(api)
	if len(targets) == 0 {
		f.cfg.Logf("calexpiry: 未配置提醒目标，只记日志 → %s", text.OneLine(msg))
		f.settle(a.ID)
		return nil
	}

	submitted := 0
	for _, target := range targets {
		if f.servedTo(target, a.ID) {
			continue // 该目标已有回执（或整条已全局了结），不重发
		}
		// Topic 相同 + Mergeable：同一轮里多个活动一起到期时，队列把它们合成一条发出去
		tgt := target
		item := queue.Item{
			ID:        "expiry:" + a.ID + "@" + tgt,
			Target:    tgt,
			Text:      msg, // 与日志同一条文本：f.message 是纯函数，不必重排一遍
			Topic:     "expiry",
			Mergeable: true,
			// 回执卡随包裹走：队列报发送成功才落账，失败与丢弃永远不记
			OnDelivered: func() { f.markTo(tgt, a.ID) },
		}
		if err := api.Submit(item); err != nil {
			return fmt.Errorf("calexpiry: 投递 %s 到 %s 失败: %w", a.ID, tgt, err)
		}
		submitted++
	}
	if submitted > 0 {
		f.cfg.Logf("calexpiry: 已投递 %s（%s）的提醒给 %d 个目标，回执到了才算数", a.ID, a.Title, submitted)
	}
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
	for id, rec := range f.sent {
		if act, ok := cal.Get(id); ok {
			if now.After(act.End) {
				toDelete = append(toDelete, id)
			}
		} else {
			if now.Sub(rec.newest()) > f.cfg.Lead+24*time.Hour {
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

// done 报告这个活动的提醒是否已了结：全局了结过，或当前全部订阅目标都已收到。
// 后一半就是补投引擎：某个目标的回执缺席（投递失败被队列丢弃）时这里返回 false，
// 下一轮扫描会用同一任务 ID 重排（At 已过立即触发），remind 只补缺回执的目标。
func (f *feature) done(api kernel.API, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.sent[id]
	if !ok {
		return false
	}
	if !rec.SettledAt.IsZero() {
		return true
	}
	targets := f.targets(api)
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if _, ok := rec.Targets[t]; !ok {
			return false
		}
	}
	return true
}

// servedTo 报告该活动是否已提醒过指定目标（有回执，或整条已全局了结）。
func (f *feature) servedTo(tgt, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.sent[id]
	if !ok {
		return false
	}
	if !rec.SettledAt.IsZero() {
		return true
	}
	_, ok = rec.Targets[tgt]
	return ok
}

func (f *feature) arm(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed[id] = true
}

// markTo 记"该目标已收到提醒"；当前全部订阅目标都收到时收敛写全局了结。
// 幂等（覆盖时间戳）；磁盘在锁内以内存镜像为准整条写回，严格同序。
func (f *feature) markTo(tgt, id string) {
	now := f.cfg.Now()
	f.mu.Lock()
	rec := f.sent[id]
	if rec == nil {
		rec = &remindRec{Targets: map[string]time.Time{}}
		f.sent[id] = rec
	}
	if rec.Targets == nil {
		rec.Targets = map[string]time.Time{}
	}
	rec.Targets[tgt] = now
	if rec.SettledAt.IsZero() && f.api != nil {
		if targets := f.targets(f.api); len(targets) > 0 {
			allServed := true
			for _, t := range targets {
				if _, ok := rec.Targets[t]; !ok {
					allServed = false
					break
				}
			}
			if allServed {
				rec.SettledAt = now
				delete(f.armed, id)
			}
		}
	}
	err := f.doc.Put(NS, id, rec)
	f.mu.Unlock()
	if err != nil {
		f.cfg.Logf("calexpiry: 写已提醒记录 %s 失败（重启后可能重发一次）: %v", id, err)
	}
}

// settle 把活动记为全局了结。无目标只记日志的轮次也走这里，否则每轮扫描都刷日志。
func (f *feature) settle(id string) {
	now := f.cfg.Now()
	f.mu.Lock()
	rec := f.sent[id]
	if rec == nil {
		rec = &remindRec{}
		f.sent[id] = rec
	}
	rec.SettledAt = now
	delete(f.armed, id)
	err := f.doc.Put(NS, id, rec)
	f.mu.Unlock()
	if err != nil {
		f.cfg.Logf("calexpiry: 写已提醒记录 %s 失败（重启后可能重发一次）: %v", id, err)
	}
}

func taskID(activityID string) string { return "expiry:" + activityID }
