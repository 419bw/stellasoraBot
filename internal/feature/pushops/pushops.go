// Package pushops 是主动推送运维功能：允许群管理员开启、关闭或查看本群的主动推送。
//
// 分层契约：本包是功能层，通过 target.Manager 控制推送目标，不直接操纵存储，
// 也不认识具体推送了什么业务内容。
package pushops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"xingta/internal/command"
	"xingta/internal/kernel"
	"xingta/internal/kernel/target"
	"xingta/internal/qq"
)

// Config 配置项。
type Config struct {
	Logf func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

type feature struct {
	reg command.Registrar
	mgr target.Manager
	cfg Config
}

// New 构造 pushops 功能实例。
func New(reg command.Registrar, mgr target.Manager, cfg Config) kernel.Feature {
	return &feature{
		reg: reg,
		mgr: mgr,
		cfg: cfg.withDefaults(),
	}
}

func (f *feature) Name() string { return "pushops" }

// Start 注册 push 管理员命令。
func (f *feature) Start(ctx context.Context, _ kernel.API) error {
	if f.reg == nil {
		return errors.New("pushops: reg 不能为空")
	}
	if f.mgr == nil {
		return errors.New("pushops: mgr 不能为空")
	}

	cmd := command.Cmd{
		Name:  "push",
		Admin: true,
		Usage: "push on/off 控制本群主动推送；单独发送 push 查看状态",
		Run:   command.Text(f.handlePush),
	}
	return f.reg.Add(cmd)
}

func (f *feature) handlePush(ctx context.Context, m *qq.Message, args []string) (string, error) {
	if m.GroupOpenID == "" {
		return "该命令仅支持在群聊中使用。", nil
	}

	targetID := "g:" + m.GroupOpenID
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}

	switch sub {
	case "", "status":
		if f.mgr.Has(targetID) {
			return "本群主动推送已开启。\n（包含活动到期提醒与版本日历图；发送 push off 可关闭）", nil
		}
		return "本群主动推送已关闭。\n（发送 push on 可开启）", nil

	case "on":
		if err := f.mgr.Enable(targetID); err != nil {
			return "", fmt.Errorf("开启推送失败: %w", err)
		}
		f.cfg.Logf("pushops: 群 %s 开启主动推送", m.GroupOpenID)
		return "本群主动推送已开启（包含活动到期提醒与版本日历图）。\n提示：请确保群管理员已在 QQ 群设置中开启机器人的「允许主动发送消息」权限。", nil

	case "off":
		existed, err := f.mgr.Disable(targetID)
		if err != nil {
			return "", fmt.Errorf("关闭推送失败: %w", err)
		}
		f.cfg.Logf("pushops: 群 %s 关闭主动推送（此前开启状态: %v）", m.GroupOpenID, existed)
		return "本群主动推送已关闭。", nil

	default:
		return "用法：\n- push on：开启本群主动推送\n- push off：关闭本群主动推送\n- push：查看当前推送状态", nil
	}
}
