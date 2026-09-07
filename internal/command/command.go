// Package command 是命令机制层：注册表 + Hub 适配器 + 被动回复的铁律。
//
// 分层契约：本包是基础设施，不含任何业务命令。活动 / 待确认 / 覆盖 这些住在各自的
// 功能包里，通过 Registrar 注册进来；功能只声明"我叫什么、要不要管理员、怎么跑"，
// 回复几次、多长截断、权限怎么查，全由这里统一保证。
//
// 铁律：**一条命令 = 一条被动回复**。平台的被动回复上限是群 5 次 / 单聊 4 次
// （qq.MaxGroupReplies / qq.MaxC2CReplies），超了直接拒发。所以 Cmd.Run 返回一段文本
// 而不是自己发消息——机制层拿去合成单条回复。要列更多内容由命令自己分页
// （用户发新消息 = 新 msg_id = 新一批预算），不要试图一次发好几条。
package command

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"xingta/internal/qq"
)

// Cmd 是一条命令。功能包构造它并交给 Registrar。
type Cmd struct {
	Name    string   // 主名，如 "活动"
	Aliases []string // 别名，如 "進行中"
	Admin   bool     // 只给群主/管理员（单聊里则要求发送者在 Config.AdminOpenIDs 里）
	Usage   string   // 一行用法，进「帮助」列表
	Run     func(ctx context.Context, m *qq.Message, args []string) (string, error)
}

// Registrar 是机制层给功能包的注册面。功能只拿得到这个，拿不到 Registry 的内部。
type Registrar interface {
	Add(Cmd) error
}

// Registry 是命令表。Add 在重名时报错——两个功能抢同一个命令名是接线错误，
// 静默让后注册的覆盖前一个，会让"关掉某功能"的行为变得不可预测。
type Registry struct {
	mu     sync.RWMutex
	byName map[string]*Cmd
	order  []*Cmd
}

var _ Registrar = (*Registry)(nil)

func NewRegistry() *Registry {
	return &Registry{byName: map[string]*Cmd{}}
}

func (r *Registry) Add(c Cmd) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("command: 命令名不能为空")
	}
	if c.Run == nil {
		return fmt.Errorf("command: 命令 %s 没有 Run", c.Name)
	}
	names := append([]string{c.Name}, c.Aliases...)

	r.mu.Lock()
	defer r.mu.Unlock()
	cp := c
	for _, n := range names {
		key := fold(n)
		if key == "" {
			return fmt.Errorf("command: 命令 %s 有空别名", c.Name)
		}
		if old, dup := r.byName[key]; dup {
			return fmt.Errorf("command: %q 与命令 %s 重名", n, old.Name)
		}
	}
	for _, n := range names {
		r.byName[fold(n)] = &cp
	}
	r.order = append(r.order, &cp)
	return nil
}

// Lookup 按名字（或别名）找命令。大小写不敏感，中文名不受影响。
// 前导的 "/" 与全角 "／" 会被剥掉：QQ 没有平台级斜杠命令，"@机器人 /events"
// 里的斜杠只是文本，但玩家和运营都会按习惯打。
func (r *Registry) Lookup(name string) (Cmd, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byName[fold(strings.TrimLeft(name, "/／"))]
	if !ok {
		return Cmd{}, false
	}
	return *c, true
}

