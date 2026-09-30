// 白盒文件：冷却表会不会缩、令牌桶怎么补，只有内部看得见，而"只增不减的表"正是
// 全仓点名的最终被 OOM 杀掉那种。本包其余测试都在黑盒文件里。
package aichat

import (
	"fmt"
	"testing"
	"time"
)

type testClock struct{ at time.Time }

func (c *testClock) now() time.Time { return c.at }

func (c *testClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func TestLimiterShrinksTheCooldownTable(t *testing.T) {
	ck := &testClock{at: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	l := newLimiter(Config{Cooldown: 15 * time.Second, MaxPerMin: 600, Now: ck.now})

	for i := 0; i < 300; i++ {
		if _, ok := l.mark(fmt.Sprintf("g|%d", i), ck.now()); !ok {
			t.Fatalf("第 %d 个人被冷却挡了：不同键之间不该互相影响", i)
		}
	}
	if len(l.last) != 300 {
		t.Fatalf("表里 %d 条，want 300", len(l.last))
	}

	// 清理挂在正常调用上（不配后台循环）：推过保留窗口后，下一次调用就该把过期的扫掉。
	ck.advance(cooldownRetention + cooldownCleanupEvery)
	if _, ok := l.mark("g|new-comer", ck.now()); !ok {
		t.Fatal("新键被挡了")
	}
	if len(l.last) != 1 {
		t.Errorf("过期条目没被扫掉，表里还留 %d 条（无界增长就是在这儿长出来的）", len(l.last))
	}
}

// 令牌桶的三层账：突发只给 2 个（两个人同时开口接住，第三个必须等）、
// 按 max_per_min 的速度补、以及**闲置多久也不会攒超过突发位** ——
// 最后这条是"攒一分钟后一口气打光"那个事故形状防线。
func TestLimiterBucketBurstAndRefill(t *testing.T) {
	ck := &testClock{at: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	l := newLimiter(Config{Cooldown: 0, MaxPerMin: 12, Now: ck.now}) // 12/min = 5 秒一个

	for i := 0; i < 2; i++ {
		if !l.take(ck.now()) {
			t.Fatalf("突发位 %d 都没给满，第 %d 次就被拒了", bucketBurst, i+1)
		}
	}
	if l.take(ck.now()) {
		t.Error("同一瞬间放行了第 3 条，突发位没起作用")
	}

	ck.advance(5 * time.Second)
	if !l.take(ck.now()) {
		t.Error("过了 5 秒（正好一个令牌）还没放行")
	}

	ck.advance(10 * time.Minute)
	if !l.take(ck.now()) || !l.take(ck.now()) {
		t.Error("闲置 10 分钟后没补到突发位的令牌")
	}
	if l.take(ck.now()) {
		t.Errorf("攒出了超过突发位 %d 的令牌（一分钟额度一口气打光就是这么来的）", bucketBurst)
	}
}

// 熔断窗口取 5 分钟是有硬依据的：平台的被动回复窗口就是群 5 分钟，
// 过了点那条消息本来也回不了。这一条钉住它不会因为改动而悄悄变长。
func TestLimiterBreakerHoldsExactlyTheWindow(t *testing.T) {
	ck := &testClock{at: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	l := newLimiter(Config{Cooldown: 0, MaxPerMin: 60, Now: ck.now})

	if _, _, open := l.breaker(ck.now()); open {
		t.Fatal("没出过事就算熔断开着")
	}
	l.trip(ck.now(), "HTTP 401 凭据被拒")

	ck.advance(breakerHold - time.Second)
	if until, why, open := l.breaker(ck.now()); !open {
		t.Error("静默期没过就恢复了")
	} else if until.Sub(ck.now()) > time.Second+time.Millisecond || why == "" {
		t.Errorf("静默期剩余 %v 或原因 %q 不合预期", until.Sub(ck.now()), why)
	}

	ck.advance(2 * time.Second)
	if _, _, open := l.breaker(ck.now()); open {
		t.Error("过了 breakerHold 还在静默，上游修好了也不会再问")
	}
}
