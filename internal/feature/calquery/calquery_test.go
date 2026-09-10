package calquery_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/feature/calquery"
	"xingta/internal/kernel/calendar"
	"xingta/internal/qq"
)

// 这个功能的构造参数里没有 store.Doc —— 它不存东西，所以拿不到存储面。
// 分层契约就是这么执行的：能力由注入决定，不是靠功能自觉。

var (
	zone = time.FixedZone("CST", 8*60*60)
	now  = time.Date(2026, 9, 2, 12, 0, 0, 0, zone)
)

func at(month, day, hour, min int) time.Time {
	return time.Date(2026, time.Month(month), day, hour, min, 0, 0, zone)
}

type fakeStatus struct {
	st  annsync.Status
	err error
}

func (f fakeStatus) Status() (annsync.Status, error) { return f.st, f.err }

// fixture 灌一份像真数据的日历：三条进行中（其中一条 16 小时后结束）、一条已结束、一条两天后开始。
func fixture() *calendar.Store {
	cal := calendar.NewStore()
	cal.BulkUpsert([]calendar.Activity{
		{ID: "s:1:0", Game: "s", Title: "悠悠漫時，愜意夕暮", Start: at(8, 18, 0, 0), End: at(9, 8, 10, 59)},
		{ID: "s:2:0", Game: "s", Title: "絢麗夜空、煙火綻放", Start: at(9, 1, 12, 0), End: at(9, 22, 10, 59)},
		{ID: "s:3:0", Game: "s", Title: "緊急懸賞", Start: at(9, 1, 4, 0), End: at(9, 3, 4, 0)},
		{ID: "s:4:0", Game: "s", Title: "沁夏滋味，獨享時光", Start: at(8, 1, 4, 0), End: at(8, 31, 3, 59)},
		{ID: "s:5:0", Game: "s", Title: "雪融時分新芽綻", Start: at(9, 4, 12, 0), End: at(9, 25, 10, 59)},
	})
	return cal
}

// start 起功能并返回命令表。
func start(t *testing.T, cal calendar.View, cfg calquery.Config) *command.Registry {
	t.Helper()
	reg := command.NewRegistry()
	f := calquery.New(reg, cal, cfg)
	if err := f.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return reg
}

// run 直接调命令体：机制层（回复几次、截断、权限）在 command 包已经测过了。
func run(t *testing.T, reg *command.Registry, name string, args ...string) string {
	t.Helper()
	c, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("命令 %q 没注册上", name)
	}
	m := &qq.Message{Kind: qq.EventGroupAtMessage, ID: "M1", GroupOpenID: "G1", Content: name}
	rep, err := c.Run(context.Background(), m, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return rep.Text
}

func TestRegistersAllQueryCommands(t *testing.T) {
	reg := start(t, fixture(), calquery.Config{Now: func() time.Time { return now }})
	for _, name := range []string{"events", "ending", "upcoming", "help"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("命令 %q 没注册上", name)
		}
	}
}

// 问"现在有什么活动"，真正要紧的是快跑掉的那几个，所以按结束时间升序。
func TestActiveListsSoonestEndingFirst(t *testing.T) {
	reg := start(t, fixture(), calquery.Config{Now: func() time.Time { return now }})
	got := run(t, reg, "events")

	if !strings.Contains(got, "3 条") {
		t.Errorf("头部计数不对：%q", got)
	}
	iUrgent := strings.Index(got, "緊急懸賞")
	iCostume := strings.Index(got, "悠悠漫時，愜意夕暮")
	iBanner := strings.Index(got, "絢麗夜空、煙火綻放")
	if !(iUrgent >= 0 && iUrgent < iCostume && iCostume < iBanner) {
		t.Errorf("没按结束时间升序排（紧急的该排第一）：%q", got)
	}
	if strings.Contains(got, "沁夏滋味") {
		t.Errorf("已结束的活动混进了「进行中」：%q", got)
	}
	if strings.Contains(got, "雪融時分") {
		t.Errorf("还没开始的活动混进了「进行中」：%q", got)
	}
	if !strings.Contains(got, "剩 16小时") {
		t.Errorf("剩余时长没说成人话：%q", got)
	}
	if !strings.Contains(got, "09-08 10:59") {
		t.Errorf("结束时间没按 月-日 时:分 显示：%q", got)
	}
}

func TestPagingKeepsOneReplyPerPage(t *testing.T) {
	cal := calendar.NewStore()
	for i := 0; i < 7; i++ {
		cal.Upsert(calendar.Activity{
			ID: id(i), Title: title(i), Start: at(9, 1, 0, 0), End: at(9, 10+i, 0, 0),
		})
	}
	reg := start(t, cal, calquery.Config{Now: func() time.Time { return now }, PageSize: 3})

	page1 := run(t, reg, "events")
	if n := strings.Count(page1, "\n· "); n != 3 {
		t.Errorf("第 1 页有 %d 行, want 3：%q", n, page1)
	}
	if !strings.Contains(page1, "7 条，第 1/3 页") {
		t.Errorf("头部没写清总数与页码：%q", page1)
	}
	if !strings.Contains(page1, "发「events 2」看下一页") {
		t.Errorf("没有翻页提示：%q", page1)
	}

	page3 := run(t, reg, "events", "3")
	if n := strings.Count(page3, "\n· "); n != 1 {
		t.Errorf("末页有 %d 行, want 1（7 条 / 每页 3）：%q", n, page3)
	}
	if strings.Contains(page3, "看下一页") {
		t.Errorf("末页还在提示翻页：%q", page3)
	}

	// 页码超出范围不该报错，退回最后一页：用户多打一位数字而已
	if beyond := run(t, reg, "events", "99"); !strings.Contains(beyond, "第 3/3 页") {
		t.Errorf("超范围页码没退回末页：%q", beyond)
	}
	if junk := run(t, reg, "events", "二"); !strings.Contains(junk, "第 1/3 页") {
		t.Errorf("非数字页码没退回第 1 页：%q", junk)
	}
}

