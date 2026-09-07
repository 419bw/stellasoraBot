package qq

import (
	"context"
	"encoding/json"
	"net/http"
)

// HTTP 回调链路的 op 码。
const (
	OpDispatch           = 0
	OpHeartbeat          = 1
	OpHeartbeatAck       = 11
	OpCallbackAck        = 12
	OpCallbackValidation = 13
)

type Callback struct {
	Op      int             `json:"op"`
	Seq     uint32          `json:"s,omitempty"`
	Type    string          `json:"t,omitempty"`
	EventID string          `json:"id,omitempty"`
	Data    json.RawMessage `json:"d,omitempty"`
}

type validationRequest struct {
	PlainToken string `json:"plain_token"`
	EventTs    string `json:"event_ts"`
}

// EventHandler 处理 op=0 的业务事件。返回 error 会让回包 d=1，平台按失败重试投递，
// 所以耗时工作必须自行转异步，否则回调会在平台侧超时。
type EventHandler func(ctx context.Context, eventType, eventID string, data json.RawMessage) error

func NewCallbackHandler(secret string, onEvent EventHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := readLimited(r.Body, maxResponseBytes)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		pass, err := Verify(secret, r.Header.Get(HeaderTimestamp),
			r.Header.Get(HeaderSignature), body)
		if err != nil || !pass {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var cb Callback
		if err := json.Unmarshal(body, &cb); err != nil {
			writeJSON(w, map[string]any{"op": OpCallbackAck, "d": 1})
			return
		}
		switch cb.Op {
		case OpCallbackValidation:
			handleValidation(w, secret, cb.Data)
		case OpHeartbeat:
			writeJSON(w, map[string]any{"op": OpHeartbeatAck, "d": cb.Seq})
		case OpDispatch:
			d := 0
			if err := onEvent(r.Context(), cb.Type, cb.EventID, cb.Data); err != nil {
				d = 1
			}
			writeJSON(w, map[string]any{"op": OpCallbackAck, "d": d})
		}
	})
}

// handleValidation 是配置回调地址时平台的一次性握手：
// 用 event_ts+plain_token 作为签名内容，把 plain_token 原样带回。
func handleValidation(w http.ResponseWriter, secret string, data json.RawMessage) {
	var req validationRequest
	if err := json.Unmarshal(data, &req); err != nil || req.PlainToken == "" || req.EventTs == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sig, err := Sign(secret, req.EventTs, []byte(req.PlainToken))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"plain_token": req.PlainToken, "signature": sig})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
