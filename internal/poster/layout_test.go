package poster

import (
	"testing"
	"time"
)

var cstZone = time.FixedZone("CST", 8*3600)

func mustTime(s string) time.Time {
	v, err := time.ParseInLocation("2006-01-02 15:04", s, cstZone)
	if err != nil {
		panic(err)
	}
	return v
}

// 基准窗：09-08 00:00 开（占位；估算 17:00 开闸），兑换止 09-29 10:59 → 补齐 4 周 = 28 格。
const (
	baseG0   = "2026-09-08 00:00"
	baseDays = 28
)

func base(t *testing.T) Input {
	t.Helper()
	return Input{
		Zone:   cstZone,
		Now:    mustTime("2026-09-08 20:00"),
		Open:   17 * time.Hour,
		Tracks: []Track{{Name: "卡池"}, {Name: "活动"}, {Name: "周期"}},
		Window: Window{Key: "202609071600", Name: "V",
			ActStart: mustTime(baseG0), Redeem: mustTime("2026-09-29 10:59")},
		Merge:       true,
		ShowDropped: true,
	}
}

// pctOf 把"某个时刻在日轴上的百分比"写成时刻本身，免得期望值靠手算小数。
// 它只重做"两个时刻之差除以总长"这一步算术；哪两个时刻才是断言的内容。
func pctOf(s string) float64 {
	return float64(mustTime(s).UnixMilli()-mustTime(baseG0).UnixMilli()) / float64(baseDays*DayMs) * 100
}

func diff(a, b float64) float64 {
	if a < b {
		return b - a
	}
	return a - b
}

func near(a, b float64) bool { return diff(a, b) < 1e-9 }

func band(id string, track int, start, end string) Band {
	return Band{ID: id, Track: track, Name: id, Start: mustTime(start), End: mustTime(end)}
}

func ids(f Frame) [][]string {
	out := make([][]string, len(f.Bands))
	for i, b := range f.Bands {
		out[i] = b.IDs()
	}
	return out
}

func join(xs [][]string) string {
	s := ""
	for _, g := range xs {
		if s != "" {
			s += " "
		}
		for i, x := range g {
			if i > 0 {
				s += ","
			}
			s += x
		}
	}
	return s
}

func TestFrameWindow(t *testing.T) {
	f := Layout(base(t))
	if f.Days != baseDays {
		t.Errorf("日格数 = %d, 想 %d（补齐整周）", f.Days, baseDays)
	}
	if got, want := f.G0, mustTime(baseG0).UnixMilli(); got != want {
		t.Errorf("日轴左缘 = %d, 想 %d", got, want)
	}
	if got, want := f.G1-f.G0, int64(baseDays*DayMs); got != want {
		t.Errorf("日轴跨度 = %d, 想 %d", got, want)
	}
	// 本窗判定左缘 = 占位起点 + 开闸估计：库里的 00:00 只是下界。
	if got, want := f.W0-f.G0, int64(17*time.Hour/time.Millisecond); got != want {
		t.Errorf("W0 离左缘 = %dms, 想 %dms", got, want)
	}
	if f.NowPct == nil {
		t.Fatal("NowPct = nil, 但此刻在本窗内")
	}
	if want := pctOf("2026-09-08 20:00"); !near(*f.NowPct, want) {
		t.Errorf("NowPct = %v, 想 %v", *f.NowPct, want)
	}
	// 只开 2 天的窗也补齐成一周。
	in := base(t)
	in.Window.Redeem = mustTime("2026-09-09 10:00")
	if got := Layout(in).Days; got != 7 {
		t.Errorf("短窗补齐后 = %d 格, 想 7", got)
	}
}

func TestNowLineOutsideWindow(t *testing.T) {
	in := base(t)
	in.Now = mustTime("2026-10-08 00:00")
	if f := Layout(in); f.NowPct != nil {
		t.Errorf("NowPct = %v, 想 nil（不在本窗内就不画线）", *f.NowPct)
	}
}

