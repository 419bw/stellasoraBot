package annsync_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/kernel/calendar"
	"xingta/internal/store/storetest"
)

// ---- 假件 ----------------------------------------------------------------

type fakeSource struct {
	name string

	mu        sync.Mutex
	refs      []annsync.Ref
	items     map[string]annsync.Item
	errs      map[string]error
	fetches   []string
	listErr   error
	listCalls int
	listTimes []time.Time
}

func newSource(name string, refs ...annsync.Ref) *fakeSource {
	return &fakeSource{
		name:  name,
		refs:  refs,
		items: map[string]annsync.Item{},
		errs:  map[string]error{},
	}
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) List(ctx context.Context) ([]annsync.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.listTimes = append(f.listTimes, time.Now())
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]annsync.Ref(nil), f.refs...), nil
}

// listGaps 返回相邻两次 List 之间的间隔：退避有没有真的生效，看这个序列在不在长。
func (f *fakeSource) listGaps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, 0, len(f.listTimes))
	for i := 1; i < len(f.listTimes); i++ {
		out = append(out, f.listTimes[i].Sub(f.listTimes[i-1]))
	}
	return out
}

func (f *fakeSource) Fetch(ctx context.Context, r annsync.Ref) (annsync.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, r.ID)
	if err := f.errs[r.ID]; err != nil {
		return annsync.Item{}, err
	}
	it := f.items[r.ID]
	it.Ref = r
	return it, nil
}

func (f *fakeSource) setItem(id, title string, evs ...annsync.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = annsync.Item{Title: title, Events: evs}
}

func (f *fakeSource) setItemWithPub(id, title string, pub time.Time, evs ...annsync.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = annsync.Item{Title: title, Published: pub, Events: evs}
}

func (f *fakeSource) setErr(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, id)
		return
	}
	f.errs[id] = err
}

func (f *fakeSource) addRef(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref(id))
}

func (f *fakeSource) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, got := range f.fetches {
		if got == id {
			n++
		}
	}
	return n
}

func (f *fakeSource) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fetches)
}

// mergingSource 让假件同时实现 annsync.Merger，用来断言引擎真的调了它。
type mergingSource struct {
	*fakeSource
	drop func([]annsync.Item) []annsync.Item
}

func (m mergingSource) Merge(all []annsync.Item) []annsync.Item { return m.drop(all) }

// recWriter 包住真日历，另外记下写侧调用，用来断言 RemoveSet 的参数。
type recWriter struct {
	inner *calendar.Store

	mu      sync.Mutex
	removed []string
	bulks   int
}

func newWriter() *recWriter { return &recWriter{inner: calendar.NewStore()} }

func (w *recWriter) Upsert(a calendar.Activity) { w.inner.Upsert(a) }

func (w *recWriter) BulkUpsert(list []calendar.Activity) {
	w.mu.Lock()
	w.bulks++
	w.mu.Unlock()
	w.inner.BulkUpsert(list)
}

// bulksDone 是 waitFor 用的观察口：已完成的全量投影次数。
func (w *recWriter) bulksDone() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bulks
}

func (w *recWriter) Remove(id string) bool { return w.inner.Remove(id) }

func (w *recWriter) RemoveSet(ids []string) int {
	w.mu.Lock()
	w.removed = append(w.removed, ids...)
	w.mu.Unlock()
	return w.inner.RemoveSet(ids)
}

func (w *recWriter) removedIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.removed...)
}

// ---- 公共辅助 ------------------------------------------------------------

func ref(id string) annsync.Ref { return annsync.Ref{ID: id, Hash: sha256.Sum256([]byte(id))} }

func ev(title string, start, end time.Time, status string) annsync.Event {
	return annsync.Event{Title: title, Start: start, End: end, Status: status}
}

// testCfg 的间隔都是毫秒级：引擎的等待全部走 Interval/MinGap/BackoffBase，
// 测试因此能在几十毫秒内跑完，而不是等真实的 30 分钟与 1 分钟退避。
func testCfg(now func() time.Time) annsync.Config {
	return annsync.Config{
		Interval:    5 * time.Millisecond,
		FullEvery:   time.Hour,
		MinGap:      time.Millisecond,
		BackoffBase: 5 * time.Millisecond,
		Now:         now,
	}
}

