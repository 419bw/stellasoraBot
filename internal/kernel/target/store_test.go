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
	s, err := target.NewStore(doc, []string{"g:STATIC_1"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// 初始状态包含静态种子
	if !s.Has("g:STATIC_1") {
		t.Errorf("期望包含静态种子 g:STATIC_1")
	}
	if !reflect.DeepEqual(s.Targets(), []string{"g:STATIC_1"}) {
		t.Errorf("Targets() = %v, 期望 [g:STATIC_1]", s.Targets())
	}

	// 动态启用新群
	if err := s.Enable("g:DYNAMIC_2"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !s.Has("g:DYNAMIC_2") {
		t.Errorf("期望包含动态启用的 g:DYNAMIC_2")
	}
	expected := []string{"g:DYNAMIC_2", "g:STATIC_1"}
	if !reflect.DeepEqual(s.Targets(), expected) {
		t.Errorf("Targets() = %v, 期望 %v", s.Targets(), expected)
	}

	// 停用静态种子
	existed, err := s.Disable("g:STATIC_1")
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !existed {
		t.Errorf("Disable 期望返回 existed = true")
	}
	if s.Has("g:STATIC_1") {
		t.Errorf("g:STATIC_1 被停用后不应再存在")
	}
	if !reflect.DeepEqual(s.Targets(), []string{"g:DYNAMIC_2"}) {
		t.Errorf("Targets() = %v, 期望 [g:DYNAMIC_2]", s.Targets())
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
	s1, err := target.NewStore(doc, nil)
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

	// 第二任进程：模拟重启，不带静态种子
	s2, err := target.NewStore(doc, nil)
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
