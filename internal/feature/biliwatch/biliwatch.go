// Package biliwatch 实现 B站官方动态轮询与卡片出图推送。
package biliwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"xingta/internal/kernel"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/target"
	"xingta/internal/store"
)

const (
	// NS 是 biliwatch 在 store.Doc 中的命名空间。
	NS = "biliwatch"
	// DefaultUID 是《星塔旅人》官方账号 UID。
	DefaultUID = "3546645778139206"
	// maxCachedItems 是内存中暂存的动态元数据条目上限（避免无界增长）
	maxCachedItems = 30
	// pushedMemoryTTL 是内存中已推记录的保活窗口（超期从内存清理，磁盘 BoltDB 永久保留）
	pushedMemoryTTL = 30 * 24 * time.Hour
)

// Capturer 抽象无头浏览器截图执行，便于脱离真实 Chrome 单测。
type Capturer interface {
	Capture(page []byte, route string) ([]byte, error)
}

// Config 配置 biliwatch 功能。
type Config struct {
	Doc      store.Doc
	Cap      Capturer
	Fetcher  Fetcher
	UID      string
	Interval time.Duration
	Logf     func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.UID == "" {
		c.UID = DefaultUID
	}
	if c.Interval <= 0 {
		c.Interval = 3 * time.Minute
	}
	if c.Fetcher == nil {
		c.Fetcher = NewHTTPClient()
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

type call struct {
	wg  sync.WaitGroup
	out []byte
	err error
}

// Feature 实现 kernel.Feature 接口。
type Feature struct {
	doc     store.Doc
	cfg     Config
	fetcher Fetcher

	mu         sync.Mutex
	api        kernel.API
	hasHistory bool
	pushed     map[string]time.Time
	items      map[string]DynamicItem

	// 单图槽位缓存：内存中永远只存当前最新的一张动态图
	slotMu    sync.RWMutex
	latestID  string
	latestPNG []byte

	// 并发单飞合并：同 ID 并发出图只调一次底层浏览器
	flightMu sync.Mutex
	calls    map[string]*call
}

// New 构造 biliwatch 功能实例。
func New(cfg Config) *Feature {
	c := cfg.withDefaults()
	return &Feature{
		doc:     c.Doc,
		cfg:     c,
		fetcher: c.Fetcher,
		pushed:  make(map[string]time.Time),
		items:   make(map[string]DynamicItem),
		calls:   make(map[string]*call),
	}
}

func (f *Feature) Name() string { return "biliwatch" }

// Start 启动后台轮询主循环。
func (f *Feature) Start(ctx context.Context, api kernel.API) error {
	if f.doc == nil {
		return errors.New("biliwatch: doc 不能为空（需要账本记录已推送动态）")
	}
	if f.cfg.Cap == nil {
		return errors.New("biliwatch: Cap 不能为空（需要无头浏览器出图）")
	}
	if api == nil {
		return errors.New("biliwatch: api 不能为空（需要队列投递与目标查询）")
	}

	f.mu.Lock()
	f.api = api
	f.mu.Unlock()

	// 1. 自声明并注册推送主题
	_ = api.RegisterTopic(target.Topic{
		Key:  "bili",
		Name: "B站官方动态",
		Desc: "官方动态与更新资讯长图推送",
	})

	// 2. 加载历史已推送账本
	if err := f.loadPushed(); err != nil {
		return fmt.Errorf("biliwatch: 读取历史账本失败: %w", err)
	}

	// 3. 启动后台定时轮询 goroutine
	go f.loop(ctx)
	return nil
}

func (f *Feature) loadPushed() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	return f.doc.Scan(NS, "", func(key string, raw []byte) error {
		f.hasHistory = true
		var at time.Time
		if err := json.Unmarshal(raw, &at); err != nil {
			f.cfg.Logf("biliwatch: 历史记录 %s 解码失败，按当前时间兜底: %v", key, err)
			at = now
		}
		// 仅在 30 天保活窗口内的热记录载入内存活跃集，更早的持久化保留在磁盘不占内存
		if now.Sub(at) <= pushedMemoryTTL {
			f.pushed[key] = at
		}
		return nil
	})
}

func (f *Feature) isPushed(id string) bool {
	f.mu.Lock()
	_, ok := f.pushed[id]
	f.mu.Unlock()
	if ok {
		return true
	}

	// 内存未命中（可能因超期已被内存清理），查询磁盘永久账本兜底
	var at time.Time
	found, err := f.doc.Get(NS, id, &at)
	if err == nil && found {
		f.mu.Lock()
		f.pushed[id] = at // 回填内存活跃集
		f.mu.Unlock()
		return true
	}
	return false
}

// MarkPushed 记录已向群投递成功的动态 ID。由 activeSink 发送完成后回调。
func (f *Feature) MarkPushed(id string) {
	now := time.Now()
	f.mu.Lock()
	f.hasHistory = true
	f.pushed[id] = now
	f.mu.Unlock()

	// 磁盘持久化永久保存（不删磁盘）
	_ = f.doc.Put(NS, id, now)
}

func (f *Feature) loop(ctx context.Context) {
	f.round(ctx) // 启动时先跑一轮

	ticker := time.NewTicker(f.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.round(ctx)
		}
	}
}

