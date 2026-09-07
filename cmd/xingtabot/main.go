// Command xingtabot 是星塔活动机器人的主入口，也是全项目唯一一处把具体实现接起来的地方。
//
// 分层契约在这里落地：基础设施（bbolt 存储 / 日历索引 / 公告同步引擎 / 命令机制 / QQ 接入）
// 只以接口交给功能，功能自己决定要不要存东西、存什么。关掉一个功能就是删掉对应的那一行
// Register —— 它的命令、定时任务与命名空间写入会一起停（命名空间里的旧数据留着，
// 重新开启时续用）。
//
// 凭据只从 -creds 指定的文件读（默认 .probe/creds.json，已被 .gitignore 排除），
// AppSecret 与 access_token 不出现在任何输出里。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/feature/calexpiry"
	"xingta/internal/feature/calops"
	"xingta/internal/feature/calquery"
	"xingta/internal/kernel"
	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/qq"
	"xingta/internal/stellasora"
	"xingta/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		credsPath = flag.String("creds", ".probe/creds.json", "凭据文件路径（JSON: appId / clientSecret）")
		dbPath    = flag.String("db", "data/xingta.db", "bbolt 数据库文件路径")
		tz        = flag.String("tz", "+08:00", "公告日期与回复时间用的时区：+08:00 或 Asia/Shanghai")
		sourceURL = flag.String("source", stellasora.DefaultBaseURL, "公告源站点基地址")
		refresh   = flag.Duration("refresh", 30*time.Minute, "公告刷新间隔")
		lead      = flag.Duration("lead", 48*time.Hour, "活动结束前多久开始提醒")
		scan      = flag.Duration("scan", 10*time.Minute, "到期提醒的扫描间隔")
		remind    = flag.String("remind", "", "提醒目标，逗号分隔：g:<群 openid> / u:<用户 openid>；留空只记日志")
		admins    = flag.String("admin", "", "单聊管理员 openid 白名单，逗号分隔（群聊按群角色判定）")
		apiBase   = flag.String("api", qq.DefaultBaseURL, "QQ API 基地址")
		intents   = flag.Int64("intents", qq.IntentPublicMessages, "订阅的 intent 位掩码")
	)
	flag.Parse()

	logf := func(format string, args ...any) {
		fmt.Println(fmt.Sprintf(format, args...))
	}

	zone, err := parseZone(*tz)
	if err != nil {
		return err
	}
	targets, err := parseTargets(*remind)
	if err != nil {
		return err
	}
	creds, err := loadCreds(*credsPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- 基础设施：具体实现只在这个函数里出现 -----------------------------

	doc, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer doc.Close()

	cal := calendar.NewStore() // 同时满足 calendar.View（给功能）与 calendar.Writer（给同步引擎）

	client := qq.NewClientAt(creds.AppID, creds.AppSecret, *apiBase)
	profile, err := client.Me(ctx)
	if err != nil {
		return fmt.Errorf("获取机器人身份失败（先查凭据与 -api）: %w", err)
	}
	logf("身份确认: %s (%s)", profile.Username, profile.ID)

	src := stellasora.New(stellasora.Config{BaseURL: *sourceURL, Zone: zone})
	sync := annsync.NewFeature(doc, cal, src, nil, annsync.Config{Interval: *refresh, Logf: logf})
	reg := command.NewRegistry()

	// ---- 功能：一行一个，删掉即关掉 ---------------------------------------

	rt := kernel.NewRuntime(activeSink{client: client}, queue.DefaultPolicy(), cal)
	rt.Register(sync)
	rt.Register(calquery.New(reg, cal, calquery.Config{
		Zone:   zone,
		Status: annsync.NewStatusReader(doc, src.Name()), // 拿得到同步状况，回复尾巴才敢说"数据截至"
		Logf:   logf,
	}))
	rt.Register(calexpiry.New(doc, calexpiry.Config{
		Lead: *lead, Every: *scan, Targets: targets, Zone: zone, Logf: logf,
	}))
	rt.Register(calops.New(doc, reg, calops.Config{
		Source: src.Name(), Zone: zone, Refresh: sync, Logf: logf,
	}))

	// ---- QQ 接入：命令机制挂在事件入口上 ----------------------------------

	hub := qq.NewHub(nil, logf)
	command.Attach(reg, hub, client, command.Config{
		AdminOpenIDs: splitList(*admins),
		Logf:         logf,
	})

	gw, err := client.Gateway(ctx)
	if err != nil {
		return fmt.Errorf("获取网关地址失败: %w", err)
	}
	g := qq.NewGateway(qq.GatewayConfig{
		Tokens:  client.Tokens(), // 必须复用同一个 TokenSource，否则 HTTP 侧与 WS 侧互相挤掉对方的 token
		URL:     gw.URL,
		Intents: *intents,
		Shard:   [2]uint32{0, uint32(max(gw.Shards, 1))},
		Logf:    func(format string, args ...any) { logf("[网关] "+format, args...) },
	})

	logf("数据库: %s｜时区: %s｜刷新: %s｜提醒: 提前 %s，每 %s 扫一次，%d 个目标",
		*dbPath, zone.String(), *refresh, *lead, *scan, len(targets))
	if len(targets) == 0 {
		logf("没配 -remind，到期提醒只写日志不发送")
	}
	logf("命令: %s", strings.ReplaceAll(reg.HelpText(), "\n", "／"))
	logf("开始监听，Ctrl-C 退出。去 @ 机器人发「帮助」看看。")

	rtErr := make(chan error, 1)
	go func() { rtErr <- rt.Run(ctx) }()

	gwErr := g.Run(ctx, hub.Handle)
	stop() // 网关这条链路断了就没必要继续跑内核

	select {
	case err := <-rtErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("内核退出: %w", err)
		}
	case <-time.After(5 * time.Second):
		logf("内核 5 秒内没退出，直接收工")
	}

	st := rt.Stats()
	logf("收工。队列：投递 %d 条 / 发出 %d 条 / 合并 %d 次 / 过期丢弃 %d / 失败丢弃 %d",
		st.Submitted, st.Sent, st.Merged, st.DroppedExpired, st.DroppedFailed)

	if gwErr != nil && !errors.Is(gwErr, context.Canceled) {
		return fmt.Errorf("网关退出: %w", gwErr)
	}
	return nil
}

