# 星塔机器人 架构设计

## 定位

QQ 群机器人，从《星塔旅人》中文官网抓活动公告并监控官方 B站动态，提供活动日历图、B站动态长图、@命令查询与到期提醒。
部署目标 Termux (Android ARM64)，开发环境 Windows + WSL + Docker。

## 包结构与依赖方向

```
cmd/                      ← 唯一的组装点
  xingtabot/main.go       ← 生产入口：把下面所有包接线并装配 activeSink 多媒体提供者
  qqsim/main.go           ← 本地网关模拟器 + 压测驾驶舱
  qqprobe/main.go         ← 真连调试工具（脱敏输出）
  qqwatch/main.go         ← 真连事件采集
  calshot/main.go         ← 本地离线海报出图测试工具

internal/
  qq/                     ← QQ 平台薄客户端（token/签名/HTTP/WS/去重/Hub/MediaCache）
  store/                  ← bbolt Doc 接口 + 实现 + memDoc 测试替身
  render/                 ← 通用无头浏览器驱动（视口测量、无头 Chromium 截图，零业务词）
  kernel/                 ← 可插拔 Feature 运行时
    calendar/             ← 内存时间索引（二分查询，View/Writer 拆分）
    target/               ← 推送目标管理（持久化 + 内存镜像，主题 Topic 自声明与订阅）
    queue/                ← 发送队列（多维限流、合并、退避重试、主动消息投递）
    schedule/             ← 最小堆调度器（动态改期/取消）
  command/                ← 命令机制层（注册表 + dispatch + 被动回复铁律）
  annsync/                ← 通用公告同步引擎（列目录→抓详情→投影→日历）
  stellasora/             ← 中文网 Source（HTTP 客户端 + 规则 v3 解析器 + 跨条合并）
  feature/                ← 业务功能（功能自治，功能间零互相 import）
    calposter/            ← 版本活动日历海报（完全自治：内嵌专属 template.html、dataset 装配、出图、预热与推图）
    biliwatch/            ← B站官方动态监听（完全自治：内嵌专属 template.html、WBI 签名、CookieJar、单图槽位缓存与 Singleflight）
    calquery/             ← 查活动（活动/快结束/即将/帮助）
    calops/               ← 运维操作（待确认/覆盖/确认/隐藏/显示）
    pushops/              ← 推送设置（push on/off [expiry|poster|bili] 控制群推送开关）
    calexpiry/            ← 到期提醒（定时扫描 + 主动推送）
    text/                 ← 文本工具
  devtools/               ← 开发辅助（qqsim payload 生成器 + loadtest 框架）
```

依赖方向严格单向：`cmd → feature → command/annsync → kernel → qq/store/render`。
功能之间不互相 import，通过 `kernel.API` 和共享接口交互。

## 分层契约

**基础设施只暴露接口，不暴露具体类型。**

| 基础设施 | 暴露接口 | 消费者 |
|----------|---------|--------|
| store | `Doc` (Get/Put/Scan/Delete/Batch) | annsync, calops, calexpiry, target, calposter, biliwatch |
| calendar | `View` (Active/EndingSoon/StartingSoon/All) | calquery |
| calendar | `Writer` (BulkUpsert/Upsert/Remove) | annsync |
| target | `View` (Targets/TargetsFor/Has) | kernel.API (calexpiry, calposter, biliwatch) |
| target | `Manager` (Enable/EnableTopic/Disable/DisableTopic/Topics) | pushops |
| kernel | `API` (Submit/Schedule/Cancel/Calendar/Targets/TargetsFor/RegisterTopic/Topics) | Feature.Start() 参数 |
| command | `Registrar` (Add)、`Text(fn)` 适配 | calquery, calops, calposter, pushops |
| stellasora | `Source` (Name/List/Fetch) | annsync |
| render | `Capturer` (`Capture(page, route) ([]byte, error)`) | calposter, biliwatch |

**基础设施的名字里不许出现业务词。** 队列搬图不叫 `PosterSpec{VersionKey}`，叫 `Media{Kind, Key}`——`Kind`
的取值由接线的 Sink 认领（如 `"poster"`、`"bili"`），队列不枚举；也不带平台给的 `file_info`（ttl 只有几分钟，排队加退避一过期必发不出去），
只带"该发哪一张"的引用，上传与生成推到 Sink 真要发的那一刻。同理，`annsync` 不认识 `"version"` 这个源自述的
取值：版本列表由功能自己 `ReadRecs` 后过滤 `Provenance`，那个字面量在 `main.go` 从 `stellasora.ProvVersion` 传进去。
`render` 只做纯粹的无头浏览器驱动：测量尺寸、把一页 HTML 截成 PNG。它不认识日历也不认识 B站动态，更不回答"该发哪一张"——
海报模板内嵌在 `internal/feature/calposter/template.html`，动态模板内嵌在 `internal/feature/biliwatch/template.html`。
业务何时出图、出哪张图、缓存怎么管、发过没有，全部下沉在各自的功能包内完全自治。

