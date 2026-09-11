package calposter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/schedule"
	"xingta/internal/qq"
	"xingta/internal/store"
	"xingta/internal/store/storetest"
)

// ---------- 内核侧假件（与 calexpiry 那份同形） ----------

type fakeAPI struct {
	cal   calendar.View
	sched schedule.Scheduler

	mu        sync.Mutex
	targets   []string
	submitted []queue.Item
	tasks     map[string]func(context.Context) error
	at        map[string]time.Time
}

func newAPI() *fakeAPI {
	return &fakeAPI{
		cal: calendar.NewStore(), sched: schedule.New(),
		tasks: map[string]func(context.Context) error{}, at: map[string]time.Time{},
	}
}

func (a *fakeAPI) Submit(it queue.Item) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.submitted = append(a.submitted, it)
	return nil
}

func (a *fakeAPI) Schedule(id string, at time.Time, fn func(context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tasks[id] = fn
	a.at[id] = at
}

func (a *fakeAPI) Cancel(string) bool            { return false }
func (a *fakeAPI) Calendar() calendar.View       { return a.cal }
func (a *fakeAPI) Scheduler() schedule.Scheduler { return a.sched }
func (a *fakeAPI) Targets() []string             { return a.targets }

// fire 触发并消费掉一个排期，与真调度器一致：跑完就不再排着。
func (a *fakeAPI) fire(id string) error {
	a.mu.Lock()
	fn := a.tasks[id]
	delete(a.tasks, id)
	a.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("没有 %s 这个排期任务", id)
	}
	return fn(context.Background())
}

// armed 是"此刻还排着期吗"。断言这个而不是数 Schedule 被调用几次：Start 会立刻
// 跑一轮预热，与测试里显式调的 round 天然并发，数调用次数必然抖。
func (a *fakeAPI) taskIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for k := range a.tasks {
		out = append(out, k)
	}
	return out
}

func (a *fakeAPI) armed(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.tasks[id]
	return ok
}

func (a *fakeAPI) atOf(id string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.at[id]
	return t, ok
}

func (a *fakeAPI) items() []queue.Item {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]queue.Item(nil), a.submitted...)
}

// ---------- 推图排期 ----------

// recBox 是测试侧的"公告库"：带锁，读的时候发快照。
//
// 为什么不直接传切片：Start 会立刻起一轮后台预热，它和测试主体并发读同一批 Rec，
// 测试一改海报 URL 就是 -race 抓到的那种"data race 出现在产品代码里"——
// 其实错在测试把可变的底层数组递了出去。
type recBox struct {
	mu   sync.Mutex
	list []annsync.Rec
}

func box(recs ...annsync.Rec) *recBox {
	return &recBox{list: append([]annsync.Rec(nil), recs...)}
}

func (b *recBox) recs() ([]annsync.Rec, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]annsync.Rec(nil), b.list...), nil
}

func (b *recBox) setPoster(i int, url string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.list[i].Poster = url
}

// flakyCap 是"先画不出来、修好后能画"的画布。
// 换 Capturer 实例不行：那要改 p.cfg.Cap，而后台循环正在读它。
type flakyCap struct {
	mu  sync.Mutex
	bad bool
	n   int
}

func (c *flakyCap) Capture(page []byte, route string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bad {
		return nil, errors.New("浏览器起不来")
	}
	c.n++
	return []byte("PNG:" + route), nil
}

func (c *flakyCap) fix() {
	c.mu.Lock()
	c.bad = false
	c.mu.Unlock()
}

func (c *flakyCap) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func featureFor(t *testing.T, recs *recBox, cap Capturer, doc store.Doc, now time.Time, targets ...string) (*Poster, *fakeAPI) {
	t.Helper()
	api := newAPI()
	p := New(Config{
		Doc: doc, Records: recs.recs,
		Cap: cap, Label: "version", Zone: zone, Targets: targets,
		Now: func() time.Time { return now },
	})
	if err := p.Start(context.Background(), api); err != nil {
		t.Fatal(err)
	}
	return p, api
}

