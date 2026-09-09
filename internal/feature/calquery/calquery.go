// Package calquery 是「查活动」功能：@机器人问现在有什么活动、什么快结束、什么快开始。
//
// 分层契约的执行点：这个功能**不存任何东西**，所以构造时拿不到存储面——
// 它只有 calendar.View（只读日历）与命令注册面。想知道"是不是还在同步中"，
// 它声明一个 Status() 的小接口由 main 决定给不给（见 SyncStatus）。
package calquery

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
	"xingta/internal/kernel/calendar"
	"xingta/internal/qq"
)

// Registrar 是这个功能需要的命令注册面：能加命令，还能取全表来渲染「帮助」。
// 由 *command.Registry 满足——功能自己声明需要什么，而不是拿整个具体类型。
type Registrar interface {
	command.Registrar
	HelpText() string
}

// SyncStatus 是可选依赖：拿得到就在回复尾巴带一句同步状况，
// 拿不到（nil）就只报日历内容。
type SyncStatus interface {
	Status() (annsync.Status, error)
}

// Config 的零值必须可用。
type Config struct {
	PageSize        int           // 每页列几条，默认 8
	DefaultEndLead  time.Duration // 「快结束」不带参数时的窗口，默认 48h
	DefaultSoonDays int           // 「即将」不带参数时的天数，默认 7
	MaxDays         int           // 「即将」天数上限，默认 60
	Zone            *time.Location
	Now             func() time.Time
	Status          SyncStatus
	Logf            func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.PageSize <= 0 {
		c.PageSize = 8
	}
	if c.DefaultEndLead <= 0 {
		c.DefaultEndLead = 48 * time.Hour
	}
	if c.DefaultSoonDays <= 0 {
		c.DefaultSoonDays = 7
	}
	if c.MaxDays <= 0 {
		c.MaxDays = 60
	}
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
	reg Registrar
	cal calendar.View
	cfg Config
}

// New 造出功能。cal 是只读面：这个功能改不了日历，想改期得走 calops 的人工覆盖。
func New(reg Registrar, cal calendar.View, cfg Config) kernel.Feature {
	return &feature{reg: reg, cal: cal, cfg: cfg.withDefaults()}
}

func (f *feature) Name() string { return "calquery" }

// Start 只注册命令，非阻塞（kernel.Feature 契约）。重名会报错，内核整体退出。
func (f *feature) Start(ctx context.Context, api kernel.API) error {
	cmds := []command.Cmd{
		{Name: "活动", Aliases: []string{"進行中", "进行中", "活動", "events"},
			Usage: "正在进行的活动，可加页码：活动 2", Run: command.Text(f.active)},
		{Name: "快结束", Aliases: []string{"快結束", "快結束了", "快结束了", "ending"},
			Usage: fmt.Sprintf("默认 %d 小时内结束的活动，可加小时数：快结束 12", int(f.cfg.DefaultEndLead.Hours())),
			Run:   command.Text(f.ending)},
		{Name: "即将", Aliases: []string{"即將", "預告", "预告", "upcoming"},
			Usage: fmt.Sprintf("默认 %d 天内开始的活动，可加天数：即将 3", f.cfg.DefaultSoonDays),
			Run:   command.Text(f.upcoming)},
		{Name: "帮助", Aliases: []string{"幫助", "help", "命令"},
			Usage: "列出所有命令", Run: command.Text(f.help)},
	}
	for _, c := range cmds {
		if err := f.reg.Add(c); err != nil {
			return err
		}
	}
	return nil
}

func (f *feature) active(ctx context.Context, m *qq.Message, args []string) (string, error) {
	now := f.cfg.Now()
	list := f.cal.Active(now)
	// 最快结束的先列：用户问"现在有什么"，真正要紧的是快跑掉的那几个
	sort.Slice(list, func(i, j int) bool { return list[i].End.Before(list[j].End) })

	rows := make([]string, 0, len(list))
	for _, a := range list {
		rows = append(rows, fmt.Sprintf("· %s → %s 结束（剩 %s）",
			a.Title, f.fmtTime(a.End, now), text.Human(a.End.Sub(now))))
	}
	return f.render("进行中的活动", rows, pageArg(args, 1), "活动"), nil
}

