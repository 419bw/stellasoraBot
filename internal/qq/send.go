package qq

import (
	"errors"
	"fmt"
)

// 发送请求的 msg_type 取值，来自「消息收发概述」。
//
// 注意与入站事件的 message_type 是两套枚举：这里描述「我要发什么格式」，
// 取值 0/2/7；事件里的 message_type 描述「平台推来的是什么」，取值 0/3/101/102/103。
// 两者只有 0 重合，不要混用同一组常量。
const (
	MsgTypeText     = 0
	MsgTypeMarkdown = 2
	MsgTypeMedia    = 7
)

// SendResult 是发送消息接口的响应体，平台只回这两个字段。
type SendResult struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
}

type Markdown struct {
	Content string `json:"content,omitempty"`
	// 开启后图片转存失败会中断发送并返回失败，默认 false 保持平台原有行为。
	ForceVerifyImageResource bool `json:"force_verify_image_resource,omitempty"`
}

type MediaInfo struct {
	FileInfo string `json:"file_info,omitempty"`
}

type MessageReference struct {
	MessageID string `json:"message_id,omitempty"`
}

type SendRequest struct {
	MsgType   int               `json:"msg_type"`
	Content   string            `json:"content,omitempty"`
	Markdown  *Markdown         `json:"markdown,omitempty"`
	Media     *MediaInfo        `json:"media,omitempty"`
	MsgID     string            `json:"msg_id,omitempty"`
	EventID   string            `json:"event_id,omitempty"`
	MsgSeq    int               `json:"msg_seq,omitempty"`
	Reference *MessageReference `json:"message_reference,omitempty"`
}

// validate 挡住平台明确互斥的字段组合：这些请求发出去只会失败，没必要占一次频控额度。
func (r *SendRequest) validate() error {
	switch r.MsgType {
	case MsgTypeText:
		if r.Markdown != nil || r.Media != nil {
			return errors.New("qq: msg_type=0 时不能携带 markdown/media")
		}
		if r.Content == "" {
			return errors.New("qq: msg_type=0 需要非空 content")
		}
	case MsgTypeMarkdown:
		if r.Content != "" {
			return errors.New("qq: 传了 markdown 之后 content 必须为空")
		}
		if r.Markdown == nil || r.Markdown.Content == "" {
			return errors.New("qq: msg_type=2 需要 markdown.content")
		}
		if r.Media != nil {
			return errors.New("qq: markdown 与 media 互斥")
		}
	case MsgTypeMedia:
		if r.Content != "" || r.Markdown != nil {
			return errors.New("qq: msg_type=7 时 content/markdown 必须为空")
		}
		if r.Media == nil || r.Media.FileInfo == "" {
			return errors.New("qq: msg_type=7 需要 media.file_info")
		}
	default:
		return fmt.Errorf("qq: 不支持的 msg_type %d", r.MsgType)
	}
	if r.MsgID != "" && r.EventID != "" {
		return errors.New("qq: msg_id 与 event_id 二选一")
	}
	return nil
}
