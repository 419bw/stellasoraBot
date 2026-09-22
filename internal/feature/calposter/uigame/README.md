# 活动日历 · 游戏待办手账风皮肤（uigame）

**本目录是活动日历页的唯一源。** 甘特图业务逻辑与《星塔旅人》游戏内「待办事项」皮肤
（二次元轻拟物手账 / Casual Anime Stationery UI）都在这里维护；Go `//go:embed` 用的
`../template.html` 是**本目录的构建产物**，不要直接改它（见下面「改版式的流程」）。

## 目录结构

```
uigame/
├── index.html          # 结构骨架：游戏导航 + 双层卡纸 + 甘特容器，保留 /*__DATA__*/ 注入点
├── css/
│   └── style.css       # 游戏待办手账皮肤（全部视觉都在这里，不碰逻辑）
├── js/
│   ├── sample-data.js  # 仅本地预览的样本数据；Go 注入真实 DATA 时自动跳过，不覆盖
│   └── app.js          # 甘特业务逻辑：解析 DATA、分组合并、装箱、版式全在这里
├── build-inline.js     # 零依赖构建：内联成 Go //go:embed 用的单文件（写两个产物）
└── dist/               # 构建产物（内容与 ../template.html 相同）
```

## 本地预览（无需起服务）

直接用浏览器打开 `index.html` 即可：普通 `<link>/<script>` 相对路径在 `file://` 下可加载，
`sample-data.js` 检测到没有 Go 注入的 `DATA` 时会用样本兜底。

- 交互模式：`index.html`
- 出图模式（机器人实际出图路径，隐藏导航/开关、锁 16:9）：`index.html#x` 或 `#x<版本键>`

## 数据从哪来（和线上完全一致）

- `index.html` 里的 `/*__DATA__*/` 是注入点，生产时由 Go `calposter.Page(Dataset, tpl)`
  替换为 `const DATA = {...};`，字段定义见 `page.go`（now/openOffsetMs/records/versions/monthly）。
- 时间解析、版本窗口、分组合并、泳道装箱、真实时刻定位、16:9 迭代、出图 title 自报尺寸
  全在 `app.js`；**取数口径全在 Go 侧**（`dataset.go` 选记录、`detectPermanent` 判常驻玩法、
  `page.go` 定字段）。页面不认识"版本""常驻"这些词的含义，只照 DATA 画 —— 这些口径原先也印在
  页脚给群友看，2026-09-22 按"开发者看文档就行"撤掉了，所以这段是它唯一的落点。

## 改版式的流程（`../template.html` 是产物，别手改）

机器人出图是把 HTML 写到临时目录再用无头浏览器打开，外部 `css/js` 相对路径在临时目录
加载不到，所以 Go 内嵌的必须是内联好的单文件。`build-inline.js` 一次写**两个**产物：

```bash
cd internal/feature/calposter/uigame
node build-inline.js   # → dist/template_uigame.html 和 ../template.html（同一个内容）
```

改完源文件就跑一次，把「源 + 两个产物」一起提交。直接编辑 `../template.html` 会被下一次
构建整份覆盖 —— 那次改动凭空消失，而且没有任何东西会提醒你。`Page()` 与两趟截图流程都
不用改。

## 视觉对应关系（游戏 UI → 甘特）

| 游戏待办元素 | 本皮肤落点 |
| --- | --- |
| 模糊明亮宿舍背景 | `.screen` 多层柔光斑渐变 |
| 深蓝斜切导航（返回/定位标签/房子） | `.topbar`（仅交互模式，出图隐藏） |
| 深藏青衬板 + 奶白纸双层卡纸 | `.win::before / ::after` |
| 定位图钉 + 荧光笔标题 | `h1::before` 矢量 pin、`h1::after` 粉色涂抹 |
| 当前周便利贴 | `.wk.on` 淡黄 |
| 粉/黄/蓝分类 | `.grp.pool/.ver/.mon .gt` 斜角和纸胶带 |
| 白边模切贴纸缩略图 | `.ava`（无海报时 `.noart` 出占位图标） |
| 任务条左缺口 | `.band::before` 内凹咬痕（跨窗延续 `oL` 不画） |
| 时钟倒计时 | `.ends` 时钟胶囊，≤2 天 `.hot` 变红 |
| 红墨水手绘今天线 | `.nowline` 虚线 + 顶部不规则红圈 |
| 四角星点缀 | `.doodle` |
