package qqsim

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 延迟打点是压测里最容易「静默失效」的一环：探针返回 false 时不报错，
// 只是样本数为 0，报告里的分位就全成了 0s。这一整组测试就是钉住这件事。

func TestGeneratedPayloadStartsWithID(t *testing.T) {
	g := DefaultGenerator()
	_, raw, id := g.NextGroupAt()

	if !strings.HasPrefix(string(raw), `{"id":"`) {
		t.Fatalf("报文第一个键不是 id，打点探针会全部落空：%s", head(string(raw), 40))
	}
	if !strings.HasPrefix(id, idEnvelope) {
		t.Errorf("id = %s，应以 %s 开头", id, idEnvelope)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("生成的报文不是合法 JSON: %v", err)
	}
	for _, key := range []string{"id", "group_openid", "content", "timestamp", "message_type", "author", "message_scene"} {
		if _, ok := back[key]; !ok {
			t.Errorf("报文缺少真连样本里应有的字段 %q", key)
		}
	}
	if _, ok := back["author"]; ok {
		var a map[string]json.RawMessage
		if err := json.Unmarshal(back["author"], &a); err != nil {
			t.Fatalf("author 不是对象: %v", err)
		}
		for _, key := range []string{"id", "member_openid", "member_role", "username", "bot"} {
			if _, ok := a[key]; !ok {
				t.Errorf("author 缺少 %q，与真连群事件形状不一致", key)
			}
		}
	}
}

// 报文体积要落在真实量级：太小会让压测数字虚高。
func TestGeneratedPayloadSizeIsRealistic(t *testing.T) {
	g := DefaultGenerator()
	_, raw, _ := g.NextGroupAt()
	if len(raw) < 350 {
		t.Errorf("群事件报文只有 %d 字节，真连样本是 700+ 字节量级，压测会低估成本", len(raw))
	}
	if len(raw) > 2048 {
		t.Errorf("群事件报文 %d 字节，比真实报文大得离谱，压测会高估成本", len(raw))
	}
	t.Logf("群事件报文 %d 字节，单聊 %d 字节", len(raw), func() int {
		_, c2c, _ := g.NextC2C()
		return len(c2c)
	}())
}

func TestProbeSentRoundTrips(t *testing.T) {
	g := DefaultGenerator()
	before := time.Now()
	typ, raw, id := g.NextGroupAt()
	if typ != EventGroupAtMessage {
		t.Errorf("事件类型 = %q", typ)
	}

	sent, ok := ProbeSent(raw)
	if !ok {
		t.Fatal("ProbeSent 在刚生成的报文上取不到发出时刻，压测报告里的延迟会是全 0")
	}
	if sent.Before(before.Add(-time.Second)) || sent.After(time.Now().Add(time.Second)) {
		t.Errorf("取回的发出时刻 %v 不在合理区间", sent)
	}
	// 读侧两条路径必须给同一个答案：从 id 取和从原文取。
	fromID, ok := ParseSent(id)
	if !ok {
		t.Fatal("ParseSent 在刚生成的 id 上取不到发出时刻")
	}
	if !fromID.Equal(sent) {
		t.Errorf("两条取法不一致：id=%v raw=%v", fromID, sent)
	}
}

func TestProbeSentRejectsRealPlatformShapes(t *testing.T) {
	// 真平台报文：id 里只有十六进制、没有第二个下划线；或者 id 根本不在开头。
	cases := map[string]string{
		"真格式 id":  `{"id":"ROBOT1.0_A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4","content":"x"}`,
		"id 不在开头": `{"author":{"id":"ROBOT1.0_123_4"},"id":"ROBOT1.0_123_4"}`,
		"太短":      `{"id":"`,
		"非消息":     `""`,
	}
	for name, payload := range cases {
		if _, ok := ProbeSent([]byte(payload)); ok {
			t.Errorf("%s 被误判成模拟器报文，打点会把真实事件也算进延迟里", name)
		}
	}
}

func TestGeneratorIDsUniqueAcrossKinds(t *testing.T) {
	g := DefaultGenerator()
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		_, _, id := g.NextGroupAt()
		if seen[id] {
			t.Fatalf("第 %d 条群消息 id 重复：%s —— 重复会被去重层挡掉，压测就变成空转", i, id)
		}
		seen[id] = true
		_, _, c2c := g.NextC2C()
		if seen[c2c] {
			t.Fatalf("单聊 id 与已有 id 重复：%s", c2c)
		}
		seen[c2c] = true
	}
}

func TestSampleMessageJSONIsDecodableShape(t *testing.T) {
	raw := SampleMessageJSON("m1", "内容")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if m["id"] != "m1" || m["content"] != "内容" {
		t.Errorf("id/content 没按传入值生成: %v", m["id"])
	}
	if m["message_scene"] == nil {
		t.Error("缺 message_scene，与真连样本形状不一致")
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
