package aichat

import (
	"sync"
	"testing"
	"time"

	"xingta/internal/qq"
)

func TestSessionGroupSharing(t *testing.T) {
	sm := newSessionManager(10*time.Minute, 4)
	now := time.Now()

	msgA := &qq.Message{
		Kind:        qq.EventGroupAtMessage,
		GroupOpenID: "GROUP_100",
		Author:      qq.Author{MemberOpenID: "USER_A", Username: "张三"},
	}
	msgB := &qq.Message{
		Kind:        qq.EventGroupAtMessage,
		GroupOpenID: "GROUP_100",
		Author:      qq.Author{MemberOpenID: "USER_B", Username: "李四"},
	}

	keyA := sessionKeyOf(msgA)
	keyB := sessionKeyOf(msgB)
	if keyA != keyB {
		t.Fatalf("群聊 Key 应相同: got keyA=%q, keyB=%q", keyA, keyB)
	}

	// 用户 A 提问并记录回答
	sm.Append(keyA, "今天有什么活动？", "有星塔探险哦！", now)

	// 用户 B 此时读取群上下文，应该能看到 A 的对话
	histB := sm.Get(keyB, now.Add(5*time.Second))
	if len(histB) != 2 {
		t.Fatalf("用户 B 应看到群内 1 轮（2 条）历史，got %d", len(histB))
	}
	if histB[0].Content != "今天有什么活动？" || histB[1].Content != "有星塔探险哦！" {
		t.Errorf("群历史内容不匹配: %+v", histB)
	}
}

func TestSessionC2CIsolation(t *testing.T) {
	sm := newSessionManager(10*time.Minute, 4)
	now := time.Now()

	msgA := &qq.Message{
		Kind:   qq.EventC2CMessage,
		Author: qq.Author{UserOpenID: "USER_A"},
	}
	msgB := &qq.Message{
		Kind:   qq.EventC2CMessage,
		Author: qq.Author{UserOpenID: "USER_B"},
	}

	keyA := sessionKeyOf(msgA)
	keyB := sessionKeyOf(msgB)
	if keyA == keyB {
		t.Fatalf("单聊 Key 应不同: keyA=%q, keyB=%q", keyA, keyB)
	}

	sm.Append(keyA, "私聊问个问题", "私聊回答", now)

	// 用户 B 读取自己的私聊，应为空
	histB := sm.Get(keyB, now.Add(time.Second))
	if len(histB) != 0 {
		t.Fatalf("用户 B 的单聊上下文应为空，got %d", len(histB))
	}

	// 用户 A 读取自己的私聊，能看到
	histA := sm.Get(keyA, now.Add(time.Second))
	if len(histA) != 2 {
		t.Fatalf("用户 A 应看到 2 条历史，got %d", len(histA))
	}
}

func TestSessionTTL(t *testing.T) {
	sm := newSessionManager(10*time.Minute, 4)
	now := time.Now()
	key := "test_ttl"

	sm.Append(key, "你好", "在的在的", now)

	// 9 分钟后仍有效
	if hist := sm.Get(key, now.Add(9*time.Minute)); len(hist) != 2 {
		t.Fatalf("9 分钟后未超时，应有 2 条记录，got %d", len(hist))
	}

	// 距离上次活跃（9分钟时刻）又过了 11 分钟 -> 超时
	if hist := sm.Get(key, now.Add(9*time.Minute+11*time.Minute)); len(hist) != 0 {
		t.Fatalf("超时后历史应已清空，got %d", len(hist))
	}
}

func TestSessionMaxRoundsSlidingWindow(t *testing.T) {
	sm := newSessionManager(10*time.Minute, 2) // 最多 2 轮（4 条消息）
	now := time.Now()
	key := "test_sliding"

	sm.Append(key, "问题1", "回答1", now)
	sm.Append(key, "问题2", "回答2", now.Add(time.Second))
	sm.Append(key, "问题3", "回答3", now.Add(2*time.Second))

	hist := sm.Get(key, now.Add(3*time.Second))
	if len(hist) != 4 {
		t.Fatalf("保留最大 2 轮（4 条消息），got %d", len(hist))
	}

	// 问题1/回答1 已被淘汰，剩下 问题2/回答2 和 问题3/回答3
	if hist[0].Content != "问题2" || hist[1].Content != "回答2" {
		t.Errorf("滑动窗口头部淘汰不正确，第一条应为问题2: %+v", hist[0])
	}
	if hist[2].Content != "问题3" || hist[3].Content != "回答3" {
		t.Errorf("滑动窗口尾部不正确: %+v", hist[3])
	}
}

func TestSessionConcurrentAccess(t *testing.T) {
	sm := newSessionManager(10*time.Minute, 4)
	now := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := "concurrent_key"
			sm.Append(key, "ping", "pong", now)
			_ = sm.Get(key, now)
		}(i)
	}
	wg.Wait()
}
