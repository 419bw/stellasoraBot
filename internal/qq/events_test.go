package qq_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	. "xingta/internal/qq"
)

// 这里的三个 fixture：两个是真连采集后脱敏的报文，一个是官方文档示例原文。
// 不要把断言写成「实现返回什么就断言什么」，要断言报文里本来有什么。
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", name, err)
	}
	return raw
}

func TestDecodeRealC2C(t *testing.T) {
	raw := loadFixture(t, "c2c_message_create.json")
	m, err := DecodeMessage(EventC2CMessage, raw)
	if err != nil {
		t.Fatalf("解析单聊报文失败: %v", err)
	}

	if m.Kind != EventC2CMessage {
		t.Errorf("Kind = %q，想要 %q", m.Kind, EventC2CMessage)
	}
	if !strings.HasPrefix(m.ID, "ROBOT1.0_") {
		t.Errorf("ID = %q，应以 ROBOT1.0_ 开头", m.ID)
	}
	if m.MessageType != MessageTypeText {
		t.Errorf("MessageType = %d，想要 %d", m.MessageType, MessageTypeText)
	}
	if m.Text() != "你好" {
		t.Errorf("Text() = %q，想要 %q", m.Text(), "你好")
	}
	if m.Content != "你好" {
		t.Errorf("单聊样本的 content 不该有前后空白，实际 %q", m.Content)
	}

	// 单聊事件没有群字段。
	if m.IsGroup() {
		t.Error("单聊报文被判成了群消息")
	}
	if m.GroupOpenID != "" || m.GroupID != "" {
		t.Errorf("单聊报文不应带群 openid，实际 %q / %q", m.GroupOpenID, m.GroupID)
	}

	// 真实样本：username 是空串。这是 Display() 必须兜底的直接原因。
	if m.Author.Username != "" {
		t.Errorf("单聊 username 应为空串，实际 %q", m.Author.Username)
	}
	if m.Author.Bot {
		t.Error("作者被判成了机器人，会造成自我回复")
	}
	if got := m.Author.OpenID(); got != m.Author.UserOpenID || got == "" {
		t.Errorf("OpenID() = %q，单聊应取 user_openid %q", got, m.Author.UserOpenID)
	}
	target, isGroup := m.ReplyTarget()
	if isGroup || target != m.Author.UserOpenID {
		t.Errorf("ReplyTarget() = (%q,%v)，单聊应为 (%q,false)", target, isGroup, m.Author.UserOpenID)
	}
	if d := m.Display(); !strings.HasSuffix(d, "…") {
		t.Errorf("Display() = %q，昵称缺失时应退回 openid 截断", d)
	}

	// 单聊的 ext 只有 msg_idx，没有 auth_token —— 与群事件的差异就体现在这里。
	if m.Scene.ExtValue("msg_idx") == "" {
		t.Error("ext 里应当有 msg_idx")
	}
	if got := m.Scene.ExtValue("auth_token"); got != "" {
		t.Errorf("单聊样本不该带 auth_token，实际 %q", got)
	}
	if got := m.Scene.ExtValue("nope"); got != "" {
		t.Errorf("取不存在的键应得空串，实际 %q", got)
	}
	if m.Scene.Source != "default" {
		t.Errorf("Scene.Source = %q，想要 default", m.Scene.Source)
	}

	ts, err := m.CreatedAt()
	if err != nil {
		t.Fatalf("CreatedAt() 失败: %v", err)
	}
	if ts.Year() != 2026 || ts.Month() != 9 || ts.Day() != 5 {
		t.Errorf("CreatedAt() = %v，想要 2026-09-05 那天", ts)
	}
	if _, offset := ts.Zone(); offset != 8*3600 {
		t.Errorf("CreatedAt() 时区偏移 = %d，想要 28800（+08:00）", offset)
	}
}

