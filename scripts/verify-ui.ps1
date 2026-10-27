#!/usr/bin/env powershell
# 一次性验收：起 stub 全链路 → 造一张待批卡 → 用 Edge(CDP) 断言「卡面只有人话、技术信息在折叠区」。
# 产物：out/ui-verify/card.png（收起态）、card-open.png（展开态）、打印的 JSON 断言。
#
# 为什么要浏览器：R5 断言的是**渲染结果**（可见文本里没有 action id、details 默认收起），
# 读源码只能猜。这里用 Edge 的 --remote-debugging-port 驱动，零额外依赖。
[CmdletBinding()]
param(
	[int]$PendingTimeoutSec = 40,
	[int]$DebugPort = 9222
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root

$work = Join-Path $root 'out/ui-verify'
$bin = Join-Path $work 'bin'
$logDir = Join-Path $work 'logs'
$stateDir = Join-Path $work 'agent-state'
New-Item -ItemType Directory -Force -Path $bin, $logDir | Out-Null
foreach ($p in $stateDir, (Join-Path $work 'inbound')) {
	if (Test-Path $p) { Remove-Item -Recurse -Force $p }
}
New-Item -ItemType Directory -Force -Path $stateDir, (Join-Path $work 'inbound') | Out-Null

function Fail([string]$m) { Write-Host "✗ $m" -ForegroundColor Red; exit 1 }

foreach ($port in 8080, 8081, 8082, 8083, 8099, $DebugPort) {
	if ([bool](Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)) {
		Fail "端口 $port 被占用，先停掉占者"
	}
}

foreach ($svc in 'host', 'agent', 'inventory', 'procurement', 'quote') {
	& go build -o (Join-Path $bin "$svc.exe") "./cmd/$svc"
	if ($LASTEXITCODE -ne 0) { Fail "构建 $svc 失败" }
}
& go build -o (Join-Path $bin 'llmstub.exe') ./tools/llmstub
if ($LASTEXITCODE -ne 0) { Fail '构建 llmstub 失败' }

$hmacFile = Join-Path $work 'hmac-secret'
if (-not (Test-Path $hmacFile)) {
	$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
	$buf = New-Object byte[] 32
	$rng.GetBytes($buf)
	[System.IO.File]::WriteAllText($hmacFile, [System.BitConverter]::ToString($buf).Replace('-', '').ToLowerInvariant())
}

function Start-Svc([string]$name, [hashtable]$envMap) {
	$exe = Join-Path $bin "$name.exe"
	$saved = @{}
	foreach ($k in $envMap.Keys) {
		$saved[$k] = [Environment]::GetEnvironmentVariable($k)
		[Environment]::SetEnvironmentVariable($k, [string]$envMap[$k])
	}
	$p = Start-Process -FilePath $exe -WorkingDirectory $root -PassThru -NoNewWindow `
		-RedirectStandardOutput (Join-Path $logDir "$name.out") `
		-RedirectStandardError (Join-Path $logDir "$name.err")
	foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
	return $p
}

function Wait-Ready([string]$name, [string]$url, [int]$timeoutSec = 40) {
	$deadline = (Get-Date).AddSeconds($timeoutSec)
	while ((Get-Date) -lt $deadline) {
		try {
			if ((Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2).StatusCode -eq 200) { return }
		} catch { }
		Start-Sleep -Milliseconds 250
	}
	$tail = ''
	$errFile = Join-Path $logDir "$name.err"
	if (Test-Path $errFile) { $tail = (Get-Content $errFile -Tail 20 | Out-String) }
	Fail "$name 未就绪（$url）。stderr：`n$tail"
}

$procs = @()
$agentJob = $null
$edge = $null
try {
	$procs += Start-Svc 'llmstub' @{ PORT = '8099' }
	Wait-Ready 'llmstub' 'http://127.0.0.1:8099/healthz'

	$procs += Start-Svc 'inventory' @{ PORT = '8081'; TENON_MODULE_ID = 'inventory'; TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-inventory.jsonl" }
	$procs += Start-Svc 'procurement' @{ PORT = '8082'; TENON_MODULE_ID = 'procurement'; TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-procurement.jsonl" }
	$procs += Start-Svc 'quote' @{ PORT = '8083'; TENON_MODULE_ID = 'quote'; TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-quote.jsonl" }
	Wait-Ready 'inventory' 'http://127.0.0.1:8081/readyz'
	Wait-Ready 'procurement' 'http://127.0.0.1:8082/readyz'
	Wait-Ready 'quote' 'http://127.0.0.1:8083/readyz'

	# 不开 TEST_MODE：入站端点要留着（agent 每 tick 会 poll 它们，404 只是噪音但没必要）。
	$procs += Start-Svc 'host' @{
		PORT = '8080'; TENON_STRICT_RULES = '1'
		TENON_MODULES = 'quote=http://127.0.0.1:8083,inventory=http://127.0.0.1:8081,procurement=http://127.0.0.1:8082'
		TENON_USERS_FILE = 'config/users.dev.json'; TENON_SESSION_SECRET = 'dev'
		TENON_SKILLS_DIR = 'skills'; TENON_POLICY_FILE = 'config/policies.dev.json'
		TENON_GRANT_FILE = 'config/grants.dev.json'; TENON_INBOUND_DIR = "$work/inbound"
		TENON_AUDIT_PATH = "$work/audit-host.jsonl"; TENON_UI_DIR = 'ui'
	}
	Wait-Ready 'host' 'http://127.0.0.1:8080/readyz'

	$resultFile = Join-Path $work 'agent-result.jsonl'
	if (Test-Path $resultFile) { Remove-Item -Force $resultFile }
	$agentEnv = @{
		TENON_HOST_URL = 'http://127.0.0.1:8080'; TENON_AGENT_ID = 'ag_fulfillment'
		TENON_ON_BEHALF_OF = 'user:alice'; TENON_ONCE = '1'
		TENON_AGENT_USER = 'agent'; TENON_AGENT_PASSWORD = 'agent-dev'
		TENON_AGENT_STATE_DIR = $stateDir; TENON_RESULT_FILE = $resultFile
		TENON_SKILLS_DIR = 'skills'; TENON_LLM_TIMEOUT = '60s'
		TENON_LLM_BASE_URL = 'http://127.0.0.1:8099/v1'; TENON_LLM_API_KEY = 'dummy'; TENON_LLM_MODEL = 'stub-plan'
	}
	$agentExe = Join-Path $bin 'agent.exe'
	$agentJob = Start-Job -Name uiverify -ScriptBlock {
		param($exe, $workDir, $logPath, $envMap)
		Set-Location $workDir
		foreach ($k in $envMap.Keys) { Set-Item -Path "env:$k" -Value ([string]$envMap[$k]) }
		while ($true) { & $exe *>> $logPath; Start-Sleep -Milliseconds 300 }
	} -ArgumentList $agentExe, $root, (Join-Path $logDir 'agent.err'), $agentEnv

	# 造一张待批卡：quote 建单 → quote.completed 进发件箱 → agent 规划出 DeductStock → 策略要审批 → 挂起。
	# 请求体必须落文件再 --data-binary @file：PowerShell 把含双引号的字符串交给原生程序时会剥引号，
	# 直接用 -d 会让服务端收到 invalid character 'q'（实测踩过）。
	$quoteBodyPath = Join-Path $work 'quote-body.json'
	[System.IO.File]::WriteAllText($quoteBodyPath, '{"quoteNo":"Q-UI-1","lines":[{"sku":"STD-1001","qty":12}]}', (New-Object System.Text.UTF8Encoding($false)))
	& curl.exe -s -X POST 'http://127.0.0.1:8083/tenon/actions/quote.CreateStandardPartQuote' `
		-H 'Content-Type: application/json' -H 'Tenon-Version: 0.1' -H 'Idempotency-Key: tenon:ui/1' `
		--data-binary "@$quoteBodyPath" -o (Join-Path $work 'quote-resp.json')
	if ($LASTEXITCODE -ne 0) { Fail '触发报价失败' }

	$deadline = (Get-Date).AddSeconds($PendingTimeoutSec)
	$pendingFile = $null
	while ((Get-Date) -lt $deadline) {
		$pendingFile = Get-ChildItem (Join-Path $stateDir 'pending') -Filter '*.json' -ErrorAction SilentlyContinue | Select-Object -First 1
		if ($pendingFile) { break }
		Start-Sleep -Milliseconds 400
	}
	if (-not $pendingFile) { Fail "等了 $PendingTimeoutSec 秒没有 pending 文件（agent 没规划/没挂起）" }
	Write-Host "✓ pending 已生成：$($pendingFile.Name)" -ForegroundColor Green

	# 两个身份的 token：bob 是仓管（应看到批准按钮），alice 是业务员（不该看到）。
	$loginBodyPath = Join-Path $work 'login-body.json'
	[System.IO.File]::WriteAllText($loginBodyPath, '{"user":"bob","password":"bob-dev"}', (New-Object System.Text.UTF8Encoding($false)))
	$bobRaw = & curl.exe -s -X POST 'http://127.0.0.1:8080/tenon/session' -H 'Content-Type: application/json' --data-binary "@$loginBodyPath"
	$bobToken = ($bobRaw | ConvertFrom-Json).token
	if (-not $bobToken) { Fail "bob 登录失败：$bobRaw" }

	[System.IO.File]::WriteAllText($loginBodyPath, '{"user":"alice","password":"alice-dev"}', (New-Object System.Text.UTF8Encoding($false)))
	$aliceRaw = & curl.exe -s -X POST 'http://127.0.0.1:8080/tenon/session' -H 'Content-Type: application/json' --data-binary "@$loginBodyPath"
	$aliceToken = ($aliceRaw | ConvertFrom-Json).token
	if (-not $aliceToken) { Fail "alice 登录失败：$aliceRaw" }
	$token = $bobToken

	# 等公告板出现这条（agent 每 tick 重推，通常 1 秒内）。
	$boardDeadline = (Get-Date).AddSeconds(20)
	$boardOK = $false
	while ((Get-Date) -lt $boardDeadline) {
		$board = & curl.exe -s 'http://127.0.0.1:8080/tenon/pending' -H "Authorization: Bearer $bobToken"
		try { if ((($board | ConvertFrom-Json).items | Measure-Object).Count -ge 1) { $boardOK = $true; break } } catch { }
		Start-Sleep -Milliseconds 500
	}
	if (-not $boardOK) { Fail '公告板上一直没有条目（agent 推送失败？）' }
	Write-Host '✓ 公告板已有待批条目' -ForegroundColor Green

	# 起 Edge（headless）并等 CDP 端口。
	$edgePath = 'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe'
	if (-not (Test-Path $edgePath)) { Fail "找不到 Edge：$edgePath" }
	$profile = Join-Path $work 'edge-profile'
	if (Test-Path $profile) { Remove-Item -Recurse -Force $profile }
	$edge = Start-Process -FilePath $edgePath -PassThru -ArgumentList @(
		'--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
		"--remote-debugging-port=$DebugPort", "--user-data-dir=$profile", 'http://127.0.0.1:8080/'
	)
	$cdpDeadline = (Get-Date).AddSeconds(25)
	while ((Get-Date) -lt $cdpDeadline) {
		try { if ((Invoke-WebRequest -Uri "http://127.0.0.1:$DebugPort/json/version" -UseBasicParsing -TimeoutSec 2).StatusCode -eq 200) { break } } catch { }
		Start-Sleep -Milliseconds 300
	}

	Write-Host '=== 浏览器断言（bob：仓管，应看到批准按钮）===' -ForegroundColor Cyan
	& node scripts/cdp-check.mjs 'http://127.0.0.1:8080' $DebugPort $bobToken (Join-Path $work 'card-bob.png') 'bob' 'role:warehouse_manager,role:procurement_manager' '1'
	$bobExit = $LASTEXITCODE
	if ($bobExit -ne 0) { Write-Host "bob 断言失败（$bobExit）" -ForegroundColor Red; exit $bobExit }

	Write-Host '=== 浏览器断言（alice：业务员，不该看到批准按钮）===' -ForegroundColor Cyan
	& node scripts/cdp-check.mjs 'http://127.0.0.1:8080' $DebugPort $aliceToken (Join-Path $work 'card-alice.png') 'alice' 'role:warehouse_clerk' '0'
	$aliceExit = $LASTEXITCODE
	Write-Host "断言退出码：bob=$bobExit alice=$aliceExit" -ForegroundColor Cyan
	if ($aliceExit -ne 0) { exit $aliceExit }
}
finally {
	if ($agentJob) { Stop-Job $agentJob -ErrorAction SilentlyContinue; Remove-Job $agentJob -Force -ErrorAction SilentlyContinue }
	if ($edge -and -not $edge.HasExited) { Stop-Process -Id $edge.Id -Force -ErrorAction SilentlyContinue }
	foreach ($p in $procs) { if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } }
}
