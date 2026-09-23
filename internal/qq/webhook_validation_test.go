package qq_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	. "xingta/internal/qq"
)

// 回调握手的畸形 d 与坏包形状：这些分支决定"配回调地址时平台为什么一直显示失败"
// 这类问题能不能从状态码上读出来（400 = 握手数据不对，200+d=1 = 包本身坏了）。

func nopHandler(context.Context, string, string, json.RawMessage) error { return nil }

func TestValidationRejectsIncompleteHandshake(t *testing.T) {
	h := NewCallbackHandler(webhookSecret, nopHandler)
	cases := map[string]string{
		"缺 event_ts":    `{"op":13,"d":{"plain_token":"PT"}}`,
		"缺 plain_token": `{"op":13,"d":{"event_ts":"1721000000"}}`,
		"d 不是对象":        `{"op":13,"d":"junk"}`,
		"d 整个缺席":        `{"op":13}`,
	}
	for name, body := range cases {
		rec := postCallback(t, h, body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400（体过签名但握手字段不全）", name, rec.Code)
		}
	}
}

func TestCallbackGarbageBodyAcksFailure(t *testing.T) {
	h := NewCallbackHandler(webhookSecret, nopHandler)
	body := `{"op":`
	rec := postCallback(t, h, body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rsp struct {
		Op int `json:"op"`
		D  any `json:"d"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rsp); err != nil {
		t.Fatalf("应答不是 JSON: %v (%s)", err, rec.Body)
	}
	if rsp.Op != OpCallbackAck || rsp.D != float64(1) {
		t.Errorf("应答 = %+v，want op=12,d=1（坏包让平台按失败处理）", rsp)
	}
}

func TestCallbackUnknownOpIsSilent(t *testing.T) {
	h := NewCallbackHandler(webhookSecret, nopHandler)
	rec := postCallback(t, h, `{"op":99}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("未知 op 不该有应答体，got %q", rec.Body.String())
	}
}
