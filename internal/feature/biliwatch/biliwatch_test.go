package biliwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/schedule"
	"xingta/internal/kernel/target"
	"xingta/internal/store/storetest"
)

// mockCapturer 用于测试无头浏览器截图与计数
type mockCapturer struct {
	mu       sync.Mutex
	calls    atomic.Int32
	delay    time.Duration
	retBytes []byte
	lastPage []byte
}

func (m *mockCapturer) Capture(page []byte, route string) ([]byte, error) {
	m.calls.Add(1)
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	m.lastPage = page
	res := m.retBytes
	if len(res) == 0 {
		res = []byte("fake-png-data-" + fmt.Sprint(m.calls.Load()))
	}
	m.mu.Unlock()
	return res, nil
}

// mockFetcher 用于注入受控的 B站动态列表。
// Start 起的后台轮会并发读 items，测试改它必须走 setter 加锁——
// 与 calposter 测试的 recBox 同一条教训（直接递可变切片就是 -race 抓的现行）。
type mockFetcher struct {
	mu    sync.Mutex
	items []DynamicItem
	err   error
}

func (m *mockFetcher) FetchLatest(ctx context.Context, uid string) ([]DynamicItem, error) {
	m.mu.Lock()
	items, err := m.items, m.err
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (m *mockFetcher) setItems(items []DynamicItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = items
}

// mockKernelAPI 用于记录主题注册和消息投递
type mockKernelAPI struct {
	mu         sync.Mutex
	topics     []target.Topic
	submitted  []queue.Item
	targetList []string
}

func (m *mockKernelAPI) RegisterTopic(t target.Topic) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.topics = append(m.topics, t)
	return nil
}

func (m *mockKernelAPI) Topics() []target.Topic {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.topics
}

func (m *mockKernelAPI) TargetsFor(topicKey string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.targetList
}

func (m *mockKernelAPI) Submit(item queue.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.submitted = append(m.submitted, item)
	return nil
}

func (m *mockKernelAPI) items() []queue.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]queue.Item(nil), m.submitted...)
}

func (m *mockKernelAPI) clearSubmitted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.submitted = nil
}

func (m *mockKernelAPI) Schedule(id string, at time.Time, fn func(context.Context) error) {}

func (m *mockKernelAPI) Cancel(id string) bool { return true }

func (m *mockKernelAPI) Calendar() calendar.View { return nil }

func (m *mockKernelAPI) Scheduler() schedule.Scheduler { return nil }

func (m *mockKernelAPI) Targets() []string {
	return m.TargetsFor("bili")
}

func TestWbiSigning(t *testing.T) {
	imgKey := "7cd084941338484a827105b93b682852"
	subKey := "4932c493102447f8816041d33203235b"
	params := map[string]string{
		"foo":      "114",
		"bar":      "514",
		"special!": "hello world (test)",
	}

	fixedTime := int64(1700000000)
	query := SignWbi(params, imgKey, subKey, fixedTime)

	if query.Get("wts") != "1700000000" {
		t.Fatalf("wts 期望 1700000000，实际: %s", query.Get("wts"))
	}
	wRid := query.Get("w_rid")
	if len(wRid) != 32 {
		t.Fatalf("w_rid 长度应为 32 位 MD5，实际: %s", wRid)
	}

	// 验证相同输入输出严格幂等
	query2 := SignWbi(params, imgKey, subKey, fixedTime)
	if query2.Get("w_rid") != wRid {
		t.Errorf("两次签名不一致: %s vs %s", query2.Get("w_rid"), wRid)
	}
}

