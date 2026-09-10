// Package calops 是「活动数据运维」功能：看解析不出来的那些、人工改时间、把值固化下来。
//
// 分层契约的执行点：这个功能**自己决定**存什么——人工覆盖写进 annsync 的 OverrideStore
// （底层是 store.Doc 的 "override" 命名空间），基础设施既不知道"覆盖"这个概念，
// 也不为它预建 schema。刷新永不删除覆盖，每轮投影重新应用，所以覆盖值不会被同步冲掉。
//
// 全部命令都是管理员命令：改错了会让整个群看到错的活动时间。
package calops

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/feature/text"
	"xingta/internal/kernel"
	"xingta/internal/qq"
	"xingta/internal/store"
)

// Refresher 是可选依赖：接上同步引擎（annsync.Syncer）就能在写完覆盖后立刻重投影，
// 不用等下一轮。nil 表示"等下一轮"，命令照样可用。
type Refresher interface{ Refresh() }

// Config 的零值必须可用。
type Config struct {
	Source   string // 只看某个源，空 = 所有源
	Zone     *time.Location
	PageSize int
	Now      func() time.Time
	Refresh  Refresher
	Logf     func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.Zone == nil {
		c.Zone = time.FixedZone("CST", 8*60*60)
	}
	if c.PageSize <= 0 {
		c.PageSize = 6
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

type feature struct {
	doc store.Doc
	ov  annsync.OverrideStore
	reg command.Registrar
	cfg Config
}

// New 造出功能。存储面在这里被功能自己切成两块用途：读活动记录（待确认桶）、
// 写人工覆盖（OverrideStore）。
func New(doc store.Doc, reg command.Registrar, cfg Config) kernel.Feature {
	c := cfg.withDefaults()
	return &feature{doc: doc, ov: annsync.NewOverrideStore(doc), reg: reg, cfg: c}
}

func (f *feature) Name() string { return "calops" }

func (f *feature) Start(ctx context.Context, api kernel.API) error {
	cmds := []command.Cmd{
		{Name: "review", Admin: true,
			Usage: "列出解析存疑的活动，可加页码：review 2", Run: command.Text(f.review)},
		{Name: "override", Admin: true,
			Usage: "改时间：override <id> <开始> <结束> [备注]，时间写 2026-09-08 10:59", Run: command.Text(f.override)},
		{Name: "confirm", Admin: true,
			Usage: "把当前解析值固化下来：confirm <id>", Run: command.Text(f.confirm)},
		{Name: "hide", Admin: true,
			Usage: "把一条记录从日历里撤下：hide <id> [备注]", Run: command.Text(f.hide)},
		{Name: "show", Admin: true,
			Usage: "撤销隐藏：show <id>", Run: command.Text(f.show)},
	}
	for _, c := range cmds {
		if err := f.reg.Add(c); err != nil {
			return err
		}
	}
	return nil
}

// review 列待确认桶：状态不是 ok / fuzzy_start 的记录，加上"整条可疑但一个区间都没
// 解析出来"的公告——后者只会出现在条目快照里，不列出来这类漏抓是静默的。
//
// 30 天（RetryAge）以外的旧公告不占页面：全量实测 345 篇里 31 篇未采信，九成是
// 半年前的维护说明，旧公告天天占满第一页这个命令就等于没有。省下的只在尾巴上
// 交代数量；近 30 天的按发布时间新→旧排，每行带发布时间，人工好判断还要不要看。
func (f *feature) review(ctx context.Context, m *qq.Message, args []string) (string, error) {
	pending, err := annsync.ReadPending(f.doc, f.cfg.Source)
	if err != nil {
		return "", fmt.Errorf("读待确认记录: %w", err)
	}
	items, err := annsync.ReadItems(f.doc, f.cfg.Source)
	if err != nil {
		return "", fmt.Errorf("读公告快照: %w", err)
	}

	rows := make([]string, 0, len(pending)+len(items))
	for _, r := range pending {
		rows = append(rows, fmt.Sprintf("· %s\n  %s［%s］%s",
			r.ID, r.Title, r.Status, text.Clip(text.OneLine(r.Fragment), 60)))
	}

	now := f.cfg.Now()
	var ancient, recent []annsync.Item
	for _, it := range items {
		if !it.Suspect || len(it.Events) > 0 {
			continue // 有子活动的可疑公告，问题已经体现在上面那些记录里了
		}
		if !it.Published.IsZero() && now.Sub(it.Published) >= annsync.RetryAge {
			ancient = append(ancient, it)
			continue
		}
		recent = append(recent, it)
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].Published.After(recent[j].Published) })
	for _, it := range recent {
		label := it.Ref.ID
		if f.cfg.Source != "" {
			label = f.cfg.Source + ":" + it.Ref.ID // 人工拿"源:公告号"去官网核对
		}
		note := it.Note
		if note == "" {
			note = "整条没解析出时间，日期多半在图片里"
		}
		when := "发布时间未知"
		if !it.Published.IsZero() {
			when = it.Published.In(f.cfg.Zone).Format("01-02 15:04") + " 发"
		}
		rows = append(rows, fmt.Sprintf("· %s\n  %s %s［%s］",
			label, when, text.OneLine(it.Title), note))
	}

	page, _ := pageArg(args)
	if len(rows) == 0 {
		if len(ancient) > 0 {
			return fmt.Sprintf("30 天内没有待确认的；更早未采信的 %d 条没列出来", len(ancient)), nil
		}
		return "待确认桶是空的：所有活动都解析干净了", nil
	}
	out := renderPaged("待确认", rows, page, f.cfg.PageSize, "review")
	if len(ancient) > 0 {
		out += fmt.Sprintf("\n更早未采信的 %d 条没列出来（超过 30 天的旧公告）", len(ancient))
	}
	return out, nil
}

