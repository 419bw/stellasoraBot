// Package aichat 是「看板娘闲聊兜底」：@机器人 说的话没命中任何命令时，按当期活动
// 事实回一句话。
//
// 分层契约的执行点：本包不注册任何命令，也不实现 kernel.Feature——它只是
// command.Config.Fallback 那一个回调的实现者，由机制层的第二条被动流驱动。
// 上游是谁、走哪条协议、密钥在哪、怎么限流，全在本包里；机制层只搬一个函数值。
//
// 三条政策是定过的，改之前先读各自的理由：
//   - 上游失败一律静默（只记日志）。群里 @机器人 说闲话今天的表现就是没人理，
//     失败时不回一句"我有点忙"——那会把一次上游抖动放大成群聊刷屏，
//     也与 README「满则丢、绝不回太忙提示」同口径。
//   - 只有"同一个人说太快"回一句 cd_reply：那是明确的行为反馈，而且零 API 调用。
//   - 只走被动回复（挂在被 @ 的那条 msg_id 上），绝不为兜底动用主动消息配额。
package aichat

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/feature/text"
	"xingta/internal/kernel/calendar"
	"xingta/internal/qq"
)

// SyncStatus 是可选依赖：拿得到就能在事实列表里说清"是真没活动"还是"还没同步完"。
// 形制同 calquery 自己声明的那一份——两个功能各自要什么各自说，不互相 import。
type SyncStatus interface {
	Status() (annsync.Status, error)
}

// Config 的零值不可用：部署数值都得由调用方给（生产那份来自 config/aichat.yml），
// withDefaults 只兜接线依赖（Zone/Now/Logf/Chat）。
type Config struct {
	// ---- 部署数值：唯一源是 config/aichat.yml，代码里不留默认值 ----
	Endpoint        string        // 完整请求地址（含协议路径），换上游就改这一行
	Model           string        // 具体型号，不用 -latest 之类别名
	Timeout         time.Duration // 整趟的时间预算，含反代那一跳
	Cooldown        time.Duration // 同一个人两句话之间的最小间隔
	MaxPerMin       int           // 全局每分钟最多问上游几次
	MaxOutputTokens int           // 进 body 的 max_tokens
	Temperature     float64       // 强事实内容用低温
	MaxPromptEvents int           // 事实列表最多几条（三段合计）
	Proxy           string        // 空 = 直连；支持 http:// 与 socks5://
	CDReply         string        // 冷却命中的回复文案，含一个字面量 %d（剩余秒数）

	// ---- 接线依赖 ----
	Zone *time.Location
	Now  func() time.Time
	Logf func(format string, args ...any)
	// Chat 是模型接口面。留 nil 时本包按上面几项自建一份实现——那是接线不是
	// 部署参数（同 biliwatch 的 Fetcher）。测试注入假件就走这个口子。
	Chat Chat
	// APIKey 只从 creds.json 来，配置文件一个密钥都不装。为空时上游必然拒绝，
	// 所以接线处根本不该把 Fallback 递进机制层。
	APIKey string
	// Status 可选：nil 就只报日历里的内容。
	Status SyncStatus
	// EndingLead 与 SoonDays 是事实列表的两段窗口。值由接线处从
	// config/calquery.yml 传过来：与 /ending、/upcoming 同一把尺子，
	// 同一个数不在第二处存一份。两者必须是正数（那份配置文件已校验过）。
	EndingLead time.Duration
	SoonDays   int
}

func (c Config) withDefaults() Config {
	if c.Zone == nil {
		c.Zone = time.FixedZone("CST", 8*60*60)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Chat == nil {
		c.Chat = newOpenAIChat(c)
	}
	return c
}

// Service 是兜底本体。它没有后台循环：来一条非命令消息才动一次。
type Service struct {
	cal calendar.View
	cfg Config
	lim *limiter
}

// New 造出兜底服务。cal 是只读面，本包改不了日历，要改期得走 calops 的人工覆盖。
func New(cal calendar.View, cfg Config) *Service {
	cfg = cfg.withDefaults()
	return &Service{cal: cal, cfg: cfg, lim: newLimiter(cfg)}
}

// Fallback 是 command.Config.Fallback 的实现，跑在机制层的第二条被动流上：
// 串行、有界、panic 有保险丝，所以这里等网络不会占用命令流。
func (s *Service) Fallback(ctx context.Context, m *qq.Message) (command.Reply, error) {
	now := s.cfg.Now()

	// 顺序是定的：先判"上游是不是已经确定坏了"，再判冷却，最后花配额。
	// 上游判死时连那句"你说话太快"都不该说——那会让人以为再等等就有回答。
	if until, why, open := s.lim.breaker(now); open {
		s.cfg.Logf("aichat: 上游被判定永久失败（%s），%s 之内静默，丢弃消息 %s",
			why, until.Sub(now).Round(time.Second), m.ID)
		return command.Reply{}, nil
	}
	// 冷却在"决定要问上游"这一刻就占位，与后来成败无关：它的用途就是挡住同一个人
	// 连刷，上游正在抖的时候这层拦截反而最省钱。
	if wait, ok := s.lim.mark(keyOf(m), now); !ok {
		return command.Reply{Text: s.cdReply(wait)}, nil
	}
	if !s.lim.take(now) {
		s.cfg.Logf("aichat: 全局配额到点（%d 次/分钟），静默丢弃消息 %s", s.cfg.MaxPerMin, m.ID)
		return command.Reply{}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	answer, err := s.cfg.Chat.Chat(cctx, s.systemPrompt(now), questionOf(m))
	if err != nil {
		if permanent, why := permanentFailure(err); permanent {
			s.lim.trip(now, why)
		}
		return command.Reply{}, err // 失败静默由机制层落日志，不回给用户
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		// 上游给了 200 却没内容（被内容审查吃掉、choices 为空）：客户端已经记过
		// 一句带 finish_reason 的日志，这里同样不回话。
		return command.Reply{}, nil
	}
	return command.Reply{Text: answer}, nil
}

// cdReply 把剩余冷却时间说成整秒填进文案。
//
// 不用 fmt.Sprintf：文案来自配置文件，谁都可能在里面写一个百分号（"100% 加油"），
// Sprintf 会把后面那个字符当动词吃掉，回出一句 %!d(MISSING)。只认字面量 %d。
func (s *Service) cdReply(wait time.Duration) string {
	secs := int64(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1 // 报"0 秒后再来"等于骗人再撞一次冷却
	}
	return strings.ReplaceAll(s.cfg.CDReply, "%d", strconv.FormatInt(secs, 10))
}

// keyOf 是"同一个人"的键：会话目标 + 说话人 openid。单聊没有群，ReplyTarget()
// 返回的就是这个人自己，键照样唯一；同一个人换群聊就是换键，冷却不该跨群欠着。
func keyOf(m *qq.Message) string {
	target, _ := m.ReplyTarget()
	return target + "|" + m.SenderOpenID()
}

// maxQuestionRunes 是给群友原文的长度上限：一句话提问用不到 400 字，
// 上限挡的是"有人贴一整屏聊天记录"那种把事实列表挤出上下文的情况。
const maxQuestionRunes = 400

func questionOf(m *qq.Message) string {
	// 先压成一行再截：群消息里的换行是排版，不是内容，留着只会占上下文。
	return text.Clip(text.OneLine(m.Text()), maxQuestionRunes)
}
