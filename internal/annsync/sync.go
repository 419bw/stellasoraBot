package annsync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"xingta/internal/kernel"
	"xingta/internal/kernel/calendar"
	"xingta/internal/store"
)

// 命名空间与键的约定（多源共用一个 Doc，所以键一律带源名前缀）：
//
//	sync     / "<src>:meta"        → state（进度）
//	news     / "<src>:<refID>"     → Item（条目快照，含 Suspect 与原文片段）
//	activity / "<src>:<refID>:<n>" → Rec（投影用活动记录，键即 Rec.ID）
const (
	nsSync     = "sync"
	nsNews     = "news"
	nsActivity = "activity"
)

// Throttled 表示源侧临时不可用（限流 / 5xx / 网络）：引擎中断本轮抓取、
// 把已抓到的先投影出去，然后退避。未分类的错误按 Throttled 处理——保守比激进便宜。
type Throttled struct{ Err error }

func (e Throttled) Error() string { return "临时不可用: " + e.Err.Error() }
func (e Throttled) Unwrap() error { return e.Err }

// Permanent 表示这一条本轮不该重试（404、业务码错误）：引擎跳过它继续抓下一条。
type Permanent struct{ Err error }

func (e Permanent) Error() string { return "不可重试: " + e.Err.Error() }
func (e Permanent) Unwrap() error { return e.Err }

// state 是引擎自己的进度，写在 "sync" ns。重启后靠它续抓，不要求一轮抓完。
type state struct {
	Source      string            `json:"source"`
	Known       map[string]string `json:"known"`   // refID → hex(Hash)
	Pending     map[string]bool   `json:"pending"` // 名下有待确认子活动，每轮重试解析
	Fails       int               `json:"fails"`
	LastSuccess time.Time         `json:"lastSuccess"`
	LastFull    time.Time         `json:"lastFull"`
	LastError   string            `json:"lastError"`
}

type syncer struct {
	doc store.Doc
	cal calendar.Writer
	src Source
	mg  Merger
	ov  OverrideStore
	cfg Config

	refresh chan struct{}

	// 只有 loop 那一个 goroutine 读写，无需加锁
	lastIDs []string
}

// Name 带源名：main 里可以注册多个源的引擎，日志与启动错误要能分清。
func (s *syncer) Name() string { return "annsync/" + s.src.Name() }

// Start 非阻塞（kernel.Feature 契约）：循环自己起 goroutine，首轮立刻同步。
func (s *syncer) Start(ctx context.Context, _ kernel.API) error {
	if s.doc == nil || s.cal == nil || s.src == nil {
		return errors.New("annsync: doc / cal / src 都不能为空")
	}
	go s.loop(ctx)
	return nil
}

// Refresh 请求立刻重投影（只读存储，不碰网络）。非阻塞：已有一次待处理就合并。
func (s *syncer) Refresh() {
	select {
	case s.refresh <- struct{}{}:
	default:
	}
}

// loop 是引擎唯一的执行体：抓取、投影、退避全在这一个 goroutine 里串行发生，
// 所以日历写侧天然只有一个写者。
func (s *syncer) loop(ctx context.Context) {
	wait := time.Duration(0) // 首轮立即
	for {
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-s.refresh:
			if err := s.reproject(); err != nil {
				s.cfg.Logf("annsync: 重投影失败: %v", err)
			}
			wait = s.cfg.Interval
			continue
		default:
		}

		err := s.round(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			st, lerr := s.loadState()
			if lerr != nil {
				s.cfg.Logf("annsync: %v（且读不回状态: %v）", err, lerr)
				wait = s.cfg.Interval
				continue
			}
			wait = backoff(st.Fails, s.cfg.BackoffBase, s.cfg.Interval)
			s.cfg.Logf("annsync: 本轮失败，%s 后重试: %v", wait, err)
			continue
		}
		wait = s.cfg.Interval
	}
}

