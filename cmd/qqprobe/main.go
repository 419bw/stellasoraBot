// Command qqprobe 用自己的凭据验证 QQ 开放平台的域名与接口现状。
//
// 设计约束：凭据只在本进程内使用，任何输出都不包含 AppSecret 或完整 access_token。
// token 用 SHA-256 前 8 位做指纹，既能对比"两个域名拿到的是不是同一把"，又不泄露可用凭证。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"xingta/internal/qq"
)

type creds struct {
	AppID     string `json:"appId"`
	AppSecret string `json:"clientSecret"`
}

func main() {
	credsPath := flag.String("creds", ".probe/creds.json", "凭据文件路径（JSON: appId / clientSecret）")
	sendGroup := flag.String("send-group", "", "可选：真的往这个 group_openid 发一条联调文本消息（会出现在群里，只在测试群用）")
	flag.Parse()

	c, err := loadCreds(*credsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取凭据失败: %v\n", err)
		os.Exit(1)
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	ctx := context.Background()

	fmt.Printf("AppID: %s (secret 已加载，%d 字节，不打印)\n\n", c.AppID, len(c.AppSecret))

	tokenA := probeToken(ctx, hc, "A 新域名 api.bot.qq.com", qq.DefaultBaseURL, c)
	probeToken(ctx, hc, "B 旧域名 bots.qq.com（botgo 硬编码的那个）", "https://bots.qq.com", c)

	if tokenA != "" {
		probeMe(ctx, c)
		probeSandbox(ctx, hc, c, tokenA)
		probeSendRoute(ctx, hc, c, tokenA)
	}
	if *sendGroup != "" && tokenA != "" {
		probeRealSend(ctx, c, *sendGroup)
	}
}

func loadCreds(path string) (creds, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return creds{}, err
	}
	var c creds
	if err := json.Unmarshal(data, &c); err != nil {
		return creds{}, fmt.Errorf("%s 不是合法 JSON: %w", path, err)
	}
	if c.AppID == "" || c.AppSecret == "" {
		return creds{}, fmt.Errorf("%s 缺少 appId 或 clientSecret", path)
	}
	return c, nil
}

// probeToken 返回 token 指纹（失败时返回空串），失败详情走 describe。
func probeToken(ctx context.Context, hc *http.Client, label, baseURL string, c creds) string {
	ts := qq.NewTokenSource(c.AppID, c.AppSecret, baseURL, hc)
	tok, err := ts.Token(ctx)
	if err != nil {
		fmt.Printf("[%s]\n  FAIL %s\n\n", label, describe(err))
		return ""
	}
	fmt.Printf("[%s]\n  OK   access_token len=%d fingerprint=%s\n\n", label, len(tok), fingerprint(tok))
	return tok
}

func probeMe(ctx context.Context, c creds) {
	u, err := qq.NewClientAt(c.AppID, c.AppSecret, qq.DefaultBaseURL).Me(ctx)
	if err != nil {
		fmt.Printf("[C GET /users/@me]\n  FAIL %s\n\n", describe(err))
		return
	}
	fmt.Printf("[C GET /users/@me]\n  OK   id=%s username=%s bot=%v\n       share_url=%s\n       （鉴权头 QQBot <token> 可用）\n\n",
		u.ID, u.Username, u.Bot, u.ShareURL)
}

func probeSandbox(ctx context.Context, hc *http.Client, c creds, token string) {
	fmt.Println("[D 沙箱域名可达性]")
	for _, base := range []string{
		"https://sandbox.api.bot.qq.com",
		"https://sandbox.api.sgroup.qq.com",
		"https://api.bot.qq.com",
	} {
		status, detail := rawGet(ctx, hc, base+"/users/@me", token)
		fmt.Printf("  %-36s %s %s\n", base, status, detail)
	}
	fmt.Println()
}

// probeSendRoute 用一个明显非法的 group_openid 探路由是否存在。
// 目的只是区分「接口不在了」和「参数不被接受」，不会发出任何消息。
func probeSendRoute(ctx context.Context, hc *http.Client, c creds, token string) {
	fmt.Println("[E POST /v2/groups/<非法openid>/messages 路由探针]")
	status, detail := rawPost(ctx, hc, qq.DefaultBaseURL+"/v2/groups/PROBE_ONLY_NOT_A_REAL_OPENID/messages", token,
		map[string]any{"msg_type": 0, "content": "route probe"})
	fmt.Printf("  %s %s\n", status, detail)
	fmt.Println()
}

func probeRealSend(ctx context.Context, c creds, groupOpenID string) {
	fmt.Printf("[F 真实发送一条群消息到 %s…（测试群专用）]\n", groupOpenID)
	client := qq.NewClientAt(c.AppID, c.AppSecret, qq.DefaultBaseURL)
	msg, err := client.SendGroupMessage(ctx, groupOpenID, qq.SendRequest{
		MsgType: qq.MsgTypeText,
		Content: "【联调】星塔机器人域名/鉴权验证，可忽略。",
	})
	if err != nil {
		fmt.Printf("  FAIL %s\n\n", describe(err))
		return
	}
	fmt.Printf("  OK   message_id=%s timestamp=%s\n\n", msg.ID, msg.Timestamp)
}

type errBody struct {
	ErrCode int    `json:"err_code"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
}

func rawGet(ctx context.Context, hc *http.Client, url, token string) (string, string) {
	return rawCall(ctx, hc, http.MethodGet, url, token, nil)
}

func rawPost(ctx context.Context, hc *http.Client, url, token string, body any) (string, string) {
	return rawCall(ctx, hc, http.MethodPost, url, token, body)
}

func rawCall(ctx context.Context, hc *http.Client, method, url, token string, body any) (string, string) {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return "ERR", err.Error()
		}
		rdr = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return "ERR", err.Error()
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := hc.Do(req)
	if err != nil {
		return "UNREACHABLE", transportHint(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Sprintf("HTTP %d", resp.StatusCode), "响应读取失败"
	}
	var eb errBody
	if json.Unmarshal(data, &eb) == nil && (eb.ErrCode != 0 || eb.Code != 0) {
		return fmt.Sprintf("HTTP %d", resp.StatusCode),
			fmt.Sprintf("err_code=%d code=%d trace_id=%s message=%s", eb.ErrCode, eb.Code, eb.TraceID, eb.Message)
	}
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) > 160 {
		trimmed = trimmed[:160] + "…"
	}
	return fmt.Sprintf("HTTP %d", resp.StatusCode), trimmed
}

// describe 只透出可判读的结构化信息，绝不回显请求体。
func describe(err error) string {
	msg := err.Error()
	if idx := strings.Index(msg, "message="); idx >= 0 {
		head := strings.TrimSpace(msg[:idx])
		return head + " " + truncate(msg[idx:], 200)
	}
	if isTransport(msg) {
		return "网络不可达（DNS/TLS 失败）: " + truncate(msg, 160)
	}
	return truncate(msg, 220)
}

func isTransport(s string) bool {
	for _, k := range []string{"no such host", "TLS handshake", "timeout", "connection refused", "dial tcp"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func transportHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such host"):
		return "DNS 解析失败"
	case strings.Contains(msg, "timeout"):
		return "超时"
	default:
		return truncate(msg, 160)
	}
}

func fingerprint(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:4])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