func TestDecodeRealGroupAt(t *testing.T) {
	raw := loadFixture(t, "group_at_message_create.json")
	m, err := DecodeMessage(EventGroupAtMessage, raw)
	if err != nil {
		t.Fatalf("解析群报文失败: %v", err)
	}

	if !m.IsGroup() {
		t.Error("群报文被判成了单聊")
	}
	target, isGroup := m.ReplyTarget()
	if !isGroup || target != m.GroupOpenID || target == "" {
		t.Errorf("ReplyTarget() = (%q,%v)，群聊应为 (group_openid,true)", target, isGroup)
	}
	if m.Text() != "你好" {
		t.Errorf("Text() = %q，平台剥掉 @ 后留有空白，必须被 Trim 掉", m.Text())
	}
	if m.Display() != "迪西" {
		t.Errorf("Display() = %q，想要昵称 %q", m.Display(), "迪西")
	}
	if !m.MemberRoleIs("owner") {
		t.Errorf("MemberRole = %q，想要 owner", m.Author.MemberRole)
	}
	if m.Author.OpenID() != m.Author.MemberOpenID {
		t.Errorf("OpenID() 群聊应优先取 member_openid")
	}
	if m.Scene.ExtValue("auth_token") == "" {
		t.Error("群事件样本里是有 auth_token 的，解析不出来就是解析写错了")
	}

	// group_id 只出现在真实报文、不在官方 schema 里，所以单独钉一下：
	// 它当前与 group_openid 同值，但那是观测、不是承诺，代码不许依赖。
	if m.GroupID == "" {
		t.Error("真实群报文带 group_id，解析后不应为空")
	}
	if m.GroupID != m.GroupOpenID {
		t.Logf("注意：本样本 group_id 与 group_openid 不同值")
	}
}

func TestDecodeOfficialDocExample(t *testing.T) {
	// 官方示例一字未改，用来卡住「我们自己的结构体把官方字段名拼错」这类问题。
	raw := loadFixture(t, "group_at_official.json")
	m, err := DecodeMessage(EventGroupAtMessage, raw)
	if err != nil {
		t.Fatalf("解析官方示例失败: %v", err)
	}
	if want := "/今日天气"; m.Text() != want {
		t.Errorf("Text() = %q，官方示例 content 是 %q，Trim 后应得 %q", m.Text(), " /今日天气 ", want)
	}
	if m.Display() != "小明" {
		t.Errorf("Display() = %q，想要 小明", m.Display())
	}
	if !m.MemberRoleIs("member") {
		t.Errorf("MemberRole = %q，想要 member", m.Author.MemberRole)
	}
	if m.Scene.ExtValue("msg_idx") != "REFIDX_xxxxxxxxxxxxxxx==" {
		t.Errorf("msg_idx 解析不对: %q", m.Scene.ExtValue("msg_idx"))
	}
	// 官方示例没有 attachments / mentions / ark_data，缺字段必须解成零值而不是报错。
	if len(m.Attachments) != 0 || len(m.Mentions) != 0 || m.ArkData != nil {
		t.Error("缺省字段没有解成零值")
	}
}

func TestExtValueUsesFirstMatchAndKeepsEqualsInValue(t *testing.T) {
	s := MessageScene{Ext: []string{"a=1", "msg_idx=xx=yy", "msg_idx=dup"}}
	if got := s.ExtValue("msg_idx"); got != "xx=yy" {
		t.Errorf("ExtValue = %q，value 里含 = 时不能被二次切分，且要取第一个匹配", got)
	}
	if got := s.ExtValue("a"); got != "1" {
		t.Errorf("ExtValue(a) = %q", got)
	}
	if got := (MessageScene{}).ExtValue("a"); got != "" {
		t.Errorf("空 ext 应得空串，实际 %q", got)
	}
}

