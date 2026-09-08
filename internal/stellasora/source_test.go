package stellasora_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/stellasora"
)

// serveDetails 按 id 路由到对应夹具；没登记的 id 返回业务码错误（真接口就是这样）。
func serveDetails(t *testing.T, ids ...int64) *httptest.Server {
	t.Helper()
	files := map[string]map[string]any{}
	for _, id := range ids {
		files[strconv.FormatInt(id, 10)] = readFixture(t, "detail_"+strconv.FormatInt(id, 10)+".json")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resource/news/", func(w http.ResponseWriter, r *http.Request) {
		if d, ok := files[strings.TrimPrefix(r.URL.Path, "/api/resource/news/")]; ok {
			json.NewEncoder(w).Encode(d)
			return
		}
		w.Write([]byte(`{"code":100007,"message":"news not exist","data":null}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fetchItem(t *testing.T, url string, id int64) annsync.Item {
	t.Helper()
	src := stellasora.New(stellasora.Config{BaseURL: url})
	it, err := src.Fetch(context.Background(), annsync.Ref{ID: strconv.FormatInt(id, 10)})
	if err != nil {
		t.Fatalf("Fetch(%d): %v", id, err)
	}
	return it
}

func eventTitles(it annsync.Item) []string {
	out := make([]string, 0, len(it.Events))
	for _, e := range it.Events {
		out = append(out, e.Title)
	}
	return out
}

// 官网全量 345 条、每页 40 条：只取首页会漏掉八成的公告，
// 漏掉的那些如果还在进行中，日历上就永远没有它们。
func TestSourceListPaginatesWholeCatalog(t *testing.T) {
	rows := readFixture(t, "list.json")["data"].(map[string]any)["rows"].([]any)
	const pageSize = 4

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resource/news", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("size"); got != strconv.Itoa(pageSize) {
			t.Errorf("size 参数 = %q, want %d", got, pageSize)
		}
		idx, _ := strconv.Atoi(r.URL.Query().Get("index"))
		if idx < 1 {
			t.Errorf("index = %q, want >=1（官网页码是 1 基：index=0 会重复拿到第一页）", r.URL.Query().Get("index"))
		}
		atomic.AddInt32(&calls, 1)
		start := (idx - 1) * pageSize
		if start > len(rows) {
			start = len(rows)
		}
		end := min(start+pageSize, len(rows))
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{"count": len(rows), "rows": rows[start:end]},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	src := stellasora.New(stellasora.Config{BaseURL: srv.URL, ListSize: pageSize})
	refs, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != len(rows) {
		t.Errorf("取到 %d 条引用, want %d（翻页没取全）", len(refs), len(rows))
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("列表请求 %d 次, want 3（11 条 / 每页 4 条）", n)
	}
	if first := rows[0].(map[string]any); refs[0].ID != strconv.FormatInt(int64(first["id"].(float64)), 10) {
		t.Errorf("首条引用 = %s, want %v（冷启动要按站点给的顺序抓，而站点第一行是置顶公告，不一定是最新的）",
			refs[0].ID, first["id"])
	}

	seenID, seenHash := map[string]bool{}, map[[32]byte]string{}
	for _, r := range refs {
		if seenID[r.ID] {
			t.Errorf("引用 %s 重复出现", r.ID)
		}
		seenID[r.ID] = true
		if other, dup := seenHash[r.Hash]; dup {
			t.Errorf("引用 %s 与 %s 指纹相同：增量判定会把其中一条当没变", r.ID, other)
		}
		seenHash[r.Hash] = r.ID
	}
}

// 接口若无视 index（或像实测那样偶尔吐出一截断掉的 JSON），翻页必须自己刹住，
// 否则每轮同步都变成死循环。
func TestSourceListStopsWhenServerIgnoresIndex(t *testing.T) {
	rows := readFixture(t, "list.json")["data"].(map[string]any)["rows"].([]any)

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resource/news", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{"count": 999, "rows": rows}, // count 撒谎，且每页都一样
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	src := stellasora.New(stellasora.Config{BaseURL: srv.URL})
	refs, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != len(rows) {
		t.Errorf("取到 %d 条引用, want %d（重复页没去重）", len(refs), len(rows))
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("列表请求 %d 次, want 2（第二页没带来新条目就该停）", n)
	}
}

// 名字与海报都取自公告本身；正文只负责那一个主窗口。
func TestFetchFillsNamePosterURLFromPost(t *testing.T) {
	srv := serveDetails(t, 4506)
	it := fetchItem(t, srv.URL, 4506)
	fixture := readFixture(t, "detail_4506.json")
	news := fixture["data"].(map[string]any)["news"].(map[string]any)

	if len(it.Events) != 1 {
		t.Fatalf("事件数 = %d, want 1（%v）", len(it.Events), eventTitles(it))
	}
	got := it.Events[0]
	if got.Title != "缭乱碎晶球" {
		t.Errorf("活动名 = %q, want 缭乱碎晶球", got.Title)
	}
	if got.Label != "活动时间" {
		t.Errorf("Label = %q, want 活动时间（正文里还有活动奖励领取时间，那行不算活动）", got.Label)
	}
	if !got.ClaimStart.Equal(at("2026-09-01 12:00")) || !got.ClaimEnd.Equal(at("2026-09-11 03:59")) {
		t.Errorf("领奖窗 = %s ~ %s, want 09/01 12:00 ~ 09/11 03:59（活动奖励领取时间随事件带出）",
			got.ClaimStart, got.ClaimEnd)
	}
	if got.Status != annsync.StatusOK {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if want := news["thumbnail"].(string); got.Poster != want {
		t.Errorf("Poster = %q, want 该公告自己的封面 %q", got.Poster, want)
	}
	if want := srv.URL + "/news/4506"; got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
	if got.Fragment == "" {
		t.Error("Fragment 为空：待确认桶要靠它给人看原文")
	}
	if it.Title != strings.TrimRight(news["title"].(string), "\t") {
		t.Errorf("Item.Title = %q, want %q", it.Title, news["title"])
	}
	if it.Ref.ID != "4506" {
		t.Errorf("Ref.ID = %q, want 4506", it.Ref.ID)
	}
	wantPublished := time.UnixMilli(int64(news["publishTime"].(float64))).In(stellasora.DefaultZone)
	if !it.Published.Equal(wantPublished) {
		t.Errorf("Published = %s, want %s", it.Published, wantPublished)
	}
}

// 不是活动的公告一律产不出事件，而且不该报警——否则「待确认」会被
// 付费礼包、版本汇总、维护说明淹掉。
func TestFetchLeavesNonActivitiesEmptyAndQuiet(t *testing.T) {
	for _, c := range []struct {
		id   int64
		what string
	}{
		{4507, "整条只有售卖时间的付费礼包"},
		{3801, "正文只有长图的版本内容一览"},
		{4357, "方括号标题的版本一览"},
		{4490, "写了开放后常驻的内容更新"},
	} {
		t.Run(c.what, func(t *testing.T) {
			srv := serveDetails(t, c.id)
			it := fetchItem(t, srv.URL, c.id)
			if len(it.Events) != 0 {
				t.Errorf("产出了事件 %v, want 空", eventTitles(it))
			}
			if it.Suspect {
				t.Errorf("被判成待确认: %s", it.Note)
			}
		})
	}
}

// 「[X]活动一览」= 版本主活动自己的公告：Fetch 产出主活动事件，海报用它的封面
// （版本主视觉）。4356 是线上真夹具（欢歌劲浪活动一览）。
func TestFetchVersionOverviewEmitsMainEvent(t *testing.T) {
	srv := serveDetails(t, 4356)
	it := fetchItem(t, srv.URL, 4356)

	if len(it.Events) != 1 {
		t.Fatalf("事件数 = %d, want 1（%v）", len(it.Events), eventTitles(it))
	}
	got := it.Events[0]
	if got.Title != "欢歌劲浪·闪耀假日惊涛探险！" {
		t.Errorf("活动名 = %q, want 版本名", got.Title)
	}
	if got.Label != "活动时间" {
		t.Errorf("Label = %q, want 活动时间", got.Label)
	}
	if got.Status != annsync.StatusFuzzyStart {
		t.Errorf("Status = %q, want fuzzy_start（维护结束后）", got.Status)
	}
	if got.Provenance != stellasora.ProvVersion {
		t.Errorf("Provenance = %q, want version", got.Provenance)
	}
	if !got.ClaimStart.Equal(at("2026-08-18 00:00")) || !got.ClaimEnd.Equal(at("2026-09-08 10:59")) {
		t.Errorf("领奖窗 = %s ~ %s, want 08/18 00:00 ~ 09/08 10:59（活动商店与奖励兑换时间）",
			got.ClaimStart, got.ClaimEnd)
	}
	fixture := readFixture(t, "detail_4356.json")
	want := fixture["data"].(map[string]any)["news"].(map[string]any)["thumbnail"].(string)
	if got.Poster != want {
		t.Errorf("Poster = %q, want 版本主视觉 %q", got.Poster, want)
	}
	if it.Suspect {
		t.Errorf("被判成待确认: %s", it.Note)
	}
}

// 快照要留下：即便这条没产出事件，「待确认」也得能说出"我们看过这条公告"。
func TestFetchKeepsItemSnapshotForEveryRef(t *testing.T) {
	srv := serveDetails(t, 4507)
	it := fetchItem(t, srv.URL, 4507)
	if it.Title == "" || it.Thumbnail == "" || it.Published.IsZero() {
		t.Errorf("快照缺字段：%+v", it)
	}
}

// 多个主窗口的汇总公告：不采信，但要留下报警与说明。
// 线上一时没有这种形状，所以直接改写 4481 的正文来验接缝行为。
func TestFetchRoutesMultiWindowPostToReview(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resource/news/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"news": map[string]any{"id": 9001, "title": "「两期活动」活动说明",
				"publishTime": float64(pipelineNow.UnixMilli()), "thumbnail": "t.jpg",
				"content": `<p>▌活动时间</p><p>2026/09/01 12:00 ~ 2026/09/08 10:59</p>` +
					`<p>▌第二期时间</p><p>2026/09/10 12:00 ~ 2026/09/20 10:59</p>`},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	it := fetchItem(t, srv.URL, 9001)
	if len(it.Events) != 0 {
		t.Errorf("多个主窗口时不该采信任何一个: %v", eventTitles(it))
	}
	if !it.Suspect {
		t.Error("该进待确认")
	}
	if !strings.Contains(it.Note, "2 个主时间窗口") {
		t.Errorf("Note 没说明原因: %q", it.Note)
	}
}

// 429 在繁中站实测存在（简体中文站 30 连发没触发，但不能当它没有）。
// 引擎靠 Throttled/Permanent 决定"整轮退避"还是"跳过这条"，
// 翻译错了要么把限流当坏数据静默跳过，要么让一条 404 卡死整轮同步。
func TestFetchClassifiesErrorsForEngine(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantThrot bool
		wantPerm  bool
	}{
		{"429 限流", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}, true, false},
		{"500 服务器错", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, true, false},
		{"业务码非零", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"code":100007,"message":"news not exist","data":null}`))
		}, false, true}, // 这条取不到 ≠ 链路坏了：整轮退避会让冷启动永远卡在一条失效公告上
		{"404 没这条", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}, false, true},
		{"详情缺字段", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"code":0,"message":"ok","data":{"news":{"id":0,"title":"","publishTime":0}}}`))
		}, false, true},
		{"正文是一截断掉的 JSON", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"code":0,"message":"ok","data":{"news":{"id":1,"title":"x","publishTime":1,"content":"<p>`))
		}, false, true}, // 实测官网偶尔会吐这种：本轮少一条，下一轮补回来
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			t.Cleanup(srv.Close)

			_, err := stellasora.New(stellasora.Config{BaseURL: srv.URL}).
				Fetch(context.Background(), annsync.Ref{ID: "1"})
			if err == nil {
				t.Fatal("期望出错，实际成功")
			}
			var th annsync.Throttled
			var pm annsync.Permanent
			if got := errors.As(err, &th); got != c.wantThrot {
				t.Errorf("Throttled = %v, want %v (err=%v)", got, c.wantThrot, err)
			}
			if got := errors.As(err, &pm); got != c.wantPerm {
				t.Errorf("Permanent = %v, want %v (err=%v)", got, c.wantPerm, err)
			}
			var rl *stellasora.RateLimitError
			if c.name == "429 限流" && !errors.As(err, &rl) {
				t.Errorf("429 的原始类型丢了: %v", err)
			}
		})
	}
}

