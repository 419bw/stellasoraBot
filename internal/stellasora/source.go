package stellasora

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xingta/internal/annsync"
)

// SourceName 进 Rec.SourceID 与日历的 Game 字段，也是存储键的前缀。
const SourceName = "stellasora"

// maxListPages 是列表翻页的硬上限。全量实测 345 条 / 每页 40 条 = 9 页，
// 20 页足够覆盖到"这游戏出到 800 条公告"，再多就是接口在骗人。
const maxListPages = 20

// Source 把官网接口装成 annsync.Source：List 翻页取全量目录并算指纹，
// Fetch 拉详情、按规则 v2 从标题取活动名、从正文取那一个主时间窗口。
type Source struct {
	cl  *Client
	cfg Config
}

var _ annsync.Source = (*Source)(nil)

// New 造一个源。cfg 零值可用（默认简体中文官网、固定 +08:00、latest 栏目）。
func New(cfg Config) *Source {
	c := cfg.withDefaults()
	return &Source{cl: NewClient(c), cfg: c}
}

func (s *Source) Name() string { return SourceName }

// List 翻页拉全量目录。只看列表就能算指纹，所以增量轮次的开销是常数次请求。
func (s *Source) List(ctx context.Context) ([]annsync.Ref, error) {
	seen := make(map[int64]bool)
	var refs []annsync.Ref

	for page := 1; page <= maxListPages; page++ {
		rows, count, err := s.cl.ListNews(ctx, page)
		if err != nil {
			return nil, classify(err)
		}
		added := 0
		for _, r := range rows {
			if seen[r.ID] {
				continue // 置顶公告会在自己该在的位置再出现一次
			}
			seen[r.ID] = true
			added++
			refs = append(refs, annsync.Ref{ID: strconv.FormatInt(r.ID, 10), Hash: listHash(r)})
		}
		// 越界页返回空 rows 而 count 不变，所以终止条件是"这页没带来新条目"。
		if added == 0 || len(refs) >= count {
			break
		}
	}
	return refs, nil
}

// listHash 是自制 etag：接口不返回 ETag，标题/发布时间/封面任一变化就当内容变了。
// 盲区：正文改了而这三个字段没改，增量轮次会漏掉——FullEvery 的全量校准轮兜这个。
func listHash(r ListItem) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d|%s", r.ID, r.Title, r.PublishTime, r.Thumbnail)))
}

// Fetch 拉一条详情并解析。标题里没有「」的（维护说明、周边上新、版本预告）
// 一律产不出事件；正文里只有领取/售卖窗口的（付费礼包那种）同理。
func (s *Source) Fetch(ctx context.Context, r annsync.Ref) (annsync.Item, error) {
	id, err := strconv.ParseInt(r.ID, 10, 64)
	if err != nil {
		return annsync.Item{}, annsync.Permanent{Err: fmt.Errorf("stellasora: 引用 ID %q 不是数字: %w", r.ID, err)}
	}
	d, err := s.cl.NewsDetail(ctx, id)
	if err != nil {
		return annsync.Item{}, classify(err)
	}

	res := Parse(d.News, s.cfg.Zone)
	item := annsync.Item{
		Ref:       r,
		Published: time.UnixMilli(d.News.PublishTime).In(s.cfg.Zone),
		Title:     strings.TrimSpace(d.News.Title),
		Thumbnail: d.News.Thumbnail,
		Suspect:   res.Suspect(),
		Note:      res.Note(),
	}
	if len(res.Primary) != 1 {
		return item, nil // 0 个没档期，>1 个交人工，都不进日历
	}
	iv := res.Primary[0]
	item.Events = []annsync.Event{{
		Title:    res.Name,
		Label:    iv.Tag,
		Start:    iv.Start,
		End:      iv.End,
		Status:   iv.Status,
		Fragment: iv.Line,
		Poster:   d.News.Thumbnail,
		URL:      s.newsURL(d.News.ID),
	}}
	return item, nil
}

// newsURL 拼公告页地址。形状照列表接口真实返回的 link 字段核对过：<base>/news/<id>。
func (s *Source) newsURL(id int64) string {
	return s.cfg.BaseURL + "/news/" + strconv.FormatInt(id, 10)
}

// classify 把站点错误翻译成引擎语义。
//
//	限流(429)、5xx、网络错                        → Throttled：引擎中断本轮并退避
//	其他 4xx、业务码非零、正文形状不符、JSON 解不开 → Permanent：跳过这一条，本轮继续
//
// 业务码非零（HTTP 200 + code≠0，实测形状是 {"code":100007,"message":"news not exist"}）
// 归 Permanent 是权衡过的：它是"这一条取不到"，不是"链路坏了"。若当成 Throttled，
// 目录里只要有一条详情失效，冷启动就永远卡在那条上，后面的公告一条也进不来。
// 代价是站点若用业务码表达"服务忙"，会被逐条跳过而不是整轮退避——引擎每轮都会重试
// 并逐条记日志，不静默。
//
// 形状/解码错误同理按 Permanent 处理：官网改一次版不至于让同步彻底停摆，
// 而每轮重试的日志会一直喊，人不至于看不见。实测官网偶尔会吐出一截断掉的 JSON
// （index=9 那次），走的正是这条路：这一轮少一页，下一轮补回来。
func classify(err error) error {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return annsync.Throttled{Err: err}
	}
	var se *StatusError
	if errors.As(err, &se) {
		if se.Status >= 500 {
			return annsync.Throttled{Err: err}
		}
		return annsync.Permanent{Err: err} // 4xx 与业务码：这条不要了，本轮继续
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return annsync.Throttled{Err: err}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return annsync.Throttled{Err: err}
	}
	return annsync.Permanent{Err: err}
}
