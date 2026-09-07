package command_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"xingta/internal/command"
	"xingta/internal/qq"
)

// ---- 假件 ----------------------------------------------------------------

type sent struct {
	target  string
	msgID   string
	content string
}

type fakeSend struct {
	mu       sync.Mutex
	group    []sent
	c2c      []sent
	failWith error
}

func (f *fakeSend) SendGroupReply(ctx context.Context, target, msgID string, req qq.SendRequest) (*qq.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.group = append(f.group, sent{target: target, msgID: msgID, content: req.Content})
	return &qq.SendResult{}, nil
}

func (f *fakeSend) SendC2CReply(ctx context.Context, target, msgID string, req qq.SendRequest) (*qq.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.c2c = append(f.c2c, sent{target: target, msgID: msgID, content: req.Content})
	return &qq.SendResult{}, nil
}

func (f *fakeSend) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(append([]sent(nil), f.group...), f.c2c...)
}

func (f *fakeSend) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
}

// ---- 驱动 ----------------------------------------------------------------

// harness 把命令表接到真 Hub 上，用真报文形状驱动：走的是线上来消息的同一条路径。
type harness struct {
	reg  *command.Registry
	send *fakeSend
	hub  *qq.Hub
	logs []string

	mu sync.Mutex
}

func newHarness(t *testing.T, cfg command.Config) *harness {
	t.Helper()
	h := &harness{reg: command.NewRegistry(), send: &fakeSend{}}
	cfg.Logf = h.log
	h.hub = qq.NewHub(nil, h.log)
	command.Attach(h.reg, h.hub, h.send, cfg)
	return h
}

func (h *harness) log(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, fmt.Sprintf(format, args...))
}

func (h *harness) logged() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.logs, "\n")
}

// sayGroup 发一条群 @机器人 的消息。role 取 member / admin / owner。
func (h *harness) sayGroup(t *testing.T, id, content, role string) {
	t.Helper()
	h.say(t, qq.EventGroupAtMessage, map[string]any{
		"id": id, "group_openid": "GROUP1", "content": content,
		"author": map[string]any{
			"id": "A1", "member_openid": "USER1", "username": "某人", "member_role": role,
		},
	})
}

// sayC2C 发一条单聊消息。单聊报文里没有 member_role，这是平台给的形状。
func (h *harness) sayC2C(t *testing.T, id, content, openID string) {
	t.Helper()
	h.say(t, qq.EventC2CMessage, map[string]any{
		"id": id, "content": content,
		"author": map[string]any{"id": "A2", "user_openid": openID},
	})
}

func (h *harness) say(t *testing.T, kind string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造报文: %v", err)
	}
	if err := h.hub.Handle(context.Background(), qq.Event{Type: kind, Data: raw}); err != nil {
		t.Errorf("Hub.Handle 返回错误 %v：处理器报错会让 webhook 侧撤销去重等平台重投，命令就会被执行两遍", err)
	}
}

func echo(text string) func(context.Context, *qq.Message, []string) (string, error) {
	return func(context.Context, *qq.Message, []string) (string, error) { return text, nil }
}

func mustAdd(t *testing.T, reg *command.Registry, c command.Cmd) {
	t.Helper()
	if err := reg.Add(c); err != nil {
		t.Fatalf("Add(%s): %v", c.Name, err)
	}
}

// ---- 用例 ----------------------------------------------------------------

func TestUnknownTextIsSilent(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Run: echo("x")})

	h.sayGroup(t, "M1", "今天天气不错", "member")
	h.sayGroup(t, "M2", "", "member")
	h.sayGroup(t, "M3", "   ", "member")

	if got := h.send.all(); len(got) != 0 {
		t.Errorf("非命令消息被回复了 %d 条：%v（群里 @机器人说闲话不该被回一句\"没听懂\"）", len(got), got)
	}
}

// 平台的被动回复上限是群 5 / 单聊 4，所以"一条命令 = 一条回复"是硬约束，不是风格偏好。
func TestOneCommandOneReply(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Run: echo("第一行\n第二行\n第三行")})

	h.sayGroup(t, "M1", " 活動 ", "member") // 平台剥掉 @机器人 后会留下空格

	got := h.send.group
	if len(got) != 1 {
		t.Fatalf("回复了 %d 条, want 1（多行内容必须合成单条）：%v", len(got), got)
	}
	if got[0].content != "第一行\n第二行\n第三行" {
		t.Errorf("回复内容 = %q", got[0].content)
	}
	if got[0].target != "GROUP1" || got[0].msgID != "M1" {
		t.Errorf("回复目标 = %q / msg_id = %q, want GROUP1 / M1", got[0].target, got[0].msgID)
	}
}

