# 星塔机器人 测试计划（Test Plan）

| | |
|---|---|
| 文档编号 | XT-TP-001 |
| 依据标准 | ISO/IEC/IEEE 29119-1（概念）· 29119-3（测试文档）· 29119-4（测试技术）；GB/T 25000.51-2016（系统与软件质量要求和评价，含性能效率准则）；GB/T 9386-2008（测试文档编制）；GB/T 15532-2008（单元测试规范） |
| 被测版本 | master @ `ce5ed98`（被动命令分道后） |
| 状态 | 已批准执行（单人项目：开发即测试负责人） |

## 1. 概述

### 1.1 目标
验证星塔机器人（QQ 官方机器人 + 游戏日历后端，Go 1.26/1.27）在**功能正确性、可靠性（并发安全/故障容忍）、性能效率（时间特性/资源利用/容量）**三个维度满足设计约束，并留下符合 29119 文档集的追溯记录。

### 1.2 范围
- **在范围内**：`internal/**` 全部业务与内核包、`cmd/xingtabot` 装配层与自动同步调度循环、`cmd/qqwatch` 工具、端到端链路（mock 平台→网关→Hub→command worker→业务）、性能与压力特性。
- **不在范围内**：QQ 平台侧行为（限流真值、网关稳定性）、浏览器渲染出图的真像素质量（人工抽检）、第三方依赖库内部（vendor）。

### 1.3 测试层级（29119-1 §4.2）
| 层级 | 载体 | 说明 |
|---|---|---|
| 单元 | 各包 `_test.go` 表驱动用例 | 与实现同包，含非导出函数 |
| 集成 | `internal/qq/cmd_test.go`、`feature/text` 全链、`devtools/loadtest` 端到端 | 真组件 + mock 平台 |
| 系统（准生产） | `qqsim` 双前端 mock 网关联调 + `.probe/bizload`、`.probe/posterload` | 完整装配路径；真机 `-mode=ws` 验收为遗留项 |

## 2. 质量特性与度量（GB/T 25000.51）

| 特性 | 度量 | 通过准则 |
|---|---|---|
| 功能适合性 | 用例通过率 | 100%（已知 flaky 见 §7 风险） |
| 可靠性·容错 | panic 注入存活；故障分级 | 相关用例全绿；`-race` 0 竞争 |
| 可靠性·成熟性 | 浸泡 30min goroutine 净增 | 稳态 < 基线抖动带（无单调泄漏趋势） |
| 性能·时间特性 | 投递/处理延迟分位 | 投递 p99 < 10ms（mock 平台）；命令处理随业务画像另行判定 |
| 性能·资源利用 | RSS、goroutine 数、句柄 | soak 无泄漏；worker 池恒定 1 |
| 性能·容量 | 持续吞吐拐点、队列边界行为 | 分级压力测出饱和点；队列满→丢弃可见（日志计数），不阻塞上游 |

## 3. 测试项与特性清单

| 特性号 | 特性 | 测试项（包/命令） |
|---|---|---|
| F01 | 被动命令回路（分派/门禁/回复/图片/截断） | `internal/qq`、`internal/command` |
| F02 | 日历查询命令（active/ending/upcoming/help） | `internal/feature/calposter` |
| F03 | 人工校准命令（review/override/confirm/hide/show） | `internal/feature/calexpiry` |
| F04 | 订阅开关命令（push on/off） | `internal/feature/calquery`、`internal/kernel/target` 订阅 |
| F05 | 日历出图与热缓存（fingerprint/singleflight/pending） | `internal/feature/calops`、`internal/devtools/artcache` |
| F06 | 出图文本渲染 | `internal/feature/text`、`internal/render` |
| F07 | 公告解析（窗口/告警/多主窗口送审） | `internal/render`（stellasora 解析器） |
| F08 | 到期提醒调度 | `internal/feature/biliwatch` |
| F09 | 公告自动同步调度（退避/节流/全量校准/账本） | `cmd/xingtabot`（syncloop） |
| F10 | 内核：日历域/队列/规则存储 | `internal/kernel/{calendar,queue,schedule}` |
| F11 | 内核：发送队列（合并/重试/配额/过期/溢出） | `internal/kernel/calendar`（queue） |
| F12 | 平台接入：token/限频/回复序列/媒体上传缓存 | `internal/kernel/target` |
| F13 | 平台接入：WS 网关心跳/重连/退避 | `internal/kernel/target`（gateway） |
| F14 | 事件中枢：解码/去重/多前端/签名验签 | `internal/kernel`（hub/dedup/webhook） |
| F15 | 存储契约（mem/bolt 行为一致） | `internal/kernel/store` + `storetest` 契约套件 |
| F16 | 装配与启动校验 | `cmd/xingtabot`（wiring）、`internal/qq`（startupcheck/Run） |
| F17 | 性能效率专项 | `internal/kernel/target` Bench*、`devtools/loadtest`、`.probe/*` |