// 可见性只有那一条判据：玩法段与本窗 [w0,g1) 有交集。
func TestVisibility(t *testing.T) {
	in := base(t)
	in.Bands = []Band{
		band("收在开闸前", 1, "2026-09-07 12:00", "2026-09-08 16:59"),
		band("正好压在开闸", 1, "2026-09-08 12:00", "2026-09-08 17:00"),
		band("开闸后一分钟", 1, "2026-09-08 12:00", "2026-09-08 17:01"),
		band("起点在右缘", 1, "2026-10-06 00:00", "2026-10-08 00:00"),
	}
	if got := join(ids(Layout(in))); got != "开闸后一分钟" {
		t.Errorf("可见带 = %q, 想只剩「开闸后一分钟」（终点早于 w0 的不该被捞回来）", got)
	}
}

// fuzzy 起点按开闸估计摆；成员标记要留着，名字前的"≈"靠它。
func TestFuzzyStart(t *testing.T) {
	in := base(t)
	b := band("f", 1, "2026-09-08 00:00", "2026-09-10 00:00")
	b.Fuzzy = true
	in.Bands = []Band{b}
	p := Layout(in).Bands[0]
	if want := pctOf("2026-09-08 17:00"); !near(p.Left, want) {
		t.Errorf("fuzzy 带 left = %v, 想 %v（00:00 是占位，按 17:00 摆）", p.Left, want)
	}
	if !p.Items[0].Fuzzy {
		t.Error("成员的 Fuzzy 标记丢了，名字前的 ≈ 会画不出来")
	}
	// 不 fuzzy 的同一条带就老实停在 00:00。
	in.Bands = []Band{band("f", 1, "2026-09-08 00:00", "2026-09-10 00:00")}
	if p := Layout(in).Bands[0]; p.Left != 0 {
		t.Errorf("非 fuzzy 带 left = %v, 想 0", p.Left)
	}
}

func TestTailAndNoTail(t *testing.T) {
	mk := func(track int, noTail bool) Band {
		b := band("x", track, "2026-09-10 00:00", "2026-09-12 03:59")
		b.ClaimEnd = mustTime("2026-09-19 10:59")
		b.NoTail = noTail
		return b
	}
	in := base(t)
	in.Bands = []Band{mk(1, false)}
	p := Layout(in).Bands[0]
	if !near(p.Left, pctOf("2026-09-10 00:00")) {
		t.Errorf("left = %v, 想 %v", p.Left, pctOf("2026-09-10 00:00"))
	}
	// 实心段止于玩法期止，尾段单独一条到兑换止。
	wantW := pctOf("2026-09-12 03:59") - pctOf("2026-09-10 00:00")
	if !near(p.Width, wantW) {
		t.Errorf("实心段宽 = %v, 想 %v", p.Width, wantW)
	}
	if !near(p.TailLeft, p.Left+p.Width) {
		t.Errorf("尾段左缘 %v != 实心段右缘 %v", p.TailLeft, p.Left+p.Width)
	}
	wantTW := pctOf("2026-09-19 10:59") - pctOf("2026-09-12 03:59")
	if !near(p.TailWidth, wantTW) {
		t.Errorf("尾段宽 = %v, 想 %v", p.TailWidth, wantTW)
	}

	in.Bands = []Band{mk(2, true)}
	p = Layout(in).Bands[0]
	if p.TailWidth != 0 {
		t.Errorf("NoTail 的带尾段宽 = %v, 想 0（周期玩法的排名奖尾巴比本体长）", p.TailWidth)
	}
	if !near(p.Left+p.Width, pctOf("2026-09-12 03:59")) {
		t.Errorf("NoTail 的带右缘 = %v, 想停在玩法期止 %v", p.Left+p.Width, pctOf("2026-09-12 03:59"))
	}
}

// 早于玩法期止的领奖窗不该把带缩短。
func TestTailNeverShrinks(t *testing.T) {
	in := base(t)
	b := band("x", 1, "2026-09-10 00:00", "2026-09-12 00:00")
	b.ClaimEnd = mustTime("2026-09-11 00:00")
	in.Bands = []Band{b}
	p := Layout(in).Bands[0]
	if p.TailWidth != 0 {
		t.Errorf("尾段宽 = %v, 想 0", p.TailWidth)
	}
	if !near(p.Left+p.Width, pctOf("2026-09-12 00:00")) {
		t.Errorf("右缘 = %v, 想 %v（按玩法期止）", p.Left+p.Width, pctOf("2026-09-12 00:00"))
	}
}

