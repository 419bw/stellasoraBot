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
	"xingta/internal/qq"
	"xingta/internal/render"
	"xingta/internal/store"
)

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
	Client *http.Client  // nil = 不下载海报（海报位画斜纹占位）
	Warm   time.Duration // 多久看一次数据有没有变，变了就去预热海报，默认 5m
	// Targets 是版本开启日主动推图的目标，形如 "g:<群 openid>" / "u:<用户 openid>"，
	// 前缀由发送侧解释。空 = 只记日志不发送。
	// ArtDir 非空时海报字节落盘：海报 CDN 会掐反复整窗拉取的客户端，
	// 只放内存等于每次重启都重新捶一遍。
	ArtDir   string
	Template []byte // nil = 用 render 内嵌的那一份模板
	Targets  []string
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
		c.Template = render.Template
	}
	return c
}

// Poster 出图并记住最近画过的那几张。
type Poster struct {
	cfg Config

	mu    sync.Mutex
	api   kernel.API // Start 之后才有：推图要排期与投递
	sent  map[string]time.Time
	art   *ArtCache        // 海报字节，跨分钟复用
	shots map[string]shot  // 版本键 → 当前数据下画好的那一张
	calls map[string]*call // 同一个键并发的取图请求合成一次画布
	hits  int
	built int
}

// shot 每个版本只留一份：数据变了指纹就变、时钟跨过 5 分钟桶也变，旧的直接作废。
// 版本总共个位数，不需要 LRU。
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
		cfg: cfg.withDefaults(), art: NewArtCache(cfg.ArtDir, 0), sent: map[string]time.Time{},
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
				now.In(p.cfg.Zone).Format(render.TimeLayout), len(recs))
		}
		key = VersionKey(r.Start)
	}
	// 画布与令牌共用桶起点：同一桶内画两次，字节一样，只画一次；红线一桶一跳，
	// 亚像素级，肉眼不可见。Current 判开闸仍用真 now，版本切换不受桶影响。
	bucket := now.Truncate(renderBucket)
	token := fp + "|" + bucket.In(p.cfg.Zone).Format(render.TimeLayout)

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

	c.out, c.err = p.draw(ctx, recs, key, bucket, fetch)

	p.mu.Lock()
	delete(p.calls, key)
	if c.err == nil {
		p.shots[key] = shot{token: token, data: c.out}
		p.built++
	}
	p.mu.Unlock()
	c.wg.Done()
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
	page, err := render.Page(d, p.cfg.Template)
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

// NS 是本功能在 store.Doc 里占的命名空间：键=版本键，值=推图时刻。
const NS = "calposter"

// pushGrace 是"开启时刻已经过了还补不补"的窗口。重启晚了 1 小时内照发一次；
// 更早的不补——这个功能第一次上线时，把历史上每个版本都推一遍是刷屏。
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

// round 每轮做两件事：把当前版本窗缺的海报补进缓存；把每个版本的推图排上期。
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
	p.warmArt(ctx, recs)
	p.armPushes(recs)
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

// armPushes 给每个"开启时刻还没到（或刚过不久）且没推过"的版本排一次期。
//
// 触发点用 act0 + 开闸估计，与"当前版本"同一条判据：00:00 只是维护中的下界，
// 那时候新版本既没开、海报也常常还没上传，推出去是一张全是占位图的图。
// Schedule 按 ID 幂等，所以每轮重排不会重复；已推过的直接跳过。
func (p *Poster) armPushes(recs []annsync.Rec) {
	now := p.cfg.Now()
	for _, w := range Windows(recs, p.cfg.Label, p.cfg.Zone) {
		t, err := time.ParseInLocation(render.TimeLayout, w.Start, p.cfg.Zone)
		if err != nil {
			continue
		}
		at := t.Add(p.cfg.OpenAt)
		if now.Sub(at) > pushGrace || p.done(w.Key) {
			continue
		}
		key := w.Key
		p.api.Schedule("poster:"+key, at, func(ctx context.Context) error { return p.push(ctx, key) })
	}
}

// push 到点了：只做一件事——把"该发哪一张"投进发送队列。
//
// 这里绝不出图：调度器是同步跑回调的（schedule.go 的 Fn 契约），画一趟就把同期
// 到期的提醒一起拖住了。渲染与上传都推到 Sink 真要发的那一刻——file_info 的 ttl
// 只有几分钟，在队列里排过一轮就可能已经过期。
//
// 账本也不在这里写：只有 Sink 报"发出去了"才算了结（MarkPushed）。画不出来或发送
// 失败时队列自己退避重试，最终放弃也不记账，下一轮 armPushes 会重新排上——宁可晚发，
// 也不要发一张空的或干脆不发还装作发过了。
func (p *Poster) push(_ context.Context, key string) error {
	// 以账本为准再判一次：armPushes 的"查已推 → 排期"不是原子的，排期循环可能
	// 刚越过那道检查、这个任务就触发了，于是它会被重新排上、被调度器立刻再跑一遍。
	if p.done(key) {
		return nil
	}
	targets := p.targets()
	if len(targets) == 0 {
		p.cfg.Logf("calposter: 没配推图目标，版本 %s 的图只记日志", key)
		p.markSent(key)
		return nil
	}
	var errs []error
	for _, target := range targets {
		item := queue.Item{
			ID:     "poster:" + key + "@" + target,
			Target: target,
			Media:  &queue.Media{Kind: "poster", Key: key},
			Topic:  "poster",
		}
		// 每个目标都试完再汇总：中途因为队列满返回，会漏掉后面的目标。
		if err := p.api.Submit(item); err != nil {
			errs = append(errs, fmt.Errorf("投版本 %s 到 %s: %w", key, target, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("calposter: %v", errors.Join(errs...))
	}
	return nil
}

func (p *Poster) targets() []string {
	if p.api != nil {
		if t := p.api.Targets(); len(t) > 0 {
			return t
		}
	}
	return p.cfg.Targets
}

// MarkPushed 由发送侧在"这条真的发出去了"之后调用：这是账本唯一的写入口。
// 队列对同一批目标会重试，所以这里要幂等（markSent 自己覆盖时间戳）。
func (p *Poster) MarkPushed(key string) {
	if key == "" || p.done(key) {
		return
	}
	p.markSent(key)
	p.cfg.Logf("calposter: 版本 %s 已发给目标，记为已推", key)
}

// loadSent 捞回上一次进程的推图记录。值解不开时按"已推过"处理，与 calexpiry
// 同一条取舍：键在就说明确实发过一次，重发只会刷屏。
func (p *Poster) loadSent() error {
	return p.cfg.Doc.Scan(NS, "", func(key string, raw []byte) error {
		var at time.Time
		if err := json.Unmarshal(raw, &at); err != nil {
			p.cfg.Logf("calposter: 已推图记录 %s 解不开，按已推处理: %v", key, err)
			p.sent[key] = time.Time{}
			return nil
		}
		p.sent[key] = at
		return nil
	})
}

func (p *Poster) done(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.sent[key]
	return ok
}

func (p *Poster) markSent(key string) {
	if err := p.cfg.Doc.Put(NS, key, p.cfg.Now()); err != nil {
		p.cfg.Logf("calposter: 写已推图记录 %s 失败（重启后可能重发一次）: %v", key, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent[key] = p.cfg.Now()
}
