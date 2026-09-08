package render

import (
	"bytes"
	"encoding/json"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPageInjectsDataset(t *testing.T) {
	tpl := []byte("before " + DataMark + " after")
	d := Dataset{Now: "2026-09-08 20:00", OpenMs: 61200000,
		Windows: []Window{{Key: "202609071600", Name: "V", Start: "2026-09-08 00:00", Until: "2026-09-29 10:59"}}}
	out, err := Page(d, tpl)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	const head = "before const DATA = "
	if !strings.HasPrefix(s, head) || !strings.HasSuffix(s, "; after") {
		t.Fatalf("注入形式不对: %q", s)
	}
	var back Dataset
	if err := json.Unmarshal([]byte(s[len(head):len(s)-len("; after")]), &back); err != nil {
		t.Fatalf("注入的不是合法 JSON: %v\n%s", err, s)
	}
	if back.OpenMs != 61200000 || back.Windows[0].Key != "202609071600" {
		t.Errorf("回读不一致: %+v", back)
	}
	// 线格式键名不能漂：页面按这些名字取值。
	for _, k := range []string{`"openOffsetMs"`, `"versions"`, `"act0"`, `"redeem"`, `"monthly"`} {
		if !strings.Contains(s, k) {
			t.Errorf("缺少模板要用的键 %s", k)
		}
	}
	// 原文里带 </script> 也不能把脚本段截断。
	d.Records = []Record{{ID: "1", Raw: "</script><script>alert(1)</script>"}}
	bad, err := Page(d, []byte("<script>"+DataMark+"</script>"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(bad), "</script>"); n != 1 {
		t.Errorf("转义后仍有 %d 个 </script>，脚本段会被截断", n)
	}
	if !strings.Contains(string(bad), `\u003c/script`) {
		t.Errorf("没把 < 转义: %s", bad)
	}
	if _, err := Page(d, []byte("没有占位")); err == nil {
		t.Error("模板里没有占位却成功了")
	}
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
	if win := urlOf(`J:\学习\cal.html`); !strings.Contains(win, "/J:/") {
		t.Errorf("Windows 路径没变成 file:///J:/…: %s", win)
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

func TestTemplateIsEmbedded(t *testing.T) {
	if len(Template) < 1000 {
		t.Fatalf("模板没打进来（%d 字节）", len(Template))
	}
	if !strings.Contains(string(Template), DataMark) {
		t.Error("内嵌模板里没有数据占位")
	}
	if !strings.Contains(string(Template), "location.hash") {
		t.Error("内嵌模板不像那份日历页")
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

// 真跑一遍：内嵌模板 + 最小数据集 → 浏览器 → PNG。
// 验的是两趟契约：截图尺寸必须正好是页面自己解出的卡片尺寸 + 四周留白。
// （不能拿宽高比当断言：记录少时卡片被 MIN_PPD 下限顶宽，比例本就不是 16:9。）
func TestCaptureWithRealBrowser(t *testing.T) {
	page, err := Page(Dataset{
		Now:    "2026-09-08 20:00",
		OpenMs: 61200000,
		Windows: []Window{{Key: "202609071600", Name: "测试版本",
			Start: "2026-09-08 00:00", End: "2026-09-22 03:59", Until: "2026-09-29 10:59"}},
		Records: []Record{
			{ID: "1-0", Name: "甲活动", Label: "活动时间", Start: "2026-09-08 00:00", End: "2026-09-12 03:59",
				StartKind: "fuzzy", Tint: "eef3fb", Source: "solo", Ref: "1"},
			{ID: "2-0", Name: "乙尾段", Label: "招募时间", Start: "2026-09-14 00:00", End: "2026-09-16 10:59",
				ClaimEnd: "2026-09-20 10:59", Tint: "fdeef2", Source: "solo", Ref: "2"},
		},
		Repeating: []string{},
		Stats:     map[string]int{"records": 2},
	}, Template)
	if err != nil {
		t.Fatal(err)
	}
	bin := chromeBin(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "page.html")
	if err := os.WriteFile(path, page, 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Browser{Bin: bin, WorkDir: dir}
	w, h, err := b.measure(path, "#x202609071600")
	if err != nil {
		t.Fatal(err)
	}
	if w < 1000 || h < 400 {
		t.Fatalf("页面报的尺寸不像画过东西: %dx%d", w, h)
	}
	raw, err := b.Capture(page, "#x202609071600")
	if err != nil {
		t.Fatal(err)
	}
	im, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("出来的不是能解的图: %v", err)
	}
	got := im.Bounds()
	if got.Dx() != w+StagePad || got.Dy() != h+StagePad {
		t.Errorf("截图 %dx%d, 想 %dx%d（页面自报尺寸 + 四周留白）", got.Dx(), got.Dy(), w+StagePad, h+StagePad)
	}
	t.Logf("真出图 %dx%d（页面报卡片 %dx%d），%.0f KB", got.Dx(), got.Dy(), w, h, float64(len(raw))/1024)
}
