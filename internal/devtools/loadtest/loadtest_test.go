package loadtest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 冒烟：确认这套量具自己跑得起来，并且数字与两端计数自洽。
// 真正的吞吐结论不在这儿，在 cmd/qqsim 的压测报告里。
func TestRunSmokeClosedLoop(t *testing.T) {
	r, err := Run(context.Background(), Profile{Total: 2000})
	if err != nil {
		t.Fatalf("跑失败: %v", err)
	}
	if r.DecodeFail != 0 {
		t.Errorf("解码失败 %d 条，压测数据形状不对", r.DecodeFail)
	}
	if r.Offered != 2000 {
		t.Errorf("Offered = %d，想要 2000", r.Offered)
	}
	if r.Admitted != r.Offered {
		t.Errorf("放行 %d / 灌入 %d，中间丢了 %d 条", r.Admitted, r.Offered, r.Offered-r.Admitted)
	}
	if r.Received != r.Offered {
		t.Errorf("客户端收到 %d，想要 %d", r.Received, r.Offered)
	}
	if r.Backlog != 0 {
		t.Errorf("收尾积压 %d 条", r.Backlog)
	}
	if r.Throughput < 100 {
		t.Errorf("吞吐 %.0f msg/s 低得可疑，量具可能把延迟算进了发送循环", r.Throughput)
	}
	if r.Total.Count != r.Offered {
		t.Errorf("延迟样本 %d 条，想要每帧一条（%d）", r.Total.Count, r.Offered)
	}
	// p50 在时钟粒度粗的机器上可能整体被抹成 0，所以用 max 判定"确实打到点了"。
	if r.Total.Max <= 0 {
		t.Errorf("max = %v，延迟打点完全没生效", r.Total.Max)
	}
	if r.ClockQuantum > time.Millisecond {
		t.Errorf("本机时钟粒度 %v 粗到测不了延迟，量具应当直接拒绝", r.ClockQuantum)
	}
	t.Logf("\n%s", r.Summary())
}

// 开环：目标速率应当被大致遵守，不能因为实现偷懒而变成闭环上限。
func TestRunRespectsTargetRate(t *testing.T) {
	const rate = 5000
	r, err := Run(context.Background(), Profile{Total: rate, RatePerSec: rate})
	if err != nil {
		t.Fatalf("跑失败: %v", err)
	}
	// 5000 条按 5000/s 应当花掉约 1 秒；快于 0.7 秒说明限速没起作用。
	if r.Elapsed < 700*time.Millisecond {
		t.Errorf("目标 %d/s 灌 %d 条只用了 %v，限速没生效，这条测试就是空跑",
			rate, r.Total.Count, r.Elapsed.Round(time.Millisecond))
	}
	if r.Admitted != r.Offered {
		t.Errorf("放行 %d / 灌入 %d", r.Admitted, r.Offered)
	}
	t.Logf("限速校验：耗时 %v，吞吐 %.0f msg/s", r.Elapsed.Round(time.Millisecond), r.Throughput)
}

// 业务处理器有 CPU 耗时时，吞吐必须按 1/handler 量级掉下来。
// 这条是量具的灵敏度对照：如果连烧 CPU 都测不出差别，报告里的数就没有参考价值。
func TestRunDetectsHandlerCost(t *testing.T) {
	idle, err := Run(context.Background(), Profile{Total: 3000})
	if err != nil {
		t.Fatalf("空处理器那轮跑失败: %v", err)
	}
	burned, err := Run(context.Background(), Profile{Total: 3000, HandlerCPU: 200 * time.Microsecond})
	if err != nil {
		t.Fatalf("带业务耗时那轮跑失败: %v", err)
	}
	if burned.Throughput >= idle.Throughput*0.9 {
		t.Errorf("烧 200µs 业务 CPU 后吞吐仍是 %.0f（空载 %.0f），量具测不出业务成本",
			burned.Throughput, idle.Throughput)
	}
	if burned.Total.P50 < idle.Total.P50 {
		t.Errorf("带业务耗时的 p50 = %v 反而低于空载 %v，打点有问题",
			burned.Total.P50, idle.Total.P50)
	}
	t.Logf("空载 %.0f msg/s，带 200µs 业务 %.0f msg/s", idle.Throughput, burned.Throughput)
}

func TestSummaryMentionsBothEndpoints(t *testing.T) {
	r := &Result{Offered: 1, Backlog: 3, DecodeFail: 2}
	s := r.Summary()
	for _, want := range []string{"解码失败", "积压", "RSS"} {
		if !strings.Contains(s, want) {
			t.Errorf("报告里缺 %q：\n%s", want, s)
		}
	}
}

func TestSampleLatenciesEmptyIsSafe(t *testing.T) {
	if p := sampleLatencies(nil); p.Count != 0 || p.P99 != 0 {
		t.Errorf("空样本应全零: %+v", p)
	}
	if p := sampleLatencies([]int64{1000}); p.P50 != time.Microsecond || p.Max != time.Microsecond {
		t.Errorf("单样本: %+v", p)
	}
}
