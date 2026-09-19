# 星塔机器人 测试总结报告

| | |
|---|---|
| 文档编号 | XT-TR-001 |
| 上位文档 | XT-TP-001《测试计划》（docs/testing/test-plan.md） |
| 依据标准 | ISO/IEC/IEEE 29119-3（总结报告内容集）· 29119-4（技术）；GB/T 25000.51-2016（性能效率专项）；GB/T 9386-2008（文档编制）；GB/T 15532-2008（单元测试） |
| 被测版本 | master @ `ce5ed98` + 本轮测试用例补齐（工作区） |
| 执行日期 | 2026-09-19 |
| 结论 | **通过（带声明）** —— 见 §7 符合性结论与 §6 遗留项 |

## 1. 概述

按测试计划对全仓产品包执行了单元/集成/系统（准生产）三级测试与六类性能场景。总计 **360 个自动化测试函数（含本轮新增 13 个黑盒边界用例）全部通过**；Linux 容器 `-race` 全仓 0 数据竞争；业务包语句覆盖率最低线 74%、核心域 90%+。发现 **0 个产品缺陷**；测试活动自身暴露 3 个探针脚本缺陷（XT-IMP-01..03，不影响被测产品质量结论）。

## 2. 测试环境与执行方式

| 项 | 值 |
|---|---|
| 宿主 | Windows 11 26200 x64，20 核，Go 1.26.2（vendor） |
| Linux 复核 | Docker `xingta-race:go1.26-1.27`（golang:1.26 + 1.27 双工具链） |
| 平台依赖 | `devtools/qqsim` mock QQ 网关（WS+回调）与各包 fake 平台，未触碰真实 QQ 平台 |
| 已知测量限制 | Windows `time.Now` 步进 300–600µs，毫秒以下分位被抹平；RSS 在 Windows 不可读（HeapInUse 替代），Linux 容器补真实 RSS |

复现命令统一收录于附录 B。

## 3. 用例执行与追溯（29119-3 §条款）

用例即代码：全量用例清单（顶层测试函数名）可由 `go test ./... -list '.*'` 复现，原始导出存档 `scratch/tests-list.txt`；下表按源码文件精确统计（`func Test` 计数，含本轮新增；大量用例内部还有 t.Run 子用例，未再拆分计数）：

