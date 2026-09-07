// Command qqwatch 用真实凭据连上 QQ 网关，把收到的每一条事件原样落盘，
// 供后续做「真实报文回放 + 故障注入」的测试夹具。
//
// 设计约束与 qqprobe 一致：AppSecret 与完整 access_token 不出现在任何输出里。
// 事件原文只写进 -out 指定的文件（默认在 .probe/ 下，已被 .gitignore 排除），
// 终端上只印摘要。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xingta/internal/qq"
)

type creds struct {
	AppID     string `json:"appId"`
	AppSecret string `json:"clientSecret"`
}

func main() {
	var (
		credsPath = flag.String("creds", ".probe/creds.json", "凭据文件路径（JSON: appId / clientSecret）")
		outPath   = flag.String("out", "", "事件原文落盘路径；留空则不落盘")
		baseURL   = flag.String("base", qq.DefaultBaseURL, "API 基地址")
		intents   = flag.Int64("intents", qq.IntentPublicMessages, "订阅的 intent 位掩码")
		once      = flag.Duration("timeout", 0, "收到信号之外，最长跑多久；0 表示不限")
	)
	flag.Parse()

	c, err := loadCreds(*credsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *once)
		defer cancel()
	}

	client := qq.NewClientAt(c.AppID, c.AppSecret, *baseURL)

	profile, err := client.Me(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取机器人身份失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("身份确认: %s (%s)\n", profile.Username, profile.ID)

	gw, err := client.Gateway(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取网关地址失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("网关地址: %s\n建议分片: %d\n建联配额: 总 %d / 剩余 %d / %dms 后重置\n\n",
		gw.URL, gw.Shards, gw.Session.Total, gw.Session.Remaining, gw.Session.ResetAfter)

	sink, err := newRecorder(*outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	defer sink.Close()

	g := qq.NewGateway(qq.GatewayConfig{
		Tokens:  client.Tokens(),
		URL:     gw.URL,
		Intents: *intents,
		Shard:   [2]uint32{0, uint32(max(gw.Shards, 1))},
		Logf:    func(format string, args ...any) { fmt.Println("[网关]", fmt.Sprintf(format, args...)) },
	})

	fmt.Println("开始监听，Ctrl-C 退出。去 @ 机器人发几条消息试试。")
	if err := g.Run(ctx, sink.handle); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "网关退出:", err)
		os.Exit(1)
	}
	fmt.Printf("\n结束。共采集 %d 条事件", sink.count)
	if sink.path != "" {
		fmt.Printf("，原文在 %s", sink.path)
	}
	fmt.Println()
}

func loadCreds(path string) (creds, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return creds{}, fmt.Errorf("读凭据文件失败: %w", err)
	}
	var c creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return creds{}, fmt.Errorf("%s 不是合法 JSON: %w", path, err)
	}
	if c.AppID == "" || c.AppSecret == "" {
		return creds{}, fmt.Errorf("%s 缺少 appId 或 clientSecret", path)
	}
	return c, nil
}

// recorder 把事件原文写盘，终端只印一行摘要。
type recorder struct {
	path  string
	f     *os.File
	enc   *json.Encoder
	count int
}

func newRecorder(path string) (*recorder, error) {
	r := &recorder{path: path}
	if path == "" {
		return r, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开事件文件失败: %w", err)
	}
	r.f, r.enc = f, json.NewEncoder(f)
	return r, nil
}

func (r *recorder) handle(ctx context.Context, ev qq.Event) error {
	r.count++
	fmt.Printf("  #%d %-28s seq=%-6d %d 字节  %s\n", r.count, ev.Type, ev.Seq, len(ev.Data), digest(ev.Data))

	if r.enc == nil {
		return nil
	}
	rec := struct {
		At   string          `json:"at"`
		Type string          `json:"type"`
		Seq  int64           `json:"seq"`
		Body json.RawMessage `json:"body"`
	}{At: time.Now().Format(time.RFC3339Nano), Type: ev.Type, Seq: ev.Seq, Body: ev.Data}
	if err := r.enc.Encode(rec); err != nil {
		return fmt.Errorf("事件落盘失败: %w", err)
	}
	return nil
}

func (r *recorder) Close() error {
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// digest 只印定位字段，避免把昵称等内容刷满屏；原文已经完整落盘了。
//
// 字段位置取自真连样本：openid 嵌在 author 下，顶层只有 group_openid，
// 且单聊事件的 author.username 是空串 —— 昵称拿不到，别指望显示。
func digest(raw json.RawMessage) string {
	var probe struct {
		ID          string `json:"id"`
		GroupOpenID string `json:"group_openid"`
		ChannelID   string `json:"channel_id"`
		MessageType int    `json:"message_type"`
		Author      struct {
			ID           string `json:"id"`
			UserOpenID   string `json:"user_openid"`
			MemberOpenID string `json:"member_openid"`
			Username     string `json:"username"`
			Bot          bool   `json:"bot"`
		} `json:"author"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "-"
	}
	out := ""
	if probe.GroupOpenID != "" {
		out += "群=" + head(probe.GroupOpenID, 10) + " "
	}
	if who := probe.Author.UserOpenID; who != "" {
		out += "用户=" + head(who, 10) + " "
	} else if probe.Author.MemberOpenID != "" {
		out += "成员=" + head(probe.Author.MemberOpenID, 10) + " "
	}
	if probe.Author.Username != "" {
		out += "昵称=" + head(probe.Author.Username, 12) + " "
	}
	if probe.ID != "" {
		out += "msg_id=" + head(probe.ID, 12) + " "
	}
	if probe.Content != "" {
		out += "内容=" + head(probe.Content, 24)
	}
	if out == "" {
		out = "-"
	}
	return out
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
