package calposter

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"image"
	_ "image/jpeg" // 海报是 JPEG，取色要能解
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/render"
)

// 这个文件是"一张卡片要画什么"的判断：哪些记录进这个版本窗、哪些算周期玩法、
// 海报取不到时用什么底色。这些是业务口径，所以住在功能里；internal/render 只
// 认识 render.Dataset 这个形状，不认识版本、活动、周期。
//
// 选择一律在 time.Time 上做，格式化只在最后一步做：拿字符串比时刻看着省事，
// 但 "2026-09-08 00:00" 属于哪个时区得靠约定，一旦调用方的 now 与库里的时区不
// 同套，判据就悄悄错了。

// keyFmt 是版本身份的键：ActStart 的 UTC yyyyMMddHHmm。排序下标会被新版本挤左，
// 标题会被改写，只有开启时刻不动——出图文件名、缓存键、去重记录都用它。
const keyFmt = "200601021504"

// Options 描述一次出图的口径。零值不可用：Zone / Label / Now 都得调用方给。
type Options struct {
	Zone   *time.Location
	OpenAt time.Duration // fuzzy 起点与"版本算不算已开"用的开闸估计
	// Label 是哪个 Provenance 取值代表版本窗口公告。字面量由调用方传进来
	// （main 从 stellasora.ProvVersion 给），功能不认识 "version" 这个词。
	Label string
	Key   string // 要出图的版本键；空 = 按 Now 取当前版本
	Now   time.Time
	// HTTPClient 非空才下载海报并内联；nil 表示只出结构，海报位画斜纹占位。
	HTTPClient *http.Client
	// Fetch 表示这一趟允许现抓海报。默认 false：请求路径（用户点「日历」）只读
	// 本地字节，抓与重试归后台预热轮。海报 CDN 派到国内网络这些边缘上实测要么
	// 15KB/s、要么一发 TLS 握手就被断，把它放在用户等待的那一次里就是 44 秒起。
	Fetch bool
	// RetryAfter 是"这张海报上次没抓到，隔多久再试"。0 = 用默认 10 分钟。
	// 不设冷却就等于每轮都去捶一个正在拒我们的域名。
	RetryAfter time.Duration
	// Art 是海报字节的缓存，跨次出图复用（同一份数据过一分钟重画时不必再下一遍）。
	// nil = 每次都现取。
	Art *ArtCache
	// AltArt 是"原地址没拿到时换哪个地址再试一次"的策略，nil = 用 acceleratedPoster。
	// 存在的理由：海报域是 CDN 前置域，实测它轮换到的那批边缘节点会在我方发出
	// ClientHello 后立刻 RST（Go/curl/Chrome 三家一致），而同一批 key 在 OSS
	// 传输加速端点上能直读到、字节与 CDN 那份逐字节相同。它是兜底不是首选：
	// 加速端点没有边缘缓存，每一次都回源，走多了是在替对方多掏回源与加速流量。
	AltArt   func(url string) string
	Workers  int // 下载海报的并发，默认 4
	PerImage time.Duration
	Logf     func(format string, args ...any)
}

