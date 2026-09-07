package qq_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	. "xingta/internal/qq"
)

// fakeClock 让时间窗口测试不用真的等 5 分钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestDeduper() (*Deduper, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	return NewDeduper(DedupConfig{Now: c.Now}), c
}

func TestDedupSuppressesDuplicateDelivery(t *testing.T) {
	d, _ := newTestDeduper()

	if !d.See(EventGroupAtMessage, "m1") {
		t.Fatal("第一次见到应当放行")
	}
	if d.See(EventGroupAtMessage, "m1") {
		t.Fatal("第二次是 resume 补发的重复，应当挡下")
	}
	if d.See(EventGroupAtMessage, "m1") {
		t.Fatal("第三次同样应当挡下")
	}
	s := d.Stats()
	if s.Admitted != 1 || s.Dropped != 2 || s.Live != 1 {
		t.Errorf("Stats = %+v，想要 admitted=1 dropped=2 live=1", s)
	}
}

func TestDedupEventTypeIsPartOfTheKey(t *testing.T) {
	// 负对照：键里少了事件类型，同 id 的不同事件就会被互相吞掉。
	d, _ := newTestDeduper()
	id := "ROBOT1.0_same"

	if !d.See(EventGroupAtMessage, id) {
		t.Fatal("群消息首次到达应放行")
	}
	if !d.See(EventC2CMessage, id) {
		t.Fatal("同一个 id 的单聊消息不该被群消息的记录吞掉")
	}
	if !d.See(EventGroupMessage, id) {
		t.Fatal("同一个 id 的群全量消息不该被吞掉")
	}
	if d.See(EventGroupAtMessage, id) {
		t.Error("真正的重复仍须挡下")
	}
}

func TestDedupWindowBoundary(t *testing.T) {
	// 窗口从配置给，不去改结构体私有字段：黑盒测试不该依赖实现细节可变。
	clock := &fakeClock{t: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	d := NewDeduper(DedupConfig{Window: 5 * time.Minute, Now: clock.Now})

	if !d.See(EventC2CMessage, "m1") {
		t.Fatal("首次应放行")
	}
	clock.Add(5*time.Minute - time.Millisecond)
	if d.See(EventC2CMessage, "m1") {
		t.Error("窗口内 1ms 之差仍应判为重复")
	}
	clock.Add(2 * time.Millisecond)
	if !d.See(EventC2CMessage, "m1") {
		t.Error("超出窗口后应重新放行")
	}
	// 放行后必须重新计时，否则紧接着的下一条重复会漏。
	if d.See(EventC2CMessage, "m1") {
		t.Error("重新放行后应立即进入新的窗口")
	}
}

func TestDedupCapacityIsBoundedAndEvictsOldest(t *testing.T) {
	const limit = 128
	d := NewDeduper(DedupConfig{MaxKeys: limit, Now: time.Now})

	for i := 0; i < limit*2; i++ {
		d.See(EventGroupAtMessage, fmt.Sprintf("m%d", i))
	}
	s := d.Stats()
	if s.Live != limit {
		t.Errorf("表内键数 = %d，容量上限 %d 没生效", s.Live, limit)
	}
	if s.Evicted != limit {
		t.Errorf("Evicted = %d，想要 %d", s.Evicted, limit)
	}
	// 最新的键必须仍在表内（否则环形槽位算错了）。
	if d.See(EventGroupAtMessage, fmt.Sprintf("m%d", limit*2-1)) {
		t.Error("最新的键已被误淘汰")
	}
	// 淘汰是有界内存的代价：最老的键会被放回来。这条断言钉住「我们知道并接受这个行为」。
	if !d.See(EventGroupAtMessage, "m0") {
		t.Error("最老的键本应已被淘汰并重新放行")
	}
}

func TestDedupKeepsSlotOnWindowReentry(t *testing.T) {
	// 窗口过期后重新放行不能占用新槽位，否则反复过期的同一批键会把表撑爆。
	const limit = 8
	clock := &fakeClock{t: time.Unix(0, 0)}
	d := NewDeduper(DedupConfig{MaxKeys: limit, Window: time.Minute, Now: clock.Now})

	for i := 0; i < 5; i++ {
		clock.Add(2 * time.Minute)
		for j := 0; j < limit; j++ {
			d.See(EventGroupAtMessage, fmt.Sprintf("m%d", j))
		}
	}
	if s := d.Stats(); s.Live != limit {
		t.Errorf("反复过期同一批键后 Live = %d，想要保持 %d", s.Live, limit)
	}
}

func TestDedupIgnoresEventsWithoutID(t *testing.T) {
	d, _ := newTestDeduper()
	for i := 0; i < 3; i++ {
		if !d.See(EventReady, "") {
			t.Fatalf("没有 id 的事件第 %d 次被挡了，会把 READY/RESUMED 全吞掉", i+1)
		}
	}
	if s := d.Stats(); s.Live != 0 {
		t.Errorf("无 id 事件不该进表，Live = %d", s.Live)
	}
}

func TestDedupSingleAdmissionUnderConcurrency(t *testing.T) {
	// 这条是给 -race 用的：多个前端同时推同一条消息，只能有一个处理器跑到。
	d, _ := newTestDeduper()

	const g = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if d.See(EventGroupAtMessage, "hot") {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted != 1 {
		t.Errorf("%d 个并发里放行 %d 次，想要恰好 1 次", g, admitted)
	}
	if s := d.Stats(); s.Dropped != uint64(g-1) {
		t.Errorf("Stats.Dropped = %d，想要 %d", s.Dropped, g-1)
	}
}

func TestDedupConcurrentDistinctKeysStayBounded(t *testing.T) {
	d := NewDeduper(DedupConfig{MaxKeys: 256, Now: time.Now})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				d.See(EventGroupAtMessage, fmt.Sprintf("w%d-m%d", w, i))
			}
		}(w)
	}
	wg.Wait()
	if s := d.Stats(); s.Live > 256 {
		t.Errorf("并发写入后 Live = %d，超出容量 256", s.Live)
	}
}

