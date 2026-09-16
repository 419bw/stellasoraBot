#!/usr/bin/env node
/*
 * 把多文件前端工程内联成 Go //go:embed 需要的单文件模板。
 * 纯 Node、零依赖：node js/../build-inline.js
 *
 *  - <link rel="stylesheet" href="css/.."> → <style>..</style>
 *  - <script src="js/app.js">              → <script>..</script>
 *  - js/sample-data.js 仅本地预览兜底，生产构建默认剔除（Go 会注入真实 DATA）
 *  - 保留 /*__DATA__*​/ 注入点，交给 calposter.Page 替换
 * 输出：dist/template_uigame.html
 */
const fs = require('fs');
const path = require('path');

const root = __dirname;
const dist = path.join(root, 'dist');
fs.mkdirSync(dist, { recursive: true });

let html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');

// 1) 内联 CSS（装饰图形全部为内联 SVG data-URI，无本地图片资源需要处理）
html = html.replace(/<link[^>]*rel="stylesheet"[^>]*href="([^"]+)"[^>]*>/g,
  (m, href) => '<style>\n' + fs.readFileSync(path.join(root, href), 'utf8') + '\n</style>');

// 2) 剔除本地样本兜底（生产由 Go 注入真实 DATA）
html = html.replace(/[ \t]*<script[^>]*src="[^"]*sample-data\.js"[^>]*><\/script>\s*\n?/g, '');

// 3) 内联其余本地脚本（app.js）
html = html.replace(/<script[^>]*src="(?!https?:)([^"]+)"[^>]*><\/script>/g,
  (m, src) => '<script>\n' + fs.readFileSync(path.join(root, src), 'utf8') + '\n</script>');

const out = path.join(dist, 'template_uigame.html');
fs.writeFileSync(out, html, 'utf8');
const outEmbed = path.join(root, '..', 'template.html');
fs.writeFileSync(outEmbed, html, 'utf8');

const hasMark = html.includes('/*__DATA__*/');
console.log('已生成', path.relative(root, out), '和', path.relative(root, outEmbed), '| 数据注入点保留:', hasMark, '| 字节:', Buffer.byteLength(html));
if (!hasMark) { console.error('错误：内联后丢失 /*__DATA__*/ 注入点'); process.exit(1); }
