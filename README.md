# 星塔机器人 (StellasoraBot)

[![CI](https://github.com/419bw/stellasoraBot/actions/workflows/ci.yml/badge.svg)](https://github.com/419bw/stellasoraBot/actions/workflows/ci.yml)

《星塔旅人》官方活动日历、到期提醒与 B站动态推送 QQ 机器人。

自动抓取并解析《星塔旅人》官方网站公告与官方 B站动态，提供实时活动进度查询、到期提醒推送，并支持一键生成全彩版本活动日历长图海报与动态卡片。

---

## 核心功能

- **版本活动日历**：一键生成当期版本时间轴长图海报，带官方封绘、卡池周期与今日进度红线。
- **B站动态推送**：自动轮询官方 B站动态，高保真渲染富文本与多图卡片秒级推送入群。
- **活动到期提醒**：活动结束前自动预警（默认提前 48 小时），避免遗漏限时奖励。
- **多维活动查询**：支持快捷查询进行中 (`/events`)、即将结束 (`/ending`) 及未来开启 (`/upcoming`) 的活动。
- **多主题订阅管理**：群管可按需独立开启/关闭各主题推送（`/push`），支持公告临时调整的人工订正。
- **超轻量低功耗**：纯 Go 编写，常驻物理内存仅 **8~10 MB**，深度适配 Android (Termux) 与 Linux 服务器。

---

## 效果展示

### 1. 版本活动日历海报 (`/calendar`)

发送 `/calendar` 后，机器人调用无头渲染引擎生成的版本活动日历长图海报：

- **全新 V2 版本**（包含罗盘暗纹水印、稀疏星座航线图背景、临期状态标识与自适应封绘）：

![星塔机器人版本活动日历 V2 效果图](docs/assets/calendar_poster_v2.png)

- **V1 基础版本**：

![星塔机器人版本活动日历 V1 效果图](docs/assets/calendar_poster.png)

### 2. B站官方动态推送卡片 (`biliwatch`)

官方 B站账号发布新动态时，机器人自动渲染并推送到群内的高保真动态长图卡片（包含富文本排版、话题标签、表情包混排与自适应多图布局）：

![星塔机器人B站动态推送效果图](docs/assets/biliwatch.png)

---

## 指令列表

### 1. 公开指令

群成员与私聊均可直接使用。支持在聊天框输入 `/` 唤起菜单点击，或直接发送指令（如 `/calendar` 或 `@机器人 /calendar`）：

| 指令 | 说明 | 示例返回内容 |
| :--- | :--- | :--- |
| `/calendar` | **版本活动日历** | 发送高清版本活动日历长图海报，带全量活动封绘与时间进度轴 |
| `/events` | **当前进行中活动** | 列出当前所有正在开放的活动、限定招募、周期玩法及剩余时间 |
| `/ending` | **即将结束的活动** | 列出未来 48 小时内即将截止的活动，提醒玩家及时兑换奖励 |
| `/upcoming` | **即将开启的活动** | 列出已在公告公布、但尚未开启的新版本内容与预热卡池 |
| `/push` | **查看群推送状态** | 查看本群主动推送总开关以及各功能主题（到期提醒/日历海报/B站动态）订阅状态 |
| `/help` | **指令帮助** | 查看机器人使用说明与全部支持的命令菜单 |

### 2. 管理员指令

仅限群主、管理员或白名单配置用户使用，用于维护日历与推送开关：

| 指令 | 格式 | 说明 |
| :--- | :--- | :--- |
| `/push on/off` | `/push on [all\|功能]`<br>`/push off [all\|功能]` | 独立开启或关闭本群主动推送（功能可选：`expiry` 到期提醒、`poster` 日历海报、`bili` B站动态） |
| `/review` | `/review` | 查看当前尚未自动识别、或待人工复核的公告条目 |
| `/override` | `/override <活动ID> <开始时间> <结束时间> [标题]` | 强制修正指定活动的时间窗口（应对官方临时维护改口） |
| `/confirm` | `/confirm <活动ID>` | 确认人工核验通过，转为正式日历条目 |
| `/hide` | `/hide <活动ID>` | 在日历与查询列表中隐藏该条目（如测试公告、已作废内容） |
| `/show` | `/show <活动ID>` | 重新公开并显示被隐藏的活动条目 |

---

## 快速上手

### 1. 准备凭据

在项目根目录新建 `creds.json`（已在 `.gitignore` 中排除，请勿提交公开）：

```json
{
  "appId": "你的QQ机器人AppID",
  "clientSecret": "你的QQ机器人AppSecret",
  "biliCookie": "可选，你的B站账号 Cookie（如 SESSDATA 等，大幅降低动态轮询风控；留空则使用匿名访客模式）"
}
```

### 2. 注册 QQ 官方指令菜单面板（可选）

运行内置工具，将公开指令一键提交至腾讯开放平台审核：

```bash
go run ./cmd/panelreg -creds creds.json
```

### 3. 启动运行

#### 方式 A：标准本地运行

```bash
# 纯文本模式（无需浏览器内核，内存占用极低；仅启用查询与到期提醒）
go run ./cmd/xingtabot -creds creds.json -db data/xingta.db

# 完整出图模式（启用日历海报与 B站长图动态推送；指定无头 Chrome/Chromium 路径）
go run ./cmd/xingtabot -creds creds.json -db data/xingta.db -chrome "C:/Program Files/Google/Chrome/Application/chrome.exe"
```

常用命令行参数：
* `-chrome`：无头浏览器可执行文件路径；留空则不启用出图相关功能（日历海报与 B站动态推图）。
* `-bili-uid`：监听的 B站官方账号 UID（默认内置星塔旅人官方 UID）。
* `-bili-interval`：B站动态轮询间隔（默认 `5m`）。
* `-bili-cookie`：B站登录态 Cookie（降低风控概率；留空优先从 `creds.json` 中的 `biliCookie` 读取）。
* `-refresh`：官网公告同步间隔（默认 `30m`）。
* `-lead`：活动结束前提早提醒时长（默认 `48h`）。

#### 方式 B：Android 手机 (Termux) 后台常驻

项目针对 Android Termux 环境进行了专项适配（内置 DNS 优化、字体挂载及无沙箱环境兼容），GitHub Actions 每次提交均会自动打包发布适用于 Termux 的 ARM64 二进制文件，并提供了开箱即用的后台管理脚本：

```bash
cd ~/xingtabot

# 启动（screen 后台守护运行，默认带 -chrome ./chrome-headless）
./start.sh

# 查看当前运行状态与日志
./status.sh

# 持续跟踪日志输出
./logs.sh

# 安全停止
./stop.sh
```

---

## 开源许可证

本项目基于 MIT License 协议开源。