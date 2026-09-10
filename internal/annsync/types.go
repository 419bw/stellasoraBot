// Package annsync 是通用同步引擎：把任意 Source 的条目增量拉回、持久化、
// 投影进活动日历。它不认识任何站点——站点知识全在 Source 实现里。
//
// 分层契约：本包是基础设施，对功能只暴露 Rec / Override / OverrideStore 这些
// 读面和人工覆盖的存储；功能（calquery/calexpiry/calops）不碰 Source，也不碰日历写侧。
package annsync

import (
	"context"
	"encoding/json"
	"time"

	"xingta/internal/kernel"
	"xingta/internal/kernel/calendar"
	"xingta/internal/store"
)

// Ref 是 Source 里一条内容的引用与列表级指纹。
// Hash 由 Source 自定（通常是标题+发布时间+封面的摘要），引擎靠它判断"这条变过没有"。
type Ref struct {
	ID   string
	Hash [32]byte
}

// Event 是一个子活动区间。Status 取 Source 的状态常量（ok / fuzzy_start / pending…）。
type Event struct {
	Title  string
	Label  string
	Start  time.Time
	End    time.Time
	Status string

	ClaimStart time.Time // 领奖/兑换窗口；零值 = 无。查询与提醒口径不使用，仅供展示
	ClaimEnd   time.Time

	Fragment   string // 原文片段，人工核对用
	Poster     string // 该子活动的海报 URL
	URL        string
	Provenance string // 源自述来源：solo=独立公告 / version=版本公告兜底
	TwinRef    string // 被合并的另一侧引用 ID
}

// Item 是 Source 里一条内容（通常=一条公告）及其解析出的全部子活动。
type Item struct {
	Ref       Ref
	Published time.Time
	Title     string
	Thumbnail string
	Events    []Event
	Suspect   bool
	Note      string
}

// Source 是同步引擎唯一认识的东西。
type Source interface {
	Name() string
	List(ctx context.Context) ([]Ref, error)
	Fetch(ctx context.Context, r Ref) (Item, error)
}

// Merger 是可选接缝：源可以在投影前对「全量条目」做跨条合并。
// 星塔源用它把版本公告与独立公告的重复子活动并成一行。
type Merger interface {
	Merge(all []Item) []Item
}

// Rec 是投影后的活动记录，也是功能包读活动数据的形状。
// pending 状态的 Rec 不进日历，但会留在存储里供「待确认」列出。
type Rec struct {
	ID       string // "n<refID>-<seq>"
	SourceID string // = Source.Name()
	RefID    string
	Title    string
	Label    string
	Start    time.Time
	End      time.Time
	Status   string
	Fragment string
	Poster   string
	URL      string

	ClaimStart time.Time // 领奖/兑换窗口；零值 = 无。查询与提醒口径不使用，仅供展示
	ClaimEnd   time.Time

	Provenance string
	TwinRef    string
	ParsedAt   time.Time
}

// InCalendar 报告这条记录是否应当出现在日历里。
func (r Rec) InCalendar() bool { return r.Status == StatusOK || r.Status == StatusFuzzyStart }

// 状态常量放在引擎里，Source 与功能共用同一套词。
const (
	StatusOK         = "ok"
	StatusFuzzyStart = "fuzzy_start"
	StatusPending    = "pending"
)

// Override 是人工覆盖：解析错了或官方改口时，运维用命令写进来。
// 刷新永不删除 override；投影时逐条应用。
type Override struct {
	Start, End time.Time
	Title      string
	Hide       bool
	Note       string
	By         string
	At         time.Time
}

// OverrideStore 是人工覆盖的存储面。基础设施提供实现，calops 功能提供命令入口。
type OverrideStore interface {
	Get(recID string) (Override, bool, error)
	All() (map[string]Override, error)
	Put(recID string, o Override) error
}

// NewOverrideStore 用 Doc 的 "override" 命名空间造一个 OverrideStore。
func NewOverrideStore(doc store.Doc) OverrideStore { return overrideStore{doc: doc} }

type overrideStore struct{ doc store.Doc }

const overrideNS = "override"

func (s overrideStore) Get(recID string) (Override, bool, error) {
	var o Override
	ok, err := s.doc.Get(overrideNS, recID, &o)
	return o, ok, err
}

func (s overrideStore) All() (map[string]Override, error) {
	out := map[string]Override{}
	err := s.doc.Scan(overrideNS, "", func(key string, raw []byte) error {
		var o Override
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		out[key] = o
		return nil
	})
	return out, err
}

func (s overrideStore) Put(recID string, o Override) error {
	return s.doc.Put(overrideNS, recID, o)
}

// Config 的零值必须可用。
type Config struct {
	Interval    time.Duration // 轮询间隔，默认 30m
	FullEvery   time.Duration // 全量校准间隔，默认 24h
	MinGap      time.Duration // 两次 Fetch 之间的最小间隔，默认 1s（限流实测换来的）
	BackoffBase time.Duration // 失败退避基数，默认 1m；第 n 次失败等 base×2^(n-1)，上限 max(2h, Interval)
	Retention   time.Duration // 公告保留期，默认 49 天（7周）；超过此时间的旧公告不进投影
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Minute
	}
	if c.FullEvery <= 0 {
		c.FullEvery = 24 * time.Hour
	}
	if c.MinGap <= 0 {
		c.MinGap = time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 49 * 24 * time.Hour
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

// Syncer 是同步引擎对外的形状：既是 kernel.Feature，也提供 Refresh
// 让运维功能（calops 的「覆盖」）写完人工覆盖后能立刻收敛日历，而不必等下一轮。
type Syncer interface {
	kernel.Feature

	// Refresh 请求立刻重投影：只读存储重算日历，不碰网络。非阻塞。
	Refresh()
}

// NewFeature 把引擎装成一个 kernel.Feature。Start 非阻塞：循环自己起 goroutine。
// src 若同时实现 Merger，投影前会对全量条目做一次跨条合并（星塔源用它并 twin）。
func NewFeature(doc store.Doc, cal calendar.Writer, src Source, ov OverrideStore, cfg Config) Syncer {
	if ov == nil {
		ov = NewOverrideStore(doc)
	}
	mg, _ := src.(Merger)
	return &syncer{
		doc: doc, cal: cal, src: src, mg: mg, ov: ov, cfg: cfg.withDefaults(),
		refresh: make(chan struct{}, 1),
	}
}
