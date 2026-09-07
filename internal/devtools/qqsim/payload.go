package qqsim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// 事件类型，同样按文档独立定义，理由见包注释。
const (
	EventGroupAtMessage = "GROUP_AT_MESSAGE_CREATE"
	EventGroupMessage   = "GROUP_MESSAGE_CREATE"
	EventC2CMessage     = "C2C_MESSAGE_CREATE"
)

// Sender 是一个模拟用户。openid 用可辨识的十六进制串，
// 出问题时从日志里能直接认出是第几个模拟用户。
type Sender struct {
	OpenID string
	Nick   string
	Role   string
}

// Generator 按真实报文形状生成消息事件。
//
// 报文形态来自真连采集样本（internal/qq/testdata），不是凭空写的：
// 群事件带 group_openid / member_role / auth_token，单聊不带。
// 压测数据的形状如果不真，压出来的数就没有参考价值。
type Generator struct {
	GroupOpenID string
	Senders     []Sender
	TextPool    []string

	// ext 值与 channel id 预生成：每条都 strings.Repeat 一次，
	// 发送端会先于被测端成为瓶颈，压出来的吞吐就不准了。
	extMsgIdx  string
	extAuthTok string
	channelID  string

	n atomic.Uint64
}

// hexID 生成 32 位大写十六进制 openid，与真平台 openid 的形态一致。
func hexID(v uint64) string { return fmt.Sprintf("%032X", v) }

// DefaultGenerator 造一个贴近观测样本的生成器：40 个群成员轮流说话。
func DefaultGenerator() *Generator {
	senders := make([]Sender, 40)
	for i := range senders {
		id := hexID(uint64(0xA000000000000000) + uint64(i))
		senders[i] = Sender{
			OpenID: id,
			Nick:   fmt.Sprintf("模拟用户%02d", i),
			Role:   "member",
		}
	}
	texts := []string{
		" 今天几点开", " /活动日历 ", " 求助 这个boss怎么打", " 有人一起吗",
		" 刚那个公告看了吗", " 新版本什么时候更", " 这个礼包值得买吗", " 大佬带带我",
	}
	return &Generator{
		GroupOpenID: hexID(0xB000000000000000),
		Senders:     senders,
		TextPool:    texts,
		extMsgIdx:   "msg_idx=" + strings.Repeat("A", 84),
		extAuthTok:  "auth_token=" + strings.Repeat("b", 124),
		channelID:   hexID(0xC000000000000000),
	}
}

// 消息 id 里刻意编进「服务端发出时刻 + 计数」：载荷字段会被解码器按结构体丢弃，
// 而 id 一定要原样到达业务处理器，所以拿它当延迟打点的载体最稳。
// ParseSent 负责在读侧还原。
// 时间戳必须是下划线之前的纯数字段，ProbeSent/ParseSent 就是按这个约定切第一段。
// 前缀字母放在第二段：既保住「群/单聊可分辨」，又不会让数字解析当场失败。
func formatSimID(kind string, seq uint64) string {
	return fmt.Sprintf("ROBOT1.0_%d_%s%08d", time.Now().UnixNano(), kind, seq)
}

const idEnvelope = "ROBOT1.0_"

