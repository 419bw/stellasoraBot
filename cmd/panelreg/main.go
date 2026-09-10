// Command panelreg 用于通过 QQ 机器人 API-v2 注册或更新官方「指令面板」（Command Panel）。
//
// 对应开放平台官方文档：
// https://bot.q.qq.com/wiki/develop/api-v2/server-inter/menu-panel/
//
// 凭据支持四种传入方式（不强制依赖任何本地固定路径）：
// 1. 命令行参数：-appid <AppID> -secret <ClientSecret>
// 2. JSON 字符串：-json '{"appId": "1905559245", "clientSecret": "..."}'
// 3. 凭据文件：-creds <任意路径.json>
// 4. 环境变量：QQ_APP_ID 与 QQ_CLIENT_SECRET
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.bot.qq.com"

type creds struct {
	AppID        string `json:"appId"`
	ClientSecret string `json:"clientSecret"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   any    `json:"expires_in"`
	Code        int    `json:"code"`
	Message     string `json:"message"`
}

type PanelItem struct {
	Type      string `json:"type"`                 // "command" 或 "link"
	Name      string `json:"name"`                 // 点击后填入聊天框的指令名，如 "/calendar"，最多 14 字符
	Desc      string `json:"desc"`                 // 说明文本，最多 30 字符
	OnlyAdmin bool   `json:"only_admin,omitempty"` // 是否仅管理员可见
	Link      string `json:"link,omitempty"`       // 跳转链接
}

type PanelConfig struct {
	Items   []PanelItem `json:"items"`
	Remark  string      `json:"remark,omitempty"`
	Version int         `json:"version,omitempty"`
}

type CreatePanelReq struct {
	Scope      string      `json:"scope"`       // "group", "c2c", "channel", "dm"
	TargetType string      `json:"target_type"` // "all" 或 "specific"
	Panel      PanelConfig `json:"panel"`
}

type UpdatePanelReq struct {
	Panel PanelConfig `json:"panel"`
}

type PanelRecord struct {
	PanelID    string      `json:"panel_id"`
	Scope      string      `json:"scope"`
	TargetType string      `json:"target_type"`
	Panel      PanelConfig `json:"panel"`
	Version    int         `json:"version"`
}

type ListPanelsResp struct {
	Records    []PanelRecord `json:"records"`
	NextCursor string        `json:"next_cursor"`
	IsEnd      bool          `json:"is_end"`
	Code       int           `json:"code"`
	Message    string        `json:"message"`
}

type CreatePanelResp struct {
	PanelID string `json:"panel_id"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type APIErrorResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	var (
		appID     = flag.String("appid", "", "机器人 AppID")
		secret    = flag.String("secret", "", "机器人 ClientSecret")
		jsonStr   = flag.String("json", "", "JSON 凭据字符串，形如: {\"appId\": \"1905559245\", \"clientSecret\": \"...\"}")
		credsPath = flag.String("creds", "", "凭据 JSON 文件路径")
		apiBase   = flag.String("api", defaultBaseURL, "QQ 开放平台 API 基地址")
		scope     = flag.String("scope", "group,c2c", "生效场景，逗号分隔，可选：group（群聊）、c2c（单聊）")
		slash     = flag.Bool("slash", true, "指令名是否前缀斜杠 / (如 /calendar)，用户点击后回填到聊天框")
		listOnly  = flag.Bool("list", false, "仅查询并打印当前已配置的指令面板列表，不作修改")
		clean     = flag.Bool("clean", false, "删除现有全局指令面板（慎用）")
	)
	flag.Parse()

	c, err := resolveCreds(*appID, *secret, *jsonStr, *credsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "凭据错误: %v\n\n", err)
		fmt.Fprintln(os.Stderr, "用法示例：")
		fmt.Fprintln(os.Stderr, "  1. 命令行参数：")
		fmt.Fprintln(os.Stderr, "     go run ./cmd/panelreg -appid 1905559245 -secret <你的Secret>")
		fmt.Fprintln(os.Stderr, "  2. JSON 字符串：")
		fmt.Fprintln(os.Stderr, "     go run ./cmd/panelreg -json '{\"appId\": \"1905559245\", \"clientSecret\": \"...\"}'")
		fmt.Fprintln(os.Stderr, "  3. JSON 凭据文件：")
		fmt.Fprintln(os.Stderr, "     go run ./cmd/panelreg -creds /path/to/creds.json")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseURL := strings.TrimRight(*apiBase, "/")
	fmt.Printf("正在连接 QQ API (%s) 获取凭证 (AppID: %s)...\n", baseURL, c.AppID)
	token, err := getAccessToken(ctx, baseURL, c.AppID, c.ClientSecret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取 AccessToken 失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("AccessToken 获取成功！")

	scopes := strings.Split(*scope, ",")
	client := &http.Client{Timeout: 10 * time.Second}

	prefix := ""
	if *slash {
		prefix = "/"
	}

	// 先创建 5 个公共用户指令
	items := []PanelItem{
		{Type: "command", Name: prefix + "calendar", Desc: "当前版本的活动日历图"},
		{Type: "command", Name: prefix + "events", Desc: "查看正在进行的活动"},
		{Type: "command", Name: prefix + "ending", Desc: "查看即将结束的活动"},
		{Type: "command", Name: prefix + "upcoming", Desc: "查看即将开始的活动"},
		{Type: "command", Name: prefix + "help", Desc: "列出所有可用命令"},
	}

	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		fmt.Printf("\n--- 处理场景: %s ---\n", s)

		// 1. 查询该场景下现有面板
		records, err := listPanels(ctx, client, baseURL, token, s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询 %s 场景面板失败: %v\n", s, err)
			continue
		}

		fmt.Printf("当前已存在 %d 个面板配置\n", len(records))
		for i, r := range records {
			fmt.Printf("  [%d] ID=%s, TargetType=%s, Items=%d 个, Remark=%q\n",
				i+1, r.PanelID, r.TargetType, len(r.Panel.Items), r.Panel.Remark)
		}

		if *listOnly {
			continue
		}

		if *clean {
			for _, r := range records {
				fmt.Printf("正在删除面板 %s...\n", r.PanelID)
				if err := deletePanel(ctx, client, baseURL, token, r.PanelID); err != nil {
					fmt.Fprintf(os.Stderr, "删除面板 %s 失败: %v\n", r.PanelID, err)
				} else {
					fmt.Printf("[✓] 面板 %s 删除成功\n", r.PanelID)
				}
			}
			continue
		}

		// 2. 检查是否有现成的 target_type=all 面板
		var existingAll *PanelRecord
		for i := range records {
			if records[i].TargetType == "all" {
				existingAll = &records[i]
				break
			}
		}

		if existingAll != nil {
			// 更新已有面板
			fmt.Printf("发现全局面板 %s，正在调用 PUT 更新指令列表...\n", existingAll.PanelID)
			err := updatePanel(ctx, client, baseURL, token, existingAll.PanelID, items, "星塔日历指令面板")
			if err != nil {
				fmt.Fprintf(os.Stderr, "更新面板失败: %v\n", err)
			} else {
				fmt.Printf("[✓] 场景 %s 指令面板更新成功！已同步 5 个指令\n", s)
			}
		} else {
			// 创建全新面板
			fmt.Printf("未发现全局面板，正在调用 POST 创建全新全局面板...\n")
			panelID, err := createPanel(ctx, client, baseURL, token, s, items, "星塔日历指令面板")
			if err != nil {
				fmt.Fprintf(os.Stderr, "创建面板失败: %v\n", err)
			} else {
				fmt.Printf("[✓] 场景 %s 全新指令面板创建成功！ID: %s，已同步 5 个指令\n", s, panelID)
			}
		}
	}

	fmt.Println("\n全部操作执行完毕。")
}

