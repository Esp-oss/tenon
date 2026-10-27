#!/usr/bin/env powershell
# D1 真模型判定：起三个模块 + host + agent 重启循环，跑 tenon d1 run，收尸。
#
# 为什么需要脚本：D1 的前置条件比「跑个测试」苛刻——host 与三个模块都要测试态
# （否则两轮输入无法相同）、agent 每轮必须是新进程（模块 reset 会把发件箱序号清回 0，
# 长驻 agent 的游标会把 B 轮新事件当成已读吞掉，见 internal/d1/runner.go:26-32）。
# 手工会漏，漏了就得到「等不到结果行」的假 fail。
#
# 注意：本文件必须带 UTF-8 BOM 保存。Windows PowerShell 5.1 读无 BOM 的 UTF-8
# 会按系统代码页解码，中文串会把引号吃掉并报「string is missing the terminator」。
#
# 用法（本机没有 pwsh 时用 powershell）：
#   powershell -NoProfile -File scripts/d1-e2e.ps1                      # 真模型（读 docker/secrets/llm.key）
#   powershell -NoProfile -File scripts/d1-e2e.ps1 -Stub                # 只验编排：用 tools/llmstub，不出判定
#   powershell -NoProfile -File scripts/d1-e2e.ps1 -Model deepseek-v4-pro  # 换模型（见下方注意）
#
# 模型选择（2026-10-27 实测）：缺省用 deepseek-flash。deepseek-v4-pro 是**推理模型**，
# 同一条规划请求实测 600 token 全花在 reasoning 上、可见 content 为空，真实链路里
# 会把轮次拖过超时被杀——所以它不作为 D1 的缺省，想用就显式传。
[CmdletBinding()]
param(
	[string]$Model   = 'deepseek-flash',
	[string]$BaseUrl = 'https://api.deepseek.com',
	[string]$KeyFile = 'docker/secrets/llm.key',
	[switch]$Stub,
	[int]$RoundTimeoutSec = 180
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root

$out = Join-Path $root 'out/d1'
$bin = Join-Path $out 'bin'
$logDir = Join-Path $out 'logs'
$stateDir = Join-Path $out 'agent-state'
New-Item -ItemType Directory -Force -Path $bin, $logDir, (Join-Path $out 'inbound') | Out-Null

# 每次运行都从干净状态开始：上一轮留下的 pending/ 会被自检当成活条目重推，
# processed.jsonl 与 result 文件则会让「本轮结果行」的判定含糊。
foreach ($p in $stateDir, (Join-Path $out 'inbound')) {
	if (Test-Path $p) { Remove-Item -Recurse -Force $p }
}
New-Item -ItemType Directory -Force -Path $stateDir, (Join-Path $out 'inbound') | Out-Null
$resultFile = Join-Path $out 'agent-result.jsonl'
if (Test-Path $resultFile) { Remove-Item -Force $resultFile }

function Fail([string]$message) {
	Write-Host "✗ $message" -ForegroundColor Red
	exit 1
}

# 1) 端口：被占时报出占者，不静默重试（重试会连到别人的进程上，给出更离谱的结论）。
# 用普通哈希表而不是 [ordered]@{}：Windows PowerShell 5.1 的 OrderedDictionary
# 不允许用索引器新增键（会抛 ArgumentOutOfRangeException: index）。
$ports = @{ 8080 = 'host'; 8081 = 'inventory'; 8082 = 'procurement'; 8083 = 'quote' }
if ($Stub) { $ports[8099] = 'llmstub' }
foreach ($port in $ports.Keys) {
	# Get-NetTCPConnection 在有的 Windows 上会被策略禁用，退回 .NET 的监听探测。
	$busy = $false
	try {
		$busy = [bool](Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)
	} catch {
		$client = New-Object System.Net.Sockets.TcpClient
		try { $client.Connect('127.0.0.1', $port); $busy = $true } catch { $busy = $false } finally { $client.Dispose() }
	}
	if ($busy) { Fail "端口 $port（$($ports[$port])）被占用：先停掉占者再跑" }
}

# 2) 凭据：key 只从文件读（docker/secrets/ 已 gitignore），不进命令行、不进日志。
$keyPath = $null
if (-not $Stub) {
	if (-not (Test-Path $KeyFile)) { Fail "缺少 key 文件 $KeyFile" }
	$keyPath = (Resolve-Path $KeyFile).Path
}

# 2b) 模块的 HMAC 密钥文件：/readyz 会检查 hmac_secret 与 audit_file 两项
# （internal/module module.go 的 serveReady），缺一项模块永远不就绪——这不是可选配置。
# 开发用随机密文放 out/ 下（out/ 已 gitignore）。
$hmacFile = Join-Path $out 'hmac-secret'
if (-not (Test-Path $hmacFile)) {
	$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
	$buf = New-Object byte[] 32
	$rng.GetBytes($buf)
	[System.IO.File]::WriteAllText($hmacFile, [System.BitConverter]::ToString($buf).Replace('-', '').ToLowerInvariant())
}

# 3) 构建：先构建再起进程，避免「go run 还在编译，readyz 超时」的假失败。
foreach ($svc in 'host', 'agent', 'inventory', 'procurement', 'quote', 'tenon') {
	& go build -o (Join-Path $bin "$svc.exe") "./cmd/$svc"
	if ($LASTEXITCODE -ne 0) { Fail "构建 $svc 失败" }
}
if ($Stub) {
	& go build -o (Join-Path $bin 'llmstub.exe') ./tools/llmstub
	if ($LASTEXITCODE -ne 0) { Fail '构建 llmstub 失败' }
}

# Start-Svc：只给这个子进程设环境变量，起完立刻还原，避免串味。
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
			$resp = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2
			if ($resp.StatusCode -eq 200) { return }
		} catch { }
		Start-Sleep -Milliseconds 250
	}
	$tail = ''
	$errFile = Join-Path $logDir "$name.err"
	if (Test-Path $errFile) { $tail = (Get-Content $errFile -Tail 20 | Out-String) }
	Fail "$name 未就绪（$url）。stderr 尾部：`n$tail"
}

