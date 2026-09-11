package pushops_test

import (
	"context"
	"strings"
	"testing"

	"xingta/internal/command"
	"xingta/internal/feature/pushops"
	"xingta/internal/kernel/target"
	"xingta/internal/qq"
	"xingta/internal/store/storetest"
)

func newTestRig(t *testing.T, staticTargets []string) (*command.Registry, *target.Store, command.Cmd) {
	t.Helper()
	reg := command.NewRegistry()
	doc := storetest.NewMem()
	store, err := target.NewStore(doc, staticTargets)
	if err != nil {
		t.Fatalf("target.NewStore: %v", err)
	}

	feat := pushops.New(reg, store, pushops.Config{})
	if err := feat.Start(context.Background(), nil); err != nil {
		t.Fatalf("pushops Start: %v", err)
	}

	cmd, ok := reg.Lookup("push")
	if !ok {
		t.Fatalf("命令 push 未注册")
	}
	return reg, store, cmd
}

func TestPushCommandRegisteredAsAdminOnly(t *testing.T) {
	_, _, cmd := newTestRig(t, nil)
	if !cmd.Admin {
		t.Error("push 命令应配置为 Admin: true")
	}
	if cmd.Name != "push" {
		t.Errorf("命令名 = %q, 期望 push", cmd.Name)
	}
	if len(cmd.Aliases) != 0 {
		t.Errorf("期望无别名，实际有: %v", cmd.Aliases)
	}
}

func TestPushToggleInGroup(t *testing.T) {
	_, store, cmd := newTestRig(t, nil)
	_ = store.RegisterTopic(target.Topic{Key: "expiry", Name: "活动到期提醒"})
	_ = store.RegisterTopic(target.Topic{Key: "poster", Name: "版本日历海报"})

	msgGroup := &qq.Message{
		Kind:        qq.EventGroupAtMessage,
		ID:          "M1",
		GroupOpenID: "GRP_A",
		Author:      qq.Author{MemberRole: "admin"},
	}

	// 1. 初始状态：未开启
	rep, err := cmd.Run(context.Background(), msgGroup, []string{})
	if err != nil {
		t.Fatalf("push status: %v", err)
	}
	if !strings.Contains(rep.Text, "[✗] expiry") || !strings.Contains(rep.Text, "[✗] poster") {
		t.Errorf("初始状态期望各项均为 [✗]，实际输出:\n%s", rep.Text)
	}
	if store.Has("g:GRP_A") {
		t.Error("store 不应包含 g:GRP_A")
	}

	// 2. 单独开启 expiry：push on expiry
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"on", "expiry"})
	if err != nil {
		t.Fatalf("push on expiry: %v", err)
	}
	if !strings.Contains(rep.Text, "已开启本群「活动到期提醒 (expiry)」主动推送") {
		t.Errorf("push on expiry 输出未包含开启提示: %s", rep.Text)
	}
	if !store.HasTopic("g:GRP_A", "expiry") {
		t.Error("store 应该开启 expiry")
	}
	if store.HasTopic("g:GRP_A", "poster") {
		t.Error("store 不应开启 poster")
	}

	// 3. 查看状态：push
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"status"})
	if err != nil {
		t.Fatalf("push status: %v", err)
	}
	if !strings.Contains(rep.Text, "[✓] expiry") || !strings.Contains(rep.Text, "[✗] poster") {
		t.Errorf("期望 expiry 为 [✓] 且 poster 为 [✗]，实际输出:\n%s", rep.Text)
	}

	// 4. 全开：push on all
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"on", "all"})
	if err != nil {
		t.Fatalf("push on all: %v", err)
	}
	if !strings.Contains(rep.Text, "已全部开启") {
		t.Errorf("push on all 输出未包含全开提示: %s", rep.Text)
	}
	if !store.HasTopic("g:GRP_A", "poster") || !store.HasTopic("g:GRP_A", "expiry") {
		t.Error("全开后两者都应为 true")
	}

	// 5. 单关 poster：push off poster
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"off", "poster"})
	if err != nil {
		t.Fatalf("push off poster: %v", err)
	}
	if !strings.Contains(rep.Text, "已关闭本群「版本日历海报 (poster)」主动推送") {
		t.Errorf("push off poster 输出错误: %s", rep.Text)
	}
	if store.HasTopic("g:GRP_A", "poster") {
		t.Error("poster 应被关闭")
	}
	if !store.HasTopic("g:GRP_A", "expiry") {
		t.Error("expiry 仍应保持开启")
	}

	// 6. 全关：push off
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"off"})
	if err != nil {
		t.Fatalf("push off: %v", err)
	}
	if !strings.Contains(rep.Text, "已全部关闭") {
		t.Errorf("push off 输出未包含全关提示: %s", rep.Text)
	}
	if store.Has("g:GRP_A") {
		t.Error("全关后目标应被彻底移除")
	}

	// 7. 测试未知主题提示
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"on", "unknown_func"})
	if err != nil {
		t.Fatalf("push on unknown: %v", err)
	}
	if !strings.Contains(rep.Text, "未知功能 \"unknown_func\"") || !strings.Contains(rep.Text, "expiry, poster") {
		t.Errorf("未知功能提示异常: %s", rep.Text)
	}
}

func TestPushRejectsDirectMessage(t *testing.T) {
	_, _, cmd := newTestRig(t, nil)

	msgC2C := &qq.Message{
		Kind:   qq.EventC2CMessage,
		ID:     "M2",
		Author: qq.Author{ID: "USER_1"},
	}

	rep, err := cmd.Run(context.Background(), msgC2C, []string{"on"})
	if err != nil {
		t.Fatalf("push on in c2c: %v", err)
	}
	if !strings.Contains(rep.Text, "群聊") {
		t.Errorf("单聊使用期望提示仅支持群聊，实际输出: %s", rep.Text)
	}
}

func TestPushUnknownSubcommand(t *testing.T) {
	_, _, cmd := newTestRig(t, nil)

	msgGroup := &qq.Message{
		Kind:        qq.EventGroupAtMessage,
		ID:          "M3",
		GroupOpenID: "GRP_B",
		Author:      qq.Author{MemberRole: "admin"},
	}

	rep, err := cmd.Run(context.Background(), msgGroup, []string{"unknown"})
	if err != nil {
		t.Fatalf("push unknown: %v", err)
	}
	if !strings.Contains(rep.Text, "用法") {
		t.Errorf("未知参数期望提示用法，实际输出: %s", rep.Text)
	}
}
