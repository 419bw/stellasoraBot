package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 2026-08-10 起平台把所有接口域名统一为 api.bot.qq.com（changelog 20260810）。
const DefaultBaseURL = "https://api.bot.qq.com"

const maxResponseBytes = 1 << 20

type APIError struct {
	Operation  string
	HTTPStatus int
	ErrCode    int    `json:"err_code"`
	Code       int    `json:"code"`
	Message    string `json:"message"`
	TraceID    string `json:"trace_id"`
}

func (e *APIError) Error() string {
	op := e.Operation
	if op == "" {
		op = "调用"
	}
	return fmt.Sprintf("qq: %s 失败 http=%d err_code=%d code=%d trace_id=%s message=%s",
		op, e.HTTPStatus, e.ErrCode, e.Code, e.TraceID, e.Message)
}

// IsTokenRejected 表示凭证不被接受，调用方可丢弃缓存凭证后重试一次。
func (e *APIError) IsTokenRejected() bool {
	return e.HTTPStatus == http.StatusUnauthorized || e.ErrCode == CodeTokenInvalid || e.Code == CodeTokenInvalid
}

// IsPlatformRejection 表示平台明确拒了这条请求（拿到了 HTTP 响应且判为失败），
// 消息一定没发出去；网络错与 5xx 不算——那些可能已经送达，重试就是重发。
// HTTP 200 但带 err_code 的也算（token 场景实测过平台这么回），decodeError 里
// 只要 code 非零就进 APIError，所以这里按 HTTPStatus<500 一网打尽。
func IsPlatformRejection(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.HTTPStatus < 500
}

func decodeError(status int, body []byte, out any) *APIError {
	apiErr := &APIError{HTTPStatus: status}
	if err := json.Unmarshal(body, apiErr); err != nil {
		apiErr.Message = strings.TrimSpace(string(body))
		if status >= 400 {
			return apiErr
		}
		return nil
	}
	// 实测：access_token 失败时回 HTTP 200 + 仅含遗留字段 code（如 100007），不带 err_code。
	// 两个字段任一非零都算失败，否则真实原因会被吞成"空凭证"。
	if status >= 400 || apiErr.ErrCode != 0 || apiErr.Code != 0 {
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &APIError{
			HTTPStatus: status,
			Message:    "响应体无法按预期结构解析: " + err.Error(),
		}
	}
	return nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("qq: 响应超过 %d 字节上限", limit)
	}
	return data, nil
}

type Client struct {
	baseURL string
	hc      *http.Client
	tokens  *TokenSource

	replier *replier
}

func NewClient(appID, appSecret string) *Client {
	return NewClientAt(appID, appSecret, DefaultBaseURL)
}

// NewClientAt 用于指向沙箱或本地 mock 平台；baseURL 不带结尾斜杠。
func NewClientAt(appID, appSecret, baseURL string) *Client {
	hc := &http.Client{Timeout: 10 * time.Second}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      hc,
		tokens:  NewTokenSource(appID, appSecret, strings.TrimRight(baseURL, "/"), hc),
		replier: newReplier(),
	}
}

func (c *Client) do(ctx context.Context, operation, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("qq: %s 请求体编码失败: %w", operation, err)
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.tokens.Token(ctx)
		if err != nil {
			return err
		}
		var body *bytes.Reader = bytes.NewReader(nil)
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "QQBot "+tok)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, err := c.hc.Do(req)
		if err != nil {
			return fmt.Errorf("qq: %s 请求失败: %w", operation, err)
		}
		data, err := readLimited(resp.Body, maxResponseBytes)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("qq: %s 读取响应失败: %w", operation, err)
		}
		apiErr := decodeError(resp.StatusCode, data, out)
		if apiErr == nil {
			return nil
		}
		apiErr.Operation = operation
		// 凭证被拒时重取一次 token 再试，其余错误直接上抛
		if attempt == 0 && apiErr.IsTokenRejected() {
			c.tokens.Invalidate()
			continue
		}
		return apiErr
	}
	return fmt.Errorf("qq: %s 凭证重试后仍失败", operation)
}