func TestPushArmedAtOpenMoment(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	// 早上 8 点：新版本还没过开闸，排期应当排在 17:00。
	// 到点：只投一条只带媒体引用的队列项。
	cap := &fakeCap{}
	p, api := featureFor(t, recs, cap, storetest.NewMem(), at("2026-09-08 08:00"), "g:grp")
	p.round(context.Background())

	want := "poster:202609071600"
	gotAt, ok := api.atOf(want)
	if !ok {
		t.Fatalf("没排上期，任务表 %+v", api.tasks)
	}
	if !gotAt.Equal(at("2026-09-08 17:00")) {
		t.Errorf("触发时刻应是开闸估计 17:00，得到 %s", gotAt)
	}
	if !api.armed(want) {
		t.Error("没排上期")
	}

	if err := api.fire(want); err != nil {
		t.Fatal(err)
	}
	// 这条断言钉的是"回调不占调度线程"：调度器同步跑 Fn，在这儿画一趟就把同期
	// 到期的提醒一起拖住了。出图只能发生在发送 worker 上。
	if n := cap.count(); n != 0 {
		t.Errorf("到点触发画了 %d 趟图，投递必须与渲染解耦", n)
	}
	items := api.items()
	if len(items) != 1 {
		t.Fatalf("该投 1 条，投了 %d 条", len(items))
	}
	if items[0].Media == nil || items[0].Media.Kind != "poster" || items[0].Media.Key != "202609071600" {
		t.Errorf("队列项没带对媒体引用: %+v", items[0])
	}
	if items[0].Mergeable {
		t.Error("图不能被并进文字 batch（队列的合并判据要求 Media 为空时才可合并）")
	}

	// 发送侧报"发出去了"之后，再跑几轮都不该投第二条。
	// 这里断言的是"发出去几次"而不是"有没有被重新排上"——排期检查与触发之间
	// 本来就允许重排，只要 push 以账本为准，就不会真发第二遍。
	p.MarkPushed("202609071600")
	for i := 0; i < 3; i++ {
		p.round(context.Background())
		_ = api.fire(want)
	}
	if got := len(api.items()); got != 1 {
		t.Errorf("同一个版本投了 %d 条, want 1", got)
	}
}

func TestHistoryIsNotPushedOnFirstRun(t *testing.T) {
	// 库里三个版本都已开闸。功能第一次上线时只有"刚过开闸不到宽限期"的那个能排上，
	// 否则一上线就把历史上每个版本都推一遍，是刷屏。
	recs := box(
		verRec("3994", "晴风碧海", "2026-07-21 00:00", "2026-08-04 03:59", "2026-08-11 10:59"),
		verRec("4356", "欢歌劲浪", "2026-08-18 00:00", "2026-09-01 03:59", "2026-09-08 10:59"),
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
	)
	p, api := featureFor(t, recs, &fakeCap{}, storetest.NewMem(), at("2026-09-08 17:30"), "g:grp")
	p.round(context.Background())

	if !api.armed("poster:202609071600") {
		t.Error("刚开闸的那个版本没排上期")
	}
	for _, old := range []string{"poster:202607201600", "poster:202608171600"} {
		if api.armed(old) {
			t.Errorf("历史版本 %s 被排上了期，上线即刷屏", old)
		}
	}
}

func TestRestartDoesNotResend(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	doc := storetest.NewMem()
	now := at("2026-09-08 17:05")
	p, api := featureFor(t, recs, &fakeCap{}, doc, now, "g:grp")
	p.round(context.Background())
	if err := api.fire("poster:202609071600"); err != nil {
		t.Fatal(err)
	}
	if len(api.items()) != 1 {
		t.Fatalf("第一次该投 1 条，%d 条", len(api.items()))
	}
	p.MarkPushed("202609071600") // 模拟发送 worker 真把它发出去了

	// 换一个 Poster 实例（= 重启），账本还在库里。
	p2, api2 := featureFor(t, recs, &fakeCap{}, doc, at("2026-09-08 17:06"), "g:grp")
	p2.round(context.Background())
	if api2.armed("poster:202609071600") {
		t.Error("重启后又排上期了，会重发")
	}
	if len(api2.items()) != 0 {
		t.Errorf("重启后立刻又投了 %d 条", len(api2.items()))
	}
}

