// Package poster 把"时间轴上的事实"画成一张日历卡片。
//
// 它只吃输入事实（有哪些条带、哪个窗口、现在几点、几条泳道），不认识活动、版本、
// QQ 这些业务词：轨道叫什么、哪条带算周期玩法、要不要画领奖尾，都由调用方（功能层）
// 决定后用字段传进来。要改判断就改调用方，不在这里判断。
//
// 分两层：layout.go 是纯计算（只出数字，可单测），draw.go 才碰像素。
// layout.go 与网页模板 .probe/calpng/template.html 是同一套几何的两个独立实现，
// 由 .probe/calpng/parity.js 逐条带对账。
package poster

import (
	"sort"
	"time"
)

// 布局常量。改动必须同步模板，否则对账门会炸——这正是它存在的意义。
const (
	RowHeight = 52 // 一条带的高（名字可换行，行高吸收两个名字）
	RowGap    = 5
	DayMs     = int64(24 * time.Hour / time.Millisecond)
	// MinBandWidth 是兜底：不足一天的活动也留个能看的条，占 0.8 个日格宽。
	MinBandWidth = 0.8
	// NarrowDays：玩法段在本窗内不足这么多天的算"窄带"，名字要缩。
	NarrowDays = 4
)

// Track 是一条泳道分组的名字，由调用方给（模板里是 限定卡池/版本活动/周期玩法）。
type Track struct {
	Name string
}

// Band 是一条候选带。时间都是绝对时刻，没有"相对第几天"这种半成品。
type Band struct {
	ID    string
	Track int    // 索引 Input.Tracks
	Name  string // 活动名
	Label string // 条上的时间标签原文（"招募时间"…），渲染器原样画，不解释
	// Start 是玩法段起点。原文只写"维护结束后"时传官方占位（当日 00:00），
	// 并把 Fuzzy 置真——渲染器按 Input.Open 摆放，机器人查询口径不受影响。
	Start time.Time
	End   time.Time
	// ClaimEnd 是领奖/兑换窗终点，零值表示没有尾段。
	ClaimEnd time.Time
	// NoTail 真的带不画尾段：周期玩法的排名奖能领一个月，尾巴比本体还长，喧宾夺主。
	NoTail bool
	// Fuzzy 标记起点是估算值，渲染时叠 Open 并在名字前留"≈"。
	Fuzzy bool
	// MergeKey 相同的带允许并成一条（同档期的角色池+秘纹池）。空 = 永不并。
	// 谁来赋值是业务判断，渲染器只按"键相同则并"算。
	MergeKey string
	// Dropped = 被上游过滤掉的项，只在 ShowDropped 口径下出现。
	Dropped bool
	// Poster 是封面（data URI 或本地路径），空 = 画占位；Tint 是底色。
	Poster string
	Tint   string
}

// Window 是这一张图对应的那个版本窗口。
type Window struct {
	Key      string // 稳定键：ActStart 的 UTC yyyyMMddHHmm
	Name     string
	ActStart time.Time // 占位起点（当日 00:00），开闸估计由 Open 叠上去
	Redeem   time.Time // 兑换截止，决定这张图画多少天
}

// Input 是一次出图的全部输入。
type Input struct {
	Zone   *time.Location // 日历日按哪个时区切
	Now    time.Time
	Open   time.Duration // fuzzy 起点的开闸估计（与网页共用同一个数，由数据下发）
	Tracks []Track
	Window Window
	Bands  []Band
	// DayStart 非零表示"游戏日"口径：日格从该时刻起列（模板里 04:00）。
	DayStart    time.Duration
	Merge       bool // 关掉后每条带各占一条（对比用）
	ShowDropped bool // 画出被过滤项
}

