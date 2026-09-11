package render

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Capturer 定义从 HTML 页面与路由截取 PNG 的通用能力。
type Capturer interface {
	Capture(page []byte, route string) ([]byte, error)
}

// StagePad 是出图视口的默认留白补正。
const StagePad = 24

// Browser 用无头浏览器把页面截成 PNG。
type Browser struct {
	// Bin 是浏览器可执行文件，由调用方给（包不猜装在哪）。
	Bin string
	// Budget 是每趟给浏览器的虚拟时间上限；要等远程配图加载，默认 8 秒。
	Budget int
	// WorkDir 放临时页面与截图，必须是纯 ASCII 路径（含中文的 --screenshot 参数会被
	// Windows 的 argv 转换搞坏）。空 = os.TempDir()。
	WorkDir string
	// Env 追加到子进程环境（Linux 上跑 headless 常要补变量；测试也用它注入假浏览器）。
	Env []string
	// Logf 可选：两趟浏览器各自的耗时与尺寸记在这儿。出图慢的时候要先分清
	// 是量尺寸那趟慢还是截图那趟慢，两者都是独立起一个浏览器进程。
	Logf func(format string, args ...any)
}

func (b *Browser) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

func (b *Browser) command(args ...string) *exec.Cmd {
	cmd := exec.Command(b.Bin, args...)
	if len(b.Env) > 0 {
		cmd.Env = append(os.Environ(), b.Env...)
	}
	return cmd
}

// Capture 把调用方组好的页面截成 PNG。route 是页面锚点（如出图模式的 "#x<版本键>"），
// 由调用方决定，包不认识它的含义。
//
// 两趟：先让页面自己把解出的卡片尺寸写进 <title>，再按"卡片 + 留白"设视口截图 ——
// 16:9 的宽是页面按内容反解的，外面算不出来。
func (b *Browser) Capture(page []byte, route string) ([]byte, error) {
	if b.Bin == "" {
		return nil, fmt.Errorf("render: 没有浏览器可执行文件")
	}
	dir, err := b.workDir()
	if err != nil {
		return nil, err
	}
	// 临时文件必须唯一：同一目录里两个版本并发出图时，固定名会让后写的那份
	// 页面顶掉前一份，截出来就不是调用方要的那张。
	pageFile, err := os.CreateTemp(dir, "xingta-cal-*.html")
	if err != nil {
		return nil, fmt.Errorf("render: 建临时页面失败: %w", err)
	}
	path := pageFile.Name()
	defer os.Remove(path)
	if _, err := pageFile.Write(page); err != nil {
		pageFile.Close()
		return nil, fmt.Errorf("render: 写临时页面失败: %w", err)
	}
	if err := pageFile.Close(); err != nil {
		return nil, fmt.Errorf("render: 写临时页面失败: %w", err)
	}

	mStart := time.Now()
	w, h, err := b.measure(path, route)
	if err != nil {
		return nil, err
	}
	b.logf("render: 量尺寸 %s → %dx%d", time.Since(mStart).Round(10*time.Millisecond), w, h)
	shotFile, err := os.CreateTemp(dir, "xingta-cal-*.png")
	if err != nil {
		return nil, fmt.Errorf("render: 建临时截图失败: %w", err)
	}
	shot := shotFile.Name()
	shotFile.Close()
	_ = os.Remove(shot) // Chrome 要求目标不存在或可覆盖，先让位给它
	cStart := time.Now()
	cmd := b.command(b.captureArgs(path, route, w, h, shot)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("render: 截图失败: %v（%s）", err, tail(out))
	}
	raw, err := os.ReadFile(shot)
	if err != nil {
		return nil, fmt.Errorf("render: 浏览器没产出截图: %w", err)
	}
	b.logf("render: 截图 %s → %d KB", time.Since(cStart).Round(10*time.Millisecond), len(raw)/1024)
	os.Remove(shot)
	return raw, nil
}

func (b *Browser) measureArgs(page, hash string) []string {
	return []string{"--headless=new", "--disable-gpu",
		"--virtual-time-budget=" + strconv.Itoa(b.budget()), "--dump-dom", urlOf(page) + hash}
}

func (b *Browser) captureArgs(page, hash string, w, h int, shot string) []string {
	return []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=1",
		"--window-size=" + strconv.Itoa(w+StagePad) + "," + strconv.Itoa(h+StagePad),
		"--virtual-time-budget=" + strconv.Itoa(b.budget()),
		"--screenshot=" + shot, urlOf(page) + hash}
}

var titleSize = regexp.MustCompile(`<title>\s*(\d+)x(\d+)\s*`)

// parseTitle 读回页面自己解出的卡片尺寸。出图模式下页面一定会写这个 <title>；
// 没写到就说明出图模式没跑起来（比如 hash 不对），不能拿默认视口凑一张。
func parseTitle(dom []byte) (int, int, error) {
	m := titleSize.FindSubmatch(dom)
	if m == nil {
		return 0, 0, fmt.Errorf("render: 页面没报尺寸（<title>WxH</title>），出图模式没跑起来")
	}
	w, _ := strconv.Atoi(string(m[1]))
	h, _ := strconv.Atoi(string(m[2]))
	if w < 100 || h < 100 {
		return 0, 0, fmt.Errorf("render: 页面报的尺寸不合理: %dx%d", w, h)
	}
	return w, h, nil
}

// measure 跑一趟 --dump-dom 读回页面自己解出的卡片尺寸。
func (b *Browser) measure(page, hash string) (w, h int, err error) {
	out, err := b.command(b.measureArgs(page, hash)...).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("render: 量尺寸失败: %v（%s）", err, tail(out))
	}
	return parseTitle(out)
}

func (b *Browser) budget() int {
	if b.Budget <= 0 {
		return 8000
	}
	return b.Budget
}

func (b *Browser) workDir() (string, error) {
	dir := b.WorkDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("render: 临时目录不可用: %w", err)
	}
	return dir, nil
}

// urlOf 把本地路径变成 file:/// URL。中文段要 percent-encode，否则 Chrome 解不出文件。
func urlOf(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	p := filepath.ToSlash(abs)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}
	var sb bytes.Buffer
	sb.WriteString("file://")
	for _, seg := range bytes.Split([]byte(p), []byte("/")) {
		sb.WriteByte('/')
		sb.WriteString(escape(string(seg)))
	}
	return sb.String()
}

// escape 按 RFC 3986 的 unreserved 集合逐字节百分号编码；非 ASCII 因此自然被编码，
// Chrome 才解得开含中文的路径。
func escape(s string) string {
	const hex = "0123456789ABCDEF"
	var sb bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == ':':
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0xf])
		}
	}
	return sb.String()
}

func tail(out []byte) string {
	if len(out) > 200 {
		return "..." + string(out[len(out)-200:])
	}
	return string(out)
}
