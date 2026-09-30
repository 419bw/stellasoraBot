package aichat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

// Chat 是一次问答，与协议无关。
//
// 分界线画在这里：换上游、换模型名、换温度与超时都是改配置；换协议族（比如 Google
// 原生 generateContent）是再写一个实现文件，调用方一行不动。
//
// 返回 ("", nil) 表示"上游给了 200 但没有可发的内容"（被内容审查吃掉、choices 为空），
// 调用方据此不回话。
//
// 实现必须尊重 ctx 的截止：本包只在调用方那一级挂 WithTimeout，不在 client 上再设
// 一道墙——两处各设一遍超时，出问题时分不清是谁先到点。
type Chat interface {
	Chat(ctx context.Context, systemPrompt, userText string) (string, error)
}

// Failure 是一次上游失败的归类。
type Failure int

const (
	failNone      Failure = iota // 没失败
	failPermanent                // 再问一次也不会不一样：凭据被拒、model 名写错、请求本身坏了
	failBusy                     // 429：上游说太快了
	failTransport                // 5xx / 超时 / 连接重置 / 反代自己坏了
)

func (f Failure) String() string {
	switch f {
	case failPermanent:
		return "配置或凭据被拒"
	case failBusy:
		return "上游忙"
	case failTransport:
		return "传输或反代异常"
	default:
		return "无"
	}
}

// UpstreamError 是一次上游失败。
//
// 字段刻意不含请求 URL、请求正文与密钥：这些错误会被机制层写进日志，而日志可能被
// 截图（凭据不进日志这条红线）。Reason 是 error.type/message 的截断片段，可空。
// Status 为 0 表示连响应都没拿到（传输层失败）。
type UpstreamError struct {
	Status int
	Kind   Failure
	Reason string
}

func (e *UpstreamError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("aichat: 上游没答上（%s）: %s", e.Kind, e.Reason)
	}
	if e.Reason == "" {
		return fmt.Sprintf("aichat: 上游 HTTP %d（%s）", e.Status, e.Kind)
	}
	return fmt.Sprintf("aichat: 上游 HTTP %d（%s）: %s", e.Status, e.Kind, e.Reason)
}

// classifyStatus 把状态码归到三类。以 HTTP 状态码为准而不信错误体：OpenAI 形态的
// error.code 字段上游并不统一（字符串、整数、null 都见过），而错误体本身还可能不是
// JSON（反代自己坏的时候给你 HTML）。
func classifyStatus(status int) Failure {
	switch status {
	case http.StatusTooManyRequests:
		return failBusy
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest:
		return failPermanent
	default:
		return failTransport
	}
}

// permanentFailure 判的是"要不要熔断"。
//
// 401/403/400/404 都属于"同一个请求再发一次还是错"，熔断挡住的是每条闲话都白跑一趟
// 注定失败的往返；429 与超时不熔断，那是暂时的，下一条可能就通了。
func permanentFailure(err error) (bool, string) {
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Kind != failPermanent {
		return false, ""
	}
	return true, fmt.Sprintf("HTTP %d %s", ue.Status, ue.Reason)
}

// maxResponseBytes 是响应体上限。一次回答正常就几百字节；超这个数只可能是反代把整页
// 错误 HTML 灌回来了，那已经不是一个模型回复，没必要全读进内存。
const maxResponseBytes = 1 << 20

// readCapped 读到上限为止，返回正文与"是不是超限"。
// 多留一个字节就是为了分辨"正好这么大"和"更大"。
//
// 正常那一路读到 EOF 才返回，底层连接因此回得了空闲池（README 硬坑 10：只 Close
// 不排空会攒 TIME_WAIT）。超限那一路不排空：剩下的只会是反代灌回来的整页错误 HTML，
// 为了池化一条连接去读它才是本末倒置，代价是这条连接被丢弃、下次重建，每个异常
// 响应一次。
func readCapped(r io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxResponseBytes {
		return data[:maxResponseBytes], true, nil
	}
	return data, false, nil
}

// newHTTPClient 按部署参数造传输层。
//
// Proxy 留空 → 用默认的 nil Transport，它其实是 http.DefaultTransport，
// 会读 HTTP(S)_PROXY 环境变量（"Go 不读环境变量"是常见误解，值得写在这儿免得下一个人
// 又踩）。显式设了 Proxy 就换成自建 Transport，本机的 NO_PROXY 分派随之失效——
// 这是"配置文件里写明代理"的代价。
func newHTTPClient(cfg Config) *http.Client {
	p := ""
	if cfg.Proxy != "" {
		u, err := url.Parse(cfg.Proxy)
		if err != nil {
			cfg.Logf("aichat: proxy %q 解析失败，这次按直连跑: %v", cfg.Proxy, err)
		} else if u.Host == "" {
			cfg.Logf("aichat: proxy %q 没有主机名，这次按直连跑", cfg.Proxy)
		} else {
			p = cfg.Proxy
		}
	}
	if p == "" {
		return &http.Client{}
	}
	u, _ := url.Parse(p) // 上面已经解析成功过一次
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

// transportReason 从传输层错误里抠出一句能看的话，同时不把请求 URL 带出去。
// net/http 把错误包成 *url.Error，它的 Error() 开头就是 "Post https://...：",
// 而这里的错误是要进日志的。
func transportReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	var ne *net.OpError
	if errors.As(err, &ne) {
		return fmt.Sprintf("%s %s", ne.Op, ne.Err)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "整趟超过 timeout"
	case errors.Is(err, context.Canceled):
		return "进程停机取消"
	}
	return err.Error()
}