// 画不出来 / 发不出去都不能被记成"已推"，否则这一版永远不会补发。
// 投递与渲染解耦之后，失败点从"到点那一刻"挪到了"worker 要图那一刻"。
func TestBrokenImageIsNotMarkedSent(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	doc := storetest.NewMem()
	bad := &flakyCap{bad: true}
	p, api := featureFor(t, recs, bad, doc, at("2026-09-08 17:05"), "g:grp")
	p.round(context.Background())

	if err := api.fire("poster:202609071600"); err != nil {
		t.Fatalf("投递本身不该失败（渲染不在这一趟）: %v", err)
	}
	if len(api.items()) != 1 {
		t.Fatalf("该投 1 条，%d 条", len(api.items()))
	}
	// worker 去要图：画不出来 → 不调 MarkPushed → 账本必须还是空的。
	if _, err := p.Fetch(context.Background(), "202609071600"); err == nil {
		t.Fatal("画不出来必须往上报错，否则就当发过了")
	}
	var raw []byte
	if ok, _ := doc.Get(NS, "202609071600", &raw); ok {
		t.Error("没发出去的一轮被记成已推，之后永远不会补发")
	}

	// 下一轮（浏览器修好了）重新排上、再投，发出去之后记账，此后不再重投。
	bad.fix()
	p.round(context.Background())
	if err := api.fire("poster:202609071600"); err != nil {
		t.Fatalf("修好后重投失败: %v", err)
	}
	if got := len(api.items()); got != 2 {
		t.Fatalf("修好后该再投 1 条（累计 2 条），实际 %d 条", got)
	}
	if _, err := p.Fetch(context.Background(), "202609071600"); err != nil {
		t.Fatalf("修好后要图还失败: %v", err)
	}
	p.MarkPushed("202609071600")

	p.round(context.Background())
	_ = api.fire("poster:202609071600")
	if got := len(api.items()); got != 2 {
		t.Errorf("记账之后又投了，累计 %d 条, want 停在 2", got)
	}
}

func TestNoTargetsLogsOnly(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	p, api := featureFor(t, recs, &fakeCap{}, storetest.NewMem(), at("2026-09-08 17:05"))
	p.round(context.Background())
	if err := api.fire("poster:202609071600"); err != nil {
		t.Fatal(err)
	}
	if len(api.items()) != 0 {
		t.Errorf("没配目标却投了 %d 条", len(api.items()))
	}
	var v time.Time
	if ok, _ := p.cfg.Doc.Get(NS, "202609071600", &v); !ok {
		t.Error("只记日志的一轮也要落账，否则每轮都重排")
	}
}

// ---------- 预热 ----------

func TestRoundFetchesArtOncePerFingerprint(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte(strings.Repeat("x", 600))) // 够大即可，解不出图只会退回哈希底色
	}))
	defer cdn.Close()

	recs := box(
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		annsync.Rec{ID: "stellasora:4565:0", RefID: "4565", Title: "有海报的活动", Label: "活动时间",
			Start: at("2026-09-09 00:00"), End: at("2026-09-12 03:59"),
			Status: annsync.StatusOK, Provenance: "solo", Poster: cdn.URL + "/a.jpg"},
	)
	// Client 必须在 Start 之前配好：Start 立刻跑的那一轮会读它，事后改 p.cfg.Client
	// 既是竞态，又会让那一轮按"不要海报"的口径把指纹变化消费掉。
	p := New(Config{
		Doc: storetest.NewMem(), Records: recs.recs,
		Cap: &fakeCap{}, Label: "version", Zone: zone, Client: cdn.Client(),
		Now: func() time.Time { return at("2026-09-08 20:00") },
	})
	if err := p.Start(context.Background(), newAPI()); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		p.round(context.Background())
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 1 {
		t.Errorf("数据没变时海报只该拉一次，拉了 %d 次", got)
	}

	// 数据一变（海报 URL 换了）就该重拉。
	recs.setPoster(1, cdn.URL+"/b.jpg")
	p.round(context.Background())
	mu.Lock()
	got = hits
	mu.Unlock()
	if got != 2 {
		t.Errorf("数据变了没重拉，共 %d 次", got)
	}
}