// start 起引擎并返回一个已取消即停的上下文。
func start(t *testing.T, s annsync.Syncer) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx, nil); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cancel)
	return ctx
}

// waitFor 轮询断言：引擎是异步循环，任何"某事发生了"的断言都得给它时间。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("超时：%s", what)
}

func waitNot(t *testing.T, what string, cond func() bool) {
	t.Helper()
	time.Sleep(60 * time.Millisecond) // 十几轮的机会
	if cond() {
		t.Fatalf("不该发生：%s", what)
	}
}

func idsOf(list []calendar.Activity) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

func has(id string, list []calendar.Activity) bool {
	for _, a := range list {
		if a.ID == id {
			return true
		}
	}
	return false
}

// ---- 用例 ----------------------------------------------------------------

var base = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func TestFirstRoundRunsImmediatelyAndFillsCalendar(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"), ref("2"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(48*time.Hour), annsync.StatusOK))
	src.setItem("2", "販售B", ev("販售B", base.Add(time.Hour), base.Add(72*time.Hour), annsync.StatusOK))

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "日历里出现两条活动", func() bool { return len(w.inner.Active(base.Add(2*time.Hour))) == 2 })
	// 状况是轮末才落盘的，日历灌完时 LastSuccess 可能还在路上
	waitFor(t, "首轮状况落盘", func() bool {
		st, err := annsync.ReadStatus(doc, "fake")
		return err == nil && !st.LastSuccess.IsZero()
	})

	st, err := annsync.ReadStatus(doc, "fake")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.Known != 2 {
		t.Errorf("Known = %d, want 2", st.Known)
	}
	if st.Fails != 0 {
		t.Errorf("Fails = %d, want 0", st.Fails)
	}
}

func TestIncrementalOnlyFetchesNewRefs(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(48*time.Hour), annsync.StatusOK))

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)
	waitFor(t, "首轮抓完", func() bool { return src.count("1") == 1 })

	src.setItem("2", "販售B", ev("販售B", base, base.Add(time.Hour), annsync.StatusOK))
	src.addRef("2")

	waitFor(t, "新条目被抓到", func() bool { return src.count("2") >= 1 })
	waitFor(t, "旧条目没有被反复重抓", func() bool { return src.total() >= 2 })

	if n := src.count("1"); n != 1 {
		t.Errorf("指纹未变的旧条目被抓了 %d 次, want 1（增量失效）", n)
	}
	// 抓取计数涨了只说明这一条抓完，投影在同一轮末尾才做——等记录出现，别直接读。
	waitFor(t, "新条目进日历", func() bool {
		_, ok := w.inner.Get("fake:2:0")
		return ok
	})
}

func TestThrottledAbortsRoundThenResumesWithoutRefetching(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"), ref("2"), ref("3"))
	for i := 1; i <= 3; i++ {
		id := fmt.Sprint(i)
		src.setItem(id, "活动"+id, ev("活动"+id, base, base.Add(time.Duration(i)*time.Hour), annsync.StatusOK))
	}
	src.setErr("3", annsync.Throttled{Err: fmt.Errorf("HTTP 429")})

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "限流被记成一次失败", func() bool {
		st, err := annsync.ReadStatus(doc, "fake")
		return err == nil && st.Fails >= 1 && st.LastError != ""
	})
	waitFor(t, "已抓到的两条照常投影", func() bool {
		_, ok1 := w.inner.Get("fake:1:0")
		_, ok2 := w.inner.Get("fake:2:0")
		return ok1 && ok2
	})
	waitNot(t, "限流的那条不该进日历", func() bool { _, ok := w.inner.Get("fake:3:0"); return ok })

	src.setErr("3", nil) // 退避结束后源恢复

	waitFor(t, "断点续抓补上第 3 条", func() bool { _, ok := w.inner.Get("fake:3:0"); return ok })
	if n := src.count("1"); n != 1 {
		t.Errorf("续抓时重复请求了已成功的条目 1：抓了 %d 次, want 1", n)
	}
	if n := src.count("2"); n != 1 {
		t.Errorf("续抓时重复请求了已成功的条目 2：抓了 %d 次, want 1", n)
	}
	if n := src.count("3"); n < 2 {
		t.Errorf("恢复后第 3 条只被抓了 %d 次, want ≥2", n)
	}
	waitFor(t, "成功后失败计数归零", func() bool {
		st, err := annsync.ReadStatus(doc, "fake")
		return err == nil && st.Fails == 0 && st.LastError == ""
	})
}

