// 黑盒测试：只走 Service.Fallback 这个导出口，与机制层调用它的方式一模一样。
// 需要看内部状态的时候（冷却表会不会缩）才开白盒文件，见 limiter_test.go。
package aichat_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"xingta/internal/feature/aichat"
	"xingta/internal/kernel/calendar"
	"xingta/internal/qq"
)

var (
	zone = time.FixedZone("CST", 8*60*60)
	fix  = time.Date(2026, 9, 30, 12, 0, 0, 0, zone)
)

// offset 用相对 fix 的说法写 fixture，读者一眼看得出每条落在哪个窗口里。
func offset(d time.Duration) time.Time { return fix.Add(d) }

// fixture 灌一份形状像真数据的日历：
//   - 悠悠漫時：进行中，10 小时后结束 → 只该出现在"即将结束"那段
//   - 絢麗夜空：进行中，20 天后结束 → 只该出现在"正在进行"
//   - 緊急懸賞：进行中，正好 48 小时后结束 → 临期段的边界
//   - 雪融時分：3 天后开始 → 未来那段
//   - 還早得很：20 天后开始 → 三段都不该有它
func fixture() *calendar.Store {
	cal := calendar.NewStore()
	cal.BulkUpsert([]calendar.Activity{
		{ID: "s:1", Game: "stellasora", Title: "悠悠漫時，愜意夕暮", Start: offset(-5 * 24 * time.Hour), End: offset(10 * time.Hour)},
		{ID: "s:2", Game: "stellasora", Title: "絢麗夜空、煙火綻放", Start: offset(-10 * 24 * time.Hour), End: offset(20 * 24 * time.Hour)},
		{ID: "s:3", Game: "stellasora", Title: "緊急懸賞", Start: offset(-24 * time.Hour), End: offset(48 * time.Hour)},
		{ID: "s:4", Game: "stellasora", Title: "雪融時分新芽綻", Start: offset(3 * 24 * time.Hour), End: offset(25 * 24 * time.Hour)},
		{ID: "s:5", Game: "stellasora", Title: "還早得很", Start: offset(20 * 24 * time.Hour), End: offset(40 * 24 * time.Hour)},
	})
	return cal
}

// clock 是可拨的表。冷却、配额、熔断三样全由时间决定，不控表就只能 sleep 等，
// 测试会变成 CI 里那种偶发红。
type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

// logs 收住 Logf，让"失败要静默但要留痕"这类断言有得查。
type logs struct{ lines []string }

func (l *logs) f(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}
func (l *logs) String() string { return strings.Join(l.lines, "\n") }

// fakeChat 是被注入的模型接口：记下每次问了什么，按脚本答。
type fakeChat struct {
	calls  int
	system []string
	asked  []string
	answer string
	err    error
}

func (f *fakeChat) Chat(_ context.Context, systemPrompt, userText string) (string, error) {
	f.calls++
	f.system = append(f.system, systemPrompt)
	f.asked = append(f.asked, userText)
	return f.answer, f.err
}

type harness struct {
	svc  *aichat.Service
	ck   *clock
	chat *fakeChat
	logs *logs
}