func (f *feature) override(ctx context.Context, m *qq.Message, args []string) (string, error) {
	id, rest, err := f.takeID(args)
	if err != nil {
		return err.Error(), nil
	}
	rec, ok, err := annsync.ReadRec(f.doc, id)
	if err != nil {
		return "", fmt.Errorf("读活动记录: %w", err)
	}
	if !ok {
		return f.notFound(id), nil
	}

	start, rest, err := takeTime(rest, f.cfg.Now(), f.cfg.Zone, false)
	if err != nil {
		return "开始时间没看懂：" + err.Error() + "\n写法：override " + id + " 2026-09-08 10:59 2026-09-22 10:59", nil
	}
	end, rest, err := takeTime(rest, f.cfg.Now(), f.cfg.Zone, true)
	if err != nil {
		return "结束时间没看懂：" + err.Error() + "\n写法：override " + id + " 2026-09-08 10:59 2026-09-22 10:59", nil
	}
	if !end.After(start) {
		return fmt.Sprintf("结束时间 %s 不在开始时间 %s 之后，这样写日历上会显示不出来",
			f.when(end), f.when(start)), nil
	}

	old, _, _ := f.ov.Get(id)
	o := annsync.Override{
		Start: start, End: end, Title: old.Title, Hide: false,
		Note: strings.TrimSpace(strings.Join(rest, " ")),
		By:   m.Display(), At: f.cfg.Now(),
	}
	if err := f.ov.Put(id, o); err != nil {
		return "", fmt.Errorf("写覆盖: %w", err)
	}
	f.touch()

	return fmt.Sprintf("已覆盖 %s\n  %s\n  原解析：%s［%s］\n  改为：%s → %s%s",
		id, text.OneLine(rec.Title), oldStartEnd(rec, f), rec.Status,
		f.when(start), f.when(end), noteSuffix(o.Note)), nil
}

// confirm 把当前解析值固化成覆盖：官方改口或解析抖动时，人工核过的那一份不再被冲掉。
func (f *feature) confirm(ctx context.Context, m *qq.Message, args []string) (string, error) {
	id, rest, err := f.takeID(args)
	if err != nil {
		return err.Error(), nil
	}
	rec, ok, err := annsync.ReadRec(f.doc, id)
	if err != nil {
		return "", fmt.Errorf("读活动记录: %w", err)
	}
	if !ok {
		return f.notFound(id), nil
	}
	if rec.Start.IsZero() || rec.End.IsZero() {
		return "这条记录还没有解析出时间，没法固化；用「override」直接写时间", nil
	}

	old, _, _ := f.ov.Get(id)
	o := annsync.Override{
		Start: rec.Start, End: rec.End, Title: old.Title, Hide: old.Hide,
		Note: firstNonEmpty(strings.TrimSpace(strings.Join(rest, " ")), "人工确认解析值"),
		By:   m.Display(), At: f.cfg.Now(),
	}
	if err := f.ov.Put(id, o); err != nil {
		return "", fmt.Errorf("写覆盖: %w", err)
	}
	f.touch()

	return fmt.Sprintf("已固化 %s\n  %s\n  %s → %s%s",
		id, text.OneLine(rec.Title), f.when(o.Start), f.when(o.End), noteSuffix(o.Note)), nil
}

func (f *feature) hide(ctx context.Context, m *qq.Message, args []string) (string, error) {
	id, rest, err := f.takeID(args)
	if err != nil {
		return err.Error(), nil
	}
	rec, ok, err := annsync.ReadRec(f.doc, id)
	if err != nil {
		return "", fmt.Errorf("读活动记录: %w", err)
	}
	if !ok {
		return f.notFound(id), nil
	}

	old, _, _ := f.ov.Get(id)
	o := annsync.Override{
		Start: old.Start, End: old.End, Title: old.Title, Hide: true,
		Note: firstNonEmpty(strings.TrimSpace(strings.Join(rest, " ")), old.Note),
		By:   m.Display(), At: f.cfg.Now(),
	}
	if err := f.ov.Put(id, o); err != nil {
		return "", fmt.Errorf("写覆盖: %w", err)
	}
	f.touch()
	return fmt.Sprintf("已隐藏 %s（%s）\n发「show %s」可以撤销", id, text.OneLine(rec.Title), id), nil
}

