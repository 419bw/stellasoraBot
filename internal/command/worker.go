// 本文件是被动消息的执行流：在哪跑、怎么排队、跑挂了怎么办。
// 命令表与回复规则见 command.go。
//
// 执行模型：Hub 的同步派发只把消息塞进 inbox（µs 级返回），真正的处理跑在 Attach
// 起的单 worker 上——网关循环与心跳/判死共享，绝不等命令跑完（「日历」冷出图实测
// 最坏单趟 60s，两趟同挂 130s 会越过 82.5s 的判死阈值，把通着的链路自己判死）。
// 单 worker = 全局按到达顺序处理，与挪走前的循环语义一致，也不额外叠加浏览器并发。
// 被动回复**不进 kernel/queue**：那条队列管的是主动消息的配额（限流/合并/退避/
// 日额度），这些恰恰都不该管被动回复；inbox 满则就地丢弃 + 记日志，不回"太忙"
// 提示（那会白耗这条消息的回复槽）。
//
// Config.Fallback 挂了的时候另有第二条同形制的流专跑「没命中命令的消息」：命令流
// 照旧一条、串行、零网络，慢的那一趟（要等外网上游）全在另一条上。见 Attach。
package command

import (
	"context"
	"runtime/debug"

	"xingta/internal/qq"
)

// Worker 是一条被动消息处理流：一个有界 inbox + 一个串行消费的 goroutine。
// 消息在 Hub 的同步派发里只是被塞进 inbox；Lookup、执行、回复全部发生在 worker 上，
// 与网关循环（心跳、判死）从此无关。
type Worker struct {
	jobs chan func(context.Context)
	done chan struct{}
	size int
	logf func(string, ...any)
	// next 是兜底流，只在挂了 Config.Fallback 时存在。Drain 必须连它一起等：
	// 否则测试里的 flush 只排空命令流，兜底回复会晚于断言到达——用例照样通过。
	next *Worker
}

// newWorker 按 cfg 造一条流。两条流同形制、同容量（QueueSize），不另开旋钮：
// 兜底的消息量不会超过 @机器人 的量，同一个上限够用。
func newWorker(cfg Config) *Worker {
	return &Worker{
		jobs: make(chan func(context.Context), cfg.QueueSize),
		done: make(chan struct{}),
		size: cfg.QueueSize,
		logf: cfg.Logf,
	}
}

// Drain 阻塞到"调用时刻之前入队的消息在两条流上都处理完"，供测试与停机诊断用。
// 返回 false = 没排空（某条流已退出，或兜底队列满到屏障挤不进去），调用方别当成功。
// 屏障作业走的是同一条 inbox，排队顺序即处理顺序；兜底屏障必须由命令流自己转投，
// 从外面直接投 next 会让兜底作业排在屏障之后。
func (w *Worker) Drain() bool {
	drained := make(chan bool, 1) // 带缓冲：本函数可能从别的 case 先返回，别让屏障卡住
	select {
	case w.jobs <- func(context.Context) { drained <- w.drainNext() }:
	case <-w.done:
		return false
	}
	var nextDone chan struct{}
	if w.next != nil {
		nextDone = w.next.done
	}
	select {
	case ok := <-drained:
		return ok
	case <-w.done:
		return false
	case <-nextDone:
		// 兜底流中途退出，不再等它。已排空的情形要认出来：drained 与 nextDone
		// 会同时就绪，select 随机挑，不回查一次就把成功判成失败。
		select {
		case ok := <-drained:
			return ok
		default:
			return false
		}
	}
}

// drainNext 在命令流上把屏障转投兜底流，等它跑完。
// 每个跨流交互点都带 done 分支：jobs 这个 chan 全仓从不 close，往已退出 worker 的
// jobs 上裸投是永久阻塞（不是 panic），少一个分支就少一条退出路径。
// 投屏障用 default 而不是干等空位：兜底流串行、每条最多几十秒，等空位等于让命令流
// 被自己的一次排空检查堵住——宁可报"没排空"。
func (w *Worker) drainNext() bool {
	if w.next == nil {
		return true
	}
	inner := make(chan struct{})
	select {
	case w.next.jobs <- func(context.Context) { close(inner) }:
	case <-w.next.done:
		return false
	default:
		return false
	}
	select {
	case <-inner:
		return true
	case <-w.next.done:
		return false
	}
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.done)
	for {
		select {
		case <-ctx.Done():
			return // 停机不排空：被动回复窗口只有几分钟，攒下的旧消息没有意义
		case job := <-w.jobs:
			w.invoke(ctx, job)
		}
	}
}

// invoke 是 worker 与业务闭包之间的保险丝：形制同 hub 的 invokeHandler——只包这一次
// 调用，一条消息的 panic 不该带走 worker（后续消息全哑 = 机器人看起来死了），
// 更不该带走进程。机制层自己的 bug 不在此列。
func (w *Worker) invoke(ctx context.Context, job func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			w.logf("command: 被动消息处理 panic（已兜住，继续处理下一条）: %v\n%s", r, debug.Stack())
		}
	}()
	job(ctx)
}

// Attach 把命令表接到 Hub 上：群 @机器人 与单聊两条事件各挂一个处理器，并起一个
// 专用 worker 串行执行命令。处理器只做"投进 inbox"，µs 级返回；ctx 决定 worker
// 的生命周期（生产传进程级 signal ctx，网关重连不取消它）。
//
// 返回值给测试与诊断用（Drain）；生产接线忽略即可。
func Attach(ctx context.Context, reg *Registry, h *qq.Hub, send SendAPI, cfg Config) *Worker {
	cfg = cfg.withDefaults()
	w := newWorker(cfg)
	var fb *Worker
	if cfg.Fallback != nil {
		// 兜底另起一条流，而不是在命令流里就地执行：回调要等外网上游，压在命令流上
		// 就是让闲聊拖住所有人的命令，也破了「命令路径零网络」这条红线。
		// 复用 Worker 而不是裸 goroutine，才继承 run/invoke 的有界队列与 panic 保险丝
		// （裸 goroutine 里的 panic 不在任何 recover 之下）。
		fb = newWorker(cfg)
		w.next = fb
		go fb.run(ctx)
	}
	go w.run(ctx)
	handle := func(_ context.Context, m *qq.Message) error {
		// 闭包捕获 m：报文在 Hub 里解好、判过重，进到这里已是"确认要处理的一条"。
		// 刻意不接 Hub 传来的 ctx——那是网关的连接 ctx，重连不该掐断在途命令；
		// 用 worker 自己的 ctx（runtime 级），见 run 的启动处。
		job := func(ctx context.Context) { dispatch(ctx, reg, send, cfg, fb, m) }
		select {
		case w.jobs <- job:
		default:
			// 只丢不弹"太忙"：回话会白耗这条消息的回复槽，静默与"不是命令"同口径。
			cfg.Logf("command: 被动 inbox 已满（容量 %d），丢弃消息 %s", w.size, m.ID)
		}
		// 刻意不往上抛错误：webhook 前端见到处理器报错会撤销去重登记等平台重投，
		// 而命令可能已经在 worker 上跑过一遍了，重投就是"同一条命令回两遍"。
		return nil
	}
	h.OnMessage(qq.EventGroupAtMessage, handle)
	h.OnMessage(qq.EventC2CMessage, handle)
	return w
}
