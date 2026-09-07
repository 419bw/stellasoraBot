package text_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xingta/internal/feature/text"
)

// 这三个排版件被每个面向用户的字符串用到，出错方式是「发出去的话看不懂」而不是
// panic，编译器一条都抓不出来。所以在这里独立钉死，不靠 calquery 的用例顺带覆盖。

var zone = time.FixedZone("CST", 8*60*60)

func TestHumanBoundaries(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "已到点"},
		{-time.Second, "已到点"},
		{time.Minute, "1分钟"},
		{59*time.Minute + 59*time.Second, "59分钟"}, // 不足一小时绝不写成「0小时」
		{time.Hour, "1小时"},
		{23*time.Hour + 59*time.Minute, "23小时"},
		{24 * time.Hour, "1天"},
		{25 * time.Hour, "1天1小时"},
		{48 * time.Hour, "2天"},
		{365 * 24 * time.Hour, "365天"},
	}
	// 整天不说「1天0小时」；一年以内都用天，不自己发明「月」。
	for _, c := range cases {
		if got := text.Human(c.d); got != c.want {
			t.Errorf("Human(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestClipNeverSplitsACharacter(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"活動", 5, "活動"},      // 没超就原样，连长度都不动
		{"活動", 2, "活動"},      // 恰好到界，不加省略号
		{"活動", 1, "活…"},      // 按 rune 数，不是按字节
		{"abcde", 3, "abc…"}, // 纯 ASCII 也一样只按字符数
		{"活動中", 0, "…"},      // n=0 也只剩省略号而不是空串
	}
	for _, c := range cases {
		if got := text.Clip(c.in, c.n); got != c.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	// 最坏情况是截在半个 UTF-8 序列上：发出去就是一个方块。
	for _, in := range []string{"星塔旅人活動公告", "a界b界c界", "🙂😀😃"} {
		got := text.Clip(in, 4)
		if !utf8.ValidString(got) {
			t.Errorf("Clip(%q, 4) 产出非法 UTF-8：%q", in, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("Clip(%q, 4) 产出替换字符：%q", in, got)
		}
	}
}

func TestClockHidesYearOnlyWithinSameYear(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, zone)
	cases := []struct {
		what string
		t    time.Time
		want string
	}{
		{"同年省略年份", time.Date(2026, 1, 1, 0, 0, 0, 0, zone), "01-01 00:00"},
		{"12-31 仍是同年", time.Date(2026, 12, 31, 23, 59, 0, 0, zone), "12-31 23:59"},
		{"跨年必须带年份", time.Date(2027, 1, 1, 0, 0, 0, 0, zone), "2027-01-01 00:00"},
		{"往前跨年也带", time.Date(2025, 12, 31, 23, 0, 0, 0, zone), "2025-12-31 23:00"},
		// 公告时刻是 UTC 存储、按源时区展示：不换算就会差 8 小时。
		{"UTC 时刻按 +08 显示", time.Date(2026, 9, 1, 17, 0, 0, 0, time.UTC), "09-02 01:00"},
	}
	for _, c := range cases {
		if got := text.Clock(c.t, now, zone); got != c.want {
			t.Errorf("%s：Clock = %q, want %q", c.what, got, c.want)
		}
	}
	// zone 为 nil 时退到 UTC：这不是想要的显示，但必须有兜底而不是 panic。
	if got := text.Clock(now, now, nil); got != "09-02 04:00" {
		t.Errorf("zone 为 nil 时没兜底：%q", got)
	}
}

func TestOneLineCollapsesEveryLineBreak(t *testing.T) {
	cases := []struct{ in, want string }{
		{"第一行\n第二行", "第一行 第二行"},
		{"第一行\r\n第二行", "第一行 第二行"},
		{"制表\t符与  连跑空格", "制表 符与 连跑空格"},
		{"  两头空白  ", "两头空白"},
		{"", ""},
		// 全角空格是中日文标题里的真实字符，逐字核对时不能被替人改掉。
		{"悠悠　漫時", "悠悠　漫時"},
	}
	for _, c := range cases {
		if got := text.OneLine(c.in); got != c.want {
			t.Errorf("OneLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
