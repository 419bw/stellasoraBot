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
  render/                 ← 日历出图：组数据集 + 无头浏览器截图（模板 template.html 是运行时资产，入库）
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
| command | `Registrar` (Add)、`Text(fn)` 适配 | calquery, calops, calposter |
| stellasora | `Source` (Name/List/Fetch) | annsync |
| render | `Page(Dataset, 模板)` + `Browser.Capture(页面, 锚点)` | feature 层 |

**基础设施的名字里不许出现业务词。** 队列搬图不叫 `PosterSpec{VersionKey}`，叫 `Media{Kind, Key}`——`Kind`
的取值由接线的 Sink 认领，队列不枚举；也不带平台给的 `file_info`（ttl 只有几分钟，排队加退避一过期必发不出去），
只带"该发哪一张"的引用，上传与生成推到 Sink 真要发的那一刻。同理，`annsync` 不认识 `"version"` 这个源自述的
取值：版本列表由功能自己 `ReadRecs` 后过滤 `Provenance`，那个字面量在 `main.go` 从 `stellasora.ProvVersion` 传进去。
`render` 只给两件基础件：把数据集注进模板、把一页 HTML 截成 PNG。它不认识版本键，也不回答"该发哪一张"——
那是 `calposter.Poster.Image(ctx, key)` 的脸，住在功能层。哪个版本该发、什么时候发、发过没有，全在功能层。
（Go 原生绘制实现过——纯几何 + 字体 + 逐像素画，还与模板做过逐条带对账门——但观感明显不如浏览器、
且要背一份字体子集与绘制代码，已撤。几何真相只留 `internal/render/template.html` 这一份。）

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
calquery.active() / ending() / upcoming()      ← 旧的文字口径，日历图跑通后删
       ↓ calendar.View 二分查询
格式化 → command.dispatch → SendGroupReply
       ↓
QQ 平台 (api.bot.qq.com)

版本日历图（calposter）：

annsync 投影出的 []Rec（ReadRecs，功能自己按 Provenance 过滤版本窗口）
       ↓ 每 -warm 一轮（启动立刻先跑一轮）
calposter.warmArt → 只补本窗缺的海报字节：每张先问公告里的原地址，被拒才换 OSS 传输加速
       ↓            端点补一次；失败的 URL 进 10 分钟冷却，同一 URL 并发只抓一次
       ↓            （实测：CDN 边缘全拒那一轮 6.4s / 32 请求 / 16 张靠候选域补齐；
       ↓             海报缓存命中后一轮 50ms / 0 请求）
calposter.Image(key)   ← 「日历」命令走这条：只读本地字节，零网络，缺海报就画占位（实测 1.9s）
calposter.Fetch(key)   ← 版本开闸推图走这条：允许现抓，宁可慢也不推一张全是占位的图
       ↓ Build 数据集 → render.Page → render.Browser.Capture → PNG
「日历」命令回 command.Reply{Image}；版本开闸投 queue.Item{Media{poster,key}}
       ↓
qq.UploadGroupImage 四步 → file_info → msg_type=7
```

## QQ 平台硬约束

- **openid 体系**：群内成员用 `member_openid`，单聊用 `user_openid`；每个 bot 对同一用户的 openid 不同。
- **被动回复上限**：群 5 次 / 单聊 4 次 per msg_id。超限直接 403。设计铁律：一条命令 = 一条回复。
  命令的返回是 `command.Reply{Text | Image}`——图与文字二选一：带图时机制层先上传拿 `file_info`
  再按 `msg_type=7` 发，不会另外补一段文字；上传失败只回一句说明，绝不把日历降级成文字列表。
- **主动消息配额**：群每用户每天有限条。发送队列 (`kernel/queue`) 负责节流和合并。
- **WS 有 seq 补发**：断线重连后平台会补发历史事件，可能重复 → **必须 msg_id 去重**。
- **重连分 resume/identify 两段**：resume 带 session_id + last_seq；失败后走 identify 重新握手。
- **心跳 ack 判死**：连续 N 次心跳无 ACK → 判定连接死亡，断开重连。
- **Go 1.27 硬约束**：bbolt v1.3.11 用了 `unsafe.Slice` 不兼容 1.26 以下；Go 1.27 起 `crypto/rand.Read` 签名变了。
- **ed25519 签名**：HTTP 请求头带 `X-Tts-Signature`，格式 `Ed25519 <timestamp>;random(<6digits>);sign(<base64>)`。
- **富媒体（图）只能分片上传**：`upload_prepare` → 逐片 `PUT` 预签名 URL → `upload_part_finish` → `files`，
  四步之后拿 `file_info` 再 `msg_type=7` 发。`file_size`/`block_size` 按文档是**字符串**；`md5`/`sha1`/`md5_10m` 必填
  （`md5_10m` 供秒传）。URL 直传要求文件公网可访问，我们没有公网。**`file_info` 不透明、示例 ttl 只有 300 秒、
  且不能跨场景复用** → 不缓存，每次发送前重跑四步；队列里因此只搬"该发哪一张"的引用（见 `kernel/queue` 的 `Media`）。

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
- **名字可换行，行高 52**：`.nm` 是内联 span，`text-overflow` 对它无效，长名字过去是直接压到相邻
  段的海报上；改成换行后由行高吸收（合并带里两个名字各缺 40+px，靠加宽整表不划算）。
- **时间标记只剩一条**：一根红色竖线贯穿整个图区（分钟级，`calc()` 把日轴百分比换算进 `.plot` 的框），
  上端接住标尺里"今天"那一格。原先的"今天整列黄底"（整列近似，比条带粗糙）与"玩法期止红双线"
  （与每条带的右端重复，且三页签语义不一致：上一版本贴右缘、最新版本没有下一次维护数据）都删了。
- **出图模式 `#x[版本键]`**：去掉导航与开关行，卡片宽度由 16:9 反解（设宽→读高→再算宽；
  泳道数与宽度无关，一次即收敛），下限 `MIN_PPD=50`。`bash .probe/calpng/export.sh` 按版本逐个出图，
  产物文件名 = 版本键。**版本键 = `ActStart` 的 UTC `yyyyMMddHHmm`**（`data.json` 里 `versions[].key`）：
  以前用"排序后的下标"（`#x0`~`#x2`、`vi0.png`），新版本一出现下标全体左移，同一个文件名下的内容就换了。
