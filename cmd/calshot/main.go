// Command calshot 用真库 + 真浏览器出一张版本日历图，给人眼核对与量耗时。
//
// 它不是机器人的一部分：不开队列、不连平台，只把"读库 → 组数据 → 画"这条链路
// 单独跑一遍。产出的 PNG 与机器人将来发的应当是同一张图——两边调的是同一个
// calposter.Poster，没有第二条实现可走。
//
// 用法（先跑过一轮同步，库里有数据）：
//
//	go run ./cmd/calshot                       # 当前版本
//	go run ./cmd/calshot -key 202608171600     # 指定版本键
//	go run ./cmd/calshot -noart                # 不下载海报，离线只看结构
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/feature/calposter"
	"xingta/internal/render"
	"xingta/internal/stellasora"
	"xingta/internal/store"
)

func main() {
	var (
		dbPath = flag.String("db", "data/xingta-live.db", "bbolt 数据库文件")
		key    = flag.String("key", "", "版本键（ActStart 的 UTC yyyyMMddHHmm），留空=当前版本")
		chrome = flag.String("chrome", "", "浏览器可执行文件；留空按常见安装位置找")
		outDir = flag.String("out", filepath.Join(os.TempDir(), "xingta-shot"), "输出目录（纯 ASCII）")
		noArt  = flag.Bool("noart", false, "不下载海报，海报位画斜纹占位")
		budget = flag.Int("budget", 0, "每趟给浏览器的虚拟时间上限（毫秒），0=用 render 的默认")
		repeat = flag.Int("repeat", 1, "连着要几次图：分开看 PNG 缓存与海报缓存各省了多少")
	)
	flag.Parse()

	logf := func(format string, args ...any) { fmt.Println(fmt.Sprintf(format, args...)) }

	doc, err := store.OpenBolt(*dbPath)
	if err != nil {
		fail("开库: %v", err)
	}
	defer doc.Close()

	bin, err := findBrowser(*chrome)
	if err != nil {
		fail("%v", err)
	}
	work := filepath.Join(os.TempDir(), "xingta-shot-work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		fail("建临时目录: %v", err)
	}

	var client *http.Client
	if !*noArt {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// 每轮把"现在"推前一分钟：同一分钟再要一次是 PNG 缓存命中（几乎 0 成本），
	// 推到下一分钟 PNG 过期，才看得出海报缓存有没有把重下载省掉。
	now := time.Now()
	ps := calposter.New(calposter.Config{
		Records: func() ([]annsync.Rec, error) { return annsync.ReadRecs(doc, stellasora.SourceName) },
		Label:   stellasora.ProvVersion,
		Client:  client,
		Now:     func() time.Time { return now },
		ArtDir:  filepath.Join(filepath.Dir(*dbPath), "art"),
		Cap:     &render.Browser{Bin: bin, WorkDir: work, Budget: *budget, Logf: logf},
		Logf:    logf,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	k := *key
	if k == "" {
		if k, err = ps.CurrentKey(); err != nil {
			fail("%v", err)
		}
		logf("当前版本键: %s", k)
	}
	var raw []byte
	for i := 0; i < *repeat; i++ {
		started := time.Now()
		var err error
		raw, err = ps.Image(ctx, k)
		if err != nil {
			fail("%v", err)
		}
		hits, builds := ps.Stats()
		logf("第 %d 次要图：%s（命中 %d / 真画 %d）", i+1, time.Since(started).Round(10*time.Millisecond), hits, builds)
		now = now.Add(time.Minute)
	}
	out := filepath.Join(*outDir, k+".png")
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		fail("建输出目录: %v", err)
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		fail("写文件: %v", err)
	}
	hits, builds := ps.Stats()
	logf("已写 %s（%d KB）｜缓存命中 %d 次，真画 %d 次", out, len(raw)/1024, hits, builds)
}

// findBrowser 只在开发工具里找浏览器：机器人本体由 main 的 -chrome 决定，
// 这里给几个常见位置是为了"clone 下来就能跑"。
func findBrowser(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	candidates := []string{
		"C:/Program Files/Google/Chrome/Application/chrome.exe",
		"C:/Program Files (x86)/Google/Chrome/Application/chrome.exe",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, name := range []string{"chrome", "chromium", "chromium-browser", "microsoft-edge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到浏览器，用 -chrome 指定可执行文件")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}
