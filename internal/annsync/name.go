package annsync

import (
	"regexp"
	"strings"
)

// NormName 是"同一条活动"配对用的名字规范化：去空白与常见标点，剥引号。
// 「优雅起舞 千金驾到」与「优雅起舞 千金驾到！」（官方加叹号）必须配得上。
// 住引擎层是因为跨条合并与运维按名查记录用的是同一把尺子，两处各写一份必然漂移。
var normPunctRe = regexp.MustCompile(`[！!。，,、~\s]`)

func NormName(s string) string {
	return strings.Trim(normPunctRe.ReplaceAllString(s, ""), "「」『』")
}