func TestDecodeRejectsPayloadWithoutID(t *testing.T) {
	// 平台改结构最典型的表现就是关键字段消失，这里必须报错而不是返回一个空壳。
	_, err := DecodeMessage(EventC2CMessage, []byte(`{"content":"hi","timestamp":"2026-01-01T00:00:00+08:00"}`))
	if err == nil {
		t.Fatal("缺 id 的报文应当报错")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("报错信息没提 id: %v", err)
	}
	if _, err := DecodeMessage(EventC2CMessage, []byte(`{`)); err == nil {
		t.Error("畸形 JSON 应当报错")
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	// 平台加字段不能让我们解不开报文。
	raw := []byte(`{"id":"ROBOT1.0_x","content":"hi","timestamp":"2026-01-01T00:00:00+08:00","message_type":0,"brand_new_field":{"nested":[1,2,3]}}`)
	if _, err := DecodeMessage(EventC2CMessage, raw); err != nil {
		t.Fatalf("未知字段导致解析失败: %v", err)
	}
}

func TestArkMessageKeepsFields(t *testing.T) {
	// 形态按官方 ARKData 表构造（ark_type/fields 的键名都有出处），
	// 但真连样本还没抓到过卡片消息 —— 等 qqwatch 收到一条后应当替换掉这段。
	raw := []byte(`{
		"id":"ROBOT1.0_ark",
		"content":"",
		"timestamp":"2026-01-01T00:00:00+08:00",
		"message_type":3,
		"group_openid":"G1",
		"author":{"id":"U1","member_openid":"U1","username":"迪西","bot":false},
		"ark_data":{
			"ark_type":"miniapp",
			"ark_name":"小程序",
			"fields":{"title":"某B站视频","desc":"简介","jump_url":"https://b23.tv/x","source":"哔哩哔哩"}
		},
		"msg_elements":[{"message_type":3,"content":"外层","msg_elements":[{"message_type":0,"content":"内层"}]}]
	}`)
	m, err := DecodeMessage(EventGroupAtMessage, raw)
	if err != nil {
		t.Fatalf("解析卡片消息失败: %v", err)
	}
	if m.MessageType != MessageTypeArk {
		t.Errorf("MessageType = %d，想要 %d", m.MessageType, MessageTypeArk)
	}
	if m.Text() != "" {
		t.Errorf("卡片消息的 content 应为空，实际 %q", m.Text())
	}
	// 只看 Content 的功能会一条都抓不到，标题必须能从 ark_data 里取出来。
	for key, want := range map[string]string{
		"title": "某B站视频", "jump_url": "https://b23.tv/x", "source": "哔哩哔哩",
	} {
		if got := m.ArkData.FieldString(key); got != want {
			t.Errorf("FieldString(%q) = %q，想要 %q", key, got, want)
		}
	}
	if got := m.ArkData.FieldString("missing"); got != "" {
		t.Errorf("取不存在的字段应得空串，实际 %q", got)
	}
	var noArk Message
	if got := noArk.ArkData.FieldString("title"); got != "" {
		t.Error("nil ArkData 上取值不应 panic")
	}
}

func TestWalkElementsDepthCap(t *testing.T) {
	nest := func(depth int) string {
		const open = `[{"message_type":102,"msg_elements":`
		return `{"id":"m","content":"","timestamp":"2026-01-01T00:00:00+08:00","message_type":102,"msg_elements":` +
			strings.Repeat(open, depth) + `[]` + strings.Repeat(`}]`, depth) + `}`
	}
	count := func(t *testing.T, payload string) int {
		t.Helper()
		var m Message
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("构造嵌套报文失败: %v", err)
		}
		visited := 0
		m.WalkElements(func(MsgElement) bool {
			visited++
			return true
		})
		return visited
	}

	// 正对照：没到上限的层数必须一个不漏，否则「被截断」和「上限生效」就分不开了。
	if got := count(t, nest(3)); got != 3 {
		t.Errorf("3 层嵌套访问了 %d 个元素，想要 3", got)
	}
	// 负对照：官方结构是递归的，畸形/超深报文必须被上限挡住而不是栈溢出。
	// 不断言"恰好停在第 8 层"——那个数字是实现细节，会随版本调；
	// 契约是"一定会在远小于 400 的某处停下"，且浅嵌套不受影响。
	if got := count(t, nest(400)); got >= 400 || got > 64 {
		t.Errorf("400 层嵌套访问了 %d 个元素，想要在一个小上限处停下", got)
	}
}

func TestWalkElementsEarlyStopPropagates(t *testing.T) {
	m := Message{Elements: []MsgElement{
		{Content: "a", Elements: []MsgElement{{Content: "a1"}, {Content: "a2"}}},
		{Content: "b"},
	}}
	var seen []string
	m.WalkElements(func(e MsgElement) bool {
		seen = append(seen, e.Content)
		return e.Content != "a" // 在 a 上返回 false，整个遍历（含子树和兄弟）都该停
	})
	if strings.Join(seen, ",") != "a" {
		t.Errorf("seen = %v，回调返回 false 后不应继续遍历", seen)
	}
}

func TestCreatedAtRejectsBadTimestamp(t *testing.T) {
	m := Message{Timestamp: "1767225600"}
	if _, err := m.CreatedAt(); err == nil {
		t.Error("秒级时间戳不是 RFC3339，应当报错")
	}
}

func TestFixturesMatchDocumentedKeySets(t *testing.T) {
	// 卡住「有人手改了 testdata」：脱敏时断言过一遍键集合，这里再断言一遍。
	cases := map[string][]string{
		"c2c_message_create.json":      {"author", "content", "id", "message_scene", "message_type", "timestamp"},
		"group_at_message_create.json": {"author", "content", "group_id", "group_openid", "id", "message_scene", "message_type", "timestamp"},
		"group_at_official.json":       {"author", "content", "group_openid", "id", "message_scene", "message_type", "timestamp"},
	}
	for name, want := range cases {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(loadFixture(t, name), &obj); err != nil {
			t.Fatalf("%s 不是合法 JSON: %v", name, err)
		}
		got := make([]string, 0, len(obj))
		for k := range obj {
			got = append(got, k)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s 顶层键 = %v，想要 %v", name, got, want)
		}
	}
}
