package calposter

import (
	"context"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/store"
	"xingta/internal/store/storetest"
)

// prune 用例。钉的是两条互相制衡的不变量：
//   - 不涨：缓存规模 = Timeline 的 live 集（当值 ∪ 未过气），与公告历史多长无关
//     （XT-KN-02 的修复本体）；
//   - 不删错：当值窗（「日历」命令热读，随整版存活）与未过气窗（调度器未触发的
//     任务、时钟回拨后的宽限窗账）都不能被收掉。两条条款历史上各栽过一版：
//     首版"出 pushGrace 即删"把最热的当期图每轮删掉；二版"留末二键"的位置尺
//     在三窗并存时漏掉当值。负对照用例：TestPruneKeepsCurrentBeyondLastTwo
//     （抽掉当值条款必红）、TestPruneKeepsArmedLedgerAfterRewind（给未过气
//     条款加下界必红）。
//
// 刻意不叫 featureFor/不跑 Start：Start 的后台 loop 会插一轮不受控的 round，
// 本文件要"先种缓存、单步 round、再断言"，每轮都必须由测试主体驱动。

const (
	oldKey   = "202607281600" // 远古版本 2026-07-29 00:00 (+08)
	staleKey = "202608171600" // 欢歌劲浪 2026-08-18 00:00 (+08)
	curKey   = "202609071600" // 奋斗吧 2026-09-08 00:00 (+08)，与既有测试同键
	nextKey  = "202609211600" // 下期版本 2026-09-22 00:00 (+08)
)

// 三个版本窗按时间升序：old(07-29) < stale(08-18) < cur(09-08) < next(09-22)。
func oldRec() annsync.Rec {
	return verRec("4300", "远古版本", "2026-07-29 00:00", "2026-08-11 03:59", "2026-08-18 10:59")
}

// startedPoster 是"api 与 Doc 都齐、但不起后台循环"的 Poster：p.api 直接赋值
// （同包），loadSent 手动执行，round 由测试单步驱动。
//
// 不走 Start 是刻意的。Start 会 go loop，那一次立即 round 与测试主体自己调的
// round 并发，而 armPushes 的"查已推 → 排期"不是原子的（push 入口那句注释说的
// 就是它）：后台轮可能判定 settled 为假后被挂起，等测试主体 fire→settle 跑完才
// 把 Schedule 落下去，于是"了结之后不再排期"这类断言偶发翻车。产品侧无害——
// 多排的那次任务触发后 push 会再查一次账本直接返回——错在断言了这个被刻意容忍
// 为陈旧的裸调度器状态。要覆盖 Start 的接线（立即预热读 Client、Reg 注册命令）
// 就直接调 Start，见 TestRoundFetchesArtOncePerFingerprint 与
// TestCalendarCommandRepliesWithImage。
func startedPoster(t *testing.T, recs *recBox, cap Capturer, doc store.Doc, now time.Time, targets ...string) (*Poster, *fakeAPI) {
	t.Helper()
	api := newAPI()
	api.targets = targets // 与生产一致：目标来自 api.TargetsFor 的订阅表
	p := New(Config{
		Doc: doc, Records: recs.recs, Cap: cap, Label: "version", Zone: zone,
		Now: func() time.Time { return now },
	})
	p.api = api
	if err := p.loadSent(); err != nil {
		t.Fatal(err)
	}
	return p, api
}

func seedShot(p *Poster, key, data string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shots[key] = shot{token: "seed|" + key, data: []byte(data)}
}

func shotKeys(p *Poster) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k := range p.shots {
		out = append(out, k)
	}
	return out
}

