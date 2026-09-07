package calexpiry_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"xingta/internal/feature/calexpiry"
	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/schedule"
	"xingta/internal/store/storetest"
)

// 这些用例用真的 calendar.Store 当查询面（EndingWithin 的窗口语义是真的），
// 只有 kernel.API 是假件——这样才能断言"排了几次期、投了几条、撤了哪个排期"。
// 排期任务由测试手动触发，不然就得跟真时钟赛跑。

var (
	zone = time.FixedZone("CST", 8*60*60)
	now  = time.Date(2026, 9, 2, 12, 0, 0, 0, zone)
	lead = 48 * time.Hour
)

const (
	soonID = "stellasora:4432:0" // 窗口内：20 小时后结束
	edgeID = "stellasora:4435:3" // 窗口边缘：47 小时后结束
	farID  = "stellasora:9:0"    // 窗口外：30 天后结束
	nextID = "stellasora:10:0"   // 还没开始
)

func taskID(activityID string) string { return "expiry:" + activityID }

func fixtures() []calendar.Activity {
	return []calendar.Activity{
		{ID: soonID, Game: "stellasora", Title: "悠悠漫時，愜意夕暮",
			Start: now.Add(-24 * time.Hour), End: now.Add(20 * time.Hour),
			URL: "https://example.test/news/4432"},
		{ID: edgeID, Game: "stellasora", Title: "限時招募「花火」",
			Start: now.Add(-time.Hour), End: now.Add(47 * time.Hour)},
		{ID: farID, Game: "stellasora", Title: "還早得很",
			Start: now.Add(-time.Hour), End: now.Add(30 * 24 * time.Hour)},
		{ID: nextID, Game: "stellasora", Title: "還沒開始",
			Start: now.Add(48 * time.Hour), End: now.Add(72 * time.Hour)},
	}
}

// ---- 假 API ----------------------------------------------------------------

type fakeTask struct {
	at time.Time
	fn func(context.Context) error
}

type fakeAPI struct {
	cal   calendar.View
	sched schedule.Scheduler

	mu        sync.Mutex
	submitted []queue.Item
	submitErr error
	tasks     map[string]fakeTask
	schedules map[string]int // 任务 ID → Schedule 被调用几次
	cancelled []string
}

func newAPI(cal calendar.View) *fakeAPI {
	return &fakeAPI{
		cal: cal, sched: schedule.New(),
		tasks: map[string]fakeTask{}, schedules: map[string]int{},
	}
}

func (a *fakeAPI) Submit(it queue.Item) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.submitErr != nil {
		return a.submitErr
	}
	a.submitted = append(a.submitted, it)
	return nil
}

func (a *fakeAPI) Schedule(id string, at time.Time, fn func(context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tasks[id] = fakeTask{at: at, fn: fn}
	a.schedules[id]++
}

func (a *fakeAPI) Cancel(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.tasks[id]
	delete(a.tasks, id)
	a.cancelled = append(a.cancelled, id)
	return ok
}

func (a *fakeAPI) Calendar() calendar.View       { return a.cal }
func (a *fakeAPI) Scheduler() schedule.Scheduler { return a.sched }

// fire 手动触发一个排期任务，模拟调度器到点。
func (a *fakeAPI) fire(id string) error {
	a.mu.Lock()
	t, ok := a.tasks[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("没有 %s 这个排期任务", id)
	}
	return t.fn(context.Background())
}

func (a *fakeAPI) atOf(id string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tasks[id]
	return t.at, ok
}

func (a *fakeAPI) scheduleCount(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.schedules[id]
}

func (a *fakeAPI) items() []queue.Item {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]queue.Item(nil), a.submitted...)
}

func (a *fakeAPI) wasCancelled(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.cancelled {
		if c == id {
			return true
		}
	}
	return false
}

func (a *fakeAPI) failSubmits(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.submitErr = err
}

// ---- 装置 ------------------------------------------------------------------

type logSink struct {
	mu   sync.Mutex
	line []string
}

