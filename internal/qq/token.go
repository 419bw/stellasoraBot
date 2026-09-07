package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 平台侧 access_token 生命周期 7200s，到期前 60s 内重新获取会拿到新 token、旧 token 在这 60s 内仍有效。
// 提前量取 60s 与之对齐，避免拿着即将失效的凭证去发消息。
const tokenExpiryDelta = 60 * time.Second

const (
	CodeTokenInvalid = 11244 // token 过期或不存在
)

type tokenResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresIn   flexInt64 `json:"expires_in"`
}

// flexInt64 同时接受 7200 与 "7200"：官方参数表把 expires_in 写成 number，返回示例却是字符串。
type flexInt64 int64

func (v *flexInt64) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*v = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("qq: 无法解析 expires_in %s", string(data))
	}
	*v = flexInt64(n)
	return nil
}

type TokenSource struct {
	appID     string
	appSecret string

	baseURL string
	hc      *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
	now    func() time.Time
}

func NewTokenSource(appID, appSecret, baseURL string, hc *http.Client) *TokenSource {
	return &TokenSource{
		appID:     appID,
		appSecret: appSecret,
		baseURL:   baseURL,
		hc:        hc,
		now:       time.Now,
	}
}

// Token 返回一个仍然有效的 access_token，必要时向平台重新获取。
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Add(tokenExpiryDelta).Before(t.expiry) {
		return t.token, nil
	}
	return t.fetchLocked(ctx)
}

// Invalidate 丢弃缓存凭证，用于收到 401 / err_code 11244 后强制重取。
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
}

func (t *TokenSource) fetchLocked(ctx context.Context) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"appId":        t.appID,
		"clientSecret": t.appSecret,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.baseURL+"/app/getAppAccessToken", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := t.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("qq: 请求 access_token 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := readLimited(resp.Body, maxResponseBytes)
	if err != nil {
		return "", fmt.Errorf("qq: 读取 access_token 响应失败: %w", err)
	}
	var tr tokenResponse
	apiErr := decodeError(resp.StatusCode, body, &tr)
	if apiErr != nil {
		// 请求体里有 clientSecret，任何情况下都不能把它带进错误信息
		apiErr.Operation = "获取 access_token"
		return "", apiErr
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("qq: 获取 access_token 返回空凭证")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		return "", fmt.Errorf("qq: access_token 返回非法有效期 %d 秒", int64(tr.ExpiresIn))
	}
	t.token = tr.AccessToken
	t.expiry = t.now().Add(ttl)
	return t.token, nil
}