- **当前版本 = 已开闸的最新一个**（`act0 + openOffsetMs <= now`），不是"窗口含今天"：版本交接那天新旧两窗
  重叠（旧版本只剩兑换尾段），按窗口判会同时冒出两个"当前版本"，而且默认挑到旧的那个。
- **开闸偏移 17:00 由数据下发**：`openOffsetMs` 写在 `data.json`（Go 侧 `openAt` 常量），模板读 `DATA.openOffsetMs`。
  这个数以前只活在模板里，机器人出图那份要再存一份，两边必然漂移。
- 模板 `internal/render/template.html` 是**入库的运行时资产**（机器人自己出图要用它），产物页
  `.probe/timetable/web/{data.json,calendar.html}` 与全部校验脚本仍在 `.probe/`（不入库）；
  `.probe/calgen` 的 Go 渲染器冻结在旧天格模型，`.probe/timetable/web` 读的是上面这份模板。

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
| 时间表文字溢出 | `.probe/calpng/fit.js`（真排版量字宽 + 图例色块压字 + 出图时刻线的落点与贯穿；负对照 = 调小 PPD / 改错 inset） | 本机 node + Chrome |
| 版本键→内容映射 | `.probe/calpng/keycheck.js`（真 Chrome 渲染 `#x<key>`，核 `#vname` 与按钮选中态；负对照 = 键命中后错一位） | 本机 node + Chrome |
| 富媒体四步上传 | `internal/qq/media_test.go`（httptest 假平台：必填字段、`file_size`/`block_size` 是字符串、分片正文无 token、`url` 留空、`srv_send_msg=false`） | 任何 |
| 日历图口径与缓存 | `internal/feature/calposter/*_test.go`（版本键/当前版本判据、只取本窗记录、周期玩法判据、指纹敏感性、PNG 与海报两级缓存、singleflight、推图排期与账本；负对照 = 去掉宽限期 / 去掉已推判定 / 先记账再画图） | 任何 |
| 抓取只在后台 | 同上（命令路径零网络请求、缺海报照常出图、预热抓到才带图、失败按 RetryAfter 冷却、并发预热同 URL 只抓一次；海报取字节先问原地址、被拒才换候选域、两边都失败才画占位、多张并发抓取计数与收尾日志要等于真实值；负对照 `python .probe/mutate/artmutate.py` = 把 Image 的 Fetch 打反 / 去掉冷却判定 / 去掉在飞表 / 关掉兜底 / 抢先问候选域 / 换域改成子串匹配 / 新抓数报错） | 任何 |
| 入口阻塞压测 | `go run ./.probe/posterload -mode=stall\|ws\|warm\|chrome`（真 Hub + 真命令表 + 真 calposter，画布是可控耗时假件；量排队等待、心跳间隔、判死、预热开销、并发浏览器进程数） | 本机 |
| 真库真浏览器出图 | `go run ./cmd/calshot -db .probe/livesync.db -repeat 3`（三段耗时打进日志；-noart 只核结构） | 本机 Chrome |
| 冷启动验证 | `.probe/livesync/main.go` | 本机 |

## 构建与运行

```bash
# 开发（本机 Windows）
go run ./cmd/xingtabot -db data/xingta-live.db -scan 1m

# 带主动发送（到期提醒与版本日历图共用这一份目标）+ 启用日历图
go run ./cmd/xingtabot -db data/xingta-live.db -scan 1m -lead 80h \
  -push g:<group_openid> -chrome "C:/Program Files/Google/Chrome/Application/chrome.exe"
# 不给 -chrome 就是不启用日历图功能：命令表里没有「日历」，也不会推图，其余照常。

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
