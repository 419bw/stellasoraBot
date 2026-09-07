package qq

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 事件类型。当前只用到前两个，其余按 schema 定义好以便识别后丢弃。
const (
	EventGroupAtMessage = "GROUP_AT_MESSAGE_CREATE"
	EventGroupMessage   = "GROUP_MESSAGE_CREATE"
	EventC2CMessage     = "C2C_MESSAGE_CREATE"
	EventResumed        = "RESUMED"
	EventReady          = "READY"
)

// 消息内容类型，取自官方 Message/MsgElement 的 message_type 枚举。
const (
	MessageTypeText     = 0   // 普通文本
	MessageTypeArk      = 3   // 结构化卡片，数据在 ArkData
	MessageTypeParallel = 101 // 并行消息
	MessageTypeHistory  = 102 // 聊天记录（合并转发）
	MessageTypeQuoted   = 103 // 引用消息，原消息嵌在 Elements 里
)

// Author 是官方 User 结构。群聊填 MemberOpenID，单聊填 UserOpenID，
// 两者在本项目观测到的样本里同值，但不要依赖这一点。
type Author struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Bot          bool   `json:"bot"`
	UnionOpenID  string `json:"union_openid,omitempty"`
	UnionAccount string `json:"union_user_account,omitempty"`
	UserOpenID   string `json:"user_openid,omitempty"`
	MemberOpenID string `json:"member_openid,omitempty"`

	// MemberRole 取值 member / admin / owner，官方 schema 有、单聊事件里没有。
	MemberRole string `json:"member_role,omitempty"`
}

// OpenID 返回这个作者在当前场景下的稳定标识。
func (a Author) OpenID() string {
	switch {
	case a.MemberOpenID != "":
		return a.MemberOpenID
	case a.UserOpenID != "":
		return a.UserOpenID
	default:
		return a.ID
	}
}

// MessageScene 是消息场景上下文。Ext 是 key=value 字符串列表，
// 官方列了三种键：msg_idx（引用用消息索引）、ref_msg_idx（被引用消息索引）、
// auth_token（鉴权令牌）。群事件里带 auth_token，单聊样本里只带 msg_idx。
type MessageScene struct {
	Source string   `json:"source,omitempty"`
	Ext    []string `json:"ext,omitempty"`
}

// ExtValue 按 key 取 message_scene.ext 里的值，没有则返回空串。
func (s MessageScene) ExtValue(key string) string {
	prefix := key + "="
	for _, e := range s.Ext {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):]
		}
	}
	return ""
}

// Attachment 消息附件。语音消息平台已经给了转码后的 WAV 地址和 ASR 参考文本。
type Attachment struct {
	URL          string `json:"url,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	Size         int    `json:"size,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	VoiceWAVURL  string `json:"voice_wav_url,omitempty"`
	ASRReferText string `json:"asr_refer_text,omitempty"`
}

// ArkData 结构化卡片消息。
//
// 重要：群里分享 B站/微信小程序等内容时 Content 是空的，标题与跳转链接
// 全在 Fields 里。只看 Content 的功能会一条都抓不到。
type ArkData struct {
	Prompt  string          `json:"prompt,omitempty"`
	ArkType string          `json:"ark_type,omitempty"` // tuwen/feed/miniapp/map/contact_card/video_share/music_together
	ArkName string          `json:"ark_name,omitempty"`
	Fields  json.RawMessage `json:"fields,omitempty"`
}

