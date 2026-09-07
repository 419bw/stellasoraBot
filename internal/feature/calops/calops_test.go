package calops_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/feature/calops"
	"xingta/internal/kernel/calendar"
	"xingta/internal/qq"
	"xingta/internal/store/storetest"
)

// 这些用例跑的是真链路：假源 → 真同步引擎 → 真存储 → 真日历 → calops 命令。
// 只有这样才能证明"覆盖不被刷新冲掉"是真的，而不是两个假件互相配合演出来的。

var (
	zone = time.FixedZone("CST", 8*60*60)
	now  = time.Date(2026, 9, 2, 12, 0, 0, 0, zone)
)

// ---- 假源 ----------------------------------------------------------------

type fakeSource struct {
	mu    sync.Mutex
	items map[string]annsync.Item
	hash  map[string]string // refID → 指纹种子，改一下就模拟"官方改了公告"
}

func newSource() *fakeSource {
	return &fakeSource{items: map[string]annsync.Item{}, hash: map[string]string{}}
}

func (f *fakeSource) Name() string { return "test" }

func (f *fakeSource) List(ctx context.Context) ([]annsync.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	refs := make([]annsync.Ref, 0, len(f.items))
	for id := range f.items {
		refs = append(refs, annsync.Ref{ID: id, Hash: sha256.Sum256([]byte(id + f.hash[id]))})
	}
	return refs, nil
}

func (f *fakeSource) Fetch(ctx context.Context, r annsync.Ref) (annsync.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[r.ID]
	it.Ref = r
	return it, nil
}

func (f *fakeSource) set(id string, it annsync.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = it
	f.hash[id] = ""
}

// revise 改动一条公告的内容与指纹，模拟"官方改口"，逼引擎下一轮重抓。
func (f *fakeSource) revise(id string, it annsync.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = it
	f.hash[id] = time.Now().Format(time.RFC3339Nano)
}

func (f *fakeSource) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.items))
	for id := range f.items {
		out = append(out, id)
	}
	return out
}

// ---- 装置 ----------------------------------------------------------------

