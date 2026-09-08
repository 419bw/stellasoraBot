// Package queue 按平台配额把待发消息投递给 Sink，负责限流、合并、退避与丢弃。
package queue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrQueueFull = errors.New("queue: 发送队列已满")

var ErrExpired = errors.New("queue: 消息超过投递截止时间")

// Media 表示"这一条要发的不是文字，而是一个待解析的非文字载荷"。
//
// 队列只搬运一个引用，不解释它：Kind 由 Sink 自己认领，Key 是不透明标识。
// 这里刻意不放已经上传好的 file_info——平台给的 file_info ttl 只有几分钟
// （文档响应示例 ttl=300）且不能跨场景复用，排队加退避一过期就必发不出去，
// 所以上传与生成都推迟到 Sink 真要发的那一刻。
type Media struct {
	Kind string // 取值由接线处与 Sink 约定，队列不枚举
	Key  string // 该 Kind 内的不透明键
}

type Item struct {
	ID     string
	Target string // group_openid / user_openid
	Text   string
	// Media 非空表示发这个而不是 Text；此时必须 Mergeable=false。
	Media *Media
	// Topic 相同且 Mergeable 的消息会合并成一条发送，用于"多个活动同时结束"。
	Topic     string
	Mergeable bool
}

type Batch struct {
	Target string
	Text   string
	Media  *Media
	Items  []*Item
}

type Sink interface {
	Send(ctx context.Context, b *Batch) error
}

type Policy struct {
	TargetPerMin float64
	TargetBurst  float64
	GlobalPerMin float64
	GlobalBurst  float64
	TargetPerDay int
	DayZone      *time.Location

	MergeWindow time.Duration
	Deadline    time.Duration
	MaxAttempts int
	BackoffBase time.Duration
	BackoffMul  int

	Workers   int
	QueueSize int
	// MaxPendingPerTarget 限制单个群的积压，避免日额度耗尽后内存无界增长。
	MaxPendingPerTarget int
}

// DefaultPolicy 的配额直接抄平台文档：单群主动消息 20/qpm、Bot 维度 60/qpm、每群每天 1000 条。
// 突发额度刻意留小，免得一瞬间把一分钟的额度打光后紧跟一段静默。
func DefaultPolicy() Policy {
	return Policy{
		TargetPerMin: 20, TargetBurst: 4,
		GlobalPerMin: 60, GlobalBurst: 8,
		TargetPerDay: 1000,
		DayZone:      fixedCST(),
		MergeWindow:  1500 * time.Millisecond,
		Deadline:     10 * time.Minute,
		MaxAttempts:  4,
		BackoffBase:  time.Second,
		BackoffMul:   3,
		Workers:      2,
		QueueSize:    4096,

		MaxPendingPerTarget: 200,
	}
}

// fixedCST 用固定偏移而不是 Local：手机端的本地时区和时区数据库都不可信。
func fixedCST() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
}

type Stats struct {
	Submitted      int
	Rejected       int
	Sent           int
	Batches        int
	Merged         int
	DroppedExpired int
	DroppedFailed  int
	DroppedOverflw int
	Retries        int
	DailyBlocked   int
}

type itemState struct {
	item     Item
	readyAt  time.Time
	deadline time.Time
	attempts int
}

// bucket 是令牌桶。tryTake 返回"若成功该如何更新"的候选值而不改动自身，
// 这样多个维度的配额可以全部通过后再一次性提交，不会白扣。
type bucket struct {
	tokens float64
	cap    float64
	rate   float64 // 每秒补充
	last   time.Time
}

func newBucket(perMin, burst float64, now time.Time) bucket {
	return bucket{tokens: burst, cap: burst, rate: perMin / 60, last: now}
}

func (b bucket) refilled(now time.Time) bucket {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.cap, b.tokens+elapsed.Seconds()*b.rate)
		b.last = now
	}
	return b
}