// FieldString 从 fields 里取一个字符串字段（title / desc / jump_url / source 等）。
func (a *ArkData) FieldString(key string) string {
	if a == nil || len(a.Fields) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(a.Fields, &m); err != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// MsgElement 消息元素。官方定义里 Elements 会递归嵌套（引用消息、聊天记录），
// 所以遍历必须带深度上限，否则畸形报文能把解析栈打爆。
type MsgElement struct {
	MsgIdx      string       `json:"msg_idx,omitempty"`
	Author      Author       `json:"author,omitempty"`
	MessageType int          `json:"message_type,omitempty"`
	Content     string       `json:"content,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	ArkData     *ArkData     `json:"ark_data,omitempty"`
	Elements    []MsgElement `json:"msg_elements,omitempty"`
}

// Message 是一条入站消息，字段集合取「官方 autogen schema ∪ 真连样本」。
//
// GroupID 是个例外：真实报文里有、官方 schema 和示例里都没有，且与
// GroupOpenID 同值（n=1）。当附带信息看待，不要依赖它。
type Message struct {
	Kind        string       `json:"-"`
	ID          string       `json:"id"`
	GroupOpenID string       `json:"group_openid,omitempty"`
	GroupID     string       `json:"group_id,omitempty"`
	Author      Author       `json:"author"`
	Content     string       `json:"content"`
	Timestamp   string       `json:"timestamp"`
	MessageType int          `json:"message_type"`
	Scene       MessageScene `json:"message_scene"`

	Attachments []Attachment `json:"attachments,omitempty"`
	Mentions    []Author     `json:"mentions,omitempty"`
	ArkData     *ArkData     `json:"ark_data,omitempty"`
	Elements    []MsgElement `json:"msg_elements,omitempty"`
}

// DecodeMessage 解析一条消息类事件。kind 传事件类型名，用于后续去重与路由。
func DecodeMessage(kind string, raw json.RawMessage) (*Message, error) {
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("qq: 解析 %s 报文失败: %w", kind, err)
	}
	if m.ID == "" {
		return nil, fmt.Errorf("qq: %s 报文缺少 id 字段，响应结构可能已变更", kind)
	}
	m.Kind = kind
	return &m, nil
}

// Text 返回去掉首尾空白的正文。
//
// 平台剥掉 @机器人 之后会留下空格，官方示例里 content 就是 " /今日天气 "，
// 不 Trim 会导致命令匹配不上。
func (m *Message) Text() string { return strings.TrimSpace(m.Content) }

// IsGroup 判断这条消息来自群还是单聊。
func (m *Message) IsGroup() bool {
	return m.Kind == EventGroupAtMessage || m.Kind == EventGroupMessage || m.GroupOpenID != ""
}

// ReplyTarget 返回被动回复该往哪个 openid 发。
func (m *Message) ReplyTarget() (target string, isGroup bool) {
	if m.IsGroup() {
		return m.GroupOpenID, true
	}
	return m.Author.OpenID(), false
}

// SenderOpenID 返回说话人的 openid。注意单聊事件的 Username 是空串，
// 昵称只有群事件才有，展示时不能指望它。
func (m *Message) SenderOpenID() string { return m.Author.OpenID() }

// MemberRoleIs 判断说话人在群里的角色，用于管理员权限门槛。
func (m *Message) MemberRoleIs(role string) bool { return m.Author.MemberRole == role }

// CreatedAt 解析消息时间戳（RFC3339，秒级精度）。
func (m *Message) CreatedAt() (time.Time, error) {
	t, err := time.Parse(time.RFC3339, m.Timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("qq: timestamp %q 不是 RFC3339: %w", m.Timestamp, err)
	}
	return t, nil
}

// Display 返回一个适合日志与回复里用的昵称：群事件有昵称，单聊没有，
// 缺失时退回 openid 前 8 位而不是显示空串。
func (m *Message) Display() string {
	if m.Author.Username != "" {
		return m.Author.Username
	}
	return head(m.SenderOpenID(), 8)
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// WalkElements 深度优先遍历递归的 msg_elements，带深度上限防畸形报文。
func (m *Message) WalkElements(visit func(MsgElement) bool) {
	walk(m.Elements, visit, 0)
}

const maxElementDepth = 8

func walk(list []MsgElement, visit func(MsgElement) bool, depth int) {
	if depth > maxElementDepth {
		return
	}
	for _, e := range list {
		if !visit(e) {
			return
		}
		walk(e.Elements, visit, depth+1)
	}
}
