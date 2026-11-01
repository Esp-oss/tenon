#!/usr/bin/env powershell
# 本地开发栈：起 llmstub + 三个模块 + host + agent，**常驻不退出**，供浏览器手工验收。
#
#   powershell -NoProfile -File scripts/dev-up.ps1          # 桩规划器（默认，结果可预期）
#   powershell -NoProfile -File scripts/dev-up.ps1 -Real    # 真模型（读 docker/secrets/llm.key）
#   powershell -NoProfile -File scripts/dev-down.ps1        # 收尸
#
# 与 d1-e2e.ps1 的区别：那个跑完就杀（判定用），这个留着让人点。
# 与 verify-ui.ps1 的区别：那个自己断言完就杀（回归用），这个不自动断言。
#
# R1（模块动态注册）之后：TENON_MODULES 已删除，模块靠 TENON_HOST 自注册 + 每 5s 心跳；
# 宿主靠 TENON_HMAC_SECRET_FILE 验签（缺则拒绝启动，D7）、靠 TENON_REGISTRY_EVENTS 落
# 注册事件链。所以「模块起好了」的判据是 GET /tenon/modules 里看得到它，不是端口通。
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
# tenon-mcp 不常驻：它由 dsh 以 stdio 拉起，这里只负责把它构建到 $bin。
# 少了它，dsh 那边的 MCP 条目会指向一个不存在的可执行文件，症状是「工具一个都没有」。
foreach ($svc in 'host', 'agent', 'inventory', 'procurement', 'quote', 'tenon-mcp') {
	& go build -o (Join-Path $bin "$svc.exe") "./cmd/$svc"
	if ($LASTEXITCODE -ne 0) { Fail "构建 $svc 失败" }
}
& go build -o (Join-Path $bin 'llmstub.exe') ./tools/llmstub
if ($LASTEXITCODE -ne 0) { Fail '构建 llmstub 失败' }

# 模块的 /readyz 会检查 hmac_secret 与 audit_file，缺一项永远不就绪——不是可选配置。
# 宿主与模块必须拿到**同一份**密钥：注册/心跳的验签两边取值不同就只会得到 401。
$hmacFile = Join-Path $work 'hmac-secret'
if (-not (Test-Path $hmacFile)) {
	$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
	$buf = New-Object byte[] 32
	$rng.GetBytes($buf)
	[System.IO.File]::WriteAllText($hmacFile, [System.BitConverter]::ToString($buf).Replace('-', '').ToLowerInvariant())
}

# 票据签名密钥：宿主缺它拒绝启动（P0-1）。公钥文件同时给三个模块做信任集，
# 两边必须成对，所以一起生成、一起放在 $work 里。
$ticketKey = Join-Path $work 'host-ticket.key'
$ticketPub = Join-Path $work 'host-ticket.pub.json'
if (-not (Test-Path $ticketKey)) {
	& go run ./cmd/tenon keygen --out $ticketKey --pub $ticketPub
	if ($LASTEXITCODE -ne 0) { Fail '生成票据签名密钥失败' }
}

# R1 自注册的三处接线：模块的 TENON_HOST、宿主的事件链路径、预期可见的模块集合。
# 事件链**保留**跨会话追加（append-only 的意义就在这里）：开发期重启宿主是最常见的动作，
# 每次都清空就看不出「模块什么时候失联过」。
$registryEvents = Join-Path $work 'registry-events.jsonl'
$hostURL = "http://127.0.0.1:$HostPort"
$expectedModules = @('inventory', 'procurement', 'quote')

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

# Get-VisibleModules 读可见模块清单（GET /tenon/modules = enabled && alive，D5 无 static 例外）。
# $null 表示这次没读到（宿主没起/正在重启）；空数组表示读到了，但一个模块都没注册上。
# 返回值用 `,@(...)` 包一层：PowerShell 会把空数组压成 $null，不包的话这两种情形就分不开。
function Get-VisibleModules([string]$baseURL) {
	try {
		$resp = Invoke-WebRequest -Uri "$baseURL/tenon/modules" -UseBasicParsing -TimeoutSec 3
		if ($resp.StatusCode -ne 200) { return $null }
		return ,@(($resp.Content | ConvertFrom-Json).modules)
	} catch {
		return $null
	}
}

# Wait-ModulesVisible 轮询 GET /tenon/modules 直到预期模块全部可见。
#
# R1 之后这是唯一的「模块在场」判据：/readyz 200 只说明模块自己活着，宿主可能完全不知道
# 它存在（注册没发出去/签名不对/心跳还没轮到）。没有这一步，页面会打开但待批卡造不出来，
# 排查方向会偏到前端。
function Wait-ModulesVisible {
	param([string]$baseURL, [string[]]$expected, [int]$timeoutSec = 40)
	$deadline = (Get-Date).AddSeconds($timeoutSec)
	$last = '还没读到 GET /tenon/modules'
	while ((Get-Date) -lt $deadline) {
		$mods = Get-VisibleModules $baseURL
		if ($null -ne $mods) {
			$seen = @($mods | ForEach-Object { $_.id })
			$missing = @($expected | Where-Object { $seen -notcontains $_ })
			if ($missing.Count -eq 0) { return $seen }
			$last = "当前可见 [$($seen -join ', ')]，仍缺 [$($missing -join ', ')]"
		}
		Start-Sleep -Milliseconds 300
	}
	$tails = ''
	foreach ($name in 'host', 'inventory', 'procurement', 'quote') {
		$f = Join-Path $logDir "$name.err"
		if (Test-Path $f) { $tails += "[$name.err]`n$((Get-Content $f -Tail 10 -Encoding UTF8 | Out-String).Trim())`n" }
	}
	# dev-up 的宿主不带 TEST_MODE，所以没有管理面全量视图可打——直接给可见清单与日志。
	Fail ("等了 $timeoutSec 秒仍未在 GET /tenon/modules 里看到全部预期模块（$($expected -join ', ')）：$last`n" +
		"注册事件链：$registryEvents`n进程日志尾部：`n$tails")
}