// 装箱：首尾相接同一条道，真重叠哪怕一刻才另起一行。
func TestPack(t *testing.T) {
	in := base(t)
	in.Bands = []Band{
		band("A", 1, "2026-09-10 00:00", "2026-09-12 00:00"),
		band("B", 1, "2026-09-12 00:00", "2026-09-14 00:00"),
		band("C", 1, "2026-09-13 00:00", "2026-09-15 00:00"),
	}
	f := Layout(in)
	if got := f.Lanes[1]; got != 2 {
		t.Errorf("泳道数 = %d, 想 2", got)
	}
	want := map[string]int{"A": 1, "B": 1, "C": 2}
	for _, p := range f.Bands {
		id := p.IDs()[0]
		if p.Lane != want[id] {
			t.Errorf("%s 泳道 = %d, 想 %d", id, p.Lane, want[id])
		}
		if p.Top != (want[id]-1)*(RowHeight+RowGap) {
			t.Errorf("%s top = %d, 想 %d", id, p.Top, (want[id]-1)*(RowHeight+RowGap))
		}
	}
}

// 同起点同右端的带按输入序排：装箱用的是稳定排序，换了不稳定版本行序会随机。
func TestPackStable(t *testing.T) {
	in := base(t)
	dup := func(id string) Band {
		return band(id, 1, "2026-09-10 00:00", "2026-09-12 00:00")
	}
	in.Bands = []Band{dup("先"), dup("后")}
	f := Layout(in)
	if got := join(ids(f)); got != "先 后" {
		t.Errorf("行序 = %q, 想「先 后」（按输入序）", got)
	}
	if f.Bands[0].Lane != 1 || f.Bands[1].Lane != 2 {
		t.Errorf("两条真重叠的带各占一行，得到 %d/%d", f.Bands[0].Lane, f.Bands[1].Lane)
	}
}

// 并带：同键才并；被过滤的与没有键的不并；关掉 Merge 各画各的。
func TestMerge(t *testing.T) {
	mk := func(id, key string, dropped bool) Band {
		b := band(id, 0, "2026-09-10 00:00", "2026-09-12 00:00")
		b.MergeKey = key
		b.Dropped = dropped
		return b
	}
	in := base(t)
	in.Bands = []Band{mk("a", "k1", false), mk("b", "k1", false), mk("c", "", false), mk("d", "k1", true)}
	f := Layout(in)
	if got := join(ids(f)); got != "a,b c d" {
		t.Errorf("并带结果 = %q, 想「a,b c d」", got)
	}
	if p := f.Bands[0]; !near(p.Left, pctOf("2026-09-10 00:00")) || !near(p.Width, pctOf("2026-09-12 00:00")-pctOf("2026-09-10 00:00")) {
		t.Errorf("并成的带几何 = %v/%v", p.Left, p.Width)
	}
	if len(f.Bands[0].Items) != 2 {
		t.Errorf("成员数 = %d, 想 2（一条带上要画两个名字）", len(f.Bands[0].Items))
	}
	in.Merge = false
	if got := join(ids(Layout(in))); got != "a b c d" {
		t.Errorf("Merge=false 时 = %q, 想各画各的", got)
	}
	in.Merge = true
	in.ShowDropped = false
	if got := join(ids(Layout(in))); got != "a,b c" {
		t.Errorf("ShowDropped=false 时 = %q, 想滤掉 d", got)
	}
}

