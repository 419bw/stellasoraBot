# 活动日历 · 游戏待办手账风皮肤（uigame）

把原单文件 `internal/feature/calposter/template.html` 的**甘特图业务逻辑原样保留**，
视觉皮肤对齐《星塔旅人》游戏内「待办事项」界面（二次元轻拟物手账 / Casual Anime
Stationery UI），并拆成多文件前端工程，便于维护。

## 目录结构

```
uigame/
├── index.html          # 结构骨架：游戏导航 + 双层卡纸 + 甘特容器，保留 /*__DATA__*/ 注入点
├── css/
│   └── style.css       # 游戏待办手账皮肤（全部视觉都在这里，不碰逻辑）
├── js/
│   ├── sample-data.js  # 仅本地预览的样本数据；Go 注入真实 DATA 时自动跳过，不覆盖
│   └── app.js          # 甘特业务逻辑（从 template.html 迁出，口径一致，只改了 band 的视觉 HTML）
├── build-inline.js     # 零依赖构建：内联回 Go //go:embed 需要的单文件
└── dist/               # 构建产物（运行 build-inline.js 后生成）
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
  全部沿用原模板同一套算法，**没有改任何业务口径**；`app.js` 相对原脚本只把活动条右端的
  结束日期换成了带时钟图标的胶囊，并新增"剩余 ≤2 天转红"的展示判定（纯视觉）。

## 接回 Go（需要你拍板后再做，当前未改任何 Go 代码、未动远端）

机器人出图是把 HTML 写到临时目录再用无头浏览器打开，外部 `css/js` 相对路径在临时目录
加载不到，因此上线前需要内联成单文件：

```bash
node build-inline.js        # 生成 dist/template_uigame.html（已剔除样本、保留注入点）
```

之后在 `calposter.go` 增加一行 `//go:embed dist/template_uigame.html`（或替换现有
template.html 的 embed）即可，`Page()` 与两趟截图流程都不用改。

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
