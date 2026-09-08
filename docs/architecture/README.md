# 星塔机器人 架构设计

## 定位

QQ 群机器人，从《星塔旅人》中文官网抓活动公告，解析成时间区间，提供 @命令查询与到期提醒。
部署目标 Termux (Android ARM64)，开发环境 Windows + WSL + Docker。

## 包结构与依赖方向

```
cmd/                      ← 唯一的组装点
  xingtabot/main.go       ← 生产入口：把下面所有包接线
  qqsim/main.go           ← 本地网关模拟器 + 压测驾驶舱
  qqprobe/main.go         ← 真连调试工具（脱敏输出）
  qqwatch/main.go         ← 真连事件采集

internal/
  qq/                     ← QQ 平台薄客户端（token/签名/HTTP/WS/去重/Hub）
  store/                  ← bbolt Doc 接口 + 实现 + memDoc 测试替身
  kernel/                 ← 可插拔 Feature 运行时
    calendar/             ← 内存时间索引（二分查询，View/Writer 拆分）
    queue/                ← 发送队列（多维限流、合并、退避重试）
    schedule/             ← 最小堆调度器（动态改期/取消）
  command/                ← 命令机制层（注册表 + dispatch + 被动回复铁律）
  annsync/                ← 通用公告同步引擎（列目录→抓详情→投影→日历）
  stellasora/             ← 中文网 Source（HTTP 客户端 + 规则 v3 解析器 + 跨条合并）
  feature/                ← 业务功能
    calquery/             ← 查活动（活动/快结束/即将/帮助）
    calops/               ← 运维操作（待确认/覆盖/确认/隐藏/显示）
    calexpiry/            ← 到期提醒（定时扫描 + 主动推送）
    text/                 ← 文本工具
  devtools/               ← 开发辅助（qqsim payload 生成器 + loadtest 框架）
```

依赖方向严格单向：`cmd → feature → command/annsync → kernel → qq/store`。
功能之间不互相 import，通过 kernel.API 和共享接口交互。

## 分层契约

**基础设施只暴露接口，不暴露具体类型。**

| 基础设施 | 暴露接口 | 消费者 |
|----------|---------|--------|
| store | `Doc` (Get/Put/Scan/Delete/Batch) | annsync, calops, calexpiry |
| calendar | `View` (Active/EndingSoon/StartingSoon/All) | calquery |
| calendar | `Writer` (BulkUpsert/Upsert/Remove) | annsync |
| kernel | `API` (Submit/Schedule/Cancel/Calendar) | Feature.Start() 参数 |
| command | `Registrar` (Add) | calquery, calops |
| stellasora | `Source` (Name/List/Fetch) | annsync |

关掉一个功能 = 删掉 main.go 里对应那行 `rt.Register(...)`。它的命令、定时任务、命名空间写入一起停。

## 数据流

```
stellasora.yostar.cn
       ↓ HTTP (300ms MinGap)
annsync.round()
  ├─ src.List() → []Ref (分页列表)
  ├─ src.Fetch(ref) → Item (详情 + Parse 结果)
  ├─ saveItem() → bbolt "news/<src>:<id>"
  ├─ buildRecs() → []Rec
  ├─ saveRecs() → bbolt "activity/<src>:<id>:<n>"
  └─ reproject() → calendar.Store (内存)
       ↓
calquery.active() / ending() / upcoming()
       ↓ calendar.View 二分查询
格式化 → command.dispatch → SendGroupReply
       ↓
QQ 平台 (api.bot.qq.com)
```

## QQ 平台硬约束