func newHarness(t *testing.T, tweak func(*aichat.Config)) *harness {
	t.Helper()
	h := &harness{ck: &clock{at: fix}, chat: &fakeChat{answer: "在的在的 (｡•̀ᴗ-)✧"}, logs: &logs{}}
	cfg := aichat.Config{
		Endpoint:        "https://upstream.test/v1/chat/completions",
		Model:           "model-under-test",
		Timeout:         5 * time.Second,
		Cooldown:        15 * time.Second,
		MaxPerMin:       12,
		MaxOutputTokens: 300,
		Temperature:     0.3,
		MaxPromptEvents: 8,
		CDReply:         "太快啦，请 %d 秒后再来",
		Zone:            zone,
		Now:             h.ck.now,
		Logf:            h.logs.f,
		Chat:            h.chat,
		EndingLead:      48 * time.Hour,
		SoonDays:        7,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	h.svc = aichat.New(fixture(), cfg)
	return h
}

func groupMsg(id, content, user string) *qq.Message {
	return &qq.Message{
		Kind:        qq.EventGroupAtMessage,
		ID:          id,
		Content:     content,
		GroupOpenID: "GROUP1",
		Author:      qq.Author{ID: "A" + user, MemberOpenID: user, Username: "某人", MemberRole: "member"},
	}
}

func ask(t *testing.T, h *harness, m *qq.Message) (string, error) {
	t.Helper()
	rep, err := h.svc.Fallback(context.Background(), m)
	return rep.Text, err
}

// 兜底要把当期事实一起问出去：人设在最前、当前时间带年份、三段列表按窗口归位。
func TestFallbackAsksTheModelWithCurrentFacts(t *testing.T) {
	h := newHarness(t, nil)
	got, err := ask(t, h, groupMsg("M1", "今天有什么活动呀", "USER1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != h.chat.answer {
		t.Errorf("回复 = %q，want %q", got, h.chat.answer)
	}
	if h.chat.calls != 1 {
		t.Fatalf("问了上游 %d 次，want 1", h.chat.calls)
	}
	if h.chat.asked[0] != "今天有什么活动呀" {
		t.Errorf("传给模型的原话 = %q", h.chat.asked[0])
	}

	sys := h.chat.system[0]
	for _, want := range []string{
		"引航者",              // 静态层：人设与称呼
		"【当期事实】",           // 动态段的分隔
		"2026-09-30 12:00", // 当前时间必须带年份：省了年份模型会自己编
		"正在进行：1 条",
		"48 小时内即将结束",
		"未来 7 天内开启",
		"絢麗夜空、煙火綻放",
		"雪融時分新芽綻",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("上下文里少了 %q：\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "還早得很") {
		t.Error("20 天后才开始的活动被列进来了，horizon 没生效")
	}
}

// EndingWithin 是 Active 的子集，同一条活动必须在两段里只出现一次。
// 这条就是钉那句"从进行中里剔掉临期"的：把剔重那行删掉，这里就红。
func TestEndingActivitiesAreNotListedTwice(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := ask(t, h, groupMsg("M1", "有什么快结束了", "USER1")); err != nil {
		t.Fatal(err)
	}
	sys := h.chat.system[0]
	for _, title := range []string{"悠悠漫時，愜意夕暮", "緊急懸賞"} {
		if n := strings.Count(sys, title); n != 1 {
			t.Errorf("「%s」在上下文里出现了 %d 次，want 1（临期条目被重复列进「正在进行」）", title, n)
		}
	}
	// 临期优先：10 小时后结束的那条该排在 48 小时那条前面。
	if strings.Index(sys, "悠悠漫時") > strings.Index(sys, "緊急懸賞") {
		t.Error("临期段没按结束时间升序：最要紧的那条排到了后面")
	}
}

// 条数上限是三段合计的，超出的条目要在列表里说明"还有几条没列"，
// 否则模型会把截剩的当成全部，回一句"一共就这两个活动"。
func TestPromptEventBudgetIsSharedAcrossSections(t *testing.T) {
	h := newHarness(t, func(c *aichat.Config) { c.MaxPromptEvents = 2 })
	if _, err := ask(t, h, groupMsg("M1", "活动一览", "USER1")); err != nil {
		t.Fatal(err)
	}
	sys := h.chat.system[0]
	if n := strings.Count(sys, "\n· "); n != 2 {
		t.Errorf("列了 %d 行活动，want 2（上限是三段合计 2 条）", n)
	}
	if !strings.Contains(sys, "其余 1 条未列出") {
		t.Errorf("截断没在列表里说明，模型会以为那就是全部：\n%s", sys)
	}
	// 优先级：预算先给临期，所以被截掉的不该是临期那两条。
	if !strings.Contains(sys, "悠悠漫時") || !strings.Contains(sys, "緊急懸賞") {
		t.Error("临期的两条被截掉了：预算该先给「即将结束」这一段")
	}
}

// 上下文总长度得有上界：静态层一个常量、动态段一个条数上限，两头都钉住。
func TestPromptStaysWithinBudget(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := ask(t, h, groupMsg("M1", "在吗", "USER1")); err != nil {
		t.Fatal(err)
	}
	sys := h.chat.system[0]
	// 上界的推导：静态层 1500 rune（persona 的硬上限）+ 每条活动行 80 rune 的余量
	// ×8 条 + 表头与同步状况 300 rune。行宽的实测值远小于 80（名称 40 封顶 +
	// 时刻 16 + 措辞 20），所以这里留的是刹车位，不是刻度。
	bound := 1500 + 80*8 + 300
	if n := len([]rune(sys)); n > bound {
		t.Errorf("上下文 %d rune，超过上界 %d", n, bound)
	}
}

// 冷却挡的是同一个人连刷：命中时零 API 调用，只回一句 cd_reply。
// 秒数向上取整，剩 1.1 秒要说"2 秒后"，说"1 秒后"会让人立刻再撞一次。
func TestCooldownBlocksOnlyTheSameSender(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := ask(t, h, groupMsg("M1", "第一句", "USER1")); err != nil {
		t.Fatal(err)
	}

	h.ck.advance(13*time.Second + 100*time.Millisecond) // 还要等 1.9 秒
	got, err := ask(t, h, groupMsg("M2", "第二句", "USER1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "太快啦，请 2 秒后再来" {
		t.Errorf("冷却回复 = %q，want「太快啦，请 2 秒后再来」", got)
	}
	if h.chat.calls != 1 {
		t.Errorf("冷却命中还是问了上游 %d 次，want 1（零 API 调用是这层的卖点）", h.chat.calls)
	}

	// 换个人不该被同一条冷却欠着。
	if _, err := ask(t, h, groupMsg("M3", "别人的句子", "USER2")); err != nil {
		t.Fatal(err)
	}
	if h.chat.calls != 2 {
		t.Errorf("换个人被冷却挡了（calls=%d），want 2：冷却的键是该到人的", h.chat.calls)
	}

	// 过了 15 秒就放行。
	h.ck.advance(2 * time.Second)
	if _, err := ask(t, h, groupMsg("M4", "等一下再问", "USER1")); err != nil {
		t.Fatal(err)
	}
	if h.chat.calls != 3 {
		t.Errorf("冷却期过了还没放行（calls=%d），want 3", h.chat.calls)
	}
}

// 文案里出现别的百分号不能被当动词吃掉：这是 cd_reply 不走 Sprintf 的全部理由。
func TestCDReplyKeepsLiteralPercentSigns(t *testing.T) {
	h := newHarness(t, func(c *aichat.Config) {
		c.CDReply = "今天 100% 收到了，请 %d 秒后再来"
	})
	if _, err := ask(t, h, groupMsg("M1", "先问一句", "USER1")); err != nil {
		t.Fatal(err)
	}
	h.ck.advance(5 * time.Second)
	got, err := ask(t, h, groupMsg("M2", "再问一句", "USER1"))
	if err != nil {
		t.Fatal(err)
	}
	want := "今天 100% 收到了，请 10 秒后再来"
	if got != want {
		// 格式串里不写那个百分号：Errorf 自己就会把它当动词吃掉，
		// 这正是 cdReply 不走 Sprintf 的理由。
		t.Errorf("冷却回复 = %q", got)
	}
}

// 全局配额只在多人同时开口时起作用（兜底流是串行的）。超出的一律静默：
// 不回"太忙"，那会把配额问题变成刷屏问题。
func TestGlobalQuotaDeniesSilently(t *testing.T) {
	h := newHarness(t, func(c *aichat.Config) { c.MaxPerMin = 2 })
	for i, user := range []string{"U1", "U2", "U3", "U4"} {
		got, err := ask(t, h, groupMsg(fmt.Sprintf("M%d", i+1), "问一句", user))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case i < 2 && got != h.chat.answer:
			t.Errorf("第 %d 个人被挡了（桶的突发位是 2）", i+1)
		case i >= 2 && got != "":
			t.Errorf("第 %d 个人超出配额还回了话：%q", i+1, got)
		}
	}
	if h.chat.calls != 2 {
		t.Errorf("问了上游 %d 次，want 2", h.chat.calls)
	}
	if !strings.Contains(h.logs.String(), "配额") {
		t.Errorf("配额丢弃没留日志：%s", h.logs.String())
	}
}

// 上游失败要当没说过：回复为空、错误原样交回给机制层落日志。
// 命令那条路会把 err.Error() 回给用户，兜底这条刻意不，两种政策不能混。
func TestUpstreamFailureStaysSilent(t *testing.T) {
	h := newHarness(t, nil)
	h.chat.err = errors.New("上游说它很忙")
	got, err := ask(t, h, groupMsg("M1", "在吗", "USER1"))
	if err == nil {
		t.Fatal("上游失败却被当成成功返回了")
	}
	if got != "" {
		t.Errorf("失败还回了话：%q", got)
	}
	if strings.Contains(got, "上游说它很忙") {
		t.Error("错误串漏进了用户可见文本")
	}
}

// 上游给了 200 但没内容（审查吃掉）：不回话，也不算失败。
func TestEmptyAnswerIsSilentWithoutError(t *testing.T) {
	h := newHarness(t, nil)
	h.chat.answer = "   "
	got, err := ask(t, h, groupMsg("M1", "在吗", "USER1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("空答案被回了 %q", got)
	}
}

// 长消息压成一行并截断：贴一整屏聊天记录不该把事实列表挤出上下文。
func TestLongQuestionIsFlattenedAndClipped(t *testing.T) {
	h := newHarness(t, nil)
	long := strings.Repeat("啊", 500) + "\n第二行"
	if _, err := ask(t, h, groupMsg("M1", long, "USER1")); err != nil {
		t.Fatal(err)
	}
	q := h.chat.asked[0]
	if n := len([]rune(q)); n != 401 { // 400 个字 + Clip 的那个省略号
		t.Errorf("原话 %d rune，want 401（400 上限 + 省略号）：%q", n, q[len(q)-10:])
	}
	if strings.Contains(q, "\n") {
		t.Error("换行没被压成一行")
	}
}
