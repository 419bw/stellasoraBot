// Package schedule 按绝对时刻触发任务，支持幂等注册、原地改期与取消。
package schedule

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

type Task struct {
	ID string
	At time.Time
	// Fn 应当只做非阻塞的投递（例如往发送队列塞消息），耗时会拖住整个调度循环。
	Fn func(ctx context.Context) error

	heapIndex int
	seq       uint64
}

type Scheduler interface {
	// Schedule 按 ID 幂等注册：同 ID 已存在则原地改期，用于活动公告时间变更。
	Schedule(t Task)
	Cancel(id string) bool
	Pending() int
	Next() (time.Time, bool)
	Run(ctx context.Context) error
}

type taskHeap []*Task

func (h taskHeap) Len() int { return len(h) }

func (h taskHeap) Less(i, j int) bool {
	if !h[i].At.Equal(h[j].At) {
		return h[i].At.Before(h[j].At)
	}
	return h[i].seq < h[j].seq
}

func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIndex = i
	h[j].heapIndex = j
}

func (h *taskHeap) Push(x any) {
	t := x.(*Task)
	t.heapIndex = len(*h)
	*h = append(*h, t)
}

func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return t
}

// idleWake 是队列为空时的兜底唤醒间隔，避免零时长 Reset 导致空转。
const idleWake = time.Hour

type HeapScheduler struct {
	// OnError 非空时用于接收 Fn 返回的错误。
	OnError func(id string, err error)

	mu     sync.Mutex
	tasks  taskHeap
	byID   map[string]*Task
	seq    uint64
	clock  func() time.Time
	wakeCh chan struct{}
}

func New() *HeapScheduler {
	return &HeapScheduler{
		byID:   make(map[string]*Task),
		clock:  time.Now,
		wakeCh: make(chan struct{}, 1),
	}
}

func (s *HeapScheduler) Schedule(t Task) {
	if t.ID == "" {
		panic("schedule: 任务缺少 ID，无法幂等改期")
	}
	s.mu.Lock()
	existing, ok := s.byID[t.ID]
	if ok {
		existing.At = t.At
		existing.Fn = t.Fn
		heap.Fix(&s.tasks, existing.heapIndex)
	} else {
		s.seq++
		t.seq = s.seq
		heap.Push(&s.tasks, &t)
		s.byID[t.ID] = &t
	}
	s.mu.Unlock()
	s.notify()
}

func (s *HeapScheduler) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return false
	}
	heap.Remove(&s.tasks, t.heapIndex)
	delete(s.byID, id)
	return true
}

func (s *HeapScheduler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

func (s *HeapScheduler) Next() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) == 0 {
		return time.Time{}, false
	}
	return s.tasks[0].At, true
}

// Run 阻塞直到 ctx 取消。整个生命周期只有一个 goroutine 和一把 timer。
func (s *HeapScheduler) Run(ctx context.Context) error {
	timer := time.NewTimer(s.delay())
	// Go 1.23 起 timer channel 无缓冲且 1.27 移除了 asynctimerchan 逃生阀：
	// Stop 之后绝不能再去 drain，否则会在"已触发但无人接收"的状态上永久阻塞。
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-s.wakeCh:
			timer.Stop()
			timer.Reset(s.delay())

		case <-timer.C:
			s.runDue(ctx)
			timer.Reset(s.delay())
		}
	}
}

func (s *HeapScheduler) runDue(ctx context.Context) {
	now := s.clock()
	for {
		s.mu.Lock()
		if len(s.tasks) == 0 || s.tasks[0].At.After(now) {
			s.mu.Unlock()
			return
		}
		t := heap.Pop(&s.tasks).(*Task)
		delete(s.byID, t.ID)
		s.mu.Unlock()

		if err := t.Fn(ctx); err != nil && s.OnError != nil {
			s.OnError(t.ID, err)
		}
	}
}

func (s *HeapScheduler) delay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) == 0 {
		return idleWake
	}
	d := s.tasks[0].At.Sub(s.clock())
	if d < 0 {
		return 0
	}
	return d
}

func (s *HeapScheduler) notify() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

var _ Scheduler = (*HeapScheduler)(nil)