// 账本值的容错：库里那条解不开时按"已推"处理（与 calexpiry 同一条取舍）。
func TestUnreadableLedgerCountsAsSent(t *testing.T) {
	doc := storetest.NewMem()
	if err := doc.Put(NS, "202609071600", "不是时间"); err != nil {
		t.Fatal(err)
	}
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	p, api := featureFor(t, recs, &fakeCap{}, doc, at("2026-09-08 17:05"), "g:grp")
	p.round(context.Background())
	if api.armed("poster:202609071600") {
		t.Error("解不开的账本该按已推处理，却还是排上了期")
	}
}

// ---------- 「日历」命令 ----------

func TestCalendarCommandRepliesWithImage(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	cap := &fakeCap{}
	api := newAPI()
	reg := command.NewRegistry()
	p := New(Config{
		Doc: storetest.NewMem(), Records: recs.recs,
		Cap: cap, Label: "version", Zone: zone, Reg: reg, Now: func() time.Time { return at("2026-09-08 20:00") },
	})
	if err := p.Start(context.Background(), api); err != nil {
		t.Fatal(err)
	}
	c, ok := reg.Lookup("calendar")
	if !ok {
		t.Fatal("「calendar」没注册上")
	}
	rep, err := c.Run(context.Background(), &qq.Message{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(rep.Image) != "PNG:#x202609071600" {
		t.Errorf("回的不是当前版本那张图: %q", rep.Image)
	}
	if rep.Text != "" {
		t.Errorf("回图时不该带文字: %q", rep.Text)
	}
	if cap.count() != 1 {
		t.Errorf("画了 %d 次", cap.count())
	}
}

// 没给 Reg 就只推图不接命令：关掉命令面不该连带关掉推图。
func TestNoRegistrarMeansNoCommand(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	p, api := featureFor(t, recs, &fakeCap{}, storetest.NewMem(), at("2026-09-08 17:05"), "g:grp")
	p.round(context.Background())
	if err := api.fire("poster:202609071600"); err != nil {
		t.Fatalf("没注册命令时推图照常要能跑: %v", err)
	}
}

// ---------- 抓取只发生在后台 ----------

// 海报 CDN 派给我们的边缘实测要么 ~15KB/s、要么一发 TLS 握手就被断。所以"用户点
// 「日历」"那一次一个网络请求都不能发：这条测试就是钉这个的。
func TestImageCommandPathNeverHitsNetwork(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(bigArt())
	}))
	defer cdn.Close()

	recs := artRecs(cdn.URL)
	cap := &fakeCap{}
	p := New(Config{
		Doc: storetest.NewMem(), Records: recs.recs,
		Cap: cap, Label: "version", Zone: zone, Client: cdn.Client(),
		Now: func() time.Time { return at("2026-09-08 20:00") },
	})
	if _, err := p.Image(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("命令路径发了 %d 个网络请求，海报只能由后台轮去抓", n)
	}
	if cap.count() != 1 {
		t.Error("没出图：缺海报不该让出图失败")
	}
	if cap.inlined() {
		t.Error("本地没有字节，页面里却出现了内联海报")
	}
}

// 预热轮抓到之后，命令拿到的那张就该带图了。
func TestWarmedPosterShowsUpInCommandImage(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(bigArt())
	}))
	defer cdn.Close()

	recs := artRecs(cdn.URL)
	cap := &fakeCap{}
	p := New(Config{
		Doc: storetest.NewMem(), Records: recs.recs,
		Cap: cap, Label: "version", Zone: zone, Client: cdn.Client(),
		Now: func() time.Time { return at("2026-09-08 20:00") },
	})
	p.round(context.Background()) // 后台那一轮允许抓
	if _, err := p.Image(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !cap.inlined() {
		t.Error("预热抓到了，命令出的图里却没有内联海报")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("海报一共拉了 %d 次, want 只有预热那一次", n)
	}
}

// 取不到不能变成"永远不再试"，也不能"每轮都去捶"：中间那条线就是 RetryAfter。
func TestFailedPosterRetriesOnlyAfterCooldown(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer cdn.Close()

	recs := artRecs(cdn.URL)
	now := at("2026-09-08 20:00")
	p := New(Config{
		Doc: storetest.NewMem(), Records: recs.recs,
		Cap: &fakeCap{}, Label: "version", Zone: zone, Client: cdn.Client(),
		Now: func() time.Time { return now },
	})
	for i := 0; i < 3; i++ {
		p.round(context.Background())
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("冷却期内重试了 %d 次, want 1 次", n)
	}

	now = now.Add(11 * time.Minute) // 默认冷却 10 分钟
	p.round(context.Background())
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("过了冷却没重拉，共 %d 次", n)
	}
}

