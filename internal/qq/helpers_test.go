package qq_test

import "time"

// 这几个是从包内测试里带出来的小工具。刻意保持和原来同名同行为，
// 这样搬过来的断言不用逐行改写 —— 改写 1600 行断言本身就是新的出错来源。

// waitFor 轮询直到 cond 为真，返回是否等到。
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// head 截断长串用于日志与报错，openid/id 这类不透明串太长没法读。
func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
