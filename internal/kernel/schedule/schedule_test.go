package schedule

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("超时未满足条件: %s", what)
}

func TestFiresAtDeadlineWithBoundedJitter(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	var fired atomic.Int64
	want := time.Now().Add(120 * time.Millisecond)
	s.Schedule(Task{ID: "a", At: want, Fn: func(context.Context) error {
		fired.Store(time.Now().UnixNano())
		return nil
	}})

	waitFor(t, func() bool { return fired.Load() != 0 }, 2*time.Second, "任务未触发")
	delta := time.Duration(fired.Load()-want.UnixNano()) * time.Nanosecond
	if delta < -20*time.Millisecond || delta > 300*time.Millisecond {
		t.Errorf("触发偏差 = %v, 期望落在 [-20ms, 300ms]", delta)
	}
	if s.Pending() != 0 {
		t.Errorf("触发后仍残留 %d 个任务", s.Pending())
	}
}

// 改期是高频操作：公告时间变了，旧的那个触发点必须作废，且总共只触发一次。
func TestRescheduleReplacesEarlierDeadline(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	var mu sync.Mutex
	var hits []time.Time
	late := time.Now().Add(2 * time.Second)
	soon := time.Now().Add(80 * time.Millisecond)

	s.Schedule(Task{ID: "act", At: late, Fn: func(context.Context) error {
		mu.Lock()
		hits = append(hits, time.Now())
		mu.Unlock()
		return nil
	}})
	s.Schedule(Task{ID: "act", At: soon, Fn: func(context.Context) error {
		mu.Lock()
		hits = append(hits, time.Now())
		mu.Unlock()
		return nil
	}})

	if got := s.Pending(); got != 1 {
		t.Fatalf("同 ID 重复注册后任务数 = %d, want 1", got)
	}
	if next, _ := s.Next(); next.After(soon.Add(time.Second)) {
		t.Errorf("改期未生效，Next 仍是 %v", next)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(hits) > 0
	}, 2*time.Second, "改期后的任务未触发")

	// 早的那次触发之后，晚的时间点必须还在等待窗口之外；这里再等一段确认没有第二次
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 {
		t.Errorf("触发了 %d 次, want 1（改期没把旧触发点作废）", len(hits))
	}
}

func TestCancelPreventsFire(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	var fired atomic.Bool
	s.Schedule(Task{ID: "gone", At: time.Now().Add(50 * time.Millisecond), Fn: func(context.Context) error {
		fired.Store(true)
		return nil
	}})
	if !s.Cancel("gone") {
		t.Fatal("Cancel 返回 false，任务没找到")
	}
	if s.Cancel("gone") {
		t.Error("重复 Cancel 应返回 false")
	}
	time.Sleep(250 * time.Millisecond)
	if fired.Load() {
		t.Error("已取消的任务仍然触发了")
	}
}

// 插入一个比当前 timer 更早的任务，必须立刻重排，而不是等原 timer 到期。
func TestEarlierTaskWakesLoopImmediately(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Schedule(Task{ID: "far", At: time.Now().Add(30 * time.Second), Fn: func(context.Context) error { return nil }})

	start := time.Now()
	done := make(chan time.Duration, 1)
	s.Schedule(Task{ID: "near", At: time.Now().Add(60 * time.Millisecond), Fn: func(context.Context) error {
		done <- time.Since(start)
		return nil
	}})

	select {
	case elapsed := <-done:
		if elapsed > 500*time.Millisecond {
			t.Errorf("新任务等了 %v 才触发，唤醒机制失效", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("新任务未触发")
	}
}

func TestBurstNoDuplicateNoLoss(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	const n = 200
	var mu sync.Mutex
	seen := make(map[string]int, n)
	base := time.Now()
	for i := 0; i < n; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		s.Schedule(Task{ID: id, At: base.Add(time.Duration(i%40) * time.Millisecond), Fn: func(_ context.Context) error {
			mu.Lock()
			seen[id]++
			mu.Unlock()
			return nil
		}})
	}

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == n
	}, 3*time.Second, "部分任务未触发")
	mu.Lock()
	defer mu.Unlock()
	for id, c := range seen {
		if c != 1 {
			t.Errorf("任务 %s 触发了 %d 次, want 1", id, c)
		}
	}
}

func TestRunReturnsOnContextCancel(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	s.Schedule(Task{ID: "x", At: time.Now().Add(time.Hour), Fn: func(context.Context) error { return nil }})
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run 返回 %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消 ctx 后 Run 没有退出")
	}
}

// OnError 是 Fn 失败的唯一出口：任务到点即从堆里弹出删除，错误无人接就是静默丢失。
// 成功路径绝不回调；失败任务不残留，补投是调用方用同 ID 幂等重排的事。
func TestOnErrorReceivesFnError(t *testing.T) {
	s := New()
	var mu sync.Mutex
	var gotID string
	var gotErr error
	var calls int
	s.OnError = func(id string, err error) {
		mu.Lock()
		defer mu.Unlock()
		gotID, gotErr, calls = id, err, calls+1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Schedule(Task{ID: "boom", At: time.Now().Add(time.Millisecond), Fn: func(context.Context) error {
		return errors.New("任务自己的错误")
	}})
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 1
	}, 2*time.Second, "Fn 错误未回调 OnError")
	mu.Lock()
	if gotID != "boom" {
		t.Errorf("OnError 收到任务 %q, want \"boom\"", gotID)
	}
	if gotErr == nil || gotErr.Error() != "任务自己的错误" {
		t.Errorf("OnError 收到错误 %v, want \"任务自己的错误\"", gotErr)
	}
	mu.Unlock()
	if s.Pending() != 0 {
		t.Errorf("失败后任务仍残留 %d 个", s.Pending())
	}

	// 成功任务不回调
	s.Schedule(Task{ID: "ok", At: time.Now().Add(time.Millisecond), Fn: func(context.Context) error { return nil }})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("成功任务也触发了 OnError：共 %d 次", calls)
	}
}
