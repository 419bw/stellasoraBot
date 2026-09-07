package stellasora

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
	"xingta/internal/annsync"
)

// 这份文件是规则 v2，2026-09-05 在 74 条线上真实公告正文上验证过：
// 11 条进行中 / 50 条已过期 / 7 条丢弃，判定全部正确，且采信的公告每一条
// 都恰好只有一个主区间——没有一条需要靠猜来二选一。
//
//	名字 = 公告标题里第一对「」。正文不参与命名：▌小节标题是给编辑看的小标题，
//	       历史上「無序」「領主資訊」这类查无此名的活动就是从这里来的。
//	窗口 = 正文里"日期 时刻 ~ 日期 时刻"的行。每行归属于它自己的时间标签：
//	       行内前缀（"活动奖励领取时间："）优先于所在的 ▌小节标题，因为
//	       ▌开放时间 下面经常同时挂着「活动时间」和「活动奖励领取时间」两行。
//	标签里含 领取/结算/兑换/售卖/上架… 的是附属窗口（领奖期、商店兑换期、礼包售卖期），
//	不是活动本身 → 丢弃。这一条同时解决了"一个活动被列成两行"和"商店购买活动被当活动"。
//	剩下的主窗口必须恰好一个才采信；多了说明这是条汇总公告（版本一览那种），
//	整条交人工，不猜。

// Interval 是正文里解析出来的一个时间窗口，带着它的标签归属与原文行。
type Interval struct {
	Tag    string // 时间标签，如「招募时间」「排名奖励领取时间」
	Start  time.Time
	End    time.Time
	Status string // annsync.StatusOK / StatusFuzzyStart / StatusPending
	Line   string // 原文整行，人工核对用
}

// Result 是一条公告的解析结果。零值表示"什么都没有"。
type Result struct {
	Name          string     // 标题里的活动名；空 = 这条不是活动公告
	Primary       []Interval // 主活动窗口
	Secondary     []Interval // 领奖/兑换/售卖等附属窗口，采信时丢弃
	Weird         []string   // 含日期却没认出窗口的行
	TimeLabel     bool       // 正文出现过「▌…时间」这类小节标题
	SaysPermanent bool       // 正文写了"常驻"：这类内容有意不跟踪

	ExtraPrimary []Interval // Primary[1:]，进「待确认」时说清还有几个
}

// Suspect 报告这条公告值不值得进「待确认」：
//   - 主窗口多于一个：这是条汇总公告，交人工挑；
//   - 有活动名、一个窗口都没抓到、而正文又长得像有档期（出现过时间小节，
//     或者有含日期却没认出的行）：日期多半画在图片里，静默漏掉最坏。
//
// 只有附属窗口（4507「创业激励基金」那种整条只写售卖时间的） confidently 不是活动，不报警。
func (r Result) Suspect() bool {
	if len(r.Primary) > 1 {
		return true
	}
	if r.Name == "" || len(r.Primary) > 0 || r.SaysPermanent {
		return false
	}
	// 一个窗口都没有才谈得上"漏抓"；只有附属窗口的（4507 那种整条只写售卖时间的）
	// 已经确定不是活动，报警只会淹掉桶。
	return len(r.Secondary) == 0 && (r.TimeLabel || len(r.Weird) > 0)
}

// Note 是给「待确认」看的一句话：为什么这条需要人看一眼。
func (r Result) Note() string {
	switch {
	case len(r.Primary) > 1:
		return "正文有 " + strconv.Itoa(len(r.Primary)) + " 个主时间窗口，像是汇总公告，未采信"
	case r.Suspect():
		return "标题像活动公告，但正文没抓到时间窗口（日期多半在图片里）"
	default:
		return ""
	}
}

var (
	// 活动名：标题里第一对引号。方括号 [版本名] 那一类不收（它们是版本汇总页）。
	nameRe = regexp.MustCompile(`[「『]([^」』]{1,40})[」』]`)

	// 相对起点："起点不是具体时刻"的表述，命中即 fuzzy_start。
	// 这里是有意写成开放类的：闭表枚举过一次，全量实测就漏了「公测开启后」这种
	// 写法（1750/1751 两个限时卡池、1732 创业激励基金都用的这个）。
	// 上限 12 字且必须紧跟波浪号，长句里夹个"后"字吃不进去。
	relAnchor = `[^\s~～]{0,12}后`

	// 一个完整窗口：起始日期 + 时刻或相对表述 + 波浪号 + 结束日期（年份可省）+ 时刻。
	// 覆盖线上全部形状：「2026/09/01 12:00 ~ 2026/09/08 10:59」
	// 与「2026/08/18 维护结束后 ~ 2026/09/08 10:59」。
	intervalRe = regexp.MustCompile(
		`(\d{4})[./-](\d{1,2})[./-](\d{1,2})\s*(?:(` + relAnchor + `)|(\d{1,2}):(\d{2}))\s*[~～]\s*` +
			`(?:(\d{4})[./-])?(\d{1,2})[./-](\d{1,2})\s*(\d{1,2}):(\d{2})`)

	// 只要"长得像日期"就算：区间正则没吃下的这类行会进 Weird，触发报警。
	// 放宽到这里是有意的——两位年份、日期段之间夹空格，都属于"我们读不懂"而不是"没有日期"。
	dateRe = regexp.MustCompile(`\d{2,4}\s*[./-]\s*\d{1,2}\s*[./-]\s*\d{1,2}`)

	// 行内时间标签：「活动时间：2026/…」取"活动时间"。
	inlineTagRe = regexp.MustCompile(`^([^，。；]{1,16}?(?:时间|時間|期间|期間|日期))[:：]`)

	// 小节标题是不是时间类：▌开放时间 / ▌售卖时间。
	sectionTimeRe = regexp.MustCompile(`(?:时间|時間|期间|期間|日期)$`)

	// 附属窗口词表。线上出现过的完整标签：活动奖励领取时间、排名奖励结算时间、
	// 排名奖励领取时间、活动商店与奖励兑换时间、奖励领取时间、售卖时间。
	secondaryRe = regexp.MustCompile(`领取|領取|结算|結算|兑换|兌换|售卖|售賣|上架|返场|返場|上新|折扣|发放|發放|发货|發貨|有效期|使用期限|回收|纪念|紀念`)

	// 常驻内容（4490「诺瓦异闻」写"开放后常驻"）与道具有效期：有意不跟踪，也不算漏抓。
	permanentRe  = regexp.MustCompile(`常驻|常駐`)
	ignoreDateRe = regexp.MustCompile(`使用期限|有效期至|回收|有效期`)
)

