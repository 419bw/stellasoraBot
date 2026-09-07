// Command qqsim 是本地网关模拟器与压测驾驶舱。
//
// 设计目标是「同一份二进制，本机跑一遍、手机跑一遍，出同一份报告」：
// 桌面 x86 的数字对他那台 Termux 服务器没有参考价值，所以这里不内置任何
// "预期值"，只负责把量出来的数印清楚，包括量具自身的分辨率下限。
//
//	qqsim load  -total 50000                  # 闭环灌满，看吞吐与延迟分位
//	qqsim load  -total 30000 -rate 3000       # 开环定速
//	qqsim sweep -rates 200,500,1000,2000,5000 # 分级提速找崩点
//	qqsim serve -rate 2                       # 挂着当假网关，给 qqwatch 连
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"xingta/internal/devtools/loadtest"
	"xingta/internal/devtools/qqsim"
)

func main() {
	mode := "load"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		mode = os.Args[1]
	}
	var err error
	switch mode {
	case "load":
		err = runLoad(os.Args[2:])
	case "sweep":
		err = runSweep(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	default:
		fmt.Printf("未知子命令 %q，可用: load / sweep / serve\n", mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "失败:", err)
		os.Exit(1)
	}
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	total := fs.Int("total", 20000, "灌入条数")
	rate := fs.Int("rate", 0, "目标速率 条/秒，0=尽力灌")
	cpu := fs.Duration("cpu", 0, "模拟业务处理器的纯 CPU 耗时，如 2ms")
	keys := fs.Int("dedup-keys", 0, "去重表容量，0=默认 8192")
	hb := fs.Int64("heartbeat-ms", 45000, "模拟器下发的心跳间隔（毫秒）")
	verbose := fs.Bool("v", false, "打印实时进度")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	start := time.Now()
	res, err := loadtest.Run(ctx, loadtest.Profile{
		Total: *total, RatePerSec: *rate, HandlerCPU: *cpu,
		DedupKeys: *keys, HeartbeatMS: *hb, Verbose: *verbose,
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n== 单轮压测（起进程到收尾 %v）==\n%s", time.Since(start).Round(time.Millisecond), res.Summary())
	return nil
}

func runSweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	rates := fs.String("rates", "100,300,600,1200,2500,5000,10000", "逗号分隔的目标速率")
	seconds := fs.Int("seconds", 3, "每档持续秒数")
	cpu := fs.Duration("cpu", 0, "模拟业务处理器的纯 CPU 耗时")
	keys := fs.Int("dedup-keys", 0, "去重表容量")
	verbose := fs.Bool("v", false, "打印每档明细")
	if err := fs.Parse(args); err != nil {
		return err
	}
	list, err := parseInts(*rates)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rep, err := loadtest.Sweep(ctx, loadtest.SweepProfile{
		Rates: list, Seconds: *seconds, HandlerCPU: *cpu, DedupKeys: *keys, Verbose: *verbose,
	})
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println(rep.String())
	if *verbose {
		for _, s := range rep.Steps {
			fmt.Printf("\n--- %d/s ---\n%s", s.Rate, s.Result.Summary())
		}
	}
	return nil
}

// runServe 挂一个模拟器持续推消息，给别的客户端（含 qqwatch 那种真连工具）连。
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	rate := fs.Int("rate", 2, "每秒推几条消息，0=不自动推")
	replay := fs.Bool("replay-on-resume", true, "resume 时补发历史事件（真平台行为）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sim, err := qqsim.New(qqsim.Config{
		Addr:           "127.0.0.1:8321",
		HeartbeatMS:    45000,
		ReplayOnResume: *replay,
		Logf:           func(f string, a ...any) { fmt.Printf("[sim] "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Printf("网关模拟器已就绪： %s （HTTP 底座 %s）\n", sim.WSURL(), sim.BaseURL())
	fmt.Printf("resume 补发: %v，自动推送: %d 条/秒\n按 Ctrl-C 退出\n", *replay, *rate)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	gen := qqsim.DefaultGenerator()
	tick := time.NewTicker(time.Second / time.Duration(max(*rate, 1)))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("退出前计数: %+v\n", sim.Stats())
			return sim.Close()
		case <-tick.C:
			if *rate <= 0 {
				continue
			}
			for i := 0; i < *rate; i++ {
				t, raw, _ := gen.NextGroupAt()
				if _, err := sim.Push(t, raw); err != nil {
					return err
				}
			}
		}
	}
}

func parseInts(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("速率 %q 不是正整数", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-rates 是空的")
	}
	return out, nil
}
