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
	Run     func(ctx context.Context, m *qq.Message, args []string) (Reply, error)
}

// Reply 是一条命令的回复。Text 与 Image 二选一，非此即彼——不设并行的第二个入口
// 是"一条命令 = 一条被动回复"这条铁律的另一面：两个入口一定漂移。
//
// 图给的是字节而不是 file_info：file_info 的作废时机不可预知（实测 ttl 24h 但文档
// 示例只写 300s、不能跨场景复用），从命令开跑到真要发之间还可能跨过几次回复。
// 上传由机制层在发的那一刻做，重复字节经 SendAPI 背后的 MediaCache 免掉四步。
type Reply struct {
	Text  string
	Image []byte
	Name  string // 图片的文件名（平台侧只拿它做记录），空则用默认
}

// Text 把"只回文字"的命令函数适配成 Reply 面。
//
// 入口仍然只有一个：Cmd.Run 返回 Reply。这里只是省掉十几处 return Reply{Text: ...}
// 的噪音——运维命令、帮助这类永远不会发图，写 string 更清楚。
func Text(fn func(ctx context.Context, m *qq.Message, args []string) (string, error)) func(context.Context, *qq.Message, []string) (Reply, error) {
	return func(ctx context.Context, m *qq.Message, args []string) (Reply, error) {
		s, err := fn(ctx, m, args)
		return Reply{Text: s}, err
	}
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

// SendAPI 是回复一条消息所需的最小面。实现者是 `qq.MediaCache`（内嵌 *qq.Client 再叠
// 一层 file_info 复用）——裸 *qq.Client 不再满足本面，它没有 Forget。
// 只要被动回复：主动消息有配额限制，走 kernel 的发送队列，不在命令这条路上。
type SendAPI interface {
	SendGroupReply(ctx context.Context, groupOpenID, msgID string, req qq.SendRequest) (*qq.SendResult, error)
	SendC2CReply(ctx context.Context, userOpenID, msgID string, req qq.SendRequest) (*qq.SendResult, error)
	UploadGroupImage(ctx context.Context, groupOpenID, fileName string, data []byte) (qq.MediaRef, error)
	UploadC2CImage(ctx context.Context, userOpenID, fileName string, data []byte) (qq.MediaRef, error)
	// Forget 把一份被平台判死的 file_info 从上传缓存里摘掉。没缓存时是空操作。
	Forget(qq.MediaRef)
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
		reply(ctx, send, cfg, m, Reply{Text: "「" + c.Name + "」只给群主与管理员用"})
		return
	}

	rep, err := c.Run(ctx, m, fields[1:])
	if err != nil {
		cfg.Logf("command: %s 执行失败: %v", c.Name, err)
		rep = Reply{Text: "「" + c.Name + "」执行失败：" + err.Error()}
	}
	if strings.TrimSpace(rep.Text) == "" && len(rep.Image) == 0 {
		return // 命令明确表示不回话（例如后台任务已受理）
	}
	reply(ctx, send, cfg, m, rep)
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

func reply(ctx context.Context, send SendAPI, cfg Config, m *qq.Message, rep Reply) {
	target, isGroup := m.ReplyTarget()
	if target == "" {
		cfg.Logf("command: 消息 %s 没有可回复的 openid", m.ID)
		return
	}
	upload := func(data []byte) (qq.MediaRef, error) {
		name := rep.Name
		if name == "" {
			name = "calendar.png"
		}
		if isGroup {
			return send.UploadGroupImage(ctx, target, name, data)
		}
		return send.UploadC2CImage(ctx, target, name, data)
	}
	sendOnce := func(req qq.SendRequest) error {
		if isGroup {
			_, err := send.SendGroupReply(ctx, target, m.ID, req)
			return err
		}
		_, err := send.SendC2CReply(ctx, target, m.ID, req)
		return err
	}

	req := qq.SendRequest{MsgType: qq.MsgTypeText, Content: clamp(rep.Text, cfg.MaxRunes)}
	var ref qq.MediaRef
	if len(rep.Image) > 0 {
		var err error
		ref, err = upload(rep.Image)
		if err != nil {
			// 只报"传不上去"，不把图换成文字版发出去：那是另一份内容，
			// 玩家看到的会是"图没了但多了一大段字"。
			cfg.Logf("command: 回复 %s 的配图上传失败: %v", m.ID, err)
			req = qq.SendRequest{MsgType: qq.MsgTypeText,
				Content: "图传不上去：" + err.Error()}
		} else {
			if ref.Cached {
				cfg.Logf("command: 回复 %s 命中 file_info 缓存，跳过四步上传", m.ID)
			}
			req = qq.SendRequest{MsgType: qq.MsgTypeMedia, Media: &qq.MediaInfo{FileInfo: ref.FileInfo}}
		}
	}

	err := sendOnce(req)
	// 命中缓存的那份 file_info 被平台判死：摘掉、就地重传、再发一次。
	// 只在平台明确回拒时重试——网络错或 5xx 可能其实已送达，重试就是重发。
	if err != nil && ref.Cached && qq.IsPlatformRejection(err) {
		cfg.Logf("command: 缓存 file_info 被平台拒（%v），重传再发一次", err)
		send.Forget(ref)
		fresh, uerr := upload(rep.Image)
		if uerr != nil {
			err = fmt.Errorf("重传配图失败: %w", uerr)
		} else {
			err = sendOnce(qq.SendRequest{MsgType: qq.MsgTypeMedia, Media: &qq.MediaInfo{FileInfo: fresh.FileInfo}})
		}
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