// Parse 把一条公告正文拆成"名字 + 若干时间窗口"。纯函数，零 IO。
func Parse(n NewsBody, loc *time.Location) Result {
	var r Result
	r.Name = activityName(n.Title)

	section := ""
	for _, ln := range htmlToLines(n.Content) {
		if strings.HasPrefix(ln, "▌") {
			section = strings.TrimRight(strings.TrimSpace(ln[len("▌"):]), " :：")
			if sectionTimeRe.MatchString(section) {
				r.TimeLabel = true
			}
			continue
		}
		if permanentRe.MatchString(ln) && dateRe.MatchString(ln) {
			r.SaysPermanent = true
		}
		m := intervalRe.FindStringSubmatch(ln)
		if m == nil {
			if dateRe.MatchString(ln) && !ignoreDateRe.MatchString(ln) {
				r.Weird = append(r.Weird, ln)
			}
			continue
		}
		iv := buildInterval(ln, tagOf(ln, section), m, loc)
		if secondaryRe.MatchString(iv.Tag) {
			r.Secondary = append(r.Secondary, iv)
		} else {
			r.Primary = append(r.Primary, iv)
		}
	}
	if len(r.Primary) > 1 {
		r.ExtraPrimary = r.Primary[1:]
	}
	return r
}

// activityName 取标题里第一对「」内的活动本名。取不到返回空串，调用方据此判
// "这条公告与活动无关"。真实形状：
//
//	「缭乱碎晶球」活动说明            → 缭乱碎晶球
//	「出击海滨阵地！倾尽全力的碎瓜一击」说明 → 出击海滨阵地！倾尽全力的碎瓜一击
//	《星塔旅人》 × 星际传奇 联动活动开启！  → （空，标题没有引号）
//	09月01日已知问题说明               → （空）
func activityName(title string) string {
	m := nameRe.FindStringSubmatch(strings.TrimSpace(title))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// tagOf 定这个窗口该记在哪个标签下。行内前缀赢：▌开放时间 下面经常并列两行
// 「活动时间：…」和「活动奖励领取时间：…」，只看小节会把它们混成一类。
func tagOf(line, section string) string {
	trimmed := strings.TrimLeft(line, "-—*■✦ \t")
	if m := inlineTagRe.FindStringSubmatch(trimmed); m != nil {
		return strings.TrimSpace(m[1])
	}
	return section
}

// buildInterval 按捕获组造窗口。组序与 intervalRe 一致：
// 1 起始年 2 起始月 3 起始日 4 相对起点 5 时起 6 时分 7 结束年 8 结束月 9 结束日 10 止时 11 止分。
func buildInterval(line, tag string, m []string, loc *time.Location) Interval {
	iv := Interval{Tag: tag, Line: line}
	ey := m[7]
	if ey == "" {
		ey = m[1] // 结束日期省年份：同一个活动期内，借起始年
	}
	iv.End = timeDate(ey, m[8], m[9], m[10], m[11], loc)

	if m[4] != "" {
		// 「维护结束后」只给日期不给时刻。估成当天 00:00 并标 fuzzy：
		// 日历上差几个小时无所谓，EndingWithin 只依赖 End。估成发布时间是错的（会早一天）。
		iv.Start = timeDate(m[1], m[2], m[3], "0", "0", loc)
		iv.Status = annsync.StatusFuzzyStart
	} else {
		iv.Start = timeDate(m[1], m[2], m[3], m[5], m[6], loc)
		iv.Status = annsync.StatusOK
	}
	if !iv.End.After(iv.Start) {
		iv.Status = annsync.StatusPending // 倒挂：解析错了或原文写错，交人工
	}
	return iv
}

func timeDate(y, mo, d, hh, mi string, loc *time.Location) time.Time {
	return time.Date(num(y), time.Month(num(mo)), num(d), num(hh), num(mi), 0, 0, loc)
}

func num(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// htmlToLines 把富文本拍成行：块级标签换行，实体解码，去空白行。
func htmlToLines(content string) []string {
	s := content
	for _, tag := range []string{"<br>", "<br/>", "<br />", "</p>", "</div>", "</li>", "</tr>"} {
		s = strings.ReplaceAll(s, tag, "\n")
	}
	s = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// &nbsp; 解出来是 U+00A0，而 Go 的 \s 只认 ASCII 空白，日期里的这个"假空格"
	// 会让 intervalRe 整行吃不下去。真正文里出现过。
	s = strings.ReplaceAll(s, "\u00a0", " ")

	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}
