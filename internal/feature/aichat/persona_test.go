// 白盒文件：静态层的上限只有包内够得着（personaStatic 与 personaRuneLimit 都是
// 非导出的），而这条用例是那个常量唯一的引用点——没有它，"改上限"和"改一份没人
// 读的注释"就是同一件事。
package aichat

import "testing"

// persona.md 是 go:embed 进来的运行时资产，往里加黑话字典、角色外号不需要动 Go 代码，
// 所以超限的唯一预报点就是这条用例。它红的时候先别调上限：先算 token 账，
// 静态层每涨一个 rune，每次请求都多付一个 rune，而动态事实段的预算是被挤的那一方。
func TestPersonaStaysUnderRuneLimit(t *testing.T) {
	n := len([]rune(personaStatic))
	if n > personaRuneLimit {
		t.Errorf("persona.md 已 %d rune，超过上限 %d：静态层会挤掉当期事实的预算，要么删内容要么连带重算上界", n, personaRuneLimit)
	}
	t.Logf("aichat: 静态层 %d rune / 上限 %d", n, personaRuneLimit)
}
