package qq_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "xingta/internal/qq"
)

const webhookSecret = "naOC0ocQE3shWLAfffVLB1rhYPG7"

func postCallback(t *testing.T, h http.Handler, body string, sign bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/qqbot", strings.NewReader(body))
	if sign {
		ts := "1636373772"
		sig, err := Sign(webhookSecret, ts, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderSignature, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 握手回包的 signature 必须能被平台用同一 secret 推导出的公钥验过，
// 签名内容是 event_ts+plain_token，不含 HTTP body。
func TestValidationHandshakeProducesPlatformVerifiableSignature(t *testing.T) {
	var got EventHandler = func(context.Context, string, string, json.RawMessage) error { return nil }
	h := NewCallbackHandler(webhookSecret, got)

	body := `{"op":13,"d":{"plain_token":"PT-9527","event_ts":"1721000000"}}`
	rec := postCallback(t, h, body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rsp struct {
		PlainToken string `json:"plain_token"`
		Signature  string `json:"signature"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rsp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body)
	}
	if rsp.PlainToken != "PT-9527" {
		t.Errorf("plain_token = %q, want PT-9527", rsp.PlainToken)
	}
	ok, err := Verify(webhookSecret, "1721000000", rsp.Signature, []byte("PT-9527"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("平台侧验签失败，握手会被拒")
	}
	// 负对照：拿整段请求体当签名内容必须验不过
	if bad, _ := Verify(webhookSecret, "1721000000", rsp.Signature, []byte(body)); bad {
		t.Error("用 body 当签名内容竟然验过，说明签名内容没按 event_ts+plain_token 构造")
	}
}

func TestCallbackRejectsUnsignedOrTamperedRequest(t *testing.T) {
	var got EventHandler = func(context.Context, string, string, json.RawMessage) error { return nil }
	h := NewCallbackHandler(webhookSecret, got)

	unsigned := `{"op":0,"t":"GROUP_AT_MESSAGE_CREATE","d":{}}`
	if rec := postCallback(t, h, unsigned, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("未签名请求 status = %d, want 401", rec.Code)
	}

	signed := postCallback(t, h, unsigned, true)
	tampered := strings.Replace(unsigned, "GROUP_AT_MESSAGE_CREATE", "C2C_MESSAGE_CREATE", 1)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/qqbot", strings.NewReader(tampered))
	req.Header.Set(HeaderTimestamp, "1636373772")
	sig, err := Sign(webhookSecret, "1636373772", []byte(unsigned))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(HeaderSignature, sig)
	h.ServeHTTP(rec, req)

	if signed.Code != http.StatusOK {
		t.Fatalf("合法签名请求 status = %d, want 200", signed.Code)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("签名与 body 不匹配 status = %d, want 401", rec.Code)
	}
}

func TestHeartbeatAcksWithSameSeq(t *testing.T) {
	h := NewCallbackHandler(webhookSecret, func(context.Context, string, string, json.RawMessage) error { return nil })
	rec := postCallback(t, h, `{"op":1,"s":4273}`, true)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"d":4273,"op":11}` {
		t.Errorf("心跳回包 = %s, want {\"d\":4273,\"op\":11}", got)
	}
}

func TestDispatchAckEncodesHandlerOutcome(t *testing.T) {
	cases := []struct {
		name    string
		handler EventHandler
		want    string
	}{
		{"成功 d=0", func(context.Context, string, string, json.RawMessage) error { return nil }, `{"d":0,"op":12}`},
		{"失败 d=1 触发平台重试", func(context.Context, string, string, json.RawMessage) error {
			return context.DeadlineExceeded
		}, `{"d":1,"op":12}`},
	}
	for _, tc := range cases {
		rec := postCallback(t, NewCallbackHandler(webhookSecret, tc.handler),
			`{"op":0,"t":"GROUP_AT_MESSAGE_CREATE","id":"e1","d":{"id":"msg1"}}`, true)
		if got := strings.TrimSpace(rec.Body.String()); got != tc.want {
			t.Errorf("%s: 回包 = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestDispatchPassesEventMetadataToHandler(t *testing.T) {
	var (
		gotType, gotID string
		gotData        map[string]any
	)
	h := NewCallbackHandler(webhookSecret, func(_ context.Context, eventType, eventID string, data json.RawMessage) error {
		gotType, gotID = eventType, eventID
		return json.Unmarshal(data, &gotData)
	})
	postCallback(t, h, `{"op":0,"t":"GROUP_AT_MESSAGE_CREATE","id":"evt-7","d":{"id":"msg-1","group_id":"g1"}}`, true)

	if gotType != "GROUP_AT_MESSAGE_CREATE" || gotID != "evt-7" {
		t.Errorf("type/id = %q/%q, want GROUP_AT_MESSAGE_CREATE/evt-7", gotType, gotID)
	}
	if gotData["group_id"] != "g1" {
		t.Errorf("d 未原样透传: %v", gotData)
	}
}

func TestCallbackRejectsNonPost(t *testing.T) {
	h := NewCallbackHandler(webhookSecret, func(context.Context, string, string, json.RawMessage) error { return nil })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/qqbot", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}