func (o Options) withDefaults() Options {
	if o.Zone == nil {
		o.Zone = time.FixedZone("CST", 8*3600)
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.PerImage <= 0 {
		o.PerImage = 20 * time.Second
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 10 * time.Minute
	}
	if o.AltArt == nil {
		o.AltArt = acceleratedPoster
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// VersionKey 把一条版本记录的时刻翻成稳定键。
func VersionKey(t time.Time) string { return t.UTC().Format(keyFmt) }

// Windows 从活动记录里挑出版本窗口。版本边界在解析阶段就被产成一条
// Provenance=Label 的记录（Start=玩法起, End=玩法止, ClaimEnd=兑换止），
// 所以这里不需要第二个数据源。返回值按开启时刻升序。
func Windows(recs []annsync.Rec, label string, zone *time.Location) []render.Window {
	if zone == nil {
		zone = time.Local
	}
	by := map[string]render.Window{}
	for _, r := range versionRecs(recs, label) {
		w := render.Window{
			Key:    VersionKey(r.Start),
			Name:   r.Title,
			Start:  r.Start.In(zone).Format(render.TimeLayout),
			End:    r.End.In(zone).Format(render.TimeLayout),
			Until:  r.ClaimEnd.In(zone).Format(render.TimeLayout),
			Source: r.RefID,
		}
		// 同一开启时刻被多篇公告声明时留兑换尾更长的那篇：窗口画宽点不伤人。
		if old, ok := by[w.Key]; !ok || w.Until > old.Until {
			by[w.Key] = w
		}
	}
	out := make([]render.Window, 0, len(by))
	for _, w := range by {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// Current 返回 now 时刻的当前版本：开闸时刻（act0 + OpenAt）已过里的最新一个。
//
// 不按"窗口包含今天"判：版本交接日新旧两窗重叠，那样会同时算出两个当前版本。
// 用开闸而不是 00:00：官方写"维护结束后开启"，00:00 只是下界，维护中就宣布
// "新版本已开"是错的。
func Current(recs []annsync.Rec, label string, now time.Time, openAt time.Duration) (annsync.Rec, bool) {
	var best annsync.Rec
	found := false
	for _, r := range versionRecs(recs, label) {
		if now.Before(r.Start.Add(openAt)) {
			continue
		}
		if !found || r.Start.After(best.Start) {
			best, found = r, true
		}
	}
	return best, found
}

func versionRecs(recs []annsync.Rec, label string) []annsync.Rec {
	if label == "" {
		return nil
	}
	var out []annsync.Rec
	for _, r := range recs {
		if r.Provenance == label && !r.Start.IsZero() && !r.ClaimEnd.IsZero() {
			out = append(out, r)
		}
	}
	return out
}

// Build 组装一次出图的全部输入。opt.Key 为空时按 opt.Now 取当前版本。
func Build(ctx context.Context, recs []annsync.Rec, opt Options) (render.Dataset, error) {
	opt = opt.withDefaults()

	var sel annsync.Rec
	if opt.Key == "" {
		r, ok := Current(recs, opt.Label, opt.Now, opt.OpenAt)
		if !ok {
			return render.Dataset{}, fmt.Errorf("calposter: %s 还没有任何版本开闸（%d 条记录里没有版本窗口）",
				opt.Now.Format(render.TimeLayout), len(recs))
		}
		sel = r
	} else {
		found := false
		for _, r := range versionRecs(recs, opt.Label) {
			if VersionKey(r.Start) == opt.Key {
				sel, found = r, true
				break
			}
		}
		if !found {
			return render.Dataset{}, fmt.Errorf("calposter: 没有版本键 %s（%d 条记录里没有这条版本窗口）",
				opt.Key, len(recs))
		}
	}

	win := render.Window{
		Key:    opt.Key,
		Name:   sel.Title,
		Start:  sel.Start.In(opt.Zone).Format(render.TimeLayout),
		End:    sel.End.In(opt.Zone).Format(render.TimeLayout),
		Until:  sel.ClaimEnd.In(opt.Zone).Format(render.TimeLayout),
		Source: sel.RefID,
	}
	if win.Key == "" {
		win.Key = VersionKey(sel.Start)
	}

	// 只带与本窗相交的记录。判据故意比模板松：下界用 act0 而不是开闸时刻，上界
	// 多给一周覆盖模板的"补齐整周"。多带的模板自己会丢掉，少带就是画面上真缺一条。
	hi := sel.ClaimEnd.Add(7 * 24 * time.Hour)
	monthly := detectMonthly(recs, opt.Zone)

	list := make([]render.Record, 0, 32)
	for _, r := range recs {
		if !r.InCalendar() || r.End.Before(sel.Start) || r.Start.After(hi) {
			continue
		}
		rec := render.Record{
			ID:    r.ID,
			Name:  r.Title,
			Label: r.Label,
			Start: r.Start.In(opt.Zone).Format(render.TimeLayout),
			End:   r.End.In(opt.Zone).Format(render.TimeLayout),
			Raw:   r.Fragment,
			Tint:  wash(r.Title),
			// 口径取值由模板解释（summary 走"无海报"那段说明文案），这里只照抄。
			Source: r.Provenance,
			Ref:    r.RefID,
		}
		// 海报 URL 只在确实要内联时带上：留着一个拉不到的 URL 在页面里，
		// 浏览器就会自己去等 CDN 超时（实测同一张图从 1s 变 15s），
		// 出图慢的原因于是从"取数"悄悄挪到"排版"，查起来看不见。
		if opt.HTTPClient != nil {
			rec.Poster = r.Poster
		}
		if r.Status == annsync.StatusFuzzyStart {
			rec.StartKind = "fuzzy"
		}
		if !r.ClaimEnd.IsZero() {
			rec.ClaimStart = r.ClaimStart.In(opt.Zone).Format(render.TimeLayout)
			rec.ClaimEnd = r.ClaimEnd.In(opt.Zone).Format(render.TimeLayout)
		}
		list = append(list, rec)
	}

	if opt.HTTPClient != nil {
		inlineArt(ctx, list, opt)
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].Start != list[j].Start {
			return list[i].Start < list[j].Start
		}
		return list[i].ID < list[j].ID
	})
	return render.Dataset{
		Now:       opt.Now.In(opt.Zone).Format(render.TimeLayout),
		OpenMs:    int64(opt.OpenAt / time.Millisecond),
		Records:   list,
		Windows:   []render.Window{win},
		Repeating: monthly,
	}, nil
}

// ArtCache 是海报字节的缓存：URL → 图片正文。抓失败的只记一个"上次失败时刻"，
// 供重试冷却用——不把它当成一种缓存值，否则一次抖动就变成永远不再试。
//
// 它跨的是"同一份数据、不同分钟"这一次重画：PNG 缓存按分钟失效，
// 但海报内容不会一分钟一变，重新下载 40 多秒纯属白给。
//
// dir 非空时同时落盘。这不是性能洁癖：海报图床是 CDN 前置域，能不能通取决于 DNS
// 当下给的那台边缘（实测有的 15KB/s、有的一发 TLS 握手就被断），只放内存等于每次重启赌一遍。
type ArtCache struct {
	mu       sync.Mutex
	m        map[string][]byte
	fail     map[string]time.Time // URL → 上次没抓到的时刻，用来做重试冷却
	inflight map[string]*artCall  // URL → 正在进行中的那次抓取
	cap      int
	dir      string
}

func NewArtCache(dir string, maxEntries int) *ArtCache {
	if maxEntries <= 0 {
		maxEntries = 256
	}
	return &ArtCache{m: map[string][]byte{}, fail: map[string]time.Time{}, cap: maxEntries, dir: dir}
}

// get 只认真正有字节的条目。失败不是一种"缓存值"——把它缓存下来就等于永远不再
// 试，而对方拒不拒我们是要换时间的（实测同一 key：一台边缘 15KB/s、另一台直接拒，
// 隔天还可能反过来）。
func (c *ArtCache) get(url string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.m[url]; ok && len(b) > 0 {
		return b, true
	}
	if c.dir == "" {
		return nil, false
	}
	b, err := os.ReadFile(c.fileOf(url))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	c.m[url] = b
	return b, true
}

// cooling 回答"这张上次没抓到，现在还不到再试的时候吗"。
func (c *ArtCache) cooling(url string, now time.Time, after time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.fail[url]
	return ok && now.Sub(at) < after
}

func (c *ArtCache) noteFail(url string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail == nil {
		c.fail = map[string]time.Time{}
	}
	c.fail[url] = now
}

func (c *ArtCache) noteOK(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.fail, url)
}

// artCall 是一次正在进行中的抓取，别人可以等它的结果而不是再发一遍。
type artCall struct {
	wg  sync.WaitGroup
	b   []byte
	err error
}

// begin 问"这个 URL 有人正在抓吗"。没人抓就把令牌给我（mine=true）；
// 有人抓就返回那份在飞的活，调用方等它。
//
// 这不是性能洁癖：后台预热轮和到点推图可能同时缺同一张图，各抓一次就是对着
// 一个正在拒我们的域名翻倍发请求。
func (c *ArtCache) begin(url string) (call *artCall, mine bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight == nil {
		c.inflight = map[string]*artCall{}
	}
	if busy, ok := c.inflight[url]; ok {
		return busy, false
	}
	call = &artCall{}
	call.wg.Add(1)
	c.inflight[url] = call
	return call, true
}

// end 交还令牌：成功的字节就地入账，失败的记一个时刻供冷却用。
func (c *ArtCache) end(url string, call *artCall, b []byte, err error) {
	c.mu.Lock()
	delete(c.inflight, url)
	c.mu.Unlock()
	call.b, call.err = b, err
	call.wg.Done()
}

func (c *ArtCache) put(url string, b []byte) {
	if len(b) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.m[url]; !dup && len(c.m) >= c.cap {
		// 到顶就整张清掉重开：海报随版本换代，留着三个版本前的图没有意义，
		// 而按 LRU 记账的复杂度在这里换不回什么。
		c.m = map[string][]byte{}
	}
	c.m[url] = b
	if c.dir == "" {
		return
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return // 落不了盘就只吃内存：出图不该因为缓存写不进去而失败
	}
	_ = os.WriteFile(c.fileOf(url), b, 0o600)
}

// fileOf 用 URL 的 sha1 当文件名：URL 里带路径与查询串，直接拼会跑到目录外面去。
func (c *ArtCache) fileOf(url string) string {
	s := sha1.Sum([]byte(url))
	return filepath.Join(c.dir, hex.EncodeToString(s[:])+".img")
}

// inlineArt 把本窗要画的海报转成 data URI 内联，并按海报本身取淡底色。
//
// 为什么内联而不是让浏览器去拉 URL：出图那一刻不该依赖 CDN 可达（机器人跑在
// 手机上），而且取色要拿到字节。
//
// 为什么默认不在这儿抓：图床是海外桶，一次整窗拉取实测 41 秒、被拒时是 16 个
// RST。发生在谁等回复的那一次上，就是几十秒的入口阻塞。所以请求路径只读本地，
// 抓与重试由后台预热轮（opt.Fetch=true）负责。真要抓时先问公告里给的原地址，
// 被拒了才换候选地址再试一次（见 fetchArt）。取不到图的记录清空 Poster，
// 模板画斜纹占位——这不是失败，维护公告切出来的条目本来就没有自己的海报。
func inlineArt(ctx context.Context, list []render.Record, opt Options) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done, miss, fetched, altN := 0, 0, 0, 0
	for w := 0; w < opt.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				url := list[i].Poster
				var b []byte
				fromCache := false
				var viaAlt bool
				var fail error
				if opt.Art != nil {
					if cached, ok := opt.Art.get(url); ok {
						b, fromCache = cached, true
					} else if opt.Fetch && !opt.Art.cooling(url, opt.Now, opt.RetryAfter) {
						if call, mine := opt.Art.begin(url); !mine {
							call.wg.Wait()
							b, fromCache, fail = call.b, call.err == nil, call.err
						} else {
							cctx, cancel := context.WithTimeout(ctx, opt.PerImage)
							got, alt, err := fetchArt(cctx, opt.HTTPClient, url, opt.AltArt)
							cancel()
							opt.Art.end(url, call, got, err)
							b, viaAlt, fail = got, alt, err
							switch {
							case err != nil:
								opt.Art.noteFail(url, opt.Now) // 下一轮要等冷却才再捶它
							default:
								opt.Art.put(url, b)
								opt.Art.noteOK(url)
							}
						}
					}
				}
				// 计数和写 list 都在 mu 里：四个 worker 同时 ++ 会互相吞掉，
				// 而这里报出去的数是要拿去对账的。
				mu.Lock()
				switch {
				case len(b) < 512:
					miss++
					list[i].Poster = ""
					switch {
					case fail != nil:
						opt.Logf("calposter: %s 的海报取不到，画占位并等冷却重试: %v", list[i].ID, fail)
					case !fromCache && len(b) > 0:
						opt.Logf("calposter: %s 的海报只有 %d 字节，当没有", list[i].ID, len(b))
					}
				default:
					done++
					if !fromCache {
						fetched++
						if viaAlt {
							altN++
						}
					}
					if t, ok := washFromImage(b); ok {
						list[i].Tint = t
					}
					list[i].Poster = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b)
				}
				mu.Unlock()
			}
		}()
	}
	for i := range list {
		if list[i].Poster != "" {
			jobs <- i
		}
	}
	close(jobs)
	wg.Wait()
	if fetched > 0 || miss > 0 {
		opt.Logf("calposter: 海报 本地图 %d 张｜新抓 %d 张｜占位 %d 张%s",
			done-fetched, fetched, miss, altNote(altN))
	}
}

