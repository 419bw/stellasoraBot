package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// WS 链路独有的 op 码；0/1/11 三个是两条链路共用的，声明在 webhook.go。
const (
	OpIdentify       = 2
	OpResume         = 6
	OpReconnect      = 7
	OpInvalidSession = 9
	OpHello          = 10
)

// IntentPublicMessages 是群/单聊公域消息的 intent 位。
// 依据不是文档而是实测：他生产在跑的 Java 客户端只开这一位，
// GROUP_AT_MESSAGE_CREATE 与 GROUP_MESSAGE_CREATE 都收得到。
const IntentPublicMessages int64 = 1 << 25

// 官方明确"不允许 resume、必须重新 identify"的三个 close code。
// 注意 botgo 源码注释说 4009 允许 resume，与官方文档相左，这里按文档。
const (
	codeInvalidSession = 4006
	codeBadSeq         = 4007
	codeSessionExpired = 4009
)

var (
	// ErrGiveUp 表示重连预算用尽，交给上层决定是退避到小时级还是告警退出。
	ErrGiveUp = errors.New("qq: 网关重连预算用尽")

	errNeedIdentify = errors.New("qq: 会话失效，需重新 identify")
	errDeadLink     = errors.New("qq: 心跳长时间无应答，判定链路假活")
)

// Event 是一条已投递的网关事件。Data 保持原始 JSON，由各功能自己按需解析，
// 免得网关层替全平台的事件都建一遍结构体。
type Event struct {
	Seq  int64
	Type string
	Data json.RawMessage
}

// GatewayInfo 是 GET /gateway/bot 的响应。
type GatewayInfo struct {
	URL     string `json:"url"`
	Shards  int    `json:"shards"`
	Session struct {
		Total      int   `json:"total"`
		Remaining  int   `json:"remaining"`
		ResetAfter int64 `json:"reset_after"` // 毫秒
	} `json:"session_start_limit"`
}

// Gateway 取网关地址与建联配额。
func (c *Client) Gateway(ctx context.Context) (GatewayInfo, error) {
	var g GatewayInfo
	if err := c.do(ctx, "获取网关地址", http.MethodGet, "/gateway/bot", nil, &g); err != nil {
		return GatewayInfo{}, err
	}
	// 与 Me() 同理：HTTP 200 但字段对不上会解出空结构体，必须显式判空
	if g.URL == "" {
		return GatewayInfo{}, fmt.Errorf("qq: 网关响应缺少 url 字段，响应结构可能已变更")
	}
	return g, nil
}

// 两段退避。identify 层的数值来自他 Java 版实测过的 5s*2^n 封顶 60s 序列，
// 去掉首次的 5s（那一次是握手成功但秒被踢时最伤配额的间隔）。
var (
	DefaultResumeBackoff   = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	DefaultIdentifyBackoff = []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}
)

// GatewayConfig 网关连接参数。
type GatewayConfig struct {
	Tokens  *TokenSource
	URL     string // 来自 Gateway()，形如 wss://api.bot.qq.com/websocket
	Intents int64
	Shard   [2]uint32

	// AckMisses 是连续几个心跳周期没收到 op 11 就判定链路假活。默认 2。
	// 手机休眠后 NAT 表项过期，socket 仍是 ESTABLISHED，读不会报错，
	// 只靠"读失败重连"会永远卡住 —— 这条判死是唯一的兜底。
	AckMisses int
	// MinHealthy 连接至少存活这么久才算成功，才清零退避计数。默认 60s。
	// 不设这条，"连上 1 秒就被踢"会让计数每次都归零，退避形同虚设。
	MinHealthy time.Duration

	ResumeBackoff   []time.Duration
	IdentifyBackoff []time.Duration

	Logf func(format string, args ...any)
	Now  func() time.Time
}

// Gateway 是 QQ 网关长连接的状态机。
//
// 所有协议决策都在 Run 的单线程循环里做，读帧协程只负责把字节搬到 channel，
// 因此不存在需要加锁的共享状态 —— sessionID/seq 只有循环体读写。
type Gateway struct {
	cfg GatewayConfig

	sessionID string
	seq       int64

	resumeFailures   int
	identifyFailures int
}

