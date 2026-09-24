// Package calposter 出「版本日历图」：把某个版本的活动排期画成一张 16:9 卡片。
//
// 分层契约在这里的执行点是**取数与版式的口径**：库里读出来的是 annsync.Rec，
// "哪些记录进这个版本窗""哪些算周期玩法""版本键怎么定"写在本包，不下沉进
// internal/render（它只认 render.Dataset 这个形状，不认识版本与活动），也不
// 上浮进 main。
//
// 本文件只管"要一张图 → 给这张图"：读库、算数据指纹、命中缓存就直接返回，
// 没命中才组装数据集并叫浏览器画。版式判断在 dataset.go。
package calposter

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"sync"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/kernel"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/target"
	"xingta/internal/qq"
	"xingta/internal/store"
)

//go:embed template.html
var defaultTemplate []byte

//go:embed template_legacy.html
var legacyTemplate []byte // 保留旧版模板以备查阅或切回

// DefaultTemplate 返回当前使用的默认出图模板（手账风新版 UI）。
func DefaultTemplate() []byte {
	return defaultTemplate
}

// LegacyTemplate 返回历史旧版单文件模板（保留备查或对比测试）。
func LegacyTemplate() []byte {
	return legacyTemplate
}

// Capturer 是"给一页 HTML，回一张 PNG"的能力，render.Browser 天然满足。
// 抽出来是为了单测不必真起浏览器。
type Capturer interface {
	Capture(page []byte, route string) ([]byte, error)
}

