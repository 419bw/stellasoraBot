package aichat

import (
	"sync"
	"time"

	"xingta/internal/qq"
)

// Message 是一条进出模型的会话记录。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// session 是单个上下文窗口（按群或按单聊隔离）。
type session struct {
	lastActive time.Time
	history    []Message // 长度至多 maxRounds * 2
}

// sessionManager 负责本地上下文的内存维护、滑动窗口裁剪与过期回收。
type sessionManager struct {
	mu        sync.Mutex
	ttl       time.Duration
	maxRounds int
	sessions  map[string]*session
}

func newSessionManager(ttl time.Duration, maxRounds int) *sessionManager {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if maxRounds <= 0 {
		maxRounds = 4
	}
	return &sessionManager{
		ttl:       ttl,
		maxRounds: maxRounds,
		sessions:  make(map[string]*session),
	}
}

// Get 返回指定会话在 now 时刻未过期的历史消息切片副本。
func (sm *sessionManager) Get(key string, now time.Time) []Message {
	if sm == nil {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	s, ok := sm.sessions[key]
	if !ok {
		return nil
	}
	if now.Sub(s.lastActive) > sm.ttl {
		delete(sm.sessions, key)
		return nil
	}
	s.lastActive = now
	res := make([]Message, len(s.history))
	copy(res, s.history)
	return res
}

// Append 在成功回复后将这一对 (user, assistant) 追加进会话并淘汰超限的最早轮次。
func (sm *sessionManager) Append(key string, userText, assistantText string, now time.Time) {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	s, ok := sm.sessions[key]
	if !ok {
		s = &session{}
		sm.sessions[key] = s
	}
	s.lastActive = now
	s.history = append(s.history,
		Message{Role: "user", Content: userText},
		Message{Role: "assistant", Content: assistantText},
	)

	// 滑动窗口：超过最大轮数时成对淘汰最早的轮次
	maxMsgs := sm.maxRounds * 2
	if len(s.history) > maxMsgs {
		s.history = s.history[len(s.history)-maxMsgs:]
	}
}

// sessionKeyOf 提取会话隔离的 Key：
//   - 群聊：按群共享上下文（"group:" + groupOpenID），群内所有成员在同一语境下连续追问；
//   - 单聊：按私聊用户隔离（"c2c:" + senderOpenID）。
func sessionKeyOf(m *qq.Message) string {
	target, isGroup := m.ReplyTarget()
	if isGroup {
		return "group:" + target
	}
	return "c2c:" + target
}
