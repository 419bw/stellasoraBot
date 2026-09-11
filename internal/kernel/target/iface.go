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

// Topic 是一个可供目标订阅的推送主题元数据（由功能层在 Start 阶段自声明注册）。
// 零业务词契约：本结构体仅保存标识键、显示名称与描述，不包含任何具体的业务枚举或业务逻辑。
type Topic struct {
	Key  string // 唯一英文标识键，如 "expiry", "poster", "bili"
	Name string // 显示名称，如 "活动到期提醒", "版本日历海报"
	Desc string // 简明用途说明
}

// View 是读面：给内核与主动推送功能查当前有哪些已启用的目标。
// 只查不改，满足分层契约。
type View interface {
	// Targets 返回当前所有启用了至少一个主题的目标列表，按字典序排序保证稳定。
	Targets() []string
	// TargetsFor 返回当前启用了指定主题的目标列表，按字典序排序保证稳定。
	TargetsFor(topic string) []string
	// Has 判断某个目标是否启用了任意推送主题。
	Has(target string) bool
	// HasTopic 判断某个目标是否启用了指定主题。
	HasTopic(target, topic string) bool
	// TopicsOf 返回目标当前已启用的主题 Key 列表。
	TopicsOf(target string) []string
}

// Manager 是写面/管理面：给管理员命令用。
type Manager interface {
	View
	// RegisterTopic 注册一个可用主题（幂等）。
	RegisterTopic(t Topic) error
	// Topics 返回当前所有已注册的主题列表。
	Topics() []Topic
	// Topic 查询指定 key 的主题元数据。
	Topic(key string) (Topic, bool)
	// Enable 启用目标并开启所有当前已注册的主题。
	Enable(target string) error
	// EnableTopic 启用目标的指定主题。
	EnableTopic(target, topic string) error
	// Disable 停用目标并清除其所有主题。返回值表示原先是否处于启用状态。
	Disable(target string) (bool, error)
	// DisableTopic 停用目标的指定主题。若停用后目标没有任何主题，则将目标彻底移除。
	DisableTopic(target, topic string) (bool, error)
	// List 返回当前所有启用的目标列表（与 Targets 同义，给管理端展示）。
	List() []string
}
