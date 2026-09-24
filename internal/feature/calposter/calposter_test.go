package calposter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xingta/internal/annsync"
)

var zone = time.FixedZone("CST", 8*3600)

func at(s string) time.Time {
	t, err := time.ParseInLocation(TimeLayout, s, zone)
	if err != nil {
		panic(err)
	}
	return t
}

// verRec 造一条"版本窗口"记录：Start=玩法起，End=玩法止，ClaimEnd=兑换止。
func verRec(ref, name, start, end, redeem string) annsync.Rec {
	return annsync.Rec{
		ID: "n" + ref + "-0", RefID: ref, Title: name, Label: "活动时间",
		Start: at(start), End: at(end), ClaimEnd: at(redeem),
		Status: annsync.StatusOK, Provenance: "version",
	}
}

func actRec(id, name, start, end string) annsync.Rec {
	return annsync.Rec{
		ID: id, RefID: strings.SplitN(id, "-", 2)[0], Title: name, Label: "活动时间",
		Start: at(start), End: at(end), Status: annsync.StatusOK, Provenance: "solo",
	}
}

func TestWindowsKeysAndOrder(t *testing.T) {
	recs := []annsync.Rec{
		verRec("4356", "欢歌劲浪", "2026-08-18 00:00", "2026-09-01 03:59", "2026-09-08 10:59"),
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		actRec("4100-0", "无关活动", "2026-09-08 00:00", "2026-09-09 00:00"),
	}
	ws := Windows(recs, "version", zone)
	if len(ws) != 2 {
		t.Fatalf("应该只有两条版本窗口，拿到 %d 条: %+v", len(ws), ws)
	}
	// 键是 ActStart 的 UTC yyyyMMddHHmm，不是排序下标：新版本插进来不会挤左。
	if ws[0].Key != "202608171600" || ws[1].Key != "202609071600" {
		t.Errorf("版本键不对: %s %s", ws[0].Key, ws[1].Key)
	}
	if ws[0].Start != "2026-08-18 00:00" {
		t.Errorf("窗口时间要按调用方时区落: %s", ws[0].Start)
	}
}

func TestCurrentPicksLatestOpened(t *testing.T) {
	recs := []annsync.Rec{
		verRec("4356", "欢歌劲浪", "2026-08-18 00:00", "2026-09-01 03:59", "2026-09-08 10:59"),
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
	}
	// 交接日：09-08 08:00 两窗都含今天，但新版本还没过开闸（17:00）——只能算旧的。
	r, ok := Current(recs, "version", at("2026-09-08 08:00"), 17*time.Hour)
	if !ok || VersionKey(r.Start) != "202608171600" {
		t.Errorf("开闸前应停在旧版本，得到 %s (ok=%v)", VersionKey(r.Start), ok)
	}
	// 17:00 一过就换新的。
	r, _ = Current(recs, "version", at("2026-09-08 17:00"), 17*time.Hour)
	if VersionKey(r.Start) != "202609071600" {
		t.Errorf("开闸后应切到新版本，得到 %s", VersionKey(r.Start))
	}
	// 一个都没开：不猜。
	if _, ok := Current(recs, "version", at("2026-08-01 00:00"), 17*time.Hour); ok {
		t.Error("全都没开闸时不该返回一个当前版本")
	}
}