// altNote 只在真用到兜底时说话：一轮里几张靠候选域补齐，说明 CDN 那条路此刻不通。
func altNote(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("｜其中 %d 张走的候选域", n)
}

// fetchArt 取一张海报：先按公告里给的原地址，没拿到才换候选地址再试一次。
//
// 顺序不能反：原地址是对方花钱放在边缘缓存上的，绝大多数访客走的就是它；
// 候选地址（OSS 传输加速）没有缓存，每问一次都真的回一次源。
func fetchArt(ctx context.Context, client *http.Client, url string, alt func(string) string) (b []byte, viaAlt bool, err error) {
	b, err = get(ctx, client, url)
	if err == nil && len(b) >= 512 {
		return b, false, nil
	}
	first := err
	if first == nil {
		first = fmt.Errorf("只有 %d 字节", len(b))
	}
	if alt == nil {
		return b, false, err
	}
	to := alt(url)
	if to == "" || to == url {
		return b, false, err
	}
	b2, err2 := get(ctx, client, to)
	switch {
	case err2 != nil:
		return nil, false, fmt.Errorf("%v（候选域也不行: %v）", first, err2)
	case len(b2) < 512:
		return nil, false, fmt.Errorf("%v（候选域只有 %d 字节）", first, len(b2))
	}
	return b2, true, nil
}