try {
	Start-Svc 'llmstub' @{ PORT = '8099' } | Out-Null
	Wait-Ready 'llmstub' 'http://127.0.0.1:8099/healthz' 20

	Start-Svc 'inventory' @{
		PORT = '8081'; TENON_MODULE_ID = 'inventory'; TENON_TEST_MODE = '1'
		TENON_HOST = $hostURL
		TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-inventory.jsonl"
		TENON_TRUSTED_KEYS = $ticketPub; TENON_TICKET_MODE = 'enforce'
	} | Out-Null
	Start-Svc 'procurement' @{
		PORT = '8082'; TENON_MODULE_ID = 'procurement'; TENON_TEST_MODE = '1'
		TENON_HOST = $hostURL
		TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-procurement.jsonl"
		TENON_TRUSTED_KEYS = $ticketPub; TENON_TICKET_MODE = 'enforce'
	} | Out-Null
	Start-Svc 'quote' @{
		PORT = '8083'; TENON_MODULE_ID = 'quote'; TENON_TEST_MODE = '1'
		TENON_HOST = $hostURL
		TENON_HMAC_SECRET_FILE = $hmacFile; TENON_MODULE_AUDIT_PATH = "$work/audit-quote.jsonl"
		TENON_TRUSTED_KEYS = $ticketPub; TENON_TICKET_MODE = 'enforce'
	} | Out-Null
	Wait-Ready 'inventory' 'http://127.0.0.1:8081/readyz'
	Wait-Ready 'procurement' 'http://127.0.0.1:8082/readyz'
	Wait-Ready 'quote' 'http://127.0.0.1:8083/readyz'

	# 模块先起、host 后起：模块的注册推送会自己重试（失败每 5s 一次），所以顺序反过来也能
	# 收敛，但先起模块能让「模块注册 → 补校验挂起规则」发生在模块**已经监听**之后，规则
	# 一次到位。STRICT_RULES=1 仍把「规则没加载」变硬：挂起（模块还没注册）不算失败。
	Start-Svc 'host' @{
		PORT = "$HostPort"; TENON_STRICT_RULES = '1'
		TENON_HMAC_SECRET_FILE = $hmacFile; TENON_REGISTRY_EVENTS = $registryEvents
		TENON_USERS_FILE = 'config/users.dev.json'; TENON_SESSION_SECRET = 'dev'
		TENON_SKILLS_DIR = 'skills'; TENON_POLICY_FILE = 'config/policies.dev.json'
		TENON_GRANT_FILE = 'config/grants.dev.json'; TENON_INBOUND_DIR = "$work/inbound"
		TENON_AUDIT_PATH = "$work/audit-host.jsonl"; TENON_UI_DIR = 'ui'
		TENON_HOST_SIGNING_KEY = $ticketKey
	} | Out-Null
	Wait-Ready 'host' "http://127.0.0.1:$HostPort/readyz"
	# 端口通 ≠ 注册完：等清单齐了再起 agent，否则第一张待批卡会因为「agent 看不到模块」造不出来。
	$visible = Wait-ModulesVisible -baseURL $hostURL -expected $expectedModules -timeoutSec 40
	Write-Host "✓ 三模块已自注册并可见：$($visible -join ', ')" -ForegroundColor Green

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
		# 种一张待批卡：走**入站请求帧**，不直连模块。
		#   直连模块写（旧实现）在 P0-1 之后必然 403 TICKET_MISSING —— 模块要求宿主签发的票，
		#   而脚本手里没有；旧版本把失败吞掉了，于是「已预置一张待批卡」这句话是假的。
		#   改走入站帧以后，规划由 agent 做、卡由 agent 落，与人在对话里说一句话走的是同一条路径。
		$reqBody = Join-Path $work 'seed-request.json'
		[System.IO.File]::WriteAllText($reqBody, '{"type":"tenon.request","text":"把 STD-1001 的 12 件库存扣掉，给 Q-LOCAL 这张单发货"}', (New-Object System.Text.UTF8Encoding($false)))
		try {
			$session = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$HostPort/tenon/session" -ContentType 'application/json' -Body '{"user":"alice","password":"alice-dev"}'
			$seed = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$HostPort/tenon/requests" -ContentType 'application/json' -Headers @{ Authorization = "Bearer $($session.token)" } -InFile $reqBody
			Write-Host ("  种子请求：seq $($seed.seq)  requestId $($seed.requestId)") -ForegroundColor DarkGray
		} catch {
			Write-Host ('  ! 种子请求失败：' + $_.Exception.Message) -ForegroundColor Yellow
		}
		# 等 agent 规划完并落卡（桩模型约 2-3 秒；真模型一个回合 10-40 秒）。
		$deadline = (Get-Date).AddSeconds(45)
		while ((Get-Date) -lt $deadline) {
			if (Get-ChildItem (Join-Path $stateDir 'pending') -Filter '*.json' -ErrorAction SilentlyContinue) { break }
			Start-Sleep -Milliseconds 400
		}
		$pendingFiles = @(Get-ChildItem (Join-Path $stateDir 'pending') -Filter '*.json' -ErrorAction SilentlyContinue)
		if ($pendingFiles.Count -eq 0) {
			Write-Host '  ! 45 秒内没等到待批卡：看 out/dev/logs/agent.err（规划失败/被闸门拒都是这个症状）' -ForegroundColor Yellow
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
