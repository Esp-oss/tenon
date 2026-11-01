#!/usr/bin/env powershell
# 真模型对话验收：假设栈已用 scripts/dev-up.ps1 -Real 起好（8080 常驻）。
#
# 断言链路：alice 投一句话 → 规划出计划 → 命中 approve 策略 → 挂起 → bob 批准 →
#           库存真的减少 → 审计 result 段带 approvedBy=user:bob。
#
# 为什么默认 Qty=12 而不是 5：qty<=10 在 dev 策略里**没有登记**，会按 fail-closed 走 deny
# （转人工但不挂起，没有可批准的卡）。要验「审批链路」必须命中 approve，即 qty>10。
# 这是 2026-10-27 的产品决定，见 README「对话发起的小量扣减会转人工」。
[CmdletBinding()]
param(
	[string]$BaseUrl = 'http://127.0.0.1:8080',
	[string]$Sku = 'STD-1001',
	[int]$Qty = 12,
	[int]$TimeoutSec = 180
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root
$work = Join-Path $root 'out/ai-verify'
$resultFile = Join-Path $root 'out/dev/agent-result.jsonl'
New-Item -ItemType Directory -Force -Path $work | Out-Null

function Fail([string]$m) { Write-Host "✗ $m" -ForegroundColor Red; exit 1 }
function W([string]$name) { Join-Path $work $name }

function PostJson([string]$path, [string]$bodyFile, [string]$token) {
	$argv = @('-s', '-X', 'POST', "$BaseUrl$path", '-H', 'Content-Type: application/json', '--data-binary', "@$bodyFile")
	if ($token) { $argv += @('-H', "Authorization: Bearer $token") }
	& curl.exe @argv
}

function GetStock([string]$sku) {
	# 不用 -f 拼 JSON：PowerShell 的 -f 会把字面花括号当格式占位符（踩过）。
	[System.IO.File]::WriteAllText((W 'stock.json'), '{"sku":"' + $sku + '"}', (New-Object System.Text.UTF8Encoding($false)))
	$raw = & curl.exe -s -X POST 'http://127.0.0.1:8081/tenon/actions/inventory.GetStock' `
		-H 'Content-Type: application/json' -H 'Tenon-Version: 0.1' --data-binary "@$(W 'stock.json')"
	($raw | ConvertFrom-Json).onHand
}

# 1) 两个身份的 token
[System.IO.File]::WriteAllText((W 'alice.json'), '{"user":"alice","password":"alice-dev"}', (New-Object System.Text.UTF8Encoding($false)))
[System.IO.File]::WriteAllText((W 'bob.json'), '{"user":"bob","password":"bob-dev"}', (New-Object System.Text.UTF8Encoding($false)))
$alice = (PostJson '/tenon/session' (W 'alice.json') '' | ConvertFrom-Json).token
$bob = (PostJson '/tenon/session' (W 'bob.json') '' | ConvertFrom-Json).token
if (-not $alice -or -not $bob) { Fail '登录失败（栈没起？先跑 scripts/dev-up.ps1 -Real）' }

# 2) 记录投递前的库存与结果行数
$onHandBefore = GetStock $Sku
$linesBefore = 0
if (Test-Path $resultFile) { $linesBefore = (Get-Content $resultFile -Encoding UTF8 | Measure-Object).Count }
Write-Host "投递前：$Sku onHand=$onHandBefore；结果文件已有 $linesBefore 行"

# 3) 投一句话
[System.IO.File]::WriteAllText((W 'req.json'), ('{{"type":"tenon.request","text":"扣 {0} 件 {1}"}}' -f $Qty, $Sku), (New-Object System.Text.UTF8Encoding($false)))
$enq = PostJson '/tenon/requests' (W 'req.json') $alice
Write-Host "投递：$enq"
if ($enq -notmatch '"ok":true') { Fail "入队失败：$enq" }

# 4) 等本回合的结果行（按行号增量取，避免读到上一轮的旧行）
$deadline = (Get-Date).AddSeconds($TimeoutSec)
$line = $null
while ((Get-Date) -lt $deadline) {
	if (Test-Path $resultFile) {
		$all = Get-Content $resultFile -Encoding UTF8
		if ($all.Count -gt $linesBefore) {
			$line = $all[$linesBefore] | ConvertFrom-Json
			if ($line.planId) { break }
		}
	}
	Start-Sleep -Seconds 2
}
if (-not $line -or -not $line.planId) { Fail "等不到新结果行（agent 日志：out/dev/logs/agent.err）" }
Write-Host "结果行：outcome=$($line.outcome) planId=$($line.planId) actions=$($line.actions -join ',')"
if ($line.detail) { Write-Host "  detail=$($line.detail)" }

$planId = $line.planId

# 5) 挂起则由 bob 批准（帧形状对齐页面：type/confirmId/planId/decision/patch/by/at）
if ($line.outcome -eq 'needs_human') {
	$at = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
	$cfm = '{{"type":"tenon.confirm","confirmId":"cfm_verify{0}","planId":"{1}","decision":"approve","patch":null,"by":"user:bob","at":"{2}"}}' -f (Get-Random -Maximum 99999), $planId, $at
	[System.IO.File]::WriteAllText((W 'confirm.json'), $cfm, (New-Object System.Text.UTF8Encoding($false)))
	$resp = PostJson '/tenon/requests' (W 'confirm.json') $bob
	Write-Host "批准：$resp"
	if ($resp -notmatch '"ok":true') { Fail "confirm 入队失败：$resp" }

	$deadline = (Get-Date).AddSeconds($TimeoutSec)
	$archived = $null
	while ((Get-Date) -lt $deadline) {
		$archived = Get-ChildItem 'out/dev/agent-state/pending/archived' -Filter "$planId.json" -ErrorAction SilentlyContinue
		if ($archived) { break }
		Start-Sleep -Seconds 2
	}
	if (-not $archived) { Fail "批准后 pending 没有归档（planId=$planId）" }
	$state = (Get-Content $archived.FullName -Raw -Encoding UTF8 | ConvertFrom-Json).state
	Write-Host "归档：state=$state"
	if ($state -ne 'done') { Fail "归档状态应是 done，得到 $state" }
} elseif ($line.outcome -ne 'completed') {
	Fail "回合没有正常推进：outcome=$($line.outcome)"
}

# 6) 库存真的变了
$onHandAfter = GetStock $Sku
Write-Host "投递后：$Sku onHand=$onHandAfter（期望 $($onHandBefore - $Qty)）"
if ($onHandAfter -ne ($onHandBefore - $Qty)) {
	Fail "库存没有按预期减少：before=$onHandBefore after=$onHandAfter qty=$Qty"
}

# 7) 审计 result 段必须有 approvedBy（审批链路的核心证据）
$auditRaw = & curl.exe -s "$BaseUrl/tenon/audit?planId=$planId" -H "Authorization: Bearer $bob"
if ($auditRaw -notmatch '"stage":"result"') { Fail "审计没有 result 段：$auditRaw" }
if ($auditRaw -notmatch '"approvedBy":"user:bob"') { Fail "审计 result 段缺 approvedBy=user:bob：$auditRaw" }

Write-Host ''
Write-Host '✓ 真模型对话链路通过：计划合法 → 命中审批 → 挂起 → 批准 → 库存真变 → 审计可核' -ForegroundColor Green
