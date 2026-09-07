package loadtest

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// 分级提速找崩点：一档一档往上加目标速率，直到某一档跟不上。
//
// 只给一个"最大吞吐"是不够的：闭环灌出来的上限里混着模拟器和 loopback 的开销，
// 而真正要回答的是「手机能稳定扛住多少条每秒、到多少开始堆积」。
type SweepProfile struct {
	// Rates 递升的目标速率序列（条/秒）。
	Rates []int

	// Seconds 是每档持续秒数。太短会被 GC 和调度噪声主导。
	Seconds int

	HandlerCPU time.Duration
	DedupKeys  int
	Heartbeat  int64
	Verbose    bool

	// MaxBacklog 是收尾时允许残留的条数，超过即判这一档跟不上。默认 50。
	MaxBacklog int
	// MinRatio 是「实测吞吐 / 目标速率」的最低比值，默认 0.95。
	MinRatio float64
}

type Step struct {
	Rate   int
	Result *Result
	OK     bool
	Reason string
}

type SweepReport struct {
	Profile    SweepProfile
	Steps      []Step
	Sustained  int // 最高一档扛得住的速率
	BreakPoint int // 第一档跟不上的速率；0 表示全部扛住了
}

func Sweep(ctx context.Context, p SweepProfile) (*SweepReport, error) {
	if len(p.Rates) == 0 {
		return nil, fmt.Errorf("loadtest: Sweep 需要至少一个速率")
	}
	if p.Seconds <= 0 {
		p.Seconds = 3
	}
	if p.MaxBacklog <= 0 {
		p.MaxBacklog = 50
	}
	if p.MinRatio <= 0 {
		p.MinRatio = 0.95
	}

	rep := &SweepReport{Profile: p}
	for _, rate := range p.Rates {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		res, err := Run(ctx, Profile{
			Total:       rate * p.Seconds,
			RatePerSec:  rate,
			HandlerCPU:  p.HandlerCPU,
			DedupKeys:   p.DedupKeys,
			HeartbeatMS: p.Heartbeat,
			Verbose:     p.Verbose,
		})
		if err != nil {
			return rep, fmt.Errorf("loadtest: %d/s 这档跑失败: %w", rate, err)
		}
		s := Step{Rate: rate, Result: res}
		switch {
		case res.DecodeFail > 0:
			s.Reason = fmt.Sprintf("解码失败 %d 条", res.DecodeFail)
		case res.Backlog > p.MaxBacklog:
			s.Reason = fmt.Sprintf("收尾仍积压 %d 条", res.Backlog)
		case res.Throughput < float64(rate)*p.MinRatio:
			s.Reason = fmt.Sprintf("实测吞吐 %.0f 只有目标的 %.0f%%",
				res.Throughput, 100*res.Throughput/float64(rate))
		default:
			s.OK = true
			s.Reason = "扛住"
		}
		rep.Steps = append(rep.Steps, s)
		if s.OK {
			if rate > rep.Sustained {
				rep.Sustained = rate
			}
		} else if rep.BreakPoint == 0 {
			rep.BreakPoint = rate
		}
	}
	return rep, nil
}

func (r *SweepReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "分级压测（每档 %ds，GOMAXPROCS=%d，处理器耗时 %v）\n",
		r.Profile.Seconds, runtime.GOMAXPROCS(0), r.Profile.HandlerCPU)
	fmt.Fprintf(&b, "%-10s %-10s %-10s %-10s %-10s %s\n",
		"目标/s", "实测/s", "积压", "p99", "p999", "判定")
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "%-10d %-10.0f %-10d %-10v %-10v %s\n",
			s.Rate, s.Result.Throughput, s.Result.Backlog,
			s.Result.Total.P99.Round(time.Microsecond),
			s.Result.Total.P999.Round(time.Microsecond), s.Reason)
	}
	fmt.Fprintf(&b, "可持续速率: %d msg/s\n", r.Sustained)
	if r.BreakPoint > 0 {
		fmt.Fprintf(&b, "崩点（首次跟不上）: %d msg/s\n", r.BreakPoint)
	} else {
		fmt.Fprintf(&b, "崩点: 未触及（最后一档 %d msg/s 仍扛得住）\n",
			r.Steps[len(r.Steps)-1].Rate)
	}
	return b.String()
}
