package qq

import (
	"context"
	"strings"
	"testing"
)

// C2C（单聊）出站是本轮黑盒补测的高价值边界：群链路早已测透，单聊的路径、
// 空 openid、以及"回复上限群 5 / 单聊 4 不同"此前只靠命令层间接经过。

func TestSendC2CMessageUsesUserEndpoint(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")

	res, err := c.SendC2CMessage(context.Background(), "USER OPENID", SendRequest{
		MsgType: MsgTypeText, Content: "在的",
	})
	if err != nil {
		t.Fatalf("SendC2CMessage: %v", err)
	}
	if res == nil || res.ID == "" {
		t.Fatalf("SendResult 为空：%+v", res)
	}

	calls := mp.sendCalls()
	if len(calls) != 1 {
		t.Fatalf("平台收到 %d 次调用，want 1", len(calls))
	}
	// 空格必须转义成 %20：路径里塞原始 openid 会让带 "/" 的值注入到别的资源段
	if want := "/v2/users/USER%20OPENID/messages"; calls[0].path != want {
		t.Errorf("path = %q, want %q", calls[0].path, want)
	}
	if calls[0].body["content"] != "在的" {
		t.Errorf("body.content = %v", calls[0].body["content"])
	}
}

func TestSendC2CMessageRejectsEmptyOpenID(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	_, err := c.SendC2CMessage(context.Background(), "", SendRequest{
		MsgType: MsgTypeText, Content: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "user_openid") {
		t.Fatalf("err = %v，want 明确报 user_openid 为空", err)
	}
	if got := len(mp.sendCalls()); got != 0 {
		t.Errorf("空 openid 仍发出 %d 次请求：%v", got, mp.sendCalls())
	}
}

// 单聊回复上限 4（群是 5）：第 5 条必须被本地拦下，而不是白烧一次平台拒发。
func TestSendC2CReplySequenceAndLimit(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	req := SendRequest{MsgType: MsgTypeText, Content: "r"}

	for i := 1; i <= MaxC2CReplies; i++ {
		if _, err := c.SendC2CReply(context.Background(), "USER1", "MSG1", req); err != nil {
			t.Fatalf("第 %d 条回复被拒: %v", i, err)
		}
	}
	_, err := c.SendC2CReply(context.Background(), "USER1", "MSG1", req)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("第 %d 条应触顶被拒，got %v", MaxC2CReplies+1, err)
	}

	calls := mp.sendCalls()
	if len(calls) != MaxC2CReplies {
		t.Fatalf("平台只该收到 %d 条，got %d", MaxC2CReplies, len(calls))
	}
	for i, call := range calls {
		if call.body["msg_id"] != "MSG1" {
			t.Errorf("第 %d 条 msg_id = %v, want MSG1", i+1, call.body["msg_id"])
		}
		if seq, _ := call.body["msg_seq"].(float64); int(seq) != i+1 {
			t.Errorf("第 %d 条 msg_seq = %v，want %d（平台按 seq 区分多条回复）", i+1, call.body["msg_seq"], i+1)
		}
	}
}

func TestSendReplyRequiresMsgID(t *testing.T) {
	c, _ := newTestClient(t, "123456", "sec")
	req := SendRequest{MsgType: MsgTypeText, Content: "x"}
	if _, err := c.SendGroupReply(context.Background(), "GROUP1", "", req); err == nil ||
		!strings.Contains(err.Error(), "msg_id") {
		t.Errorf("群回复空 msg_id: err = %v，want 报缺 msg_id", err)
	}
	if _, err := c.SendC2CReply(context.Background(), "USER1", "", req); err == nil ||
		!strings.Contains(err.Error(), "msg_id") {
		t.Errorf("单聊回复空 msg_id: err = %v，want 报缺 msg_id", err)
	}
}