// 预热轮和到点推图可能同时缺同一张图：那是一次抓取，不是两次。
func TestConcurrentWarmersFetchEachPosterOnce(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(120 * time.Millisecond) // 留出重叠的窗口
		w.Write(bigArt())
	}))
	defer cdn.Close()

	recs := artRecs(cdn.URL)
	p := New(Config{
		Doc: storetest.NewMem(), Records: recs.recs,
		Cap: &fakeCap{}, Label: "version", Zone: zone, Client: cdn.Client(),
		Now: func() time.Time { return at("2026-09-08 20:00") },
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.round(context.Background()) }()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("四个预热轮拉了 %d 次同一张海报, want 1 次", n)
	}
}

func artRecs(cdnURL string) *recBox {
	return box(
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		annsync.Rec{ID: "stellasora:4565:0", RefID: "4565", Title: "有海报的活动", Label: "活动时间",
			Start: at("2026-09-09 00:00"), End: at("2026-09-12 03:59"),
			Status: annsync.StatusOK, Provenance: "solo", Poster: cdnURL + "/a.jpg"},
	)
}

// bigArt 给一段够长但解不出的字节：海报只要过 512 字节的门槛就算"拿到了"，
// 底色会退回哈希值——这里不验取色，验的是有没有把字节带进页面。
func bigArt() []byte { return []byte(strings.Repeat("x", 600)) }

// 海报落盘是为了"重启不重新捶 CDN"（实测反复整窗拉取之后会被直接掐连接）。
// 所以这里断言的是第二个进程一次网络都不该碰。
func TestArtCacheSurvivesRestart(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte(strings.Repeat("y", 600)))
	}))
	defer cdn.Close()

	recs := box(
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		annsync.Rec{ID: "stellasora:4565:0", RefID: "4565", Title: "有海报的活动", Label: "活动时间",
			Start: at("2026-09-09 00:00"), End: at("2026-09-12 03:59"),
			Status: annsync.StatusOK, Provenance: "solo", Poster: cdn.URL + "/a.jpg"},
	)
	dir := t.TempDir()
	for round := 0; round < 2; round++ {
		// 每轮换一个新 Poster 实例 = 重启：内存清空，只剩盘上那份。
		// ctx 每轮都停掉：真重启时旧进程死透，这里不能留着一个旧 loop 的
		// 预热轮在后台和"新实例"抢同一块缓存目录。
		ctx, stop := context.WithCancel(context.Background())
		p := New(Config{
			Doc: storetest.NewMem(), Records: recs.recs,
			Cap: &fakeCap{}, Label: "version", Zone: zone, ArtDir: dir,
			Client: cdn.Client(), Now: func() time.Time { return at("2026-09-08 20:00") },
		})
		if err := p.Start(ctx, newAPI()); err != nil {
			t.Fatal(err)
		}
		p.round(ctx)
		stop()
		mu.Lock()
		got := hits
		mu.Unlock()
		if got != 1 {
			t.Fatalf("第 %d 次启动拉了 %d 次, want 只有第一次拉", round+1, got)
		}
	}
}

func TestPosterUsesAPITargets(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	cap := &fakeCap{}
	// Config.Targets 为空
	p, api := featureFor(t, recs, cap, storetest.NewMem(), at("2026-09-08 08:00"))
	// 但 api.targets 设置了动态目标
	api.targets = []string{"g:DYNAMIC_POSTER_GROUP"}

	p.round(context.Background())

	want := "poster:202609071600"
	if err := api.fire(want); err != nil {
		t.Fatal(err)
	}

	items := api.items()
	if len(items) != 1 {
		t.Fatalf("期望投递 1 条，实际投递 %d 条", len(items))
	}
	if items[0].Target != "g:DYNAMIC_POSTER_GROUP" {
		t.Errorf("投递目标 = %q，期望 g:DYNAMIC_POSTER_GROUP", items[0].Target)
	}
}