func (f *feature) ending(ctx context.Context, m *qq.Message, args []string) (string, error) {
	now := f.cfg.Now()
	lead, rest := durationArg(args, f.cfg.DefaultEndLead, time.Hour, 24*30*time.Hour)
	list := f.cal.EndingWithin(now, lead)
	sort.Slice(list, func(i, j int) bool { return list[i].End.Before(list[j].End) })

	rows := make([]string, 0, len(list))
	for _, a := range list {
		rows = append(rows, fmt.Sprintf("· %s → %s 结束（剩 %s）",
			a.Title, f.fmtTime(a.End, now), text.Human(a.End.Sub(now))))
	}
	return f.render(fmt.Sprintf("%d 小时内结束的活动", int(lead.Hours())), rows, pageArg(rest, 1), "快结束"), nil
}

func (f *feature) upcoming(ctx context.Context, m *qq.Message, args []string) (string, error) {
	now := f.cfg.Now()
	days, rest := intArg(args, f.cfg.DefaultSoonDays, 1, f.cfg.MaxDays)
	list := f.cal.Upcoming(now, time.Duration(days)*24*time.Hour) // 已按开始时间升序

	rows := make([]string, 0, len(list))
	for _, a := range list {
		rows = append(rows, fmt.Sprintf("· %s → %s 开始（还有 %s）",
			a.Title, f.fmtTime(a.Start, now), text.Human(a.Start.Sub(now))))
	}
	return f.render(fmt.Sprintf("%d 天内开始的活动", days), rows, pageArg(rest, 1), "即将"), nil
}

func (f *feature) help(ctx context.Context, m *qq.Message, args []string) (string, error) {
	text := f.reg.HelpText()
	if text == "" {
		return "现在还没有可用命令", nil
	}
	return "可用命令（@我 + 命令名）\n" + text, nil
}

// render 把一个列表合成**一条**回复：头部计数、当前页的行、尾部翻页提示与同步状况。
// 分页由命令自己做（它才知道总条数），机制层只负责兜住单条长度上限。
func (f *feature) render(title string, rows []string, page int, cmd string) string {
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString(title + "：暂无\n")
		if note := f.statusNote(); note != "" {
			b.WriteString(note)
		}
		return strings.TrimRight(b.String(), "\n")
	}

	pages := (len(rows) + f.cfg.PageSize - 1) / f.cfg.PageSize
	if page > pages {
		page = pages
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * f.cfg.PageSize
	end := min(start+f.cfg.PageSize, len(rows))

	fmt.Fprintf(&b, "%s（%d 条，第 %d/%d 页）\n", title, len(rows), page, pages)
	for _, row := range rows[start:end] {
		b.WriteString(row + "\n")
	}
	if page < pages {
		fmt.Fprintf(&b, "发「%s %d」看下一页\n", cmd, page+1)
	}
	if note := f.statusNote(); note != "" {
		b.WriteString(note + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// statusNote 是尾巴上的同步状况。没接 Status 依赖就什么都不说。
func (f *feature) statusNote() string {
	if f.cfg.Status == nil {
		return ""
	}
	st, err := f.cfg.Status.Status()
	if err != nil {
		f.cfg.Logf("calquery: 读同步状况失败: %v", err)
		return ""
	}
	now := f.cfg.Now()
	switch {
	case st.LastSuccess.IsZero():
		return "活动数据还在同步中（首次同步要逐条抓公告），过一会儿再问"
	case st.Fails > 0:
		return fmt.Sprintf("数据截至 %s（源侧最近失败 %d 次：%s）",
			f.fmtTime(st.LastSuccess, now), st.Fails, text.Clip(st.LastError, 40))
	default:
		return "数据截至 " + f.fmtTime(st.LastSuccess, now)
	}
}

func (f *feature) fmtTime(t time.Time, now time.Time) string {
	return text.Clock(t, now, f.cfg.Zone)
}

// pageArg 取页码：第一个能当正整数的参数就是页码，其余忽略。
func pageArg(args []string, def int) int {
	n, _ := intArg(args, def, 1, 1<<20)
	return n
}

// intArg 取第一个整数参数并夹在 [lo, hi] 内，返回剩下的参数。
// 用户写错（"活动 二"）就按默认值走，不回一句"参数错误"——查活动不值得吵一架。
func intArg(args []string, def, lo, hi int) (int, []string) {
	for i, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil {
			continue
		}
		if n < lo {
			n = lo
		}
		if n > hi {
			n = hi
		}
		return n, append(append([]string(nil), args[:i]...), args[i+1:]...)
	}
	return def, args
}

// durationArg 取第一个整数参数当"多少个 unit"，夹在 [0, hi] 内。
func durationArg(args []string, def, unit, hi time.Duration) (time.Duration, []string) {
	n := int(def / unit)
	v, rest := intArg(args, n, 1, int(hi/unit))
	return time.Duration(v) * unit, rest
}
