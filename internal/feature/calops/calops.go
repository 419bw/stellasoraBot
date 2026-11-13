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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/feature/text"
	"xingta/internal/kernel"
	"xingta/internal/kernel/queue"
	"xingta/internal/qq"
	"xingta/internal/store"
)

// Refresher 是可选依赖：接上同步引擎（annsync.Syncer）就能在写完覆盖后立刻重投影，
// 不用等下一轮。nil 表示"等下一轮"，命令照样可用。
type Refresher interface{ Refresh() }

// Config 的零值不可用：PageSize 是部署参数，得调用方给（生产那份在 config/calops.yml）。
type Config struct {
	Source   string // 只看某个源，空 = 所有源
	Zone     *time.Location
	PageSize int // 运维列表每页几条，必须 >= 1
	Now      func() time.Time
	Refresh  Refresher
	Logf     func(format string, args ...any)
	// Admins 与 Pending 是接线值（main 给），不是功能级可调参数，不进 calops.yml：
	// 管理员名单是机器级的（config.yml 的 admin），通道是引擎与功能之间的接缝。
	Admins  []string              // 空 = 待确认通知关闭
	Pending <-chan []annsync.Item // 引擎"新待确认"批次的接收端；nil = 不启用
}

func (c Config) withDefaults() Config {
	// PageSize 不在这里兜：唯一源是 config/calops.yml。
	if c.Zone == nil {
		c.Zone = time.FixedZone("CST", 8*60*60)
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
		{Name: "review", Admin: true, C2COnly: true,
			Usage: "列出解析存疑的活动，可加页码：review 2", Run: command.Text(f.review)},
		{Name: "override", Admin: true, C2COnly: true,
			Usage: "改时间：override <id> <开始> <结束> [备注]，时间写 2026-09-08 10:59", Run: command.Text(f.override)},
		{Name: "extend", Admin: true, C2COnly: true,
			Usage: "延期（只改终点）：extend <活动名或id> <新结束时间> [备注]，时间写 2026-11-13 10:59", Run: command.Text(f.extend)},
		{Name: "confirm", Admin: true, C2COnly: true,
			Usage: "把当前解析值固化下来：confirm <id>", Run: command.Text(f.confirm)},
		{Name: "hide", Admin: true, C2COnly: true,
			Usage: "把一条记录从日历里撤下：hide <id> [备注]", Run: command.Text(f.hide)},
		{Name: "show", Admin: true, C2COnly: true,
			Usage: "撤销隐藏：show <id>", Run: command.Text(f.show)},
	}
	for _, c := range cmds {
		if err := f.reg.Add(c); err != nil {
			return err
		}
	}
	if len(f.cfg.Admins) > 0 && f.cfg.Pending != nil {
		go f.drainPending(ctx, api)
	} else {
		f.cfg.Logf("calops: 待确认通知未启用（admin 为空或未接线）")
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
	active, err := annsync.ReadActiveRecs(f.doc, f.cfg.Source)
	if err != nil {
		return "", fmt.Errorf("读在历活动: %w", err)
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
		row := fmt.Sprintf("· %s\n  %s %s［%s］",
			label, when, text.OneLine(it.Title), note)
		rows = append(rows, row+f.sameNameHint(it.Title, active))
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

// 仅用于 review 提示：从公告标题里取第一对引号内的活动名。与解析器的命名判据
// 同形但各管各的——这里只做展示线索，不参与任何数据判定。
var reviewNameRe = regexp.MustCompile(`[「『]([^」』]{1,40})[」』]`)

// sameNameHint 给可疑公告附上同名在历记录：待确认桶里的快照号不是记录 ID，
// 不给出真正能改的那条，运维就得自己考古原公告号。
func (f *feature) sameNameHint(title string, active []annsync.Rec) string {
	m := reviewNameRe.FindStringSubmatch(title)
	if m == nil {
		return ""
	}
	want := annsync.NormName(strings.TrimSpace(m[1]))
	var hits []annsync.Rec
	for _, r := range active {
		if annsync.NormName(r.Title) == want {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	for i, h := range hits {
		if i == 3 {
			fmt.Fprintf(&b, "\n  …等 %d 条同名", len(hits))
			break
		}
		fmt.Fprintf(&b, "\n  同名在历：%s（%s → %s）", h.ID, f.when(h.Start), f.when(h.End))
	}
	fmt.Fprintf(&b, "\n  改期发：extend %s <新结束时间>", hits[0].ID)
	return b.String()
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

// extend 是延期专用：官方延长活动时人只需要给新终点，起点与既有覆盖的其余字段
// 原样保留。按名字定位省掉"考古原公告号再拼三段 ID"——review 里的快照号不是
// 记录 ID，照抄必然失败，这条路就是为那个死胡同开的。
func (f *feature) extend(ctx context.Context, m *qq.Message, args []string) (string, error) {
	if len(args) == 0 {
		return "缺少活动名或 ID。写法：extend <活动名或id> <新结束时间> [备注]，时间写 2026-11-13 10:59", nil
	}
	var rec annsync.Rec
	if strings.Count(args[0], ":") >= 2 {
		r, ok, err := annsync.ReadRec(f.doc, args[0])
		if err != nil {
			return "", fmt.Errorf("读活动记录: %w", err)
		}
		if !ok {
			return f.notFound(args[0]), nil
		}
		rec = r
	} else {
		rs, err := annsync.ReadActiveRecs(f.doc, f.cfg.Source)
		if err != nil {
			return "", fmt.Errorf("读在历活动: %w", err)
		}
		want := annsync.NormName(args[0])
		var hits []annsync.Rec
		for _, r := range rs {
			if annsync.NormName(r.Title) == want {
				hits = append(hits, r)
			}
		}
		switch len(hits) {
		case 0:
			return fmt.Sprintf("没找到叫 %q 的在历活动；发 events 看名字，或用 review 里的 ID", args[0]), nil
		case 1:
			rec = hits[0]
		default:
			// 复玩活动同名不同窗，改错一期全群都看到错时间——宁可让人挑。
			var b strings.Builder
			b.WriteString("同名多条，用 ID 重发：extend <id> <新结束时间>")
			for _, h := range hits {
				fmt.Fprintf(&b, "\n· %s\n  %s（%s → %s）", h.ID, text.OneLine(h.Title), f.when(h.Start), f.when(h.End))
			}
			return b.String(), nil
		}
	}

	end, rest, err := takeTime(args[1:], f.cfg.Now(), f.cfg.Zone, true)
	if err != nil {
		return "新结束时间没看懂：" + err.Error() + "\n写法：extend " + args[0] + " 2026-11-13 10:59", nil
	}
	old, _, _ := f.ov.Get(rec.ID)
	start := old.Start
	if start.IsZero() {
		start = rec.Start
	}
	if start.IsZero() {
		return "这条还没有起点，没法只改终点；用 override 写全起止", nil
	}
	if !end.After(start) {
		return fmt.Sprintf("结束时间 %s 不在开始时间 %s 之后，这样写日历上会显示不出来",
			f.when(end), f.when(start)), nil
	}
	o := annsync.Override{
		Start: start, End: end, Title: old.Title, Hide: old.Hide,
		Note: firstNonEmpty(strings.TrimSpace(strings.Join(rest, " ")), "人工延期"),
		By:   m.Display(), At: f.cfg.Now(),
	}
	if err := f.ov.Put(rec.ID, o); err != nil {
		return "", fmt.Errorf("写覆盖: %w", err)
	}
	f.touch()

	oldEnd := old.End
	if oldEnd.IsZero() {
		oldEnd = rec.End
	}
	return fmt.Sprintf("已延期 %s（%s）\n  原：%s → %s\n  改为：%s → %s%s",
		text.OneLine(rec.Title), rec.ID, f.when(start), f.when(oldEnd),
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

// drainPending 把引擎报来的"新待确认"批次投成管理员私聊。走队列而不是直发：
// 限流、退避、日额度全在那一条路上，主动消息的规矩不能绕。
func (f *feature) drainPending(ctx context.Context, api kernel.API) {
	for {
		select {
		case <-ctx.Done():
			return
		case items := <-f.cfg.Pending:
			msg := formatPending(items, f.cfg.Zone, f.cfg.Source)
			for _, admin := range f.cfg.Admins {
				it := queue.Item{
					ID:     fmt.Sprintf("calops:pending:%s:%d@u:%s", f.cfg.Source, f.cfg.Now().UnixMilli(), admin),
					Target: "u:" + admin,
					Text:   msg,
				}
				if err := api.Submit(it); err != nil {
					f.cfg.Logf("calops: 待确认通知投递失败（%s）: %v", admin, err)
				}
			}
		}
	}
}

const pendingNoticeCap = 10

// formatPending 一批新待确认合成一条私聊。封顶条数：冷启动全新库可能一次冒出
// 几十条旧公告，全列出来会撞平台单条长度上限，尾巴交代数量即可。
func formatPending(items []annsync.Item, zone *time.Location, source string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "新待确认公告（%d 条）：", len(items))
	for i, it := range items {
		if i == pendingNoticeCap {
			fmt.Fprintf(&b, "\n…等 %d 条", len(items)-pendingNoticeCap)
			break
		}
		label := it.Ref.ID
		if source != "" {
			label = source + ":" + it.Ref.ID
		}
		when := "发布时间未知"
		if !it.Published.IsZero() {
			when = it.Published.In(zone).Format("01-02 15:04") + " 发"
		}
		fmt.Fprintf(&b, "\n· %s（%s，%s）", text.OneLine(it.Title), label, when)
		note := it.Note
		if note == "" {
			note = eventsBrief(it.Events)
		}
		if note != "" {
			fmt.Fprintf(&b, "\n  %s", text.OneLine(note))
		}
	}
	b.WriteString("\n发 review 看全部；改期可发 extend <活动名> <新结束时间>")
	return b.String()
}

func eventsBrief(evs []annsync.Event) string {
	parts := make([]string, 0, len(evs))
	for _, ev := range evs {
		parts = append(parts, text.OneLine(ev.Title)+"［"+ev.Status+"］")
	}
	return strings.Join(parts, "、")
}