func TestPermanentErrorSkipsItemWithoutFailingRound(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"), ref("2"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))
	src.setErr("2", annsync.Permanent{Err: fmt.Errorf("HTTP 404")})

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "本轮仍然算成功", func() bool {
		st, err := annsync.ReadStatus(doc, "fake")
		return err == nil && !st.LastSuccess.IsZero() && st.Fails == 0
	})
	if _, ok := w.inner.Get("fake:1:0"); !ok {
		t.Error("永久错误的一条把整轮拖垮了：好条目没进日历")
	}
	waitNot(t, "404 的条目不该进日历", func() bool { _, ok := w.inner.Get("fake:2:0"); return ok })
}

func TestPendingItemIsRefetchedNextRound(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"), ref("2"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))
	src.setItem("2", "说不清的", ev("说不清的", time.Time{}, time.Time{}, annsync.StatusPending))

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "待确认条目被重试解析", func() bool { return src.count("2") >= 2 })
	if n := src.count("1"); n != 1 {
		t.Errorf("状态干净的条目被反复重抓：%d 次, want 1", n)
	}
	if _, ok := w.inner.Get("fake:2:0"); ok {
		t.Error("pending 记录不该进日历")
	}
	pend, err := annsync.ReadPending(doc, "fake")
	if err != nil {
		t.Fatalf("ReadPending: %v", err)
	}
	if len(pend) != 1 || pend[0].ID != "fake:2:0" {
		t.Errorf("待确认桶 = %+v, want 一条 fake:2:0", pend)
	}
	st, _ := annsync.ReadStatus(doc, "fake")
	if st.PendingRefs != 1 {
		t.Errorf("PendingRefs = %d, want 1", st.PendingRefs)
	}
}

func TestSuspectItemWithoutEventsIsStillVisible(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.mu.Lock()
	src.items["1"] = annsync.Item{Title: "全是图片的公告", Suspect: true}
	src.mu.Unlock()

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "整条可疑的条目每轮都被重试解析", func() bool { return src.count("1") >= 2 })
	waitFor(t, "可疑条目的快照被记下来", func() bool {
		items, err := annsync.ReadItems(doc, "fake")
		return err == nil && len(items) == 1 && items[0].Suspect
	})
	if _, ok := w.inner.Get("fake:1:0"); ok {
		t.Error("一个子活动都没解析出来的条目不该在日历里")
	}
}

func TestOverrideSurvivesRefreshAndIsApplied(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)
	waitFor(t, "首轮完成", func() bool { _, ok := w.inner.Get("fake:1:0"); return ok })

	ov := annsync.NewOverrideStore(doc)
	newEnd := base.Add(9 * time.Hour)
	if err := ov.Put("fake:1:0", annsync.Override{End: newEnd, Title: "招募A（改期）", By: "tester", At: base}); err != nil {
		t.Fatalf("Put override: %v", err)
	}
	s.Refresh()

	waitFor(t, "覆盖立即生效", func() bool {
		a, ok := w.inner.Get("fake:1:0")
		return ok && a.End.Equal(newEnd) && a.Title == "招募A（改期）"
	})

	// 覆盖不许被刷新冲掉：再来一条新公告，逼引擎跑完整一轮重投影
	src.setItem("2", "販售B", ev("販售B", base, base.Add(time.Hour), annsync.StatusOK))
	src.addRef("2")
	waitFor(t, "新一轮同步完成", func() bool { _, ok := w.inner.Get("fake:2:0"); return ok })

	a, _ := w.inner.Get("fake:1:0")
	if !a.End.Equal(newEnd) || a.Title != "招募A（改期）" {
		t.Errorf("刷新把人工覆盖冲掉了：%+v", a)
	}
	recs, err := annsync.ReadRecs(doc, "fake")
	if err != nil {
		t.Fatalf("ReadRecs: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("记录数 = %d, want 2", len(recs))
	}
	for _, r := range recs {
		if r.ID == "fake:1:0" && !r.End.Equal(base.Add(time.Hour)) {
			t.Errorf("存储里的解析原值被覆盖改写了（覆盖只应作用于投影）：%v", r.End)
		}
	}
}

