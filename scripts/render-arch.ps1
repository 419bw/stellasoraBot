# 渲染 docs/architecture/*.dot 到 out/arch/（PNG 看细节，SVG 可无限放大）。
# 需要 Graphviz：winget install Graphviz.Graphviz  （装完重开一个终端让 PATH 生效）
# 用法：powershell -ExecutionPolicy Bypass -File scripts\render-arch.ps1
$ErrorActionPreference = 'Stop'

$dot = Get-Command dot -ErrorAction SilentlyContinue
if (-not $dot) {
    Write-Error '找不到 dot，请先安装 Graphviz：winget install Graphviz.Graphviz'
}

$root = Split-Path -Parent $PSScriptRoot
$src  = Join-Path $root 'docs\architecture'
$dst  = Join-Path $root 'out\arch'
New-Item -ItemType Directory -Force -Path $dst | Out-Null

foreach ($file in Get-ChildItem -Path $src -Filter *.dot) {
    foreach ($fmt in 'png', 'svg') {
        $out = Join-Path $dst ($file.BaseName + '.' + $fmt)
        & dot -T$fmt -Gdpi=150 -o $out $file.FullName
        if ($LASTEXITCODE) { Write-Error "渲染失败：$($file.Name) → $fmt" }
        Write-Host "OK  $out"
    }
}