// round 是一轮完整同步：列目录 → 挑出要抓的 → 串行抓 → 全量投影。
func (s *syncer) round(ctx context.Context) error {
	src := s.src.Name()
	now := s.cfg.Now()

	st, err := s.loadState()
	if err != nil {
		return fmt.Errorf("读取同步状态: %w", err)
	}
	// 全量校准的判据不能带 "LastFull 为零值"：冷启动与被限流打断时 LastFull 一直是零，
	// 那样每轮都会重抓所有条目——限流时正好火上浇油。冷启动本来就因为所有 ref 都未知而抓全。
	full := !st.LastFull.IsZero() && now.Sub(st.LastFull) >= s.cfg.FullEvery

	refs, err := s.src.List(ctx)
	if err != nil {
		s.fail(&st, err)
		return fmt.Errorf("列目录: %w", err)
	}

	todo := pick(refs, st, full)
	var aborted error
	var prev time.Time
	for i, r := range todo {
		if i > 0 {
			if gap := s.cfg.MinGap - time.Since(prev); gap > 0 {
				if err := sleepCtx(ctx, gap); err != nil {
					return err
				}
			}
		}
		item, ferr := s.src.Fetch(ctx, r)
		prev = time.Now()
		if ferr != nil {
			var pm Permanent
			if errors.As(ferr, &pm) {
				s.cfg.Logf("annsync: 跳过 %s/%s: %v", src, r.ID, ferr)
				continue
			}
			// 限流与未知错误：中断抓取，但把已抓到的投影出去再退避
			s.fail(&st, ferr)
			aborted = fmt.Errorf("抓取 %s/%s: %w", src, r.ID, ferr)
			break
		}
		if err := s.saveItem(item); err != nil {
			return fmt.Errorf("写入 %s/%s: %w", src, r.ID, err)
		}
		st.Known[r.ID] = hex.EncodeToString(r.Hash[:])
		if wantsRetry(item, now) {
			st.Pending[r.ID] = true
		} else {
			delete(st.Pending, r.ID)
		}
		if err := s.saveState(st); err != nil {
			return fmt.Errorf("写入进度: %w", err)
		}
	}

	if err := s.reproject(); err != nil {
		return err
	}

	// LastSuccess 记的是"最近一次成功把数据投进日历"：抓取被限流打断也算，
	// 因为已抓到的那部分确实可见了；链路健康与否看 Fails 与 LastError。
	st.LastSuccess = s.cfg.Now()
	if aborted == nil {
		st.Fails = 0
		st.LastError = ""
		if full || st.LastFull.IsZero() {
			st.LastFull = st.LastSuccess // 被打断的全量校准不算完成，下一轮接着做
		}
	}
	if err := s.saveState(st); err != nil {
		return fmt.Errorf("写入进度: %w", err)
	}
	return aborted
}

// reproject 只读存储重算日历：条目快照 → 合并 → Rec → 应用 override → 灌日历。
func (s *syncer) reproject() error {
	items, err := s.loadItems()
	if err != nil {
		return fmt.Errorf("读取条目快照: %w", err)
	}
	if s.mg != nil {
		items = s.mg.Merge(items)
	}
	recs := s.buildRecs(items)
	if err := s.saveRecs(recs); err != nil {
		return fmt.Errorf("写入活动记录: %w", err)
	}

	ovs, err := s.ov.All()
	if err != nil {
		s.cfg.Logf("annsync: 读不到人工覆盖，本轮按无覆盖投影: %v", err)
		ovs = nil
	}
	acts, dups := project(recs, ovs)
	now := s.cfg.Now()
	for _, d := range dups {
		// 同一时间窗口被多篇公告声明：日历只留一行，但要喊出来——
		// 已经结束的旧公告只会淹没还能伤到人的那几条，不报。
		if !d.End.After(now) {
			continue
		}
		s.cfg.Logf("annsync: 警告 多篇公告声明了同一个时间窗口（%q %s ~ %s），日历只留一行: %s",
			d.Title, d.Start.Format(time.RFC3339), d.End.Format(time.RFC3339), d.ID)
	}

	s.cal.BulkUpsert(acts)
	newIDs := make([]string, 0, len(acts))
	for _, a := range acts {
		newIDs = append(newIDs, a.ID)
	}
	if gone := missingIDs(s.lastIDs, newIDs); len(gone) > 0 {
		s.cal.RemoveSet(gone)
	}
	s.lastIDs = newIDs
	return nil
}