func TestBuildCardPageFormattingAndXSS(t *testing.T) {
	item := DynamicItem{
		IdStr: "10001",
		Type:  "DYNAMIC_TYPE_DRAW",
	}
	item.Modules.ModuleAuthor.Name = "星塔旅人"
	item.Modules.ModuleAuthor.Face = "https://example.com/face.jpg"
	item.Modules.ModuleAuthor.PubTime = "1小时前"
	item.Modules.ModuleDynamic.Desc = &struct {
		Text          string     `json:"text"`
		RichTextNodes []RichNode `json:"rich_text_nodes"`
	}{
		Text: "正文 <script>alert(1)</script>",
		RichTextNodes: []RichNode{
			{Type: "RICH_TEXT_NODE_TYPE_LOTTERY", Text: "抽奖活动"},
			{Type: "RICH_TEXT_NODE_TYPE_TOPIC", Text: "#星塔旅人#"},
			{Type: "RICH_TEXT_NODE_TYPE_AT", Text: "@迪西"},
			{Type: "RICH_TEXT_NODE_TYPE_WEB", Text: "https://bilibili.com"},
			{Type: "TEXT", Text: " <script>危险脚本</script>"},
		},
	}
	item.Modules.ModuleDynamic.Major = &struct {
		Type string `json:"type"`
		Opus *struct {
			Summary struct {
				Text          string     `json:"text"`
				RichTextNodes []RichNode `json:"rich_text_nodes"`
			} `json:"summary"`
			Pics []PicInfo `json:"pics"`
		} `json:"opus"`
		Draw *struct {
			Items []PicInfo `json:"items"`
		} `json:"draw"`
		Archive *struct {
			Bvid     string `json:"bvid"`
			Title    string `json:"title"`
			Cover    string `json:"cover"`
			Desc     string `json:"desc"`
			Duration string `json:"duration_text"`
			Stat     struct {
				Play    string `json:"play"`
				Danmaku string `json:"danmaku"`
			} `json:"stat"`
		} `json:"archive"`
	}{
		Draw: &struct {
			Items []PicInfo `json:"items"`
		}{
			Items: []PicInfo{
				{Url: "https://p1.jpg"},
				{Url: "https://p2.jpg"},
			},
		},
	}

	htmlBytes, err := BuildCardPage(item)
	if err != nil {
		t.Fatalf("BuildCardPage 报错: %v", err)
	}

	page := string(htmlBytes)
	// 验证富文本标记
	if !strings.Contains(page, "badge-lottery") || !strings.Contains(page, "tag-topic") {
		t.Errorf("页面缺少抽奖或话题标签: %s", page)
	}
	// 验证 XSS 转义：不应该包含未转义的 <script>危险脚本
	if strings.Contains(page, "<script>危险脚本</script>") {
		t.Errorf("未转义危险 XSS 内容: %s", page)
	}
	// 验证双图网格布局
	if !strings.Contains(page, "grid-pics-2") {
		t.Errorf("双图应采用 grid-pics-2 布局: %s", page)
	}
}

// TestSingleflightAndSingleSlotCacheConcurrency 验证极其核心的并发与单飞合并机制
// 模拟 20 个并发协程（多群推送同时到达）同时请求 Fetch 同一个动态 ID。
func TestSingleflightAndSingleSlotCacheConcurrency(t *testing.T) {
	doc := storetest.NewMem()
	cap := &mockCapturer{
		delay:    40 * time.Millisecond, // 模拟浏览器渲染耗时
		retBytes: []byte("render-result-dyn-001"),
	}

	feat := New(Config{
		Doc: doc,
		Cap: cap,
	})

	dynID := "dyn-001"
	// 注入动态数据以供出图
	feat.items[dynID] = DynamicItem{
		IdStr: dynID,
	}

	const goroutines = 20
	var startBarrier sync.WaitGroup
	startBarrier.Add(1)

	var doneBarrier sync.WaitGroup
	doneBarrier.Add(goroutines)

	results := make([][]byte, goroutines)
	errors := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		idx := i
		go func() {
			defer doneBarrier.Done()
			startBarrier.Wait() // 全部同时起跑，产生真实瞬时并发
			data, err := feat.Fetch(context.Background(), dynID)
			results[idx] = data
			errors[idx] = err
		}()
	}

	// 启动并发枪声
	startBarrier.Done()
	doneBarrier.Wait()

	// 1. 验证所有协程均成功获取结果
	for i := 0; i < goroutines; i++ {
		if errors[i] != nil {
			t.Fatalf("协程 %d 报错: %v", i, errors[i])
		}
		if !bytes.Equal(results[i], cap.retBytes) {
			t.Fatalf("协程 %d 获取到的字节不一致: got %s, want %s", i, string(results[i]), string(cap.retBytes))
		}
	}

	// 2. 验证底层浏览器被调用的次数严格等于 1！
	calls := cap.calls.Load()
	if calls != 1 {
		t.Fatalf("并发 20 个请求出图，底层浏览器预期只画 1 次，实际调用了 %d 次！", calls)
	}

	// 3. 验证后续单次/多次串行请求直接命中内存单槽（0ms），不再调用浏览器
	for i := 0; i < 5; i++ {
		data, err := feat.Fetch(context.Background(), dynID)
		if err != nil || !bytes.Equal(data, cap.retBytes) {
			t.Fatalf("串行命中测试失败: %v", err)
		}
	}
	if cap.calls.Load() != 1 {
		t.Fatalf("缓存命中期底层浏览器不应被重复调用，当前调用数: %d", cap.calls.Load())
	}

	// 4. 验证新动态到达时，覆盖旧动态，重新出图且调用数增 1
	newDynID := "dyn-002"
	feat.items[newDynID] = DynamicItem{IdStr: newDynID}
	cap.retBytes = []byte("render-result-dyn-002")

	newData, err := feat.Fetch(context.Background(), newDynID)
	if err != nil || !bytes.Equal(newData, []byte("render-result-dyn-002")) {
		t.Fatalf("新动态出图失败: %v", err)
	}
	if cap.calls.Load() != 2 {
		t.Fatalf("新动态预期使调用数递增为 2，实际为: %d", cap.calls.Load())
	}

	// 再次查新动态，继续命中单槽
	_, _ = feat.Fetch(context.Background(), newDynID)
	if cap.calls.Load() != 2 {
		t.Fatalf("新动态缓存未生效，调用数变为了: %d", cap.calls.Load())
	}
}

