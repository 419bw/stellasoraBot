package render

import (
	"bytes"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain 兼任假浏览器：真测试跑之前先看环境变量。假件吃掉所有参数、不调 m.Run，
// chrome 风格的未知 flag 就永远不会被 flag 包拒掉——这是 helper 形制能跑通的关键。
func TestMain(m *testing.M) {
	switch os.Getenv("XINGTA_RENDER_FAKE") {
	case "hang":
		// 卡死的浏览器：永不主动退，等真测试的 ctx 到点 kill。
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "leak":
		// 起一个继承 stdout 的子进程再退出：主进程死了、管道被孙进程攥着，
		// 复现 Chrome 渲染子进程残留、Output() 等不到 EOF 的经典挂法。
		// 孙进程执行的是自身副本：Windows 上运行中的 exe 文件删不掉，若直接
		// 执行测试二进制，会把 go test 收尾清理拖成假红（PASS 却 exit 1）。
		stale, _ := filepath.Glob(filepath.Join(os.TempDir(), "xingta-render-fake-*.exe"))
		for _, f := range stale {
			os.Remove(f) // 运行中的那份删不掉（被锁），上一轮的死副本顺势清走
		}
		dst := filepath.Join(os.TempDir(), fmt.Sprintf("xingta-render-fake-%d.exe", os.Getpid()))
		if b, err := os.ReadFile(os.Args[0]); err == nil && os.WriteFile(dst, b, 0o755) == nil {
			c := exec.Command(dst)
			c.Env = append(os.Environ(), "XINGTA_RENDER_FAKE=hang")
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if c.Start() == nil {
				c.Process.Release()
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestURLOfEscapesNonASCII(t *testing.T) {
	got := urlOf(filepath.Join(string(os.PathSeparator)+"tmp", "星塔", "cal.html"))
	if !strings.HasPrefix(got, "file:///") {
		t.Errorf("URL 前缀不对: %s", got)
	}
	if strings.Contains(got, "星塔") {
		t.Errorf("非 ASCII 段没编码: %s", got)
	}
	if !strings.Contains(got, "%E6%98%9F%E5%A1%94") {
		t.Errorf("「星塔」的 UTF-8 百分号编码不对: %s", got)
	}
	// 盘符里的冒号必须留着，否则 Chrome 解不出文件。
	// 只在 Windows 上验：Linux 的 filepath.Abs 把反斜杠当普通字符，路径根本不会被认成盘符。
	if runtime.GOOS == "windows" {
		if win := urlOf(`J:\学习\cal.html`); !strings.Contains(win, "/J:/") {
			t.Errorf("Windows 路径没变成 file:///J:/…: %s", win)
		}
	}
}

func TestParseTitle(t *testing.T) {
	w, h, err := parseTitle([]byte("<html><title>1689x950</title>"))
	if err != nil || w != 1689 || h != 950 {
		t.Errorf("解析 = %d,%d,%v 想 1689,950,nil", w, h, err)
	}
	for _, bad := range []string{"<html><title></title>", "<html>", "<title>12x9</title>"} {
		if _, _, err := parseTitle([]byte(bad)); err == nil {
			t.Errorf("畸形尺寸没报错: %q", bad)
		}
	}
}

func TestArgBuilders(t *testing.T) {
	b := &Browser{Bin: "chrome", Budget: 5000}
	m := strings.Join(b.measureArgs("/tmp/a.html", "#x2026"), " ")
	if !strings.Contains(m, "--dump-dom") || !strings.Contains(m, "--virtual-time-budget=5000") ||
		!strings.Contains(m, "file:///") || !strings.HasSuffix(m, "#x2026") {
		t.Errorf("量尺寸那趟参数不对: %s", m)
	}
	c := strings.Join(b.captureArgs("/tmp/a.html", "#x2026", 1600, 900, "/tmp/s.png"), " ")
	// 视口 = 卡片 + 四周留白；截图与锚点都要带上。
	if !strings.Contains(c, "--window-size=1624,924") || !strings.Contains(c, "--screenshot=/tmp/s.png") ||
		!strings.HasSuffix(c, "#x2026") {
		t.Errorf("截图那趟参数不对: %s", c)
	}
	if strings.Contains(c, "--dump-dom") {
		t.Error("截图那趟不该带 --dump-dom")
	}
	if got := (&Browser{}).budget(); got != 8000 {
		t.Errorf("默认预算 = %d, 想 8000（要等远程配图）", got)
	}
}

func TestCaptureValidatesInput(t *testing.T) {
	if _, err := (&Browser{}).Capture([]byte("x"), "#x"); err == nil {
		t.Error("没给浏览器路径却成功了")
	}
}

func TestTimeoutDefault(t *testing.T) {
	if got := (&Browser{}).timeout(); got != 60*time.Second {
		t.Errorf("默认超时 = %s, 想 60s（夹在合法最坏 44s 与判死阈值 82.5s 之间）", got)
	}
	if got := (&Browser{Timeout: -1}).timeout(); got != 60*time.Second {
		t.Errorf("负值没回落默认: %s", got)
	}
	if got := (&Browser{Timeout: 250 * time.Millisecond}).timeout(); got != 250*time.Millisecond {
		t.Errorf("正值被改写: %s", got)
	}
}

// 卡死的浏览器（hang 假件）必须在 Timeout 内被杀回来，而不是永远阻塞。
// 变异负对照：command() 退回 exec.Command（不带 ctx）后，本测试由看门狗先判红。
func TestCaptureTimesOutHangingBrowser(t *testing.T) {
	b := &Browser{Bin: os.Args[0], WorkDir: t.TempDir(),
		Env: []string{"XINGTA_RENDER_FAKE=hang"}, Timeout: 300 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := b.Capture([]byte("<html><title>400x200</title></html>"), "")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("假浏览器卡死却成功了")
		}
		if !strings.Contains(err.Error(), "超时") {
			t.Fatalf("错误没说超时: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("卡死的浏览器没能在超时内返回（CommandContext 被退回 Command 了？）")
	}
}

// 主进程退出后孙进程攥着输出管道（Chrome 渲染子进程残局）时，WaitDelay 必须把
// Output() 从等 EOF 里解放出来。变异负对照：摘掉 WaitDelay 后本测试要等满
// 30s 假件时长，看门狗先判红。
func TestCaptureReturnsWhenGrandchildHoldsPipe(t *testing.T) {
	b := &Browser{Bin: os.Args[0], WorkDir: t.TempDir(),
		Env: []string{"XINGTA_RENDER_FAKE=leak"}, Timeout: time.Minute}
	done := make(chan error, 1)
	go func() {
		_, err := b.Capture([]byte("<html><title>400x200</title></html>"), "")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("管道被攥住却成功了")
		}
		if !strings.Contains(err.Error(), "管道") {
			t.Fatalf("错误没说管道未收尾: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("WaitDelay 没生效：主进程死后还在等孙进程松手（等满 30s 假件时长）")
	}
}

// chromeBin 找一台机器上的浏览器；找不到就跳过真截图（单元测试不依赖它）。
func chromeBin(t *testing.T) string {
	for _, p := range []string{os.Getenv("XINGTA_CHROME"),
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	t.Skip("没有找到浏览器，跳过真截图")
	return ""
}

// 真跑一遍：最小自报尺寸 HTML → 浏览器 → PNG。
func TestCaptureWithRealBrowser(t *testing.T) {
	page := []byte("<!DOCTYPE html><html><head><title>400x200</title></head><body><div style=\"width:400px;height:200px;background:#f00;\">Hello</div></body></html>")
	bin := chromeBin(t)
	dir := t.TempDir()
	b := &Browser{Bin: bin, WorkDir: dir}
	raw, err := b.Capture(page, "")
	if err != nil {
		t.Fatal(err)
	}
	im, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("出来的不是能解的图: %v", err)
	}
	got := im.Bounds()
	if got.Dx() != 400+StagePad || got.Dy() != 200+StagePad {
		t.Errorf("截图 %dx%d, 想 %dx%d（自报尺寸 + 四周留白）", got.Dx(), got.Dy(), 400+StagePad, 200+StagePad)
	}
	t.Logf("真出图 %dx%d，%.0f KB", got.Dx(), got.Dy(), float64(len(raw))/1024)
}
