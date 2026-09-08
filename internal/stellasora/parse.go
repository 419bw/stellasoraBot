package stellasora

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
	"xingta/internal/annsync"
)

// 这份文件是规则 v3（2026-09-07），在官网全量 349 条真实正文上验证过：
// 事件 181 条（独立 173 + 汇总切出 8，去重丢弃 99，版本名重复滤 6）、切分失败报警 0。
// v2 的判定对单活动公告全部保留；变化只有三处：
//   汇总公告（维护更新说明）按条目切分成 N 条事件，不再整条交人工——
//   版本后半段还没发独立公告的活动，档期就从这里来；
//   「[XXX]活动一览/版本一览」改判为版本窗口（不是活动，不进日历）；
//   标题没有「」且不含"活动"的（4118 在线闪断更新那类）不再产出——
//   v2 把它正文里补偿邮件的有效期误判成了一条横跨整月的"活动"。
//
//	名字 = 公告标题里第一对「」。正文不参与命名：▌小节标题是给编辑看的小标题，
//	       历史上「無序」「領主資訊」这类查无此名的活动就是从这里来的。
//	窗口 = 正文里"日期 时刻 ~ 日期 时刻"的行。每行归属于它自己的时间标签：
//	       行内前缀（"活动奖励领取时间："）优先于所在的 ▌小节标题，因为
//	       ▌开放时间 下面经常同时挂着「活动时间」和「活动奖励领取时间」两行。
//	标签里含 领取/结算/兑换/售卖/上架… 的是附属窗口（领奖期、商店兑换期、礼包售卖期），
//	不是活动本身 → 不产事件；但汇总条目块内的「兑换」窗口会挂在 Entry.Redeem 上，
//	供核对时间表与人工覆盖用。
//	单活动公告的主窗口仍须恰好一个；多了且切不出条目名才交人工（Suspect）。

// 产出来源。写在 Event.Provenance / Rec.Provenance 上。
const (
	ProvSolo    = "solo"    // 独立公告：一条公告讲一个活动
	ProvSummary = "summary" // 汇总公告切分：维护更新说明里列出的档期
	ProvVersion = "version" // 版本窗口公告：版本边界 + 版本主活动事件（封面即版本主视觉）
)

// Interval 是正文里解析出来的一个时间窗口，带着它的标签归属与原文行。
type Interval struct {
	Tag    string // 时间标签，如「招募时间」「排名奖励领取时间」
	Start  time.Time
	End    time.Time
	Status string // annsync.StatusOK / StatusFuzzyStart / StatusPending
	Line   string // 原文整行，人工核对用
}

// Entry 是汇总公告切出来的一个具名活动。
type Entry struct {
	Name   string
	Ival   Interval
	Redeem *Interval // 条目块内的「兑换」窗口（活动商店与奖励兑换时间），可空
	Line   string    // 条目行原文，人工核对用
}

// VersionWindow 是「[XXX]活动一览 / [XXX]版本一览」给出的版本边界，只随 Result 带出
// （核对表与将来的版本图层用，不进日历）。「版本一览」正文常是整张图（零窗口），
// 属已知形状；有窗口的活动一览同时把版本主活动产出为一条事件（见分派处）。
type VersionWindow struct {
	Name      string // 方括号里的版本名
	ActStart  time.Time
	PlayEnd   time.Time // 玩法期止（活动时间行的终点）
	RedeemEnd time.Time // 版本终点（兑换截止行）；没有兑换行时 = PlayEnd
}

// Result 是一条公告的解析结果。零值表示"什么都没有"。
type Result struct {
	Name          string     // 标题里的活动名；空 = 这条不是活动公告
	Primary       []Interval // 主活动窗口
	Secondary     []Interval // 领奖/兑换/售卖等附属窗口，不产事件
	Weird         []string   // 含日期却没认出窗口的行
	TimeLabel     bool       // 正文出现过「▌…时间」这类小节标题
	SaysPermanent bool       // 正文写了"常驻"：这类内容有意不跟踪

	ExtraPrimary []Interval // Primary[1:]，进「待确认」时说清还有几个

	Entries    []Entry        // 汇总公告的条目切分；单活动公告恰一条（Name+唯一主窗口）
	Ver        *VersionWindow // 版本窗口公告的抽取结果；其他公告为 nil
	Provenance string         // ProvSolo / ProvSummary / ProvVersion；空 = 不产出
}