// posterCDNHost 是海报的 CDN 前置域；accelerateHost 是同一个桶的 OSS 传输加速端点。
const (
	posterCDNHost  = "webusstatic.yo-star.com"
	accelerateHost = "webusstatic.oss-accelerate.aliyuncs.com"
)

// acceleratedPoster 把海报 URL 换到 OSS 传输加速端点。只有 host 正好是那个海报域
// 才改写，其余一律返回空串表示"没有候选地址"——按子串换会把路径里长出同样字样的
// 地址也送到别的服务器上去。key 原样保留，实测两边同一 key 的字节 sha256 相同。
func acceleratedPoster(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != posterCDNHost {
		return ""
	}
	u.Host = accelerateHost
	return u.String()
}

func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// ---------- 淡底色：海报优先，没海报就按名字哈希 ----------

// wash 按名字哈希取一个淡色。同一个活动在任何一张图上颜色一致，跨图不会串味。
func wash(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	x := h.Sum32()
	return pastel(float64(x%360)/360.0, 0.34+float64((x>>9)%14)/100.0)
}

// washFromImage 取海报中心 60% 区域的平均色，压成淡彩。饱和度夹在 [.30,.52]：
// 再艳就抢文字的戏，再灰就看不出这条带是从哪张海报来的。
func washFromImage(b []byte) (string, bool) {
	im, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return "", false
	}
	bl := im.Bounds()
	w, h := bl.Dx()*6/10, bl.Dy()*6/10
	if w < 6 || h < 6 {
		return "", false
	}
	var sr, sg, sb float64
	for gy := 0; gy < 6; gy++ {
		for gx := 0; gx < 6; gx++ {
			x := bl.Min.X + (gx*2+1)*w/12
			y := bl.Min.Y + (gy*2+1)*h/12
			r, g, bb, _ := im.At(x, y).RGBA()
			sr += float64(r >> 8)
			sg += float64(g >> 8)
			sb += float64(bb >> 8)
		}
	}
	n := 36.0
	hh, ss, _ := rgb2hsl(sr/n/255, sg/n/255, sb/n/255)
	if ss < 0.30 {
		ss = 0.30
	}
	if ss > 0.52 {
		ss = 0.52
	}
	return pastel(hh, ss), true
}

