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
	if !strings.Contains(rep.Text, "已关闭") {
		t.Errorf("初始状态期望已关闭，实际输出: %s", rep.Text)
	}
	if store.Has("g:GRP_A") {
		t.Error("store 不应包含 g:GRP_A")
	}

	// 2. 开启推送：push on
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"on"})
	if err != nil {
		t.Fatalf("push on: %v", err)
	}
	if !strings.Contains(rep.Text, "本群主动推送已开启") {
		t.Errorf("push on 输出未包含开启提示: %s", rep.Text)
	}
	if !strings.Contains(rep.Text, "允许主动发送消息") {
		t.Errorf("push on 输出未包含权限提示: %s", rep.Text)
	}
	if !store.Has("g:GRP_A") {
		t.Error("store 应该包含 g:GRP_A")
	}

	// 3. 再次查看状态：push
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"status"})
	if err != nil {
		t.Fatalf("push status: %v", err)
	}
	if !strings.Contains(rep.Text, "已开启") {
		t.Errorf("开启后期望状态为已开启，实际输出: %s", rep.Text)
	}

	// 4. 关闭推送：push off
	rep, err = cmd.Run(context.Background(), msgGroup, []string{"off"})
	if err != nil {
		t.Fatalf("push off: %v", err)
	}
	if !strings.Contains(rep.Text, "本群主动推送已关闭") {
		t.Errorf("push off 输出未包含关闭提示: %s", rep.Text)
	}
	if store.Has("g:GRP_A") {
		t.Error("store 不应再包含 g:GRP_A")
	}

	// 5. 再次查看状态
	rep, err = cmd.Run(context.Background(), msgGroup, []string{})
	if err != nil {
		t.Fatalf("push status: %v", err)
	}
	if !strings.Contains(rep.Text, "已关闭") {
		t.Errorf("关闭后期望状态为已关闭，实际输出: %s", rep.Text)
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
