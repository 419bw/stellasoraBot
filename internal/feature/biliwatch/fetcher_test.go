package biliwatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestHTTPClient_DowngradeToVisitor(t *testing.T) {
	var logs []string
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	client := NewHTTPClient("test_sessdata=123", logf)
	if client.getRawCookie() != "test_sessdata=123" {
		t.Fatalf("expected cookie to be set, got %q", client.getRawCookie())
	}

	// 第一次降级应成功
	ok := client.downgradeToVisitor("测试原因: code=-101")
	if !ok {
		t.Fatalf("expected downgradeToVisitor to return true on first call")
	}
	if client.getRawCookie() != "" {
		t.Fatalf("expected rawCookie to be empty after downgrade, got %q", client.getRawCookie())
	}

	// 第二次降级应返回 false，且不应产生多余日志
	ok2 := client.downgradeToVisitor("重复降级")
	if ok2 {
		t.Fatalf("expected downgradeToVisitor to return false when already visitor")
	}

	logMu.Lock()
	defer logMu.Unlock()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 log, got %d", len(logs))
	}
	if !strings.Contains(logs[0], "配置的 B站账号 Cookie 已失效") || !strings.Contains(logs[0], "测试原因: code=-101") {
		t.Fatalf("unexpected log message: %s", logs[0])
	}
}

func TestHTTPClient_FetchSpace_AutoDowngradeOnExpiredCookie(t *testing.T) {
	var logs []string
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	client := NewHTTPClient("SESSDATA=expired_token", logf)

	// 拦截 HTTP 请求模拟 B 站行为：
	// 1. /x/web-interface/nav: 返回正常的 WBI 键
	// 2. /x/frontend/finger/spi: 返回 buvid
	// 3. /x/polymer/web-dynamic/v1/feed/space:
	//    - 若携带 Cookie: 返回 code -101 (账号未登录)
	//    - 若无 Cookie: 返回 code 0，包含 1 条动态
	client.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		path := req.URL.Path
		switch {
		case strings.Contains(path, "/x/web-interface/nav"):
			body := `{"code":0,"data":{"isLogin":true,"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/7cd084941338484a827105b93b682852.png","sub_url":"https://i0.hdslb.com/bfs/wbi/4932c493102447f8816041d33203235b.png"}}}`
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		case strings.Contains(path, "/x/frontend/finger/spi"):
			body := `{"code":0,"data":{"b_3":"test_b3","b_4":"test_b4"}}`
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		case strings.Contains(path, "/x/polymer/web-dynamic/v1/feed/space"):
			cookie := req.Header.Get("Cookie")
			if strings.Contains(cookie, "SESSDATA") {
				// Cookie 过期
				body := `{"code":-101,"message":"账号未登录","ttl":1,"data":{}}`
				return &http.Response{
					StatusCode: 200,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			// 降级为访客后，无 Cookie 或仅有访客 Cookie
			body := `{"code":0,"message":"0","ttl":1,"data":{"has_more":true,"items":[{"id_str":"123456789"}]}}`
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		default:
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		}
	})

	ctx := context.Background()
	items, err := client.FetchLatest(ctx, "3546645778139206")
	if err != nil {
		t.Fatalf("expected FetchLatest to succeed via auto-downgrade, got err: %v", err)
	}
	if len(items) != 1 || items[0].IdStr != "123456789" {
		t.Fatalf("expected 1 item with id 123456789, got %v", items)
	}

	// 验证降级状态
	if client.getRawCookie() != "" {
		t.Fatalf("expected rawCookie to be cleared after auto-downgrade")
	}

	logMu.Lock()
	defer logMu.Unlock()
	foundWarning := false
	for _, l := range logs {
		if strings.Contains(l, "配置的 B站账号 Cookie 已失效") && strings.Contains(l, "-101") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("expected warning log about expired cookie, got logs: %v", logs)
	}
}

func TestHTTPClient_GetWbiKeys_DowngradeOnNavExpired(t *testing.T) {
	var logs []string
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	client := NewHTTPClient("SESSDATA=invalid_token", logf)

	client.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// nav 接口直接返回未登录
		body := `{"code":-101,"message":"账号未登录","data":{"isLogin":false,"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/7cd084941338484a827105b93b682852.png","sub_url":"https://i0.hdslb.com/bfs/wbi/4932c493102447f8816041d33203235b.png"}}}`
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})

	k1, k2, err := client.getWbiKeys(context.Background())
	if err != nil {
		t.Fatalf("expected getWbiKeys to succeed, got: %v", err)
	}
	if k1 == "" || k2 == "" {
		t.Fatalf("expected non-empty keys, got k1=%s, k2=%s", k1, k2)
	}

	// 检查是否触发了降级
	if client.getRawCookie() != "" {
		t.Fatalf("expected rawCookie to be cleared")
	}

	logMu.Lock()
	defer logMu.Unlock()
	foundWarning := false
	for _, l := range logs {
		if strings.Contains(l, "配置的 B站账号 Cookie 已失效") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("expected warning log about expired cookie, got logs: %v", logs)
	}
}