- **openid 体系**：群内成员用 `member_openid`，单聊用 `user_openid`；每个 bot 对同一用户的 openid 不同。
- **被动回复上限**：群 5 次 / 单聊 4 次 per msg_id。超限直接 403。设计铁律：一条命令 = 一条回复。
- **主动消息配额**：群每用户每天有限条。发送队列 (`kernel/queue`) 负责节流和合并。
- **WS 有 seq 补发**：断线重连后平台会补发历史事件，可能重复 → **必须 msg_id 去重**。
- **重连分 resume/identify 两段**：resume 带 session_id + last_seq；失败后走 identify 重新握手。
- **心跳 ack 判死**：连续 N 次心跳无 ACK → 判定连接死亡，断开重连。
- **Go 1.27 硬约束**：bbolt v1.3.11 用了 `unsafe.Slice` 不兼容 1.26 以下；Go 1.27 起 `crypto/rand.Read` 签名变了。
- **ed25519 签名**：HTTP 请求头带 `X-Tts-Signature`，格式 `Ed25519 <timestamp>;random(<6digits>);sign(<base64>)`。

## 解析器规则 v3 (stellasora/parse.go)

从公告 HTML 正文提取活动名、时间区间与条目。公告先分三类（`Provenance`）：

| 类别 | 判据 | 产出 |
|------|------|------|
| solo 独立公告 | 标题有 `「…」` 且恰好一个主窗口 | 1 条事件，海报=公告封面 |
| version 版本公告 | 标题匹配 `^\[…\](活动\|版本)一览$` | 版本边界（`Result.Ver`，不进日历）+ **1 条版本主活动事件**（活动一览就是主活动自己的公告，海报=版本主视觉）；整图"版本一览"零产出 |
| summary 汇总公告 | 主窗+附窗 ≥2 且切出 ≥1 个具名条目 | N 条事件（维护更新说明里列的档期，无海报） |

规则要点：

1. **活动名** = 标题第一对 `「…」`；正文散文里的 `「」` 不参与命名。汇总条目名取条目行
   （`-` 开头），`限时招募活动「X」` 一行可产多池共用同一窗口；`✦■◆●` 是分组行。
2. **窗口** = `intervalRe` 行；行内前缀（`活动时间：`）优先于所在 `▌小节标题`。
3. **附属窗口**（领取/结算/兑换/售卖…）不产事件；其中领取/兑换行作为**领奖窗**
   （`Event/Rec.ClaimStart/ClaimEnd`）随事件带出，供展示层画领奖尾段（查询与提醒
   口径不使用）；售卖/结算/补偿丢弃。汇总条目的「兑换」行挂 `Entry.Redeem`，版本
   公告的兑换行随版本主活动事件带出。
4. **跨条合并** (`merge.go`)：规范化同名 + 窗口 IoU ≥0.8 视为同一条，优先级
   solo > version > summary，被删侧引用记入 `TwinRef`。同名不同窗（复玩模式）不并。
5. **模糊起始**：`relAnchor = [^\s~～]{0,12}后`（"维护结束后"等）→ 估当天 00:00 + fuzzy_start。
6. **常驻**（"维护结束后常驻"）有意不跟踪；倒挂窗口标 pending 交「待确认」。
7. **NBSP 归一化**：Go `\s` 不匹配 U+00A0，必须先替换为普通空格。
8. 待确认（`Suspect`）：汇总切分失败 / 版本公告有正文档期却抽不出边界 / 有名字零窗口
   且正文像有档期（日期多半在图片里）。

规则全文与决策记录：`docs/plan/parse-v3-maintenance-split.md`。

## 时间表渲染（连续时间模型）

排版核对用的网页/PNG 是离线复算：读 `.probe/calgen/body/` 正文缓存，走与机器人相同的生产函数
（Parse+BuildItem+Merge），再叠渲染变换。库里 `Rec` 与查询/提醒口径一概不受渲染影响。

- **一条带 = 真实起止时刻**，在 `[g0,g1]` 线性映射上绝对定位；日格只是背景标尺，不再吸附。
  自然日/游戏日开关只改日格边界与日期标签取整，不改条带位置。
- **fuzzy 起点**：库内存当天 00:00 下界（"维护结束后"），渲染按 `OPEN=17:00` 开闸估计摆放；
  窗口判定左缘 `w0 = act0 + OPEN`。全量 184 条里 00:00 起点 100% 带 fuzzy 标记，零例外。
- **可见性只剩一条判据**：玩法段与本窗 `[w0,g1)` 有交集才画。于是"维护前收档"的活动连同它的
  兑换尾都不会带进新版本视图——原先靠 cut 形状启发 + 回看过滤两条特判，二者已删。
