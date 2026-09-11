package render

import (
	"bytes"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