| 包 | 用例数 | 通过 | 本轮新增 |
|---|---|---|---|
| internal/qq（平台接入：client/token/gateway/hub/dedup/webhook/媒体缓存/签名） | 94（另 6 Bench） | 94 | +7（C2C 端点与转义/空 openid/单聊 seq 与 4 次上限/缺 msg_id；握手缺字段→400/坏包 ack/未知 op 静默） |
| internal/command + internal/annsync（命令机制、执行流、消息泵、同步器） | 59 | 59 | +5（Text 适配器全链 ×2；只读面 StatusReader/ReadRec/待确认分区/坏字节传播） |
| internal/kernel 系（runtime 门面/calendar/queue/schedule/target 订阅） | 50 | 50 | +1（API 门面透传与非 Manager 安静降级） |
| internal/store + storetest（契约套件 mem/bolt 双跑） | 4 | 4 | — |
| internal/feature/*（calposter/calexpiry/calops/biliwatch/calquery/pushops/text） | 89 | 89 | — |
| internal/render + internal/stellasora（公告解析/出图文本） | 48 | 48 | — |
| internal/devtools（loadtest/qqsim 自测） | 11 | 11 | — |
| cmd/xingtabot（装配与启动校验） | 5 | 5 | — |
| **合计** | **360** | **360** | **+13** |

**黑盒技术覆盖声明（29119-4）**：功能分解法逐特性拆到"命令 × 门禁(群角色/单聊白名单/C2COnly) × 参数边界"；等价类+边界值覆盖时间窗口、分页、1500 字符截断、回复上限 5/4、日配额日切、媒体缓存 TTL；错误推测法覆盖越权、openid 注路径、HTTP200 伪错误、签名可延展性、file_info 缓存投毒、panic 兜底——均有对应用例（历史缺陷编号见各测试注释）。

**白盒技术声明（29119-4 工具限制）**：Go 仅提供语句覆盖，未覆盖块作为分支缺口代理；MC/DC 在本工具链不可行，如实声明不虚构达成。

## 4. 覆盖率（本轮补测前后，语句覆盖·go test 口径）

| 包 | 补测前 | 补测后 | 包 | 补测前 | 补测后 |
|---|---|---|---|---|---|
| command | 90.2% | **96.7%** | kernel 门面 | 74.5% | **95.7%** |
| annsync | 68.8% | **87.2%** | calendar | 97.6% | 97.6% |
| qq | 84.7% | **86.6%** | queue | 94.5% | 94.9% |
| calposter | 81.6% | 81.7% | schedule | 96.2% | 96.2% |
| calquery | 89.8% | 89.8% | target(订阅) | 90.2% | 90.2% |
| calops | 83.6% | 85.2% | store | 86.0% | 86.0% |
| pushops | 83.6% | 83.6% | storetest | 76.0% | 76.0% |
| text | 100% | 100% | render | 81.9% | 81.9% |
| calexpiry | — | 93.2% | stellasora | 84.7% | 84.7% |
| biliwatch | — | 75.8% | loadtest(工具) | — | 70.5% |

补测收益集中在此前四个真实缺口：kernel 门面 +21.2、annsync 只读面 +18.4、command +6.5、qq 单聊/回调边界 +1.9。cmd/xingtabot 语句覆盖 19.0%（main 壳与 ws 拨号段占大头，其启动校验/组装纯函数由 5 个用例覆盖）——按 XT-TP-001 §4 排除声明不计入缺陷清单。剩余低覆盖为错误处理深枝与浏览器/网络壳，逐函数清单见 `scratch/cov2-func.txt`。

## 5. 性能效率测试（GB/T 25000.51 §6）

场景执行器 `.probe/quality`（S1–S5 Windows，基线+浸泡 Linux 复核）。

### 5.1 时间特性
| 指标 | Windows 实测 | Linux 容器复核 |
|---|---|---|
| 事件投递（网关→Hub→worker inbox，空处理，50k 条 @5000/s） | 吞吐 4989/s，p99=2.1ms / p999=6.2ms，零丢失 | 吞吐 5000/s，**p50=121µs / p99=437µs / p999=694µs**（时钟步进 62ns，分位可信） |
| 2ms 业务处理器（4k 条 @500/s） | 吞吐 415/s，排队 p50≈700ms（业务成本即吞吐墙） | 吞吐 491/s，排队 p50=65ms / p99=134ms |
| 慢命令(1s)在飞时 Hub 投递 400 条 | p99≈0（低于时钟步进） | **p50=2.7µs / p99=31µs** —— 与判死阈值 82.5s 差 6 个数量级，**被动分道达成** |

RSS/内存（Linux，浸泡后见 5.3）；`ClockQuantum` 告警在两平台均已随结果打印。

### 5.2 容量
| 场景 | 结果 |
|---|---|
| S2 分级压力（qqsim 网关闭环，8s/档 ×6） | 1k~16k/s 全档扛住（p99<2ms）；24000/s 档实测掉到 9043/s → **可持续 16000 msg/s，崩点 24000**（mock 环回测的是本机链路成本，非平台侧） |
| S3 命令 worker 串行吞吐 | 1ms 处理 → 2000 条 2.95s = **678 cmd/s**（含 Windows sleep 粒度税；纯机制上限见架构文档 21k/s 基线） |
| S4 被动 inbox 满丢 | 容量 128：投 300 → 受理 129（含在飞 1）+ 丢弃 171 = 300，**守恒精确**，每条丢弃有日志计数可观测 |
| S5 主动队列容量墙 | QueueSize=512 全速灌 3000 → **满拒 94**、受理 2906；8 群排空 14.9s（≈195 msg/s，sink 20ms/批），零丢弃零失败，排空后恢复受理（背压可逆） |
| 设计观察 | 调度器对**同群严格串行**（保消息顺序），排空速率 = 1/单批延迟 × 活跃群数 —— 记录为容量模型，非缺陷 |

### 5.3 资源利用与稳定性（浸泡）
| 指标 | 结果（Linux 容器，S6 30 轮 × 80k 事件 @4000/s ≈ 30 分钟 / 240 万事件） |
|---|---|
| goroutine | 全程区间 [8, 8]，**净增 0** |
| RSS | 147–157MB 横盘，无单调上升趋势 |
| HeapInUse | 108–136MB 有界抖动 |
| 端到端 p99 | 398–508µs 全程稳定（无退化） |
| 完整性 | dup=0、decodeFail=0、吞吐 30/30 轮满额 4000/s |
| 判定 | **通过**（结果文件 `scratch/quality-soak.txt`） |

## 6. 缺陷与改进记录

| 编号 | 等级 | 描述 | 状态 |
|---|---|---|---|
| XT-IMP-01 | 测试改进 | S3 探针未设 QueueSize 吃了默认 128 容量，把"吞吐"测成"满丢"——场景修正后达成（教训：容量语义要显式声明） | 已修正 |
| XT-IMP-02 | 测试改进 | S5 探针守恒方程漏计 backlog/在飞、且单群灌满没按"同群串行"设计留足排空时间 | 已修正 |
| XT-IMP-03 | 测试改进 | pwsh 对 `-flag=a,b` 拆逗号的坑二次踩中，测试命令规范固化为整体单引号 | 已固化 |
| XT-IMP-04 | 测试改进 | XT-KN-02 首版修复的守卫用例是单版本窗跑三轮，"当期被误删"的坏实现照样全绿——增长类守卫必须构造 ≥2 个版本键并断言旧键被删、当期跨轮热读保留；抄先例（calexpiry）要抄"拿谁的生死当判据"而非只抄删除动作 | 已固化（判据经探针实测定罪后改两版键，守卫转正为两用例） |
| XT-KN-01 | 已知缺口 | stellasora `dedupeEntries` 无"汇总类公告"样本可喂，白盒未闭合 | 备案，待真样本 |
| XT-KN-02 | 产品缺陷（低） | 浸泡后静态审计发现：calposter `shots`（每版本一张 PNG 字节）与 `ledger`（含磁盘账）按版本键只增不删，长期运行无界增长（~10-30MB/年+账堆积）。浸泡场景未驱动业务特性状态 map，属浸泡盲区，由审计补位 | 已修复：round 尾部 prune 至 Windows() 最新两个版本键——当期供「日历」命令热读、上期给调度器留重试余单（公告从不开闸后回改起点，≥3 键并活为死数据），内存+磁盘同删（对齐 calexpiry 纪律）；首版误用 pushGrace 宽限窗当判据，被真机探针实测定罪「当期出窗即误删、同桶重画」后修正；prune_test.go 6 用例（当期跨轮热读/三窗收敛/幂等/增长守卫）+ 变异复核 3 轮 |
| 产品缺陷 | — | **本轮测试执行期未发现**（360 用例全绿；race 0 竞争；压测守恒成立）；执行后审计另立 XT-KN-02，已修复 | — |

## 7. 符合性结论（29119-3）

1. **过程符合性**：按 XT-TP-001 计划执行了计划声明的全部测试项与技术；偏差（stellasora 数据缺口、真机验收遗留）均在本报告显式声明，符合 29119-3"偏差记录"要求。
2. **功能适合性**：特性 F01–F16 用例通过率 100%。
3. **可靠性**：panic 注入、平台故障分级、网关断连恢复、溢出守恒类用例全绿；Linux `-race` 全仓 27 包 **0 数据竞争、全部 ok**（结果文件 `scratch/race-full.txt`）。
4. **性能效率**：时间特性（空处理 p99≈0.4ms @24000/s 注入、4000/s 稳态 p99 399µs）、资源利用（浸泡 240 万事件 goroutine 净增 0、RSS 横盘）、容量（inbox 守恒精确、队列满拒语义+排空恢复）三维均有量化判定：**通过**（详见 §5.1–§5.4）。
5. **遗留项（不阻塞）**：真机 `-mode=ws` 心跳验收（XT-TP-001 §7）；queue 合并用例历史关注点已复核——5 连跑全部通过（`-count=5 ./internal/kernel/queue/`，0.19–0.21s/次）无抖动；`ce5ed98` 未 push。

## 附录 A：本轮新增用例编号映射

| 编号 | 特性 | 用例（Go 函数） | 结果 |
|---|---|---|---|
| TC-F01-21 | 命令回路 | TestTextAdapterRunsThroughWorker | 通过 |
| TC-F01-22 | 命令回路 | TestTextAdapterPassesThroughError | 通过 |
| TC-F01-07 | 命令门禁 | TestStatusReaderDelegatesToReadStatus（annsync 只读面） | 通过 |
| TC-F01-08 | 命令门禁 | TestReadRecAndPendingPartition | 通过 |
| TC-F01-09 | 命令门禁 | TestReadStatusPropagatesCorruptBytes | 通过 |
| TC-F12-31 | 目标发送 | TestSendC2CMessageUsesUserEndpoint（含 %20 转义防注入） | 通过 |
| TC-F12-32 | 目标发送 | TestSendC2CMessageRejectsEmptyOpenID | 通过 |
| TC-F12-33 | 目标发送 | TestSendC2CReplySequenceAndLimit（单聊 4 次上限+seq 序列） | 通过 |
| TC-F12-34 | 目标发送 | TestSendReplyRequiresMsgID | 通过 |
| TC-F14-09 | 事件中枢 | TestValidationRejectsIncompleteHandshake（4 种缺字段→400） | 通过 |
| TC-F14-10 | 事件中枢 | TestCallbackGarbageBodyAcksFailure（验签过但包坏→d=1） | 通过 |
| TC-F14-11 | 事件中枢 | TestCallbackUnknownOpIsSilent | 通过 |
| TC-F16-05 | 装配 | TestAPIFacadeDelegates（门面透传+非 Manager 安静降级） | 通过 |

## 附录 B：复现命令集

```powershell
# 功能回归（含本轮新增用例）
go build ./cmd/... ./internal/... ; go vet ./cmd/... ./internal/...
go test -count=1 ./cmd/... ./internal/...

# 覆盖率
go test -count=1 '-coverprofile=scratch/cov-all.out' ./cmd/... ./internal/...
go tool cover '-func=scratch/cov-all.out'

# 性能场景（Windows 侧，S1–S5；参数必须整体单引号防 pwsh 拆逗号）
go run ./.probe/quality '-s=all'
go run ./.probe/quality '-s=cmdflow,cmdcap,queuecap'

# race 全量 + Linux 基线/浸泡（容器；镜像内 go 在 /opt/g126/bin）
docker run --rm --entrypoint bash -v "j:/学习/项目/星塔机器人:/src" -w /src xingta-race:go1.26-1.27 -c "export PATH=/opt/g126/bin:/usr/local/bin:/usr/bin:/bin GOFLAGS=-mod=mod; go test -race -count=1 ./cmd/... ./internal/...; go run ./.probe/quality -s=base; go run ./.probe/quality -s=soak -soak-loops=30"
```
