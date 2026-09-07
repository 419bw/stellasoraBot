// Package text 是功能层的公共排版件：把时间和长度说成人话。
//
// 它不含任何业务概念（不认识"活动""公告"），所以功能之间不会因为它互相依赖——
// 拆掉 calquery 不影响 calexpiry 用它。三个功能各自抄一份"剩 X 天 Y 小时"才是真耦合。
package text

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Human 把时长说成人话。日历上的活动动辄几天，分钟级精度没有意义。
func Human(d time.Duration) string {
	if d <= 0 {
		return "已到点"
	}
	totalMinutes := int(d.Minutes())
	days, hours := totalMinutes/1440, (totalMinutes%1440)/60
	minutes := totalMinutes % 60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%d天%d小时", days, hours)
	case days > 0:
		return fmt.Sprintf("%d天", days)
	case hours > 0:
		return fmt.Sprintf("%d小时", hours)
	default:
		return fmt.Sprintf("%d分钟", minutes)
	}
}

// Clock 按 zone 格式化时刻，同年省略年份：公告都在近期，省下的字符留给活动名。
func Clock(t, now time.Time, zone *time.Location) string {
	if zone == nil {
		zone = time.UTC
	}
	t = t.In(zone)
	if t.Year() == now.In(zone).Year() {
		return t.Format("01-02 15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// Clip 按 rune 截断，绝不在字符中间切开（半个 UTF-8 序列发出去就是乱码）。
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// wsRe 只认 ASCII 空白：全角空格（U+3000）在中日文标题里是有意义的字符，
// 而这段文本的用途恰恰是「人工拿去官网逐字核对」，不能替人改掉。
var wsRe = regexp.MustCompile(`[ \t\r\n]+`)

// OneLine 把多行文本压成一行，用于把原文片段塞进列表行。
// 连跑的空白压成一个空格（CRLF 直接替换会留下两个空格）。
func OneLine(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}
