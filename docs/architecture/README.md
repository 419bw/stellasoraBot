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
  stellasora/             ← 中文网 Source（HTTP 客户端 + 规则 v2 解析器）
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

## 解析器规则 v2 (stellasora/parse.go)

从公告 HTML 正文提取活动名和时间区间：

1. **活动名**：标题里第一个 `「…」` 内的文本。
2. **主区间**：正文中匹配 `intervalRe` 的日期范围。要求恰好一个主区间（多 = suspect）。
3. **次区间**：含"领取/结算/兑换/售卖/补给/开启/关闭"关键词的 → 丢弃不进日历。
4. **模糊起始**：`relAnchor` = `[^\s~～]{0,12}后`（开放类型，"公测开启后"/"维护结束后"等）。
5. **永久**：`permanentRe` 匹配"常驻"且同行有日期 → SaysPermanent。
6. **NBSP 归一化**：Go `\s` 不匹配 U+00A0，必须先替换为普通空格。

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
