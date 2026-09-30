package aichat

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"xingta/internal/feature/text"
	"xingta/internal/kernel/calendar"
)

// personaStatic 是上下文里的静态层：看板娘人设 + 回答守则。
//
// 它是内嵌的运行时资产（形制同 calposter/biliwatch 各自的 template.html）：
// 以后往里加黑话字典、角色外号、规则底表，只改 persona.md，Go 代码一行不动。
// 代价交底：改文案要重新编译并传一次二进制，不像 config/*.yml 那样改文件重启就行
// —— 所以只有"会随版本换的数值"才配住在配置文件里，人设不是。
//
//go:embed persona.md
var personaStatic string

// personaRuneLimit 是静态层的硬上限，单测钉住。
// 量级推导：MVP 那份约 400 rune，1500 给到约 3.7 倍余量，够塞下一整套黑话字典；
// 再往上就是"有人往里贴了几万字百科、把每次请求的上下文吃光"那种事故。
// 它是刹车，不是刻度。
const personaRuneLimit = 1500

// clockLayout 是"当前时间"那一行的格式。带年份与偏移：模型要拿它算"还有几天"，
// 省掉年份（text.Clock 为省字符干的事）会让它自己去编年份。
const clockLayout = "2006-01-02 15:04 -0700"

// titleRuneLimit 是单条名称进上下文的长度上限。公告名取自标题里第一对「」，
// 正常远不到这个数；超了就是解析抽到了异常串，没必要让一整行挤掉别的条目。
const titleRuneLimit = 40

// systemPrompt 拼出「静态层 + 当期事实」。
//
// 长度是有界的，两头各管一截：静态层由 personaRuneLimit 钉住，事实列表由
// MaxPromptEvents 合计封顶，且每条名称再受 titleRuneLimit 约束。
func (s *Service) systemPrompt(now time.Time) string {
	ending := s.cal.EndingWithin(now, s.cfg.EndingLead)
	sortByEnd(ending)
	upcoming := s.cal.Upcoming(now, time.Duration(s.cfg.SoonDays)*24*time.Hour)
	active := s.activeOutside(now, ending)

	// 预算按「临期 → 进行中 → 即将开启」排优先级：只有一个总数上限时，快要跑掉的
	// 那几条最不该被截掉。段落仍按阅读顺序输出，被截掉的数量写在段尾——不然模型
	// 会把"截剩的"当成"全部"，回一句"一共就这三个活动"。
	budget := s.cfg.MaxPromptEvents
	ke := take(&budget, len(ending))
	ka := take(&budget, len(active))
	ku := take(&budget, len(upcoming))

	var b strings.Builder
	b.WriteString(strings.TrimSpace(personaStatic))
	b.WriteString("\n\n【当期事实】\n")
	fmt.Fprintf(&b, "当前时间：%s\n", now.In(s.cfg.Zone).Format(clockLayout))
	s.writeSection(&b, "正在进行", active, ka, now, s.endRow)
	s.writeSection(&b, fmt.Sprintf("%s内即将结束（这些也还在进行中）", text.LeadHours(s.cfg.EndingLead, true)),
		ending, ke, now, s.endRow)
	s.writeSection(&b, fmt.Sprintf("未来 %d 天内开启（还没开始）", s.cfg.SoonDays), upcoming, ku, now, s.startRow)
	if note := s.statusNote(now); note != "" {
		b.WriteString(note + "\n")
	}
	return b.String()
}

// endRow / startRow 是两段的行排版。模板本身在 feature/text 里，与 /events
// 那些回复共用同一份——同一种行在仓里出现第三份就是给自己埋漂移。
func (s *Service) endRow(a calendar.Activity, now time.Time) string {
	return text.EndingRow(text.Clip(a.Title, titleRuneLimit),
		text.Clock(a.End, now, s.cfg.Zone), text.Human(a.End.Sub(now)))
}

func (s *Service) startRow(a calendar.Activity, now time.Time) string {
	return text.StartingRow(text.Clip(a.Title, titleRuneLimit),
		text.Clock(a.Start, now, s.cfg.Zone), text.Human(a.Start.Sub(now)))
}

// activeOutside 取「正在进行、且不在临期名单里」的那些。
//
// EndingWithin 是 Active 的子集（两个查询共用同一段前缀切片，只差一个 End<=limit
// 判据，见 kernel/calendar/calendar.go），同一条目在两处都列的话，模型会数出
// 两个活动。临期那段更有用，所以从"进行中"里剔掉它。
func (s *Service) activeOutside(now time.Time, ending []calendar.Activity) []calendar.Activity {
	soon := make(map[string]bool, len(ending))
	for _, a := range ending {
		soon[a.ID] = true
	}
	list := s.cal.Active(now)
	out := make([]calendar.Activity, 0, len(list))
	for _, a := range list {
		if !soon[a.ID] {
			out = append(out, a)
		}
	}
	sortByEnd(out)
	return out
}

// sortByEnd 把"最快结束的排最前"。calendar 的查询一律按 Start 升序返回，
// 临期优先这个顺序得自己排（calquery 的三个 handler 也是这么做的）。
func sortByEnd(list []calendar.Activity) {
	sort.Slice(list, func(i, j int) bool { return list[i].End.Before(list[j].End) })
}

// take 从预算里给一段留出条数，返回留下的条数。前面的段用掉就不回来了。
func take(budget *int, want int) int {
	if *budget <= 0 {
		return 0
	}
	if want <= *budget {
		*budget -= want
		return want
	}
	n := *budget
	*budget = 0
	return n
}

// writeSection 输出一段。kept 是进上下文的条数，剩下的只报个数不报内容。
// 空列表要说"暂无"：不写这段，模型会把"没列出来"理解成"没查到"。
// 计数用冒号不用括号，是为了让标题自己也能带括号（"未来 7 天内开启（还没开始）"）
// 而不叠成两层。
func (s *Service) writeSection(b *strings.Builder, title string, list []calendar.Activity,
	kept int, now time.Time, row func(calendar.Activity, time.Time) string) {
	if len(list) == 0 {
		fmt.Fprintf(b, "%s：暂无\n", title)
		return
	}
	fmt.Fprintf(b, "%s：%d 条\n", title, len(list))
	for _, a := range list[:kept] {
		b.WriteString(row(a, now) + "\n")
	}
	if dropped := len(list) - kept; dropped > 0 {
		fmt.Fprintf(b, "（其余 %d 条未列出）\n", dropped)
	}
}

// statusNote 给模型说清这份列表的可信度。
//
// 判据与 calquery 同源（都是 annsync.Status 的 LastSuccess/Fails），措辞不同是刻意的：
// 那一份尾巴是给人看的（紧挨着翻页提示），这一句是给模型读的约束。
// 没接 Status 依赖就什么都不说。
func (s *Service) statusNote(now time.Time) string {
	if s.cfg.Status == nil {
		return ""
	}
	st, err := s.cfg.Status.Status()
	if err != nil {
		s.cfg.Logf("aichat: 读同步状况失败: %v", err)
		return ""
	}
	switch {
	case st.LastSuccess.IsZero():
		return "注意：公告还在首次同步中，上面的列表可能还不完整，别断言「什么也没有」。"
	case st.Fails > 0:
		return fmt.Sprintf("注意：公告源最近失败 %d 次（%s），列表可能落后于官网，涉及具体日期时提醒引航者以官网为准。",
			st.Fails, text.Clip(text.OneLine(st.LastError), 40))
	default:
		return "公告数据最近同步成功于 " + text.Clock(st.LastSuccess, now, s.cfg.Zone) + "。"
	}
}