func (b bucket) tryTake(now time.Time) (next bucket, ok bool, wait time.Duration) {
	b = b.refilled(now)
	if b.tokens >= 1 {
		b.tokens--
		return b, true, 0
	}
	if b.rate <= 0 {
		return b, false, time.Hour
	}
	return b, false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

type dayCount struct {
	day string
	n   int
}

// job 连同 itemState 一起交给 worker：重试计数存在 itemState 上。
// 只传 *Batch 会让 worker 找不到原条目（早已被弹出）、重建 attempts 归零的副本，
// 于是永久失败的消息无限重试。
type job struct {
	batch  *Batch
	states []*itemState
}

type Dispatcher struct {
	p         Policy
	sink      Sink
	retryable func(error) bool
	OnError   func(Item, error)

	in   chan Item
	wake chan struct{}

	mu      sync.Mutex
	pending map[string][]*itemState
	global  bucket
	targets map[string]*bucket
	days    map[string]*dayCount
	// served 记录每个目标上次被服务的时间，用于跨群公平轮转，避免队头群吃光配额。
	served map[string]time.Time
	stats  Stats
	active int // 已交给 worker、尚未定成败的条目数
	clock  func() time.Time
}

func New(sink Sink, p Policy) *Dispatcher {
	if p.Workers <= 0 {
		p.Workers = 1
	}
	if p.QueueSize <= 0 {
		p.QueueSize = 256
	}
	if p.MaxPendingPerTarget <= 0 {
		p.MaxPendingPerTarget = 200
	}
	if p.BackoffMul < 1 {
		p.BackoffMul = 2
	}
	if p.DayZone == nil {
		p.DayZone = fixedCST()
	}
	return &Dispatcher{
		p:         p,
		sink:      sink,
		retryable: func(error) bool { return true },
		in:        make(chan Item, p.QueueSize),
		wake:      make(chan struct{}, 1),
		pending:   make(map[string][]*itemState),
		targets:   make(map[string]*bucket),
		days:      make(map[string]*dayCount),
		served:    make(map[string]time.Time),
		clock:     time.Now,
	}
}

// SetRetryClassifier 注入错误分类。等确认了"用户关闭主动消息"的错误码，
// 就在这里判成不可重试，免得对一条注定失败的消息反复退避重刷。
func (d *Dispatcher) SetRetryClassifier(f func(error) bool) {
	if f == nil {
		f = func(error) bool { return true }
	}
	d.retryable = f
}

// Submit 非阻塞投递，队列满时返回 ErrQueueFull 交由调用方重排。
func (d *Dispatcher) Submit(item Item) error {
	d.mu.Lock()
	d.stats.Submitted++
	d.mu.Unlock()

	select {
	case d.in <- item:
		d.notify()
		return nil
	default:
		d.mu.Lock()
		d.stats.Rejected++
		d.mu.Unlock()
		return ErrQueueFull
	}
}

func (d *Dispatcher) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats
}

// InFlight 返回已经离开排队状态、但还没定成败的条目数（含仍在 in 通道里排队的部分）。
func (d *Dispatcher) InFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active + len(d.in)
}

// Backlog 返回内存中排队的条目数（不含尚未从 in 通道取走的部分）。
func (d *Dispatcher) Backlog() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, q := range d.pending {
		n += len(q)
	}
	return n
}

func (d *Dispatcher) Run(ctx context.Context) error {
	jobs := make(chan *job, d.p.Workers)
	var wg sync.WaitGroup
	for i := 0; i < d.p.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.worker(ctx, jobs)
		}()
	}
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item := <-d.in:
			d.enqueue(item)
		case <-d.wake:
		case <-timer.C:
		}
		timer.Stop()
		timer.Reset(d.pump(jobs))
	}
}

func (d *Dispatcher) enqueue(item Item) {
	now := d.clock()
	d.mu.Lock()
	defer d.mu.Unlock()

	q := d.pending[item.Target]
	if len(q) >= d.p.MaxPendingPerTarget {
		// 丢最老的，保证新提醒还能发出去
		d.pending[item.Target] = q[1:]
		d.stats.DroppedOverflw++
		q = d.pending[item.Target]
	}
	ready := now
	if canMerge(item) {
		ready = now.Add(d.p.MergeWindow) // 等同伴，凑齐再合成一条
	}
	d.pending[item.Target] = append(q, &itemState{
		item:     item,
		readyAt:  ready,
		deadline: now.Add(d.p.Deadline),
	})
}