关掉一个功能 = 删掉 `main.go` 里对应那行 `rt.Register(...)`。它的命令、定时任务、命名空间写入一起停。

### 推送目标管理与多主题订阅（target 包）
- **按主题独立订阅**：群管理员通过 `push on/off [topic]` 可以独立订阅不同业务模块的推送（如 `expiry` 到期提醒、`poster` 版本日历海报、`bili` B站动态）。
- **主题自声明机制**：各功能在 `Start(ctx, api)` 时通过 `api.RegisterTopic` 自行声明业务主题（Key、名称、说明），`pushops` 动态拉取主题列表展示，彻底解除功能与管理层硬编码耦合。
- **容量与开销**：单机维护上万个群/私聊目标（`g:...`、`u:...`）在 Go 内存中仅耗费约 1MB 内存，切片遍历投递耗时仅数毫秒。
- **物理瓶颈在平台**：主动推送的真正瓶颈从来不是内存遍历，而是 QQ 官方开放平台的出站发信频控与网络 I/O。本架构下游通过 `internal/kernel/queue` 的令牌桶流控、单群合并、退避重试与 Deadline 超时丢弃来平抑流量峰值。

## 数据流

```
1. 官网公告同步链：
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
格式化 → command.dispatch → SendGroupReply → QQ 平台 (api.bot.qq.com)

2. 版本日历海报链（calposter）：
annsync 投影出的 []Rec（ReadRecs，功能自己按 Provenance 过滤版本窗口）
       ↓ 每 -warm 一轮（启动立刻先跑一轮）
calposter.warmArt → 只补本窗缺的海报字节：每张先问公告里的原地址，被拒才换 OSS 传输加速
       ↓            端点补一次；失败的 URL 进 10 分钟冷却，同一 URL 并发只抓一次
calposter.Image(key)   ← 「日历」命令走这条：只读本地字节，零网络，缺海报就画占位。
calposter.Fetch(key)   ← 发送队列的 Sink 在 worker 上走这条：允许现抓，配合 Singleflight 单飞合并
       ↓ Build 数据集 → calposter.Page → render.Browser.Capture → PNG
「日历」命令回 command.Reply{Image}；版本开闸只投 queue.Item{Media{poster,key}}——
调度回调绝不出图，图在真要发的那一刻由 Sink 现取；账本是 Sink 发送成功后回调 MarkPushed 写的。

3. B站官方动态监听链（biliwatch）：
api.bilibili.com
       ↓ 每 -bili-interval (默认 3 分钟一轮，启动时先跑一轮建立基线)
biliwatch.round()
  ├─ fetcher.FetchLatest() (访客 CookieJar + WBI 混淆签名签名 space/feed)
  ├─ 发现未推送新动态 (按时间倒序遍历，确保旧动态先入队、聊天流时序正确)
  └─ api.Submit(queue.Item{Media: {Kind: "bili", Key: dynID}, Topic: "bili"})
       ↓
activeSink (发送侧 worker 出队):
  ├─ biliwatch.Fetch(dynID)
  │    ├─ 单图槽位缓存 latestPNG 命中？→ 0ms 内存秒出
  │    └─ 未命中 → Singleflight 汇聚并发请求 → 浏览器仅渲染一次 → 覆盖单槽缓存
  ├─ qq.UploadGroupImage (分片上传获取 file_info)
  └─ qq.SendGroupMessage → 成功回调 biliwatch.MarkPushed 记录落盘账本

4. 主动消息与出图分发（activeSink）：
queue.Dispatcher (多群消息出队)
       ↓
main.activeSink
       ├─ Media.Kind == "poster" → calposter.Fetch() → 上传 → 发送 → calposter.MarkPushed()
       └─ Media.Kind == "bili"   → biliwatch.Fetch() → 上传 → 发送 → biliwatch.MarkPushed()
```

## QQ 平台硬约束

