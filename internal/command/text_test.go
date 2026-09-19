package command_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xingta/internal/command"
	"xingta/internal/qq"
)

// Text 是"只回文字"命令的适配器：入口仍只有 Cmd.Run 一个，这里只是省掉
// return Reply{Text:...} 的噪音。它跑在同一条 worker/inbox 链路上，
// 所以要用真报文过一遍全链，而不是单测那个闭包。

func TestTextAdapterRunsThroughWorker(t *testing.T) {
	h := newHarness(t, command.Config{})
	mustAdd(t, h.reg, command.Cmd{
		Name: "ping",
		Run: command.Text(func(_ context.Context, m *qq.Message, args []string) (string, error) {
			return "pong " + strings.Join(args, "|") + " @" + m.Author.ID, nil
		}),
	})

	h.sayGroup(t, "M1", "ping 一 二", "member")

	got := h.send.group
	if len(got) != 1 {
		t.Fatalf("回复 %d 条，want 1: %v", len(got), got)
	}
	if got[0].content != "pong 一|二 @A1" {
		t.Errorf("content = %q, want %q", got[0].content, "pong 一|二 @A1")
	}
}

func TestTextAdapterPassesThroughError(t *testing.T) {
	h := newHarness(t, command.Config{})
	boom := errors.New("command: 故意失败")
	mustAdd(t, h.reg, command.Cmd{
		Name: "fail",
		Run: command.Text(func(context.Context, *qq.Message, []string) (string, error) {
			return "半截话", boom
		}),
	})

	h.sayGroup(t, "M1", "fail", "member")

	// 带 error 时机制层回的是错误提示而不是文本本身——这条契约在 reply 里统一测过，
	// Text 只负责把 (string, error) 原样搬进 (Reply, error)，不得吞错。
	if n := len(h.send.group); n != 1 {
		t.Fatalf("回复 %d 条，want 1（出错也要有一条可解释的回复）", n)
	}
	if strings.Contains(h.send.group[0].content, "半截话") {
		t.Errorf("出错时把文本照常回了：%q（错误被吞）", h.send.group[0].content)
	}
}
