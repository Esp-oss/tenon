# e2e-dsh.ps1 —— 把设计文档 §10 的 AC2–AC8 固化成可重复执行的验收。
#
# 为什么要有它：这些判据此前只活在对话与临时脚本（out/dev/e2e-mcp.ps1）里，换一台机器
# 就没人能复现「对话层到底还能不能批」。本脚本只依赖：宿主 8080、三个模块 8081–8083、
# tenon-mcp.exe 已构建。dsh 本身**不参与**——dsh 是外部组件（要联网安装、开发者预览），
# 它起不来不代表 MCP 层坏了；dsh 里的对话步骤见 docs 末尾的手工清单。
#
# 覆盖：
#   AC2  registry_list → 三模块 + 心跳 <10s + 每个动作的 kind（与 /tenon/modules 同值）
#   AC3  action_invoke 读 STD-1001 → 与业务页同源（inventory.ListStock）同值
#   AC4  plan_preview qty=12 → approve/verified；qty=5 → deny；库存不变、无新卡
#   AC5  人发请求帧 → 落卡 → pending_list / pending_decide 批准 → 库存 −12 + 两段审计
#   AC6  无 token 调 pending_decide → 拒绝，且卡仍是 awaiting_approval（有卡时复测）
#   AC7  lastSeen 是带偏移的 RFC3339（现场 + 单测双证）
#   AC8  action_invoke 传写动作 → 拒绝，且模块审计 headSeq 不变（一个请求都没到）
#
# 前五条里 AC4 与 AC6 是负向判据：缺了它们，「能对话」就只是能聊天。
#
# 用法：
#   powershell -NoProfile -File scripts/e2e-dsh.ps1
#   powershell -NoProfile -File scripts/e2e-dsh.ps1 -WaitSeconds 120 -SkipGoTest
#
# 注意：AC5 会真的扣 12 件 STD-1001（这正是它要证明的）。重复跑请重开 dev-up
# 重置内存库存（脚本末尾会打印剩余量，低于 12 时后续运行会判 STOCK_INSUFFICIENT）。
param(
	[string]$HostUrl = 'http://127.0.0.1:8080',
	[string]$McpBin = '',
	[string]$HumanUser = 'alice',
	[string]$HumanPassword = 'alice-dev',
	[int]$WaitSeconds = 90,
	[switch]$SkipGoTest
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if (-not $McpBin) { $McpBin = Join-Path $root 'out\dev\bin\tenon-mcp.exe' }
$work = Join-Path $root 'out\dsh-e2e'
New-Item -ItemType Directory -Force -Path $work | Out-Null

$results = New-Object System.Collections.Generic.List[object]
function Add-Result([string]$ac, [string]$name, [bool]$ok, [string]$detail) {
	$results.Add([pscustomobject]@{ AC = $ac; 判据 = $name; 结果 = $(if ($ok) { 'PASS' } else { 'FAIL' }); 说明 = $detail })
}
function Clip([string]$s, [int]$n = 160) {
	$s = ($s -replace "`r?`n", ' ').Trim()
	if ($s.Length -le $n) { return $s }
	return $s.Substring(0, $n) + '…'
}

if (-not (Test-Path $McpBin)) { throw "找不到 $McpBin：先跑 scripts/dev-up.ps1（它会构建 tenon-mcp）" }

# ---------- 宿主侧读写（与 MCP 并行，用于对照与前后置断言）----------

function Login-Human {
	$body = @{ user = $HumanUser; password = $HumanPassword } | ConvertTo-Json
	$s = Invoke-RestMethod -Method Post -Uri "$HostUrl/tenon/session" -ContentType 'application/json' -Body $body
	if (-not $s.token) { throw "登录 $HumanUser 没有返回 token：$($s | ConvertTo-Json -Compress)" }
	return $s.token
}

function Get-Pending([string]$token) {
	return Invoke-RestMethod -Uri "$HostUrl/tenon/pending" -Headers @{ Authorization = "Bearer $token" }
}

function Get-StockSnapshot {
	$inv = ((Invoke-RestMethod "$HostUrl/tenon/modules").modules | Where-Object { $_.id -eq 'inventory' }).baseURL
	if (-not $inv) { throw '注册中心里没有 inventory：dev-up 的模块还没注册完' }
	return Invoke-RestMethod -Method Post -Uri "$inv/tenon/actions/inventory.ListStock" `
		-ContentType 'application/json' -Headers @{ 'Tenon-Version' = '0.1' } -Body '{}'
}

function Get-StockOf([string]$sku, $snapshot) {
	return [int](($snapshot.items | Where-Object { $_.sku -eq $sku } | Select-Object -First 1).onHand)
}

function Get-InventoryAuditHead {
	$inv = ((Invoke-RestMethod "$HostUrl/tenon/modules").modules | Where-Object { $_.id -eq 'inventory' }).baseURL
	$r = Invoke-RestMethod "$inv/tenon/audit/local?limit=1" -Headers @{ 'Tenon-Version' = '0.1' }
	return [int64]$r.headSeq
}

# ---------- MCP stdio 会话 ----------

# Invoke-McpBatch 起一个 tenon-mcp 进程，一次性喂进一批 JSON-RPC，按 id 回读响应。
# 不用管道：服务端把日志写 stderr，PowerShell 在 Stop 模式会把原生命令的 stderr 当错误；
# 而且真实客户端也是「spawn 一个进程，读写它的管道」。
function Invoke-McpBatch {
	param(
		[string]$Name,
		[bool]$WithToken,
		[string]$Token,
		[object[]]$Messages
	)

	$lines = foreach ($m in $Messages) { $m | ConvertTo-Json -Depth 12 -Compress }
	$inFile = Join-Path $work ("mcp-{0}-in.jsonl" -f $Name)
	$outFile = Join-Path $work ("mcp-{0}-out.jsonl" -f $Name)
	$errFile = Join-Path $work ("mcp-{0}-err.log" -f $Name)
	[IO.File]::WriteAllText($inFile, ($lines -join "`n"), (New-Object Text.UTF8Encoding($false)))

	$env:TENON_HOST = $HostUrl
	# 三个身份变量都显式处理：只清 TOKEN 而不清 USER/PASSWORD 的话，工具会惰性登录，
	# 「无 token」用例就会静默变成「有 token」。
	Remove-Item Env:TENON_MCP_USER -ErrorAction SilentlyContinue
	Remove-Item Env:TENON_MCP_PASSWORD -ErrorAction SilentlyContinue
	if ($WithToken) {
		$env:TENON_MCP_HUMAN_TOKEN = $Token
	} else {
		Remove-Item Env:TENON_MCP_HUMAN_TOKEN -ErrorAction SilentlyContinue
	}

	$proc = Start-Process -FilePath $McpBin -RedirectStandardInput $inFile `
		-RedirectStandardOutput $outFile -RedirectStandardError $errFile -NoNewWindow -PassThru -Wait
	if ($proc.ExitCode -ne 0) { throw "tenon-mcp（$Name）退出码 $($proc.ExitCode)，看 $errFile" }

	$responses = @{}
	foreach ($line in @(Get-Content $outFile -Encoding UTF8 | Where-Object { $_.Trim() -ne '' })) {
		$obj = $line | ConvertFrom-Json
		if ($null -ne $obj.id) { $responses[[string]$obj.id] = $obj }
	}
	return $responses
}

function New-Call([int]$id, [string]$tool, $arguments) {
	return @{ jsonrpc = '2.0'; id = $id; method = 'tools/call'; params = @{ name = $tool; arguments = $arguments } }
}

function Tool-Text($resp) {
	if ($resp -and $resp.result -and $resp.result.content) { return [string]$resp.result.content[0].text }
	return ''
}
function Tool-IsError($resp) {
	return [bool]($resp.result.isError)
}

# ---------- 前置 ----------

Write-Host '== 前置 ==' -ForegroundColor Cyan
try { $null = Invoke-RestMethod "$HostUrl/tenon/modules" -TimeoutSec 3 } catch { throw "宿主 $HostUrl 不可达：先跑 scripts/dev-up.ps1" }
$token = Login-Human
$snapshot0 = Get-StockSnapshot
$stockBefore = Get-StockOf 'STD-1001' $snapshot0
$pendingResp0 = Get-Pending $token
$pending0 = @($pendingResp0.items | ForEach-Object { $_.planId })
$auditHead0 = Get-InventoryAuditHead
Write-Host ("  宿主在；{0} 的 token 在手；STD-1001={1}；待批 {2} 条；inventory 审计 head={3}" -f `
		$HumanUser, $stockBefore, @($pending0).Count, $auditHead0) -ForegroundColor DarkGray

# ---------- AC2 / AC3 / AC4 / AC8（一个 token 会话 + 一个无 token 会话）----------

# 无 token 会话：AC8（写动作被拒）+ AC6a（无票审批被拒）
$noTok = Invoke-McpBatch -Name 'no-token-1' -WithToken $false -Messages @(
	@{ jsonrpc = '2.0'; id = 0; method = 'initialize'; params = @{ protocolVersion = '2025-06-18' } },
	@{ jsonrpc = '2.0'; id = 1; method = 'tools/list' },
	(New-Call 2 'action_invoke' @{ actionId = 'inventory.DeductStock'; args = @{ sku = 'STD-1001'; qty = 12; referenceNo = 'E2E-DSH-AC8' } }),
	(New-Call 3 'pending_decide' @{ planId = 'pln_e2e_no_such_plan'; decision = 'approve' })
)

$toolNames = @($noTok['1'].result.tools | ForEach-Object { $_.name })
Add-Result 'T9' '工具面 7 个且含 request_submit' `
	($toolNames.Count -eq 7 -and $toolNames -contains 'request_submit') ($toolNames -join ', ')

$writeText = Tool-Text $noTok['2']
$writeIsErr = Tool-IsError $noTok['2']
Add-Result 'AC8' 'action_invoke 写动作被拒' ($writeIsErr -and ($writeText -match '写动作|不能直接调用')) (Clip $writeText)

$headAfterBlocked = Get-InventoryAuditHead
$snapshotBlocked = Get-StockSnapshot
Add-Result 'AC8b' '被拒的写动作没有触达模块（审计 head / 库存都不变）' `
	($headAfterBlocked -eq $auditHead0 -and (Get-StockOf 'STD-1001' $snapshotBlocked) -eq $stockBefore) `
	("head {0}→{1}；stock {2}→{3}" -f $auditHead0, $headAfterBlocked, $stockBefore, (Get-StockOf 'STD-1001' $snapshotBlocked))

$decText = Tool-Text $noTok['3']
$decErr = Tool-IsError $noTok['3']
Add-Result 'AC6a' '无 token 调 pending_decide → 拒绝（无卡也不放行）' ($decErr -and ($decText -match 'token|工人身份|身份')) (Clip $decText)

# 有 token 会话：AC2 / AC3 / AC4（预检不落卡）
$withTok = Invoke-McpBatch -Name 'with-token-previews' -WithToken $true -Token $token -Messages @(
	@{ jsonrpc = '2.0'; id = 0; method = 'initialize'; params = @{ protocolVersion = '2025-06-18' } },
	(New-Call 1 'registry_list' @{}),
	(New-Call 2 'action_invoke' @{ actionId = 'inventory.GetStock'; args = @{ sku = 'STD-1001' } }),
	(New-Call 3 'plan_preview' @{ steps = @(@{ action = 'inventory.DeductStock'; args = @{ sku = 'STD-1001'; qty = 12; referenceNo = 'E2E-DSH-AC4' } }) }),
	(New-Call 4 'plan_preview' @{ steps = @(@{ action = 'inventory.DeductStock'; args = @{ sku = 'STD-1001'; qty = 5; referenceNo = 'E2E-DSH-AC4' } }) })
)

# AC2：现场 /tenon/modules 同值 + 心跳 <10s + 每个动作的 kind
$modsRaw = (Invoke-WebRequest "$HostUrl/tenon/modules").Content
$modules = ($modsRaw | ConvertFrom-Json).modules
$badAge = @()
foreach ($m in $modules) {
	$seen = [DateTimeOffset]::Parse($m.lastSeen)
	$ageSec = ([DateTimeOffset]::UtcNow - $seen.ToUniversalTime()).TotalSeconds
	if ($ageSec -ge 10) { $badAge += ("{0}={1:N1}s" -f $m.id, $ageSec) }
}
$reg = Tool-Text $withTok['1']
$missing = @()
foreach ($m in $modules) {
	if ($reg -notmatch [regex]::Escape($m.id)) { $missing += $m.id }
	foreach ($a in $m.manifest.actions) {
		if ($reg -notmatch [regex]::Escape("$($a.id) [$($a.kind)]")) { $missing += "$($a.id)[$($a.kind)]" }
	}
}
Add-Result 'AC2' '三个模块 + 心跳 <10s + 每个动作的 kind' `
	($modules.Count -eq 3 -and $badAge.Count -eq 0 -and $missing.Count -eq 0) `
	("modules={0}; 超时={1}; 缺={2}" -f $modules.Count, $(if ($badAge) { $badAge -join ',' } else { '无' }), $(if ($missing) { $missing -join ',' } else { '无' }))

# AC3：工具返回的 onHand 与业务页同源快照（ListStock）同值
$readText = Tool-Text $withTok['2']
$mOnHand = [regex]::Match($readText, 'onHand"\s*:\s*(\d+)')
$pageStock = Get-StockOf 'STD-1001' (Get-StockSnapshot)
Add-Result 'AC3' '读动作返回真值且与业务页同值' `
	($mOnHand.Success -and [int]$mOnHand.Groups[1].Value -eq $pageStock) `
	("tool={0}; page(ListStock)={1}" -f $(if ($mOnHand.Success) { $mOnHand.Groups[1].Value } else { '无 onHand' }), $pageStock)

# AC4：approve + verified；qty=5 判 deny；库存与卡都不变
$prev12 = Tool-Text $withTok['3']
$prev5 = Tool-Text $withTok['4']
$okPrev12 = ($prev12 -match '策略：(auto|approve)') -and ($prev12 -match '预演：.*verified') -and ($prev12 -match '结论：预检通过')
$okPrev5 = ($prev5 -match '策略：deny') -and ($prev5 -match 'NO_POLICY_MATCH_DEFAULT_DENY')
$snapshotAfterPrev = Get-StockSnapshot
$pendingAfterPrev = @((Get-Pending $token).items | ForEach-Object { $_.planId })
$samePending = (($pending0 | Sort-Object) -join '|') -eq (($pendingAfterPrev | Sort-Object) -join '|')
Add-Result 'AC4a' 'qty=12 预检 approve/verified' $okPrev12 (Clip $prev12 240)
Add-Result 'AC4b' 'qty=5 预检 deny 并透出理由' $okPrev5 (Clip $prev5 240)
Add-Result 'AC4c' '预检不执行：库存不变、没有新卡' `
	((Get-StockOf 'STD-1001' $snapshotAfterPrev) -eq $stockBefore -and $samePending) `
	("stock {0}→{1}；卡 {2}→{3}" -f $stockBefore, (Get-StockOf 'STD-1001' $snapshotAfterPrev), @($pending0).Count, @($pendingAfterPrev).Count)

# ---------- AC5：人发请求帧 → 落卡 → 对话里批 → 库存 −12 + 两段审计 ----------

$submit = Invoke-McpBatch -Name 'request-submit' -WithToken $true -Token $token -Messages @(
	@{ jsonrpc = '2.0'; id = 0; method = 'initialize'; params = @{ protocolVersion = '2025-06-18' } },
	(New-Call 1 'request_submit' @{ text = '把 STD-1001 的 12 件库存扣掉' })
)
$submitText = Tool-Text $submit['1']
Add-Result 'AC5a' 'request_submit 把人话投进入站队列' `
	((-not (Tool-IsError $submit['1'])) -and ($submitText -match '已交给 agent')) (Clip $submitText)

$newPlan = ''
$deadline = (Get-Date).AddSeconds($WaitSeconds)
while ((Get-Date) -lt $deadline) {
	$items = @(Get-Pending $token).items
	$cand = $items | Where-Object { $_.planId -notin $pending0 -and $_.state -eq 'awaiting_approval' } | Select-Object -First 1
	if ($cand) { $newPlan = $cand.planId; break }
	Start-Sleep -Milliseconds 600
}
if (-not $newPlan) {
	Add-Result 'AC5b' 'agent 落卡（新 planId）' $false ("${WaitSeconds}s 内没有新卡；看 out/dev/logs/agent.err")
} else {
	Add-Result 'AC5b' 'agent 落卡（新 planId）' $true $newPlan
}

# AC6（强）：卡存在时，无 token 批不动它，且状态仍是 awaiting_approval
if ($newPlan) {
	$noTok2 = Invoke-McpBatch -Name 'no-token-card' -WithToken $false -Messages @(
		@{ jsonrpc = '2.0'; id = 0; method = 'initialize'; params = @{ protocolVersion = '2025-06-18' } },
		(New-Call 1 'pending_decide' @{ planId = $newPlan; decision = 'approve' })
	)
	$stateNow = (@(Get-Pending $token).items | Where-Object { $_.planId -eq $newPlan }).state
	$text2 = Tool-Text $noTok2['1']
	Add-Result 'AC6b' '有卡时无 token 批准 → 拒绝，卡仍待批' `
		((Tool-IsError $noTok2['1']) -and ($stateNow -eq 'awaiting_approval')) `
		("state=$stateNow；tool=" + (Clip $text2))
}

if ($newPlan) {
	$decide = Invoke-McpBatch -Name 'approve' -WithToken $true -Token $token -Messages @(
		@{ jsonrpc = '2.0'; id = 0; method = 'initialize'; params = @{ protocolVersion = '2025-06-18' } },
		(New-Call 1 'pending_list' @{}),
		(New-Call 2 'pending_decide' @{ planId = $newPlan; decision = 'approve' })
	)
	$listText = Tool-Text $decide['1']
	Add-Result 'AC5c' 'pending_list 给出卡与 ask_user_question 指令' `
		(($listText -match [regex]::Escape($newPlan)) -and ($listText -match 'ask_user_question')) (Clip $listText 240)
	Add-Result 'AC5d' 'pending_decide 批准' `
		((-not (Tool-IsError $decide['2'])) -and ((Tool-Text $decide['2']) -match '批准')) (Clip (Tool-Text $decide['2']))

	# 等恢复执行：库存 −12 且卡到终态
	$expected = $stockBefore - 12
	$stockNow = $stockBefore
	$state = ''
	$deadline = (Get-Date).AddSeconds($WaitSeconds)
	while ((Get-Date) -lt $deadline) {
		$stockNow = Get-StockOf 'STD-1001' (Get-StockSnapshot)
		$state = (@(Get-Pending $token).items | Where-Object { $_.planId -eq $newPlan } | Select-Object -First 1).state
		if ($stockNow -eq $expected -and ($state -eq 'done' -or -not $state)) { break }
		Start-Sleep -Milliseconds 700
	}
	Add-Result 'AC5e' '批准后恢复执行：库存 −12，卡 done' `
		($stockNow -eq $expected -and ($state -eq 'done' -or -not $state)) `
		("stock {0}→{1}（期望 {2}）；state={3}" -f $stockBefore, $stockNow, $expected, $state)

	# 两段审计 + approvedBy
	$auditSession = Invoke-McpBatch -Name 'audit' -WithToken $true -Token $token -Messages @(
		@{ jsonrpc = '2.0'; id = 0; method = 'initialize'; params = @{ protocolVersion = '2025-06-18' } },
		(New-Call 1 'audit_query' @{ planId = $newPlan })
	)
	$auditRaw = Tool-Text $auditSession['1']
	$records = @()
	try { $records = @(($auditRaw | ConvertFrom-Json).records) } catch { }
	$disp = @($records | Where-Object { $_.stage -eq 'dispatched' })
	$res = @($records | Where-Object { $_.stage -eq 'result' })
	$approved = @($res | Where-Object { $_.decision.approvedBy -eq "user:$HumanUser" })
	Add-Result 'AC5f' '审计两段齐备且 approvedBy 是人' `
		($disp.Count -ge 1 -and $res.Count -ge 1 -and $approved.Count -ge 1) `
		("planId={0} dispatched={1} result={2} approvedBy={3}" -f $newPlan, $disp.Count, $res.Count,
			$(if ($approved.Count -gt 0) { $approved[0].decision.approvedBy } else { '无' }))
}

# ---------- AC7：lastSeen 序列化 ----------

$lastSeen = [regex]::Match($modsRaw, '"lastSeen"\s*:\s*"([^"]+)"')
$pattern = '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$'
$okFormat = $lastSeen.Success -and ($lastSeen.Groups[1].Value -match $pattern)
Add-Result 'AC7' 'lastSeen 是带偏移的 RFC3339' $okFormat $(if ($lastSeen.Success) { $lastSeen.Groups[1].Value } else { '没取到 lastSeen' })

if (-not $SkipGoTest) {
	Push-Location $root
	try {
		$goOut = & go test ./internal/controlplane -run TestModulesLastSeenIsRFC3339WithOffset -count=1 2>&1
		$okGo = ($LASTEXITCODE -eq 0)
	} finally { Pop-Location }
	Add-Result 'AC7b' 'lastSeen 序列化单测' $okGo ($goOut | Select-Object -Last 1)
}

# ---------- 汇总 ----------

$results | Format-Table -AutoSize -Wrap

$report = [pscustomobject]@{
	generatedAt = (Get-Date).ToString('o')
	host        = $HostUrl
	humanUser   = $HumanUser
	stockBefore = $stockBefore
	results     = $results
}
$reportPath = Join-Path $work 'report.json'
$report | ConvertTo-Json -Depth 6 | Set-Content $reportPath -Encoding utf8
Write-Host "`n报告：$reportPath" -ForegroundColor DarkGray

$fail = @($results | Where-Object { $_.结果 -eq 'FAIL' })
if ($fail.Count -gt 0) {
	Write-Host "`n❌ $($fail.Count) 条未通过" -ForegroundColor Red
	exit 1
}
$stockEnd = Get-StockOf 'STD-1001' (Get-StockSnapshot)
Write-Host "`n✅ AC2–AC8 全部通过（$($results.Count) 条）；STD-1001 剩余 $stockEnd（低于 12 时请重开 dev-up）" -ForegroundColor Green
exit 0
