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
                       出图按 5 分钟桶量化（渲染时钟与 PNG 缓存令牌都用桶起点，桶内字节
                       实测严格一致——出图时刻戳删掉后 sha1 连续 ~9 分钟不变），
                       同桶第二次问就是 0 渲染、直接命中
calposter.Fetch(key)   ← 发送队列的 Sink 在 worker 上走这条：允许现抓，宁可慢也不推一张全是占位的图
       ↓ Build 数据集 → calposter.Page → render.Capturer.Capture → PNG
「日历」命令回 command.Reply{Image}；版本开闸只投 queue.Item{Media{poster,key}}——
调度回调绝不出图（它同步跑 Fn，画一趟就把同期到期的提醒一起拖住），图在真要发的那一刻
由 Sink 现取；账本由队列条目自带的 OnDelivered 回执写（发送成功才逐条回调，失败与丢弃
一律不记），账键为 pushRec{SettledAt, Targets}：记到"目标×版本"粒度，全部订阅目标收齐
才收敛写全局了结；下一轮重排只补缺回执的目标。
       ↓
qq.UploadGroupImage 四步 → file_info → msg_type=7
       命令通道包一层 qq.MediaCache：同字节命中就跳过四步（只付发送那一下），
       缓存那份被平台拒 → Forget + 就地重传再发一次

3. B站官方动态监听链（biliwatch）：
api.bilibili.com
       ↓ 每 -bili-interval (默认 3 分钟一轮，启动时先跑一轮建立基线)
biliwatch.round()
  ├─ fetcher.FetchLatest() (访客 CookieJar + WBI 混淆签名 space/feed)
  ├─ 发现未推送新动态 (按时间倒序遍历，确保旧动态先入队、聊天流时序正确)
  └─ api.Submit(queue.Item{Media: {Kind: "bili", Key: dynID}, Topic: "bili"})
       ↓
activeSink (发送侧 worker 出队):
  ├─ biliwatch.Fetch(dynID)
  │    ├─ 单图槽位缓存 latestPNG 命中？→ 0ms 内存秒出
  │    └─ 未命中 → Singleflight 汇聚并发请求 → 浏览器仅渲染一次 → 覆盖单槽缓存
  ├─ qq.UploadGroupImage (分片上传获取 file_info)
  └─ qq.SendGroupMessage → 成功后队列逐条回调条目自带的 OnDelivered 回执
       └─ biliwatch 记"该群已收到"（pushRec.Targets），全部订阅群收齐才收敛写全局了结

4. 主动消息与出图分发（activeSink）：
queue.Dispatcher (多群消息出队，整批成功后逐条回调条目自带的 OnDelivered 回执)
       ↓
main.activeSink（只管出图与发送，不参与记账）
       ├─ Media.Kind == "poster" → calposter.Fetch() → 上传 → 发送 → calposter 记回执账
       └─ Media.Kind == "bili"   → biliwatch.Fetch() → 上传 → 发送 → biliwatch 记回执账
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
  四步之后拿 `file_info` 再 `msg_type=7` 发。`file_size`/`block_size` 按文档是**字符串**；`md5`/`sha1`/`md5_10m` 必填
  （`md5_10m` 供秒传）。URL 直传要求文件公网可访问，我们没有公网。**`file_info` 不透明、文档示例 ttl 只有 300 秒
  但实测本 bot 上传返回 24h、且不能跨场景复用** → 不落盘、不排队携带，命令通道由 `qq.MediaCache` 按
  `场景|目标|sha1(字节)` 在内存里复用（命中跳过整段四步；被平台判死时 Forget + 就地重传再发一次），
  队列照旧只搬"该发哪一张"的引用（见 `kernel/queue` 的 `Media`），发送侧现上传。

## 解析器规则 v3 (stellasora/parse.go)

从公告 HTML 正文提取活动名、时间区间与条目。公告先分三类（`Provenance`）：

| 类别 | 判据 | 产出 |
|------|------|------|
| solo 独立公告 | 标题有 `「…」` 且恰好一个主窗口 | 1 条事件，海报=公告封面 |
| version 版本公告 | 标题匹配 `^\[…\](活动|版本)一览$` | 版本边界（`Result.Ver`，不进日历）+ **1 条版本主活动事件**（活动一览就是主活动自己的公告，海报=版本主视觉）；整图"版本一览"零产出 |
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