// Config 的零值不可用：Records / Cap / Label 都得给。
type Config struct {
	// Doc 只用来记本功能自己的账（命名空间 calposter：版本键 → 推图时刻）。
	// 没有这份账，每次重启都会把窗口期内的版本再推一遍。
	Doc store.Doc
	// Records 读本源的活动记录。只给这一个闭包而不给整个 store.Doc：功能要的是
	// "读到 Rec"，不是"能读写任意命名空间"。main 里接 annsync.ReadRecs，
	// 单测里直接给一段切片。
	Records func() ([]annsync.Rec, error)
	Cap     Capturer
	// Label 是哪个 Provenance 取值代表版本窗口公告（main 从 stellasora 传进来）。
	Label  string
	Zone   *time.Location
	OpenAt time.Duration // 开闸估计，默认 17h：见 dataset.go 的 Current
	// PushDelay 是"该推送的时刻"相对开闸的富余：官方版本公告与海报要传上 CDN，
	// 开闸那一刻推往往推出一张海报位全是占位的图。0 = 开闸即推（库级默认，
	// 部署值在 main.go 的 -poster-push-delay）。
	PushDelay time.Duration
	Client    *http.Client  // nil = 不下载海报（海报位画斜纹占位）
	Warm      time.Duration // 多久看一次数据有没有变，变了就去预热海报，默认 5m
	// ArtDir 非空时海报字节落盘：海报 CDN 会掐反复整窗拉取的客户端，
	// 只放内存等于每次重启都重新捶一遍。
	ArtDir   string
	Template []byte // nil = 用 render 内嵌的那一份模板
	// Reg 非空就注册「日历」命令；留空表示这个功能只负责主动推图。
	Reg  command.Registrar
	Now  func() time.Time
	Logf func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.Zone == nil {
		c.Zone = time.FixedZone("CST", 8*3600)
	}
	if c.OpenAt <= 0 {
		c.OpenAt = 17 * time.Hour
	}
	if c.Warm <= 0 {
		c.Warm = 5 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Template == nil {
		c.Template = defaultTemplate
	}
	return c
}

// Poster 出图并记住最近画过的那几张。
type Poster struct {
	cfg Config

	mu     sync.Mutex
	api    kernel.API          // Start 之后才有：推图要排期与投递
	ledger map[string]*pushRec // 版本键 → 账（内存镜像，权威；磁盘是它的投影）
	art    *ArtCache           // 海报字节，跨分钟复用
	shots  map[string]shot     // 版本键 → 当前数据下画好的那一张
	calls  map[string]*call    // 同一个键并发的取图请求合成一次画布
	hits   int
	built  int
}

// shot 每个版本只留一份：数据变了指纹就变、时钟跨过 5 分钟桶也变，旧的直接作废。
// 版本键会随公告历史一直新增，但每轮 round 的 prune 只留 Timeline 的 live 集
// （当值 ∪ 未过气），稳态下只剩当期一张图，不需要 LRU。
type shot struct {
	token string // 指纹 + 桶起点，两个都一样才算同一张图
	data  []byte
}

// call 是手工版 singleflight：命令回图与版本推送可能同一刻要同一张图，
// 各画一份就是两份浏览器进程 + 两遍海报下载。
type call struct {
	wg  sync.WaitGroup
	out []byte
	err error
}

func New(cfg Config) *Poster {
	if cfg.Records == nil || cfg.Cap == nil {
		panic("calposter: Records 与 Capturer 都不能为空")
	}
	return &Poster{
		cfg: cfg.withDefaults(), art: NewArtCache(cfg.ArtDir, 0), ledger: map[string]*pushRec{},
		shots: map[string]shot{}, calls: map[string]*call{},
	}
}

// Stats 是缓存计数，给日志与测试看。
func (p *Poster) Stats() (hits, builds int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits, p.built
}

// CurrentKey 返回 now 时刻该发的那一张的版本键。命令与推送都用它定位"当前版本"。
func (p *Poster) CurrentKey() (string, error) {
	recs, err := p.cfg.Records()
	if err != nil {
		return "", fmt.Errorf("calposter: 读活动记录: %w", err)
	}
	r, ok := Current(recs, p.cfg.Label, p.cfg.Now(), p.cfg.OpenAt)
	if !ok {
		return "", fmt.Errorf("calposter: 还没有任何版本开闸（%d 条记录里没有版本窗口）", len(recs))
	}
	return VersionKey(r.Start), nil
}

// Image 要一张版本日历图，**只读本地字节**，一个网络请求都不发。key 为空表示当前版本。
//
// 缓存的边界落在"公告数据变了吗 + 这个 5 分钟桶画过吗"上：桶内任何一刻的画布输入
// 相同（出图时刻线亚像素移动，肉眼不可见），等价保证就是同数据同桶只画一次。
//
// 海报没预热到就是没预热到：这一趟画占位，快而可预期。抓与重试是 Fetch 的活。
func (p *Poster) Image(ctx context.Context, key string) ([]byte, error) {
	return p.image(ctx, key, false)
}

// Fetch 与 Image 同义，但允许现抓缺的海报。给版本推图用：那条路在队列 worker
// 上，不在用户等回复的那一次上，宁可慢 40 秒也不要推一张全是占位的图。
func (p *Poster) Fetch(ctx context.Context, key string) ([]byte, error) {
	return p.image(ctx, key, true)
}

func (p *Poster) image(ctx context.Context, key string, fetch bool) ([]byte, error) {
	recs, err := p.cfg.Records()
	if err != nil {
		return nil, fmt.Errorf("calposter: 读活动记录: %w", err)
	}
	now := p.cfg.Now()
	fp := fingerprint(recs)
	if key == "" {
		r, ok := Current(recs, p.cfg.Label, now, p.cfg.OpenAt)
		if !ok {
			return nil, fmt.Errorf("calposter: %s 还没有任何版本开闸（%d 条记录里没有版本窗口）",
				now.In(p.cfg.Zone).Format(TimeLayout), len(recs))
		}
		key = VersionKey(r.Start)
	}
	// 画布与令牌共用桶起点：同一桶内画两次，字节一样，只画一次；红线一桶一跳，
	// 亚像素级，肉眼不可见。Current 判开闸仍用真 now，版本切换不受桶影响。
	bucket := now.Truncate(renderBucket)
	token := fp + "|" + bucket.In(p.cfg.Zone).Format(TimeLayout)

	p.mu.Lock()
	if s, ok := p.shots[key]; ok && s.token == token {
		p.hits++
		p.mu.Unlock()
		return s.data, nil
	}
	if c, ok := p.calls[key]; ok {
		p.mu.Unlock()
		c.wg.Wait()
		return c.out, c.err
	}
	c := &call{}
	c.wg.Add(1)
	p.calls[key] = c
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.calls, key)
		if c.err == nil && len(c.out) > 0 {
			p.shots[key] = shot{token: token, data: c.out}
			p.built++
		}
		p.mu.Unlock()
		c.wg.Done()
	}()

	c.out, c.err = p.draw(ctx, recs, key, bucket, fetch)
	return c.out, c.err
}

