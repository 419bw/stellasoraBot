// Command xingtabot 是星塔活动机器人的主入口，也是全项目唯一一处把具体实现接起来的地方。
//
// 分层契约在这里落地：基础设施（bbolt 存储 / 日历索引 / 公告同步引擎 / 命令机制 / QQ 接入）
// 只以接口交给功能，功能自己决定要不要存东西、存什么。关掉一个功能就是删掉对应的那一行
// Register —— 它的命令、定时任务与命名空间写入会一起停（命名空间里的旧数据留着，
// 重新开启时续用）。
//
// 可调参数只从配置文件读：命令行唯一的参数是这份文件的位置（默认 config.yml）。
// 机器级的值在 config.yml，各功能的值在 config/<功能>.yml 由那个功能包自己解析。
//
// 凭据仍只从 config.yml 的 creds 指向的文件读（默认 creds.json，已被 .gitignore 排除），
// AppSecret 与 access_token 不出现在任何输出里。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/command"
	"xingta/internal/config"
	"xingta/internal/feature/biliwatch"
	"xingta/internal/feature/calexpiry"
	"xingta/internal/feature/calops"
	"xingta/internal/feature/calposter"
	"xingta/internal/feature/calquery"
	"xingta/internal/feature/pushops"
	"xingta/internal/kernel"
	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/target"
	"xingta/internal/qq"
	"xingta/internal/render"
	"xingta/internal/stellasora"
	"xingta/internal/store"
)