// ParseSent 还原模拟报文里的发出时刻；不是模拟器格式的 id 返回 ok=false。
// 真平台报文里下划线之后是纯十六进制、没有第二个下划线，因此不会被误判成模拟格式。
func ParseSent(id string) (time.Time, bool) {
	rest := strings.TrimPrefix(id, idEnvelope)
	if rest == id {
		return time.Time{}, false
	}
	i := strings.IndexByte(rest, '_')
	if i <= 0 {
		return time.Time{}, false
	}
	ns, ok := digits([]byte(rest[:i]))
	if !ok || ns <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// ProbeSent 直接从事件原文取发出时刻，用于在解码之前打点。
// 只在 id 是报文第一个键时生效，否则立刻放弃 —— 打点本身不能成为被测开销。
func ProbeSent(raw []byte) (time.Time, bool) {
	const p = `{"id":"` + idEnvelope
	if len(raw) <= len(p) || string(raw[:len(p)]) != p {
		return time.Time{}, false
	}
	rest := raw[len(p):]
	i := bytes.IndexByte(rest, '_')
	if i <= 0 {
		return time.Time{}, false
	}
	ns, ok := digits(rest[:i])
	if !ok || ns <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// digits 手写十进制解析：strconv.ParseInt 需要 []byte→string 转换，
// 每帧一次分配会混进延迟样本里。
func digits(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 19 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// NextGroupAt 生成一条群 @ 消息事件。
//
// 手工按序拼 JSON 而不是 map[string]any：Go 序列化 map 会把键按字母排序，
// id 会落到报文中间，读侧想取发出时刻就得全文扫描 —— 那是测量仪自己的开销。
// 让 id 固定当第一个键，打点就是 O(1) 前缀判断。
func (g *Generator) NextGroupAt() (t string, raw json.RawMessage, id string) {
	n := g.n.Add(1)
	sn := g.Senders[int(n%uint64(len(g.Senders)))]
	txt := g.TextPool[int(n%uint64(len(g.TextPool)))]

	id = formatSimID("G", n)
	raw = json.RawMessage(fmt.Sprintf(
		`{"id":%q,"channel_id":%q,"group_openid":%q,"group_id":%q,"content":%q,`+
			`"timestamp":%q,"message_type":0,"author":{"id":%q,"member_openid":%q,"member_role":%q,`+
			`"username":%q,"bot":false,"union_openid":""},`+
			`"message_scene":{"source":"default","ext":["msg_idx=%s","auth_token=%s"]},`+
			`"mentions":[{"id":"ROBOT1.0_BOT","username":"机器人","bot":true}]}`,
		id, g.channelID, g.GroupOpenID, g.GroupOpenID, txt,
		time.Now().Format(time.RFC3339),
		sn.OpenID, sn.OpenID, sn.Role, sn.Nick,
		g.extMsgIdx, g.extAuthTok,
	))
	return EventGroupAtMessage, raw, id
}

// NextC2C 生成一条单聊消息事件，形状与真连单聊样本一致（无群字段、无昵称）。
func (g *Generator) NextC2C() (t string, raw json.RawMessage, id string) {
	n := g.n.Add(1)
	s := g.Senders[int(n%uint64(len(g.Senders)))]
	id = formatSimID("C", n)
	m := map[string]any{
		"id":           id,
		"content":      strings.TrimSpace(g.TextPool[int(n%uint64(len(g.TextPool)))]),
		"timestamp":    time.Now().Format(time.RFC3339),
		"message_type": 0,
		"author": map[string]any{
			"id":           s.OpenID,
			"user_openid":  s.OpenID,
			"username":     "",
			"bot":          false,
			"union_openid": "",
		},
		"message_scene": map[string]any{
			"source": "default",
			"ext":    []string{"msg_idx=" + strings.Repeat("A", 84)},
		},
	}
	return EventC2CMessage, mustJSON(m), id
}

// SampleMessageJSON 返回一条固定报文，给单元测试当合成输入。
// 真实样本请用 internal/qq/testdata 里那两个文件。
func SampleMessageJSON(id, content string) json.RawMessage {
	return mustJSON(map[string]any{
		"id":           id,
		"content":      content,
		"timestamp":    "2026-09-04T12:00:00+08:00",
		"message_type": 0,
		"group_openid": hexID(0xB000000000000000),
		"author": map[string]any{
			"id": "00000000000000000000000000000001", "member_openid": "00000000000000000000000000000001",
			"member_role": "member", "username": "模拟用户", "bot": false,
		},
		"message_scene": map[string]any{"source": "default", "ext": []string{"msg_idx=x"}},
	})
}