// Placed 是一条带画完后的位置与形态。百分比相对日轴内框，像素相对泳道容器。
type Placed struct {
	Items []Band // 并成这条带的成员，顺序同输入
	Track int
	Lane  int // 组内 1 起

	Left   float64 // %
	Width  float64 // %
	Top    int     // px
	Height int     // px

	// 尾段（领奖/兑换）单独一条斜纹；TailWidth 为 0 表示没有尾。
	TailLeft  float64
	TailWidth float64

	EndsDay time.Time // 右下角显示的那个日历日
	// 形态标记。
	OverLeft  bool // 左端不封口：延续自本窗之前
	OverRight bool // 右端不封口：延续到本窗之后
	Past      bool // 玩法段已结束
	Narrow    bool
}

// IDs 是这条带包含的成员，供对账与调试。
func (p Placed) IDs() []string {
	out := make([]string, len(p.Items))
	for i, b := range p.Items {
		out[i] = b.ID
	}
	return out
}

// Frame 是一次布局的结果。
type Frame struct {
	Window Window
	G0     int64 // 日轴左缘
	G1     int64 // 日轴右缘
	Days   int   // 日格数（补齐整周）
	// W0 是本窗判定左缘 = ActStart + Open：终点早于它的都不进这张图。
	W0    int64
	Lanes []int // 各轨道用了几条泳道，索引同 Tracks
	Bands []Placed
	// NowPct 是出图时刻线在日轴上的百分比；nil = 此刻不在本窗内，不画线。
	NowPct *float64
}

// Layout 把输入算成位置。它与模板的 frame()+groups()+pack()+band() 一一对应，
// 任何一边改动对账门都会炸。
func Layout(in Input) Frame {
	zone := in.Zone
	if zone == nil {
		zone = time.UTC
	}
	t0 := in.Window.ActStart.UnixMilli()
	g0 := startOfDay(t0, zone)
	if reset := millis(in.DayStart); reset != 0 {
		g0 += reset
	}
	// 补齐整周，不留只剩 1 天的周卡。
	days := int(ceilDiv(startOfDay(in.Window.Redeem.UnixMilli(), zone)+DayMs-g0, 7*DayMs) * 7)
	if days < 7 {
		days = 7
	}
	g1 := g0 + int64(days)*DayMs
	w0 := t0 + millis(in.Open)

	f := Frame{Window: in.Window, G0: g0, G1: g1, Days: days, W0: w0, Lanes: make([]int, len(in.Tracks))}
	pct := func(ms int64) float64 {
		v := ms
		if v < g0 {
			v = g0
		}
		if v > g1 {
			v = g1
		}
		return float64(v-g0) / float64(g1-g0) * 100
	}

	groups := groupBands(in, g0, g1, w0)
	byTrack := make([][]*group, len(in.Tracks))
	for _, g := range groups {
		if g.track >= 0 && g.track < len(in.Tracks) {
			byTrack[g.track] = append(byTrack[g.track], g)
		}
	}
	cw := 100 / float64(days)
	for ti, gs := range byTrack {
		f.Lanes[ti] = pack(gs)
		for _, g := range gs {
			f.Bands = append(f.Bands, g.place(in, g0, g1, pct, cw))
		}
	}
	if ms := in.Now.UnixMilli(); ms >= g0 && ms < g1 {
		v := pct(ms)
		f.NowPct = &v
	}
	return f
}

type group struct {
	items []Band
	track int
	s0    int64
	e0    int64
	x1    int64
	lane  int
}

// group 做三件事：按真实时刻过滤出本窗看得见的带、按 MergeKey 并带、
// 算出每条带的玩法段 [s0,e0] 与含尾段的右缘 x1。
func groupBands(in Input, g0, g1, w0 int64) []*group {
	var (
		order []*group
		by    = map[string]*group{}
	)
	for _, b := range in.Bands {
		s0 := b.Start.UnixMilli()
		if b.Fuzzy {
			s0 += millis(in.Open)
		}
		e0 := b.End.UnixMilli()
		// 唯一一条可见性判据：玩法段与本窗 [w0,g1) 有交集。维护前收档的活动自然
		// 停在缝的左边，不需要"残留""回看"这类特判。
		if e0 <= w0 || s0 >= g1 {
			continue
		}
		if b.Dropped && !in.ShowDropped {
			continue
		}
		key := b.MergeKey
		if b.Dropped || key == "" {
			key = "solo:" + b.ID
		}
		if g, ok := by[key]; in.Merge && ok {
			g.add(b, s0, e0)
			continue
		}
		g := &group{items: []Band{b}, track: b.Track, s0: s0, e0: e0, x1: tailEnd(b, e0)}
		by[key] = g
		order = append(order, g)
	}
	for _, g := range order {
		if g.x1 < g.e0 {
			g.x1 = g.e0
		}
	}
	return order
}

