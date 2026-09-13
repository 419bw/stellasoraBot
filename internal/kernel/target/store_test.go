package target_test

import (
	"reflect"
	"testing"

	"xingta/internal/kernel/target"
	"xingta/internal/store/storetest"
)

func TestValidate(t *testing.T) {
	good := []string{
		"g:GROUP_123",
		"u:USER_456",
		"g:AABBCCDDEEFF00112233445566778899",
	}
	for _, in := range good {
		if err := target.Validate(in); err != nil {
			t.Errorf("Validate(%q) 期望成功，实际报错: %v", in, err)
		}
	}

	bad := []string{
		"",
		"GROUP_123",
		"g:",
		"u:",
		"g: ",
		"g: 123",
		"g:123 ",
		"x:123",
		"group:123",
	}
	for _, in := range bad {
		if err := target.Validate(in); err == nil {
			t.Errorf("Validate(%q) 期望报错，实际未报错", in)
		}
	}
}

func TestStoreEnableAndDisable(t *testing.T) {
	doc := storetest.NewMem()
	s, err := target.NewStore(doc)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if !reflect.DeepEqual(s.Targets(), []string{}) {
		t.Errorf("Targets() = %v, 期望空", s.Targets())
	}

	// 动态启用两个群
	if err := s.Enable("g:G1"); err != nil {
		t.Fatalf("Enable G1: %v", err)
	}
	if !s.Has("g:G1") {
		t.Errorf("期望包含动态启用的 g:G1")
	}
	if err := s.Enable("g:G2"); err != nil {
		t.Fatalf("Enable G2: %v", err)
	}
	if !reflect.DeepEqual(s.Targets(), []string{"g:G1", "g:G2"}) {
		t.Errorf("Targets() = %v, 期望 [g:G1 g:G2]", s.Targets())
	}

	// 停用其中一个
	existed, err := s.Disable("g:G1")
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !existed {
		t.Errorf("Disable 期望返回 existed = true")
	}
	if s.Has("g:G1") {
		t.Errorf("g:G1 被停用后不应再存在")
	}
	if !reflect.DeepEqual(s.Targets(), []string{"g:G2"}) {
		t.Errorf("Targets() = %v, 期望 [g:G2]", s.Targets())
	}

	// 停用不存在的目标
	existed, err = s.Disable("g:NOT_EXIST")
	if err != nil {
		t.Fatalf("Disable(NOT_EXIST): %v", err)
	}
	if existed {
		t.Errorf("Disable(NOT_EXIST) 期望返回 existed = false")
	}
}

func TestStorePersistenceAcrossRestarts(t *testing.T) {
	doc := storetest.NewMem()

	// 第一任进程：启用 G1 与 G2，随后退订 G1
	s1, err := target.NewStore(doc)
	if err != nil {
		t.Fatalf("s1 NewStore: %v", err)
	}
	if err := s1.Enable("g:G1"); err != nil {
		t.Fatalf("Enable G1: %v", err)
	}
	if err := s1.Enable("g:G2"); err != nil {
		t.Fatalf("Enable G2: %v", err)
	}
	if _, err := s1.Disable("g:G1"); err != nil {
		t.Fatalf("Disable G1: %v", err)
	}

	// 第二任进程：模拟重启
	s2, err := target.NewStore(doc)
	if err != nil {
		t.Fatalf("s2 NewStore: %v", err)
	}

	if s2.Has("g:G1") {
		t.Errorf("重启后已退订的 G1 不应恢复")
	}
	if !s2.Has("g:G2") {
		t.Errorf("重启后未退订的 G2 应当恢复")
	}
	if !reflect.DeepEqual(s2.Targets(), []string{"g:G2"}) {
		t.Errorf("s2 Targets() = %v, 期望 [g:G2]", s2.Targets())
	}
}

func TestStoreMultiTopic(t *testing.T) {
	doc := storetest.NewMem()
	s, err := target.NewStore(doc)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// 注册两个主题
	t1 := target.Topic{Key: "expiry", Name: "活动到期提醒", Desc: "提醒"}
	t2 := target.Topic{Key: "poster", Name: "版本日历海报", Desc: "海报"}
	if err := s.RegisterTopic(t1); err != nil {
		t.Fatalf("RegisterTopic(expiry): %v", err)
	}
	if err := s.RegisterTopic(t2); err != nil {
		t.Fatalf("RegisterTopic(poster): %v", err)
	}

	topics := s.Topics()
	if len(topics) != 2 || topics[0].Key != "expiry" || topics[1].Key != "poster" {
		t.Fatalf("Topics() = %+v, 期望 [expiry, poster]", topics)
	}
	if meta, ok := s.Topic("expiry"); !ok || meta.Name != "活动到期提醒" {
		t.Errorf("Topic(expiry) = %+v, ok=%v", meta, ok)
	}

	// 单独给 G1 开启 expiry
	if err := s.EnableTopic("g:G1", "expiry"); err != nil {
		t.Fatalf("EnableTopic: %v", err)
	}
	// 给 G2 全开
	if err := s.Enable("g:G2"); err != nil {
		t.Fatalf("Enable G2: %v", err)
	}

	// 状态断言
	if !s.HasTopic("g:G1", "expiry") || s.HasTopic("g:G1", "poster") {
		t.Errorf("G1 主题状态异常")
	}
	if !s.HasTopic("g:G2", "expiry") || !s.HasTopic("g:G2", "poster") {
		t.Errorf("G2 主题状态异常")
	}

	if !reflect.DeepEqual(s.TopicsOf("g:G1"), []string{"expiry"}) {
		t.Errorf("TopicsOf(G1) = %v, 期望 [expiry]", s.TopicsOf("g:G1"))
	}
	if !reflect.DeepEqual(s.TopicsOf("g:G2"), []string{"expiry", "poster"}) {
		t.Errorf("TopicsOf(G2) = %v, 期望 [expiry, poster]", s.TopicsOf("g:G2"))
	}

	if !reflect.DeepEqual(s.TargetsFor("expiry"), []string{"g:G1", "g:G2"}) {
		t.Errorf("TargetsFor(expiry) = %v", s.TargetsFor("expiry"))
	}
	if !reflect.DeepEqual(s.TargetsFor("poster"), []string{"g:G2"}) {
		t.Errorf("TargetsFor(poster) = %v", s.TargetsFor("poster"))
	}

	// 关闭 G2 的 poster
	existed, err := s.DisableTopic("g:G2", "poster")
	if err != nil || !existed {
		t.Fatalf("DisableTopic poster on G2: existed=%v, err=%v", existed, err)
	}
	if s.HasTopic("g:G2", "poster") {
		t.Errorf("G2 poster 被关闭后不应再开启")
	}
	if !reflect.DeepEqual(s.TargetsFor("poster"), []string{}) {
		t.Errorf("TargetsFor(poster) 期望为空，实际 = %v", s.TargetsFor("poster"))
	}

	// 关闭 G1 的 expiry（关闭仅剩的主题应使目标完全移除）
	existed, err = s.DisableTopic("g:G1", "expiry")
	if err != nil || !existed {
		t.Fatalf("DisableTopic expiry on G1: existed=%v, err=%v", existed, err)
	}
	if s.Has("g:G1") {
		t.Errorf("G1 关闭所有主题后应该被彻底移除")
	}
}
