// Package stellasora 是 annsync.Source 的第一个实现：《星塔旅人》官网公告。
//
// 官网是 SPA，但背后有公开无鉴权的 JSON 接口（同域 /api 代理）。本包只做两件事：
// 把接口形状变成 Go 类型（缺字段即报错）、把公告正文解析成带状态的时间区间。
// 站点知识全部留在这里，同步引擎不认识它。
package stellasora

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultBaseURL 是简体中文官网的接口根。路径从官网 JS 包里抠出来逐个打过：
// resource/news（列表）、resource/news/{id}（详情，路径参数）、resource/news/banner。
const DefaultBaseURL = "https://stellasora.yostar.cn"

// DefaultZone 是公告日期的默认时区：固定 +08:00，不用 time.Local
// （照 queue.fixedCST 的理由：手机端的本地时区与时区数据库都不可信）。
// 简体中文官网本身就是 +08 区的运营口径，公告里的时刻按本地读即可。
var DefaultZone = time.FixedZone("CST", 8*60*60)

// Config 的零值必须可用：BaseURL/HTTP/Type/ListSize/Zone 都有兜底。
type Config struct {
	BaseURL  string
	Zone     *time.Location
	HTTP     *http.Client
	Type     string // 列表栏目，默认 latest
	ListSize int    // 默认 40（实测全量 345 条，Source.List 会翻页取全）
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Zone == nil {
		c.Zone = DefaultZone
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if c.Type == "" {
		// notice 只有 328 条、activity 15 条，而真正的游戏活动（含大版本活动一览）
		// 两个栏目里都有；latest 实测与不带 type 参数同count，所以取它。
		c.Type = "latest"
	}
	if c.ListSize <= 0 {
		c.ListSize = 40
	}
	return c
}

// RateLimitError 对应 HTTP 429。繁中官网实测 ~25 个 detail 突发即触发；
// 简体中文官网 2026-09-05 测 30 连发全是 200（单请求 0.14s），没摸到限流，
// 但不能当它没有：真触发时同步引擎必须中断本轮并退避，而不是当成"这条公告坏了"跳过。
type RateLimitError struct {
	URL string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("stellasora: %s 返回 429（限流），本轮应中断并退避", e.URL)
}

// StatusError 是其他非 2xx 或业务 code 非零。4xx（除 429）通常意味着 id 不存在，
// 同步引擎应跳过该条而不是退避。
type StatusError struct {
	Status  int
	Code    int
	Message string
	URL     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("stellasora: %s -> HTTP %d code=%d %s", e.URL, e.Status, e.Code, e.Message)
}

// ListItem 是列表行。字段名照真实响应抄录，不许自造。
type ListItem struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Link        string `json:"link"`
	Type        string `json:"type"`
	TypeLabel   string `json:"typeLabel"`
	PublishTime int64  `json:"publishTime"` // 毫秒
	Thumbnail   string `json:"thumbnail"`
	Description string `json:"description"`
}

// NewsBody 是详情里的 news 对象。Content 是 HTML 富文本，日期只存在于其中。
type NewsBody struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	TypeLabel   string `json:"typeLabel"`
	PublishTime int64  `json:"publishTime"`
	Thumbnail   string `json:"thumbnail"`
	Content     string `json:"content"`
}

type Detail struct {
	News       NewsBody `json:"news"`
	PreNewsID  int64    `json:"preNewsId"`
	NextNewsID int64    `json:"nextNewsId"`
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type listData struct {
	Count int        `json:"count"`
	Rows  []ListItem `json:"rows"`
}

type Client struct {
	cfg Config
}

func NewClient(cfg Config) *Client { return &Client{cfg: cfg.withDefaults()} }

// ListNews 拉一页列表。index 是 1 基页码：实测 index=0 与 index=1 返回同一页，
// 越界页返回空 rows 而 count 不变（所以翻页结束的条件是"这页没新条目"，不是"页码到 count/size"）。
func (c *Client) ListNews(ctx context.Context, index int) ([]ListItem, int, error) {
	u := c.cfg.BaseURL + "/api/resource/news?" + url.Values{
		"index": {strconv.Itoa(index)},
		"size":  {strconv.Itoa(c.cfg.ListSize)},
		"type":  {c.cfg.Type},
	}.Encode()

	raw, err := c.get(ctx, u)
	if err != nil {
		return nil, 0, err
	}
	var d listData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, 0, fmt.Errorf("stellasora: 列表响应解码: %w", err)
	}
	for i, r := range d.Rows {
		if r.ID == 0 || r.Title == "" || r.PublishTime == 0 {
			return nil, 0, fmt.Errorf("stellasora: 列表第 %d 行形状不符: %+v", i, r)
		}
	}
	return d.Rows, d.Count, nil
}

// NewsDetail 拉单条详情。id 走路径参数：官网对 ?id= 的写法回 400
// （"id must be an integer number"，因为它把 query 里的 id 当成了别的东西）。
func (c *Client) NewsDetail(ctx context.Context, id int64) (Detail, error) {
	u := c.cfg.BaseURL + "/api/resource/news/" + strconv.FormatInt(id, 10)

	raw, err := c.get(ctx, u)
	if err != nil {
		return Detail{}, err
	}
	var d Detail
	if err := json.Unmarshal(raw, &d); err != nil {
		return Detail{}, fmt.Errorf("stellasora: 详情响应解码: %w", err)
	}
	// 形状自检：缺字段必须报错。上一版自造响应形状，导致测试全绿却漏掉整块空结构体。
	if d.News.ID == 0 || d.News.Title == "" || d.News.PublishTime == 0 {
		return Detail{}, fmt.Errorf("stellasora: 详情 %d 形状不符: %+v", id, d.News)
	}
	return d, nil
}

func (c *Client) get(ctx context.Context, u string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "xingta-bot/0.1 (+self-hosted; activity calendar)")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{URL: u}
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		if resp.StatusCode >= 400 {
			return nil, &StatusError{Status: resp.StatusCode, URL: u, Message: string(body)}
		}
		return nil, fmt.Errorf("stellasora: %s 响应非 JSON: %w", u, err)
	}
	if resp.StatusCode >= 400 || env.Code != 0 {
		return nil, &StatusError{Status: resp.StatusCode, Code: env.Code, Message: env.Message, URL: u}
	}
	return env.Data, nil
}