// draw 是真的走一遍：组数据集 → 注模板 → 叫浏览器画。
func (p *Poster) draw(ctx context.Context, recs []annsync.Rec, key string, now time.Time, fetch bool) ([]byte, error) {
	started := time.Now()
	d, err := Build(ctx, recs, p.options(key, now, fetch))
	if err != nil {
		return nil, err
	}
	assembled := time.Now()
	page, err := Page(d, p.cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("calposter: 注入模板: %w", err)
	}
	paged := time.Now()
	raw, err := p.cfg.Cap.Capture(page, "#x"+key)
	if err != nil {
		return nil, fmt.Errorf("calposter: 出图: %w", err)
	}
	drew := time.Now()
	// 三段各自耗时留在日志里：慢在取数、慢在排版还是慢在浏览器，处理方式完全不同。
	p.cfg.Logf("calposter: %s 组数据 %s｜注模板 %s（页面 %d KB，%d 条记录）｜浏览器 %s → %d KB",
		key, assembled.Sub(started).Round(time.Millisecond), paged.Sub(assembled).Round(time.Millisecond),
		len(page)/1024, len(d.Records), drew.Sub(paged).Round(time.Millisecond), len(raw)/1024)
	return raw, nil
}

// fingerprint 是这批记录的等价类：只要它不变，画出来的图就一模一样。
//
// 不用 annsync 的同步计数当指纹——人工覆盖（calops）改了窗口不改计数，
// 用计数就会拿旧图糊弄新数据。这里逐条摊开参与画面的字段，宁可算得细。
func fingerprint(recs []annsync.Rec) string {
	h := fnv.New64a()
	for _, r := range recs {
		if !r.InCalendar() {
			continue
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\n",
			r.ID, r.Title, r.Label,
			r.Start.Unix(), r.End.Unix(), r.ClaimEnd.Unix(),
			r.Status, r.Poster, r.Provenance)
	}
	fmt.Fprintf(h, "#%d", len(recs))
	return strconv.FormatUint(h.Sum64(), 16)
}

// renderBucket 是出图的时钟量化粒度：同桶内任何一刻画的图字节都相同
// （实测：出图分钟戳删掉后连续 ~9 分钟 sha1 不变，红线亚像素级移动），
// 所以桶就是等价类，令牌与画布输入都用桶起点，跨分钟不再白渲一遍。
const renderBucket = 5 * time.Minute

// ---------- 作为 kernel.Feature：预热 + 到点主动推图 ----------

// NS 是本功能在 store.Doc 里占的命名空间：键=版本键，值=推送账本记录。
const NS = "calposter"

// pushRec 是账本记录：键=版本键（裸键，不带目标），值=哪些目标收到了、是否全局了结。
// 写入纪律：磁盘永远是"锁内以内存镜像为准整条序列化 + Put"，不在盘上读改写——
// 两个 worker 的回执并发回来时各改各的内存、整条覆盖写盘，天然无竞争且幂等。
type pushRec struct {
	SettledAt time.Time            `json:"settled_at,omitempty"` // 全局了结：全员收齐/无目标只记日志的轮次
	Targets   map[string]time.Time `json:"targets,omitempty"`    // 目标 → 回执时刻
}

// pushGrace 是"该推送的时刻已经过了还补不补"的窗口。重启晚了 1 小时内照发一次；
// 更早的不补——这个功能第一次上线时，把历史上每个版本都推一遍是刷屏。
// 起算点是 pushAt（= 开闸 + PushDelay），不是开闸：从开闸起算的话，偏移本身就把
// 预算吃掉了（偏移 1h 等于再没有补推余量）。
const pushGrace = time.Hour

func (p *Poster) Name() string { return "calposter" }

// Start 非阻塞（kernel.Feature 契约）：循环自己起 goroutine 并尊重 ctx。
func (p *Poster) Start(ctx context.Context, api kernel.API) error {
	if p.cfg.Doc == nil {
		return errors.New("calposter: 需要 store.Doc 来记「哪个版本已经推过」")
	}
	if api == nil {
		return errors.New("calposter: 需要 kernel.API 才能排期与投递")
	}
	p.api = api
	_ = api.RegisterTopic(target.Topic{
		Key:  "poster",
		Name: "版本日历海报",
		Desc: "版本开启时主动推送日历海报长图",
	})
	if p.cfg.Reg != nil {
		if err := p.cfg.Reg.Add(command.Cmd{
			Name: "calendar", Usage: "当前版本的活动日历图", Run: p.calendar,
		}); err != nil {
			return err
		}
	}
	if err := p.loadSent(); err != nil {
		return fmt.Errorf("calposter: 读已推图记录: %w", err)
	}
	go p.loop(ctx)
	return nil
}