- **openid 体系**：群内成员用 `member_openid`，单聊用 `user_openid`；每个 bot 对同一用户的 openid 不同。
- **被动回复上限**：群 5 次 / 单聊 4 次 per msg_id。超限直接 403。设计铁律：一条命令 = 一条回复。
  命令的返回是 `command.Reply{Text | Image}`——图与文字二选一：带图时机制层先上传拿 `file_info`
  再按 `msg_type=7` 发，不会另外补一段文字；上传失败只回一句说明，绝不把日历降级成文字列表。
- **主动消息配额**：单群主动消息 20/qpm、Bot 维度 60/qpm、每群每天 1000 条。发送队列 (`kernel/queue`) 负责节流、合并与日限额控制。
- **WS 有 seq 补发**：断线重连后平台会补发历史事件，可能重复 → **必须 msg_id 去重**。
- **重连分 resume/identify 两段**：resume 带 session_id + last_seq；失败后走 identify 重新握手。
- **心跳 ack 判死**：连续 N 次心跳无 ACK → 判定连接死亡，断开重连。
- **Go 1.26+ 硬约束**：bbolt v1.4.x 兼容最新 Go 语法；`net/http` 连接复用严格遵循 Body 排空规范。
- **ed25519 签名**：HTTP 请求头带 `X-Tts-Signature`，格式 `Ed25519 <timestamp>;random(<6digits>);sign(<base64>)`。
- **富媒体（图）只能分片上传**：`upload_prepare` → 逐片 `PUT` 预签名 URL → `upload_part_finish` → `files`，
  四步之后拿 `file_info` 再 `msg_type=7` 发。`file_info` 不透明且不能跨场景复用，命令通道由 `qq.MediaCache` 按
  `场景|目标|sha1(字节)` 在内存里复用（命中跳过整段四步）；队列照旧只搬"该发哪一张"的引用（见 `kernel/queue` 的 `Media`），发送侧现上传。

## 并发模型与缓存机制

- **hub.Handle**：单 goroutine（WS reader 串行），所有事件处理都在这一个 goroutine 上。
- **calendar.Store**：`sync.RWMutex` 保护。写侧只有 annsync.loop 一个 goroutine；读侧命令 dispatch 共享。
- **queue.Dispatcher**：独立 goroutine 消费 + 定时 flush + 2 个并发 worker 出队发送。
- **biliwatch 单槽位与单飞合并**：
  - `latestID` / `latestPNG`：内存中严格只保留 1 个槽位（约 400KB），旧动态自动被 GC 回收，常驻内存增量为 0。
  - `Singleflight`：多群瞬间并发调用 `Fetch` 时，仅首个协程调用无头浏览器截图，其余协程挂起等待同一批次结果。包裹 `defer` 保证 panic 安全。
- **calposter 桶化与单飞**：出图按 5 分钟桶量化（渲染时钟与 PNG 缓存令牌都用桶起点），命令与版本推送并发碰撞时由 Singleflight 汇聚合并。

## 测试策略与 CI 流水线

| 类型 | 工具 | 运行方式 |
|------|------|--------|
| CI 持续集成 | GitHub Actions (`.github/workflows/ci.yml`) | push / PR 自动触发 |
| 并发正确性与竞态 | `go test -race ./...` | GitHub Actions (Ubuntu) / 本地 |
| 依赖与静态检查 | `go mod verify` + `go vet ./...` | CI 自动运行 |
| 多平台跨架构编译 | Linux (amd64, arm64) + Windows (amd64) | CI 矩阵编译并上传 Artifacts |
| 单飞并发与防轰炸 | `internal/feature/biliwatch/biliwatch_test.go` | 20 goroutine 瞬发单飞测试 + 冷启动基线测试 |
| 日历海报口径回归 | `internal/feature/calposter/*_test.go` | 版本键计算、5 分钟桶、海报预热与断点续存 |
| 解析器回归 | `internal/stellasora/parse_test.go` | 变异表驱动测试 |
| 平台模拟与端到端 | `cmd/qqsim` + `cmd/xingtabot -creds` | 本地沙箱与真连验证 |

## 构建与运行

```bash
# 本地运行（纯文本模式）
go run ./cmd/xingtabot -creds creds.json -db data/xingta.db

# 完整出图模式（启用日历海报与 B站动态推图）
go run ./cmd/xingtabot -creds creds.json -db data/xingta.db \
  -chrome "C:/Program Files/Google/Chrome/Application/chrome.exe" \
  -bili-interval 3m

# 离线批量生成日历长图
go run ./cmd/calshot -db data/xingta.db -repeat 1
```