type rig struct {
	src  *fakeSource
	doc  *storetest.MemDoc
	cal  *calendar.Store
	reg  *command.Registry
	eng  annsync.Syncer
	logs []string

	mu sync.Mutex
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{src: newSource(), doc: storetest.NewMem(), cal: calendar.NewStore(), reg: command.NewRegistry()}

	cfg := annsync.Config{
		Interval: 5 * time.Millisecond, FullEvery: time.Hour,
		MinGap: time.Millisecond, BackoffBase: 5 * time.Millisecond,
		Now:  func() time.Time { return now },
		Logf: r.log,
	}
	r.eng = annsync.NewFeature(r.doc, r.cal, r.src, nil, cfg)

	ops := calops.New(r.doc, r.reg, calops.Config{
		Source: "test", Zone: zone, PageSize: 4,
		Now:     func() time.Time { return now },
		Refresh: r.eng, // 接上引擎：写完覆盖立刻重投影，不用等下一轮
		Logf:    r.log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // 引擎循环随测试停，别漏 goroutine 去写别的用例的假件
	if err := r.eng.Start(ctx, nil); err != nil {
		t.Fatalf("引擎 Start: %v", err)
	}
	if err := ops.Start(ctx, nil); err != nil {
		t.Fatalf("calops Start: %v", err)
	}
	return r
}

func (r *rig) log(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, format)
}

// run 调一条命令。管理员身份由机制层查，这里直接调命令体，所以不受门槛影响。
func (r *rig) run(t *testing.T, name string, args ...string) string {
	t.Helper()
	c, ok := r.reg.Lookup(name)
	if !ok {
		t.Fatalf("命令 %q 没注册上", name)
	}
	m := &qq.Message{
		Kind: qq.EventGroupAtMessage, ID: "M1", GroupOpenID: "G1", Content: name,
		Author: qq.Author{MemberOpenID: "ADMIN1", Username: "魔王大人", MemberRole: "owner"},
	}
	text, err := c.Run(context.Background(), m, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return text
}

// seed 灌三条公告：一条干净、一条解析不出起止（pending）、一条整条可疑（一个区间都没有）。
func (r *rig) seed(t *testing.T) {
	t.Helper()
	r.src.set("1", annsync.Item{
		Title: "乾淨活動", Thumbnail: "p1.png",
		Events: []annsync.Event{{
			Title: "乾淨活動", Label: "活動時間", Start: now.Add(-24 * time.Hour), End: now.Add(72 * time.Hour),
			Status: annsync.StatusOK, Fragment: "2026-09-01 12:00 ~ 2026-09-05 12:00",
		}},
	})
	r.src.set("2", annsync.Item{
		Title: "說不清的活動", Thumbnail: "p2.png",
		Events: []annsync.Event{{
			Title: "說不清的活動", Label: "開放時間", Status: annsync.StatusPending,
			Fragment: "2026-09-01 12:00 開放後常駐",
		}},
	})
	r.src.set("3", annsync.Item{Title: "全是圖片的公告", Thumbnail: "p3.png", Suspect: true})

	r.waitRecs(t, 2)
}

func (r *rig) waitRecs(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recs, err := annsync.ReadRecs(r.doc, "test")
		if err == nil && len(recs) == n {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	recs, _ := annsync.ReadRecs(r.doc, "test")
	t.Fatalf("等不到 %d 条活动记录，实际 %d 条：%v", n, len(recs), recs)
}

func (r *rig) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("超时：%s", what)
}

// ---- 用例 ----------------------------------------------------------------

func TestReviewListsPendingRecordsAndSuspectPosts(t *testing.T) {
	r := newRig(t)
	r.seed(t)

	got := r.run(t, "待確認")
	if !strings.Contains(got, "test:2:0") {
		t.Errorf("待确认桶里没有解析不出的那条：%q", got)
	}
	if !strings.Contains(got, "說不清的活動") || !strings.Contains(got, "pending") {
		t.Errorf("没带上标题与状态：%q", got)
	}
	if !strings.Contains(got, "開放後常駐") {
		t.Errorf("没带上原文片段，人工没法核对：%q", got)
	}
	if !strings.Contains(got, "test:3") || !strings.Contains(got, "全是圖片的公告") {
		t.Errorf("整条可疑、一个区间都没解析出来的公告没被列出（这类漏抓会是静默的）：%q", got)
	}
	if strings.Contains(got, "test:1:0") {
		t.Errorf("解析干净的记录不该出现在待确认桶里：%q", got)
	}
	if strings.Contains(got, "乾淨活動") {
		t.Errorf("解析干净的记录不该出现在待确认桶里：%q", got)
	}
}

func TestReviewSaysEmptyWhenNothingToReview(t *testing.T) {
	r := newRig(t)
	r.src.set("1", annsync.Item{
		Title: "乾淨活動",
		Events: []annsync.Event{{
			Title: "乾淨活動", Label: "活動時間", Start: now, End: now.Add(time.Hour), Status: annsync.StatusOK,
		}},
	})
	r.waitRecs(t, 1)

	if got := r.run(t, "待確認"); !strings.Contains(got, "空的") {
		t.Errorf("全部解析干净时该说桶是空的：%q", got)
	}
}

func TestOverrideChangesCalendarImmediately(t *testing.T) {
	r := newRig(t)
	r.seed(t)
	r.waitFor(t, "乾淨活動进日历", func() bool { _, ok := r.cal.Get("test:1:0"); return ok })

	newEnd := time.Date(2026, 9, 30, 23, 0, 0, 0, zone)
	got := r.run(t, "覆蓋", "test:1:0", "2026-09-01", "12:00", "2026-09-30", "23:00", "官方延期")

	if !strings.Contains(got, "已覆盖 test:1:0") {
		t.Errorf("回复没确认改了哪条：%q", got)
	}
	if !strings.Contains(got, "官方延期") {
		t.Errorf("回复没回显备注：%q", got)
	}
	if !strings.Contains(got, "原解析") {
		t.Errorf("回复没给出原解析值，人工核不出来改了什么：%q", got)
	}

	r.waitFor(t, "覆盖生效到日历", func() bool {
		a, ok := r.cal.Get("test:1:0")
		return ok && a.End.Equal(newEnd)
	})

	o, ok, err := annsync.NewOverrideStore(r.doc).Get("test:1:0")
	if err != nil || !ok {
		t.Fatalf("覆盖没落盘：ok=%v err=%v", ok, err)
	}
	if !o.End.Equal(newEnd) || o.By != "魔王大人" || o.Note != "官方延期" {
		t.Errorf("覆盖内容 = %+v", o)
	}
}

// 覆盖必须比刷新命长：官方改口、引擎重抓、重投影，人工核过的那一份都得还在。
func TestOverrideSurvivesReparsedSource(t *testing.T) {
	r := newRig(t)
	r.seed(t)
	r.waitFor(t, "进日历", func() bool { _, ok := r.cal.Get("test:1:0"); return ok })

	human := time.Date(2026, 10, 1, 0, 0, 0, 0, zone)
	r.run(t, "覆蓋", "test:1:0", "2026-09-01", "12:00", "2026-10-01", "00:00", "人工核对")

	r.waitFor(t, "覆盖生效", func() bool {
		a, ok := r.cal.Get("test:1:0")
		return ok && a.End.Equal(human)
	})

	// 官方把公告改了：时间完全变了一套
	r.src.revise("1", annsync.Item{
		Title: "乾淨活動",
		Events: []annsync.Event{{
			Title: "乾淨活動", Label: "活動時間",
			Start: now.Add(-48 * time.Hour), End: now.Add(24 * time.Hour), Status: annsync.StatusOK,
		}},
	})

	r.waitFor(t, "重抓后覆盖仍在", func() bool {
		a, ok := r.cal.Get("test:1:0")
		return ok && a.End.Equal(human)
	})
	// 再等几轮，确认不是一次侥幸
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		a, ok := r.cal.Get("test:1:0")
		if !ok || !a.End.Equal(human) {
			t.Fatalf("后续刷新把人工覆盖冲掉了：%+v (ok=%v)", a, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec, ok, err := annsync.ReadRec(r.doc, "test:1:0")
	if err != nil || !ok {
		t.Fatalf("ReadRec: ok=%v err=%v", ok, err)
	}
	if rec.End.Equal(human) {
		t.Errorf("存储里的解析原值被覆盖改写了（覆盖只应作用于投影）：%v", rec.End)
	}
}

func TestConfirmFreezesParsedValues(t *testing.T) {
	r := newRig(t)
	r.seed(t)
	r.waitFor(t, "进日历", func() bool { _, ok := r.cal.Get("test:1:0"); return ok })

	got := r.run(t, "確認", "test:1:0")
	if !strings.Contains(got, "已固化") {
		t.Errorf("回复 = %q, want 含\"已固化\"", got)
	}

	o, ok, err := annsync.NewOverrideStore(r.doc).Get("test:1:0")
	if err != nil || !ok {
		t.Fatalf("固化没落盘：ok=%v err=%v", ok, err)
	}
	rec, _, _ := annsync.ReadRec(r.doc, "test:1:0")
	if !o.Start.Equal(rec.Start) || !o.End.Equal(rec.End) {
		t.Errorf("固化的值 = %s ~ %s, 解析值是 %s ~ %s", o.Start, o.End, rec.Start, rec.End)
	}
	if o.By != "魔王大人" {
		t.Errorf("没记下是谁固化的：By = %q", o.By)
	}

	// 固化之后官方改口，日历仍按人工核过的那一份走
	frozen := o.End
	changed := now.Add(time.Hour)
	r.src.revise("1", annsync.Item{
		Title: "乾淨活動",
		Events: []annsync.Event{{
			Title: "乾淨活動", Label: "活動時間", Start: now, End: changed, Status: annsync.StatusOK,
		}},
	})
	r.waitFor(t, "重抓后解析值已变", func() bool {
		fresh, ok, _ := annsync.ReadRec(r.doc, "test:1:0")
		return ok && fresh.End.Equal(changed)
	})

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if a, ok := r.cal.Get("test:1:0"); !ok || !a.End.Equal(frozen) {
			t.Fatalf("固化没挡住改期：日历 = %+v (ok=%v), want End=%s", a, ok, frozen)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConfirmRefusesRecordWithoutTimes(t *testing.T) {
	r := newRig(t)
	r.seed(t)

	got := r.run(t, "確認", "test:2:0") // 那条 pending 的没有时间
	if !strings.Contains(got, "還沒有解析出时间") && !strings.Contains(got, "没有解析出时间") {
		t.Errorf("回复 = %q, want 说明这条没法固化", got)
	}
	if _, ok, _ := annsync.NewOverrideStore(r.doc).Get("test:2:0"); ok {
		t.Error("没有时间却写了覆盖：日历上会多出一条零值区间")
	}
}

func TestOverrideRejectsBadInput(t *testing.T) {
	r := newRig(t)
	r.seed(t)

	cases := []struct {
		what string
		args []string
		want string
	}{
		{"缺 ID", nil, "缺少记录 ID"},
		{"ID 不完整", []string{"1:0", "2026-09-01", "12:00"}, "看着不完整"},
		{"ID 不存在", []string{"test:99:0", "2026-09-01", "12:00", "2026-09-02", "12:00"}, "找不到"},
		{"缺时间", []string{"test:1:0", "2026-09-01", "12:00"}, "结束时间没看懂"},
		{"时间不是日期", []string{"test:1:0", "下周三", "12:00", "2026-09-02", "12:00"}, "开始时间没看懂"},
		{"起止倒挂", []string{"test:1:0", "2026-09-10", "12:00", "2026-09-01", "12:00"}, "不在开始时间"},
	}
	for _, c := range cases {
		got := r.run(t, "覆蓋", c.args...)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s：回复 = %q, want 含 %q", c.what, got, c.want)
		}
	}
	if _, ok, _ := annsync.NewOverrideStore(r.doc).Get("test:99:0"); ok {
		t.Error("不存在的 ID 也写了覆盖：这会变成一条永远不生效的孤儿覆盖")
	}
}

// 时间写法要宽容：QQ 里日期和时刻是两个字段，用户也可能只写日期或用斜杠。
func TestOverrideAcceptsTimeShapes(t *testing.T) {
	cases := []struct {
		what      string
		args      []string
		wantStart time.Time
		wantEnd   time.Time
	}{
		{"两个字段", []string{"2026-09-01", "12:00", "2026-09-30", "23:00"},
			time.Date(2026, 9, 1, 12, 0, 0, 0, zone), time.Date(2026, 9, 30, 23, 0, 0, 0, zone)},
		{"斜杠日期", []string{"2026/09/01", "12:00", "2026/09/30", "23:00"},
			time.Date(2026, 9, 1, 12, 0, 0, 0, zone), time.Date(2026, 9, 30, 23, 0, 0, 0, zone)},
		{"省略年份", []string{"09-01", "12:00", "09-30", "23:00"},
			time.Date(2026, 9, 1, 12, 0, 0, 0, zone), time.Date(2026, 9, 30, 23, 0, 0, 0, zone)},
		{"只写日期：结束含当天整天", []string{"2026-09-01", "2026-09-30"},
			time.Date(2026, 9, 1, 0, 0, 0, 0, zone), time.Date(2026, 9, 30, 23, 59, 59, 0, zone)},
		{"带秒", []string{"2026-09-01", "12:00:30", "2026-09-30", "23:00:00"},
			time.Date(2026, 9, 1, 12, 0, 30, 0, zone), time.Date(2026, 9, 30, 23, 0, 0, 0, zone)},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			r := newRig(t)
			r.seed(t)
			r.waitFor(t, "进日历", func() bool { _, ok := r.cal.Get("test:1:0"); return ok })

			r.run(t, "覆蓋", append([]string{"test:1:0"}, c.args...)...)

			o, ok, err := annsync.NewOverrideStore(r.doc).Get("test:1:0")
			if err != nil || !ok {
				t.Fatalf("覆盖没落盘：ok=%v err=%v", ok, err)
			}
			if !o.Start.Equal(c.wantStart) || !o.End.Equal(c.wantEnd) {
				t.Errorf("解析成 %s ~ %s, want %s ~ %s", o.Start, o.End, c.wantStart, c.wantEnd)
			}
		})
	}
}

func TestHideRemovesFromCalendarAndShowRestores(t *testing.T) {
	r := newRig(t)
	r.seed(t)
	r.waitFor(t, "进日历", func() bool { _, ok := r.cal.Get("test:1:0"); return ok })

	got := r.run(t, "隱藏", "test:1:0", "这条不是活动")
	if !strings.Contains(got, "已隐藏") || !strings.Contains(got, "显示") {
		t.Errorf("回复 = %q, want 确认并告知怎么撤销", got)
	}
	r.waitFor(t, "从日历撤下", func() bool { _, ok := r.cal.Get("test:1:0"); return !ok })

	o, _, _ := annsync.NewOverrideStore(r.doc).Get("test:1:0")
	if !o.Hide || o.Note != "这条不是活动" {
		t.Errorf("覆盖 = %+v, want Hide=true 且带备注", o)
	}

	if got := r.run(t, "顯示", "test:1:0"); !strings.Contains(got, "已恢复") {
		t.Errorf("撤销的回复 = %q", got)
	}
	r.waitFor(t, "回到日历", func() bool { _, ok := r.cal.Get("test:1:0"); return ok })

	if got := r.run(t, "顯示", "test:1:0"); !strings.Contains(got, "本来就没被隐藏") {
		t.Errorf("重复撤销的回复 = %q, want 说明不用撤销", got)
	}
}

// 运维命令改的是全群看到的活动时间，全部必须是管理员命令：
// 门槛由机制层统一查，功能只负责声明。
func TestEveryOpsCommandIsAdminOnly(t *testing.T) {
	r := newRig(t)
	for _, name := range []string{"待確認", "覆蓋", "確認", "隱藏", "顯示"} {
		c, ok := r.reg.Lookup(name)
		if !ok {
			t.Errorf("命令 %q 没注册上", name)
			continue
		}
		if !c.Admin {
			t.Errorf("命令 %q 不是管理员命令：任何人都能改活动时间", name)
		}
		if c.Usage == "" {
			t.Errorf("命令 %q 没有用法说明，幫助里会是一行空白", name)
		}
	}
}

func TestReviewPaging(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 6; i++ {
		id := strings.Repeat("x", i+1) // 每条不同 refID
		r.src.set(id, annsync.Item{
			Title: "說不清" + id,
			Events: []annsync.Event{{
				Title: "說不清" + id, Label: "開放時間", Status: annsync.StatusPending, Fragment: "原文片段",
			}},
		})
	}
	r.waitRecs(t, 6)

	page1 := r.run(t, "待確認")
	if !strings.Contains(page1, "6 条，第 1/2 页") {
		t.Errorf("头部计数不对（每页 4 条）：%q", page1)
	}
	if !strings.Contains(page1, "发「待确认 2」看下一页") {
		t.Errorf("没有翻页提示：%q", page1)
	}
	page2 := r.run(t, "待確認", "2")
	if !strings.Contains(page2, "第 2/2 页") || strings.Contains(page2, "看下一页") {
		t.Errorf("末页不对：%q", page2)
	}
	if n := strings.Count(page2, "\n· "); n != 2 {
		t.Errorf("末页有 %d 行, want 2：%q", n, page2)
	}
}

// 没有接 Refresh 依赖时命令照样能用：覆盖会在下一轮同步生效，只是不当场生效。
func TestWorksWithoutRefreshHook(t *testing.T) {
	doc := storetest.NewMem()
	reg := command.NewRegistry()
	src := newSource()
	src.set("1", annsync.Item{
		Title: "乾淨活動",
		Events: []annsync.Event{{
			Title: "乾淨活動", Label: "活動時間", Start: now, End: now.Add(time.Hour), Status: annsync.StatusOK,
		}},
	})
	cal := calendar.NewStore()
	eng := annsync.NewFeature(doc, cal, src, nil, annsync.Config{
		Interval: 5 * time.Millisecond, FullEvery: time.Hour, MinGap: time.Millisecond, BackoffBase: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Start(ctx, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ops := calops.New(doc, reg, calops.Config{
		Source: "test", Zone: zone,
		Now: func() time.Time { return now }, // Refresh 为 nil
	})
	if err := ops.Start(ctx, nil); err != nil {
		t.Fatalf("calops Start: %v", err)
	}

	// 覆盖只认已存在的记录，所以先等首轮把 test:1:0 写出来
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := annsync.ReadRec(doc, "test:1:0"); ok {
			break
		}
		time.Sleep(3 * time.Millisecond)
	}

	c, _ := reg.Lookup("覆蓋")
	m := &qq.Message{Kind: qq.EventGroupAtMessage, ID: "M1", GroupOpenID: "G1",
		Author: qq.Author{MemberOpenID: "A", Username: "魔王大人", MemberRole: "owner"}}
	newEnd := time.Date(2026, 12, 31, 23, 0, 0, 0, zone)
	text, err := c.Run(ctx, m, []string{"test:1:0", "2026-09-02", "12:00", "2026-12-31", "23:00"})
	if err != nil {
		t.Fatalf("覆蓋: %v", err)
	}
	if !strings.Contains(text, "已覆盖") {
		t.Fatalf("覆蓋没写成：%q", text)
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a, ok := cal.Get("test:1:0"); ok && a.End.Equal(newEnd) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	a, _ := cal.Get("test:1:0")
	t.Fatalf("没接 Refresh 时覆盖也应在下一轮生效，实际日历仍是 %+v", a)
}

// 全量实测踩出来的：345 篇公告里 31 篇未采信，九成是半年前的维护说明。
// 待确认桶是给"这一轮是不是漏了活动"用的，旧公告天天占满第一页，这个命令就等于没有。
func TestReviewListsRecentSuspectsAndHidesAncient(t *testing.T) {
	r := newRig(t)
	r.src.set("20", annsync.Item{
		Title: "半年前的维护说明", Published: now.AddDate(0, -6, 0), Suspect: true,
		Note: "正文有 5 个主时间窗口，像是汇总公告，未采信",
	})
	r.src.set("21", annsync.Item{
		Title: "上周漏抓的活动", Published: now.AddDate(0, 0, -7), Suspect: true,
		Note: "标题像活动公告，但正文没抓到时间窗口",
	})
	r.src.set("22", annsync.Item{Title: "昨天的漏抓", Published: now.AddDate(0, 0, -1), Suspect: true})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if items, _ := annsync.ReadItems(r.doc, "test"); len(items) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等不到 3 条公告快照")
		}
		time.Sleep(3 * time.Millisecond)
	}

	got := r.run(t, "待确认")
	if strings.Contains(got, "半年前的维护说明") {
		t.Errorf("30 天以外的未采信公告还在占页面：%q", got)
	}
	if !strings.Contains(got, "更早未采信的 1 条") {
		t.Errorf("省略掉的旧公告没交代数量，看的人以为桶是空的：%q", got)
	}
	recent, older := strings.Index(got, "昨天的漏抓"), strings.Index(got, "上周漏抓的活动")
	if recent < 0 || older < 0 {
		t.Fatalf("近 30 天的两条都该列出：%q", got)
	}
	if recent > older {
		t.Errorf("没按新→旧排：%q", got)
	}
	if !strings.Contains(got, "09-01 12:00 发") {
		t.Errorf("列出公告没带发布时间，没法判断这条还要不要看：%q", got)
	}
	if !strings.Contains(got, "整条没解析出时间") {
		t.Errorf("源没写 Note 时该有套话兜底：%q", got)
	}
}

// 主名必须是简体：群里的人对着简体官网打字，「帮助」里排第一的名字得是他敲得出来的。
// 繁体留在别名里不删——老用户的手感不能因为换站点就断。这条同时钉住两边解析到同一个命令：
// 手改名字时最容易把别名一起简掉，等于悄悄把繁体入口删了。
func TestSimplifiedNameIsPrimaryAndTraditionalStillRoutes(t *testing.T) {
	r := newRig(t)
	for simp, trad := range map[string]string{
		"待确认": "待確認", "覆盖": "覆蓋", "确认": "確認", "隐藏": "隱藏", "显示": "顯示",
	} {
		for _, name := range []string{simp, trad} {
			c, ok := r.reg.Lookup(name)
			if !ok {
				t.Errorf("命令 %q 没注册上", name)
				continue
			}
			if c.Name != simp {
				t.Errorf("打 %q 解析到的主名 = %q, want %q", name, c.Name, simp)
			}
		}
	}
}
