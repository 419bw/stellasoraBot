// Package qqsim 是本地 QQ 网关模拟器，用来在不出网、不打真平台的前提下
// 验证并压测我们自己的网关状态机与事件入口。
//
// 刻意不 import internal/qq：协议常量、字段名在这里按官方文档独立写一遍。
// 如果裁判和被测方共用同一组常量，我们把 op 码写错时端到端测试照样全绿，
// 那种绿没有意义。
package qqsim

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// 网关 op 码，取自官方「websocket 连接 / 事件与 op 码」表。
const (
	OpDispatch        = 0
	OpHeartbeat       = 1
	OpIdentify        = 2
	OpResume          = 6
	OpReconnect       = 7
	OpInvalidSession  = 9
	OpHello           = 10
	OpHeartbeatAck    = 11
	OpURLVerification = 13
)

// 关闭码。4006/4007/4009 官方明确不允许 resume。
const (
	CloseInvalidSession = 4006
	CloseBadSeq         = 4007
	CloseSessionTimeout = 4009
)

type Config struct {
	// Addr 监听地址，默认 127.0.0.1:0（随机端口，避免并行跑多个实例时抢端口）。
	Addr string

	// HeartbeatMS 是 hello 里下发的 heartbeat_interval，单位毫秒。
	// 默认 45000 与真平台同量级；压测时调小可以顺带压到心跳分支。
	HeartbeatMS int64

	// ReplayOnResume 打开后，resume 成功会把此前推过的事件原帧补发。
	// 这是真平台的行为（官方：断开之后短时间内重连会补发中间遗漏的事件），
	// 想去重层出问题就把这个开关关掉做一次对照。
	ReplayOnResume bool

	// MaxReplay 限制补发条数，默认不限制。
	// MaxReplay 限制补发（以及 history 保留）的条数。0 表示不限制。
	//
	// 长跑一定要给它一个上限：history 按每条 ~825B 累积，灌 5 万条就是 41MB，
	// 那会把「被测管道的内存」压成「模拟器自己的记账」，RSS 数字就没法看了。
	MaxReplay int

	// SuppressAck 收到心跳不回 op 11，用来触发客户端的假活判死。
	SuppressAck bool

	// SessionID 是 READY 下发的会话 id，默认随机。
	SessionID string

	Logf func(format string, args ...any)
}

// Stats 是服务端侧计数，用来独立核对客户端上报的数字（两边都数一遍才叫验证）。
type Stats struct {
	Connections int           // 接受过的 WS 连接数
	Sessions    int           // 处理过的 identify 数
	Resumes     int           // 处理过的 resume 数
	Heartbeats  int           // 收到并应答的心跳数
	Dispatched  int           // 下发过的事件帧数（含补发）
	Replayed    int           // 其中属于补发的帧数
	Seq         int64         // 当前事件序号
	Identifies  []IdentifyReq // 客户端 identify/resume 的原文
	DecodeErrs  []string      // identify 报文里类型对不上的字段，非空说明协议理解有分歧
}

type IdentifyReq struct {
	Op         int     `json:"op"`
	Token      string  `json:"token"`
	Intents    int64   `json:"intents"`
	Shard      [2]uint `json:"shard"`
	SessionID  string  `json:"session_id"`
	Seq        int64   `json:"seq"`
	RemoteAddr string  `json:"-"`
}