// Suspect 报告这条公告值不值得进「待确认」：
//   - 汇总公告切分失败（有多个主窗口却切不出条目名）：规则没读懂正文，交人工；
//   - 版本窗口公告抽不出边界且正文并非整张图：形状变了，交人工；
//   - 有活动名、一个窗口都没抓到、而正文又长得像有档期（出现过时间小节，
//     或者有含日期却没认出的行）：日期多半画在图片里，静默漏掉最坏。
//
// 切分成功的汇总公告是被采信的数据源，不再进「待确认」——塞进人工桶只会淹没
// 真正需要人看的。只有附属窗口（4507「创业激励基金」那种整条只写售卖时间的）
// confidently 不是活动，不报警。
func (r Result) Suspect() bool {
	if r.Provenance == ProvVersion {
		return r.Ver == nil && (len(r.Primary) > 0 || len(r.Secondary) > 0)
	}
	if r.Provenance == ProvSummary {
		return false
	}
	if len(r.Primary) > 1 && len(r.Entries) == 0 {
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
	case r.Provenance == ProvVersion && r.Ver == nil:
		return "版本窗口公告但抽不出边界（「版本一览」整张图属正常，这是正文有档期却没抽出的情形）"
	case len(r.Primary) > 1 && len(r.Entries) == 0:
		return "正文有 " + strconv.Itoa(len(r.Primary)) + " 个主时间窗口且切不出条目名，未采信"
	case r.Suspect():
		return "标题像活动公告，但正文没抓到时间窗口（日期多半在图片里）"
	default:
		return ""
	}
}

var (
	// 活动名：标题里第一对引号。方括号 [版本名] 那一类不收（它们是版本汇总页）。
	nameRe = regexp.MustCompile(`[「『]([^」』]{1,40})[」』]`)

	// 卡池名：维护公告的招募段里一行可能并列两个池子（旅人池 + 秘纹池），
	// 共用同一个「招募时间」窗口。只取第一对「」会拿到角色名（那是旅人不是活动）。
	poolRe = regexp.MustCompile(`限时招募活动[「『]([^」』]{1,40})[」』]`)

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

	// ---- v3 新增 ----

	// 版本窗口公告：[版本名]活动一览 / [版本名]版本一览。
	verTitleRe = regexp.MustCompile(`^\[[^\]]{2,40}\](?:活动|版本)一览$`)
	verNameRe  = regexp.MustCompile(`^\[([^\]]{2,40})\](?:活动|版本)一览$`)

	// 条目行：汇总公告里以短横开头的具名行（「- 终焉绝响」）。
	// ✦/■/◆/● 是分组行不是条目行（见 groupRe）；全量实测里以 * 开头的条目没有出现过。
	dashRe = regexp.MustCompile(`^\s*[-–—]\s*(\S.*)$`)

	// 分组行：✦ 全新活动 / ■ 限时商品。命中即切段，攒着的条目名作废。
	groupRe = regexp.MustCompile(`^\s*[✦■◆●]+\s*(\S.*)$`)

	// 字段行黑名单：礼包段里「- 午后茶会·特惠礼券组合」是真条目，紧跟的
	// 「礼包内容：青空礼券*10」不是。
	fieldRe = regexp.MustCompile(`^(活动说明|参与条件|售卖价格|礼包内容|版本限购|获取方式|内容说明|备注|补偿对象|维护时间|维护补偿|售价|价格)`)

	// 2115《星塔旅人》H5趣味网页活动里程碑奖励：标题用书名号不用「」，但确实是
	// 活动公告。剥掉《游戏名》前缀当活动名。
	gamePrefixRe = regexp.MustCompile(`^《[^》]{1,12}》\s*`)
)