func TestHideOverrideRemovesFromCalendarViaRemoveSet(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"), ref("2"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))
	src.setItem("2", "販售B", ev("販售B", base, base.Add(time.Hour), annsync.StatusOK))

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)
	waitFor(t, "两条都在日历里", func() bool {
		_, ok1 := w.inner.Get("fake:1:0")
		_, ok2 := w.inner.Get("fake:2:0")
		return ok1 && ok2
	})

	if err := annsync.NewOverrideStore(doc).Put("fake:2:0", annsync.Override{Hide: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Refresh()

	waitFor(t, "隐藏的条目从日历消失", func() bool { _, ok := w.inner.Get("fake:2:0"); return !ok })
	waitFor(t, "RemoveSet 收到消失的 ID", func() bool {
		for _, id := range w.removedIDs() {
			if id == "fake:2:0" {
				return true
			}
		}
		return false
	})
	if _, ok := w.inner.Get("fake:1:0"); !ok {
		t.Error("隐藏一条却把另一条也带走了")
	}
}

func TestFuzzyStartStillEntersCalendar(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusFuzzyStart))

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "fuzzy_start 进日历（快结束提醒只依赖 End）", func() bool {
		return len(w.inner.EndingWithin(base, 2*time.Hour)) == 1
	})
	if n := src.count("1"); n != 1 {
		t.Errorf("fuzzy_start 不该被当成待确认反复重抓，抓了 %d 次, want 1", n)
	}
}

func TestMergerIsAppliedBeforeProjection(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"), ref("2"))
	src.setItem("1", "版本公告",
		ev("招募A", base, base.Add(time.Hour), annsync.StatusOK),
		ev("招募B", base, base.Add(2*time.Hour), annsync.StatusOK))
	src.setItem("2", "招募A 独立公告", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))

	// 假合并：版本公告里与独立公告重名的子活动被丢掉（真源的 twin 合并就是这个方向）
	ms := mergingSource{fakeSource: src, drop: func(all []annsync.Item) []annsync.Item {
		out := append([]annsync.Item(nil), all...)
		for i := range out {
			if out[i].Ref.ID != "1" {
				continue
			}
			kept := out[i].Events[:0]
			for _, e := range out[i].Events {
				if e.Title != "招募A" {
					kept = append(kept, e)
				}
			}
			out[i].Events = kept
		}
		return out
	}}

	s := annsync.NewFeature(doc, w, ms, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "合并后只剩两条活动", func() bool {
		recs, err := annsync.ReadRecs(doc, "fake")
		return err == nil && len(recs) == 2
	})
	if _, ok := w.inner.Get("fake:1:0"); !ok {
		t.Error("版本公告剩下的子活动没进日历")
	}
}

// 标题缺失回退条目标题；海报不回退——通用运营图当海报比没有更糟（方案 §3.4），
// 缺就让展示层画占位。
func TestTitleFallsBackToItemButPosterDoesNot(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.mu.Lock()
	src.items["1"] = annsync.Item{
		Title: "公告标题", Thumbnail: "https://example.test/poster.png",
		Events: []annsync.Event{{Start: base, End: base.Add(time.Hour), Status: annsync.StatusOK}},
	}
	src.mu.Unlock()

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "记录落盘", func() bool {
		recs, err := annsync.ReadRecs(doc, "fake")
		return err == nil && len(recs) == 1
	})
	recs, _ := annsync.ReadRecs(doc, "fake")
	if recs[0].Title != "公告标题" {
		t.Errorf("Title = %q, want 公告标题（子活动没名字时该用条目标题）", recs[0].Title)
	}
	if recs[0].Poster != "" {
		t.Errorf("Poster = %q, want 空（不拿条目封面当海报）", recs[0].Poster)
	}
	a, _ := w.inner.Get("fake:1:0")
	if a.Title != "公告标题" || a.Game != "fake" {
		t.Errorf("日历条目 = %+v", a)
	}
}