func TestC2CUsesC2CReplyWithUserOpenID(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Run: echo("好")})

	h.sayC2C(t, "M9", "活動", "USER9")

	if len(h.send.c2c) != 1 || len(h.send.group) != 0 {
		t.Fatalf("单聊走了错误的发送通道：c2c=%v group=%v", h.send.c2c, h.send.group)
	}
	if h.send.c2c[0].target != "USER9" {
		t.Errorf("单聊回复目标 = %q, want USER9", h.send.c2c[0].target)
	}
}

func TestAliasAndCaseInsensitiveLookup(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Aliases: []string{"進行中", "help"}, Run: echo("好")})

	h.sayGroup(t, "M1", "進行中", "member")
	h.sayGroup(t, "M2", "HELP", "member")
	h.sayGroup(t, "M3", "活動", "member")

	if len(h.send.group) != 3 {
		t.Errorf("别名/大小写匹配失败，回复了 %d 条, want 3", len(h.send.group))
	}
}

// 斜杠写法：QQ 没有平台级斜杠命令，"@机器人 /events" 里的 / 只是消息文本，
// 但玩家和运营都会按习惯打，中文输入法下更常见的是全角「／」。
func TestSlashPrefixedCommandMatches(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Aliases: []string{"events"},
		Run: func(_ context.Context, _ *qq.Message, args []string) (string, error) {
			return "页 " + strings.Join(args, ","), nil
		}})

	h.sayGroup(t, "M1", "/events", "member")
	h.sayGroup(t, "M2", "／活動", "member")
	h.sayGroup(t, "M3", "/events 2", "member")
	h.sayGroup(t, "M4", "//events 3", "member")

	if len(h.send.group) != 4 {
		t.Fatalf("斜杠写法没全被认出来，回复了 %d 条, want 4", len(h.send.group))
	}
	want := []string{"页 ", "页 ", "页 2", "页 3"}
	for i, w := range want {
		if got := h.send.group[i].content; got != w {
			t.Errorf("第 %d 条回复 = %q, want %q：斜杠只该作用于命令名，参数必须原样传下去", i, got, w)
		}
	}
}

func TestAdminGateInGroupUsesMemberRole(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "覆蓋", Admin: true, Run: echo("改好了")})

	h.sayGroup(t, "M1", "覆蓋 x", "member")
	h.sayGroup(t, "M2", "覆蓋 x", "admin")
	h.sayGroup(t, "M3", "覆蓋 x", "owner")

	if len(h.send.group) != 3 {
		t.Fatalf("回复了 %d 条, want 3（一次拒绝 + 两次执行）", len(h.send.group))
	}
	if !strings.Contains(h.send.group[0].content, "只给群主与管理员用") {
		t.Errorf("普通成员的回复 = %q, want 拒绝话术", h.send.group[0].content)
	}
	for i := 1; i <= 2; i++ {
		if h.send.group[i].content != "改好了" {
			t.Errorf("管理员/群主的回复 = %q, want 改好了", h.send.group[i].content)
		}
	}
}

// 单聊报文里没有 member_role，光靠角色判断会让管理员命令在私聊里永远不可用。
func TestAdminGateInC2CUsesWhitelist(t *testing.T) {
	h := newHarness(t, command.Config{AdminOpenIDs: []string{"OWNER1"}})
	mustAdd(t, h.reg, command.Cmd{Name: "覆蓋", Admin: true, Run: echo("改好了")})

	h.sayC2C(t, "M1", "覆蓋 x", "RANDOM")
	h.sayC2C(t, "M2", "覆蓋 x", "OWNER1")

	if len(h.send.c2c) != 2 {
		t.Fatalf("回复了 %d 条, want 2", len(h.send.c2c))
	}
	if !strings.Contains(h.send.c2c[0].content, "只给群主与管理员用") {
		t.Errorf("白名单外的回复 = %q, want 拒绝", h.send.c2c[0].content)
	}
	if h.send.c2c[1].content != "改好了" {
		t.Errorf("白名单内的回复 = %q, want 改好了", h.send.c2c[1].content)
	}
}

func TestLongReplyIsTruncated(t *testing.T) {
	h := newHarness(t, command.Config{MaxRunes: 100})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Run: echo(strings.Repeat("活動內容很長", 200))})

	h.sayGroup(t, "M1", "活動", "member")

	got := h.send.group[0].content
	if n := len([]rune(got)); n > 100 {
		t.Errorf("回复长度 = %d runes, 上限 100", n)
	}
	if !strings.Contains(got, "内容过长已截断") {
		t.Errorf("截断后没有说明：%q", got)
	}
	// 汉字不能被劈成半个：截断后仍是合法 UTF-8
	if strings.Contains(got, "\ufffd") {
		t.Error("截断把汉字劈开了")
	}
}

