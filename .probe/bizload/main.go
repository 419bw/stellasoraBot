// 业务路径压测：从真实 bbolt 日历灌命令，走完整 hub.Handle → dispatch → calendar → reply。
// 不连真 QQ API，发送是 no-op 只计数。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/feature/calquery"
	"xingta/internal/kernel/calendar"
	"xingta/internal/qq"
	"xingta/internal/store"
)

type mockSender struct {
	mu    sync.Mutex
	count int
}

func (s *mockSender) SendGroupReply(_ context.Context, _, _ string, req qq.SendRequest) (*qq.SendResult, error) {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	_ = req.Content
	return &qq.SendResult{}, nil
}

func (s *mockSender) SendC2CReply(_ context.Context, _, _ string, req qq.SendRequest) (*qq.SendResult, error) {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	_ = req.Content
	return &qq.SendResult{}, nil
}

func main() {
	var (
		dbPath  = flag.String("db", "data/xingta-live.db", "bbolt 数据库路径")
		total   = flag.Int("total", 10000, "灌入命令条数")
		rate    = flag.Int("rate", 0, "目标速率 msg/s, 0=尽力灌")
		workers = flag.Int("workers", 0, "并发 goroutine 数, 0=NumCPU")
	)
	flag.Parse()

	if *workers <= 0 {
		*workers = runtime.NumCPU()
	}

	doc, err := store.OpenBolt(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer doc.Close()

	cal := populateCalendar(doc)
	fmt.Printf("日历: %d 条活动\n", cal.Len())

	reg := command.NewRegistry()
	zone := time.FixedZone("CST", 8*3600)
	status := annsync.NewStatusReader(doc, "stellasora")
	feature := calquery.New(reg, cal, calquery.Config{
		Zone:   zone,
		Status: status,
	})
	if err := feature.Start(context.Background(), nil); err != nil {
		fmt.Fprintln(os.Stderr, "注册命令失败:", err)
		os.Exit(1)
	}
	fmt.Printf("注册命令: %d 条\n", strings.Count(reg.HelpText(), "\n")+1)

	hub := qq.NewHub(nil, nil)
	sender := &mockSender{}
	command.Attach(reg, hub, sender, command.Config{})

	cmds := []string{"活动", "快结束", "即将", "帮助"}
	groupOpenID := strings.Repeat("A", 32)
	senderOpenID := strings.Repeat("B", 32)

	type sample struct{ lat time.Duration }
	var (
		mu      sync.Mutex
		samples []time.Duration
	)

	ids := make(chan string, 1024)
	go func() {
		defer close(ids)
		for i := range *total {
			ids <- fmt.Sprintf("load-%010d-%d", time.Now().UnixNano(), i)
		}
	}()

	var wg sync.WaitGroup
	var offered int
	var offMu sync.Mutex

	interval := time.Duration(0)
	if *rate > 0 {
		interval = time.Second / time.Duration(*rate)
	}

	start := time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			seq := 0
			for msgID := range ids {
				offMu.Lock()
				offered++
				n := offered
				offMu.Unlock()

				cmd := cmds[n%len(cmds)]
				content := " " + cmd + " "

				raw, _ := json.Marshal(map[string]any{
					"id":           msgID,
					"group_openid": groupOpenID,
					"content":      content,
					"timestamp":    time.Now().Format(time.RFC3339),
					"message_type": 0,
					"author": map[string]any{
						"id":           senderOpenID,
						"member_openid": senderOpenID,
						"member_role":   "member",
						"username":      fmt.Sprintf("压测用户%02d", id),
					},
					"message_scene": map[string]any{"source": "default"},
					"mentions":      []map[string]any{{"id": "ROBOT1.0_BOT", "username": "机器人", "bot": true}},
				})

				ev := qq.Event{Type: qq.EventGroupAtMessage, Data: raw}
				t0 := time.Now()
				_ = hub.Handle(context.Background(), ev)
				lat := time.Since(t0)

				mu.Lock()
				samples = append(samples, lat)
				mu.Unlock()

				seq++
				if interval > 0 {
					target := start.Add(time.Duration(n) * interval)
					if d := time.Until(target); d > 0 {
						time.Sleep(d)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	fmt.Printf("\n== 业务路径压测（命令派发 → 日历查询 → 格式化 → mock发送）==\n")
	fmt.Printf("环境: %s (%s/%s, %d 核, %s)\n", hostname(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	fmt.Printf("灌入 %d 条，并发 %d workers，目标速率 %s\n", offered, *workers, rateText(*rate))
	fmt.Printf("耗时 %v，吞吐 %.0f cmd/s\n", elapsed.Round(time.Millisecond), float64(offered)/elapsed.Seconds())

	if len(samples) > 0 {
		p := pct(samples)
		fmt.Printf("业务延迟 p50=%v p90=%v p99=%v p999=%v max=%v\n",
			p.p50.Round(time.Microsecond), p.p90.Round(time.Microsecond),
			p.p99.Round(time.Microsecond), p.p999.Round(time.Microsecond),
			p.max.Round(time.Millisecond))
	}

	sender.mu.Lock()
	replies := sender.count
	sender.mu.Unlock()
	fmt.Printf("回复发送: %d 条\n", replies)
	fmt.Printf("内存 HeapInUse=%dKB Sys=%dKB GC=%d goroutine=%d\n",
		ms.HeapInuse/1024, ms.Sys/1024, ms.NumGC, runtime.NumGoroutine())
}

type percentiles struct {
	p50, p90, p99, p999, max time.Duration
}

func pct(v []time.Duration) percentiles {
	at := func(q float64) time.Duration {
		i := int(q * float64(len(v)-1))
		return v[i]
	}
	return percentiles{p50: at(0.50), p90: at(0.90), p99: at(0.99), p999: at(0.999), max: v[len(v)-1]}
}

func rateText(n int) string {
	if n <= 0 {
		return "不限（尽力灌）"
	}
	return fmt.Sprintf("%d/s", n)
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "unknown"
	}
	return h
}

func populateCalendar(doc store.Doc) *calendar.Store {
	cal := calendar.NewStore()
	var recs []annsync.Rec
	_ = doc.Scan("activity", "", func(_ string, raw []byte) error {
		var r annsync.Rec
		if json.Unmarshal(raw, &r) == nil && r.InCalendar() {
			recs = append(recs, r)
		}
		return nil
	})

	type triple struct{ title, start, end string }
	seen := map[triple]bool{}
	var acts []calendar.Activity
	for _, r := range recs {
		k := triple{r.Title, r.Start.UTC().Format(time.RFC3339), r.End.UTC().Format(time.RFC3339)}
		if seen[k] {
			continue
		}
		seen[k] = true
		acts = append(acts, calendar.Activity{
			ID: r.ID, Game: r.SourceID, Title: r.Title,
			Start: r.Start, End: r.End, URL: r.URL,
		})
	}
	cal.BulkUpsert(acts)
	return cal
}