func TestFullRecalibrationRefetchesEverything(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"), ref("2"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))
	src.setItem("2", "販售B", ev("販售B", base, base.Add(time.Hour), annsync.StatusOK))

	cfg := testCfg(nil)
	cfg.FullEvery = 20 * time.Millisecond // 比 Interval 长一点，第二轮才轮到全量
	s := annsync.NewFeature(doc, w, src, nil, cfg)
	start(t, s)

	waitFor(t, "全量校准把两条都重抓了一遍", func() bool {
		return src.count("1") >= 2 && src.count("2") >= 2
	})
	waitFor(t, "LastFull 落盘", func() bool {
		st, err := annsync.ReadStatus(doc, "fake")
		return err == nil && !st.LastFull.IsZero()
	})
}

func TestListErrorBacksOffWithoutTouchingCalendar(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))
	src.mu.Lock()
	src.listErr = annsync.Throttled{Err: fmt.Errorf("HTTP 429")}
	src.mu.Unlock()

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	start(t, s)

	waitFor(t, "列目录失败被记进状态", func() bool {
		st, err := annsync.ReadStatus(doc, "fake")
		return err == nil && st.Fails >= 1 && st.LastError != ""
	})
	waitNot(t, "列目录失败不该写日历", func() bool { return w.inner.Len() != 0 })

	src.mu.Lock()
	src.listErr = nil
	src.mu.Unlock()
	waitFor(t, "恢复后照常同步", func() bool { _, ok := w.inner.Get("fake:1:0"); return ok })
}

// TestBackoffActuallySlowsTheLoop 盯的是循环本身：退避算得对不等于等得对。
func TestBackoffActuallySlowsTheLoop(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.mu.Lock()
	src.listErr = annsync.Throttled{Err: fmt.Errorf("HTTP 429")}
	src.mu.Unlock()

	cfg := testCfg(nil)
	cfg.BackoffBase = 20 * time.Millisecond // 20,40,80,160ms：几毫秒的抖动淹不掉翻倍
	s := annsync.NewFeature(doc, w, src, nil, cfg)
	start(t, s)

	waitFor(t, "至少失败 5 轮", func() bool {
		st, err := annsync.ReadStatus(doc, "fake")
		return err == nil && st.Fails >= 5
	})

	gaps := src.listGaps()
	if len(gaps) < 4 { // 5 次失败 = 5 次 List = 4 个间隔（第 5 段退避还没走完）
		t.Fatalf("只观察到 %d 个间隔: %v", len(gaps), gaps)
	}
	first, last := gaps[0], gaps[len(gaps)-1]
	if last < first*2 {
		t.Errorf("退避没有随失败次数增长：首个间隔 %s，末个 %s（全部 %v）", first, last, gaps)
	}
	for i := 1; i < len(gaps); i++ {
		if gaps[i] < gaps[i-1]/2 {
			t.Errorf("第 %d 个间隔 %s 比前一个 %s 还短得多（全部 %v）", i, gaps[i], gaps[i-1], gaps)
			break
		}
	}
}