func main() {
	initAndroidEnv()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// featureConfigDir 是各功能配置文件所在的目录：与总配置同级、按 cwd 相对定位
// （deploy/phone/start.sh 先 cd 到脚本目录，所以手机上那份 config/ 就在部署目录里）。
const featureConfigDir = "config"

func featureConfigPath(name string) string {
	return filepath.Join(featureConfigDir, name+".yml")
}

func run() error {
	configPath := flag.String("config", "config.yml", "部署配置文件：机器级参数在这份文件里，各功能参数在它旁边的 config/<功能>.yml 里")
	flag.Parse()

	logf := func(format string, args ...any) {
		fmt.Printf("%s %s\n", time.Now().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, args...))
	}

	cfg, cfgFile, err := config.LoadRun(*configPath)
	if err != nil {
		return err
	}
	// 先把进程真正吃进去的每个值连注释打一遍，再谈凭据与网络：这样"值从文件搬到
	// 进程里"这件事，不握一份能连上平台的凭据也核得了。
	cfgFile.Dump(logf)

	// 所有配置读完才开始动手：缺一个文件、配错一个值，不该等身份确认走通网络之后
	// 才暴露。出图功能由 chrome 那一项决定，关着就不要求它那几个配置文件存在。
	syncDC, file, err := annsync.LoadDeployConfig(featureConfigPath("annsync"))
	if err != nil {
		return err
	}
	file.Dump(logf) // 值 + 那句注释，逐行落到启动日志上
	expiryDC, file, err := calexpiry.LoadDeployConfig(featureConfigPath("calexpiry"))
	if err != nil {
		return err
	}
	file.Dump(logf)
	queryDC, file, err := calquery.LoadDeployConfig(featureConfigPath("calquery"))
	if err != nil {
		return err
	}
	file.Dump(logf)
	opsDC, file, err := calops.LoadDeployConfig(featureConfigPath("calops"))
	if err != nil {
		return err
	}
	file.Dump(logf)

	var posterDC calposter.DeployConfig
	var biliDC biliwatch.DeployConfig
	if cfg.Chrome != "" {
		dc, file, err := calposter.LoadDeployConfig(featureConfigPath("calposter"))
		if err != nil {
			return err
		}
		file.Dump(logf)
		posterDC = dc
		// 动态推图与海报共用同一个浏览器开关，所以它的配置也在这条分支里读：
		// chrome 留空时不该要求 config/biliwatch.yml 存在。
		bdc, file, err := biliwatch.LoadDeployConfig(featureConfigPath("biliwatch"))
		if err != nil {
			return err
		}
		file.Dump(logf)
		biliDC = bdc
	}

	zone, err := parseZone(cfg.TZ)
	if err != nil {
		return err
	}
	creds, err := loadCreds(cfg.Creds)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- 基础设施：具体实现只在这个函数里出现 -----------------------------

	doc, err := openDB(cfg.DB)
	if err != nil {
		return err
	}
	defer doc.Close()

	cal := calendar.NewStore() // 同时满足 calendar.View（给功能）与 calendar.Writer（给同步引擎）

	targetStore, err := target.NewStore(doc)
	if err != nil {
		return fmt.Errorf("初始化推送目标存储失败: %w", err)
	}

	client := qq.NewClientAt(creds.AppID, creds.AppSecret, cfg.API)
	// 命令回图走带 file_info 缓存的发送器：同字节命中就跳过四步上传；缓存那份被平台
	// 判死时 command.reply 会 Forget + 就地重传。主动推送那条路仍用裸 client——
	// 队列自己带退避重试，缓存摘除那套兜底不该重复两份。
	sender := qq.NewMediaCache(client, 0)
	profile, err := client.Me(ctx)
	if err != nil {
		return fmt.Errorf("获取机器人身份失败（先查凭据与配置文件的 api）: %w", err)
	}
	logf("身份确认: %s (%s)", profile.Username, profile.ID)

	src := stellasora.New(stellasora.Config{BaseURL: cfg.Source, Zone: zone})
	sync := annsync.NewFeature(doc, cal, src, nil, syncDC.ToConfig(annsync.Config{Logf: logf}))
	reg := command.NewRegistry()

	// ---- 功能：一行一个，删掉即关掉 ---------------------------------------

	// 出图能力依赖无头浏览器：配置文件里 chrome 留空就是不启用出图相关功能——
	// 本机没有浏览器时，到期提醒与运维命令照常跑，而不是启动后每次发图都失败。
	var (
		poster *calposter.Poster
		bili   *biliwatch.Feature
	)
	if cfg.Chrome != "" {
		browser := &render.Browser{Bin: cfg.Chrome}
		// ToConfig 只往接线好的 Config 上盖部署数值：那六个数的唯一出处是
		// config/calposter.yml，代码里已经不留默认值。
		poster = calposter.New(posterDC.ToConfig(calposter.Config{
			Doc:     doc,
			Records: func() ([]annsync.Rec, error) { return annsync.ReadRecs(doc, src.Name()) },
			Cap:     browser,
			ArtDir:  artDir(cfg.DB),
			Label:   stellasora.ProvVersion,
			Zone:    zone,
			Client:  &http.Client{Timeout: 30 * time.Second},
			Reg:     reg,
			Logf:    logf,
		}))
		biliCookie := strings.TrimSpace(creds.BiliCookie)
		if biliCookie != "" {
			logf("biliwatch: 已配置 B站登录态 Cookie (长度 %d 字节)，防风控模式已就绪", len(biliCookie))
		} else {
			logf("biliwatch: 未配置 B站登录态 Cookie，使用匿名访客模式")
		}
		bili = biliwatch.New(biliDC.ToConfig(biliwatch.Config{
			Doc:    doc,
			Cap:    browser,
			Cookie: biliCookie,
			Logf:   logf,
		}))
	} else {
		logf("config 里 chrome 留空，出图功能不启用（日历海报与 B站动态推图都不会有；到期提醒照常）")
	}

	// 队列的"当日额度"日界线跟着同一个时区走：它此前硬编码 +08:00，与 api.tz
	// 各持一处，改 tz 会让日历与额度切线分家。
	policy := queue.DefaultPolicy()
	policy.DayZone = zone
	rt := kernel.NewRuntime(activeSink{client: client, poster: poster, bili: bili}, policy, cal, targetStore, logf)
	rt.Register(sync)
	rt.Register(calquery.New(reg, cal, queryDC.ToConfig(calquery.Config{
		Zone:   zone,
		Status: annsync.NewStatusReader(doc, src.Name()), // 拿得到同步状况，回复尾巴才敢说"数据截至"
		Logf:   logf,
	})))
	rt.Register(calexpiry.New(doc, expiryDC.ToConfig(calexpiry.Config{Zone: zone, Logf: logf})))
	rt.Register(calops.New(doc, reg, opsDC.ToConfig(calops.Config{
		Source: src.Name(), Zone: zone, Refresh: sync, Logf: logf,
	})))
	rt.Register(pushops.New(reg, targetStore, pushops.Config{Logf: logf}))
	if poster != nil {
		rt.Register(poster)
	}
	if bili != nil {
		rt.Register(bili)
	}

	// ---- QQ 接入：命令机制挂在事件入口上 ----------------------------------

	hub := qq.NewHub(nil, logf)
	// 命令走 Attach 内部的被动 worker：网关循环（心跳/判死）不等命令跑完。
	// 返回值这里用不上（Drain 供测试与诊断），生命周期随 ctx——与 rt、网关同级。
	command.Attach(ctx, reg, hub, sender, command.Config{
		AdminOpenIDs: cfg.Admin,
		Logf:         logf,
	})

	gw, err := client.Gateway(ctx)
	if err != nil {
		return fmt.Errorf("获取网关地址失败: %w", err)
	}
	g := qq.NewGateway(qq.GatewayConfig{
		Tokens: client.Tokens(), // 必须复用同一个 TokenSource，否则 HTTP 侧与 WS 侧互相挤掉对方的 token
		URL:    gw.URL,
		// intent 位不外提：全仓只订阅群与单聊消息这一位，改位数还得平台先开通、
		// 这边又要有对应的事件处理器，光在配置文件里换个数不会有任何效果。
		Intents: qq.IntentPublicMessages,
		Shard:   [2]uint32{0, uint32(max(gw.Shards, 1))},
		Logf:    func(format string, args ...any) { logf("[网关] "+format, args...) },
	})

	activeTargets := targetStore.Targets()
	// 各项参数与它的注释已在上面逐行打过（Dump），这里只报运行时才知道的两件事。
	logf("配置文件: %s｜主动消息 %d 个目标｜日历图: %s",
		*configPath, len(activeTargets),
		map[bool]string{true: cfg.Chrome, false: "未启用"}[poster != nil])
	if len(activeTargets) == 0 {
		logf("当前无主动推送目标（群管可在群内发 push on 开启），主动消息暂不发送")
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
	logf("收工。队列：投递 %d 条 / 发出 %d 条 / 合并 %d 次 / 过期丢弃 %d / 失败丢弃 %d / 溢出丢弃 %d",
		st.Submitted, st.Sent, st.Merged, st.DroppedExpired, st.DroppedFailed, st.DroppedOverflw)

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
// mediaProvider 是出图提供者的统一接口：根据键获取图片字节。"发出去才记账"的回执
// 由队列条目自带的 OnDelivered 承担（失败与丢弃一律不回调），这里不再参与记账。
type mediaProvider interface {
	Fetch(ctx context.Context, key string) ([]byte, error)
}

type activeSink struct {
	client *qq.Client
	poster mediaProvider
	bili   mediaProvider
}

func (s activeSink) Send(ctx context.Context, b *queue.Batch) error {
	openID, group, err := splitTarget(b.Target)
	if err != nil {
		return err
	}
	req := qq.SendRequest{MsgType: qq.MsgTypeText, Content: b.Text}
	var provider mediaProvider
	if b.Media != nil {
		switch b.Media.Kind {
		case "poster", "":
			if s.poster == nil {
				return fmt.Errorf("队列里有海报项但没接出图能力：检查配置文件里的 chrome")
			}
			provider = s.poster
		case "bili":
			if s.bili == nil {
				return fmt.Errorf("队列里有 B站动态项但没接出图能力：检查配置文件里的 chrome")
			}
			provider = s.bili
		default:
			return fmt.Errorf("未知的媒体类型: %s", b.Media.Kind)
		}

		raw, err := provider.Fetch(ctx, b.Media.Key)
		if err != nil {
			return fmt.Errorf("出 %s/%s 的图失败: %w", b.Media.Kind, b.Media.Key, err)
		}
		name := b.Media.Key + ".png"
		var ref qq.MediaRef
		if group {
			ref, err = s.client.UploadGroupImage(ctx, openID, name, raw)
		} else {
			ref, err = s.client.UploadC2CImage(ctx, openID, name, raw)
		}
		if err != nil {
			return fmt.Errorf("上传 %s 失败: %w", name, err)
		}
		req = qq.SendRequest{MsgType: qq.MsgTypeMedia, Media: &qq.MediaInfo{FileInfo: ref.FileInfo}}
	}

	if group {
		_, err = s.client.SendGroupMessage(ctx, openID, req)
	} else {
		_, err = s.client.SendC2CMessage(ctx, openID, req)
	}
	return err
}

// artDir 是海报落盘的位置：紧挨着数据库放，换 db 就换一套缓存，
// 不需要再多一个旋钮。
func artDir(dbPath string) string {
	if d := parentDir(dbPath); d != "" {
		return d + string(filepath.Separator) + "art"
	}
	return "art"
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
	AppID      string `json:"appId"`
	AppSecret  string `json:"clientSecret"`
	BiliCookie string `json:"biliCookie,omitempty"`
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

// initAndroidEnv 检查环境是否缺少 /etc/resolv.conf 与证书（常见于 Android / Termux 环境）。
//  1. Go 的 netgo 解析器在找不到 /etc/resolv.conf 时会默认尝试 127.0.0.1:53，导致 connection refused。
//     此处自动读取 Termux 的 resolv.conf 或回退到主流公共 DNS。
//  2. Termux 环境的根证书位于 $PREFIX/etc/tls/cert.pem，自动为其配置 SSL_CERT_FILE。
func initAndroidEnv() {
	if os.Getenv("SSL_CERT_FILE") == "" {
		termuxCert := "/data/data/com.termux/files/usr/etc/tls/cert.pem"
		if _, err := os.Stat(termuxCert); err == nil {
			os.Setenv("SSL_CERT_FILE", termuxCert)
		}
	}

	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		return
	}

	servers := []string{"223.5.5.5:53", "119.29.29.29:53"}
	termuxResolv := "/data/data/com.termux/files/usr/etc/resolv.conf"
	if data, err := os.ReadFile(termuxResolv); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					s := fields[1] + ":53"
					if s != "223.5.5.5:53" && s != "119.29.29.29:53" {
						servers = append(servers, s)
					}
				}
			}
		}
	}

	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			var lastErr error
			for _, s := range servers {
				conn, err := d.DialContext(ctx, "udp", s)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
}