## 4. 测试技术（29119-4）与导出项

- **黑盒**：功能分解法（F01-F16 逐项拆到命令×门禁×边界）、等价类+边界值（时间窗口、分页、参数缺失、上限 500 条、配额日切）、错误推测法（越权、恶意 openid 注路径、平台 200 伪错误、签名可延展性、缓存投毒——均有对应历史缺陷编号，见用例清单）。导出项：测试条件→测试用例（编号 `TC-Fxx-nn`）。
- **白盒**：语句覆盖（Go cover 原生粒度）。**技术限制声明**：Go 工具链不提供分支/MCDC 插桩，本项目以「未覆盖语句块」代理分支缺口分析，符合 29119-4 中"按工具能力选择可导出的结构覆盖"的要求。导出项：覆盖率分析报告。
- **非功能**：负载（阶梯速率）、压力（超限找拐点）、容量（队列边界）、稳定性浸泡（30min 连续 + 周期重连扰动），方法学按 GB/T 25000.51 §6 性能效率专项执行。

## 5. 环境

| 项 | 值 |
|---|---|
| 被测平台 | Windows 11 26200 x64（开发机）；功能一致性用 Linux 容器（golang:1.26-alpine / xingta-race:go1.26-1.27）复核 |
| 语言/工具 | Go 1.26.1（vendor 模式）；`-race` 仅容器可跑（Windows CGO 宿主桩损坏，见开发笔记坑 7） |
| 平台依赖 | 全部 mock：`devtools/qqsim` WS/回调网关、各包 fake sink/platform；**不触碰真实 QQ 平台** |
| 时钟注意 | Windows 定时器粒度 ~1ms，毫秒级分位数在 Windows 上无意义；容量/吞吐结论与 OS 无关 |

## 6. 进度、职责与通过总则

1. 阶段一：静态检查（build/vet）→ 单元/集成全量（含 `-count=1` 去缓存）→ race 全量 → 覆盖率测量与缺口分析；
2. 阶段二：缺口补用例（本轮新增，编号见总结报告附录 A），复跑；
3. 阶段三：性能专项（基线 Bench → loadtest 负载/压力 → 业务链路容量 → soak 浸泡）；
4. 阶段四：按 GB/T 9386 编制《测试总结报告》（XT-TR-001），含用例追溯矩阵、缺陷记录、符合性结论与残留风险声明。

通过总则：任何"失败"必须落为缺陷记录（编号/等级/状态）；未修复的已知风险（flaky、真机验收）在报告中显式声明，不得静默。

## 7. 已知风险

| 风险 | 处置 |
|---|---|
| `queue.Merge` 三用例偶发红（微秒赛跑，DEVELOPMENT-NOTES 备案） | 报告中按「已知非缺陷抖动」处理，重跑证据留档 |
| 真机 ws 心跳验收未做 | 记为遗留项，不阻塞本轮结论（mock 网关已覆盖协议行为） |
| Windows 无法本机跑 race | 容器覆盖，报告中附命令 |