## 时间表渲染与海报生成（连续时间模型）

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
- **名字可换行，行高 52**：`.nm` 是内联 span，`text-overflow` 对它无效，长名字过去是直接压到相邻
  段的海报上；改成换行后由行高吸收（合并带里两个名字各缺 40+px，靠加宽整表不划算）。
- **时间标记只剩一条**：一根红色竖线贯穿整个图区（画布按 5 分钟桶起点摆放，桶内字节严格稳定；
  `calc()` 把日轴百分比换算进 `.plot` 的框），上端接住标尺里"今天"那一格。原先的"今天整列黄底"（整列近似，
  比条带粗糙）与"玩法期止红双线"（与每条带的右端重复，且三页签语义不一致：上一版本贴右缘、最新版本没有
  下一次维护数据）都删了。页脚曾有一串"出图时刻 MM/DD HH:MM"，是每分钟唯一在变的像素（sha1 实测每分钟
  都翻），已删——图例只留"红竖线＝出图时刻"，不再写具体分钟。
- **出图模式 `#x[版本键]`**：去掉导航与开关行，卡片宽度由 16:9 反解（设宽→读高→再算宽；
  泳道数与宽度无关，一次即收敛），下限 `MIN_PPD=50`。`bash .probe/calpng/export.sh` 按版本逐个出图，
  产物文件名 = 版本键。**版本键 = `ActStart` 的 UTC `yyyyMMddHHmm`**（`data.json` 里 `versions[].key`）：
  以前用"排序后的下标"（`#x0`~`#x2`、`vi0.png`），新版本一出现下标全体左移，同一个文件名下的内容就换了。
- **当前版本 = 已开闸的最新一个**（`act0 + openOffsetMs <= now`），不是"窗口含今天"：版本交接那天新旧两窗
  重叠（旧版本只剩兑换尾段），按窗口判会同时冒出两个"当前版本"，而且默认挑到旧的那个。
- **开闸偏移 17:00 由数据下发**：`openOffsetMs` 写在 `data.json`（Go 侧 `openAt` 常量），模板读 `DATA.openOffsetMs`。
  这个数以前只活在模板里，机器人出图那份要再存一份，两边必然漂移。
- 模板 `internal/feature/calposter/template.html` 是**内嵌的运行时资产**（机器人自己出图要用它），产物页
  `.probe/timetable/web/{data.json,calendar.html}` 与全部校验脚本仍在 `.probe/`（不入库）；
  `.probe/calgen` 的 Go 渲染器冻结在旧天格模型，`.probe/timetable/web` 读的是上面这份模板。

## 并发模型与实测基准数据

- **hub.Handle**：跑在网关 attempt 主循环 goroutine 上，与心跳发送、ack 判死、重连**共享同一个循环**且同步调用（WS 读帧是独立 goroutine，经 64 缓冲 channel 喂入）。所以处理器不许做耗时活（会拖慢心跳直至判死断连），错误与 panic 也绝不能从 Handle 漏出去——返回错误 = 网关判定链路故障而断连重连（见硬坑 13）。
- **calendar.Store**：`sync.RWMutex` 保护。写侧只有 annsync.loop 一个 goroutine；读侧命令 dispatch 共享。
- **queue.Dispatcher**：独立 goroutine 消费 + 定时 flush + 2 个并发 worker 出队发送。
- **biliwatch 单槽位与单飞合并**：
  - `latestID` / `latestPNG`：内存中严格只保留 1 个槽位（约 400KB），旧动态自动被 GC 回收，常驻内存增量为 0。
  - `Singleflight`：多群瞬间并发调用 `Fetch` 时，仅首个协程调用无头浏览器截图，其余协程挂起等待同一批次结果。包裹 `defer` 保证 panic 安全。
- **calposter 桶化与单飞**：出图按 5 分钟桶量化（渲染时钟与 PNG 缓存令牌都用桶起点），命令与版本推送并发碰撞时由 Singleflight 汇聚合并。