// pump 尽可能把到期消息交给 worker，返回下一次应当醒来的间隔。
// 锁内只做决策，交付与 OnError 一律放到解锁之后 —— 回调可能反过来访问 Dispatcher。
func (d *Dispatcher) pump(jobs chan<- *job) time.Duration {
	for {
		now := d.clock()
		var (
			wait    time.Duration
			expired []Item
			out     *job
		)

		d.mu.Lock()
		expired = d.sweepExpiredLocked(now)
		target, st := d.nextReadyLocked(now)
		switch {
		case st == nil:
			wait = minDuration(d.nextFutureReadyLocked(now), d.nextDeadlineLocked(now))

		case len(jobs) == cap(jobs):
			wait = 2 * time.Millisecond

		default:
			gnext, gok, gwait := d.globalBucketLocked(now).tryTake(now)
			tnext, tok, twait := d.targetBucketLocked(target, now).tryTake(now)
			switch {
			case !gok || !tok:
				wait = minDuration(maxDuration(gwait, twait), d.nextDeadlineLocked(now))
			case d.dailyLimitReachedLocked(target, now):
				wait = minDuration(d.nextDayBoundaryLocked(now), d.nextDeadlineLocked(now))
			default:
				d.global = gnext
				d.targets[target] = &tnext
				d.served[target] = now
				batch, states := d.popBatchLocked(target, now)
				d.active += len(states)
				out = &job{batch: batch, states: states}
			}
		}
		d.mu.Unlock()

		for _, it := range expired {
			if d.OnError != nil {
				d.OnError(it, ErrExpired)
			}
		}
		if out == nil {
			return clampDelay(wait)
		}
		jobs <- out
	}
}

// nextReadyLocked 在队头已到期的目标里挑"最久没被服务"的那个。
// 之前按 readyAt 挑，先灌进来的群会长期占住队头把后面的群饿死。
func (d *Dispatcher) nextReadyLocked(now time.Time) (string, *itemState) {
	bestTarget := ""
	var best *itemState
	var bestServed time.Time
	for target, q := range d.pending {
		if len(q) == 0 || q[0].readyAt.After(now) {
			continue
		}
		last := d.served[target] // 从未服务过的目标排最前
		switch {
		case best == nil:
		case last.Before(bestServed):
		case last.Equal(bestServed) && q[0].readyAt.Before(best.readyAt):
		default:
			continue
		}
		bestTarget, best, bestServed = target, q[0], last
	}
	return bestTarget, best
}

func (d *Dispatcher) nextFutureReadyLocked(now time.Time) time.Duration {
	wait := time.Hour
	for _, q := range d.pending {
		if len(q) == 0 {
			continue
		}
		if d := q[0].readyAt.Sub(now); d < wait {
			wait = d
		}
	}
	return wait
}

// nextDeadlineLocked 返回距最近一条消息截止时间的间隔，保证过期清理准点发生，
// 不会因为被限流长时间休眠而让无效消息占着内存。
func (d *Dispatcher) nextDeadlineLocked(now time.Time) time.Duration {
	wait := time.Hour
	for _, q := range d.pending {
		for _, it := range q {
			if left := it.deadline.Sub(now); left < wait {
				wait = left
			}
		}
	}
	return wait
}

// popBatchLocked 弹出队头；可合并时把同 Topic 且已到期的邻居一起带走。
func (d *Dispatcher) popBatchLocked(target string, now time.Time) (*Batch, []*itemState) {
	q := d.pending[target]
	head := q[0]

	picked := []*itemState{head}
	kept := q[1:]
	if canMerge(head.item) {
		rest := kept[:0]
		for _, it := range kept {
			if canMerge(it.item) && it.item.Topic == head.item.Topic && !it.readyAt.After(now) {
				picked = append(picked, it)
				continue
			}
			rest = append(rest, it)
		}
		kept = rest
	}
	if len(kept) == 0 {
		delete(d.pending, target)
	} else {
		d.pending[target] = kept
	}

	texts := make([]string, 0, len(picked))
	items := make([]*Item, 0, len(picked))
	for _, it := range picked {
		texts = append(texts, it.item.Text)
		items = append(items, &it.item)
	}
	if len(picked) > 1 {
		d.stats.Merged += len(picked) - 1
	}
	return &Batch{Target: target, Text: strings.Join(texts, "\n"), Media: head.item.Media, Items: items}, picked
}