// Parse 把一条公告正文拆成"名字 + 时间窗口 + 条目切分"。纯函数，零 IO。
func Parse(n NewsBody, loc *time.Location) Result {
	var r Result
	r.Name = activityName(n.Title)
	isVer := verTitleRe.MatchString(strings.TrimSpace(n.Title))

	section := ""
	var pending []string // 攒着还没配上窗口的条目名
	var claimed []int    // 最近一次主窗口认领的 Entries 下标；块内的附属窗口挂给它们
	const maxPending = 6 // 单块条目名的护栏：超过说明判据没读懂正文，宁可丢名也不瞎配

	addName := func(nm string) {
		if nm == "" || len(pending) >= maxPending {
			return
		}
		pending = append(pending, nm)
	}

	for _, ln := range htmlToLines(n.Content) {
		// ▌小节：切段，攒的名字作废
		if strings.HasPrefix(ln, "▌") {
			section = strings.TrimRight(strings.TrimSpace(ln[len("▌"):]), " :：")
			if sectionTimeRe.MatchString(section) {
				r.TimeLabel = true
			}
			pending, claimed = nil, nil
			continue
		}
		// ✦/■ 分组行：同上作废，不参与命名
		if groupRe.MatchString(ln) {
			pending, claimed = nil, nil
			continue
		}
		if permanentRe.MatchString(ln) && dateRe.MatchString(ln) {
			r.SaysPermanent = true
		}

		// 窗口行（先于条目行判："- 活动时间：…" 两边都匹配）
		if m := intervalRe.FindStringSubmatch(ln); m != nil {
			iv := buildInterval(ln, tagOf(ln, section), m, loc)
			if secondaryRe.MatchString(iv.Tag) {
				r.Secondary = append(r.Secondary, iv)
				// 条目块内的「兑换」窗口挂到块内条目上（活动商店与奖励兑换时间）
				if strings.Contains(iv.Tag, "兑换") || strings.Contains(iv.Tag, "兌换") {
					for _, i := range claimed {
						if r.Entries[i].Redeem == nil {
							r.Entries[i].Redeem = &iv
						}
					}
				}
				continue
			}
			r.Primary = append(r.Primary, iv)
			// 攒的条目名按顺序认领这个窗口；一个窗口可以对应多个池子
			if len(pending) > 0 {
				claimed = claimed[:0]
				for _, nm := range pending {
					r.Entries = append(r.Entries, Entry{Name: nm, Ival: iv, Line: ln})
					claimed = append(claimed, len(r.Entries)-1)
				}
				pending = nil
			}
			continue
		}

		// 条目行
		if m := dashRe.FindStringSubmatch(ln); m != nil {
			// 兑换窗口的块归属到此为止；攒的名字**不清**——连续多条条目行
			// 共用同一个窗口是常规形状（4550 招募段：旅人池 + 秘纹池两行，
			// 共用同一个「招募时间」窗口，参考实现与全量实测都这么算）。
			claimed = nil
			body := strings.TrimSpace(m[1])
			if fieldRe.MatchString(body) {
				continue
			}
			// 一行里可能并列两个池子（旅人 + 秘纹）
			if ps := poolRe.FindAllStringSubmatch(body, -1); len(ps) > 0 {
				for _, p := range ps {
					addName(strings.TrimSpace(p[1]))
				}
				continue
			}
			nm := body
			if q := nameRe.FindStringSubmatch(body); q != nil {
				nm = strings.TrimSpace(q[1])
			}
			nm = strings.Trim(nm, "：: 。，,、")
			if len([]rune(nm)) > 40 {
				nm = string([]rune(nm)[:40])
			}
			addName(nm)
			continue
		}

		// 含日期却没认出窗口
		if dateRe.MatchString(ln) && !ignoreDateRe.MatchString(ln) {
			r.Weird = append(r.Weird, ln)
		}
	}

	if len(r.Primary) > 1 {
		r.ExtraPrimary = r.Primary[1:]
	}

	// ---- 分派：三类公告，决定产出形态 ----
	// 2115 例外的内层判据不满足时要落下去试汇总，而 switch 的 case 命中就定死，
	// 所以汇总判据在那个分支里用 else-if 再写一遍。
	switch {
	case isVer:
		r.Provenance = ProvVersion
		r.Ver = buildVersionWindow(n.Title, r.Primary, r.Secondary)
		// 「[XXX]活动一览」实际上就是版本主活动自己的公告：方括号标题、正文只有
		// 档期两行、详情画在长图里。所以版本窗口之外，把主活动本身也产出为一条
		// 事件（海报=它的封面=版本主视觉），Merge 按同名同窗覆盖维护公告切出的
		// 那条无名海报的条目。窗口选取必须与版本边界同源，见 verMainIval。
		// 「[XXX]版本一览」正文是整张图，一行文字档期都没有：Ver==nil 且零窗口属
		// 已知形状不报警（见 Suspect），也不产事件。
		if r.Ver != nil {
			if a, ok := verMainIval(r.Primary); ok {
				r.Entries = []Entry{{Name: r.Ver.Name, Ival: a, Line: a.Line}}
				// 兑换行挂到主活动条目上：版本商店的兑换期随主活动事件带出，
				// 展示层画成它的领奖尾段（"兑换领奖期"图例的真实数据来源之一）。
				if b, ok := verRedeemIval(r.Secondary); ok {
					r.Entries[0].Redeem = &b
				}
			}
		}
	case r.Name != "" && len(r.Primary) == 1:
		r.Provenance = ProvSolo
		r.Entries = []Entry{{Name: r.Name, Ival: r.Primary[0], Line: r.Primary[0].Line}}
	case r.Name == "" && len(r.Primary) == 1 && strings.Contains(n.Title, "活动"):
		// 标题没用「」包活动名，但确实是活动公告（2115《星塔旅人》H5趣味网页活动里程碑
		// 奖励，标题用书名号、正文实打实只有一个主窗口）。判据刻意窄：
		//   1) 必须以《游戏名》开头且剥得掉——「超长名字…」这类引号残缺标题不能进来；
		//   2) 剥完不能含空白——「《星塔旅人》 × 星际传奇 联动活动开启！」剥完是句子
		//      不是名字，仍按非活动公告处理（v2 行为）。
		if nm := strings.TrimSpace(gamePrefixRe.ReplaceAllString(strings.TrimSpace(n.Title), "")); nm != strings.TrimSpace(n.Title) && !strings.ContainsAny(nm, " \t\u3000") {
			r.Provenance = ProvSolo
			r.Name = nm
			r.Entries = []Entry{{Name: r.Name, Ival: r.Primary[0], Line: r.Primary[0].Line}}
		} else if len(r.Primary)+len(r.Secondary) >= 2 && len(r.Entries) > 0 {
			r.Provenance = ProvSummary
			r.Entries = dedupeEntries(r.Entries)
		}
	case len(r.Primary)+len(r.Secondary) >= 2 && len(r.Entries) > 0:
		r.Provenance = ProvSummary
		r.Entries = dedupeEntries(r.Entries)
	}
	return r
}