// BotProfile 对应 GET /users/@me 的响应体。
// union_openid / union_user_account 需特殊申请后才会返回。
type BotProfile struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Avatar           string `json:"avatar"`
	Bot              bool   `json:"bot"`
	UnionOpenID      string `json:"union_openid,omitempty"`
	UnionUserAccount string `json:"union_user_account,omitempty"`
	ShareURL         string `json:"share_url,omitempty"`
	WelcomeMsg       string `json:"welcome_msg,omitempty"`
}

// Tokens 暴露 Client 内部持有的 token 缓存。网关建联必须复用同一个实例，
// 另起一个 TokenSource 会让 HTTP 侧和 WS 侧各自刷新，互相把对方的 token 挤掉。
func (c *Client) Tokens() *TokenSource { return c.tokens }

// Me 用于确认凭证与域名是否可用。
func (c *Client) Me(ctx context.Context) (BotProfile, error) {
	var u BotProfile
	if err := c.do(ctx, "获取机器人身份", http.MethodGet, "/users/@me", nil, &u); err != nil {
		return BotProfile{}, err
	}
	// HTTP 200 但字段对不上会解出空结构体，早前就是这样把"结构变更"吞成了"调用成功"
	if u.ID == "" {
		return BotProfile{}, fmt.Errorf("qq: /users/@me 响应中缺少 id 字段，响应结构与预期不符")
	}
	return u, nil
}

func (c *Client) SendGroupMessage(ctx context.Context, groupOpenID string, req SendRequest) (*SendResult, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if groupOpenID == "" {
		return nil, fmt.Errorf("qq: group_openid 为空")
	}
	var res SendResult
	path := "/v2/groups/" + url.PathEscape(groupOpenID) + "/messages"
	if err := c.do(ctx, "发送群消息", http.MethodPost, path, req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) SendC2CMessage(ctx context.Context, userOpenID string, req SendRequest) (*SendResult, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if userOpenID == "" {
		return nil, fmt.Errorf("qq: user_openid 为空")
	}
	var res SendResult
	path := "/v2/users/" + url.PathEscape(userOpenID) + "/messages"
	if err := c.do(ctx, "发送单聊消息", http.MethodPost, path, req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// 被动回复上限：群聊 5 次、单聊 4 次，超出后平台直接拒绝。
// 导出是因为这就是平台规定，调用方安排回复内容时要按它排，不是我们的实现细节。
const (
	MaxGroupReplies = 5
	MaxC2CReplies   = 4
)

// SendGroupReply 以被动方式回复某条群消息，自动递增 msg_seq 并挡住超出次数的回复。
func (c *Client) SendGroupReply(ctx context.Context, groupOpenID, msgID string, req SendRequest) (*SendResult, error) {
	return c.sendReply(ctx, c.SendGroupMessage, groupOpenID, msgID, req, MaxGroupReplies)
}

func (c *Client) SendC2CReply(ctx context.Context, userOpenID, msgID string, req SendRequest) (*SendResult, error) {
	return c.sendReply(ctx, c.SendC2CMessage, userOpenID, msgID, req, MaxC2CReplies)
}

func (c *Client) sendReply(ctx context.Context,
	send func(context.Context, string, SendRequest) (*SendResult, error),
	target, msgID string, req SendRequest, limit int) (*SendResult, error) {
	if msgID == "" {
		return nil, fmt.Errorf("qq: 被动回复缺少 msg_id")
	}
	seq, err := c.replier.next(msgID, limit)
	if err != nil {
		return nil, err
	}
	req.MsgID = msgID
	req.MsgSeq = seq
	return send(ctx, target, req)
}

// replier 记录每条来消息已经回复过几次。相同的 msg_id+msg_seq 重复发送会失败，
// 所以每次回复必须换一个递增的 seq。
type replier struct {
	mu  sync.Mutex
	seq map[string]int
}

func newReplier() *replier {
	return &replier{seq: make(map[string]int)}
}

func (r *replier) next(msgID string, limit int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seq[msgID] >= limit {
		return 0, fmt.Errorf("qq: 消息 %s 已回复 %d 次，达到上限", msgID, limit)
	}
	r.seq[msgID]++
	return r.seq[msgID], nil
}