// calendar 是「日历」命令：一条命令一条回复，这里回的就是那一张图。
//
// 图与文字二选一，所以画不出来时不做"降级成文字列表"——那等于把同一件事用
// 两种口径各说一遍，而文字口径刚刚才被这个功能废掉。
func (p *Poster) calendar(ctx context.Context, m *qq.Message, args []string) (command.Reply, error) {
	raw, err := p.Image(ctx, "")
	if err != nil {
		return command.Reply{}, err
	}
	return command.Reply{Image: raw}, nil
}

func (p *Poster) loop(ctx context.Context) {
	p.round(ctx) // 立刻一轮：重启后该预热的预热、该排的排上，不等第一个 Warm 周期
	t := time.NewTicker(p.cfg.Warm)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.round(ctx)
		}
	}
}

// round 每轮做三件事：把当前版本窗缺的海报补进缓存；把未过气版本的推图排上期；
// 把死键的渲染与账本回收掉。
//
// 时间线每轮只建一份（NewTimeline）：排期与回收用同一个 now、同一条开闸公式、
// 同一条生死判据，两把尺子各算各的漂移空间被结构性消除。出图路径（Image/draw）
// 不经过这里——请求就该拿最新数据现算，与本轮的尺子无关。
//
// 预热是这条链路的性价比所在——海报是唯一"贵而与时间无关"的部分（实测 16 张
// 海报串行拉完要 40 秒，缓存命中后组数据只剩 50 毫秒）。把它放在后台轮里，
// 用户要图那一次就只剩浏览器那 3 秒。
//
// 每轮都过一遍，不限"数据变了"：上一轮没抓到的这一轮要能补，而反复捶同一个
// 域名的风险已经由 ArtCache 的失败冷却挡住了。
func (p *Poster) round(ctx context.Context) {
	recs, err := p.cfg.Records()
	if err != nil {
		p.cfg.Logf("calposter: 本轮读不到活动记录: %v", err)
		return
	}
	tl := NewTimeline(recs, p.cfg.Label, p.cfg.Zone, p.cfg.Now(), p.cfg.OpenAt, p.cfg.PushDelay)
	p.warmArt(ctx, recs)
	p.armPushes(tl)
	p.prune(tl)
}

// warmArt 把当前版本窗里每条记录的海报补进缓存；画出来的数据集丢掉。
func (p *Poster) warmArt(ctx context.Context, recs []annsync.Rec) {
	if p.cfg.Client == nil {
		return // 没配 HTTP 客户端 = 明确不要海报，不预热
	}
	started := time.Now()
	if _, err := Build(ctx, recs, p.options("", p.cfg.Now(), true)); err != nil {
		p.cfg.Logf("calposter: 预热海报没走完: %v", err)
		return
	}
	p.cfg.Logf("calposter: 预热轮 %s", time.Since(started).Round(10*time.Millisecond))
}

// options 把 Config 翻成 Build 的口径；key 为空表示当前版本。
func (p *Poster) options(key string, now time.Time, fetch bool) Options {
	return Options{
		Zone: p.cfg.Zone, OpenAt: p.cfg.OpenAt, Label: p.cfg.Label,
		Key: key, Now: now, HTTPClient: p.cfg.Client,
		Fetch: fetch, Art: p.art, Logf: p.cfg.Logf,
	}
}

// armPushes 给每个"未过气"（now − 该推送的时刻 ≤ pushGrace，未来窗恒合格，见
// Timeline.armed）且没推过的版本排一次期。
//
// 触发点用 pushAt（= act0 + 开闸估计 + PushDelay），与 prune 的保活判据是
// Timeline 里的同一条——开闸公式与偏移全包只写一份，这里拿现成的 pushAt。
// 00:00 只是维护中的下界，那时候新版本既没开、海报也常常还没上传，推出去是一张
// 全是占位图的图；PushDelay 就是在等后半句。
// Schedule 按 ID 幂等，所以每轮重排不会重复；已推过的直接跳过。
func (p *Poster) armPushes(tl *Timeline) {
	for i := range tl.wins {
		if !tl.armed(i) || p.settled(tl.wins[i].Key) {
			continue
		}
		key, at := tl.wins[i].Key, tl.wins[i].pushAt
		p.api.Schedule("poster:"+key, at, func(ctx context.Context) error { return p.push(ctx, key) })
	}
}