// round 执行一轮动态轮询。
func (f *Feature) round(ctx context.Context) {
	items, err := f.fetcher.FetchLatest(ctx, f.cfg.UID)
	if err != nil {
		f.cfg.Logf("biliwatch: 抓取动态失败: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}

	f.mu.Lock()
	// 1. 缓存解析过的条目以便 Fetch 出图；限制内存中最多缓存 maxCachedItems 条（避免无界增长）
	for _, it := range items {
		f.items[it.IdStr] = it
	}
	if len(f.items) > maxCachedItems {
		cur := make(map[string]bool, len(items))
		for _, it := range items {
			cur[it.IdStr] = true
		}
		for id := range f.items {
			if !cur[id] {
				delete(f.items, id)
				if len(f.items) <= maxCachedItems {
					break
				}
			}
		}
		for id := range f.items {
			if len(f.items) <= maxCachedItems {
				break
			}
			delete(f.items, id)
		}
	}

	// 2. 内存已推账本定期清理：超过 30 天的记录从内存淘汰（磁盘永久保留）
	now := time.Now()
	for id, at := range f.pushed {
		if now.Sub(at) > pushedMemoryTTL {
			delete(f.pushed, id)
		}
	}

	// 3. 冷启动判定：若无论内存还是磁盘都完全没有历史记录，直接设为基线不轰炸群
	if !f.hasHistory {
		f.hasHistory = true
		f.mu.Unlock()
		for _, it := range items {
			f.MarkPushed(it.IdStr)
		}
		f.cfg.Logf("biliwatch: 首次冷启动，已将当前 %d 条历史动态设为推送基线，不向群补发历史动态", len(items))
		return
	}
	f.mu.Unlock()

	// 从较旧往较新检查未推动态（逆序遍历）：
	// B站返回列表 items[0] 为最新，倒序遍历可保证发布较早的动态先投递入队列。
	// 在 QQ 群自上而下的消息流中，较早的动态排在上方，最新的动态排在最下方，阅读体验符合直觉。
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if f.isPushed(it.IdStr) {
			continue
		}

		// 发现未推新动态
		targets := f.api.TargetsFor("bili")
		if len(targets) == 0 {
			f.cfg.Logf("biliwatch: 发现新动态 %s，但当前无任何群订阅 bili 主题，记为已处理", it.IdStr)
			f.MarkPushed(it.IdStr)
			continue
		}

		f.cfg.Logf("biliwatch: 发现新动态 %s，正在向 %d 个订阅目标投递推送", it.IdStr, len(targets))
		for _, tgt := range targets {
			item := queue.Item{
				ID:     "bili:" + it.IdStr + "@" + tgt,
				Target: tgt,
				Media:  &queue.Media{Kind: "bili", Key: it.IdStr},
				Topic:  "bili",
			}
			if err := f.api.Submit(item); err != nil {
				f.cfg.Logf("biliwatch: 投递动态 %s 到 %s 失败: %v", it.IdStr, tgt, err)
			}
		}
	}
}

// Fetch 供 activeSink 在真出站发送时获取图片字节。
// 结合了「单图槽位缓存」与「Singleflight 单飞合并」，保证并发多群只画一次，旧图用完自动回收。
func (f *Feature) Fetch(ctx context.Context, id string) ([]byte, error) {
	// 1. 优先查单图槽位（极速返回，0ms）
	f.slotMu.RLock()
	if f.latestID == id && len(f.latestPNG) > 0 {
		data := f.latestPNG
		f.slotMu.RUnlock()
		return data, nil
	}
	f.slotMu.RUnlock()

	// 2. 检查是否有并发单飞任务正在出此图（Singleflight 合并）
	f.flightMu.Lock()
	if c, ok := f.calls[id]; ok {
		// 已有在途出图任务：释放锁并等待它完成
		f.flightMu.Unlock()
		c.wg.Wait()
		return c.out, c.err
	}

	c := &call{}
	c.wg.Add(1)
	f.calls[id] = c
	f.flightMu.Unlock()

	defer func() {
		f.flightMu.Lock()
		delete(f.calls, id)
		if c.err == nil && len(c.out) > 0 {
			f.slotMu.Lock()
			f.latestID = id
			f.latestPNG = c.out
			f.slotMu.Unlock()
		}
		f.flightMu.Unlock()
		c.wg.Done()
	}()

	// 3. 真正调用浏览器执行渲染（即使 draw 内部 panic，defer 亦能确保 c.wg.Done() 广播唤醒并清除 calls 键）
	c.out, c.err = f.draw(ctx, id)
	return c.out, c.err
}

func (f *Feature) draw(ctx context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	it, ok := f.items[id]
	f.mu.Unlock()

	if !ok {
		// 备选：从最新列表再刷新一次
		items, err := f.fetcher.FetchLatest(ctx, f.cfg.UID)
		if err != nil {
			return nil, fmt.Errorf("biliwatch: 找不到动态 %s 且拉取列表失败: %w", id, err)
		}
		f.mu.Lock()
		for _, item := range items {
			f.items[item.IdStr] = item
			if item.IdStr == id {
				it = item
				ok = true
			}
		}
		f.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("biliwatch: 找不到动态 %s 的数据", id)
		}
	}

	page, err := BuildCardPage(it)
	if err != nil {
		return nil, fmt.Errorf("biliwatch: 生成卡片页面失败: %w", err)
	}

	raw, err := f.cfg.Cap.Capture(page, "")
	if err != nil {
		return nil, fmt.Errorf("biliwatch: 浏览器出图失败: %w", err)
	}
	return raw, nil
}