func TestLoopStopsWhenContextCancelled(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	src := newSource("fake", ref("1"))
	src.setItem("1", "招募A", ev("招募A", base, base.Add(time.Hour), annsync.StatusOK))

	s := annsync.NewFeature(doc, w, src, nil, testCfg(nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "首轮完成", func() bool { return src.total() >= 1 })

	cancel()
	n := src.total()
	waitNot(t, "取消后还在抓", func() bool { return src.total() > n })
}

func TestStartRejectsMissingDependencies(t *testing.T) {
	doc := storetest.NewMem()
	src := newSource("fake")
	if err := annsync.NewFeature(nil, newWriter(), src, nil, testCfg(nil)).Start(context.Background(), nil); err == nil {
		t.Error("doc 为空却启动成功")
	}
	if err := annsync.NewFeature(doc, nil, src, nil, testCfg(nil)).Start(context.Background(), nil); err == nil {
		t.Error("cal 为空却启动成功")
	}
	if err := annsync.NewFeature(doc, newWriter(), nil, nil, testCfg(nil)).Start(context.Background(), nil); err == nil {
		t.Error("src 为空却启动成功")
	}
}

func TestTwoSourcesShareOneDocWithoutColliding(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	a := newSource("alpha", ref("1"))
	a.setItem("1", "甲活动", ev("甲活动", base, base.Add(time.Hour), annsync.StatusOK))
	b := newSource("beta", ref("1")) // 同样的 refID，不同源
	b.setItem("1", "乙活动", ev("乙活动", base, base.Add(2*time.Hour), annsync.StatusOK))

	start(t, annsync.NewFeature(doc, w, a, nil, testCfg(nil)))
	start(t, annsync.NewFeature(doc, w, b, nil, testCfg(nil)))

	waitFor(t, "两个源的同名 refID 各自成一条", func() bool {
		_, ok1 := w.inner.Get("alpha:1:0")
		_, ok2 := w.inner.Get("beta:1:0")
		return ok1 && ok2
	})
	recs, err := annsync.ReadRecs(doc, "")
	if err != nil {
		t.Fatalf("ReadRecs: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("跨源记录数 = %d, want 2", len(recs))
	}
	if only, err := annsync.ReadRecs(doc, "beta"); err != nil || len(only) != 1 {
		t.Errorf("按源过滤 = %d 条 (err=%v), want 1", len(only), err)
	}
}

// 真官网实测：同一场测试被两三篇公告声明了同一个时间窗口（逐光测试 / 启明测试）。
// 日历只留一行，报警只喊还没结束的那几条——旧公告每轮都喊会把还能伤到人的那行淹没。
func TestDuplicateAlarmSkipsEndedWindows(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()
	live := ev("进行中的测试", base.Add(-time.Hour), base.Add(time.Hour), annsync.StatusOK)
	old := ev("早结束的测试", base.Add(-4*time.Hour), base.Add(-3*time.Hour), annsync.StatusOK)
	src := newSource("fake", ref("1"), ref("2"), ref("3"), ref("4"), ref("5"), ref("6"))
	src.setItem("1", "a", live)
	src.setItem("2", "b", live)
	src.setItem("3", "c", live)
	src.setItem("4", "d", old)
	src.setItem("5", "e", old)
	src.setItem("6", "f", old)

	var mu sync.Mutex
	var alarms []string
	cfg := testCfg(func() time.Time { return base })
	cfg.Logf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if !strings.Contains(line, "同一个时间窗口") {
			return
		}
		mu.Lock()
		alarms = append(alarms, line)
		mu.Unlock()
	}
	start(t, annsync.NewFeature(doc, w, src, nil, cfg))

	waitFor(t, "两条重复都投影完", func() bool { return w.bulksDone() >= 2 })
	time.Sleep(30 * time.Millisecond)

	// 每组 6 篇各留一行：活着的 fake:1:0、已结束的 fake:4:0
	for _, id := range []string{"fake:1:0", "fake:4:0"} {
		if _, ok := w.inner.Get(id); !ok {
			t.Errorf("日历里没有 %s（每组的头一篇该在）", id)
		}
	}
	for _, id := range []string{"fake:2:0", "fake:3:0", "fake:5:0", "fake:6:0"} {
		if _, ok := w.inner.Get(id); ok {
			t.Errorf("重复的 %s 也进了日历，去重没生效", id)
		}
	}
	if n := len(w.inner.Active(base)); n != 1 {
		t.Errorf("进行中行数 = %d, want 1（只剩进行中的那一组）", n)
	}
}

// 验证公告保留期（默认 63 天 / 9 周）：
// 1. 超过 63 天的旧公告在 loadItems 处被过滤，不参与投影、不进 activity 桶与日历；
// 2. 63 天以内的公告正常进入日历；
// 3. 磁盘 news 桶依然完整保留所有历史快照（只读侧过滤，不删落盘数据）；
// 4. 未填发布时间（零值）的条目向后兼容保留；
// 5. 支持配置自定义保留期。
func TestRetentionWindowFiltersAncientAnnouncements(t *testing.T) {
	doc := storetest.NewMem()
	w := newWriter()

	// 四篇公告：近期 (10天前)、临界 (62天前)、远古 (65天前)、零值时间
	rRecent := ref("recent")
	rAncient := ref("ancient")
	rBoundary := ref("boundary")
	rZero := ref("zero")

	src := newSource("fake", rRecent, rAncient, rBoundary, rZero)
	src.setItemWithPub("recent", "近期活动", base.Add(-10*24*time.Hour),
		ev("近期活动", base.Add(-10*24*time.Hour), base.Add(10*24*time.Hour), annsync.StatusOK))
	src.setItemWithPub("ancient", "远古活动", base.Add(-65*24*time.Hour),
		ev("远古活动", base.Add(-65*24*time.Hour), base.Add(-30*24*time.Hour), annsync.StatusOK))
	src.setItemWithPub("boundary", "临界活动", base.Add(-62*24*time.Hour),
		ev("临界活动", base.Add(-62*24*time.Hour), base.Add(5*24*time.Hour), annsync.StatusOK))
	src.setItem("zero", "未标注时间活动",
		ev("未标注时间活动", base.Add(-5*time.Hour), base.Add(5*time.Hour), annsync.StatusOK))

	cfg := testCfg(func() time.Time { return base })
	// 使用默认的 63 天 Retention（cfg.Retention 为 0，启动时 withDefaults 填充为 63天）
	start(t, annsync.NewFeature(doc, w, src, nil, cfg))

	waitFor(t, "全量投影完成", func() bool { return w.bulksDone() >= 1 })

	// 1. 63 天以内的活动必须在日历中
	if _, ok := w.inner.Get("fake:recent:0"); !ok {
		t.Errorf("近期活动 fake:recent:0 应当在日历中")
	}
	if _, ok := w.inner.Get("fake:boundary:0"); !ok {
		t.Errorf("62天前的临界活动 fake:boundary:0 应当在日历中")
	}
	if _, ok := w.inner.Get("fake:zero:0"); !ok {
		t.Errorf("零值发布时间的活动 fake:zero:0 应当向后兼容保留在日历中")
	}

	// 2. 超过 63 天的远古活动被过滤，不在日历中
	if _, ok := w.inner.Get("fake:ancient:0"); ok {
		t.Errorf("65天前的远古活动 fake:ancient:0 应当被过滤，不应出现在日历中")
	}

	// 3. 验证 activity 桶也只包含这 3 条活动记录
	recs, err := annsync.ReadRecs(doc, "fake")
	if err != nil {
		t.Fatalf("ReadRecs: %v", err)
	}
	if len(recs) != 3 {
		t.Errorf("activity 桶记录数 = %d, want 3", len(recs))
	}

	// 4. 核心验证：磁盘 news 桶依然完整保留了 4 篇公告（包括远古公告），证明落盘数据未受破坏
	var ancientItem annsync.Item
	hasAncient, err := doc.Get("news", "fake:ancient", &ancientItem)
	if err != nil || !hasAncient {
		t.Errorf("news 桶中必须保留远古公告快照（落盘资产不丢），has=%v, err=%v", hasAncient, err)
	}
	items, err := annsync.ReadItems(doc, "fake")
	if err != nil {
		t.Fatalf("ReadItems: %v", err)
	}
	if len(items) != 4 {
		t.Errorf("news 桶快照总数 = %d, want 4（磁盘全量归档）", len(items))
	}

	// 5. 验证自定义 Retention（例如缩紧到 7 天）
	w2 := newWriter()
	cfgShort := testCfg(func() time.Time { return base })
	cfgShort.Retention = 7 * 24 * time.Hour
	// 重新起一个实例读同一个 doc
	feat2 := annsync.NewFeature(doc, w2, src, nil, cfgShort)
	start(t, feat2)
	feat2.Refresh()

	waitFor(t, "自定义保留期重投影完成", func() bool { return w2.bulksDone() >= 1 })

	// 10 天前的活动在 7 天保留期下也应当被过滤
	if _, ok := w2.inner.Get("fake:recent:0"); ok {
		t.Errorf("自定义 7 天保留期下，10天前的活动 fake:recent:0 应当被过滤")
	}
	// 零值时间的活动依然兼容保留
	if _, ok := w2.inner.Get("fake:zero:0"); !ok {
		t.Errorf("自定义 7 天保留期下，零值时间活动 fake:zero:0 应当保留")
	}
}