func ledgerKeys(p *Poster) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k := range p.ledger {
		out = append(out, k)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func docHas(t *testing.T, doc store.Doc, key string) bool {
	t.Helper()
	var rec pushRec
	found, err := doc.Get(NS, key, &rec)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func mustPutSettled(t *testing.T, doc store.Doc, key string, settled time.Time) {
	t.Helper()
	if err := doc.Put(NS, key, pushRec{SettledAt: settled}); err != nil {
		t.Fatal(err)
	}
}

// TestPruneKeepsCurrentVersionThroughRounds 「日历」命令的稳态热路径：当期早已出
// pushGrace（占它生命期 ~99%），round 反复跑也不能碰它那张图——同桶第二次 Image
// 必须命中缓存而不是重画。这是回归实锤（每轮删当期）的固化。
func TestPruneKeepsCurrentVersionThroughRounds(t *testing.T) {
	recs := box(
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		verRec("4700", "下期版本", "2026-09-22 00:00", "2026-10-06 03:59", "2026-10-13 10:59"),
	)
	now := at("2026-09-19 10:00") // 开闸（09-08 17:00）后第 11 天
	cap := &fakeCap{}
	p, _ := startedPoster(t, recs, cap, storetest.NewMem(), now, "g:grp")

	if _, err := p.Image(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	p.round(context.Background())
	if got := shotKeys(p); !contains(got, curKey) {
		t.Fatalf("round 把当期热缓存删了: %v", got)
	}
	if _, err := p.Image(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	hits, builds := p.Stats()
	if builds != 1 || hits != 1 || cap.count() != 1 {
		t.Errorf("round 后同桶再取应命中：hits=%d builds=%d captures=%d", hits, builds, cap.count())
	}
}

// TestPruneRemovesDeadVersions 三个版本窗：死键（非当值且早出宽限窗）的图与账
// （内存+磁盘）全回收；当值窗（stale，开闸已过的最新一个）必须陪跑。
// "只增不减"被终结的证据就是这条会红着进来、绿着出去——停用 prune 时 oldKey
// 的三处都在。
func TestPruneRemovesDeadVersions(t *testing.T) {
	recs := box(
		oldRec(),
		verRec("4356", "欢歌劲浪", "2026-08-18 00:00", "2026-09-01 03:59", "2026-09-08 10:59"),
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
	)
	doc := storetest.NewMem()
	mustPutSettled(t, doc, oldKey, at("2026-07-29 17:05"))
	mustPutSettled(t, doc, staleKey, at("2026-08-18 17:05"))
	// now=09-08 08:00：cur 未开闸（17:00 开），排在期上；stale 是当值；old 是死键。
	p, api := startedPoster(t, recs, &fakeCap{}, doc, at("2026-09-08 08:00"), "g:grp")
	seedShot(p, oldKey, "远古版本的海报")
	seedShot(p, staleKey, "上期的海报（当值窗，必须陪跑）")

	p.round(context.Background())

	if got := shotKeys(p); contains(got, oldKey) {
		t.Errorf("死键的图没回收: %v", got)
	}
	if got := ledgerKeys(p); contains(got, oldKey) {
		t.Errorf("死键的内存账没回收: %v", got)
	}
	if docHas(t, doc, oldKey) {
		t.Error("死键的磁盘账没回收（calexpiry 同纪律：内存与盘一起删）")
	}
	// 当值窗（上期已开闸、仍是"最新的已开一个"）：账与图都要在——「日历」命令热读挂在它身上。
	if !contains(shotKeys(p), staleKey) || !contains(ledgerKeys(p), staleKey) || !docHas(t, doc, staleKey) {
		t.Errorf("当值窗被误删: shots=%v ledger=%v doc=%v", shotKeys(p), ledgerKeys(p), docHas(t, doc, staleKey))
	}
	if !api.armed("poster:" + curKey) {
		t.Error("当期没排上期")
	}
}

// TestPruneKeepsSettledWithinGrace 已了结但仍在开闸一小时窗内：账必须留着挡重推，
// 图也还在被「日历」命令用。
func TestPruneKeepsSettledWithinGrace(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	doc := storetest.NewMem()
	mustPutSettled(t, doc, curKey, at("2026-09-08 17:05"))
	p, api := startedPoster(t, recs, &fakeCap{}, doc, at("2026-09-08 17:30"), "g:grp")
	seedShot(p, curKey, "当前版本的海报字节")

	p.round(context.Background())

	if !contains(ledgerKeys(p), curKey) || !docHas(t, doc, curKey) {
		t.Error("宽限窗内已了结的账被误删，下轮会重推")
	}
	if !contains(shotKeys(p), curKey) {
		t.Error("宽限窗内版本的 shots 被误删")
	}
	if api.armed("poster:" + curKey) {
		t.Error("已了结的版本不该被重新排推")
	}
}

// TestPruneKeepsFutureWindow 未开闸的下期是"未过气"（未来窗恒合格），预热的图与空账都算活。
func TestPruneKeepsFutureWindow(t *testing.T) {
	recs := box(
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
		verRec("4700", "下期版本", "2026-09-22 00:00", "2026-10-06 03:59", "2026-10-13 10:59"),
	)
	doc := storetest.NewMem()
	p, api := startedPoster(t, recs, &fakeCap{}, doc, at("2026-09-08 08:00"), "g:grp")
	seedShot(p, nextKey, "下期版本预热出的图")
	p.mu.Lock()
	p.ledger[nextKey] = &pushRec{}
	p.mu.Unlock()

	p.round(context.Background())

	if !contains(shotKeys(p), nextKey) {
		t.Errorf("未开闸版本的预热图被误删: %v", shotKeys(p))
	}
	if !contains(ledgerKeys(p), nextKey) {
		t.Errorf("未开闸版本的账被误删: %v", ledgerKeys(p))
	}
	if !api.armed("poster:"+nextKey) || !api.armed("poster:"+curKey) {
		t.Error("未开闸/在窗版本应全部排上期")
	}
}

// TestMarkToAfterPruneConverges 回收之后迟到的回执会重建条目（markTo 见无账即新建），
// 不 panic；下一轮 prune 幂等收掉，内存与盘都归零。
func TestMarkToAfterPruneConverges(t *testing.T) {
	recs := box(
		oldRec(),
		verRec("4356", "欢歌劲浪", "2026-08-18 00:00", "2026-09-01 03:59", "2026-09-08 10:59"),
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
	)
	doc := storetest.NewMem()
	p, _ := startedPoster(t, recs, &fakeCap{}, doc, at("2026-09-08 08:00"), "g:grp")

	p.round(context.Background())

	p.MarkPushed("g:grp", oldKey) // 模拟拖出回收窗口的在飞投递终于回执
	if !contains(ledgerKeys(p), oldKey) || !docHas(t, doc, oldKey) {
		t.Fatal("markTo 应重建条目并落盘（幂等写入口不该被回收历史破坏）")
	}

	p.round(context.Background()) // 下一轮：死键，再收掉
	if contains(ledgerKeys(p), oldKey) || docHas(t, doc, oldKey) {
		t.Error("重建的过期账没被下一轮 prune 收掉")
	}
}

// TestRoundsDoNotGrowCaches 增长守卫的正身：预种一个会出窗口的旧键，连渲五个桶、
// 跑五轮 round，最后 shots 必须精确等于当期一条——不启用 prune 时旧键会留在表里，
// 这条必红（上一版守卫只渲一个版本，量不到"增"，是坏尺子）。
func TestRoundsDoNotGrowCaches(t *testing.T) {
	recs := box(
		oldRec(),
		verRec("4356", "欢歌劲浪", "2026-08-18 00:00", "2026-09-01 03:59", "2026-09-08 10:59"),
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
	)
	doc := storetest.NewMem()
	cap := &fakeCap{}
	base := at("2026-09-08 17:05") // 当期刚开闸 5 分钟，在宽限窗内
	p, _ := startedPoster(t, recs, cap, doc, base, "g:grp")
	seedShot(p, oldKey, "远古版本的海报")

	for i := 0; i < 5; i++ {
		p.cfg.Now = func() time.Time { return base.Add(time.Duration(i) * 5 * time.Minute) } // 每次跨一个渲染桶
		if _, err := p.Image(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		p.round(context.Background())
	}

	if got := shotKeys(p); len(got) != 1 || got[0] != curKey {
		t.Errorf("五轮五桶后 shots 应只剩当期一张，得到 %v", got)
	}
	if cap.count() != 5 {
		t.Errorf("每桶重画一次共 5 次，画了 %d 次", cap.count())
	}
	if got := ledgerKeys(p); len(got) != 0 {
		t.Errorf("没人回执，账应为空，得到 %v", got)
	}
}

// TestPruneKeepsCurrentBeyondLastTwo 当值条款的负对照：四窗并存（一个死键、当值、
// 两个已预公告的未来窗）时，位置尺的"留末二"会把当值窗排到末三——它的图与账正被
// 「日历」命令热读，必须按"当值"身份存活，与位置无关。抽掉 Timeline live 集的
// 当值条款（只留未过气）这条必红：当值窗早出宽限窗，armed 救不了它。
func TestPruneKeepsCurrentBeyondLastTwo(t *testing.T) {
	recs := box(
		oldRec(), // 07-29 开闸，死键
		verRec("4356", "欢歌劲浪", "2026-08-18 00:00", "2026-09-01 03:59", "2026-09-08 10:59"),  // 当值
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),   // 未来①
		verRec("4700", "下下期版本", "2026-09-22 00:00", "2026-10-06 03:59", "2026-10-13 10:59"), // 未来②
	)
	doc := storetest.NewMem()
	mustPutSettled(t, doc, staleKey, at("2026-08-18 17:05"))
	// now=08-25 12:00：欢歌劲浪 08-18 17:00 已开闸且是最新开的一个 → 当值；
	// 后两窗未开闸（未来窗，armed）；old 出宽限窗 27 天 → 死。
	p, api := startedPoster(t, recs, &fakeCap{}, doc, at("2026-08-25 12:00"), "g:grp")
	seedShot(p, oldKey, "远古版本的海报")
	seedShot(p, staleKey, "当值窗的海报（位置尺下排末三，会被误删）")

	p.round(context.Background())

	if !contains(shotKeys(p), staleKey) || !contains(ledgerKeys(p), staleKey) || !docHas(t, doc, staleKey) {
		t.Errorf("当值窗被误删: shots=%v ledger=%v doc=%v", shotKeys(p), ledgerKeys(p), docHas(t, doc, staleKey))
	}
	if contains(shotKeys(p), oldKey) {
		t.Errorf("死键的图没回收: %v", shotKeys(p))
	}
	if !api.armed("poster:"+curKey) || !api.armed("poster:"+nextKey) {
		t.Error("两个未来窗都该排上期")
	}
	if api.armed("poster:"+staleKey) || api.armed("poster:"+oldKey) {
		t.Error("当值窗已了结、死键不再补——都不该排期")
	}
}

// TestPruneKeepsArmedLedgerAfterRewind 未过气条款（不设下界）的负对照：某版已在
// 开闸时刻推过（账已了结），时钟回拨到它开闸之前——窗口回到"未来"，now − at 为
// 负。此刻 armPushes 每轮都会把任务重排上，唯一能挡住重推的就是这份 settled 账。
// 给 armed 加下界（只认已开闸的窗）或抽掉未过气条款，这条必红。
func TestPruneKeepsArmedLedgerAfterRewind(t *testing.T) {
	recs := box(verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"))
	doc := storetest.NewMem()
	mustPutSettled(t, doc, curKey, at("2026-09-08 17:05")) // 开闸 5 分钟后推讫
	// 回拨到开闸日上午：at（17:00）还没到，窗是"未来"，当值条款也救不了（未开闸）。
	p, api := startedPoster(t, recs, &fakeCap{}, doc, at("2026-09-08 08:00"), "g:grp")
	seedShot(p, curKey, "回拨前推过的图")

	p.round(context.Background())

	if !contains(ledgerKeys(p), curKey) || !docHas(t, doc, curKey) {
		t.Error("回拨后未过气的账被误删：任务到点触发时 settled 失据，会重推")
	}
	if !contains(shotKeys(p), curKey) {
		t.Error("回拨后未过气的图被误删")
	}
	if api.armed("poster:" + curKey) {
		t.Error("已了结的版本，回拨后也不该再排推（armPushes 该被 settled 挡住）")
	}
}
