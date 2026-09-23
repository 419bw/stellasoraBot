package kernel

import (
	"context"
	"sync"
	"testing"
	"time"

	"xingta/internal/kernel/calendar"
	"xingta/internal/kernel/queue"
	"xingta/internal/kernel/schedule"
	"xingta/internal/kernel/target"
)

// mgrTargets 是带订阅写面的目标视图：api.RegisterTopic/Topics 按 Manager 接口
// 探测，视图实现不实现它决定了功能能不能自己登记主题。
type mgrTargets struct {
	dummyTargetView
	mu     sync.Mutex
	topics []target.Topic
}

func (m *mgrTargets) RegisterTopic(t target.Topic) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, old := range m.topics {
		if old.Key == t.Key {
			return nil // 契约要求登记幂等
		}
	}
	m.topics = append(m.topics, t)
	return nil
}

func (m *mgrTargets) Topics() []target.Topic {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]target.Topic(nil), m.topics...)
}

// 其余 Manager 写面：本测试只走 RegisterTopic/Topics，剩下的按"已注册即可用"
// 的最小语义桩掉，保证 *mgrTargets 满足接口探测的前提是真的满足。
func (m *mgrTargets) Topic(key string) (target.Topic, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.topics {
		if t.Key == key {
			return t, true
		}
	}
	return target.Topic{}, false
}

func (m *mgrTargets) Enable(string) error                       { return nil }
func (m *mgrTargets) EnableTopic(string, string) error          { return nil }
func (m *mgrTargets) Disable(string) (bool, error)              { return false, nil }
func (m *mgrTargets) DisableTopic(string, string) (bool, error) { return false, nil }
func (m *mgrTargets) List() []string                            { return m.Targets() }

var _ target.Manager = (*mgrTargets)(nil)

// TestAPIFacadeDelegates 覆盖门面层四个此前只被生产接线摸过的口：
// Cancel（跨 api.Schedule/api.Scheduler 两个入口）、Calendar/Scheduler 的恒等透传、
// RegisterTopic/Topics 的 Manager 探测。门面写错一行，功能拿到的就是半个内核。
func TestAPIFacadeDelegates(t *testing.T) {
	mt := &mgrTargets{dummyTargetView: dummyTargetView{list: []string{"g:1"}}}
	r := NewRuntime(flakySink{sink: newLoadSink()}, queue.DefaultPolicy(), calendar.NewStore(), mt, nil)

	type facade struct {
		cancelViaSched bool
		cancelMissing  bool
		sameCalendar   bool
		sameScheduler  bool
		scheduleWins   bool
		registerTopic  error
		topicsAfterReg int
	}
	out := make(chan facade, 1)

	r.Register(NewFeature("facade-check", func(_ context.Context, api API) error {
		var f facade
		at := time.Now().Add(time.Hour)
		api.Schedule("soon", at, func(context.Context) error { return nil })
		// 两个入口指向同一个调度器：api.Schedule 排的，api.Scheduler().Cancel 得能取消
		f.cancelViaSched = api.Scheduler().Cancel("soon")
		f.cancelMissing = api.Cancel("never-scheduled")

		api.Scheduler().Schedule(schedule.Task{ID: "later", At: at, Fn: func(context.Context) error { return nil }})
		f.scheduleWins = api.Cancel("later")

		f.sameCalendar = api.Calendar() == r.Calendar()
		// Runtime 不 exported 调度器访问器（功能用不到"整个调度器对象"），
		// 恒等性直接对内部字段断言：门面转手的必须就是那一个堆。
		f.sameScheduler = api.Scheduler() == schedule.Scheduler(r.sched)

		f.registerTopic = api.RegisterTopic(target.Topic{Key: "facade", Name: "门面测试"})
		f.topicsAfterReg = len(api.Topics())
		out <- f
		return nil
	}))

	// targets 不是 Manager 的 Runtime：探测必须安静降级而不是 panic
	plain := NewRuntime(flakySink{sink: newLoadSink()}, queue.DefaultPolicy(), calendar.NewStore(),
		mt.dummyTargetView, nil)
	plainOut := make(chan int, 1)
	plain.Register(NewFeature("plain-check", func(_ context.Context, api API) error {
		plainOut <- len(api.Topics())
		if err := api.RegisterTopic(target.Topic{Key: "x"}); err != nil {
			t.Errorf("非 Manager 视图上 RegisterTopic 应安静跳过，got %v", err)
		}
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	go func() { _ = plain.Run(ctx) }()

	var f facade
	select {
	case f = <-out:
	case <-time.After(3 * time.Second):
		t.Fatal("门面检查没有跑完")
	}
	select {
	case n := <-plainOut:
		if n != 0 {
			t.Errorf("非 Manager 视图的 Topics() = %d 条，want 0", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("降级路径检查没有跑完")
	}

	if !f.cancelViaSched {
		t.Error("api.Scheduler().Cancel 没取消 api.Schedule 排下的任务：两个入口不是同一个调度器")
	}
	if f.cancelMissing {
		t.Error("Cancel 不存在的任务返回了 true")
	}
	if !f.scheduleWins {
		t.Error("api.Scheduler().Schedule 的任务取消不掉")
	}
	if !f.sameCalendar {
		t.Error("api.Calendar() 与 Runtime.Calendar() 不是同一个视图")
	}
	if !f.sameScheduler {
		t.Error("api.Scheduler() 与 Runtime.Scheduler() 不是同一个调度器")
	}
	if f.registerTopic != nil {
		t.Errorf("RegisterTopic: %v", f.registerTopic)
	}
	if f.topicsAfterReg != 1 {
		t.Errorf("注册后 Topics() = %d 条，want 1", f.topicsAfterReg)
	}
}