// prune 收敛 shots（整张 PNG 字节）与 ledger（内存+磁盘账），只留 Timeline 的
// live 集：当值 ∪ 未过气（now − 该推送的时刻 ≤ pushGrace）。
//
// 每条账至多一类读者，live 的两个条款正好把他们收全（推导见 Timeline 注释）：
//   - 当值——「日历」命令与预热按当期键反复取图，一版存活两三周，出宽限窗
//     不等于没人读。首版按"出 pushGrace 即删"，把最热的当期图每轮删掉；
//   - 未过气——调度器里还没触发的任务（未来窗等到点、刚开闸的宽限窗内欠推
//     补推）与时钟回拨回某窗宽限期的情形，账要活着让 settled 挡住重推。
//
// 二版曾改用"留末二键"的位置尺：不数时间、只数位置，当期误删是止住了，但
// 位置假设在三窗并存（上一版兑换尾未收、未来窗已预公告两版）时失效，当值键
// 排到末三就被清空账本、宽限窗内隔轮重推。现版按读者判：一个键死了 ⟺ 非当值
// ∧ now > 开闸 + pushGrace，它的任务要么必然已触发、要么必然不再触发，图也没
// 有读者。判据与 armPushes 的排期范围共用 Timeline.armed 一条谓词，两把尺子
// 不可能各漂各的。
//
// 红利：上期不再陪跑一整期（位置尺下它恒为末二），稳态 shots 只剩当期一张。
// 代价：时钟回拨跨过某版"开闸 + pushGrace"再拨回来时，已删的账至多让海报重推
// 一张，后果有界。
//
// 账的删除跟 calexpiry 同纪律：内存与磁盘一起删、盘删失败只记日志——内存镜像是
// 权威，内存删掉后后续 prune 就看不到这个键了，孤儿盘记录进程内不重试，靠重启后
// loadSent 捞回、再被当轮 prune 收掉。写盘在锁内，与 settle 的"锁内整条写回"同序。
func (p *Poster) prune(tl *Timeline) {
	live := tl.LiveKeys()
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.shots {
		if !live[key] {
			delete(p.shots, key)
		}
	}
	for key := range p.ledger {
		if live[key] {
			continue
		}
		delete(p.ledger, key)
		if err := p.cfg.Doc.Delete(NS, key); err != nil {
			p.cfg.Logf("calposter: 删过期版本账 %s 失败: %v", key, err)
		}
	}
}

// push 到点了：只做一件事——把"该发哪一张、发给还没收到的目标"投进发送队列。
//
// 这里绝不出图：调度器是同步跑回调的（schedule.go 的 Fn 契约），画一趟就把同期
// 到期的提醒一起拖住了。渲染与上传都推到 Sink 真要发的那一刻——file_info 的 ttl
// 只有几分钟，在队列里排过一轮就可能已经过期。
//
// 账本也不在这里写：只有队列报"发出去了"（条目的 OnDelivered 回执）才算了结。
// 画不出来或发送失败时队列自己退避重试，最终放弃也不记账，下一轮 armPushes 会
// 重新排上——宁可晚发，也不要发一张空的或干脆不发还装作发过了。
func (p *Poster) push(_ context.Context, key string) error {
	// 以账本为准再判一次：armPushes 的"查已推 → 排期"不是原子的，排期循环可能
	// 刚越过那道检查、这个任务就触发了，于是它会被重新排上、被调度器立刻再跑一遍。
	if p.settled(key) {
		return nil
	}
	targets := p.targets()
	if len(targets) == 0 {
		p.cfg.Logf("calposter: 没配推图目标，版本 %s 的图只记日志", key)
		p.settle(key)
		return nil
	}
	pending := 0
	var errs []error
	for _, tgt := range targets {
		if p.servedTo(tgt, key) {
			continue // 该目标已有回执（或整条已全局了结），不重投
		}
		pending++
		item := queue.Item{
			ID:     "poster:" + key + "@" + tgt,
			Target: tgt,
			Media:  &queue.Media{Kind: "poster", Key: key},
			Topic:  "poster",
			// 回执卡随包裹走：队列报发送成功才记账，失败与丢弃永远不记
			OnDelivered: func() { p.MarkPushed(tgt, key) },
		}
		// 每个目标都试完再汇总：中途因为队列满返回，会漏掉后面的目标。
		if err := p.api.Submit(item); err != nil {
			errs = append(errs, fmt.Errorf("投版本 %s 到 %s: %w", key, tgt, err))
		}
	}
	if pending == 0 {
		// 订阅集可能在投递后收缩过（有人退订）：剩余目标全有回执就收敛写全局了结，
		// 宽限窗内不再反复空排空扫；语义与"全员收齐才收敛"一致。
		p.settle(key)
		return nil
	}
	if len(errs) > 0 {
		return fmt.Errorf("calposter: %v", errors.Join(errs...))
	}
	return nil
}

