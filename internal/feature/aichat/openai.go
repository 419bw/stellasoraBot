package aichat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"xingta/internal/feature/text"
)

// openaiChat 实现 OpenAI 兼容协议：POST {endpoint}，messages 数组，max_tokens。
//
// 本文件只管这一套协议的编解码；传输、超时、限长、错误归类都在 client.go。
// 接第二协议（比如 Google 原生 generateContent）= 再加一个同样满足 Chat 的文件，
// 调用方、prompt、限流、机制层都不动。
type openaiChat struct {
	cfg Config
	hc  *http.Client
}

func newOpenAIChat(cfg Config) Chat {
	return &openaiChat{cfg: cfg, hc: newHTTPClient(cfg)}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// MaxTokens 与 Temperature 都不用 omitempty：0 在这两项里都可能是有意填的值
	// （不限输出 = 交给上游定；温度 0 = 完全不要扰动），省略掉就等于悄悄改了配置。
	// 配置文件里这两个键本来就必须给值，不存在"没填"这一态。
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		// Code 上游给过字符串（"invalid_api_key"）、整数（401）和 null 三种形态。
		// 声明成 int 会在遇到字符串码时让整个错误体解不出来——最需要看一眼的
		// 那句话反而看不到了。原样收着，判定一律靠 HTTP 状态码。
		Code json.RawMessage `json:"code"`
	} `json:"error"`
}

func (o *openaiChat) Chat(ctx context.Context, systemPrompt, userText string) (string, error) {
	payload, err := json.Marshal(chatRequest{
		Model: o.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userText},
		},
		MaxTokens:   o.cfg.MaxOutputTokens,
		Temperature: o.cfg.Temperature,
	})
	if err != nil {
		return "", fmt.Errorf("aichat: 请求体拼不出来: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("aichat: 请求建不出来（查 endpoint）: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	// 密钥走 header 而不是 URL query：Go 的传输层错误串带着完整 URL，
	// key 挂在 query 上就会顺着日志漏出去（qq/token.go 为同一件事立过测试锁）。
	if key := strings.TrimSpace(o.cfg.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	started := time.Now()
	resp, err := o.hc.Do(req)
	if err != nil {
		return "", &UpstreamError{Kind: failTransport, Reason: transportReason(err)}
	}
	defer resp.Body.Close()

	body, over, err := readCapped(resp.Body)
	if err != nil {
		return "", &UpstreamError{Status: resp.StatusCode, Kind: failTransport, Reason: "读响应失败"}
	}
	elapsed := time.Since(started)

	if resp.StatusCode >= 400 {
		return "", &UpstreamError{
			Status: resp.StatusCode,
			Kind:   classifyStatus(resp.StatusCode),
			Reason: describeError(body, over),
		}
	}
	if over {
		// 200 却给了超过 1MB：那不是一个模型回复，是反代把别的东西灌进来了。
		return "", &UpstreamError{Status: resp.StatusCode, Kind: failTransport, Reason: "响应体超出上限"}
	}

	var out chatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		// CF 的错误页、纯文本的网关报错都走到这儿。绝不把正文当回复发出去。
		return "", &UpstreamError{Status: resp.StatusCode, Kind: failTransport,
			Reason: "响应不是合法 JSON（反代自己坏了？）: " + headOf(body)}
	}
	if len(out.Choices) == 0 {
		// 上游"成功"但零产出（被内容审查整条吃掉是这个形状之一）。不回话，但留痕。
		o.cfg.Logf("aichat: 上游 %s HTTP %d 但 choices 为空（用时 %s）",
			o.cfg.Model, resp.StatusCode, elapsed.Round(time.Millisecond))
		return "", nil
	}
	ch := out.Choices[0]
	content := strings.TrimSpace(ch.Message.Content)
	// finish_reason 与 usage 每问一条都记：usage 是 token 账的唯一真实来源（估算不算），
	// 而 content_filter 与 length 是两种完全不同的成因，事后全靠这一行分辨。
	o.cfg.Logf("aichat: 上游 %s HTTP %d finish=%s tokens=%d+%d 用时=%s",
		o.cfg.Model, resp.StatusCode, ch.FinishReason,
		out.Usage.PromptTokens, out.Usage.CompletionTokens, elapsed.Round(time.Millisecond))
	if content == "" {
		// choices[0].message.content 为 null 也会走到这儿（解成空串）。
		return "", nil
	}
	if ch.FinishReason == "length" {
		// 被 max_output_tokens 切短的半句话照样发：回一句"没听懂"或干脆不回，
		// 比半句话更亏。日志里那条 finish=length 就是要收紧上限时的依据。
		o.cfg.Logf("aichat: 回答被 max_output_tokens(%d) 截断", o.cfg.MaxOutputTokens)
	}
	return content, nil
}

// describeError 从失败响应里抠出一句能看的话。
//
// 三种坏形状都要扛住：合法的 JSON 错误体、CF 的 HTML、超限的正文。
// 拿不到结构就退回正文开头，且只取一行、截断——正文里可能夹着 URL 与堆栈。
func describeError(body []byte, over bool) string {
	if over {
		return "错误响应超出上限"
	}
	var er errorResponse
	if err := json.Unmarshal(body, &er); err == nil && (er.Error.Message != "" || er.Error.Type != "") {
		return text.Clip(text.OneLine(strings.TrimSpace(er.Error.Type+" "+er.Error.Message)), 120)
	}
	return headOf(body)
}

// headOf 取正文开头一行，用于"上游说了话但听不懂"的场合。
func headOf(body []byte) string {
	if len(body) == 0 {
		return "空响应"
	}
	return text.Clip(text.OneLine(string(body)), 80)
}
