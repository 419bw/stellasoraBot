// Package kernel 是内核组装点：调度器 + 发送队列 + 活动日历 + 可插拔功能的接线处。
package kernel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/schedule"
)

// API 是内核交给一个功能使用的能力面。
// 功能只能投递消息和排期，不能自己碰 HTTP 客户端 —— 限流与重试因此是全局唯一的。
type API interface {
	Submit(item queue.Item) error
	Schedule(id string, at time.Time, fn func(context.Context) error)
	Cancel(id string) bool
	Calendar() calendar.View
	Scheduler() schedule.Scheduler
}

// Feature 是一个可插拔功能单元（B 站动态、游戏公告、日历提醒……）。
//
// Start 必须非阻塞：需要长驻的抓取循环请自行起 goroutine 并尊重 ctx。
// 返回 error 视为启动失败，内核会整体退出。
type Feature interface {
	Name() string
	Start(ctx context.Context, api API) error
}

type Runtime struct {
	sched    schedule.Scheduler
	queue    *queue.Dispatcher
	cal      calendar.View
	features []Feature
}

func NewRuntime(sink queue.Sink, p queue.Policy, cal calendar.View) *Runtime {
	return &Runtime{
		sched: schedule.New(),
		queue: queue.New(sink, p),
		cal:   cal,
	}
}

func (r *Runtime) Register(f Feature) {
	r.features = append(r.features, f)
}

func (r *Runtime) Calendar() calendar.View { return r.cal }

// Stats 与 Backlog 是内核的观测出口：没有它们，线上只能靠猜。
func (r *Runtime) Stats() queue.Stats { return r.queue.Stats() }

func (r *Runtime) Backlog() int { return r.queue.Backlog() }

func (r *Runtime) InFlight() int { return r.queue.InFlight() }

// Run 起内核协程并按顺序启动所有功能，阻塞到 ctx 结束。
func (r *Runtime) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	schedErr := make(chan error, 1)
	queueErr := make(chan error, 1)
	go func() { schedErr <- r.sched.Run(ctx) }()
	go func() { queueErr <- r.queue.Run(ctx) }()

	api := &api{runtime: r}
	for _, f := range r.features {
		if err := f.Start(ctx, api); err != nil {
			return fmt.Errorf("功能 %s 启动失败: %w", f.Name(), err)
		}
	}

	<-ctx.Done()
	sErr, qErr := <-schedErr, <-queueErr
	return errors.Join(ctx.Err(), sErr, qErr)
}

type api struct{ runtime *Runtime }

func (a *api) Submit(item queue.Item) error { return a.runtime.queue.Submit(item) }

func (a *api) Schedule(id string, at time.Time, fn func(context.Context) error) {
	a.runtime.sched.Schedule(schedule.Task{ID: id, At: at, Fn: fn})
}

func (a *api) Cancel(id string) bool { return a.runtime.sched.Cancel(id) }

func (a *api) Calendar() calendar.View       { return a.runtime.cal }
func (a *api) Scheduler() schedule.Scheduler { return a.runtime.sched }

var (
	_ API     = (*api)(nil)
	_ Feature = featureFunc{}
)

// featureFunc 方便测试与临时功能以函数形式接入。
type featureFunc struct {
	name  string
	start func(context.Context, API) error
}

func (f featureFunc) Name() string { return f.name }

func (f featureFunc) Start(ctx context.Context, api API) error { return f.start(ctx, api) }

// NewFeature 用一个函数造一个功能，用于测试与快速试验。
func NewFeature(name string, start func(context.Context, API) error) Feature {
	return featureFunc{name: name, start: start}
}
