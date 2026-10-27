#!/usr/bin/env powershell
# 本地开发栈：起 llmstub + 三个模块 + host + agent，**常驻不退出**，供浏览器手工验收。
#
#   powershell -NoProfile -File scripts/dev-up.ps1          # 桩规划器（默认，结果可预期）
#   powershell -NoProfile -File scripts/dev-up.ps1 -Real    # 真模型（读 docker/secrets/llm.key）
#   powershell -NoProfile -File scripts/dev-down.ps1        # 收尸
#
# 与 d1-e2e.ps1 的区别：那个跑完就杀（判定用），这个留着让人点。
# 与 verify-ui.ps1 的区别：那个自己断言完就杀（回归用），这个不自动断言。
[CmdletBinding()]
param(
	[switch]$Real,
	[switch]$NoDemoCard,
	[int]$HostPort = 8080
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root

$work = Join-Path $root 'out/dev'
$bin = Join-Path $work 'bin'
$logDir = Join-Path $work 'logs'
$stateDir = Join-Path $work 'agent-state'
$pidFile = Join-Path $work 'pids.txt'

function Fail([string]$m) { Write-Host "✗ $m" -ForegroundColor Red; exit 1 }

# 已经在跑就先停：否则新进程会撞端口，报错难读。
if (Test-Path $pidFile) {
	$alive = @()
	foreach ($line in Get-Content $pidFile) {
		$parts = $line -split '='
		if ($parts.Count -eq 2) {
			$procId = 0
			if ([int]::TryParse($parts[1], [ref]$procId)) {
				$p = Get-Process -Id $procId -ErrorAction SilentlyContinue
				if ($p) { $alive += "$($parts[0])=$procId" }
			}
		}
	}
	if ($alive.Count -gt 0) { Fail "上一次的进程还在跑（$($alive -join ', ')）。先执行：powershell -NoProfile -File scripts/dev-down.ps1" }
	Remove-Item -Force $pidFile
}

foreach ($port in 8080, 8081, 8082, 8083, 8099) {
	if ([bool](Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)) {
		Fail "端口 $port 被占用，先停掉占者"
	}
}
if ($HostPort -ne 8080 -and [bool](Get-NetTCPConnection -LocalPort $HostPort -State Listen -ErrorAction SilentlyContinue)) {
	Fail "端口 $HostPort 被占用"
}

New-Item -ItemType Directory -Force -Path $bin, $logDir, $stateDir, (Join-Path $work 'inbound') | Out-Null
foreach ($svc in 'host', 'agent', 'inventory', 'procurement', 'quote') {
	& go build -o (Join-Path $bin "$svc.exe") "./cmd/$svc"
	if ($LASTEXITCODE -ne 0) { Fail "构建 $svc 失败" }
}
& go build -o (Join-Path $bin 'llmstub.exe') ./tools/llmstub
if ($LASTEXITCODE -ne 0) { Fail '构建 llmstub 失败' }

# 模块的 /readyz 会检查 hmac_secret 与 audit_file，缺一项永远不就绪——不是可选配置。
$hmacFile = Join-Path $work 'hmac-secret'
if (-not (Test-Path $hmacFile)) {
	$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
	$buf = New-Object byte[] 32
	$rng.GetBytes($buf)
	[System.IO.File]::WriteAllText($hmacFile, [System.BitConverter]::ToString($buf).Replace('-', '').ToLowerInvariant())
}

$started = @()
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
	$script:started += "$name=$($p.Id)"
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
	if (Test-Path $errFile) { $tail = (Get-Content $errFile -Tail 15 | Out-String) }
	Fail "$name 未就绪（$url）。stderr：`n$tail"
}