// NewGateway 建网关连接。cfg.URL 与 cfg.Tokens 必填。
func NewGateway(cfg GatewayConfig) *Gateway {
	if cfg.AckMisses <= 0 {
		cfg.AckMisses = 2
	}
	if cfg.MinHealthy <= 0 {
		cfg.MinHealthy = time.Minute
	}
	// 零值 Shard 是 [0,0]，意思是「第 0 片、共 0 片」——自相矛盾，平台侧行为未知，
	// 不该靠调用方每次都记得填。兜成单分片，但调用方仍应从 /gateway/bot 的 shards 显式传。
	if cfg.Shard[1] == 0 {
		cfg.Shard = [2]uint32{0, 1}
	}
	if cfg.ResumeBackoff == nil {
		cfg.ResumeBackoff = DefaultResumeBackoff
	}
	if cfg.IdentifyBackoff == nil {
		cfg.IdentifyBackoff = DefaultIdentifyBackoff
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Gateway{cfg: cfg}
}

// Run 维持长连接并把事件交给 onEvent，阻塞到 ctx 结束或重连预算用尽。
// onEvent 在循环线程里同步调用：它会拖慢心跳，所以耗时活儿请自己起 goroutine。
func (g *Gateway) Run(ctx context.Context, onEvent func(context.Context, Event) error) error {
	for {
		started := g.cfg.Now()
		err := g.attempt(ctx, onEvent)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// 只有活够久才算"这次连接是健康的"，否则计数原地归零等于没有退避
		if g.cfg.Now().Sub(started) >= g.cfg.MinHealthy {
			g.resumeFailures, g.identifyFailures = 0, 0
		}

		wait, ok := g.nextDelay()
		if !ok {
			g.cfg.Logf("网关重连预算用尽，最后一次错误：%v", err)
			return ErrGiveUp
		}
		g.cfg.Logf("网关断开（%v），%s 后重连（resume=%v）", err, wait, g.canResume())

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// nextDelay 按"能不能 resume"选退避档位。resume 连续失败到达上限后
// 主动丢弃会话降级到 identify —— 死磕 resume 只会一直白等。
func (g *Gateway) nextDelay() (time.Duration, bool) {
	if g.canResume() {
		if g.resumeFailures < len(g.cfg.ResumeBackoff) {
			d := g.cfg.ResumeBackoff[g.resumeFailures]
			g.resumeFailures++
			return d, true
		}
		g.cfg.Logf("resume 连续 %d 次未成功，丢弃会话改走 identify", g.resumeFailures)
		g.dropSession()
		g.resumeFailures = 0
	}
	if g.identifyFailures >= len(g.cfg.IdentifyBackoff) {
		return 0, false
	}
	d := g.cfg.IdentifyBackoff[g.identifyFailures]
	g.identifyFailures++
	return d, true
}

func (g *Gateway) canResume() bool { return g.sessionID != "" && g.seq > 0 }

func (g *Gateway) dropSession() {
	g.sessionID, g.seq = "", 0
}

// attempt 建一条连接并跑到它断开为止。
func (g *Gateway) attempt(ctx context.Context, onEvent func(context.Context, Event) error) error {
	conn, resp, err := websocket.Dial(ctx, g.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("qq: 连接网关 %s 失败: %w", g.cfg.URL, err)
	}
	// 101 切换成功后 body 已被 hijack，resp.Body 是 nil —— 只判 resp 非空会当场 panic
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)

	type inbound struct {
		data []byte
		err  error
	}
	frames := make(chan inbound, 64)
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	go func() {
		for {
			_, data, err := conn.Read(readCtx)
			select {
			case frames <- inbound{data: data, err: err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var (
		hb      time.Duration
		lastAck = g.cfg.Now()
	)
	hbTick := time.NewTicker(time.Hour)
	defer hbTick.Stop()
	// 判死频率跟心跳周期一致：检查的是"距上次 ack 过了几个周期"，
	// 固定 1s 一跳会让判死延迟和 heartbeat_interval 脱节。
	watch := time.NewTicker(time.Hour)
	defer watch.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case f := <-frames:
			if f.err != nil {
				return classifyReadError(f.err, g)
			}
			var fr struct {
				Op int             `json:"op"`
				S  int64           `json:"s"`
				T  string          `json:"t"`
				D  json.RawMessage `json:"d"`
			}
			if err := json.Unmarshal(f.data, &fr); err != nil {
				g.cfg.Logf("网关报文解析失败，丢弃： %v", err)
				continue
			}
			switch fr.Op {
			case OpHello:
				var hd struct {
					HeartbeatInterval int64 `json:"heartbeat_interval"`
				}
				if err := json.Unmarshal(fr.D, &hd); err != nil || hd.HeartbeatInterval <= 0 {
					return fmt.Errorf("qq: hello 里没有可用的 heartbeat_interval: %w", err)
				}
				// 官方单位是毫秒。别用整除转秒 —— 41250ms 会变成 41s 并逐次漂移。
				hb = time.Duration(hd.HeartbeatInterval) * time.Millisecond
				hbTick.Reset(hb)
				watch.Reset(hb)
				lastAck = g.cfg.Now()
				if err := g.handshake(ctx, conn); err != nil {
					return err
				}
			case OpHeartbeatAck:
				lastAck = g.cfg.Now()
			case OpDispatch:
				if fr.S > g.seq {
					g.seq = fr.S
				}
				switch fr.T {
				case "READY":
					var rd struct {
						SessionID string `json:"session_id"`
					}
					if err := json.Unmarshal(fr.D, &rd); err != nil {
						return fmt.Errorf("qq: READY 解析失败: %w", err)
					}
					if rd.SessionID == "" {
						return errors.New("qq: READY 缺少 session_id，响应结构可能已变更")
					}
					g.sessionID = rd.SessionID
					g.cfg.Logf("网关已就绪，session=%s…", rd.SessionID[:min(8, len(rd.SessionID))])
				case "RESUMED":
					g.cfg.Logf("会话恢复完成，补发事件已收齐（seq=%d）", g.seq)
				default:
					if err := onEvent(ctx, Event{Seq: fr.S, Type: fr.T, Data: fr.D}); err != nil {
						return fmt.Errorf("qq: 事件 %s 处理失败: %w", fr.T, err)
					}
				}
			case OpReconnect:
				// 平台要求换连接，但会话仍可 resume
				conn.Close(websocket.StatusGoingAway, "server requested reconnect")
				return errors.New("qq: 平台要求重连")
			case OpInvalidSession:
				g.dropSession()
				g.cfg.Tokens.Invalidate()
				return errNeedIdentify
			default:
				g.cfg.Logf("忽略未知网关 op=%d", fr.Op)
			}

		case <-hbTick.C:
			if hb == 0 {
				continue // 还没收到 hello，不该发心跳
			}
			if err := g.write(ctx, conn, OpHeartbeat, g.seq); err != nil {
				return err
			}
		case <-watch.C:
			if hb > 0 && g.cfg.Now().Sub(lastAck) > time.Duration(g.cfg.AckMisses)*hb {
				conn.CloseNow()
				return errDeadLink
			}
		}
	}
}

// handshake 按当前会话状态决定 resume 还是 identify。
func (g *Gateway) handshake(ctx context.Context, conn *websocket.Conn) error {
	token, err := g.cfg.Tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("qq: 建联取 token 失败: %w", err)
	}
	if g.canResume() {
		g.cfg.Logf("尝试 resume：session=%s seq=%d", g.sessionID, g.seq)
		return g.write(ctx, conn, OpResume, map[string]any{
			"token":      "QQBot " + token,
			"session_id": g.sessionID,
			"seq":        g.seq,
		})
	}
	return g.write(ctx, conn, OpIdentify, map[string]any{
		"token":   "QQBot " + token,
		"intents": g.cfg.Intents,
		"shard":   []uint32{g.cfg.Shard[0], g.cfg.Shard[1]},
	})
}

func (g *Gateway) write(ctx context.Context, conn *websocket.Conn, op int, d any) error {
	payload, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return fmt.Errorf("qq: 编码网关报文失败: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("qq: 发送 op=%d 失败: %w", op, err)
	}
	return nil
}

// classifyReadError 把库返回的关闭错误翻译成"下一轮还能不能 resume"。
func classifyReadError(err error, g *Gateway) error {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch int(ce.Code) {
		case codeInvalidSession, codeBadSeq, codeSessionExpired:
			g.dropSession()
			return fmt.Errorf("qq: 网关关闭 %d(%s)，不允许 resume: %w", ce.Code, ce.Reason, errNeedIdentify)
		}
		return fmt.Errorf("qq: 网关关闭 %d %s", ce.Code, ce.Reason)
	}
	return fmt.Errorf("qq: 读网关失败: %w", err)
}