func resolveCreds(flagAppID, flagSecret, flagJSON, flagPath string) (creds, error) {
	if flagAppID != "" && flagSecret != "" {
		return creds{AppID: strings.TrimSpace(flagAppID), ClientSecret: strings.TrimSpace(flagSecret)}, nil
	}
	if flagJSON != "" {
		var c creds
		if err := json.Unmarshal([]byte(flagJSON), &c); err != nil {
			return creds{}, fmt.Errorf("解析 -json 失败: %w", err)
		}
		if c.AppID == "" || c.ClientSecret == "" {
			return creds{}, fmt.Errorf("-json 中缺少 appId 或 clientSecret")
		}
		return c, nil
	}
	if flagPath != "" {
		data, err := os.ReadFile(flagPath)
		if err != nil {
			return creds{}, fmt.Errorf("读取凭据文件 %q 失败: %w", flagPath, err)
		}
		var c creds
		if err := json.Unmarshal(data, &c); err != nil {
			return creds{}, fmt.Errorf("解析凭据文件 %q 失败: %w", flagPath, err)
		}
		if c.AppID == "" || c.ClientSecret == "" {
			return creds{}, fmt.Errorf("凭据文件 %q 中缺少 appId 或 clientSecret", flagPath)
		}
		return c, nil
	}
	envID := os.Getenv("QQ_APP_ID")
	envSec := os.Getenv("QQ_CLIENT_SECRET")
	if envID != "" && envSec != "" {
		return creds{AppID: strings.TrimSpace(envID), ClientSecret: strings.TrimSpace(envSec)}, nil
	}
	return creds{}, fmt.Errorf("未提供 AppID 与 ClientSecret")
}

func getAccessToken(ctx context.Context, baseURL, appID, secret string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"appId":        appID,
		"clientSecret": secret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/app/getAppAccessToken", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var tr tokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return "", fmt.Errorf("解析响应失败 (HTTP %d): %s", resp.StatusCode, string(data))
	}
	if tr.Code != 0 || tr.AccessToken == "" {
		return "", fmt.Errorf("平台返回错误 code=%d, message=%s, body=%s", tr.Code, tr.Message, string(data))
	}
	return tr.AccessToken, nil
}

func listPanels(ctx context.Context, hc *http.Client, baseURL, token, scope string) ([]PanelRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v2/panels?scope="+scope+"&limit=50", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "QQBot "+token)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	var res ListPanelsResp
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	return res.Records, nil
}

func createPanel(ctx context.Context, hc *http.Client, baseURL, token, scope string, items []PanelItem, remark string) (string, error) {
	payload := CreatePanelReq{
		Scope:      scope,
		TargetType: "all",
		Panel: PanelConfig{
			Items:  items,
			Remark: remark,
		},
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v2/panels", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	var res CreatePanelResp
	if err := json.Unmarshal(data, &res); err != nil {
		return "", err
	}
	if res.PanelID == "" {
		return "", fmt.Errorf("响应缺少 panel_id: %s", string(data))
	}
	return res.PanelID, nil
}

func updatePanel(ctx context.Context, hc *http.Client, baseURL, token, panelID string, items []PanelItem, remark string) error {
	payload := UpdatePanelReq{
		Panel: PanelConfig{
			Items:  items,
			Remark: remark,
		},
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/v2/panels/"+panelID, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	return nil
}

func deletePanel(ctx context.Context, hc *http.Client, baseURL, token, panelID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/v2/panels/"+panelID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	return nil
}