// activeSink 把队列里的主动消息投给平台。
//
// Target 里的前缀（g: / u:）是投递约定：功能只写字符串，不认识 QQ 的两个通道，
// 解释前缀是接入层的事。启动时已经用 parseTargets 校验过，所以这里再遇到坏前缀
// 只可能是代码问题。
type activeSink struct{ client *qq.Client }

func (s activeSink) Send(ctx context.Context, b *queue.Batch) error {
	openID, group, err := splitTarget(b.Target)
	if err != nil {
		return err
	}
	req := qq.SendRequest{MsgType: qq.MsgTypeText, Content: b.Text}
	if group {
		_, err = s.client.SendGroupMessage(ctx, openID, req)
	} else {
		_, err = s.client.SendC2CMessage(ctx, openID, req)
	}
	return err
}

// splitTarget 拆 "g:<group_openid>" / "u:<user_openid>"。
// openid 里不该有空白：parseTargets 只剪掉整项两端的空格，前缀后面多打一个空格
// 是手误，放过去就会变成一个永远投不到的目标。
func splitTarget(target string) (openID string, group bool, err error) {
	var id string
	switch {
	case strings.HasPrefix(target, "g:"):
		id, group = target[len("g:"):], true
	case strings.HasPrefix(target, "u:"):
		id = target[len("u:"):]
	default:
		return "", false, fmt.Errorf("提醒目标 %q 缺少前缀，应写成 g:<群 openid> 或 u:<用户 openid>", target)
	}
	if id == "" {
		return "", false, fmt.Errorf("提醒目标 %q 的前缀后面没有 openid", target)
	}
	if strings.TrimSpace(id) != id {
		return "", false, fmt.Errorf("提醒目标 %q 的 openid 带了空白，去掉空格再写", target)
	}
	return id, group, nil
}

// parseTargets 在启动时就把目标校验一遍：写错前缀的话，提醒会一直投不出去，
// 而队列只会安静地重试到丢弃——线上看就是"没提醒"，查不出原因。
func parseTargets(list string) ([]string, error) {
	items := splitList(list)
	out := make([]string, 0, len(items))
	for _, t := range items {
		if _, _, err := splitTarget(t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseZone 支持 "+08:00" / "-0730" / "8" 这样的偏移写法，也支持 "Asia/Tokyo" 这种名字。
//
// 默认固定 +08:00 而不是 time.Local：手机端的本地时区和时区数据库都不可信。
// 公告文本里的日期不带时区标注，所以这个值是**假设**——若官方其实写的是 JST，
// 所有时间会偏 1 小时，而这一点从文本内部无法验证，只能拿真实活动的结束时刻校准。
func parseZone(s string) (*time.Location, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("时区不能为空")
	}
	if loc, err := time.LoadLocation(s); err == nil {
		return loc, nil
	}
	body := s
	sign := 1
	switch body[0] {
	case '+':
		body = body[1:]
	case '-':
		sign, body = -1, body[1:]
	}
	hh, mm := body, "0"
	if parts := strings.SplitN(body, ":", 2); len(parts) == 2 {
		hh, mm = parts[0], parts[1]
	} else if len(body) == 4 { // -0730
		hh, mm = body[:2], body[2:]
	}
	h, err := strconv.Atoi(hh)
	if err != nil || h < 0 || h > 23 {
		return nil, fmt.Errorf("时区 %q 看不懂，写成 +08:00 或 Asia/Tokyo", s)
	}
	m, err := strconv.Atoi(mm)
	if err != nil || m < 0 || m > 59 {
		return nil, fmt.Errorf("时区 %q 的分钟部分看不懂，写成 +08:00", s)
	}
	return time.FixedZone(s, sign*(h*3600+m*60)), nil
}

// openDB 建好父目录再开库：bbolt 不会替你 mkdir，而 data/ 在干净的检出里不存在。
func openDB(path string) (store.Doc, error) {
	if dir := parentDir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("建数据库目录 %s: %w", dir, err)
		}
	}
	return store.OpenBolt(path)
}

func parentDir(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return ""
	}
	return path[:i]
}

type creds struct {
	AppID     string `json:"appId"`
	AppSecret string `json:"clientSecret"`
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