// 默认配置不能用私有字段来验，改成验行为：窗口必须生效（否则重复会放行）、
// 时钟必须有兜底（否则调用即 panic）、容量必须有上界（否则长跑会 OOM）。
// 具体数值不是契约，"有界且非零"才是。
func TestDefaultDedupConfigIsUsableAndBounded(t *testing.T) {
	d := NewDeduper(DedupConfig{})

	if !d.See(EventGroupAtMessage, "a") {
		t.Fatal("首次应放行")
	}
	if d.See(EventGroupAtMessage, "a") {
		t.Error("紧接着的同 id 被放行：默认窗口为 0 或者时钟没兜底")
	}
	for i := 0; i < 50000; i++ {
		d.See(EventGroupAtMessage, fmt.Sprintf("k%d", i))
	}
	if s := d.Stats(); s.Live > 20000 {
		t.Errorf("灌 5 万个不同 id 后表内仍有 %d 条，默认容量上限没起作用（长跑必 OOM）", s.Live)
	}
}

// 默认窗口的值本身也得有牙。TestDedupWindowBoundary 显式传了 Window，永远走不到
// NewDeduper 里那条兜底分支 —— 实测把默认值从 5 分钟改成 1 小时，全仓零测试变红。
// 所以这里刻意只给 Now 不给 Window，从两侧把默认值夹死。
func TestDefaultDedupWindowIsFiveMinutes(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	d := NewDeduper(DedupConfig{Now: clock.Now})

	if !d.See(EventC2CMessage, "w1") {
		t.Fatal("首次应放行")
	}
	clock.Add(5*time.Minute - time.Millisecond)
	if d.See(EventC2CMessage, "w1") {
		t.Error("默认窗口短于 5 分钟：差 1ms 就算过期，resume 补发的余量不够")
	}
	clock.Add(2 * time.Millisecond)
	if !d.See(EventC2CMessage, "w1") {
		t.Error("默认窗口长于 5 分钟：早该放行的新消息仍被当成重复挡掉")
	}
}

func TestIsMessageEvent(t *testing.T) {
	for _, kind := range []string{EventGroupAtMessage, EventGroupMessage, EventC2CMessage} {
		if !IsMessageEvent(kind) {
			t.Errorf("%s 应被认作消息事件", kind)
		}
	}
	// 这些都不是消息事件：带 id 与否都不该走解码+去重路径。
	for _, kind := range []string{EventReady, EventResumed, "GUILD_MESSAGE_CREATE", "FUTURE_THING", ""} {
		if IsMessageEvent(kind) {
			t.Errorf("%q 不该被认作消息事件", kind)
		}
	}
}

func TestDedupForgetLetsRedeliveryThrough(t *testing.T) {
	d, _ := newTestDeduper()

	if !d.See(EventGroupAtMessage, "m1") {
		t.Fatal("首次应放行")
	}
	if d.See(EventGroupAtMessage, "m1") {
		t.Fatal("窗口内第二次应被判重")
	}
	d.Forget(EventGroupAtMessage, "m1")
	if !d.See(EventGroupAtMessage, "m1") {
		t.Fatal("Forget 之后重投必须能重新处理，否则平台的重试通道被我们自己堵死")
	}
	if s := d.Stats(); s.Forgotten != 1 {
		t.Errorf("Forgotten = %d，想要 1", s.Forgotten)
	}

	// 幂等与边界：不存在的键、空 id 都不能 panic 或错计。
	d.Forget(EventGroupAtMessage, "never-seen")
	d.Forget(EventGroupAtMessage, "")
	if s := d.Stats(); s.Forgotten != 1 {
		t.Errorf("无效 Forget 不该计数: %+v", s)
	}
}

func TestDedupForgetKeepsEvictAccountingHonest(t *testing.T) {
	// Forget 会在环形槽位上留下悬空引用。绕回来时如果不区分「槽位上的键还在不在表里」，
	// Evicted 就会虚高 —— 而压测报告正是拿这个计数判断「容量够不够」的。
	const limit = 4
	d := NewDeduper(DedupConfig{MaxKeys: limit, Now: time.Now})

	for i := 0; i < limit; i++ {
		d.See(EventGroupAtMessage, fmt.Sprintf("k%d", i))
	}
	d.Forget(EventGroupAtMessage, "k3") // 腾出 slot3，环形槽位同时清空
	// 再灌 5 条：new0..new2 各顶掉一个在用的 k（3 次淘汰），
	// new3 落在已清空的 slot3 上（不该算淘汰），new4 绕回 slot0 顶掉 new0（第 4 次）。
	for i := 0; i < 5; i++ {
		d.See(EventGroupAtMessage, fmt.Sprintf("new%d", i))
	}
	s := d.Stats()
	if s.Evicted != 4 {
		t.Errorf("Evicted = %d，想要 4（悬空槽位不该被算成淘汰）", s.Evicted)
	}
	if s.Live > limit {
		t.Errorf("Live = %d 超出容量 %d", s.Live, limit)
	}
	// 最后灌进去的必须还活着（能被判重），否则说明有在用的键被误删。
	if d.See(EventGroupAtMessage, "new4") {
		t.Error("最近登记的键被误淘汰了")
	}
}