// dedupeEntries 同一条公告内（名字, 窗口）全同的条目只留一条，有兑换行的优先——
// 4550 的「奋斗吧！」在剧情段（开放时间）与活动段（活动时间+兑换行）各出现一次，
// 同窗同名；带兑换行的才是活动本体（方案 §9.1 方案C 的判据，在唯一看得到
// Redeem 的这层做）。
func dedupeEntries(es []Entry) []Entry {
	type k struct{ n, s, e string }
	idx := map[k]int{}
	out := make([]Entry, 0, len(es))
	for _, e := range es {
		key := k{normEventName(e.Name), e.Ival.Start.Format(time.RFC3339), e.Ival.End.Format(time.RFC3339)}
		if i, ok := idx[key]; ok {
			if e.Redeem != nil && out[i].Redeem == nil {
				out[i] = e
			}
			continue
		}
		idx[key] = len(out)
		out = append(out, e)
	}
	return out
}

// buildVersionWindow 从「活动时间」+「活动商店与奖励兑换时间」两行造版本边界。
// 「[XXX]版本一览」正文常是整张图（两行都没有）→ 返回 nil，属已知形状不报警。
func buildVersionWindow(title string, prim, sec []Interval) *VersionWindow {
	a, ha := verMainIval(prim)
	if !ha {
		return nil
	}
	var b Interval
	hb := false
	for _, iv := range sec {
		if strings.Contains(iv.Tag, "兑换") || strings.Contains(iv.Tag, "兌换") {
			b = iv
			hb = true
			break
		}
	}
	v := &VersionWindow{ActStart: a.Start, PlayEnd: a.End}
	if hb {
		v.RedeemEnd = b.End
	} else {
		v.RedeemEnd = a.End // 没有兑换行：版本终点即玩法期止
	}
	if m := verNameRe.FindStringSubmatch(strings.TrimSpace(title)); m != nil {
		v.Name = strings.TrimSpace(m[1])
	}
	return v
}

// verRedeemIval 取版本公告的「兑换」窗口（活动商店与奖励兑换时间行）。
func verRedeemIval(sec []Interval) (Interval, bool) {
	for _, iv := range sec {
		if strings.Contains(iv.Tag, "兑换") || strings.Contains(iv.Tag, "兌换") {
			return iv, true
		}
	}
	return Interval{}, false
}

// verMainIval 取版本公告的「活动时间」主窗口：标签含"活动"的最后一个，没有则退到
// 最后一个主窗口。版本边界与主活动事件必须用同一条，两处选取不能各自漂移。
func verMainIval(prim []Interval) (Interval, bool) {
	var a Interval
	ha := false
	for _, iv := range prim {
		if strings.Contains(iv.Tag, "活动") || !ha {
			a = iv
			ha = true
		}
	}
	return a, ha
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
