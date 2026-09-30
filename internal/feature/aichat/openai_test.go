package aichat_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xingta/internal/feature/aichat"
)

// 这一份不注入假 Chat：让包按配置自己建客户端，打到 httptest 的假上游。
// 测的是协议本身——发出去的请求长什么样、四种坏响应（字符串码 / HTML 错误页 /
// 空 choices / 超限正文）怎么被降级成错误而不是 panic。
//
// 全文件零真实网络：假上游就在 127.0.0.1 上，端点由测试自己给。

type spy struct {
	hits   int
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

func (s *spy) handler(status int, contentType, body string, delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		s.hits++
		s.method = r.Method
		s.path = r.URL.Path
		s.query = r.URL.RawQuery
		s.auth = r.Header.Get("Authorization")
		s.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// testKey 是假上游用的假凭据，形状故意照真的写：下面有几条断言要查它会不会
// 漏进 URL、错误串或群里可见的文本里。
const testKey = "AQ.fake-key-for-tests"

// live 挂一个假上游并造出指向它的服务。
func live(t *testing.T, status int, contentType, body string, delay time.Duration, tweak func(*aichat.Config)) (*harness, *spy, *httptest.Server) {
	t.Helper()
	sp := &spy{}
	srv := httptest.NewServer(sp.handler(status, contentType, body, delay))
	t.Cleanup(srv.Close)

	h := &harness{ck: &clock{at: fix}, logs: &logs{}}
	cfg := aichat.Config{
		Endpoint:        srv.URL + "/v1/chat/completions",
		Model:           "model-under-test",
		APIKey:          testKey,
		Timeout:         2 * time.Second,
		Cooldown:        15 * time.Second,
		MaxPerMin:       60,
		MaxOutputTokens: 300,
		Temperature:     0.3,
		MaxPromptEvents: 8,
		CDReply:         "太快啦，请 %d 秒后再来",
		Zone:            zone,
		Now:             h.ck.now,
		Logf:            h.logs.f,
		EndingLead:      48 * time.Hour,
		SoonDays:        7,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	h.svc = aichat.New(fixture(), cfg)
	return h, sp, srv
}

const okBody = `{"choices":[{"message":{"role":"assistant","content":"在的在的~"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":618,"completion_tokens":12}}`

// 请求形状是这套接口的全部契约：地址、鉴权头、body 里那五个字段。
func TestOpenAIRequestShape(t *testing.T) {
	h, sp, _ := live(t, http.StatusOK, "application/json", okBody, 0, nil)

	got, err := ask(t, h, groupMsg("M1", "今晚有什么活动", "USER1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "在的在的~" {
		t.Errorf("回复 = %q", got)
	}
	if sp.hits != 1 {
		t.Fatalf("打了 %d 发，want 1", sp.hits)
	}
	if sp.method != http.MethodPost || sp.path != "/v1/chat/completions" {
		t.Errorf("请求 = %s %s，want POST /v1/chat/completions", sp.method, sp.path)
	}
	// 密钥只在 header 里。它要是出现在 URL query 或 body 里，Go 的传输层错误串
	// 就会带着它进日志（qq/token.go 为同一件事立过测试锁）。
	if sp.auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q，want %q", sp.auth, "Bearer "+testKey)
	}
	if sp.query != "" {
		t.Errorf("URL 上挂了 query：%q（key 绝不能走这里）", sp.query)
	}
	if strings.Contains(string(sp.body), testKey) {
		t.Error("请求体里带了密钥")
	}

	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens   int     `json:"max_tokens"`
		Temperature float64 `json:"temperature"`
	}
	if err := json.Unmarshal(sp.body, &req); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v\n%s", err, sp.body)
	}
	if req.Model != "model-under-test" {
		t.Errorf("model = %q", req.Model)
	}
	if req.MaxTokens != 300 || req.Temperature != 0.3 {
		t.Errorf("max_tokens/temperature = %d/%v，want 300/0.3", req.MaxTokens, req.Temperature)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v，want [system, user]", req.Messages)
	}
	if req.Messages[1].Content != "今晚有什么活动" {
		t.Errorf("user 内容 = %q", req.Messages[1].Content)
	}
	if !strings.Contains(req.Messages[0].Content, "【当期事实】") {
		t.Error("system 里没带上当期事实")
	}
}

// 没配密钥时不该发一个空 Bearer 头出去——那是"带着坏凭据敲门"，
// 与"根本不该敲门"在日志里长得一样，排查时会误导。
func TestNoKeyMeansNoAuthHeader(t *testing.T) {
	h, sp, _ := live(t, http.StatusOK, "application/json", okBody, 0, func(c *aichat.Config) { c.APIKey = "" })
	if _, err := ask(t, h, groupMsg("M1", "在吗", "USER1")); err != nil {
		t.Fatal(err)
	}
	if sp.auth != "" {
		t.Errorf("没配密钥也发了鉴权头：%q", sp.auth)
	}
}

// 401/403/400/404 是"再问一次也不会不一样"：熔断掉，5 分钟内一条都别再打上游。
// 429 与超时不算——那是暂时的，下一条可能就通了。
func TestPermanentFailureTripsBreaker(t *testing.T) {
	body := `{"error":{"message":"API key not valid. Please pass a valid API key.","type":"unauthorized","code":"invalid_api_key"}}`
	h, sp, _ := live(t, http.StatusForbidden, "application/json", body, 0, nil)

	if _, err := ask(t, h, groupMsg("M1", "在吗", "U1")); err == nil {
		t.Fatal("403 被判成了成功")
	} else if strings.Contains(err.Error(), "AQ.fake-key-for-tests") {
		t.Errorf("错误串里漏了密钥：%v", err)
	}
	if sp.hits != 1 {
		t.Fatalf("hits = %d，want 1", sp.hits)
	}

	// 换人来问（绕开冷却），必须一个字节都不再发出去。
	got, err := ask(t, h, groupMsg("M2", "在吗", "U2"))
	if err != nil || got != "" {
		t.Errorf("熔断期间回了话或报了错：(%q, %v)，want 静默且不回", got, err)
	}
	if sp.hits != 1 {
		t.Errorf("熔断没挡住：又打了上游 %d 发", sp.hits)
	}

	h.ck.advance(5 * time.Minute)
	// 静默期过了要重新去问。假上游仍然回 403，所以错误必然还在 —— 这里判的是
	// "有没有再打出去"，不是"有没有成功"。
	if _, err := ask(t, h, groupMsg("M3", "在吗", "U3")); err == nil {
		t.Fatal("上游还在回 403，不该报成功")
	}
	if sp.hits != 2 {
		t.Errorf("静默期过了没恢复去问（hits=%d），want 2", sp.hits)
	}
}

func TestBusyDoesNotTripBreaker(t *testing.T) {
	h, sp, _ := live(t, http.StatusTooManyRequests, "application/json",
		`{"error":{"message":"Resource has been exhausted","type":"rate_limit_error"}}`, 0, nil)

	if _, err := ask(t, h, groupMsg("M1", "在吗", "U1")); err == nil {
		t.Fatal("429 被判成了成功")
	}
	if _, err := ask(t, h, groupMsg("M2", "在吗", "U2")); err == nil {
		t.Fatal("第二条也该失败")
	}
	if sp.hits != 2 {
		t.Errorf("429 把上游熔断了（hits=%d），want 2：暂时的忙不该当成永久坏", sp.hits)
	}
}

// error.code 上游给过字符串、整数、null 三种。声明成 int 会让整个错误体解不出来，
// "上游到底说了什么"这句最需要看的话就丢了 —— 断言错误串里能读出 message 原文，
// 而不是回落到裸 JSON 正文。
func TestErrorCodeShapeDoesNotBreakParsing(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"字符串码", `{"error":{"message":"模型名写错了","code":"invalid_request_error"}}`},
		{"整数码", `{"error":{"message":"模型名写错了","code":401}}`},
		{"没有码", `{"error":{"message":"模型名写错了"}}`},
		{"码是 null", `{"error":{"message":"模型名写错了","code":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := live(t, http.StatusBadRequest, "application/json", tc.body, 0, nil)
			_, err := ask(t, h, groupMsg("M1", "在吗", "U1"))
			if err == nil {
				t.Fatal("400 被判成了成功")
			}
			if !strings.Contains(err.Error(), "模型名写错了") {
				t.Errorf("读不出 message，错误体反序列化多半被 code 的类型绊住了：%v", err)
			}
			if strings.Contains(err.Error(), `"code"`) {
				t.Errorf("错误串里是整段裸 JSON，说明没走结构体解析：%v", err)
			}
		})
	}
}

// CF 那一跳坏的时候给的是 HTML 错误页。必须降级成错误：不能 panic，
// 更不能把正文当模型回复发进群里。
func TestHTMLGatewayPageDegradesToError(t *testing.T) {
	page := "<!DOCTYPE html><html><head><title>Error 53</title></head><body>Cloudflare is having a bad day</body></html>"
	h, _, _ := live(t, http.StatusServiceUnavailable, "text/html; charset=utf-8", page, 0, nil)

	got, err := ask(t, h, groupMsg("M1", "在吗", "U1"))
	if err == nil {
		t.Fatal("反代的 HTML 错误页被当成了正常响应")
	}
	if got != "" {
		t.Errorf("把上游正文回给用户了：%q", got)
	}
	// 200 + HTML 是最阴的一种：状态码说没事，body 根本不是模型响应。
	h2, _, _ := live(t, http.StatusOK, "text/html", page, 0, nil)
	if got, err := ask(t, h2, groupMsg("M2", "在吗", "U1")); err == nil || got != "" {
		t.Errorf("200 配 HTML 正文被当成了正常回复：(%q, %v)", got, err)
	}
}

// 上游"成功"但零产出：不回话，但日志里要留下 finish_reason，
// 否则事后分不清是被内容审查吃掉了、还是链路根本没说通。
func TestEmptyChoicesAndNullContentStaySilent(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"choices 为空", `{"choices":[]}`},
		{"content 为 null", `{"choices":[{"message":{"content":null},"finish_reason":"content_filter"}]}`},
		{"content 是空串", `{"choices":[{"message":{"content":"   "},"finish_reason":"stop"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := live(t, http.StatusOK, "application/json", tc.body, 0, nil)
			got, err := ask(t, h, groupMsg("M1", "在吗", "U1"))
			if err != nil {
				t.Fatalf("零产出被判成失败: %v", err)
			}
			if got != "" {
				t.Errorf("零产出还回了话：%q", got)
			}
			lg := h.logs.String()
			if !strings.Contains(lg, "finish") && !strings.Contains(lg, "choices 为空") {
				t.Errorf("零产出没在日志里留下成因：%s", lg)
			}
		})
	}
}

// 被 max_output_tokens 切短的半句话照样发（不回一句"没听懂"更亏），
// 但日志要写明是截断，那条就是"该调上限"的依据。
func TestLengthFinishIsSentAndLogged(t *testing.T) {
	h, _, _ := live(t, http.StatusOK, "application/json",
		`{"choices":[{"message":{"content":"今晚有煙火"},"finish_reason":"length"}]}`, 0, nil)
	got, err := ask(t, h, groupMsg("M1", "有什么", "U1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "今晚有煙火" {
		t.Errorf("截断的回复被吞掉了：%q", got)
	}
	if !strings.Contains(h.logs.String(), "截断") {
		t.Errorf("截断没留日志：%s", h.logs.String())
	}
}

// token 账只能来自上游的 usage，估算不算 —— 这一条钉住它确实被读出来并打进日志。
func TestUsageTokensAreLogged(t *testing.T) {
	h, _, _ := live(t, http.StatusOK, "application/json", okBody, 0, nil)
	if _, err := ask(t, h, groupMsg("M1", "在吗", "U1")); err != nil {
		t.Fatal(err)
	}
	lg := h.logs.String()
	if !strings.Contains(lg, "tokens=618+12") {
		t.Errorf("usage 没进日志：%s", lg)
	}
}

// 时间预算挂在 ctx 上（不在 client 上再设一道墙）：上游装死到点就该撤。
func TestTimeoutComesFromConfig(t *testing.T) {
	h, sp, _ := live(t, http.StatusOK, "application/json", okBody, 300*time.Millisecond,
		func(c *aichat.Config) { c.Timeout = 30 * time.Millisecond })
	got, err := ask(t, h, groupMsg("M1", "在吗", "U1"))
	if err == nil {
		t.Fatal("上游拖过 timeout 还当成成功")
	}
	if got != "" {
		t.Errorf("超时了还回了话：%q", got)
	}
	if !strings.Contains(err.Error(), "超过 timeout") {
		t.Errorf("错误里看不出是超时：%v", err)
	}
	_ = sp
}