// TestColdBootProtection 验证首次冷启动防轰炸机制
func TestColdBootProtection(t *testing.T) {
	doc := storetest.NewMem()
	cap := &mockCapturer{}
	api := &mockKernelAPI{
		targetList: []string{"g:group_1", "g:group_2"},
	}

	fetcher := &mockFetcher{
		items: []DynamicItem{
			{IdStr: "dyn_103"},
			{IdStr: "dyn_102"},
			{IdStr: "dyn_101"},
		},
	}

	feat := New(Config{
		Doc:     doc,
		Cap:     cap,
		Fetcher: fetcher,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := feat.Start(ctx, api); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 等待首轮 round 跑完
	time.Sleep(50 * time.Millisecond)

	// 1. 验证自注册了 "bili" 主题
	topics := api.Topics()
	if len(topics) == 0 || topics[0].Key != "bili" {
		t.Fatalf("未成功自注册 bili 主题: %+v", topics)
	}

	// 2. 验证首次冷启动时，没有向队列投递任何一条历史旧动态！
	if len(api.submitted) != 0 {
		t.Fatalf("首次冷启动不应投递历史动态，实际投递了 %d 条！", len(api.submitted))
	}

	// 3. 验证最新的一条已被自动标记为基线已读
	if !feat.isPushed("dyn_103") {
		t.Errorf("最新动态 dyn_103 应被标记为已读基线")
	}

	// 4. 模拟官方发布了一条全新的动态 dyn_104
	fetcher.setItems([]DynamicItem{
		{IdStr: "dyn_104"},
		{IdStr: "dyn_103"},
		{IdStr: "dyn_102"},
	})

	// 手动触发一轮 round
	feat.round(ctx)

	// 此时应该向 2 个群投递 dyn_104
	if len(api.submitted) != 2 {
		t.Fatalf("新动态 dyn_104 应向 2 个群投递，实际投递数: %d", len(api.submitted))
	}
	for _, item := range api.submitted {
		if item.Media.Key != "dyn_104" || item.Media.Kind != "bili" {
			t.Errorf("投递内容不符: %+v", item)
		}
	}
}

func TestDynamicJSONCompatibility(t *testing.T) {
	// 真实 B站接口中 pub_ts 是字符串，图片 width/height 是数字或字符串，forward/comment/like count 可能是各种类型
	rawJSON := `{
		"code": 0,
		"message": "0",
		"data": {
			"has_more": true,
			"items": [
				{
					"id_str": "1234567890",
					"type": "DYNAMIC_TYPE_DRAW",
					"visible": true,
					"modules": {
						"module_author": {
							"mid": "3546645778139206",
							"name": "星塔旅人",
							"face": "https://face.jpg",
							"pub_time": "10分钟前",
							"pub_ts": "1726058400",
							"pub_action": "投稿了动态"
						},
						"module_dynamic": {
							"desc": {
								"text": "测试动态正文"
							},
							"major": {
								"type": "MAJOR_TYPE_DRAW",
								"draw": {
									"items": [
										{
											"url": "https://img.jpg",
											"width": "1920",
											"height": "1080",
											"size": 512.5
										}
									]
								}
							}
						},
						"module_stat": {
							"forward": { "count": "10" },
							"comment": { "count": 20 },
							"like": { "count": 30 }
						}
					}
				}
			]
		}
	}`

	var feed DynamicFeedResp
	if err := json.Unmarshal([]byte(rawJSON), &feed); err != nil {
		t.Fatalf("反序列化 B站动态 JSON 失败: %v", err)
	}
	if len(feed.Data.Items) != 1 {
		t.Fatalf("期望解析出 1 条动态，实际: %d", len(feed.Data.Items))
	}
	it := feed.Data.Items[0]
	if it.IdStr != "1234567890" {
		t.Errorf("id_str 不匹配: %s", it.IdStr)
	}

	// 验证可以正常渲染 HTML 卡片
	cardHTML, err := BuildCardPage(it)
	if err != nil {
		t.Fatalf("BuildCardPage 失败: %v", err)
	}
	if !strings.Contains(string(cardHTML), "测试动态正文") {
		t.Errorf("渲染页面未包含动态正文")
	}
	if !strings.Contains(string(cardHTML), "https://img.jpg") {
		t.Errorf("渲染页面未包含图片 URL")
	}
}

func TestMemoryBoundedEvictionAndDiskPersistence(t *testing.T) {
	doc := storetest.NewMem()
	api := &mockKernelAPI{targetList: []string{"group_100"}}
	capturer := &mockCapturer{}
	fetcher := &mockFetcher{}

	feat := New(Config{
		Doc:     doc,
		Cap:     capturer,
		Fetcher: fetcher,
	})

	ctx := context.Background()
	if err := feat.Start(ctx, api); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 1. 测试 f.items 内存上限裁剪（喂 40 条数据，上限 30 条）
	var manyItems []DynamicItem
	for i := 1; i <= 40; i++ {
		manyItems = append(manyItems, DynamicItem{IdStr: fmt.Sprintf("dyn_%d", i)})
	}
	fetcher.setItems(manyItems)
	feat.round(ctx)

	feat.mu.Lock()
	itemsCount := len(feat.items)
	feat.mu.Unlock()
	if itemsCount > maxCachedItems {
		t.Fatalf("f.items 数量超限: 期望 <= %d，实际 %d", maxCachedItems, itemsCount)
	}

	// 2. 测试账本内存 TTL 淘汰（模拟一条 35 天前已了结的动态）
	oldID := "dyn_ancient"
	oldTime := time.Now().Add(-35 * 24 * time.Hour)
	// 写入磁盘持久化
	_ = doc.Put(NS, oldID, pushRec{SettledAt: oldTime})

	feat.mu.Lock()
	feat.ledger[oldID] = &pushRec{SettledAt: oldTime}
	feat.mu.Unlock()

	// 跑一轮 round 触发内存账本记录淘汰
	feat.round(ctx)

	// 验证内存中已被淘汰（删除）
	feat.mu.Lock()
	_, inMem := feat.ledger[oldID]
	feat.mu.Unlock()
	if inMem {
		t.Fatalf("内存中的超期账本记录未能成功清理！")
	}

	// 3. 验证磁盘中持久化保留（磁盘不删），且 isPushed 能够从磁盘查回
	var diskRec pushRec
	foundOnDisk, err := doc.Get(NS, oldID, &diskRec)
	if err != nil || !foundOnDisk {
		t.Fatalf("磁盘中的历史持久化记录不应被删除！err: %v, found: %v", err, foundOnDisk)
	}
	if !feat.isPushed(oldID) {
		t.Fatalf("isPushed 应能从磁盘兜底查出已了结状态")
	}
}

// 多目标账必须记到"目标×动态"粒度：一个群先成功绝不再压住其他群——
// 没送到的那部分由下一轮对账补投，只投缺回执的目标，收齐后收敛全局了结。
func TestPartialDeliveryResumesOnlyMissingTarget(t *testing.T) {
	doc := storetest.NewMem()
	api := &mockKernelAPI{targetList: []string{"g:group_1", "g:group_2"}}
	fetcher := &mockFetcher{}
	feat := New(Config{Doc: doc, Cap: &mockCapturer{}, Fetcher: fetcher})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 预置一条历史基线绕过冷启动（否则首轮会把当前窗口全部设为基线）
	if err := doc.Put(NS, "dyn_old", pushRec{SettledAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("预置账本: %v", err)
	}
	if err := feat.Start(ctx, api); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // 等 Start 的首轮 round 跑完（此刻 fetcher 还是空的）
	api.clearSubmitted()

	// 官方发布 dyn_104：两个订阅目标各投一条
	fetcher.setItems([]DynamicItem{{IdStr: "dyn_104"}})
	feat.round(ctx)
	if got := len(api.items()); got != 2 {
		t.Fatalf("两个订阅目标该各投一条，实际 %d 条", got)
	}

	// 模拟队列：g:group_1 发送成功（回执），g:group_2 那条被丢弃（回执永远不来）
	for _, it := range api.items() {
		if it.Target == "g:group_1" && it.OnDelivered != nil {
			it.OnDelivered()
		}
	}

	// 下一轮对账：只给缺回执的 g:group_2 补投，g:group_1 不重收
	api.clearSubmitted()
	feat.round(ctx)
	items := api.items()
	if len(items) != 1 {
		t.Fatalf("只该给缺回执的目标补投 1 条，实际 %d 条：%v", len(items), items)
	}
	if items[0].Target != "g:group_2" {
		t.Errorf("补投目标 = %q，want g:group_2", items[0].Target)
	}

	// g:group_2 的回执到了 → 收敛全局了结 → 之后再无投递
	items[0].OnDelivered()
	api.clearSubmitted()
	feat.round(ctx)
	if got := len(api.items()); got != 0 {
		t.Errorf("全员收齐后不该再投，实际 %d 条", got)
	}
	var rec pushRec
	if ok, _ := doc.Get(NS, "dyn_104", &rec); !ok || rec.SettledAt.IsZero() {
		t.Error("全员收齐后没写全局了结账")
	}
}

// 订阅集收缩后（有人退订），"剩余目标全有回执"也要能收敛写全局了结——
// 免得这条动态在后续轮次里反复空扫，与"全员收齐才收敛"的既有语义保持一致。
func TestShrunkTargetSetSettlesLedger(t *testing.T) {
	doc := storetest.NewMem()
	api := &mockKernelAPI{targetList: []string{"g:group_1", "g:group_2"}}
	fetcher := &mockFetcher{}
	feat := New(Config{Doc: doc, Cap: &mockCapturer{}, Fetcher: fetcher})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := doc.Put(NS, "dyn_old", pushRec{SettledAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("预置账本: %v", err)
	}
	if err := feat.Start(ctx, api); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	api.clearSubmitted()

	fetcher.setItems([]DynamicItem{{IdStr: "dyn_104"}})
	feat.round(ctx)
	if got := len(api.items()); got != 2 {
		t.Fatalf("两个订阅目标该各投一条，实际 %d 条", got)
	}
	// 只有 g:group_1 的回执回来；随后 g:group_2 退订
	for _, it := range api.items() {
		if it.Target == "g:group_1" && it.OnDelivered != nil {
			it.OnDelivered()
		}
	}
	api.mu.Lock()
	api.targetList = []string{"g:group_1"}
	api.mu.Unlock()

	// 下一轮对账：剩余目标全有回执 → 收敛全局了结，且不再投递
	api.clearSubmitted()
	feat.round(ctx)
	if got := len(api.items()); got != 0 {
		t.Fatalf("订阅集收缩后不该再投，实际 %d 条", got)
	}
	var rec pushRec
	if ok, _ := doc.Get(NS, "dyn_104", &rec); !ok || rec.SettledAt.IsZero() {
		t.Error("订阅集收缩后没写全局了结账")
	}
}
