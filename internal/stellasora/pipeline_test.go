package stellasora_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/kernel/calendar"
	"xingta/internal/stellasora"
	"xingta/internal/store/storetest"
)

// 端到端：真同步引擎 + 真源实现 + 线上真夹具（简体中文官网）。
// 单元测覆盖了各自内部，这里盯的是接缝——源产出的条目经引擎投影之后，
// 日历上到底躺着哪几行、名字对不对、有没有重复。

// pipelineIDs 是 testdata 里的全部十条公告：6 条应当进日历，
// 4 条（付费礼包、版本内容一览、版本一览、常驻内容）一条都不该进。
var pipelineIDs = []int64{4506, 4492, 4480, 4481, 4378, 4156, 4507, 4357, 3801, 4490}

// serveSite 起一个像真官网的假站：列表只含登记的 id，详情按 /api/resource/news/{id} 路由，
// 没登记的 id 返回真实的业务码错误形状。
func serveSite(t *testing.T, ids []int64) *httptest.Server {
	t.Helper()

	want := map[string]bool{}
	for _, id := range ids {
		want[strconv.FormatInt(id, 10)] = true
	}

	list := readFixture(t, "list.json")
	rows := list["data"].(map[string]any)["rows"].([]any)
	trimmed := make([]any, 0, len(ids))
	for _, row := range rows {
		m := row.(map[string]any)
		if want[strconv.FormatInt(int64(m["id"].(float64)), 10)] {
			trimmed = append(trimmed, m)
		}
	}
	if len(trimmed) != len(ids) {
		t.Fatalf("列表夹具里只找到 %d/%d 条公告", len(trimmed), len(ids))
	}
	listResp := map[string]any{
		"code": 0, "message": "ok",
		"data": map[string]any{"count": len(trimmed), "rows": trimmed},
	}

	details := map[string]map[string]any{}
	for _, id := range ids {
		details[strconv.FormatInt(id, 10)] = readFixture(t, "detail_"+strconv.FormatInt(id, 10)+".json")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/resource/news", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(listResp)
	})
	mux.HandleFunc("/api/resource/news/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/resource/news/"):]
		if d, ok := details[id]; ok {
			json.NewEncoder(w).Encode(d)
			return
		}
		w.Write([]byte(`{"code":100007,"message":"news not exist","data":null}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("超时：%s", what)
}

func titlesOf(list []calendar.Activity) map[string]int {
	out := map[string]int{}
	for _, a := range list {
		out[a.Title]++
	}
	return out
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func pendingTitles(list []annsync.Rec) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.Title+"("+r.Status+")")
	}
	return out
}

func engineConfig() annsync.Config {
	return annsync.Config{
		Interval:    10 * time.Millisecond,
		FullEvery:   time.Hour,
		MinGap:      time.Millisecond,
		BackoffBase: 5 * time.Millisecond,
	}
}

// pipelineNow 是判定"进行中"的钟点：2026-09-05 12:00 +08，
// 夹具里 6 条活动都还没结束，4507 那种付费窗口的结束时间反而更晚（09-30）——
// 单看时间分不清谁是活动，只能靠规则，所以这个钟点专门挑成这样。
var pipelineNow = time.Date(2026, 9, 5, 12, 0, 0, 0, stellasora.DefaultZone)

func TestEngineWithRealFixturesFillsCalendar(t *testing.T) {
	srv := serveSite(t, pipelineIDs)
	doc := storetest.NewMem()
	cal := calendar.NewStore()
	src := stellasora.New(stellasora.Config{BaseURL: srv.URL})

	eng := annsync.NewFeature(doc, cal, src, nil, engineConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Start(ctx, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	eventually(t, "日历灌满", func() bool { return len(cal.Active(pipelineNow)) >= 6 })

	got := titlesOf(cal.Active(pipelineNow))
	// ① 该进来的六条，一条不多一条不少。缭乱碎晶球只许有一行：
	//    它的正文里同时写着活动时间和"活动奖励领取时间"，旧实现会列成两行。
	for _, title := range []string{"缭乱碎晶球", "紧急悬赏", "攻略纪录大征集", "联合讨伐", "碧浪晕七彩", "出击海滨阵地！倾尽全力的碎瓜一击"} {
		if n := got[title]; n != 1 {
			t.Errorf("活动 %q 在日历上有 %d 行, want 1（现有 %v）", title, n, keysOf(got))
		}
	}
	if len(got) != 6 {
		t.Errorf("进行中活动 %d 种, want 6（%v）", len(got), keysOf(got))
	}

	// ② 付费礼包、版本汇总、常驻内容一律不进日历
	recs, err := annsync.ReadRecs(doc, "stellasora")
	if err != nil {
		t.Fatalf("ReadRecs: %v", err)
	}
	for _, bad := range []string{"创业激励基金", "枪林弹雨覆黄沙", "欢歌劲浪·闪耀假日惊涛探险！", "诺瓦异闻·新篇章"} {
		for _, r := range recs {
			if r.Title == bad {
				t.Errorf("%q 进了记录（%s）：它不是活动", bad, r.ID)
			}
		}
	}
	if len(recs) != 6 {
		t.Errorf("活动记录 %d 条, want 6（%v）", len(recs), pendingTitles(recs))
	}

	// ③ 真公告不该产生任何假报警：待确认桶为空
	pending, err := annsync.ReadPending(doc, "stellasora")
	if err != nil {
		t.Fatalf("ReadPending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("真公告产生了 %d 条待确认（解析器在假报警）：%v", len(pending), pendingTitles(pending))
	}
	items, err := annsync.ReadItems(doc, "stellasora")
	if err != nil {
		t.Fatalf("ReadItems: %v", err)
	}
	for _, it := range items {
		if it.Suspect {
			t.Errorf("公告 %s（%q）被标成待确认：%s", it.Ref.ID, it.Title, it.Note)
		}
	}

	// ④ 每条记录带着自己那条公告的海报与链接，人工点进去就能核对
	for _, r := range recs {
		if r.Poster == "" {
			t.Errorf("记录 %s（%q）没有海报", r.ID, r.Title)
		}
		if want := srv.URL + "/news/" + r.RefID; r.URL != want {
			t.Errorf("记录 %s 的 URL = %q, want %q", r.ID, r.URL, want)
		}
		if r.Fragment == "" {
			t.Errorf("记录 %s 没带原文片段", r.ID)
		}
		if r.SourceID != "stellasora" {
			t.Errorf("记录 %s 的 SourceID = %q", r.ID, r.SourceID)
		}
	}

	// ⑤ 同步状况可读：功能靠它判断"还在同步中"
	st, err := annsync.ReadStatus(doc, "stellasora")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.LastSuccess.IsZero() || st.Fails != 0 || st.Known != len(pipelineIDs) {
		t.Errorf("同步状况 = %+v, want LastSuccess 非零 / Fails 0 / Known %d", st, len(pipelineIDs))
	}
	for _, a := range cal.Active(pipelineNow) {
		if a.Game != "stellasora" {
			t.Errorf("日历条目 %s 的 Game = %q", a.ID, a.Game)
		}
	}
}

// 覆盖不丢：人工改过的时间在后续每一轮重投影里都得活着。
func TestEngineKeepsManualOverrideAcrossRounds(t *testing.T) {
	srv := serveSite(t, []int64{4506})
	doc := storetest.NewMem()
	cal := calendar.NewStore()
	src := stellasora.New(stellasora.Config{BaseURL: srv.URL})
	eng := annsync.NewFeature(doc, cal, src, nil, engineConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Start(ctx, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const id = "stellasora:4506:0"
	eventually(t, "活动进日历", func() bool { _, ok := cal.Get(id); return ok })

	newEnd := time.Date(2026, 9, 30, 23, 0, 0, 0, stellasora.DefaultZone)
	ov := annsync.NewOverrideStore(doc)
	if err := ov.Put(id, annsync.Override{End: newEnd, Note: "官方延期", By: "tester", At: pipelineNow}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	eng.Refresh()
	eventually(t, "改期生效", func() bool { a, ok := cal.Get(id); return ok && a.End.Equal(newEnd) })

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		a, ok := cal.Get(id)
		if !ok || !a.End.Equal(newEnd) {
			t.Fatalf("刷新把人工覆盖冲掉了：%+v (ok=%v)", a, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, ok, err := ov.Get(id)
	if err != nil || !ok || got.Note != "官方延期" {
		t.Errorf("覆盖记录读不回来：%+v ok=%v err=%v", got, ok, err)
	}
}

// 官方事后修正公告正文（同一个公告号、内容变了）：全量校准轮必须把新时间换上，
// 否则日历会一直停在第一次抓到的那份上。
func TestEngineRepicksUpEditedPost(t *testing.T) {
	doc := storetest.NewMem()
	cal := calendar.NewStore()

	// 假站：4506 的正文先给一个结束时间，之后换成更晚的。
	first := `活动时间：2026/09/01 12:00 ~ 2026/09/08 10:59`
	edited := `活动时间：2026/09/01 12:00 ~ 2026/09/22 10:59`
	var current = first
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resource/news", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"count": 1, "rows": []any{map[string]any{"id": 4506, "title": "「缭乱碎晶球」活动说明",
				"publishTime": float64(pipelineNow.UnixMilli()), "thumbnail": "t.jpg"}},
		}})
	})
	mux.HandleFunc("/api/resource/news/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"news": map[string]any{"id": 4506, "title": "「缭乱碎晶球」活动说明",
				"publishTime": float64(pipelineNow.UnixMilli()), "thumbnail": "t.jpg",
				"content": "<p>▌开放时间：</p><p>" + current + "</p>"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	eng := annsync.NewFeature(doc, cal, stellasora.New(stellasora.Config{BaseURL: srv.URL}), nil, annsync.Config{
		Interval: 10 * time.Millisecond, FullEvery: 50 * time.Millisecond,
		MinGap: time.Millisecond, BackoffBase: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Start(ctx, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const id = "stellasora:4506:0"
	eventually(t, "第一次的结束时间进日历", func() bool {
		a, ok := cal.Get(id)
		return ok && a.End.Equal(at("2026-09-08 10:59"))
	})

	current = edited
	// 列表指纹没变（标题/发布时间/封面都没动），只有全量校准重抓正文才会发现改过。
	eventually(t, "校准轮把改掉的时间换上", func() bool {
		a, ok := cal.Get(id)
		return ok && a.End.Equal(at("2026-09-22 10:59"))
	})
}