实测吞吐基准（本机 i7-13700H, Go 1.26）：
| 路径 | 吞吐 | p50 | p99 | HeapInUse |
|------|------|-----|-----|-----------|
| WS→decode→dedup→noop handler | 101,243 msg/s | 65µs | 636µs | 85MB |
| 业务 dispatch 单线程 | 6,963 cmd/s | <1µs | 1.5ms | 5MB |
| 业务 dispatch 20 并发 | 17,537 cmd/s | 1ms | 3.3ms | 8MB |

结论：计算与调度瓶颈在平台 HTTP 往返（~100-200ms/reply），不在本地处理。

## 测试策略与验证矩阵

| 类型 | 工具 | 运行方式 / 说明 |
|------|------|-----------------|
| CI 持续集成 | GitHub Actions (`.github/workflows/ci.yml`) | push / PR 自动化触发，Linux 容器全量跑测与跨平台构建 |
| 并发正确性与竞态 | `go test -race ./...` | CI 环境 (Ubuntu gcc) / 容器 (xingta-race 镜像) |
| 依赖与静态检查 | `go mod verify` + `go vet ./...` | CI 自动运行 |
| 多平台跨架构编译 | Linux (amd64, arm64) + Windows (amd64) | CI 矩阵编译并上传发布产物 |
| 单飞并发与防轰炸 | `internal/feature/biliwatch/biliwatch_test.go` | 20 goroutine 瞬发并发合并测试 + 倒序时序与冷启动基线测试 |
| 解析器回归 | `internal/stellasora/parse_test.go` (变异表) | 变异表驱动测试，核验 v3 正则切分稳定性 |
| 时间表版式回归 | `.probe/calpng/check.js`（4 模式 × 3 页签 + 定点断言） | 本机 node |
| 时间表文字溢出 | `.probe/calpng/fit.js`（真排版量字宽 + 图例色块压字 + 出图时刻线的落点与贯穿；负对照 = 调小 PPD / 改错 inset） | 本机 node + Chrome |
| 版本键→内容映射 | `.probe/calpng/keycheck.js`（真 Chrome 渲染 `#x<key>`，核 `#vname` 与按钮选中态；负对照 = 键命中后错一位） | 本机 node + Chrome |
| 富媒体四步上传 | `internal/qq/media_test.go`（httptest 假平台：必填字段、`file_size`/`block_size` 是字符串、分片正文无 token、`url` 留空、`srv_send_msg=false`） | 任何环境 |
| file_info 内容缓存 | `internal/qq/mediacache_test.go`（同字节只合并一次、异字节各传、ttl 上限过期重传、群/单聊与不同目标隔离、Forget 后重传；`IsPlatformRejection` 只认平台明确回拒）+ `internal/command/command_test.go`（命中缓存被拒→Forget+重传再发一次；网络错不重试；新上传那份被拒不重传）；负对照 `python .probe/mutate/cachemutate.py` | 任何环境 |
| 日历图口径与缓存 | `internal/feature/calposter/*_test.go`（版本键/当前版本判据、只取本窗记录、周期玩法判据、指纹敏感性、PNG（5 分钟桶）与海报两级缓存、singleflight、推图排期与账本；负对照 `python .probe/mutate/pushmutate.py`） | 任何环境 |
| 抓取只在后台 | `internal/feature/calposter/*_test.go`（命令路径零网络请求、缺海报照常出图、预热抓到才带图、失败按 RetryAfter 冷却、并发预热同 URL 只抓一次；海报取字节先问原地址、被拒才换候选域、两边都失败才画占位；负对照 `python .probe/mutate/artmutate.py`） | 任何环境 |
| 入口阻塞压测 | `go run ./.probe/posterload -mode=stall\|ws\|warm\|chrome`（真 Hub + 真命令表 + 真 calposter，画布是可控耗时假件；量排队等待、心跳间隔、判死、预热开销、并发浏览器进程数） | 本机 |
| 平台模拟与端到端 | `cmd/qqsim` + `cmd/xingtabot -creds` | 本机沙箱与真连验证 |
| 真库真浏览器出图 | `go run ./cmd/calshot -db .probe/livesync.db -repeat 3`（三段耗时打进日志；-noart 只核结构） | 本机 Chrome |
| 冷启动验证 | `.probe/livesync/main.go` | 本机 |

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

## 关键设计决策与核心踩坑记录 (Hard-won Caveats)