// canMerge 是"这条能不能跟同伴合成一条文字消息"。图不参与合并：两张图拼不成一条消息，
// 而被吸收进文字批次的那条只会留下空 Text，图就这么静默丢了。
func canMerge(it Item) bool { return it.Mergeable && it.Media == nil }

func (d *Dispatcher) globalBucketLocked(now time.Time) bucket {
	if d.global.rate == 0 {
		d.global = newBucket(d.p.GlobalPerMin, d.p.GlobalBurst, now)
	}
	return d.global
}

func (d *Dispatcher) targetBucketLocked(target string, now time.Time) bucket {
	if b, ok := d.targets[target]; ok {
		return *b
	}
	b := newBucket(d.p.TargetPerMin, d.p.TargetBurst, now)
	d.targets[target] = &b
	return b
}

// dailyLimitReachedLocked 检查并占用当日额度，返回 true 表示已到顶应挡下；TargetPerDay<=0 表示不设日上限。
func (d *Dispatcher) dailyLimitReachedLocked(target string, now time.Time) bool {
	if d.p.TargetPerDay <= 0 {
		return false
	}
	day := now.In(d.p.DayZone).Format("2006-01-02")
	dc := d.days[target]
	if dc == nil || dc.day != day {
		dc = &dayCount{day: day}
		d.days[target] = dc
	}
	if dc.n >= d.p.TargetPerDay {
		d.stats.DailyBlocked++
		return true
	}
	dc.n++
	return false
}

func (d *Dispatcher) nextDayBoundaryLocked(now time.Time) time.Duration {
	local := now.In(d.p.DayZone)
	next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, d.p.DayZone)
	return next.Sub(now)
}

func (d *Dispatcher) sweepExpiredLocked(now time.Time) []Item {
	var expired []Item
	for target, q := range d.pending {
		kept := q[:0]
		for _, it := range q {
			if it.deadline.Before(now) {
				d.stats.DroppedExpired++
				expired = append(expired, it.item)
				continue
			}
			kept = append(kept, it)
		}
		if len(kept) == 0 {
			delete(d.pending, target)
			continue
		}
		d.pending[target] = kept
	}
	return expired
}

func (d *Dispatcher) worker(ctx context.Context, jobs <-chan *job) {
	for j := range jobs {
		err := d.sink.Send(ctx, j.batch)
		d.mu.Lock()
		d.active -= len(j.states)
		if err == nil {
			d.stats.Sent += len(j.batch.Items)
			d.stats.Batches++
		}
		d.mu.Unlock()

		if err != nil {
			d.requeue(j, err)
		}
		d.notify()
	}
}

func (d *Dispatcher) requeue(j *job, cause error) {
	now := d.clock()

	type failure struct {
		item  Item
		cause error
	}
	var failures []failure

	d.mu.Lock()
	for _, st := range j.states {
		st.attempts++
		if st.attempts >= d.p.MaxAttempts || !d.retryable(cause) {
			d.stats.DroppedFailed++
			failures = append(failures, failure{st.item, cause})
			continue
		}
		// 截止时间不因重试顺延：过了点就该丢，而不是无限期赖在队列里
		st.readyAt = now.Add(d.p.BackoffBase * time.Duration(powInt(d.p.BackoffMul, st.attempts-1)))
		d.pending[st.item.Target] = append(d.pending[st.item.Target], st)
		d.stats.Retries++
	}
	d.mu.Unlock()

	// 回调可能在用户代码里再访问 Dispatcher，必须在锁外执行
	for _, f := range failures {
		if d.OnError != nil {
			d.OnError(f.item, f.cause)
		}
	}
	d.notify()
}

func (d *Dispatcher) notify() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func clampDelay(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d > time.Hour:
		return time.Hour
	case d < time.Millisecond:
		return time.Millisecond
	default:
		return d
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func powInt(base, exp int) int {
	out := 1
	for i := 0; i < exp; i++ {
		out *= base
	}
	return out
}
