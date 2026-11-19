# live2motion 本地直接运行（不构建 Docker）
#
# 用法：
#   1) 先把 HEIC / MOV 成对放进 $SourceDir
#   2) 在项目根目录执行：  .\scripts\run-windows.ps1
#   3) 浏览器打开 http://localhost:8080
#
# 如果提示「无法加载文件，因为在此系统上禁止运行脚本」，
# 用根目录的 run-windows.bat 双击启动，或先执行：
#   Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass

$ErrorActionPreference = 'Stop'

# ================= 按需修改 =================
$SourceDir = 'D:\l2m\source'      # 源图库（只读使用，程序不会往里写）
$OutputDir = 'D:\l2m\output'      # 转换产物
$DataDir   = 'D:\l2m\data'        # SQLite 数据库
$WebAddr   = ':8080'
$ExifTool  = 'C:\Users\yuhao.gao\AppData\Local\Programs\ExifTool\exiftool.exe'
$HeifConv  = ''                   # 装了 heif-convert 就填绝对路径，否则留空
# ============================================

$root = Split-Path -Parent $PSScriptRoot
$exe  = Join-Path $root 'live2motion.exe'

if (-not (Test-Path $exe)) {
    Write-Host "找不到 $exe" -ForegroundColor Red
    Write-Host "请先在项目根目录构建：" -ForegroundColor Yellow
    Write-Host "  `$env:CGO_ENABLED=0; go build -o live2motion.exe ./cmd/live2motion" -ForegroundColor Gray
    exit 1
}

# 源目录：由你自己建并放文件（程序刻意不会自动创建源目录）
if (-not (Test-Path $SourceDir)) {
    New-Item -ItemType Directory -Path $SourceDir -Force | Out-Null
    Write-Host "已创建源目录：$SourceDir" -ForegroundColor Yellow
}
# 输出/数据目录：程序也会自动创建，这里提前建好便于查看
New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null
New-Item -ItemType Directory -Path $DataDir   -Force | Out-Null

$env:L2M_SOURCE_DIR = $SourceDir
$env:L2M_OUTPUT_DIR = $OutputDir
$env:L2M_DATA_DIR   = $DataDir
$env:L2M_WEB_ADDR   = $WebAddr
$env:L2M_LOG_LEVEL  = 'info'

if (Test-Path $ExifTool) {
    $env:L2M_EXIFTOOL = $ExifTool
} else {
    Write-Host "未找到 exiftool：$ExifTool" -ForegroundColor Yellow
    Write-Host "没有 exiftool 就读不到 ContentIdentifier，配对一定为空。" -ForegroundColor Gray
}
if ($HeifConv -and (Test-Path $HeifConv)) {
    $env:L2M_HEIF_CONVERT = $HeifConv
}

$fileCount = (Get-ChildItem -Path $SourceDir -Recurse -File -ErrorAction SilentlyContinue | Measure-Object).Count

Write-Host ""
Write-Host "──────────────────────────────────────────────" -ForegroundColor DarkGray
Write-Host " 源目录   $SourceDir   （当前 $fileCount 个文件）" -ForegroundColor Cyan
Write-Host " 输出目录 $OutputDir" -ForegroundColor Cyan
Write-Host " 数据目录 $DataDir" -ForegroundColor Cyan
Write-Host " 面板     http://localhost:8080" -ForegroundColor Cyan
Write-Host "──────────────────────────────────────────────" -ForegroundColor DarkGray

$hasHeif = $false
if ($HeifConv -and (Test-Path $HeifConv)) {
    $hasHeif = $true
} elseif (Get-Command heif-convert -ErrorAction SilentlyContinue) {
    $hasHeif = $true
}

if (-not $hasHeif) {
    Write-Host ""
    Write-Host "提示：本机没有 heif-convert，转换功能会被暂停（面板顶部会显示原因）。" -ForegroundColor Yellow
    Write-Host "      现在仍然可以完整验证：扫描 → 读 UUID → 配对 → 入队。" -ForegroundColor Gray
    Write-Host "      想做真实转换，用 Docker 跑：docker compose up -d --build" -ForegroundColor Gray
}

if ($fileCount -eq 0) {
    Write-Host ""
    Write-Host "提示：源目录是空的。请把成对的 .HEIC 与 .MOV/.mp4 放进去。" -ForegroundColor Yellow
    Write-Host "      源目录可以是任意目录，按 年/月 分子文件夹即可（程序递归扫描）。" -ForegroundColor Gray
}

Write-Host ""
Write-Host "按 Ctrl+C 停止服务" -ForegroundColor DarkGray
Write-Host ""

& $exe --web
