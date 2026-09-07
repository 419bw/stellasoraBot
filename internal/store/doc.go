// Package store 是存储基础设施：对上层只暴露 Doc 接口，不暴露任何引擎类型。
//
// 分层契约（本仓库的架构原则）：功能插件是「存不存、存什么、怎么取」的决定者，
// 基础设施只提供带命名空间的文档存储。因此这里没有 PutActivity / PutOverride
// 这类领域方法——领域结构体住在各自功能包里，本包只认字节。
//
// 命名空间（ns）由功能自选并自负其责：annsync 用 "sync"/"news"，calops 用
// "calops"，calexpiry 用 "calexpiry"。本包不认识任何 ns，也不为任何功能预建
// schema。代价要说清楚：值是 JSON 字节，没有编译期 schema 约束，功能改结构体
// 字段时要自己容错解码（Get 取不到或解不开就按零值 + 日志处理）。
package store

// Batch 是一次事务内的读写面。方法集与 Doc 的读写部分相同，
// 这样功能代码可以在事务内外用同一套写法。
type Batch interface {
	// Put 把 v 以 JSON 编码后写入 ns/key。
	Put(ns, key string, v any) error
	// Get 解码 ns/key 到 v；键不存在返回 false, nil。
	Get(ns, key string, v any) (bool, error)
	// Scan 按 key 的字节序遍历 ns 中以 prefix 开头的键；fn 返回非 nil 即中止并原样返回该错误。
	Scan(ns, prefix string, fn func(key string, raw []byte) error) error
	// Delete 删除 ns/key；键不存在不算错误。
	Delete(ns, key string) error
}

// Doc 是存储基础设施对功能的唯一暴露面。
type Doc interface {
	Batch

	// Batch 把 fn 内的所有写放进一个事务：fn 返回非 nil 则全部回滚。
	Batch(fn func(b Batch) error) error

	// Close 释放引擎资源。之后任何调用都返回错误。
	Close() error
}