$procs = @()
$agentJob = $null
$exitCode = 2
try {
	if ($Stub) {
		$procs += Start-Svc 'llmstub' @{ PORT = '8099' }
		Wait-Ready 'llmstub' 'http://127.0.0.1:8099/healthz'
	}

	# 三个模块：全部测试态，两轮 A/B 才能「同输入」；HMAC 文件不给就永远不就绪。
	$procs += Start-Svc 'inventory' @{
		PORT = '8081'; TENON_MODULE_ID = 'inventory'; TENON_TEST_MODE = '1'
		TENON_HMAC_SECRET_FILE = $hmacFile
		TENON_MODULE_AUDIT_PATH = "$out/audit-inventory.jsonl"
		# 库存种子改 0：需求 12、在库 0 ⇒ 扣减在业务上不成立，唯一合理的写路径就是
		# 被摘的那个模块（procurement）。否则模型可以只做「先扣 3、缺 9 再采购」，
		# A 轮不引用 procurement 时判据会被架空（2026-10-27 的「空洞通过」）。
		TENON_INVENTORY_SEED = 'STD-1002:0,STD-1001:120'
	}
	$procs += Start-Svc 'procurement' @{
		PORT = '8082'; TENON_MODULE_ID = 'procurement'; TENON_TEST_MODE = '1'
		TENON_HMAC_SECRET_FILE = $hmacFile
		TENON_MODULE_AUDIT_PATH = "$out/audit-procurement.jsonl"
	}
	$procs += Start-Svc 'quote' @{
		PORT = '8083'; TENON_MODULE_ID = 'quote'; TENON_TEST_MODE = '1'
		TENON_HMAC_SECRET_FILE = $hmacFile
		TENON_MODULE_AUDIT_PATH = "$out/audit-quote.jsonl"
	}
	Wait-Ready 'inventory' 'http://127.0.0.1:8081/readyz'
	Wait-Ready 'procurement' 'http://127.0.0.1:8082/readyz'
	Wait-Ready 'quote' 'http://127.0.0.1:8083/readyz'

	# 模块先起、host 后起：host 加载期要拉每个模块的 actions 来校验业务规则，
	# 模块不在时规则会静默不加载。STRICT_RULES=1 把这条变硬：规则没加载就起不来。
	$procs += Start-Svc 'host' @{
		PORT = '8080'; TENON_TEST_MODE = '1'; TENON_STRICT_RULES = '1'
		TENON_MODULES = 'quote=http://127.0.0.1:8083,inventory=http://127.0.0.1:8081,procurement=http://127.0.0.1:8082'
		TENON_USERS_FILE = 'config/users.dev.json'; TENON_SESSION_SECRET = 'dev'
		TENON_SKILLS_DIR = 'skills'; TENON_POLICY_FILE = 'config/policies.dev.json'
		TENON_GRANT_FILE = 'config/grants.dev.json'; TENON_INBOUND_DIR = "$out/inbound"
		TENON_AUDIT_PATH = "$out/audit-host.jsonl"
	}
	Wait-Ready 'host' 'http://127.0.0.1:8080/readyz'

	$agentEnv = @{
		TENON_HOST_URL = 'http://127.0.0.1:8080'; TENON_AGENT_ID = 'ag_fulfillment'
		TENON_ON_BEHALF_OF = 'user:alice'; TENON_ONCE = '1'
		TENON_AGENT_USER = 'agent'; TENON_AGENT_PASSWORD = 'agent-dev'
		TENON_AGENT_STATE_DIR = $stateDir; TENON_RESULT_FILE = $resultFile
		TENON_SKILLS_DIR = 'skills'
		# 模型超时必须短于轮次超时：否则模型还在等、runner 已经判「等不到结果行」，
		# 报告里只会看到超时，看不出是模型慢。
		TENON_LLM_TIMEOUT = "$($RoundTimeoutSec - 30)s"
	}
	if ($Stub) {
		$agentEnv.TENON_LLM_BASE_URL = 'http://127.0.0.1:8099/v1'
		$agentEnv.TENON_LLM_API_KEY = 'dummy'
		$agentEnv.TENON_LLM_MODEL = 'stub-plan'
	} else {
		$agentEnv.TENON_LLM_BASE_URL = $BaseUrl
		$agentEnv.TENON_LLM_API_KEY_FILE = $keyPath
		$agentEnv.TENON_LLM_MODEL = $Model
	}

	# agent 重启循环：每轮一个全新进程（空游标），这是 D1 的前提条件。
	$agentExe = Join-Path $bin 'agent.exe'
	$agentJob = Start-Job -Name d1agent -ScriptBlock {
		param($exe, $workDir, $logPath, $envMap)
		Set-Location $workDir
		foreach ($k in $envMap.Keys) { Set-Item -Path "env:$k" -Value ([string]$envMap[$k]) }
		while ($true) {
			& $exe *>> $logPath
			Start-Sleep -Milliseconds 300
		}
	} -ArgumentList $agentExe, $root, (Join-Path $logDir 'agent.err'), $agentEnv

	$reportPath = Join-Path $out 'report.json'
	& (Join-Path $bin 'tenon.exe') d1 run `
		--host 'http://127.0.0.1:8080' --quote 'http://127.0.0.1:8083' `
		--inventory 'http://127.0.0.1:8081' --procurement 'http://127.0.0.1:8082' `
		--agent-result $resultFile `
		--report $reportPath --round-timeout "${RoundTimeoutSec}s"
	$exitCode = $LASTEXITCODE

	Write-Host ''
	Write-Host "d1 退出码：$exitCode" -ForegroundColor Cyan
	if (Test-Path $reportPath) {
		Write-Host "报告：$reportPath" -ForegroundColor Cyan
		# 报告是 UTF-8：Windows PowerShell 5.1 的 Get-Content 默认按 ANSI 读，
		# 不显式给 -Encoding UTF8 会把中文理由打成乱码。
		Get-Content $reportPath -Raw -Encoding UTF8

		# 判定收紧要落在脚本上：inconclusive 说明 A 轮没引用被摘模块（命题未被检验），
		# 本轮验收不通过——不要让它以「也算跑过了」的姿态混过去。
		$verdict = (Get-Content $reportPath -Raw -Encoding UTF8 | ConvertFrom-Json).verdict
		if ($verdict -eq 'inconclusive') {
			Write-Host '✗ D1 判定为 inconclusive：A 轮没有引用被摘模块，命题未被检验' -ForegroundColor Red
			exit 1
		}
	} else {
		Write-Host '没有产出报告：说明前置条件没满足（host/模块/agent 有一环没起来）' -ForegroundColor Yellow
	}
}
finally {
	if ($agentJob) {
		Stop-Job $agentJob -ErrorAction SilentlyContinue
		Remove-Job $agentJob -Force -ErrorAction SilentlyContinue
	}
	foreach ($p in $procs) {
		if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
	}
}

exit $exitCode