func (f *feature) show(ctx context.Context, m *qq.Message, args []string) (string, error) {
	id, _, err := f.takeID(args)
	if err != nil {
		return err.Error(), nil
	}
	old, ok, err := f.ov.Get(id)
	if err != nil {
		return "", fmt.Errorf("读覆盖: %w", err)
	}
	if !ok || !old.Hide {
		return "这条本来就没被隐藏，不用撤销", nil
	}
	old.Hide = false
	old.By = m.Display()
	old.At = f.cfg.Now()
	if err := f.ov.Put(id, old); err != nil {
		return "", fmt.Errorf("写覆盖: %w", err)
	}
	f.touch()
	return "已恢复显示 " + id, nil
}

// touch 让覆盖立刻生效：接了同步引擎就请它重投影一次，没接就等下一轮。
func (f *feature) touch() {
	if f.cfg.Refresh == nil {
		return
	}
	f.cfg.Refresh.Refresh()
}

func (f *feature) notFound(id string) string {
	return fmt.Sprintf("找不到 %s：先用「review」看看有哪些 ID", id)
}

// takeID 取第一个参数当记录 ID，并要求它带源前缀（"stellasora:4432:0"）。
// 少写一段是最常见的打错，直接指出来比让后面的时间解析报莫名其妙的错好。
func (f *feature) takeID(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("缺少记录 ID")
	}
	id := args[0]
	if strings.Count(id, ":") < 2 {
		return "", nil, fmt.Errorf("ID %q 看着不完整，形状应当是 源:公告号:序号，例如 stellasora:4432:0", id)
	}
	return id, args[1:], nil
}

func (f *feature) when(t time.Time) string {
	t = t.In(f.cfg.Zone)
	if t.IsZero() {
		return "（未设置）"
	}
	if t.Year() == f.cfg.Now().In(f.cfg.Zone).Year() {
		return t.Format("01-02 15:04")
	}
	return t.Format("2006-01-02 15:04")
}

func oldStartEnd(r annsync.Rec, f *feature) string {
	if r.Start.IsZero() && r.End.IsZero() {
		return "无时间"
	}
	return f.when(r.Start) + " → " + f.when(r.End)
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return "\n  备注：" + note
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// renderPaged 把一个列表合成一条回复。分页在这里做（命令知道总条数），
// 机制层只兜单条长度上限。
func renderPaged(title string, rows []string, page, size int, cmd string) string {
	pages := (len(rows) + size - 1) / size
	if page > pages {
		page = pages
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * size
	end := min(start+size, len(rows))

	var b strings.Builder
	fmt.Fprintf(&b, "%s（%d 条，第 %d/%d 页）\n", title, len(rows), page, pages)
	for _, row := range rows[start:end] {
		b.WriteString(row + "\n")
	}
	if page < pages {
		fmt.Fprintf(&b, "发「%s %d」看下一页", cmd, page+1)
	}
	return strings.TrimRight(b.String(), "\n")
}

func pageArg(args []string) (int, []string) {
	for i, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil {
			continue
		}
		if n < 1 {
			n = 1
		}
		return n, append(append([]string(nil), args[:i]...), args[i+1:]...)
	}
	return 1, args
}

// takeTime 从参数头部吃掉一个时间点。日期与时刻在 QQ 里通常是两个字段
// （"覆盖 id 2026-09-08 10:59 …"），所以要先试"日期+时刻"两字段，再退化成单字段。
// 只写日期时：开始按 00:00，结束按 23:59:59（含当天整天，与解析器的收敛一致）。
func takeTime(args []string, now time.Time, loc *time.Location, endOfDay bool) (time.Time, []string, error) {
	if len(args) == 0 {
		return time.Time{}, nil, fmt.Errorf("缺少时间")
	}
	if len(args) >= 2 {
		d, ok := parseDate(args[0], now, loc)
		if ok {
			if hh, mm, ss, ok := parseClock(args[1]); ok {
				return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, ss, 0, loc), args[2:], nil
			}
		}
	}
	if t, ok := parseFull(args[0], loc); ok {
		return t, args[1:], nil
	}
	d, ok := parseDate(args[0], now, loc)
	if !ok {
		return time.Time{}, nil, fmt.Errorf("%q 不是日期（写 2026-09-08 或 09-08）", args[0])
	}
	if endOfDay {
		return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc), args[1:], nil
	}
	return d, args[1:], nil
}

var (
	fullLayouts  = []string{"2006-01-02 15:04", "2006/01/02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05"}
	dateLayouts  = []string{"2006-01-02", "2006/01/02", "2006.01.02"}
	monthLayouts = []string{"01-02", "01/02"} // 不带年份：借当前年
	clockLayouts = []string{"15:04", "15:04:05"}
)

func parseFull(s string, loc *time.Location) (time.Time, bool) {
	for _, l := range fullLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseDate(s string, now time.Time, loc *time.Location) (time.Time, bool) {
	for _, l := range dateLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, true
		}
	}
	for _, l := range monthLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			n := now.In(loc)
			return time.Date(n.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc), true
		}
	}
	return time.Time{}, false
}

func parseClock(s string) (hh, mm, ss int, ok bool) {
	for _, l := range clockLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Hour(), t.Minute(), t.Second(), true
		}
	}
	return 0, 0, 0, false
}
