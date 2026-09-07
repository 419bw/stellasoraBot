package stellasora_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"xingta/internal/stellasora"
)

func readFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读夹具 %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("夹具 %s 不是 JSON: %v", name, err)
	}
	return m
}

// serve 起一个像真官网的假站：列表在 /api/resource/news，详情在 /api/resource/news/{id}。
// id 走路径参数是实测结论：官网对 ?id= 的写法回 400，所以这里对 query 里的 id 直接报错。
func serve(t *testing.T, list map[string]any, wantID int64, detail map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resource/news", func(w http.ResponseWriter, r *http.Request) {
		if list == nil {
			t.Errorf("列表路由被请求，但本例没给列表响应")
			return
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/api/resource/news/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("详情带上了 query %q：官网对 ?id= 回 400", r.URL.RawQuery)
		}
		id := r.URL.Path[len("/api/resource/news/"):]
		if wantID > 0 {
			if got, _ := strconv.ParseInt(id, 10, 64); got != wantID {
				t.Errorf("详情路径里的 id = %q, 想要 %d（客户端传的要公告号必须原样到站）", id, wantID)
			}
		}
		if detail == nil {
			w.Write([]byte(`{"code":100007,"message":"news not exist","data":null}`))
			return
		}
		json.NewEncoder(w).Encode(detail)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func clientAt(t *testing.T, url string) *stellasora.Client {
	t.Helper()
	return stellasora.NewClient(stellasora.Config{BaseURL: url})
}

// 夹具是线上真响应。这条测试的意义是钉住"形状自检真的在读这些字段"：
// 一旦官网改字段名，这里必须红，而不是静默零值。
func TestListNewsReadsRealFixture(t *testing.T) {
	list := readFixture(t, "list.json")
	srv := serve(t, list, 0, nil)
	c := clientAt(t, srv.URL)

	rows, count, err := c.ListNews(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListNews: %v", err)
	}
	data := list["data"].(map[string]any)
	if count != int(data["count"].(float64)) {
		t.Errorf("count = %d, 想要 %v", count, data["count"])
	}
	want := data["rows"].([]any)[0].(map[string]any)
	if rows[0].ID != int64(want["id"].(float64)) || rows[0].Title != want["title"].(string) {
		t.Errorf("首行 = %+v, 想要 id=%v title=%v", rows[0], want["id"], want["title"])
	}
	if len(rows) != len(data["rows"].([]any)) {
		t.Errorf("行数 = %d, 想要 %d", len(rows), len(data["rows"].([]any)))
	}
}

func TestListNewsRejectsRowWithoutTitle(t *testing.T) {
	list := readFixture(t, "list.json")
	rows := list["data"].(map[string]any)["rows"].([]any)
	delete(rows[1].(map[string]any), "title")

	srv := serve(t, list, 0, nil)
	_, _, err := clientAt(t, srv.URL).ListNews(context.Background(), 1)
	if err == nil {
		t.Fatal("缺 title 的行被静默接受：形状自检失效")
	}
}

func TestNewsDetailReadsRealFixture(t *testing.T) {
	detail := readFixture(t, "detail_4506.json")
	srv := serve(t, nil, 4506, detail)
	c := clientAt(t, srv.URL)

	d, err := c.NewsDetail(context.Background(), 4506)
	if err != nil {
		t.Fatalf("NewsDetail: %v", err)
	}
	news := detail["data"].(map[string]any)["news"].(map[string]any)
	if d.News.ID != 4506 || d.News.Title != news["title"].(string) {
		t.Errorf("详情 = %+v", d.News)
	}
	if d.News.Content == "" {
		t.Error("Content 为空：日期全在里面，空了就没法解析")
	}
}

func TestNewsDetailRejectsMissingPublishTime(t *testing.T) {
	detail := readFixture(t, "detail_4506.json")
	delete(detail["data"].(map[string]any)["news"].(map[string]any), "publishTime")

	srv := serve(t, nil, 4506, detail)
	_, err := clientAt(t, srv.URL).NewsDetail(context.Background(), 4506)
	if err == nil {
		t.Fatal("缺 publishTime 被静默接受：形状自检失效")
	}
}

// 429 已实测存在（繁中官网 ~25 个 detail 突发即触发）。同步引擎靠这个类型决定
// "中断本轮并退避"，所以它不能被包成普通错误。
func TestRateLimitIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	_, err := clientAt(t, srv.URL).NewsDetail(context.Background(), 1)
	var rl *stellasora.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 的错误类型 = %T (%v), 想要 *RateLimitError", err, err)
	}
}

// 官网业务失败走 HTTP 200 + code 非零（和 QQ 平台一个德行），必须当错误。
func TestBusinessCodeNonZeroIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":100007,"message":"news not exist","data":null}`))
	}))
	t.Cleanup(srv.Close)

	_, err := clientAt(t, srv.URL).NewsDetail(context.Background(), 999999)
	var se *stellasora.StatusError
	if !errors.As(err, &se) || se.Code != 100007 {
		t.Fatalf("业务码错误的类型/码 = %T %v", err, err)
	}
}

func TestHTTP404IsStatusErrorNotRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	_, err := clientAt(t, srv.URL).NewsDetail(context.Background(), 1)
	var rl *stellasora.RateLimitError
	if errors.As(err, &rl) {
		t.Fatal("404 被当成限流：同步引擎会错误地整轮退避")
	}
	var se *stellasora.StatusError
	if !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("404 的错误 = %T %v", err, err)
	}
}