func TestBuildTakesOnlyThisWindow(t *testing.T) {
	recs := []annsync.Rec{
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		actRec("4400-0", "上版本的兑换尾", "2026-09-01 00:00", "2026-09-05 00:00"),
		actRec("4540-1", "本窗活动", "2026-09-09 00:00", "2026-09-12 03:59"),
		actRec("4540-2", "维护后才知起点", "2026-09-14 00:00", "2026-09-16 03:59"),
		actRec("4900-0", "画框之外的", "2026-10-20 00:00", "2026-10-27 03:59"),
	}
	recs[3].Status = annsync.StatusFuzzyStart
	d, err := Build(context.Background(), recs, Options{
		Zone: zone, OpenAt: 17 * time.Hour, Label: "version",
		Key: "202609071600", Now: at("2026-09-08 20:00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range d.Records {
		got = append(got, r.ID)
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "4540-1") || !strings.Contains(joined, "4540-2") {
		t.Errorf("本窗活动漏了: %s", joined)
	}
	if strings.Contains(joined, "4900-0") {
		t.Errorf("下版本的活动被带进来了: %s", joined)
	}
	// 兑换尾在窗口起点之前的"上版本收档"不进本窗（模板也会丢，这里是不白带给浏览器）。
	if strings.Contains(joined, "4400-0") {
		t.Errorf("上版本已收档的活动被带进来了: %s", joined)
	}
	if len(d.Windows) != 1 || d.Windows[0].Key != "202609071600" {
		t.Errorf("只该画选中的那一个版本: %+v", d.Windows)
	}
	if d.OpenMs != int64(17*time.Hour/time.Millisecond) {
		t.Errorf("开闸偏移没下发: %d", d.OpenMs)
	}
	if d.Now != "2026-09-08 20:00" {
		t.Errorf("出图时刻不对: %s", d.Now)
	}
	var fuzzy Record
	for _, r := range d.Records {
		if r.ID == "4540-2" {
			fuzzy = r
		}
	}
	if fuzzy.StartKind != "fuzzy" {
		t.Errorf("fuzzy 起点没标出来: %+v", fuzzy)
	}
}

// 常驻玩法判据的四个形状，档期取自真库（2026-09-22 那份 xingta-live.db）。
func TestDetectPermanent(t *testing.T) {
	recs := []annsync.Rec{
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		// 联合讨伐：月度整档，相邻两期之间恒空一整天（上期 03:59 收、下期隔一天 04:00 开）。
		actRec("a-0", "联合讨伐", "2026-08-01 04:00", "2026-08-31 03:59"),
		actRec("a-1", "联合讨伐", "2026-09-01 04:00", "2026-09-29 22:59"),
		// 灾变防线：每期压维护日开，与上一期重叠 11 小时 —— 并集要把它算一次而不是两次。
		actRec("b-0", "灾变防线", "2026-08-18 00:00", "2026-09-08 10:59"),
		actRec("b-1", "灾变防线", "2026-09-08 00:00", "2026-09-29 10:59"),
		// 猎影合围Beta：两期严丝合缝（上期 03:59 收、下期 04:00 开），占空比 100%。
		actRec("h-0", "猎影合围Beta", "2026-07-21 00:00", "2026-09-01 03:59"),
		actRec("h-1", "猎影合围Beta", "2026-09-01 04:00", "2026-10-01 03:59"),
		// 紧急悬赏：7 天期，两期之间空三周 → 占空比 40%，反复开但不是常驻玩法。
		actRec("c-0", "紧急悬赏", "2026-07-14 04:00", "2026-07-21 03:59"),
		actRec("c-1", "紧急悬赏", "2026-08-11 04:00", "2026-08-18 03:59"),
		// 午后微光共入翠梦：隔八个半月复刻一次，两期都不短，仍不是常驻。
		actRec("r-0", "午后微光共入翠梦", "2025-12-09 04:00", "2025-12-30 03:59"),
		actRec("r-1", "午后微光共入翠梦", "2026-08-04 04:00", "2026-08-25 03:59"),
		// 启明测试：同一档期被三篇公告各声明一次（真库里就是这么三条重复行），
		// 按行数是"3 期"，按档期只有 1 期。
		actRec("t-0", "启明测试", "2025-01-09 11:00", "2025-01-13 11:00"),
		actRec("t-1", "启明测试", "2025-01-09 11:00", "2025-01-13 11:00"),
		actRec("t-2", "启明测试", "2025-01-09 11:00", "2025-01-13 11:00"),
	}
	// 灾变防线真库 15 期全是 fuzzy_start（公告写「维护结束后」，库里存的是当天 00:00
	// 下界），判据得按 openAt 把它推到 17:00 —— 与出图那条带同一把尺子。
	recs[3].Status = annsync.StatusFuzzyStart
	recs[4].Status = annsync.StatusFuzzyStart

	got := detectPermanent(recs, 17*time.Hour)
	if want := "灾变防线,猎影合围Beta,联合讨伐"; strings.Join(got, ",") != want {
		t.Errorf("常驻判据不对：%v，want %s", got, want)
	}
}

func TestBuildRejectsUnknownKey(t *testing.T) {
	recs := []annsync.Rec{verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59")}
	_, err := Build(context.Background(), recs, Options{
		Zone: zone, Label: "version", Key: "202601010000", Now: at("2026-09-08 20:00"),
	})
	if err == nil || !strings.Contains(err.Error(), "202601010000") {
		t.Errorf("未知版本键要报错并带上键本身，得到 %v", err)
	}
}

// ---------- 缓存与并发 ----------

type fakeCap struct {
	mu       sync.Mutex
	calls    int
	block    chan struct{}
	last     string
	lastPage []byte
}

func (f *fakeCap) Capture(page []byte, route string) ([]byte, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = route
	f.lastPage = append([]byte(nil), page...)
	return []byte("PNG:" + route), nil
}

// inlined 回答"这一趟页面里有没有内联的海报字节"。
func (f *fakeCap) inlined() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Contains(string(f.lastPage), "data:image/jpeg")
}

func (f *fakeCap) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func posterFor(t *testing.T, recs []annsync.Rec, cap Capturer, now time.Time) *Poster {
	t.Helper()
	return New(Config{
		Records: func() ([]annsync.Rec, error) { return recs, nil },
		Cap:     cap, Label: "version", Zone: zone, Now: func() time.Time { return now },
	})
}

func TestImageCachesUntilDataOrBucketChanges(t *testing.T) {
	recs := []annsync.Rec{verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59")}
	cap := &fakeCap{}
	now := at("2026-09-08 20:00")
	p := posterFor(t, recs, cap, now)

	for i := 0; i < 3; i++ {
		if _, err := p.Image(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := cap.count(); got != 1 {
		t.Errorf("同一份数据同一个桶只该画一次，画了 %d 次", got)
	}
	if hits, _ := p.Stats(); hits != 2 {
		t.Errorf("命中计数该是 2，得到 %d", hits)
	}

	// 同一 5 分钟桶内过几分钟：出图时刻线只亚像素位移，字节不变，不该重画。
	p.cfg.Now = func() time.Time { return at("2026-09-08 20:04") }
	if _, err := p.Image(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := cap.count(); got != 1 {
		t.Errorf("同一桶内不该重画，画了 %d 次", got)
	}

	// 跨桶：重画一次。
	p.cfg.Now = func() time.Time { return at("2026-09-08 20:05") }
	if _, err := p.Image(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := cap.count(); got != 2 {
		t.Errorf("跨桶应该重画，画了 %d 次", got)
	}

	// 数据变了（活动结束时刻挪一分钟）：同一桶也必须重画，否则就是拿旧图糊弄新数据。
	recs[0].End = recs[0].End.Add(time.Minute)
	if _, err := p.Image(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := cap.count(); got != 3 {
		t.Errorf("数据变了应该重画，画了 %d 次", got)
	}
}

func TestConcurrentRequestsDrawOnce(t *testing.T) {
	recs := []annsync.Rec{verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59")}
	cap := &fakeCap{block: make(chan struct{})}
	p := posterFor(t, recs, cap, at("2026-09-08 20:00"))

	var n atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := p.Image(context.Background(), "")
			if err == nil && string(out) == "PNG:#x202609071600" {
				n.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // 让 8 个都挤到那一次画布上
	close(cap.block)
	wg.Wait()
	if got := int(n.Load()); got != 8 {
		t.Fatalf("8 个请求都该拿到图，只有 %d 个成功", got)
	}
	if got := cap.count(); got != 1 {
		t.Errorf("并发的同一张图只该画一次，画了 %d 次", got)
	}
}

func TestCaptureFailureIsNotCached(t *testing.T) {
	recs := []annsync.Rec{verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59")}
	cap := &errCap{}
	p := posterFor(t, recs, cap, at("2026-09-08 20:00"))
	if _, err := p.Image(context.Background(), ""); err == nil {
		t.Fatal("出图失败要往上抛")
	}
	if _, err := p.Image(context.Background(), ""); !errors.Is(err, errDraw) {
		t.Fatalf("失败不该被记成一张好图，第二次拿到 %v", err)
	}
	if cap.calls != 2 {
		t.Errorf("失败后下一次要真重试，calls=%d", cap.calls)
	}
}

var errDraw = errors.New("画不出来")

type errCap struct{ calls int }

func (e *errCap) Capture([]byte, string) ([]byte, error) {
	e.calls++
	return nil, errDraw
}

// ---------- 指纹 ----------

func TestFingerprintOnlyCaresAboutPictureFields(t *testing.T) {
	base := []annsync.Rec{
		actRec("4540-1", "本窗活动", "2026-09-09 00:00", "2026-09-12 03:59"),
		actRec("4540-2", "另一个", "2026-09-14 00:00", "2026-09-16 03:59"),
	}
	fp := fingerprint(base)

	drop := func(mut func([]annsync.Rec)) string {
		cp := append([]annsync.Rec(nil), base...)
		mut(cp)
		return fingerprint(cp)
	}

	// 参与画面的字段，动一个就得重画。
	for name, mut := range map[string]func([]annsync.Rec){
		"改标题":         func(r []annsync.Rec) { r[0].Title = "改名了" },
		"改结束时刻":       func(r []annsync.Rec) { r[0].End = r[0].End.Add(time.Minute) },
		"改起点口径":       func(r []annsync.Rec) { r[0].Status = annsync.StatusFuzzyStart },
		"改海报":         func(r []annsync.Rec) { r[0].Poster = "https://cdn/x.jpg" },
		"改兑换尾":        func(r []annsync.Rec) { r[0].ClaimEnd = at("2026-09-20 10:59") },
		"多一条":         func(r []annsync.Rec) { r[0] = actRec("4540-9", "新加的", "2026-09-18 00:00", "2026-09-19 00:00") },
		"pending 混进来": func(r []annsync.Rec) { r[1].Status = annsync.StatusPending },
	} {
		if got := drop(mut); got == fp {
			t.Errorf("%s：指纹没变，会拿旧图糊弄新数据", name)
		}
	}

	// 不上画面的字段变了不该惊动画布。
	for name, mut := range map[string]func([]annsync.Rec){
		"重新解析时间":   func(r []annsync.Rec) { r[0].ParsedAt = at("2026-09-09 12:00") },
		"改了正文 URL": func(r []annsync.Rec) { r[0].URL = "https://site/4540" },
		"合并来的引用":   func(r []annsync.Rec) { r[0].TwinRef = "4356-2" },
	} {
		if got := drop(mut); got != fp {
			t.Errorf("%s：指纹变了，会白重画一张一模一样的图", name)
		}
	}
}

// ---------- Config → Options 的注入口 ----------

// TestPosterConfigKnobsReachOptions 钉的是管道本身：Workers/PerImage/RetryAfter
// 全仓只有 options() 这一个注入口，接线漏一项，配下去的值就不生效，
// 而 dataset.Options 自己的兜底会把漏接伪装成"跑得正常"。
// 所以这里故意用与兜底不同的数（7 / 3s / 9m）：漏接必然露出来。
func TestPosterConfigKnobsReachOptions(t *testing.T) {
	p := New(Config{
		Records: func() ([]annsync.Rec, error) { return nil, nil },
		Cap:     &fakeCap{}, Label: "version", Zone: zone,
		Workers: 7, PerImage: 3 * time.Second, RetryAfter: 9 * time.Minute,
	})
	opt := p.options("", at("2026-09-08 12:00"), true)
	if opt.Workers != 7 {
		t.Errorf("Workers = %d, want 7：没接上就会退成 Options 的兜底 4", opt.Workers)
	}
	if opt.PerImage != 3*time.Second {
		t.Errorf("PerImage = %v, want 3s", opt.PerImage)
	}
	if opt.RetryAfter != 9*time.Minute {
		t.Errorf("RetryAfter = %v, want 9m", opt.RetryAfter)
	}
}