- **装箱按 ms 首-fit**：前一条 10:59 收、后一条 17:00 开，中间天然有空隙 → 同排首尾相接，
  不再各占一行（同名周期玩法的两期因此无需任何特判）。
- 渲染件全在 `.probe/`（不入库）：模板 `.probe/calpng/template.html`，产物
  `.probe/timetable/web/{data.json,calendar.html}`；`.probe/calgen` 的 Go 渲染器冻结在旧天格模型。

## 并发模型

- **hub.Handle**：单 goroutine（WS reader 串行），所有事件处理都在这一个 goroutine 上。
- **calendar.Store**：`sync.RWMutex` 保护。写侧只有 annsync.loop 一个 goroutine；读侧命令 dispatch 共享。
- **queue.Dispatcher**：独立 goroutine 消费 + 定时 flush。
- **annsync.loop**：独立 goroutine，周期性抓取 + 投影。
- **calexpiry**：依赖 schedule 定时触发，走 queue 发送。

实测（本机 i7-13700H, Go 1.26）：
| 路径 | 吞吐 | p50 | p99 | HeapInUse |
|------|------|-----|-----|-----------|
| WS→decode→dedup→noop handler | 101,243 msg/s | 65µs | 636µs | 85MB |
| 业务 dispatch 单线程 | 6,963 cmd/s | <1µs | 1.5ms | 5MB |
| 业务 dispatch 20 并发 | 17,537 cmd/s | 1ms | 3.3ms | 8MB |

结论：瓶颈在平台 HTTP 往返（~100-200ms/reply），不在本地处理。

## 测试策略

| 类型 | 工具 | 跑在哪 |
|------|------|--------|
| 并发正确性 | `go test -race ./internal/...` | Docker (xingta-race 镜像) |
| 覆盖率 | 同上 `-coverprofile` | 同上 |
| 基础设施压测 | `cmd/qqsim load/sweep` | Docker / Termux |
| 业务路径压测 | `.probe/bizload/main.go` | 本机（开发用） |
| 真连端到端 | `cmd/xingtabot -creds` | 本机 |
| 解析器回归 | `internal/stellasora/parse_test.go` (变异表) | 任何 |
| 时间表版式回归 | `.probe/calpng/check.js`（4 模式 × 3 页签 + 定点断言） | 本机 node |
| 冷启动验证 | `.probe/livesync/main.go` | 本机 |

## 构建与运行

```bash
# 开发（本机 Windows）
go run ./cmd/xingtabot -db data/xingta-live.db -scan 1m

# 带提醒发送
go run ./cmd/xingtabot -db data/xingta-live.db -scan 1m -remind g:<group_openid> -lead 80h

# 容器压测（从 WSL）
bash scripts/container-verify.sh

# 业务路径压测
go run .probe/bizload/main.go -db data/xingta-live.db -total 10000 -workers 1

# 查看同步状态
go run .probe/check-db.go
```

## 关键设计决策与注意点

1. **不做"进行中活动"API 查询**：官网没有这个接口。`type=activity` 是联动周边不是游戏活动。全量抓 + 本地投影是唯一可行路径。
2. **凭据只在 .probe/creds.json**：已 .gitignore 排除。AppSecret/access_token 不出现在任何输出。
3. **bbolt 不 mkdir**：`openDB()` 先 `os.MkdirAll` 再开库。data/ 在干净检出里不存在。
4. **command 注册在 Feature.Start()**：不是 New()。启动日志里 `命令:` 为空是正常的（打印在 rt.Run 之前）。
5. **被动回复不降级为主动消息**：触上限只记日志，绝不绕过。
6. **时区假设**：官网日期默认 +08:00，无法从文本验证。若官方用 JST 则全部偏 1 小时。
7. **空壳文件陷阱**：rm -rf 后"对照记录还原"会产生大小正确内容全零的文件。判恢复要读内容，受害集合正好是 .gitignore 排除的目录。

## 仓库

`git@github.com:419bw/stellasoraBot.git`，remote 名 `github`，主分支 `master`。