func (s *logSink) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.line = append(s.line, fmt.Sprintf(format, args...))
}

func (s *logSink) has(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.line {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (s *logSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.line {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

type rig struct {
	doc  *storetest.MemDoc
	cal  *calendar.Store
	logs *logSink
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{doc: storetest.NewMem(), cal: calendar.NewStore(), logs: &logSink{}}
	r.cal.BulkUpsert(fixtures())
	return r
}

// start 起一个功能实例。Every 压到 5ms，测试靠轮询等效果，不用睡固定时长。
func (r *rig) start(t *testing.T, api *fakeAPI, targets ...string) {
	t.Helper()
	f := calexpiry.New(r.doc, calexpiry.Config{
		Lead: lead, Every: 5 * time.Millisecond, Targets: targets,
		Zone: zone, Now: func() time.Time { return now }, Logf: r.logs.logf,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // 扫描循环随测试停，别漏 goroutine 去写别的用例的假件
	if err := f.Start(ctx, api); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("超时：%s", what)
}

// settle 让扫描循环再跑一会儿，用于断言"没有发生什么"。
func settle(d time.Duration) { time.Sleep(d) }

func (r *rig) sentAt(t *testing.T, activityID string) (time.Time, bool) {
	t.Helper()
	var at time.Time
	ok, err := r.doc.Get(calexpiry.NS, activityID, &at)
	if err != nil {
		t.Fatalf("读已提醒记录: %v", err)
	}
	return at, ok
}

// ---- 用例 ------------------------------------------------------------------

func TestSchedulesOnlyActivitiesInsideLeadWindow(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	r.start(t, api, "g:GROUP1")

	waitFor(t, "两个窗口内的活动都该排上期", func() bool {
		_, a := api.atOf(taskID(soonID))
		_, b := api.atOf(taskID(edgeID))
		return a && b
	})
	settle(50 * time.Millisecond)

	if at, _ := api.atOf(taskID(soonID)); !at.Equal(now.Add(20*time.Hour - lead)) {
		t.Errorf("提醒时刻 = %v，want 结束前 %v（%v）", at, lead, now.Add(20*time.Hour-lead))
	}
	if at, _ := api.atOf(taskID(edgeID)); !at.Equal(now.Add(47*time.Hour - lead)) {
		t.Errorf("边缘活动提醒时刻 = %v，want %v", at, now.Add(47*time.Hour-lead))
	}
	for _, id := range []string{farID, nextID} {
		if _, ok := api.atOf(taskID(id)); ok {
			t.Errorf("%s 不该被排期：它不在 %v 的提醒窗口内", id, lead)
		}
	}
	if got := api.items(); len(got) != 0 {
		t.Errorf("还没到点就投递了 %d 条：%v", len(got), got)
	}
}

func TestRemindSendsOneItemPerTargetWithReadableText(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	r.start(t, api, "g:GROUP1", "u:USER9")

	waitFor(t, "排期出现", func() bool { _, ok := api.atOf(taskID(soonID)); return ok })
	if err := api.fire(taskID(soonID)); err != nil {
		t.Fatalf("触发提醒: %v", err)
	}

	items := api.items()
	if len(items) != 2 {
		t.Fatalf("投了 %d 条，want 每个目标一条：%v", len(items), items)
	}
	for i, want := range []string{"g:GROUP1", "u:USER9"} {
		if items[i].Target != want {
			t.Errorf("第 %d 条 Target = %q, want %q", i, items[i].Target, want)
		}
		if items[i].ID != "expiry:"+soonID+"@"+want {
			t.Errorf("第 %d 条 ID = %q：两个目标必须不同 ID，否则队列里分不清", i, items[i].ID)
		}
		if !items[i].Mergeable || items[i].Topic != "expiry" {
			t.Errorf("第 %d 条没标成可合并（Topic=%q Mergeable=%v）：同时到期的活动该合成一条",
				i, items[i].Topic, items[i].Mergeable)
		}
	}
	for _, frag := range []string{"悠悠漫時，愜意夕暮", "20小时", "09-03 08:00", "https://example.test/news/4432"} {
		if !strings.Contains(items[0].Text, frag) {
			t.Errorf("提醒文本里没有 %q：%q", frag, items[0].Text)
		}
	}

	at, ok := r.sentAt(t, soonID)
	if !ok || !at.Equal(now) {
		t.Errorf("已提醒记录 = %v, %v；want 落盘且时刻为 %v", at, ok, now)
	}
}

func TestSameActivityRemindedOnlyOnce(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	r.start(t, api, "g:GROUP1")

	waitFor(t, "排期出现", func() bool { _, ok := api.atOf(taskID(soonID)); return ok })
	if err := api.fire(taskID(soonID)); err != nil {
		t.Fatalf("触发提醒: %v", err)
	}

	settle(60 * time.Millisecond) // 让提醒落盘，之后的扫描都该跳过它
	schedules, submitted := api.scheduleCount(taskID(soonID)), len(api.items())
	settle(120 * time.Millisecond) // 又跑了 ~24 轮扫描

	if got := api.scheduleCount(taskID(soonID)); got != schedules {
		t.Errorf("提醒过之后还在重复排期：%d → %d 次", schedules, got)
	}
	if got := len(api.items()); got != submitted {
		t.Errorf("提醒过之后又投递了：%d → %d 条", submitted, got)
	}
	if submitted != 1 {
		t.Errorf("投了 %d 条，want 1 条", submitted)
	}
}

func TestReschedulesWhenEndMoves(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	r.start(t, api, "g:GROUP1")

	waitFor(t, "排期出现", func() bool { _, ok := api.atOf(taskID(soonID)); return ok })

	moved := now.Add(6 * time.Hour)
	r.cal.Upsert(calendar.Activity{ID: soonID, Game: "stellasora", Title: "悠悠漫時，愜意夕暮",
		Start: now.Add(-24 * time.Hour), End: moved, URL: "https://example.test/news/4432"})

	want := moved.Add(-lead)
	waitFor(t, "改期后重排", func() bool { at, ok := api.atOf(taskID(soonID)); return ok && at.Equal(want) })
	if n := api.scheduleCount(taskID(soonID)); n < 2 {
		t.Errorf("Schedule 只被调用 %d 次：改期没有重排", n)
	}
}

func TestCancelsScheduleWhenActivityLeavesCalendar(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	r.start(t, api, "g:GROUP1")

	waitFor(t, "排期出现", func() bool { _, ok := api.atOf(taskID(soonID)); return ok })
	r.cal.Remove(soonID) // 被同步删掉 / 被运维隱藏

	waitFor(t, "撤销排期", func() bool { return api.wasCancelled(taskID(soonID)) })
	if err := api.fire(taskID(soonID)); err == nil {
		t.Error("撤销之后任务还能触发")
	}
	settle(50 * time.Millisecond)
	if got := api.items(); len(got) != 0 {
		t.Errorf("活动都没了还投递 %d 条：%v", len(got), got)
	}
}

func TestRestartDoesNotRemindAgain(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	r.start(t, api, "g:GROUP1")

	waitFor(t, "排期出现", func() bool { _, ok := api.atOf(taskID(soonID)); return ok })
	if err := api.fire(taskID(soonID)); err != nil {
		t.Fatalf("触发提醒: %v", err)
	}
	if _, ok := r.sentAt(t, soonID); !ok {
		t.Fatal("已提醒记录没落盘，重启必然重发")
	}

	// 重启：同一份存储、同一个日历（活动还在窗口里），换一套假 API
	api2 := newAPI(r.cal)
	r.start(t, api2, "g:GROUP1")
	waitFor(t, "重启后至少扫过一轮", func() bool { return api2.scheduleCount(taskID(edgeID)) > 0 })
	settle(80 * time.Millisecond)

	if n := api2.scheduleCount(taskID(soonID)); n != 0 {
		t.Errorf("重启后把已提醒的 %s 又排了 %d 次期", soonID, n)
	}
	for _, it := range api2.items() {
		if strings.Contains(it.ID, soonID) {
			t.Errorf("重启后重发了提醒：%v", it)
		}
	}
}

func TestNoTargetsLogsInsteadOfSending(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	r.start(t, api) // Targets 为空

	waitFor(t, "排期出现", func() bool { _, ok := api.atOf(taskID(soonID)); return ok })
	if err := api.fire(taskID(soonID)); err != nil {
		t.Fatalf("触发提醒: %v", err)
	}
	if !r.logs.has("未配置提醒目标") {
		t.Error("没配目标时连日志都没有，这条提醒就彻底静默了")
	}
	settle(80 * time.Millisecond)

	if got := api.items(); len(got) != 0 {
		t.Errorf("没配目标却投递了 %d 条：%v", len(got), got)
	}
	if _, ok := r.sentAt(t, soonID); !ok {
		t.Error("只记日志也要落已提醒记录，否则每轮扫描都刷一条日志")
	}
	if n := r.logs.count("悠悠漫時"); n != 1 {
		t.Errorf("同一个活动记了 %d 条日志，want 1 条（每轮扫描都刷一条就吵死了）", n)
	}
}

func TestSubmitFailureIsRetriedByNextScan(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	api.failSubmits(errors.New("队列满了"))
	r.start(t, api, "g:GROUP1", "u:USER9")

	waitFor(t, "排期出现", func() bool { _, ok := api.atOf(taskID(soonID)); return ok })
	if err := api.fire(taskID(soonID)); err == nil {
		t.Fatal("投递失败却没报错：调度器就无从知晓，也不会重试")
	}
	if _, ok := r.sentAt(t, soonID); ok {
		t.Error("投递失败却记成已提醒，这条提醒就永久丢了")
	}

	api.failSubmits(nil)
	waitFor(t, "下一轮重新排期", func() bool { return api.scheduleCount(taskID(soonID)) >= 2 })
	if err := api.fire(taskID(soonID)); err != nil {
		t.Fatalf("重投: %v", err)
	}
	if got := len(api.items()); got != 2 {
		t.Errorf("重投后投了 %d 条，want 每个目标一条", got)
	}
	if _, ok := r.sentAt(t, soonID); !ok {
		t.Error("重投成功后没落已提醒记录")
	}
}

func TestCorruptSentRecordIsTreatedAsSent(t *testing.T) {
	r := newRig(t)
	// 功能改了记录结构体字段之后，旧数据就会解不开——这时该按"已提醒"处理，
	// 否则每次重启都对着整个群重发一遍历史提醒。
	if err := r.doc.Put(calexpiry.NS, soonID, "不是时间"); err != nil {
		t.Fatalf("写坏记录: %v", err)
	}
	api := newAPI(r.cal)
	r.start(t, api, "g:GROUP1")

	waitFor(t, "别的应用照常排期", func() bool { _, ok := api.atOf(taskID(edgeID)); return ok })
	settle(50 * time.Millisecond)

	if n := api.scheduleCount(taskID(soonID)); n != 0 {
		t.Errorf("坏记录被当成没提醒过，又排了 %d 次期", n)
	}
	if !r.logs.has("解不开") {
		t.Error("坏记录没有留下日志，线上查不到为什么少了一条提醒")
	}
}

func TestStartRejectsMissingDependencies(t *testing.T) {
	r := newRig(t)
	api := newAPI(r.cal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := calexpiry.New(nil, calexpiry.Config{}).Start(ctx, api); err == nil {
		t.Error("没给 doc 也能启动：那就没人记得提醒过谁，重启必重发")
	}
	if err := calexpiry.New(r.doc, calexpiry.Config{}).Start(ctx, nil); err == nil {
		t.Error("没给 kernel.API 也能启动：排期与投递都无从下手")
	}
}
