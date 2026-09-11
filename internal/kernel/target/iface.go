// Package target 是主动推送目标的存储与管理基础设施。
//
// 架构思考与伸缩性设计说明（为什么单机内存镜像 + 队列，而不是外部 Pub/Sub）：
//  1. 容量与开销：对于单机守护进程（如运行在 VPS 或手机 Termux 环境），即使管理上万个 QQ 群，
//     内存中维护目标 OpenID 列表仅占用 1~2 MB，遍历耗时在毫秒级。引入外部 MQ（如 Kafka/RabbitMQ/Redis）
//     会带来沉重的运维负担与内存底噪，违背轻量化部署的设计初衷。
//  2. 真正的瓶颈与流控：主动推送的物理瓶颈从来不是内存遍历，而是 QQ 开放平台的发信频控与出站 HTTP I/O。
//     本系统通过 downstream 的 kernel/queue（令牌桶限流、按群合并、退避重试与 Deadline 丢弃）实现流控缓冲。
//  3. 演进与解耦：View 与 Manager 接口严格隐藏了底层实现。若未来规模扩大至需要多节点集群，
//     只需将 Store 的底层对接 Redis/MQ，上层所有消费功能（calexpiry, calposter, pushops）无需改动一行代码。
package target

// View 是读面：给内核与主动推送功能查当前有哪些已启用的目标。
// 只查不改，满足分层契约。
type View interface {
	// Targets 返回当前所有启用的目标列表，按字典序排序保证稳定。
	Targets() []string
	// Has 判断某个目标是否已启用。
	Has(target string) bool
}

// Manager 是写面/管理面：给管理员命令用。
type Manager interface {
	View
	// Enable 启用某个目标并持久化。若已启用则幂等更新。
	Enable(target string) error
	// Disable 停用某个目标并从存储与内存中删除。返回值表示原先是否处于启用状态。
	Disable(target string) (bool, error)
	// List 返回当前所有启用的目标列表（与 Targets 同义，给管理端展示）。
	List() []string
}