type frame struct {
	Op int             `json:"op"`
	S  int64           `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

type connEntry struct {
	id   int
	conn *websocket.Conn
}

type Server struct {
	cfg  Config
	ln   net.Listener
	http *http.Server

	mu      sync.Mutex
	conns   map[int]*connEntry
	nextID  int
	stats   Stats
	history [][]byte // 已下发帧的原文，用于 resume 补发
}

// New 起一个模拟器实例。用完必须 Close。
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if cfg.HeartbeatMS <= 0 {
		cfg.HeartbeatMS = 45000
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.SessionID == "" {
		cfg.SessionID = fmt.Sprintf("sim-%d", time.Now().UnixNano())
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("qqsim: 监听 %s 失败: %w", cfg.Addr, err)
	}
	s := &Server{
		cfg:   cfg,
		ln:    ln,
		conns: make(map[int]*connEntry),
	}
	s.http = &http.Server{Handler: http.HandlerFunc(s.route)}
	go func() { _ = s.http.Serve(ln) }()
	return s, nil
}

func (s *Server) BaseURL() string { return "http://" + s.ln.Addr().String() }

// WSURL 是给 /gateway 返回的网关地址，也是客户端该连的地址。
// 真平台返回的是 wss://…，这里同构，客户端不做特殊处理也能用。
func (s *Server) WSURL() string {
	return "ws://" + s.ln.Addr().String() + "/websocket"
}

func (s *Server) Close() error {
	s.mu.Lock()
	for _, c := range s.conns {
		c.conn.CloseNow()
	}
	s.conns = make(map[int]*connEntry)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.http.Shutdown(ctx)
}

func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stats
	out.Identifies = append([]IdentifyReq(nil), s.stats.Identifies...)
	return out
}

// route 提供真平台那三个 HTTP 接口，让客户端整条链路都能指到本地。
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/websocket":
		s.serveConn(w, r)
	case r.URL.Path == "/app/getAppAccessToken":
		writeJSON(w, map[string]any{"access_token": "sim-token", "expires_in": "7200"})
	case r.URL.Path == "/gateway" || r.URL.Path == "/gateway/bot":
		writeJSON(w, map[string]any{
			"url":    s.WSURL(),
			"shards": 1,
			"session_start_limit": map[string]any{
				"total": 20000, "remaining": 19990, "reset_after": 86400000,
			},
		})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serveConn(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.cfg.Logf("qqsim: 握手失败: %v", err)
		return
	}
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.conns[id] = &connEntry{id: id, conn: conn}
	s.stats.Connections++
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.conns, id)
		s.mu.Unlock()
		conn.CloseNow()
	}()

	ctx := r.Context()
	s.write(ctx, conn, frame{Op: OpHello, D: mustJSON(map[string]any{
		"heartbeat_interval": s.cfg.HeartbeatMS,
	})})

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Op {
		case OpIdentify:
			s.handleIdentify(ctx, conn, data)
		case OpResume:
			s.handleResume(ctx, conn, data)
		case OpHeartbeat:
			s.handleHeartbeat(ctx, conn)
		case OpReconnect:
			return
		}
	}
}

func (s *Server) handleIdentify(ctx context.Context, conn *websocket.Conn, raw []byte) {
	// intents 在真平台样例里是字符串（"16777216"），我们发的是数字。
	// 用 json.Number 两种都吃，类型不匹配时记进 DecodeErrs 而不是静默变 0 ——
	// 静默变 0 看起来就像「客户端没申请任何意图」，会把真问题藏起来。
	var req struct {
		D struct {
			Token   string      `json:"token"`
			Intents json.Number `json:"intents"`
			Shard   [2]uint     `json:"shard"`
		} `json:"d"`
	}
	err := json.Unmarshal(raw, &req)
	intents, ierr := req.D.Intents.Int64()

	s.mu.Lock()
	s.stats.Sessions++
	if err != nil {
		s.stats.DecodeErrs = append(s.stats.DecodeErrs, "identify: "+err.Error())
	}
	if ierr != nil {
		s.stats.DecodeErrs = append(s.stats.DecodeErrs, "identify.intents: "+ierr.Error())
	}
	s.stats.Identifies = append(s.stats.Identifies, IdentifyReq{
		Op: OpIdentify, Token: req.D.Token, Intents: intents, Shard: req.D.Shard,
	})
	s.mu.Unlock()

	s.dispatchTo(ctx, conn, "READY", mustJSON(map[string]any{
		"version":    1,
		"session_id": s.cfg.SessionID,
		"user":       map[string]any{"id": "sim-bot", "username": "模拟器", "bot": true},
	}))
}

func (s *Server) handleResume(ctx context.Context, conn *websocket.Conn, raw []byte) {
	var req struct {
		D struct {
			Token     string `json:"token"`
			SessionID string `json:"session_id"`
			Seq       int64  `json:"seq"`
		} `json:"d"`
	}
	_ = json.Unmarshal(raw, &req)

	s.mu.Lock()
	s.stats.Resumes++
	s.stats.Identifies = append(s.stats.Identifies, IdentifyReq{
		Op: OpResume, Token: req.D.Token, SessionID: req.D.SessionID, Seq: req.D.Seq,
	})
	replay := s.cfg.ReplayOnResume
	maxReplay := s.cfg.MaxReplay
	buf := make([][]byte, len(s.history))
	copy(buf, s.history) // 快照，写帧时不持锁
	s.mu.Unlock()

	if !replay {
		s.write(ctx, conn, frame{Op: OpDispatch, T: "RESUMED", S: s.nextSeq(), D: mustJSON("")})
		return
	}
	// 补发用原帧原 seq：官方语义是「把 last seq 之后漏掉的事件重放一遍」，
	// seq 不前进正是我们要验证客户端能扛住的情况。
	if maxReplay > 0 && len(buf) > maxReplay {
		buf = buf[len(buf)-maxReplay:]
	}
	for _, b := range buf {
		s.writeRaw(ctx, conn, b)
		s.mu.Lock()
		s.stats.Replayed++
		s.mu.Unlock()
	}
	s.write(ctx, conn, frame{Op: OpDispatch, T: "RESUMED", S: s.nextSeq(), D: mustJSON("")})
}

func (s *Server) handleHeartbeat(ctx context.Context, conn *websocket.Conn) {
	s.mu.Lock()
	s.stats.Heartbeats++
	suppress := s.cfg.SuppressAck
	s.mu.Unlock()
	if !suppress {
		s.write(ctx, conn, frame{Op: OpHeartbeatAck})
	}
}

func (s *Server) nextSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Seq++
	return s.stats.Seq
}

// Push 向所有在线连接广播一条事件，返回分配的 seq。
// 写是阻塞的：客户端读不过来时这里会慢下来，这正是压测要的背压。
func (s *Server) Push(t string, d json.RawMessage) (int64, error) {
	seq := s.nextSeq()
	b, err := json.Marshal(frame{Op: OpDispatch, S: seq, T: t, D: d})
	if err != nil {
		return 0, fmt.Errorf("qqsim: 序列化事件失败: %w", err)
	}
	s.mu.Lock()
	targets := make([]*connEntry, 0, len(s.conns))
	for _, c := range s.conns {
		targets = append(targets, c)
	}
	s.history = append(s.history, b)
	if s.cfg.MaxReplay > 0 && len(s.history) > s.cfg.MaxReplay {
		s.history = s.history[len(s.history)-s.cfg.MaxReplay:]
	}
	s.stats.Dispatched++
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range targets {
		if err := c.conn.Write(ctx, websocket.MessageText, b); err != nil {
			return seq, fmt.Errorf("qqsim: 写事件失败: %w", err)
		}
	}
	return seq, nil
}

// Reconnect 主动掐掉所有连接，制造一次「掉线后 resume」。
func (s *Server) Reconnect() int {
	s.mu.Lock()
	n := len(s.conns)
	for _, c := range s.conns {
		_ = c.conn.Close(websocket.StatusGoingAway, "sim drop")
	}
	s.mu.Unlock()
	return n
}

// SetSuppressAck 运行时改判死开关。
func (s *Server) SetSuppressAck(v bool) {
	s.mu.Lock()
	s.cfg.SuppressAck = v
	s.mu.Unlock()
}

// dispatchTo 下发一条服务端自己造的事件（READY / RESUMED 这类）。
func (s *Server) dispatchTo(ctx context.Context, conn *websocket.Conn, t string, d json.RawMessage) {
	s.write(ctx, conn, frame{Op: OpDispatch, S: s.nextSeq(), T: t, D: d})
	s.mu.Lock()
	s.stats.Dispatched++
	s.mu.Unlock()
}

func (s *Server) write(ctx context.Context, conn *websocket.Conn, f frame) {
	b, _ := json.Marshal(f)
	s.writeRaw(ctx, conn, b)
}

func (s *Server) writeRaw(ctx context.Context, conn *websocket.Conn, b []byte) {
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		s.cfg.Logf("qqsim: 写帧失败: %v", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("qqsim: 内部序列化失败: " + err.Error())
	}
	return b
}

// SplitHost 从 ws URL 里取出 host:port，日志用。
func SplitHost(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "ws://"), "wss://")
}
