package aichat

import (
	"sync"
	"time"
)

// 冷却表的边界。数值照 qq/client.go 的 replier 那一份：两处判的都是"这条记录过了
// 这个点就没有意义了"。两小时的保留期远大于 15 秒的冷却，唯一目的是让这张表随时间
// 缩小而不是只增不减。
const (
	cooldownRetention    = 2 * time.Hour
	cooldownCleanupEvery = 10 * time.Minute

	// breakerHold 是"上游被判定永久失败"之后的静默时长。取 5 分钟有个硬依据：
	// 平台的被动回复窗口就是群 5 分钟，过了点那条消息本来也回不了，多等没有代价。
	breakerHold = 5 * time.Minute

	// bucketBurst 是全局桶的初始令牌与上限。
	// 推导：max_per_min=12 等于 5 秒一个，突发留 2 个刚好接住"两个人同时开口"，
	// 第三个必须等到下一个令牌。突发给大了就等于把一分钟的额度一瞬间打光、
	// 再接着一段静默（kernel/queue 里同样的取舍写在 DefaultPolicy 的注释上）。
	bucketBurst = 2
)

// limiter 装三样有界的记账：同人冷却、全局配额、永久错误的静默期。
// 三样都有过期机制——长期运行的进程里，任何只增不减的表就是最后被 OOM 杀掉的那个。
type limiter struct {
	mu   sync.Mutex
	cfg  Config
	last map[string]time.Time // 用户键 → 上次被放行的时刻
	// lastClean 与 last 同锁保护：清理挂在正常调用上，不另起定时器（全仓的缓存都这么活）。
	lastClean  time.Time
	bucket     bucket
	breakUntil time.Time
	breakWhy   string
}

func newLimiter(cfg Config) *limiter {
	return &limiter{
		cfg:    cfg,
		last:   make(map[string]time.Time),
		bucket: newBucket(float64(cfg.MaxPerMin), bucketBurst, cfg.Now()),
	}
}

// mark 判这个人现在能不能问。能就问，并把"现在"记成上次时刻；不能就返回还要等多久。
//
// 占位挂在放行那一刻而不是"上游答得好"那一刻：冷却要挡的是同一个人连刷，
// 上游正在抖的时候这层拦截恰好最省钱。
func (l *limiter) mark(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupLocked(now)
	if at, seen := l.last[key]; seen {
		if wait := l.cfg.Cooldown - now.Sub(at); wait > 0 {
			return wait, false
		}
	}
	l.last[key] = now
	return 0, true
}

// take 从全局桶里要一个令牌。要不到就返回 false，调用方静默丢弃这条。
func (l *limiter) take(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	next, ok, _ := l.bucket.tryTake(now)
	if ok {
		l.bucket = next
	}
	return ok
}

// breaker 返回静默期的截止时刻与原因。open 为 true 时这个窗口内都不该再问上游。
func (l *limiter) breaker(now time.Time) (until time.Time, why string, open bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.breakUntil.IsZero() || !now.Before(l.breakUntil) {
		return time.Time{}, "", false
	}
	return l.breakUntil, l.breakWhy, true
}

// trip 打开静默期。只有 permanentFailure 判成真的错误才走到这儿（凭据被拒、
// model 名写错），因为那类错误的定义就是"再问一次也不会不一样"。
func (l *limiter) trip(now time.Time, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.breakUntil = now.Add(breakerHold)
	l.breakWhy = why
}

// cleanupLocked 定期淘汰过期条目。挂在正常调用上做，不配后台循环——
// 这张表只有在有人说话时才会长，也就只在有人说话时需要缩。
func (l *limiter) cleanupLocked(now time.Time) {
	if !l.lastClean.IsZero() && now.Sub(l.lastClean) < cooldownCleanupEvery {
		return
	}
	cutoff := now.Add(-cooldownRetention)
	for key, at := range l.last {
		if at.Before(cutoff) {
			delete(l.last, key)
		}
	}
	l.lastClean = now
}

// bucket 是令牌桶。值语义：refilled/tryTake 返回"如果成功该如何更新"的候选值而
// 不改动自身，调用方决定要不要收下——这一条是从 kernel/queue/queue.go 逐字搬来的
// （那一份是包私有件，功能拿不到；搬算法不算造第二套机制）。
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