// 并成的带取成员的最小起点、最大玩法止、最大含尾右缘。
func TestMergeExtents(t *testing.T) {
	mk := func(id, s, e, ce string) Band {
		b := band(id, 0, s, e)
		b.MergeKey = "k"
		if ce != "" {
			b.ClaimEnd = mustTime(ce)
		}
		return b
	}
	in := base(t)
	in.Bands = []Band{
		mk("a", "2026-09-11 00:00", "2026-09-13 00:00", "2026-09-20 10:59"),
		mk("b", "2026-09-10 00:00", "2026-09-12 00:00", ""),
	}
	p := Layout(in).Bands[0]
	if !near(p.Left, pctOf("2026-09-10 00:00")) {
		t.Errorf("左缘 = %v, 想取成员最小起点 %v", p.Left, pctOf("2026-09-10 00:00"))
	}
	if !near(p.Left+p.Width, pctOf("2026-09-13 00:00")) {
		t.Errorf("实心段右缘 = %v, 想取成员最大玩法止 %v", p.Left+p.Width, pctOf("2026-09-13 00:00"))
	}
	if !near(p.TailLeft+p.TailWidth, pctOf("2026-09-20 10:59")) {
		t.Errorf("尾段右缘 = %v, 想 %v", p.TailLeft+p.TailWidth, pctOf("2026-09-20 10:59"))
	}
}

// 不足一天的活动从右端往回撑到 0.8 个日格宽；贴左缘时不越过 0。
func TestMinWidth(t *testing.T) {
	in := base(t)
	in.Bands = []Band{
		band("中段", 1, "2026-09-10 00:00", "2026-09-10 06:00"),
		band("贴左", 1, "2026-09-08 17:00", "2026-09-08 18:00"),
	}
	got := map[string]Placed{}
	for _, p := range Layout(in).Bands {
		got[p.IDs()[0]] = p
	}
	minW := MinBandWidth * (100.0 / baseDays)
	mid := got["中段"]
	if !near(mid.Width, minW) {
		t.Errorf("短带宽 = %v, 想兜底到 %v", mid.Width, minW)
	}
	if want := pctOf("2026-09-10 06:00"); !near(mid.Left+mid.Width, want) {
		t.Errorf("兜底该从右端往回撑：右缘 = %v, 想停在真实终点 %v", mid.Left+mid.Width, want)
	}
	if left := got["贴左"]; left.Left != 0 {
		t.Errorf("贴左缘的带被撑到 %v, 想 0", left.Left)
	}
}

// 形态标记：灰化、窄带、两端不封口。
func TestFlags(t *testing.T) {
	in := base(t)
	in.Bands = []Band{
		band("已结束", 1, "2026-09-08 18:00", "2026-09-08 19:00"),
		band("延续进来", 1, "2026-09-06 00:00", "2026-09-12 00:00"),
		band("延续出去", 1, "2026-09-20 00:00", "2026-10-08 00:00"),
	}
	got := map[string]Placed{}
	for _, p := range Layout(in).Bands {
		got[p.IDs()[0]] = p
	}
	if e := got["已结束"]; !e.Past || !e.Narrow {
		t.Errorf("已结束的带 = %+v, 想 Past 且 Narrow（6 小时）", e)
	}
	if o := got["延续进来"]; !o.OverLeft || o.OverRight {
		t.Errorf("延续进来的带 = %+v, 想只有 OverLeft", o)
	}
	if o := got["延续出去"]; !o.OverRight || o.Past {
		t.Errorf("延续出去的带 = %+v, 想 OverRight 且还在进行", o)
	}
	if len(in.Bands) != len(got) {
		t.Errorf("带数 %d != 地图 %d，有两条撞了同一个 ID", len(in.Bands), len(got))
	}
}

// 右下角日期：自然日取当天，游戏日口径取最后一个完整游戏日（可能早一天）。
func TestEndsDay(t *testing.T) {
	in := base(t)
	in.Bands = []Band{band("x", 1, "2026-09-10 00:00", "2026-09-12 03:59")}
	if got := Layout(in).Bands[0].EndsDay.Format("01-02"); got != "09-12" {
		t.Errorf("自然日口径 = %s, 想 09-12", got)
	}
	in.DayStart = 4 * time.Hour
	if got := Layout(in).Bands[0].EndsDay.Format("01-02"); got != "09-11" {
		t.Errorf("游戏日口径 = %s, 想 09-11（03:59 还没到新一格）", got)
	}
}