try {
	Start-Svc 'llmstub' @{ PORT = '8099' } | Out-Null
	Wait-Ready 'llmstub' 'http://127.0.0.1:8099/healthz' 20

	Start-Svc 'inventory' @{
		PORT = '8081'; TENON_MODULE_ID = 'inventory'; TENON_TEST_MODE = '1'
		TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-inventory.jsonl"
	} | Out-Null
	Start-Svc 'procurement' @{
		PORT = '8082'; TENON_MODULE_ID = 'procurement'; TENON_TEST_MODE = '1'
		TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-procurement.jsonl"
	} | Out-Null
	Start-Svc 'quote' @{
		PORT = '8083'; TENON_MODULE_ID = 'quote'; TENON_TEST_MODE = '1'
		TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-quote.jsonl"
	} | Out-Null
	Wait-Ready 'inventory' 'http://127.0.0.1:8081/readyz'
	Wait-Ready 'procurement' 'http://127.0.0.1:8082/readyz'
	Wait-Ready 'quote' 'http://127.0.0.1:8083/readyz'

	# 模块先起、host 后起：host 加载期要拉各模块 actions 校验业务规则；STRICT_RULES=1 把
	# 「规则没加载」变成启动失败（而不是日志里少一行）。
	Start-Svc 'host' @{
		PORT = "$HostPort"; TENON_STRICT_RULES = '1'
		TENON_MODULES = 'quote=http://127.0.0.1:8083,inventory=http://127.0.0.1:8081,procurement=http://127.0.0.1:8082'
		TENON_USERS_FILE = 'config/users.dev.json'; TENON_SESSION_SECRET = 'dev'
		TENON_SKILLS_DIR = 'skills'; TENON_POLICY_FILE = 'config/policies.dev.json'
		TENON_GRANT_FILE = 'config/grants.dev.json'; TENON_INBOUND_DIR = "$work/inbound"
		TENON_AUDIT_PATH = "$work/audit-host.jsonl"; TENON_UI_DIR = 'ui'
	} | Out-Null
	Wait-Ready 'host' "http://127.0.0.1:$HostPort/readyz"

	$agentEnv = @{
		TENON_HOST_URL = "http://127.0.0.1:$HostPort"; TENON_AGENT_ID = 'ag_fulfillment'
		TENON_ON_BEHALF_OF = 'user:alice'; TENON_ONCE = ''
		TENON_AGENT_USER = 'agent'; TENON_AGENT_PASSWORD = 'agent-dev'
		TENON_AGENT_STATE_DIR = $stateDir; TENON_RESULT_FILE = "$work/agent-result.jsonl"
		TENON_SKILLS_DIR = 'skills'; TENON_LLM_TIMEOUT = '90s'
	}
	if ($Real) {
		$keyFile = 'docker/secrets/llm.key'
		if (-not (Test-Path $keyFile)) { Fail "缺 $keyFile（-Real 需要它）" }
		$agentEnv.TENON_LLM_BASE_URL = 'https://api.deepseek.com'
		$agentEnv.TENON_LLM_API_KEY_FILE = (Resolve-Path $keyFile).Path
		$agentEnv.TENON_LLM_MODEL = 'deepseek-flash'
	} else {
		$agentEnv.TENON_LLM_BASE_URL = 'http://127.0.0.1:8099/v1'
		$agentEnv.TENON_LLM_API_KEY = 'dummy'
		$agentEnv.TENON_LLM_MODEL = 'stub-plan'
	}
	# 常驻（不设 TENON_ONCE）：交互式开发要的是持续可用；D1 的「每轮新进程」只在判定时需要。
	Start-Svc 'agent' $agentEnv | Out-Null

	Start-Sleep -Seconds 2

	if (-not $NoDemoCard) {
		# 造一张待批卡，让人一打开页面就有东西可点（否则待批区是空的）。
		$bodyPath = Join-Path $work 'quote-body.json'
		[System.IO.File]::WriteAllText($bodyPath, '{"quoteNo":"Q-LOCAL-1","lines":[{"sku":"STD-1001","qty":12}]}', (New-Object System.Text.UTF8Encoding($false)))
		& curl.exe -s -X POST 'http://127.0.0.1:8083/tenon/actions/quote.CreateStandardPartQuote' `
			-H 'Content-Type: application/json' -H 'Tenon-Version: 0.1' -H 'Idempotency-Key: tenon:dev/1' `
			--data-binary "@$bodyPath" -o (Join-Path $work 'quote-resp.json')
		$deadline = (Get-Date).AddSeconds(30)
		while ((Get-Date) -lt $deadline) {
			if (Get-ChildItem (Join-Path $stateDir 'pending') -Filter '*.json' -ErrorAction SilentlyContinue) { break }
			Start-Sleep -Milliseconds 400
		}
	}

	$started | Set-Content $pidFile -Encoding ASCII

	Write-Host ''
	Write-Host '✓ 本地栈已启动（常驻）' -ForegroundColor Green
	Write-Host "  页面：    http://127.0.0.1:$HostPort/"
	Write-Host '  账号：    alice / alice-dev（业务员）    bob / bob-dev（仓管，能批准）    agent / agent-dev（服务账号）'
	if ($Real) {
		Write-Host '  规划器：  真模型 deepseek-flash（注意：入参契约缺失是已知缺口，写动作可能规划失败）' -ForegroundColor Yellow
	} else {
		Write-Host '  规划器：  桩（固定规划「扣减 12 件 STD-1001」；显示的是模板渲染出的白话，不是真理解）'
	}
	if (-not $NoDemoCard) { Write-Host '  已预置：  一张待批卡（bob 登录即可看到并点「批准」）' }
	Write-Host '  日志：    out/dev/logs/*.err'
	Write-Host '  停止：    powershell -NoProfile -File scripts/dev-down.ps1'
	Write-Host ''
}
catch {
	# 起一半失败：把已经起的进程收掉，别留垃圾。
	foreach ($line in $started) {
		$parts = $line -split '='
		if ($parts.Count -eq 2) { Stop-Process -Id ([int]$parts[1]) -Force -ErrorAction SilentlyContinue }
	}
	throw
}
