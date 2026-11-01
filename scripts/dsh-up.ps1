# dsh-up.ps1 —— 把 dsh 的对话界面跑在独立端口上，接 Tenon 的 MCP 工具。
#
# 为什么不用 3080：dsh 的默认端口常常已经被用户自己那个 dsh 实例占着（实测 2026-11-01
# 有一个 10-31 起的进程在 3080）。抢端口的结果是 `startup failed: EADDRINUSE`，
# 而它会在 spawn 完 MCP 子进程之后才失败，看起来像 MCP 坏了。
#
# 为什么这个脚本要抓 URL：dsh web 有浏览器信任栅栏 —— 直接开 http://127.0.0.1:3080/
# 只会得到 `dsh web authentication required`（401）。唯一入口是启动时打印的那条
# `http://host:port/?token=<随机>`（实测 dsh-client-connection：token 每进程随机、
# 只存在内存里、不落盘），访问它才种下 HttpOnly cookie（默认 30 天、绑定 authority）。
# 所以「把 URL 记下来」不是方便，是**唯一**能拿到入口的方式。
#
# 前置：
#   1) 先跑 scripts/dev-up.ps1（宿主与三个模块要在 8080–8083 上）
#   2) out/dev/bin/tenon-mcp.exe 已构建（dev-up 会构建）
#   3) C:\Users\<you>\.dsh\profiles\web\cordis.patch.yml 里已注册 tenon-mcp 条目
#
# 用法：powershell -NoProfile -File scripts/dsh-up.ps1 [-Port 3081]

param(
    [int]$Port = 3081,
    [string]$BindHost = '127.0.0.1'
)

$ErrorActionPreference = 'Stop'

function Fail([string]$msg) {
    Write-Host "✗ $msg" -ForegroundColor Red
    exit 1
}

if (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
    Fail ("端口 $Port 已被占用。注意：带 token 的入口 URL 只存在那个进程的内存里，读不到；" +
          "要么用它当初打印的那条 URL，要么停掉它再跑本脚本。")
}

$repo = Split-Path -Parent $PSScriptRoot
$mcp = Join-Path $repo 'out\dev\bin\tenon-mcp.exe'
if (-not (Test-Path $mcp)) { Fail "缺少 $mcp：先跑 scripts/dev-up.ps1" }

# 宿主必须在：MCP 只是转发层，宿主不在它什么也答不上来。
try {
    $null = Invoke-RestMethod 'http://127.0.0.1:8080/tenon/modules' -TimeoutSec 3
} catch {
    Fail "宿主 8080 不可达：先跑 scripts/dev-up.ps1"
}

$profileFile = Join-Path $HOME '.dsh\profiles\web\cordis.patch.yml'
if (-not (Test-Path $profileFile)) { Fail "找不到 dsh 配置：$profileFile" }
if (-not (Select-String -Path $profileFile -Pattern 'tenon-mcp' -SimpleMatch -Quiet)) {
    Write-Host "⚠ dsh 配置里没有 tenon-mcp 条目：dsh 能起来，但工具列表里不会有 mcp__tenon__*" -ForegroundColor Yellow
}

$work = Join-Path $repo 'out\dev'
$logDir = Join-Path $work 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$outFile = Join-Path $logDir 'dsh-web.out'
$errFile = Join-Path $logDir 'dsh-web.err'
$urlFile = Join-Path $work 'dsh-url.txt'
Remove-Item $outFile, $errFile -ErrorAction SilentlyContinue

Write-Host "启动 dsh web（$BindHost`:$Port）…" -ForegroundColor Cyan

# 后台起，不抢终端：入口 URL 要从 stdout 里抓，前台跑就没法回头读日志。
$proc = Start-Process -FilePath 'node' `
    -ArgumentList @('E:\npm-global\node_modules\@deepseek-ai\dsh\lib\bin.js', 'web', '--host', $BindHost, '--port', "$Port", '--no-open') `
    -RedirectStandardOutput $outFile -RedirectStandardError $errFile -PassThru -WindowStyle Hidden
$proc.Id | Set-Content (Join-Path $work 'dsh.pid') -Encoding ASCII

$entry = ''
$deadline = (Get-Date).AddSeconds(90)
while ((Get-Date) -lt $deadline) {
    if (Test-Path $outFile) {
        $line = Get-Content $outFile -Raw -ErrorAction SilentlyContinue |
            Select-String -Pattern '\?token=' -AllMatches
        if ($line) {
            $entry = (($line.Line -split "`n") | Where-Object { $_ -match '\?token=' } | Select-Object -First 1)
            $entry = ($entry -replace '^\s*dsh web:\s*', '').Trim()
            if ($entry) { break }
        }
    }
    Start-Sleep -Milliseconds 400
}

if (-not $entry) {
    Write-Host "✗ 90 秒内没抓到入口 URL，看 $errFile" -ForegroundColor Red
    Get-Content $errFile -Tail 15 -ErrorAction SilentlyContinue
    exit 1
}

$entry | Set-Content $urlFile -Encoding ASCII

Write-Host ''
Write-Host '✓ dsh web 已启动' -ForegroundColor Green
Write-Host "  入口（唯一可用的那条）：$entry" -ForegroundColor Yellow
Write-Host "  已写入：$urlFile"
Write-Host ''
Write-Host '  为什么必须用带 token 的那条：裸地址 http://127.0.0.1:3080/ 会被 401' -ForegroundColor DarkGray
Write-Host '  （"dsh web authentication required"）。token 每进程随机、只在内存里；' -ForegroundColor DarkGray
Write-Host '  开一次这条 URL 就会种下 30 天的 HttpOnly cookie，之后裸地址在同一个浏览器里直接可用。' -ForegroundColor DarkGray
Write-Host '  cookie 绑定 authority：用 127.0.0.1 进的就别用 localhost 打开（等于另一个会话）。' -ForegroundColor DarkGray
Write-Host ''
Write-Host '  工具名：  mcp__tenon__registry_list / action_invoke / plan_preview / request_submit / pending_list / pending_decide / audit_query'
Write-Host "  日志：    $outFile / $errFile（stderr 里有 MCP 握手行，例如 tenon-mcp: host=…）"
Write-Host "  停止：    taskkill /PID $($proc.Id) /F"