// HelpText 按注册顺序列出所有命令，供某个功能注册「帮助」命令时取用。
// 机制层提供数据，命令本身仍由功能注册——本包不塞业务命令进来。
func (r *Registry) HelpText() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder
	for _, c := range r.order {
		b.WriteString(c.Name)
		if len(c.Aliases) > 0 {
			b.WriteString("（" + strings.Join(c.Aliases, "/") + "）")
		}
		if c.Admin {
			b.WriteString("［管理员］")
		}
		if c.Usage != "" {
			b.WriteString("：" + c.Usage)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// SendAPI 是回复一条消息所需的最小面，*qq.Client 天然满足。
// 只要被动回复：主动消息有配额限制，走 kernel 的发送队列，不在命令这条路上。
type SendAPI interface {
	SendGroupReply(ctx context.Context, groupOpenID, msgID string, req qq.SendRequest) (*qq.SendResult, error)
	SendC2CReply(ctx context.Context, userOpenID, msgID string, req qq.SendRequest) (*qq.SendResult, error)
}

// Config 的零值必须可用。
type Config struct {
	// MaxRunes 是单条回复的长度上限。1500 是保守猜测（平台拒发阈值未实测）：
	// 超长发出去是整条失败，截断至少能把前半段送到。
	MaxRunes int
	// AdminOpenIDs 是单聊里的管理员白名单。单聊报文没有群角色，
	// 光靠 MemberRoleIs 判断会让管理员命令在私聊里永远不可用。
	AdminOpenIDs []string
	Logf         func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.MaxRunes <= 0 {
		c.MaxRunes = 1500
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

// Attach 把命令表接到 Hub 上：群 @机器人 与单聊两条事件各挂一个处理器。
func Attach(reg *Registry, h *qq.Hub, send SendAPI, cfg Config) {
	cfg = cfg.withDefaults()
	handle := func(ctx context.Context, m *qq.Message) error {
		dispatch(ctx, reg, send, cfg, m)
		// 刻意不往上抛错误：webhook 前端见到处理器报错会撤销去重登记等平台重投，
		// 而命令已经回过一条了，重投就是"同一条命令回两遍"。失败就地记日志。
		return nil
	}
	h.OnMessage(qq.EventGroupAtMessage, handle)
	h.OnMessage(qq.EventC2CMessage, handle)
}

func dispatch(ctx context.Context, reg *Registry, send SendAPI, cfg Config, m *qq.Message) {
	fields := strings.Fields(m.Text())
	if len(fields) == 0 {
		return
	}
	c, ok := reg.Lookup(fields[0])
	if !ok {
		return // 不是命令：静默。群里 @机器人说闲话不该被回一句"没听懂"
	}

	if c.Admin && !isAdmin(m, cfg) {
		reply(ctx, send, cfg, m, "「"+c.Name+"」只给群主与管理员用")
		return
	}

	text, err := c.Run(ctx, m, fields[1:])
	if err != nil {
		cfg.Logf("command: %s 执行失败: %v", c.Name, err)
		text = "「" + c.Name + "」执行失败：" + err.Error()
	}
	if strings.TrimSpace(text) == "" {
		return // 命令明确表示不回话（例如后台任务已受理）
	}
	reply(ctx, send, cfg, m, text)
}

// isAdmin 群里看角色，单聊看白名单：单聊报文里没有 member_role。
func isAdmin(m *qq.Message, cfg Config) bool {
	if m.MemberRoleIs("owner") || m.MemberRoleIs("admin") {
		return true
	}
	if m.IsGroup() {
		return false
	}
	openID := m.SenderOpenID()
	for _, id := range cfg.AdminOpenIDs {
		if id != "" && id == openID {
			return true
		}
	}
	return false
}

func reply(ctx context.Context, send SendAPI, cfg Config, m *qq.Message, text string) {
	target, isGroup := m.ReplyTarget()
	if target == "" {
		cfg.Logf("command: 消息 %s 没有可回复的 openid", m.ID)
		return
	}
	req := qq.SendRequest{MsgType: qq.MsgTypeText, Content: clamp(text, cfg.MaxRunes)}

	var err error
	if isGroup {
		_, err = send.SendGroupReply(ctx, target, m.ID, req)
	} else {
		_, err = send.SendC2CReply(ctx, target, m.ID, req)
	}
	if err != nil {
		// client.replier 的次数闸门是最后防线：触闸只记日志，绝不降级成主动消息
		cfg.Logf("command: 回复 %s 失败: %v", m.ID, err)
	}
}

// clamp 截断到 max 个字符（按 rune 切，不劈开汉字），并在尾部说明被截断了。
func clamp(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	const tail = "\n…（内容过长已截断）"
	keep := max - len([]rune(tail))
	if keep < 0 {
		keep = 0
	}
	return string(r[:keep]) + tail
}

func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