func TestEndingWithinHonorsHoursArg(t *testing.T) {
	reg := start(t, fixture(), calquery.Config{Now: func() time.Time { return now }})

	def := run(t, reg, "ending")
	if !strings.Contains(def, "緊急懸賞") || strings.Contains(def, "悠悠漫時") {
		t.Errorf("默认 48 小时窗口不对：%q", def)
	}
	if !strings.Contains(def, "48 小时内结束") {
		t.Errorf("没说清窗口多长：%q", def)
	}

	narrow := run(t, reg, "ending", "6")
	if strings.Contains(narrow, "緊急懸賞") {
		t.Errorf("6 小时窗口把 16 小时后结束的也列进来了：%q", narrow)
	}
	if !strings.Contains(narrow, "暂无") {
		t.Errorf("6 小时内确实没有活动该说暂无：%q", narrow)
	}

	wide := run(t, reg, "ending", "24", "2") // 小时数 + 页码混着写也要认
	if !strings.Contains(wide, "緊急懸賞") {
		t.Errorf("24 小时窗口该列出一条：%q", wide)
	}
}

func TestUpcomingHonorsDaysArg(t *testing.T) {
	reg := start(t, fixture(), calquery.Config{Now: func() time.Time { return now }})

	def := run(t, reg, "upcoming")
	if !strings.Contains(def, "雪融時分新芽綻") {
		t.Errorf("默认 7 天该列出两天后开始的活动：%q", def)
	}
	if strings.Contains(def, "悠悠漫時") {
		t.Errorf("已经开始了的活动混进了「即将」：%q", def)
	}
	if !strings.Contains(def, "还有 2天") {
		t.Errorf("倒计时没说成人话：%q", def)
	}

	if one := run(t, reg, "upcoming", "1"); !strings.Contains(one, "暂无") {
		t.Errorf("1 天内确实没有新活动该说暂无：%q", one)
	}
	// 上限夹住：写 9999 天也不该让日历把上千条一次性吐进一条回复
	if clamped := run(t, reg, "upcoming", "9999"); !strings.Contains(clamped, "60 天内") {
		t.Errorf("天数没被夹到上限：%q", clamped)
	}
}

func TestEmptyCalendarExplainsSyncState(t *testing.T) {
	empty := calendar.NewStore()

	// 没接同步状况依赖：只说暂无，不编理由
	plain := start(t, empty, calquery.Config{Now: func() time.Time { return now }})
	if got := run(t, plain, "events"); !strings.Contains(got, "暂无") || strings.Contains(got, "同步") {
		t.Errorf("没接 Status 依赖时不该提同步：%q", got)
	}

	// 从没同步成功过：说清是"还在同步中"，而不是让人以为现在真没活动
	syncing := start(t, empty, calquery.Config{
		Now:    func() time.Time { return now },
		Status: fakeStatus{st: annsync.Status{Source: "s"}},
	})
	got := run(t, syncing, "events")
	if !strings.Contains(got, "还在同步中") {
		t.Errorf("首轮未完成时该说还在同步中：%q", got)
	}

	// 同步过但源侧在报错：把失败说出来，别装作数据是新鲜的
	failing := start(t, fixture(), calquery.Config{
		Now: func() time.Time { return now },
		Status: fakeStatus{st: annsync.Status{
			Source: "s", LastSuccess: now.Add(-2 * time.Hour), Fails: 3, LastError: "HTTP 429",
		}},
	})
	got = run(t, failing, "events")
	if !strings.Contains(got, "数据截至 09-02 10:00") || !strings.Contains(got, "失败 3 次") || !strings.Contains(got, "HTTP 429") {
		t.Errorf("源侧失败没体现在尾注里：%q", got)
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	reg := start(t, fixture(), calquery.Config{Now: func() time.Time { return now }})
	got := run(t, reg, "help")
	for _, want := range []string{"events", "ending", "upcoming", "help"} {
		if !strings.Contains(got, want) {
			t.Errorf("帮助里缺 %q：%q", want, got)
		}
	}
	if strings.Count(got, "\n") < 4 {
		t.Errorf("帮助不像一份清单：%q", got)
	}
}

func TestCrossYearTimesShowYear(t *testing.T) {
	cal := calendar.NewStore()
	cal.Upsert(calendar.Activity{ID: "s:9:0", Title: "跨年活动", Start: at(9, 1, 0, 0), End: time.Date(2027, 1, 5, 12, 0, 0, 0, zone)})
	reg := start(t, cal, calquery.Config{Now: func() time.Time { return now }})

	got := run(t, reg, "events")
	if !strings.Contains(got, "2027-01-05 12:00") {
		t.Errorf("跨年的时间没带年份，会被读成今年：%q", got)
	}
}

func id(i int) string { return string(rune('a'+i)) + ":1:0" }

func title(i int) string {
	return []string{"甲", "乙", "丙", "丁", "戊", "己", "庚"}[i]
}