func pastel(h, s float64) string {
	r, g, b := hsl2rgb(h, s, 0.885)
	return fmt.Sprintf("%02x%02x%02x", r, g, b)
}

func rgb2hsl(r, g, b float64) (float64, float64, float64) {
	mx, mn := maxOf(r, g, b), minOf(r, g, b)
	l := (mx + mn) / 2
	if mx == mn {
		return 0, 0, l
	}
	d := mx - mn
	s := d / (mx + mn)
	if l > 0.5 {
		s = d / (2 - mx - mn)
	}
	var h float64
	switch mx {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h / 6, s, l
}

func maxOf(a, b, c float64) float64 {
	if b > a {
		a = b
	}
	if c > a {
		a = c
	}
	return a
}

func minOf(a, b, c float64) float64 {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func hsl2rgb(h, s, l float64) (int, int, int) {
	q := l * (1 + s)
	if l >= 0.5 {
		q = l + s - l*s
	}
	p := 2*l - q
	f := func(t float64) float64 {
		if t < 0 {
			t += 1
		}
		if t > 1 {
			t -= 1
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 1.0/2:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return int(f(h+1.0/3)*255 + 0.5), int(f(h)*255 + 0.5), int(f(h-1.0/3)*255 + 0.5)
}

// ---------- 周期玩法判据 ----------

var punctRe = regexp.MustCompile(`[！!。，,、~\s]`)

func norm(s string) string {
	return strings.Trim(punctRe.ReplaceAllString(s, ""), "「」『』")
}

// detectMonthly 周期玩法判据：同名 ≥2 期，且满足任一——
//
//	a) 存在一期「长度 ≥20 天 + 起点落在 1 日 04:00」（联合讨伐/猎影合围/创业激励基金）；
//	b) 存在首尾相接的相邻两期（前一期的终点日 == 后一期的起点日）且两期都 ≥20 天。
//
// b 是给灾变防线补的：它每期压着版本日开，永远落不进 a 的"1 日"判据，但期与期
// 无缝衔接是周期玩法的本质特征。长度门槛挡掉 7 天期的紧急悬赏/缭乱碎晶球——
// 它们也反复开，但不是周期玩法。
//
// 判据要历史才成立（"反复开"看的是多期），所以输入是全量记录而不是本窗的。
func detectMonthly(recs []annsync.Rec, zone *time.Location) []string {
	if zone == nil {
		zone = time.Local
	}
	type seg struct {
		st    time.Time
		sDate string
		eDate string
		days  int64
	}
	m := map[string][]seg{}
	for _, r := range recs {
		if !r.InCalendar() || r.End.Before(r.Start) {
			continue
		}
		st := r.Start.In(zone)
		et := r.End.In(zone)
		days := int64(r.End.Sub(r.Start) / (24 * time.Hour))
		m[norm(r.Title)] = append(m[norm(r.Title)], seg{
			st:    st,
			sDate: st.Format("2006-01-02"),
			eDate: et.Format("2006-01-02"),
			days:  days,
		})
	}
	var out []string
	for k, ss := range m {
		if len(ss) < 2 {
			continue
		}
		big := false
		for _, g := range ss {
			if g.days >= 20 && g.st.Day() == 1 && g.st.Hour() == 4 {
				big = true
			}
		}
		if !big {
			for i, a := range ss {
				for j, b := range ss {
					if i != j && a.eDate == b.sDate && a.days >= 20 && b.days >= 20 {
						big = true
					}
				}
			}
		}
		if big {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
