package biliwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Fetcher 是拉取 B站动态的抽象接口，方便单测完全脱离网络。
type Fetcher interface {
	FetchLatest(ctx context.Context, uid string) ([]DynamicItem, error)
}

// HTTPClient 实现真实的 B站动态抓取。
type HTTPClient struct {
	client  *http.Client
	mu      sync.Mutex
	imgKey  string
	subKey  string
	keyTime time.Time
}

func NewHTTPClient() *HTTPClient {
	jar, _ := cookiejar.New(nil)
	return &HTTPClient{
		client: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
		},
	}
}

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

func (c *HTTPClient) initCookies(ctx context.Context) error {
	// 1. 先访问首页初始化基础 cookie
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.bilibili.com/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// 2. 从 spi 获取 buvid3 和 buvid4
	spiReq, err := http.NewRequestWithContext(ctx, "GET", "https://api.bilibili.com/x/frontend/finger/spi", nil)
	if err != nil {
		return err
	}
	spiReq.Header.Set("User-Agent", userAgent)
	spiReq.Header.Set("Referer", "https://www.bilibili.com/")

	spiResp, err := c.client.Do(spiReq)
	if err != nil {
		return fmt.Errorf("biliwatch: 获取 spi 失败: %w", err)
	}
	defer spiResp.Body.Close()

	body, err := io.ReadAll(spiResp.Body)
	if err != nil {
		return err
	}

	var spiData struct {
		Code int `json:"code"`
		Data struct {
			B3 string `json:"b_3"`
			B4 string `json:"b_4"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &spiData); err != nil {
		return fmt.Errorf("biliwatch: 解析 spi 响应失败: %w", err)
	}

	u, _ := url.Parse("https://bilibili.com")
	c.client.Jar.SetCookies(u, []*http.Cookie{
		{Name: "buvid3", Value: spiData.Data.B3, Domain: ".bilibili.com", Path: "/"},
		{Name: "buvid4", Value: spiData.Data.B4, Domain: ".bilibili.com", Path: "/"},
	})
	return nil
}

func (c *HTTPClient) getWbiKeys(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	if c.imgKey != "" && c.subKey != "" && time.Since(c.keyTime) < 2*time.Hour {
		k1, k2 := c.imgKey, c.subKey
		c.mu.Unlock()
		return k1, k2, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.bilibili.com/x/web-interface/nav", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://www.bilibili.com/")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("biliwatch: 获取 nav 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	var navData struct {
		Code int `json:"code"`
		Data struct {
			WbiImg struct {
				ImgUrl string `json:"img_url"`
				SubUrl string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &navData); err != nil {
		return "", "", fmt.Errorf("biliwatch: 解析 nav 失败: %w", err)
	}

	imgKey := getKeyFromURL(navData.Data.WbiImg.ImgUrl)
	subKey := getKeyFromURL(navData.Data.WbiImg.SubUrl)
	if imgKey == "" || subKey == "" {
		return "", "", errors.New("biliwatch: nav 中未返回有效的 wbi img/sub url")
	}

	c.mu.Lock()
	c.imgKey = imgKey
	c.subKey = subKey
	c.keyTime = time.Now()
	c.mu.Unlock()

	return imgKey, subKey, nil
}

func (c *HTTPClient) FetchLatest(ctx context.Context, uid string) ([]DynamicItem, error) {
	u, _ := url.Parse("https://bilibili.com")
	if len(c.client.Jar.Cookies(u)) == 0 {
		_ = c.initCookies(ctx)
	}

	imgKey, subKey, err := c.getWbiKeys(ctx)
	if err != nil {
		// 容错：使用静态 fallback key（B 站公开常用的备选）
		imgKey = "7cd084941338484a827105b93b682852"
		subKey = "4932c493102447f8816041d33203235b"
	}

	params := map[string]string{
		"host_mid": uid,
	}
	query := SignWbi(params, imgKey, subKey, 0)
	apiURL := "https://api.bilibili.com/x/polymer/web-dynamic/v1/feed/space?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", fmt.Sprintf("https://space.bilibili.com/%s/dynamic", uid))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("biliwatch: 请求动态失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("biliwatch: 读取动态响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return nil, fmt.Errorf("biliwatch: 动态接口返回 HTTP %d: %s", resp.StatusCode, snippet)
	}

	var feed DynamicFeedResp
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("biliwatch: 解析动态 JSON 失败: %w", err)
	}

	if feed.Code != 0 {
		return nil, fmt.Errorf("biliwatch: 动态接口业务错误: code=%d, msg=%s", feed.Code, feed.Message)
	}

	return feed.Data.Items, nil
}