func TestFetchRejectsNonNumericRef(t *testing.T) {
	src := stellasora.New(stellasora.Config{BaseURL: "http://127.0.0.1:1"})
	_, err := src.Fetch(context.Background(), annsync.Ref{ID: "不是数字"})
	var pm annsync.Permanent
	if !errors.As(err, &pm) {
		t.Fatalf("非数字引用的错误 = %T %v, want annsync.Permanent", err, err)
	}
}

func TestListClassifiesRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	_, err := stellasora.New(stellasora.Config{BaseURL: srv.URL}).List(context.Background())
	var th annsync.Throttled
	if !errors.As(err, &th) {
		t.Fatalf("列表限流的错误 = %T %v, want annsync.Throttled", err, err)
	}
}

func TestSourceImplementsEngineInterfaces(t *testing.T) {
	src := stellasora.New(stellasora.Config{})
	var _ annsync.Source = src
	if src.Name() != "stellasora" {
		t.Errorf("Name = %q, want stellasora（它进 Rec.SourceID 与存储键前缀）", src.Name())
	}
}

// 默认配置必须指向现在真正在运营的那个站：换错过一次源，
// 同步会安静地抓到一堆繁体公告，日历看起来"正常"却没有一条对得上游戏里。
func TestDefaultsPointAtCNOfficialSite(t *testing.T) {
	cfg := stellasora.Config{}
	if stellasora.DefaultBaseURL != "https://stellasora.yostar.cn" {
		t.Errorf("DefaultBaseURL = %q", stellasora.DefaultBaseURL)
	}
	c := stellasora.NewClient(cfg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/resource/news" {
			t.Errorf("列表路径 = %q, want /api/resource/news", r.URL.Path)
		}
		if got := r.URL.Query().Get("type"); got == "activity" {
			t.Error("栏目还是 activity：简体官网的真实游戏活动在 notice 里，activity 只有 15 条运营帖")
		}
		w.Write([]byte(`{"code":0,"data":{"count":0,"rows":[]}}`))
	}))
	t.Cleanup(srv.Close)

	_, _, err := c.ListNews(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListNews: %v", err)
	}
}
