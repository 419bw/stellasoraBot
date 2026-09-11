package calposter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xingta/internal/annsync"
)

// 这一组钉的是"抓字节"那一步的策略：原地址优先、被拒才换候选地址，以及多张海报
// 并发抓取时计数要准（计数是拿去对账"这轮 CDN 到底拒了几张"的）。

func TestPosterFallsBackToCandidateHost(t *testing.T) {
	var primary, alt int32
	cd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primary, 1)
		http.Error(w, "reset by peer", http.StatusInternalServerError)
	}))
	defer cd.Close()
	ac := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&alt, 1)
		w.Write(bigArt())
	}))
	defer ac.Close()

	d := buildArt(t, cd, ac, 1)
	if got := atomic.LoadInt32(&primary); got != 1 {
		t.Errorf("原地址问了 %d 次, want 1 次", got)
	}
	if got := atomic.LoadInt32(&alt); got != 1 {
		t.Errorf("候选地址问了 %d 次, want 1 次", got)
	}
	if !strings.HasPrefix(recOf(t, d, "4540-0").Poster, "data:image") {
		t.Error("原地址失败、候选地址成功，海报却没进页面")
	}
}

// 顺序不能反：候选地址（OSS 传输加速）没有边缘缓存，每问一次都真的回一次源，
// 原地址能用时一个字节都不该多占。
func TestWorkingPosterNeverTouchesCandidateHost(t *testing.T) {
	var primary, alt int32
	cd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primary, 1)
		w.Write(bigArt())
	}))
	defer cd.Close()
	ac := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&alt, 1)
		w.Write(bigArt())
	}))
	defer ac.Close()

	d := buildArt(t, cd, ac, 1)
	if got := atomic.LoadInt32(&alt); got != 0 {
		t.Errorf("原地址明明能用，候选地址还是被问了 %d 次", got)
	}
	if !strings.HasPrefix(recOf(t, d, "4540-0").Poster, "data:image") {
		t.Error("海报没进页面")
	}
}

// 两边都不行才是真的没有：画占位，并且这轮算一次失败（下一轮要等冷却）。
func TestBothHostsFailingLeavesPlaceholder(t *testing.T) {
	var alt int32
	cd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer cd.Close()
	ac := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&alt, 1)
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer ac.Close()

	d := buildArt(t, cd, ac, 1)
	if got := recOf(t, d, "4540-0").Poster; got != "" {
		t.Errorf("两边都失败却留着海报字段: %q", got)
	}
	if got := atomic.LoadInt32(&alt); got != 1 {
		t.Errorf("候选地址问了 %d 次, want 1 次（只补一次，不再连锁重试）", got)
	}
}

// 多张海报是四个 worker 一起抓的：每张恰好一次，且收尾报的数要等于真实值。
// 这一条同时是计数竞态的门——计数不在锁里自增时，-race 会在这里报。
func TestManyPostersFetchOnceEachAndCountExactly(t *testing.T) {
	const n = 12
	var primary int32
	var alt int32
	cd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primary, 1)
		if strings.HasSuffix(r.URL.Path, "/bad.jpg") {
			http.Error(w, "reset", http.StatusInternalServerError)
			return
		}
		w.Write(bigArt())
	}))
	defer cd.Close()
	ac := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&alt, 1)
		w.Write(bigArt())
	}))
	defer ac.Close()

	var lines []string
	d := buildArtLog(t, cd, ac, n, &lines)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("4540-%d", i)
		if !strings.HasPrefix(recOf(t, d, id).Poster, "data:image") {
			t.Errorf("%s 的海报没进页面", id)
		}
	}
	// 最后一条是坏的那张，靠候选域补上：它也被内联了。
	if got := atomic.LoadInt32(&alt); got != 1 {
		t.Errorf("候选地址问了 %d 次, want 只有坏的那张 1 次", got)
	}
	if got := atomic.LoadInt32(&primary); got != int32(n) {
		t.Errorf("原地址问了 %d 次, want 每张一次共 %d", got, n)
	}
	joined := strings.Join(lines, "\n")
	want := fmt.Sprintf("本地图 0 张｜新抓 %d 张｜占位 0 张｜其中 1 张走的候选域", n)
	if !strings.Contains(joined, want) {
		t.Errorf("收尾日志对不上账:\n%s\nwant 含 %q", joined, want)
	}
}

func TestAcceleratedPosterOnlyRewritesPosterHost(t *testing.T) {
	got := acceleratedPoster("https://webusstatic.yo-star.com/web-cms-prod/upload/content/2026-06-23/x.jpeg")
	want := "https://webusstatic.oss-accelerate.aliyuncs.com/web-cms-prod/upload/content/2026-06-23/x.jpeg"
	if got != want {
		t.Errorf("改写出 %q, want %q", got, want)
	}
	// 只有 host 正好是那个海报域才改写：路径里长出同样的字样不算。
	for _, u := range []string{
		"https://stellasora.yostar.cn/api/resource/news",
		"https://webcnstatic.yostar.net/web-cms-prod/upload/a.jpg",
		"https://example.com/webusstatic.yo-star.com.de/x.jpg",
		"https://webusstatic.yo-star.com.evil.cn/x.jpg",
	} {
		if a := acceleratedPoster(u); a != "" {
			t.Errorf("不该改写 %q，改成了 %q", u, a)
		}
	}
}

// recOf 按 ID 取一条画布记录。版本窗口记录本身也在列表里，别按位置取。
func recOf(t *testing.T, d Dataset, id string) Record {
	t.Helper()
	for _, r := range d.Records {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("数据集里没有 %s 这条记录（共 %d 条）", id, len(d.Records))
	return Record{}
}

// buildArtLog 组一个"版本窗 + n 张海报"的数据集跑一遍 Build：海报原地址指向 cd，
// 候选地址指向 ac，最后一条的原地址故意会失败。日志按行收进 lines。
func buildArtLog(t *testing.T, cd, ac *httptest.Server, n int, lines *[]string) Dataset {
	t.Helper()
	recs := []annsync.Rec{
		verRec("4540", "奋斗吧", "2026-09-08 00:00", "2026-09-22 03:59", "2026-09-29 10:59"),
	}
	for i := 0; i < n; i++ {
		r := actRec(fmt.Sprintf("4540-%d", i), fmt.Sprintf("活动 %d", i+1),
			"2026-09-09 00:00", "2026-09-12 03:59")
		name := fmt.Sprintf("/p%d.jpg", i+1)
		if i == n-1 {
			name = "/bad.jpg" // 故意留一张原地址会失败的，看它走不走候选
		}
		r.Poster = cd.URL + name
		recs = append(recs, r)
	}
	d, err := Build(context.Background(), recs, Options{
		Zone: zone, OpenAt: 17 * time.Hour, Label: "version",
		Key: "202609071600", Now: at("2026-09-08 20:00"),
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Fetch:      true,
		Art:        NewArtCache("", 64),
		AltArt:     func(string) string { return ac.URL + "/alt.jpg" },
		Logf: func(format string, args ...any) {
			*lines = append(*lines, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func buildArt(t *testing.T, cd, ac *httptest.Server, n int) Dataset {
	t.Helper()
	var lines []string
	return buildArtLog(t, cd, ac, n, &lines)
}