// targets 返回当前订阅了 poster 主题的目标；无订阅者时返回空，push 走"只记日志"。
func (p *Poster) targets() []string {
	return p.api.TargetsFor("poster")
}

// MarkPushed 由队列条目的 OnDelivered 在"这条真的发出去了"之后调用：账本唯一的
// 常规写入口。队列对同一批目标会重试，所以这里要幂等（markTo 覆盖时间戳）。
func (p *Poster) MarkPushed(tgt, key string) {
	if key == "" || tgt == "" {
		return
	}
	p.markTo(tgt, key)
}

// loadSent 捞回上一次进程的推图记录。解不开的值按"已全局了结"兜底，与 calexpiry
// 同一条取舍：重发只会刷屏，漏发的补不回来。不认识任何旧格式（开发期不做迁移）。
func (p *Poster) loadSent() error {
	// 虽然目前只在 Start 单协程里跑，锁与另两个功能的 load 保持同一套风格，
	// 免得将来换个调用点就变成静默的数据竞争。
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Doc.Scan(NS, "", func(key string, raw []byte) error {
		var rec pushRec
		if err := json.Unmarshal(raw, &rec); err != nil {
			rec.SettledAt = time.Now()
			p.cfg.Logf("calposter: 已推图记录 %s 解不开，按已全局了结兜底: %v", key, err)
		}
		p.ledger[key] = &rec
		return nil
	})
}

// settled 报告该版本是否已全局了结（全员收到过，或历史上按"已处理"记过账）。
func (p *Poster) settled(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.ledger[key]
	return ok && !rec.SettledAt.IsZero()
}

// servedTo 报告该版本是否已发给指定目标（有回执，或整条已全局了结）。
func (p *Poster) servedTo(tgt, key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.ledger[key]
	if !ok {
		return false
	}
	if !rec.SettledAt.IsZero() {
		return true
	}
	_, ok = rec.Targets[tgt]
	return ok
}

// markTo 记"该目标已收到"；当前全部订阅目标都收到时收敛写全局了结。
// 幂等（覆盖时间戳）。磁盘在锁内以内存镜像为准整条写回，严格同序。
func (p *Poster) markTo(tgt, key string) {
	now := p.cfg.Now()
	p.mu.Lock()
	rec := p.ledger[key]
	if rec == nil {
		rec = &pushRec{Targets: map[string]time.Time{}}
		p.ledger[key] = rec
	}
	if rec.Targets == nil {
		rec.Targets = map[string]time.Time{}
	}
	rec.Targets[tgt] = now
	if rec.SettledAt.IsZero() {
		if targets := p.targets(); len(targets) > 0 {
			allServed := true
			for _, t := range targets {
				if _, ok := rec.Targets[t]; !ok {
					allServed = false
					break
				}
			}
			if allServed {
				rec.SettledAt = now
				p.cfg.Logf("calposter: 版本 %s 已发给全部 %d 个目标，记为已推", key, len(targets))
			}
		}
	}
	err := p.cfg.Doc.Put(NS, key, rec)
	p.mu.Unlock()
	if err != nil {
		p.cfg.Logf("calposter: 写已推图记录 %s 失败（重启后可能重发一次）: %v", key, err)
	}
}

// settle 把版本记为全局了结。无目标只记日志的轮次也必须落账，否则每轮都重排。
func (p *Poster) settle(key string) {
	now := p.cfg.Now()
	p.mu.Lock()
	rec := p.ledger[key]
	if rec == nil {
		rec = &pushRec{}
		p.ledger[key] = rec
	}
	rec.SettledAt = now
	err := p.cfg.Doc.Put(NS, key, rec)
	p.mu.Unlock()
	if err != nil {
		p.cfg.Logf("calposter: 写已推图记录 %s 失败（重启后可能重发一次）: %v", key, err)
	}
}