func TestRunErrorBecomesOneReply(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Run: func(context.Context, *qq.Message, []string) (string, error) {
		return "", errors.New("日历读不出来")
	}})

	h.sayGroup(t, "M1", "活動", "member")

	if len(h.send.group) != 1 {
		t.Fatalf("回复了 %d 条, want 1", len(h.send.group))
	}
	body := h.send.group[0].content
	if !strings.Contains(body, "执行失败") || !strings.Contains(body, "日历读不出来") {
		t.Errorf("错误回复 = %q, want 含命令名与原因", body)
	}
	if !strings.Contains(h.logged(), "日历读不出来") {
		t.Errorf("错误没进日志：%q", h.logged())
	}
}

// 发送失败不能把错误抛给 Hub：webhook 前端会撤销去重登记等平台重投，
// 而命令已经跑过一遍了，重投就是再跑一遍。
func TestSendFailureIsLoggedNotReturned(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "活動", Run: echo("好")})
	h.send.setErr(errors.New("平台拒发"))

	h.sayGroup(t, "M1", "活動", "member") // say 内部已断言 Handle 返回 nil

	if !strings.Contains(h.logged(), "平台拒发") {
		t.Errorf("发送失败没进日志：%q", h.logged())
	}
}

// 命令返回空串表示"不回话"：后台任务受理了就不该再占一次被动回复额度。
func TestEmptyReplyIsNotSent(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "静默", Run: echo("   ")})

	h.sayGroup(t, "M1", "静默", "member")
	if got := h.send.all(); len(got) != 0 {
		t.Errorf("空回复也发了出去：%v", got)
	}
}

func TestArgsArePassedAfterCommandName(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{Name: "快結束", Run: func(_ context.Context, _ *qq.Message, args []string) (string, error) {
		return strings.Join(args, "|"), nil
	}})

	h.sayGroup(t, "M1", "快結束 12 2  .extra", "member")
	if got := h.send.group[0].content; got != "12|2|.extra" {
		t.Errorf("参数 = %q, want 12|2|.extra（按空白切分，命令名本身不算参数）", got)
	}
}

func TestAddRejectsBadCommands(t *testing.T) {
	reg := command.NewRegistry()
	mustAdd(t, reg, command.Cmd{Name: "活動", Aliases: []string{"進行中", "help"}, Run: echo("x")})

	cases := []struct {
		what string
		cmd  command.Cmd
	}{
		{"重名", command.Cmd{Name: "活動", Run: echo("y")}},
		{"撞上别人的别名", command.Cmd{Name: "進行中", Run: echo("y")}},
		{"自己的别名撞主名", command.Cmd{Name: "幫助", Aliases: []string{"活動"}, Run: echo("y")}},
		{"大小写不同的重名", command.Cmd{Name: "HELP", Aliases: nil, Run: echo("y")}},
		{"空名", command.Cmd{Name: "  ", Run: echo("y")}},
		{"没有 Run", command.Cmd{Name: "沒有run"}},
		{"空别名", command.Cmd{Name: "某命令", Aliases: []string{" "}, Run: echo("y")}},
	}
	for _, c := range cases {
		if err := reg.Add(c.cmd); err == nil {
			t.Errorf("%s：Add 竟然成功了（%+v）", c.what, c.cmd)
		}
	}
	// 上面这些都不该留下半条记录，也不该顶替掉原有命令
	for _, name := range []string{"活動", "進行中"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("%s 查不到了：失败的 Add 把原有命令弄坏了", name)
		}
	}
	if got, _ := reg.Lookup("HELP"); got.Name != "活動" {
		t.Errorf("被拒绝的 HELP 顶掉了原有别名：查到的是 %q, want 活動", got.Name)
	}
}

func TestHelpTextListsCommandsInOrder(t *testing.T) {
	reg := command.NewRegistry()
	mustAdd(t, reg, command.Cmd{Name: "活動", Aliases: []string{"進行中"}, Usage: "看现在有什么", Run: echo("x")})
	mustAdd(t, reg, command.Cmd{Name: "覆蓋", Admin: true, Usage: "改时间", Run: echo("x")})

	got := reg.HelpText()
	if !strings.Contains(got, "活動（進行中）：看现在有什么") {
		t.Errorf("帮助里没有主名/别名/用法：%q", got)
	}
	if !strings.Contains(got, "覆蓋［管理员］：改时间") {
		t.Errorf("帮助里没标出管理员命令：%q", got)
	}
	if strings.Index(got, "活動") > strings.Index(got, "覆蓋") {
		t.Errorf("帮助没按注册顺序排：%q", got)
	}
}

func TestEmptyRegistryHelpText(t *testing.T) {
	if got := command.NewRegistry().HelpText(); got != "" {
		t.Errorf("空注册表的帮助 = %q, want 空串", got)
	}
}