// buildRecs 是投影侧的入口：映射交给 RecsOf，这里只补"这一轮是什么时候算的"。
func (s *syncer) buildRecs(items []Item) []Rec {
	recs := RecsOf(s.src.Name(), items)
	now := s.cfg.Now()
	for i := range recs {
		recs[i].ParsedAt = now
	}
	return recs
}

// RecsOf 把条目快照摊平成一行一个子活动：投影用的就是这条映射，展示侧读到的
// Rec 形状也由它定义，两边不是各写一遍。标题缺失用条目标题兜底；
// 海报不兜底——维护公告的列表封面是通用运营图，填给切分条目等于给每个
// 活动配同一张假海报，比没有更糟（方案 §3.4），没有就让展示层画占位。
// ParsedAt 留零值，由调用方决定要不要打戳。
func RecsOf(src string, items []Item) []Rec {
	var out []Rec
	for _, it := range items {
		for i, ev := range it.Events {
			title := ev.Title
			if title == "" {
				title = it.Title
			}
			out = append(out, Rec{
				ID:         recID(src, it.Ref.ID, i),
				SourceID:   src,
				RefID:      it.Ref.ID,
				Title:      title,
				Label:      ev.Label,
				Start:      ev.Start,
				End:        ev.End,
				Status:     ev.Status,
				ClaimStart: ev.ClaimStart,
				ClaimEnd:   ev.ClaimEnd,
				Fragment:   ev.Fragment,
				Poster:     ev.Poster,
				URL:        ev.URL,
				Provenance: ev.Provenance,
				TwinRef:    ev.TwinRef,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// saveRecs 一把事务里先清本源旧记录再写新记录：投影是全量重算，不留孤儿。
func (s *syncer) saveRecs(recs []Rec) error {
	prefix := s.src.Name() + ":"
	return s.doc.Batch(func(b store.Batch) error {
		var old []string
		if err := b.Scan(nsActivity, prefix, func(key string, _ []byte) error {
			old = append(old, key)
			return nil
		}); err != nil {
			return err
		}
		for _, k := range old {
			if err := b.Delete(nsActivity, k); err != nil {
				return err
			}
		}
		for _, r := range recs {
			if err := b.Put(nsActivity, r.ID, r); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *syncer) saveItem(it Item) error {
	return s.doc.Put(nsNews, s.src.Name()+":"+it.Ref.ID, it)
}

func (s *syncer) loadItems() ([]Item, error) {
	var out []Item
	err := s.doc.Scan(nsNews, s.src.Name()+":", func(_ string, raw []byte) error {
		var it Item
		if err := json.Unmarshal(raw, &it); err != nil {
			return err
		}
		out = append(out, it)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.ID < out[j].Ref.ID })
	return out, nil
}

func (s *syncer) stateKey() string { return s.src.Name() + ":meta" }

func (s *syncer) loadState() (state, error) {
	st := state{
		Source:  s.src.Name(),
		Known:   map[string]string{},
		Pending: map[string]bool{},
	}
	ok, err := s.doc.Get(nsSync, s.stateKey(), &st)
	if err != nil {
		return st, err
	}
	if !ok {
		return st, nil
	}
	if st.Known == nil {
		st.Known = map[string]string{}
	}
	if st.Pending == nil {
		st.Pending = map[string]bool{}
	}
	return st, nil
}

func (s *syncer) saveState(st state) error {
	st.Source = s.src.Name()
	return s.doc.Put(nsSync, s.stateKey(), st)
}

// fail 记一次失败并尽力落盘；落盘失败只记日志，退避仍然按内存里的次数走。
func (s *syncer) fail(st *state, err error) {
	st.Fails++
	st.LastError = err.Error()
	if serr := s.saveState(*st); serr != nil {
		s.cfg.Logf("annsync: 进度落盘失败: %v", serr)
	}
}

// pick 挑出本轮要抓的引用：没见过、指纹变了、名下有待确认子活动，或全量校准。
func pick(refs []Ref, st state, full bool) []Ref {
	var out []Ref
	for _, r := range refs {
		h, known := st.Known[r.ID]
		switch {
		case !known, full:
			out = append(out, r)
		case h != hex.EncodeToString(r.Hash[:]):
			out = append(out, r)
		case st.Pending[r.ID]:
			out = append(out, r)
		}
	}
	return out
}

// RetryAge 之后的公告不再重抓：真官网实测 28 条半年前解析不出来的旧公告，
// 会因「有待确认就重抓」每轮重抓，待确认桶永远清不完。公告自己不会变好，到期放手。
const RetryAge = 30 * 24 * time.Hour

// wantsRetry 报告这条条目值不值得下一轮再解析一次：发布未满 RetryAge（发布时间
// 未知按刚发算），且整条可疑，或还有待确认/未知状态的子活动。
func wantsRetry(it Item, now time.Time) bool {
	if !it.Published.IsZero() && now.Sub(it.Published) >= RetryAge {
		return false
	}
	if it.Suspect {
		return true
	}
	for _, ev := range it.Events {
		if ev.Status == StatusPending || ev.Status == "" {
			return true
		}
	}
	return false
}

// project 把活动记录投成日历条目：pending 不进日历，override 逐条应用。
// 返回的 dups 是被三元组去重挡下的记录——正常路径为空，非空即源侧合并漏了。
func project(recs []Rec, ov map[string]Override) (acts []calendar.Activity, dups []Rec) {
	type triple struct{ title, start, end string }
	seen := map[triple]bool{}

	for _, r := range recs {
		if !r.InCalendar() {
			continue
		}
		o, has := ov[r.ID]
		if has && o.Hide {
			continue
		}
		title, start, end := r.Title, r.Start, r.End
		if has {
			if o.Title != "" {
				title = o.Title
			}
			if !o.Start.IsZero() {
				start = o.Start
			}
			if !o.End.IsZero() {
				end = o.End
			}
		}
		if !end.After(start) {
			continue // 覆盖写坏了：宁可不显示，也不显示一个不可能的区间
		}
		k := triple{title, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)}
		if seen[k] {
			dups = append(dups, r)
			continue
		}
		seen[k] = true
		acts = append(acts, calendar.Activity{
			ID: r.ID, Game: r.SourceID, Title: title, Start: start, End: end, URL: r.URL,
		})
	}
	return acts, dups
}

// backoff 是 base×2ⁿ，上限 max(2h, Interval)：轮询间隔本身比 2h 长时以间隔为准。
func backoff(fails int, base, interval time.Duration) time.Duration {
	limit := 2 * time.Hour
	if interval > limit {
		limit = interval
	}
	if fails < 1 {
		fails = 1
	}
	shift := fails - 1
	if shift >= 20 {
		return limit // 再翻倍也超不过上限，别移位到溢出
	}
	if d := base << shift; d < limit {
		return d
	}
	return limit
}

func missingIDs(old, cur []string) []string {
	have := make(map[string]bool, len(cur))
	for _, id := range cur {
		have[id] = true
	}
	var out []string
	for _, id := range old {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}

func recID(src, refID string, seq int) string {
	return fmt.Sprintf("%s:%s:%d", src, refID, seq)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