1. **不做"进行中活动"API 查询**：官网没有这个接口。`type=activity` 是联动周边不是游戏活动。全量抓 + 本地投影是唯一可行路径。
2. **凭据只在 .probe/creds.json**：已 .gitignore 排除。AppSecret/access_token 绝不出现在任何输出或截图。
3. **bbolt 不 mkdir**：`openDB()` 先 `os.MkdirAll` 再开库。data/ 在干净检出里不存在。
4. **command 注册在 Feature.Start()**：不是 New()。启动日志里 `命令:` 为空是正常的（打印在 rt.Run 之前）。
5. **被动回复不降级为主动消息**：触上限只记日志，绝不绕过。
6. **时区假设**：官网日期默认 +08:00，无法从文本验证。若官方用 JST 则全部偏 1 小时。
7. **空壳文件陷阱**：rm -rf 后"对照记录还原"会产生大小正确内容全零的文件。判恢复要读内容，受害集合正好是 .gitignore 排除的目录。
8. **B站 WBI 签名与访客池**：B站 Web 端动态接口启用了 WBI 混淆签名校验，直接请求返回 `-403`。通过请求首页预热提取 `img_key` / `sub_key`、进行字符重排并拼接加盐 MD5，并在客户端维护带 CookieJar 的访客会话池，彻底免去登录态与账号被封风险。
9. **Singleflight 并发单飞与 Panic 保护**：多群瞬间出队推图时，若不合并会导致几十个 headless Chrome 进程同时启动压垮 CPU。必须通过 Singleflight 汇聚合并。但在单飞执行闭包中，若无头浏览器偶发崩溃引发 panic，会导致 group 内部 `c.done` 无法正常关闭、其余等待协程永久死锁。因此 Singleflight 内部必须带有具名返回值与 `defer func() { if r := recover(); r != nil { ... } }()` 保护。
10. **HTTP 响应体排空与连接池复用**：`net/http` 客户端在读取非 2xx 或忽略正文时，若仅调用 `resp.Body.Close()` 而未排空，底层 TCP 连接无法放回空闲池，会导致长跑时产生大量 TIME_WAIT 并耗尽端口。必须统一采用 `io.Copy(io.Discard, resp.Body)` 排空后再关闭。
11. **单槽位内存防爆（Single-Slot Memory Model）**：在低功耗 ARM64 (Termux 4GB 内存) 长期运行环境下，任何无上限的渲染缓存都会导致内存爬升并最终被 OOM 杀死。日历图基于 5 分钟时间桶量化单份缓存；B站动态基于 `latestID` 保持单槽位（约 400KB）内存缓存，旧图随 ID 变更由 GC 自然回收，确保常驻内存水平绝不随运行时间增加。
12. **多主题订阅解耦与轻量持久化**：群管理与推送解耦，不硬编码业务主题。`kernel/target` 在 bbolt 中以 JSON 保存 `map[string]map[string]bool`，内存镜像单机支撑万级群目标仅消耗 ~1MB 内存，彻底将出站吞吐的控制权交给 `kernel/queue`。
13. **机制层调用业务回调的边界必须装 panic 保险丝**：全库业务回调（queue worker 的 `Sink.Send`、调度器 `Task.Fn`、hub.dispatch 的处理器）都由机制层 goroutine 直接执行，而 Go 里任何 goroutine 的未恢复 panic 都终止整个进程——生产又是 screen 一次性拉起、无守护，业务偶发崩溃 = 停服到人工介入。三处边界各自带 `defer recover()` 把 panic 降级：Sink panic 走既有退避重试与终态上报；调度任务 panic 走 OnError；hub 处理器 panic 记日志 + `HandlerPanic` 计数后返回 nil（**绝不能外抛**——WS 上返回错误会让网关断连重连，webhook 上会 `dedup.Forget` 后遭平台无限重投，确定性 panic 两条路都成风暴）。guard 只包外部调用那一次，机制层自身的 bug 该炸还得炸。历史上曾误记"hub.Handle 在 WS reader goroutine 上"，读帧与处理的 goroutine 关系以并发模型一节为准。

## 仓库

`git@github.com:419bw/stellasoraBot.git`，remote 名 `github`，主分支 `master`。