func (g *group) add(b Band, s0, e0 int64) {
	g.items = append(g.items, b)
	if s0 < g.s0 {
		g.s0 = s0
	}
	if e0 > g.e0 {
		g.e0 = e0
	}
	if t := tailEnd(b, e0); t > g.x1 {
		g.x1 = t
	}
}

// tailEnd 是这条带含尾段的右缘；NoTail 或没有领奖窗的只到玩法期止。
func tailEnd(b Band, e0 int64) int64 {
	if b.NoTail || b.ClaimEnd.IsZero() {
		return e0
	}
	return max(e0, b.ClaimEnd.UnixMilli())
}

// pack 按真实时刻装箱，就地写回 lane，返回用掉的泳道数。
func pack(gs []*group) int {
	// 模板那边是 JS 的稳定排序，Go 的 sort.Slice 不稳定 —— 必须用 SliceStable，
	// 否则同起点同右端的两条带行序随机，两边对不上。
	sort.SliceStable(gs, func(i, j int) bool {
		if gs[i].s0 != gs[j].s0 {
			return gs[i].s0 < gs[j].s0
		}
		return gs[i].x1 < gs[j].x1
	})
	var free []int64
	for _, g := range gs {
		at := -1
		for j, x := range free {
			if x <= g.s0 {
				at = j
				break
			}
		}
		if at < 0 {
			free = append(free, g.x1)
			g.lane = len(free)
			continue
		}
		free[at] = g.x1
		g.lane = at + 1
	}
	return len(free)
}

func (g *group) place(in Input, g0, g1 int64, pct func(int64) float64, cw float64) Placed {
	left := pct(g.s0)
	body := pct(g.e0)
	all := pct(g.x1)
	hasTail := all > body+1e-9
	// 兜底：不足一天的活动也留个能看的条（从右端往回撑到最小宽）。
	if !hasTail && body-left < MinBandWidth*cw {
		left = max(0, body-MinBandWidth*cw)
	}
	width := all - left
	if hasTail {
		width = body - left
	}
	p := Placed{
		Items: g.items, Track: g.track, Lane: g.lane,
		Left: left, Width: width, Top: (g.lane - 1) * (RowHeight + RowGap), Height: RowHeight,
		TailLeft: body, TailWidth: max(0, all-body),
		EndsDay:   endsDay(in, g.x1),
		OverLeft:  g.s0 < g0,
		OverRight: g.x1 > g1,
		Past:      g.e0 <= in.Now.UnixMilli(),
		Narrow:    float64(g.e0-max(g.s0, g0))/float64(DayMs) <= NarrowDays,
	}
	return p
}

// endsDay 是右下角那个日期：自然日口径取当天 00:00，游戏日口径取最后一个完整游戏日
// （可能比官方文案早一天，那是口径本身决定的，不是 bug）。
func endsDay(in Input, ms int64) time.Time {
	zone := in.Zone
	if zone == nil {
		zone = time.UTC
	}
	if reset := millis(in.DayStart); reset != 0 {
		ms -= reset
	}
	return time.UnixMilli(startOfDay(ms, zone)).In(zone)
}

func startOfDay(ms int64, zone *time.Location) int64 {
	d := time.UnixMilli(ms).In(zone)
	y, m, day := d.Date()
	return time.Date(y, m, day, 0, 0, 0, 0, zone).UnixMilli()
}

func millis(d time.Duration) int64 { return int64(d / time.Millisecond) }

func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}
