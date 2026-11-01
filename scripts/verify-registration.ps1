#!/usr/bin/env powershell
# R1 注册验收：逐条断言 spec §10 的七条口径
# （docs/superpowers/specs/2026-10-27-tenon-module-registration-design.md）。
#
# 为什么单独一支脚本：D1（d1-e2e.ps1）只回答「摘掉模块后 agent 会变」，它不回答「注册本身
# 是否成立」——模块没注册上时 D1 会在 A 轮就失败，而理由看起来像业务问题。本脚本把注册
# 生命周期拆成可机械验证的断言：可见 → 错哈希 409 → 失联 → 回场 → 人工开关 → 宿主重启
# 窗口 → 事件链完整性 → 缺密钥拒绝启动。
#
# 七条口径 → 断言编号：
#   §10.1 注册后立即出现 ................................. A1（并用一条真实签名请求当场复验）
#   §10.2 停心跳后消失 + registry-events 出现 lost ....... A3
#   §10.3 错哈希 → 409；重申报后 alive + re-registered .... A2
#   §10.4 宿主重启 → 一个心跳周期内 409 重申报回场 ....... A6（并断言模块日志里出现
#        unknown-module——那是 409 路径的直接证据，不是「看起来恢复了」）
#   §10.5 人工 disable 粘性 / enable 立即恢复 ............. A5（disable 后再等 >1 个心跳
#        周期仍不可见，才能证明心跳救不回来）
#   §10.6 篡改事件链任一行 → 校验失败 .................... A7（先验真链，再在副本上篡改）
#   §10.7 缺 HMAC 密钥 → 宿主启动失败（点名环境变量） ..... A8
#   补充：A4 覆盖 §10.2 的恢复侧（被杀模块重启后回场）。
#
# 关于「≤15s」的机械口径（A3）：15s 是 registry.DefaultTTL（3 个心跳），但宿主每 5s 才清扫
# 一次，所以「从最后一次心跳到不可见」的**可观测上界**是 TTL + 清扫粒度 = 20s。断言 15s
# 会把设计的正确实现判成失败（取决于杀进程落在清扫相位的哪一段），断言 20s 才是这条设计
# 真正的约束。脚本里两处都打印：距最后一次心跳（判定用）与距杀进程（参考）。
#
# 本文件必须带 UTF-8 BOM 保存。Windows PowerShell 5.1 读无 BOM 的 UTF-8 会按系统代码页
# 解码，中文串会把引号吃掉并报「string is missing the terminator」。
#
# 用法（本机没有 pwsh 时用 powershell）：
#   powershell -NoProfile -File scripts/verify-registration.ps1
#   powershell -NoProfile -File scripts/verify-registration.ps1 -HostPort 8080
#
# 前置：端口 8080-8083 空闲（被 dev-up 占着时先跑 scripts/dev-down.ps1）；不需要 LLM 凭据。
[CmdletBinding()]
param(
	[int]$HostPort = 8080,
	# 注册后可见的等待上限（秒）：模块注册失败的进程会每 5s 重试一次，30s 足够两轮。
	[int]$VisibleTimeoutSec = 30,
	# 等「失联」从可见列表消失的上限（秒）：TTL 15s + 清扫粒度 5s + 余量，见上方注释。
	[int]$LostTimeoutSec = 40,
	# 被杀模块重启后回场的上限（秒）。
	[int]$RejoinTimeoutSec = 30,
	# 宿主重启后回场的上限（秒）：一个心跳周期 5s + 本地 RTT/调度余量 1s。
	[int]$HostRestartLimitSec = 6
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root

# registry 包里的两个常量。写在这里是为了把「15s TTL / 5s 心跳」变成断言里的数字时
# 有唯一来源——两个地方各写一个字面量，将来只改一处就会让断言与实际语义悄悄错开。
$hbSec = 5
$ttlSec = 15
# 可观测的失联上界：TTL 到点之后还要等下一拍清扫才会真的变 lost。
$lostBoundSec = $ttlSec + $hbSec

$work = Join-Path $root 'out/verify-registration'
$bin = Join-Path $work 'bin'
$logDir = Join-Path $work 'logs'
$hmacFile = Join-Path $work 'hmac-secret'
$registryEvents = Join-Path $work 'registry-events.jsonl'
$hostURL = "http://127.0.0.1:$HostPort"
$modulePorts = @{ inventory = 8081; procurement = 8082; quote = 8083 }
$expectedModules = @('inventory', 'procurement', 'quote')

# 探针模块：没有进程、只用心跳/注册端点驱动。它的价值是能**故意**送错哈希与换声明，
# 而真实模块不会配合演出（除非改它的代码）。
$probeID = 'probe-registry'
$probeURL = 'http://127.0.0.1:8097'
$probeManifestV1 = '{"tenon":"0.1","module":{"id":"probe-registry","version":"1.0.0","title":"注册验收探针"},"actions":[],"events":[],"transports":["http"],"capabilities":[]}'
$probeManifestV2 = '{"tenon":"0.1","module":{"id":"probe-registry","version":"1.0.1","title":"注册验收探针"},"actions":[],"events":[],"transports":["http"],"capabilities":[]}'

$script:results = @()
$script:procs = @()
$script:moduleProcs = @{}
$script:hostProc = $null
$script:secretBytes = $null

function Fail([string]$message) {
	Write-Host "✗ $message" -ForegroundColor Red
	exit 1
}

function Format-LogTails([string[]]$names, [int]$lines = 8) {
	$out = ''
	foreach ($name in $names) {
		foreach ($ext in '.err', '.out') {
			$f = Join-Path $logDir "$name$ext"
			if (Test-Path $f) {
				$tail = (Get-Content $f -Tail $lines -Encoding UTF8 | Out-String).Trim()
				if ($tail) { $out += "[$name$ext]`n$tail`n" }
			}
		}
	}
	return $out
}

# Check 记录一条断言结果。断言是**累积**的：一条失败不该让后面的证据全部丢失——
# 报告里最有用的是「七条里哪几条不成立」，而不是「第一条不成立」。
function Check([string]$id, [string]$title, [bool]$ok, [string]$detail) {
	$script:results += [pscustomobject]@{ Id = $id; Title = $title; Ok = $ok; Detail = $detail }
	if ($ok) {
		Write-Host "✓ [$id] $title — $detail" -ForegroundColor Green
	} else {
		Write-Host "✗ [$id] $title — $detail" -ForegroundColor Red
	}
}

# ---------- 进程与环境 ----------

function Start-Svc {
	param([string]$name, [hashtable]$envMap, [string]$logName = '')
	if (-not $logName) { $logName = $name }
	$exe = Join-Path $bin "$name.exe"
	# 只给这个子进程设环境变量，起完立刻还原：串味的典型症状是「下一个进程莫名其妙也注册了」。
	$saved = @{}
	foreach ($k in $envMap.Keys) {
		$saved[$k] = [Environment]::GetEnvironmentVariable($k)
		[Environment]::SetEnvironmentVariable($k, [string]$envMap[$k])
	}
	try {
		$p = Start-Process -FilePath $exe -WorkingDirectory $root -PassThru -NoNewWindow `
			-RedirectStandardOutput (Join-Path $logDir "$logName.out") `
			-RedirectStandardError (Join-Path $logDir "$logName.err")
	} finally {
		foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
	}
	$script:procs += $p
	return $p
}

function Stop-Svc($proc) {
	if ($null -eq $proc) { return }
	if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
	# 等端口真的释放：紧接着起同名进程会撞端口，而 Start-Process 不会报错，症状是
	# 「重启后的模块根本没起来」。
	try { $proc.WaitForExit(5000) | Out-Null } catch { }
}

function ModuleEnv([string]$id) {
	return @{
		PORT                    = [string]$modulePorts[$id]
		TENON_MODULE_ID         = $id
		TENON_HOST              = $hostURL
		TENON_HMAC_SECRET_FILE  = $hmacFile
		TENON_MODULE_AUDIT_PATH = (Join-Path $work "audit-$id.jsonl")
	}
}

function HostEnv {
	return @{
		PORT                    = [string]$HostPort
		TENON_TEST_MODE         = '1'
		TENON_STRICT_RULES      = '1'
		TENON_HMAC_SECRET_FILE  = $hmacFile
		TENON_REGISTRY_EVENTS   = $registryEvents
		TENON_USERS_FILE        = 'config/users.dev.json'
		TENON_SESSION_SECRET    = 'dev'
		TENON_SKILLS_DIR        = 'skills'
		TENON_POLICY_FILE       = 'config/policies.dev.json'
		TENON_GRANT_FILE        = 'config/grants.dev.json'
		TENON_INBOUND_DIR       = (Join-Path $work 'inbound')
		TENON_AUDIT_PATH        = (Join-Path $work 'audit-host.jsonl')
	}
}

# Start-HostAndWait 跑一次**会立刻退出**的宿主，返回退出码（A8 用）。
function Start-HostAndWait([hashtable]$envMap, [string]$logName) {
	$exe = Join-Path $bin 'host.exe'
	$saved = @{}
	foreach ($k in $envMap.Keys) {
		$saved[$k] = [Environment]::GetEnvironmentVariable($k)
		[Environment]::SetEnvironmentVariable($k, [string]$envMap[$k])
	}
	try {
		$p = Start-Process -FilePath $exe -WorkingDirectory $root -PassThru -Wait -NoNewWindow `
			-RedirectStandardOutput (Join-Path $logDir "$logName.out") `
			-RedirectStandardError (Join-Path $logDir "$logName.err")
	} finally {
		foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
	}
	return $p.ExitCode
}

function Wait-Ready([string]$name, [string]$url, [int]$timeoutSec = 40) {
	$deadline = (Get-Date).AddSeconds($timeoutSec)
	while ((Get-Date) -lt $deadline) {
		try {
			if ((Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2).StatusCode -eq 200) { return }
		} catch { }
		Start-Sleep -Milliseconds 200
	}
	Fail ("$name 未就绪（$url）。`n进程日志尾部：`n$(Format-LogTails @($name))")
}

# ---------- 注册表读取 ----------
#
# 两个视图必须分开读，这是 R1 的语义核心：
#   GET /tenon/modules        可见视图：enabled && alive。agent 看到的就是它。
#   GET /tenon/admin/modules  全量视图（仅 TENON_TEST_MODE=1）：含 disabled/lost。
# 只用可见视图无法区分「从未注册」与「注册了但被摘掉/已失联」，而本脚本有一半断言正是
# 在区分这两件事。

function Get-VisibleIds {
	param([string]$baseURL)
	$res = @{ Ok = $false; Ids = @(); Error = '' }
	try {
		$resp = Invoke-WebRequest -Uri "$baseURL/tenon/modules" -UseBasicParsing -TimeoutSec 3
		if ($resp.StatusCode -eq 200) {
			$res.Ok = $true
			$res.Ids = @(($resp.Content | ConvertFrom-Json).modules | ForEach-Object { $_.id })
		}
	} catch {
		$res.Error = $_.Exception.Message
	}
	return $res
}

function Get-RegistryEntries {
	param([string]$baseURL)
	$res = @{ Ok = $false; Entries = @(); Error = '' }
	try {
		$resp = Invoke-WebRequest -Uri "$baseURL/tenon/admin/modules" -UseBasicParsing -TimeoutSec 3
		if ($resp.StatusCode -eq 200) {
			$res.Ok = $true
			$res.Entries = @(($resp.Content | ConvertFrom-Json).modules)
		}
	} catch {
		$res.Error = $_.Exception.Message
	}
	return $res
}

function Get-Entry([string]$baseURL, [string]$id) {
	$r = Get-RegistryEntries $baseURL
	if (-not $r.Ok) { return $null }
	return ($r.Entries | Where-Object { $_.id -eq $id } | Select-Object -First 1)
}

function Wait-VisibleIds {
	param([string]$baseURL, [string[]]$expected, [int]$timeoutSec)
	$sw = [System.Diagnostics.Stopwatch]::StartNew()
	$deadline = (Get-Date).AddSeconds($timeoutSec)
	$seen = @()
	while ((Get-Date) -lt $deadline) {
		$r = Get-VisibleIds $baseURL
		if ($r.Ok) {
			$seen = @($r.Ids)
			$missing = @($expected | Where-Object { $seen -notcontains $_ })
			if ($missing.Count -eq 0) { $sw.Stop(); return @{ Ok = $true; Elapsed = $sw.Elapsed.TotalSeconds; Seen = $seen } }
		}
		Start-Sleep -Milliseconds 200
	}
	$sw.Stop()
	return @{ Ok = $false; Elapsed = $sw.Elapsed.TotalSeconds; Seen = $seen }
}

function Wait-GoneId {
	param([string]$baseURL, [string]$id, [int]$timeoutSec)
	$sw = [System.Diagnostics.Stopwatch]::StartNew()
	$deadline = (Get-Date).AddSeconds($timeoutSec)
	while ((Get-Date) -lt $deadline) {
		$r = Get-VisibleIds $baseURL
		if ($r.Ok -and ($r.Ids -notcontains $id)) { $sw.Stop(); return @{ Ok = $true; Elapsed = $sw.Elapsed.TotalSeconds } }
		Start-Sleep -Milliseconds 200
	}
	$sw.Stop()
	return @{ Ok = $false; Elapsed = $sw.Elapsed.TotalSeconds }
}

function Format-Registry([string]$baseURL) {
	$r = Get-RegistryEntries $baseURL
	if (-not $r.Ok) { return "读不到管理面（宿主未以 TENON_TEST_MODE=1 启动？）：$($r.Error)" }
	if ($r.Entries.Count -eq 0) { return '空表' }
	return (@($r.Entries | ForEach-Object { "$($_.id)(enabled=$($_.enabled),state=$($_.state))" }) -join ' ')
}

# ---------- 规范化与哈希（复算声明哈希与事件链哈希） ----------
#
# 这两处都必须与 Go 侧 protocol.CanonicalJSON（RFC 8785 JCS）算出**同一串字节**：
#   - 声明哈希：sha256: + hex(sha256(JCS(manifest)))；
#   - 事件链：hash = sha256: + hex(sha256(JCS(记录去掉 hash)))。
# 脚本自己复算而不是调 Go 工具，是因为验收脚本要能脱离被测代码独立核对——用被测代码算
# 出来的哈希去验被测代码，同错也同过。
#
# 本文件的字段名全是 ASCII，所以键序用 CompareOrdinal（UTF-16 码元序）就与 JCS 等价；
# 取值只可能是字符串与 null（时间戳/事件名/id/哈希/原因串）。

function Get-Sha256Hex([byte[]]$bytes) {
	$sha = [System.Security.Cryptography.SHA256]::Create()
	try {
		$digest = $sha.ComputeHash($bytes)
	} finally {
		$sha.Dispose()
	}
	return ([System.BitConverter]::ToString($digest)).Replace('-', '').ToLowerInvariant()
}

function ConvertTo-CanonicalString([string]$s) {
	$sb = New-Object System.Text.StringBuilder
	[void]$sb.Append('"')
	foreach ($ch in $s.ToCharArray()) {
		$code = [int]$ch
		if ($ch -eq [char]'"') { [void]$sb.Append('\"') }
		elseif ($ch -eq [char]'\') { [void]$sb.Append('\\') }
		elseif ($code -eq 8) { [void]$sb.Append('\b') }
		elseif ($code -eq 9) { [void]$sb.Append('\t') }
		elseif ($code -eq 10) { [void]$sb.Append('\n') }
		elseif ($code -eq 12) { [void]$sb.Append('\f') }
		elseif ($code -eq 13) { [void]$sb.Append('\r') }
		elseif ($code -lt 0x20) { [void]$sb.Append('\u00' + $code.ToString('x2')) }
		else { [void]$sb.Append($ch) }
	}
	[void]$sb.Append('"')
	return $sb.ToString()
}

function Sort-Ordinal([string[]]$keys) {
	$arr = @($keys)
	for ($i = 1; $i -lt $arr.Count; $i++) {
		$cur = $arr[$i]
		$j = $i - 1
		while ($j -ge 0 -and [string]::CompareOrdinal($arr[$j], $cur) -gt 0) {
			$arr[$j + 1] = $arr[$j]
			$j--
		}
		$arr[$j + 1] = $cur
	}
	return $arr
}

function ConvertTo-CanonicalJson {
	param($Value)
	if ($null -eq $Value) { return 'null' }
	if ($Value -is [string]) { return (ConvertTo-CanonicalString $Value) }
	if ($Value -is [bool]) { if ($Value) { return 'true' } else { return 'false' } }
	if ($Value -is [int] -or $Value -is [long]) { return $Value.ToString([System.Globalization.CultureInfo]::InvariantCulture) }
	if ($Value -is [double] -or $Value -is [decimal] -or $Value -is [single]) {
		# JCS 把整数值的浮点写成整数形态（1.0 → "1"）。PowerShell 5.1 的 ConvertFrom-Json 会把
		# 1.0 解析成 [decimal]，而 decimal 不支持 'R' 格式符，所以统一折成 double 再判整数。
		# 本脚本比对的数据（事件行、manifest）里只有字符串与 null，这一支是防御性的。
		$d = [double]$Value
		if ($d -eq [math]::Floor($d) -and [math]::Abs($d) -lt 1e15) {
			return ([long]$d).ToString([System.Globalization.CultureInfo]::InvariantCulture)
		}
		return $d.ToString('R', [System.Globalization.CultureInfo]::InvariantCulture)
	}
	if ($Value -is [System.Collections.IDictionary]) {
		$keys = Sort-Ordinal @($Value.Keys | ForEach-Object { [string]$_ })
		$parts = @()
		foreach ($k in $keys) { $parts += ((ConvertTo-CanonicalString $k) + ':' + (ConvertTo-CanonicalJson $Value[$k])) }
		return '{' + ($parts -join ',') + '}'
	}
	if ($Value -is [System.Management.Automation.PSCustomObject]) {
		$props = @($Value.PSObject.Properties)
		$keys = Sort-Ordinal @($props | ForEach-Object { $_.Name })
		$parts = @()
		foreach ($k in $keys) { $parts += ((ConvertTo-CanonicalString $k) + ':' + (ConvertTo-CanonicalJson $Value.$k)) }
		return '{' + ($parts -join ',') + '}'
	}
	if ($Value -is [System.Collections.IEnumerable]) {
		$parts = @()
		foreach ($item in $Value) { $parts += (ConvertTo-CanonicalJson $item) }
		return '[' + ($parts -join ',') + ']'
	}
	throw "canonical: 不支持的取值类型 $($Value.GetType().FullName)"
}

function Get-DeclarationHash([string]$manifestJson) {
	$obj = $manifestJson | ConvertFrom-Json
	$canon = ConvertTo-CanonicalJson $obj
	return 'sha256:' + (Get-Sha256Hex ([System.Text.Encoding]::UTF8.GetBytes($canon)))
}

# ---------- 签名请求 ----------
#
# 注册/心跳必须带 X-Tenon-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, "<t>." + rawBody))。
# 请求体走**文件 + curl --data-binary**：签名覆盖的是原始字节，PowerShell 把含引号的字符串
# 交给原生程序时会剥引号（本仓库其它脚本已踩过），而字节一旦变了签名就必然对不上。

function Get-SignatureHeader([byte[]]$body) {
	$ts = [int64]([DateTimeOffset]::UtcNow.ToUnixTimeSeconds())
	$prefix = [System.Text.Encoding]::ASCII.GetBytes("$ts.")
	$buf = New-Object byte[] ($prefix.Length + $body.Length)
	[System.Array]::Copy($prefix, 0, $buf, 0, $prefix.Length)
	[System.Array]::Copy($body, 0, $buf, $prefix.Length, $body.Length)
	$hmac = New-Object System.Security.Cryptography.HMACSHA256
	try {
		$hmac.Key = $script:secretBytes
		$digest = $hmac.ComputeHash($buf)
	} finally {
		$hmac.Dispose()
	}
	$hex = ([System.BitConverter]::ToString($digest)).Replace('-', '').ToLowerInvariant()
	return "t=$ts,v1=$hex"
}

function Invoke-SignedPost([string]$url, [string]$body) {
	$bodyFile = Join-Path $work 'signed-body.json'
	$respFile = Join-Path $work 'signed-resp.json'
	[System.IO.File]::WriteAllText($bodyFile, $body, (New-Object System.Text.UTF8Encoding($false)))
	$sig = Get-SignatureHeader ([System.IO.File]::ReadAllBytes($bodyFile))
	$code = & curl.exe -s -X POST $url -H 'Content-Type: application/json' -H "X-Tenon-Signature: $sig" `
		--data-binary "@$bodyFile" -o $respFile -w '%{http_code}'
	if ($LASTEXITCODE -ne 0) { return @{ Status = 0; Body = "curl 退出码 $LASTEXITCODE" } }
	$text = ''
	if (Test-Path $respFile) { $text = [System.IO.File]::ReadAllText($respFile, [System.Text.Encoding]::UTF8) }
	return @{ Status = [int]$code; Body = $text }
}

function Invoke-AdminPost([string]$baseURL, [string]$body) {
	$bodyFile = Join-Path $work 'admin-body.json'
	$respFile = Join-Path $work 'admin-resp.json'
	[System.IO.File]::WriteAllText($bodyFile, $body, (New-Object System.Text.UTF8Encoding($false)))
	$code = & curl.exe -s -X POST "$baseURL/tenon/admin/modules" -H 'Content-Type: application/json' `
		--data-binary "@$bodyFile" -o $respFile -w '%{http_code}'
	if ($LASTEXITCODE -ne 0) { return @{ Status = 0; Body = "curl 退出码 $LASTEXITCODE" } }
	$text = ''
	if (Test-Path $respFile) { $text = [System.IO.File]::ReadAllText($respFile, [System.Text.Encoding]::UTF8) }
	return @{ Status = [int]$code; Body = $text }
}

# ---------- 注册事件链 ----------

# Read-SharedLines 以**共享方式**读整个文件，并按 '\n' 切行。
#
# 两个都不能省：
#   - 共享模式：事件链被宿主以写句柄持有。.NET 的 File.ReadAllLines 默认 FileShare.Read
#     （＝不许别人写），而宿主已经持了写句柄，于是直接 IOException「文件正被另一进程使用」。
#     这里显式 allow Read|Write 共享，读到的仍是当前完整内容。
#   - 丢掉未写完的尾行：Go 侧一次 Write 落「整行 + '\n'」并 fsync，所以**完整的记录一定以
#     换行结尾**。读到末尾没有换行说明那行正在被追加，丢掉它，下一次读就完整了——这比
#     「偶尔校验失败一下」更接近事实。
function Read-SharedLines([string]$path) {
	if (-not (Test-Path $path)) { return @() }
	$fs = $null
	$reader = $null
	try {
		$fs = [System.IO.File]::Open($path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
		$reader = New-Object System.IO.StreamReader($fs, [System.Text.Encoding]::UTF8)
		$text = $reader.ReadToEnd()
	} finally {
		if ($null -ne $reader) { $reader.Dispose() } elseif ($null -ne $fs) { $fs.Dispose() }
	}
	if ($text.Length -gt 0 -and -not $text.EndsWith("`n")) {
		$cut = $text.LastIndexOf("`n")
		if ($cut -lt 0) { $text = '' } else { $text = $text.Substring(0, $cut + 1) }
	}
	return @($text -split "`n")
}

function Get-Events([string]$path) {
	if (-not (Test-Path $path)) { return @() }
	$out = @()
	$lines = Read-SharedLines $path
	for ($i = 0; $i -lt $lines.Length; $i++) {
		$raw = $lines[$i].Trim()
		if ($raw -eq '') { continue }
		try { $rec = $raw | ConvertFrom-Json } catch { continue }
		$out += [pscustomobject]@{
			Line     = $i + 1
			Event    = [string]$rec.event
			ModuleID = [string]$rec.moduleId
			At       = [string]$rec.at
		}
	}
	return $out
}

function Format-Events($events) {
	if (@($events).Count -eq 0) { return '（空）' }
	return (@($events | ForEach-Object { "$($_.Event):$($_.ModuleID)@L$($_.Line)" }) -join ' ')
}

# Test-RegistryChain 独立复算整条注册事件链。
#
# 与 internal/controlplane/registrylog.Verify 做同一件事，但**不调用它**：验收脚本若用被测
# 代码来验证被测代码，同错就同过。这里只依赖规范化 + SHA-256 这两件与实现无关的事。
function Test-RegistryChain([string]$path) {
	$known = @('at', 'declarationHash', 'event', 'moduleId', 'policyId', 'prevHash', 'reason', 'ruleFile', 'hash')
	if (-not (Test-Path $path)) { return @{ Ok = $false; Line = 0; Reason = "文件不存在：$path"; Count = 0 } }
	$lines = Read-SharedLines $path
	$prev = $null
	$count = 0
	for ($i = 0; $i -lt $lines.Length; $i++) {
		$raw = $lines[$i].Trim()
		if ($raw -eq '') { continue }
		$count++
		$lineNo = $i + 1
		try { $rec = $raw | ConvertFrom-Json } catch { return @{ Ok = $false; Line = $lineNo; Reason = '记录无法解析：' + $_.Exception.Message; Count = $count } }
		$tree = @{}
		foreach ($p in $rec.PSObject.Properties) {
			if ($known -notcontains $p.Name) {
				return @{ Ok = $false; Line = $lineNo; Reason = "出现未声明的字段 $($p.Name)（脚本的规范化实现需要同步更新）"; Count = $count }
			}
			if ($p.Name -ne 'hash') { $tree[$p.Name] = $p.Value }
		}
		$want = 'sha256:' + (Get-Sha256Hex ([System.Text.Encoding]::UTF8.GetBytes((ConvertTo-CanonicalJson $tree))))
		if ([string]$rec.hash -ne $want) {
			return @{ Ok = $false; Line = $lineNo; Reason = "记录内容与自身 hash 不符（重算 $want，记录里是 $($rec.hash)）"; Count = $count }
		}
		# prevHash 连续性：链首为 null，之后必须指向上一条的 hash。
		$prevInRecord = $null
		if ($null -ne $rec.PSObject.Properties['prevHash']) { $prevInRecord = $rec.prevHash }
		if ($null -eq $prev) {
			if ($null -ne $prevInRecord) {
				return @{ Ok = $false; Line = $lineNo; Reason = "链首的 prevHash 应为 null，实际是 $prevInRecord"; Count = $count }
			}
		} elseif ([string]$prevInRecord -ne [string]$prev) {
			return @{ Ok = $false; Line = $lineNo; Reason = "prevHash 未接上一条记录（期望 $prev，实际 $($prevInRecord)）"; Count = $count }
		}
		$prev = [string]$rec.hash
	}
	return @{ Ok = $true; Line = 0; Reason = ''; Count = $count }
}

# ---------- 主流程 ----------

# 1) 端口：被占时报出占者，不静默重试（重试会连到别人的进程上，给出更离谱的结论）。
foreach ($port in 8080, 8081, 8082, 8083) {
	$busy = $false
	try {
		$busy = [bool](Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)
	} catch {
		$client = New-Object System.Net.Sockets.TcpClient
		try { $client.Connect('127.0.0.1', $port); $busy = $true } catch { $busy = $false } finally { $client.Dispose() }
	}
	if ($busy) { Fail "端口 $port 被占用：先执行 powershell -NoProfile -File scripts/dev-down.ps1（或停掉占者）再跑" }
}

# 2) 干净状态：整目录重建。事件链必须从空链开始——本脚本要断言「第一条 registered 出现在
# 第几行」「篡改第 N 行会被发现」，沿用旧链会让行号与内容都不可预期。
if (Test-Path $work) { Remove-Item -Recurse -Force $work }
New-Item -ItemType Directory -Force -Path $bin, $logDir | Out-Null

# 3) 构建：先构建再起进程，避免「go run 还在编译，readyz 超时」的假失败。
foreach ($svc in 'host', 'inventory', 'procurement', 'quote') {
	& go build -o (Join-Path $bin "$svc.exe") "./cmd/$svc"
	if ($LASTEXITCODE -ne 0) { Fail "构建 $svc 失败" }
}

# 4) 共享密钥：宿主与模块必须是同一份字节，各生成一份只会得到 401。
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$buf = New-Object byte[] 32
$rng.GetBytes($buf)
[System.IO.File]::WriteAllText($hmacFile, [System.BitConverter]::ToString($buf).Replace('-', '').ToLowerInvariant())
$secretText = [System.IO.File]::ReadAllText($hmacFile, [System.Text.Encoding]::UTF8).Trim().TrimStart([char]0xFEFF)
$script:secretBytes = [System.Text.Encoding]::UTF8.GetBytes($secretText)

Write-Host ''
Write-Host '=== 起三个模块（自注册）与宿主（测试态 + 事件链）===' -ForegroundColor Cyan
try {
	foreach ($id in $expectedModules) {
		$script:moduleProcs[$id] = Start-Svc $id (ModuleEnv $id)
	}
	Wait-Ready 'inventory' 'http://127.0.0.1:8081/readyz'
	Wait-Ready 'procurement' 'http://127.0.0.1:8082/readyz'
	Wait-Ready 'quote' 'http://127.0.0.1:8083/readyz'
	$script:hostProc = Start-Svc 'host' (HostEnv)
	Wait-Ready 'host' "$hostURL/readyz"

	# ---- A1：§10.1 注册后立即出现 ----
	$vis = Wait-VisibleIds -baseURL $hostURL -expected $expectedModules -timeoutSec $VisibleTimeoutSec
	$entryStates = @()
	if ($vis.Ok) {
		foreach ($id in $expectedModules) {
			$e = Get-Entry $hostURL $id
			if ($e) { $entryStates += "$id(enabled=$($e.enabled),state=$($e.state))" } else { $entryStates += "$id(缺失)" }
		}
	}
	$allAlive = ($entryStates.Count -eq 3) -and (@($entryStates | Where-Object { $_ -like '*(enabled=True,state=alive)' }).Count -eq 3)
	Check 'A1' '§10.1 三模块自注册后立即出现在 GET /tenon/modules' ($vis.Ok -and $allAlive) `
		("可见 [$($vis.Seen -join ', ')]，用时 {0:N1}s；管理面：{1}" -f $vis.Elapsed, ($entryStates -join ' '))
	$events = Get-Events $registryEvents
	$registeredIDs = @($events | Where-Object { $_.Event -eq 'registered' } | ForEach-Object { $_.ModuleID } | Sort-Object -Unique)
	Check 'A1b' '§10.1/§7 事件链为三模块各写了一条 registered' `
		((@($expectedModules | Where-Object { $registeredIDs -notcontains $_ }).Count -eq 0)) `
		("registered：[$($registeredIDs -join ', ')]；链共 $(@($events).Count) 行")

	# ---- A2：§10.3 错哈希 409 + 重申报恢复 ----
	$probeHashV1 = Get-DeclarationHash $probeManifestV1
	$probeHashV2 = Get-DeclarationHash $probeManifestV2
	$regV1 = '{"module":{"id":"' + $probeID + '","baseURL":"' + $probeURL + '","manifest":' + $probeManifestV1 + '},"declarationHash":"' + $probeHashV1 + '"}'
	$r = Invoke-SignedPost "$hostURL/tenon/registry/register" $regV1
	$probeVisible = -not ((Get-VisibleIds $hostURL).Ids -notcontains $probeID)
	Check 'A2a' '§10.1/§3.1 带签名与正确声明哈希的注册 → 200 accepted，且立即出现在可见列表' `
		(($r.Status -eq 200) -and ($r.Body -like '*"accepted":true*') -and $probeVisible) `
		("HTTP $($r.Status) body=$($r.Body.Trim())；可见=$probeVisible")

	# 声明哈希与 manifest 不符 → 409 declaration-mismatch（拒绝半真半假的申报）
	$regMismatch = '{"module":{"id":"' + $probeID + '","baseURL":"' + $probeURL + '","manifest":' + $probeManifestV2 + '},"declarationHash":"' + $probeHashV1 + '"}'
	$r = Invoke-SignedPost "$hostURL/tenon/registry/register" $regMismatch
	Check 'A2b' '§3.1 声明哈希与 manifest 不符 → 409 declaration-mismatch' `
		(($r.Status -eq 409) -and ($r.Body -like '*"declaration-mismatch"*')) `
		("HTTP $($r.Status) body=$($r.Body.Trim())")

	# 心跳带错哈希 → 409 declaration-changed（且**不**把状态改成 lost：409 是要求重申报，
	# 不是判死。这条区分很重要——把 409 当死亡会让模块升级期间凭空消失）
	$wrongHash = 'sha256:' + ('a' * 64)
	$hbWrong = '{"moduleId":"' + $probeID + '","declarationHash":"' + $wrongHash + '"}'
	$r = Invoke-SignedPost "$hostURL/tenon/registry/heartbeat" $hbWrong
	$entryAfter409 = Get-Entry $hostURL $probeID
	Check 'A2c' '§10.3 心跳带错哈希 → 409 reRegister + reason=declaration-changed' `
		(($r.Status -eq 409) -and ($r.Body -like '*"reRegister":true*') -and ($r.Body -like '*"declaration-changed"*')) `
		("HTTP $($r.Status) body=$($r.Body.Trim())；409 后 state=$($entryAfter409.state)")

	# 未知模块 → 409 unknown-module（宿主重启后模块走的就是这条）
	$hbGhost = '{"moduleId":"ghost-registry","declarationHash":"' + $wrongHash + '"}'
	$r = Invoke-SignedPost "$hostURL/tenon/registry/heartbeat" $hbGhost
	Check 'A2d' '§3.2 未知模块的心跳 → 409 reRegister + reason=unknown-module' `
		(($r.Status -eq 409) -and ($r.Body -like '*"unknown-module"*')) `
		("HTTP $($r.Status) body=$($r.Body.Trim())")

	# 缺签名 → 401（注册端点属新增写入口，必须鉴权）
	$bodyFile = Join-Path $work 'unsigned-body.json'
	[System.IO.File]::WriteAllText($bodyFile, $regV1, (New-Object System.Text.UTF8Encoding($false)))
	$unsignedCode = [int](& curl.exe -s -X POST "$hostURL/tenon/registry/register" -H 'Content-Type: application/json' `
		--data-binary "@$bodyFile" -o (Join-Path $work 'unsigned-resp.json') -w '%{http_code}')
	Check 'A2e' '§5 无签名的注册请求 → 401' ($unsignedCode -eq 401) ("HTTP $unsignedCode")

	# 重申报（换了 manifest）→ 200，事件链出现 re-registered，模块回到 alive
	$regV2 = '{"module":{"id":"' + $probeID + '","baseURL":"' + $probeURL + '","manifest":' + $probeManifestV2 + '},"declarationHash":"' + $probeHashV2 + '"}'
	$r = Invoke-SignedPost "$hostURL/tenon/registry/register" $regV2
	$probeVisible = -not ((Get-VisibleIds $hostURL).Ids -notcontains $probeID)
	$probeEntry = Get-Entry $hostURL $probeID
	$reregistered = @(Get-Events $registryEvents | Where-Object { $_.Event -eq 're-registered' -and $_.ModuleID -eq $probeID })
	Check 'A2f' '§10.3 重申报（声明变化）→ 200、alive，事件链出现 re-registered' `
		(($r.Status -eq 200) -and $probeVisible -and ($probeEntry.state -eq 'alive') -and ($reregistered.Count -ge 1)) `
		("HTTP $($r.Status)；可见=$probeVisible state=$($probeEntry.state)；re-registered=$($reregistered.Count) 条")

	# ---- A3：§10.2 杀模块 → 消失 + 事件链 lost ----
	$invEntry = Get-Entry $hostURL 'inventory'
	if ($null -eq $invEntry) { Fail "管理面注册表里没有 inventory：A1 之后它不该消失，先查 A1 的失败原因" }
	# lastSeen 是宿主写的时间戳（RFC3339Nano）。Windows 时钟精度 100ns，小数位最多 7 位；
	# 这里仍裁掉多余位数，避免 .NET 的 DateTime.Parse 对 >7 位小数直接抛 FormatException。
	$lastSeenRaw = ([string]$invEntry.lastSeen) -replace '(\.\d{7})\d+', '$1'
	$lastSeen = [datetime]::Parse($lastSeenRaw, [System.Globalization.CultureInfo]::InvariantCulture, [System.Globalization.DateTimeStyles]::RoundtripKind)
	$killAt = [datetime]::UtcNow
	$lastSeenAgeAtKill = ($killAt - $lastSeen.ToUniversalTime()).TotalSeconds
	Stop-Svc $script:moduleProcs['inventory']
	$gone = Wait-GoneId -baseURL $hostURL 'inventory' -timeoutSec $LostTimeoutSec
	$goneAt = [datetime]::UtcNow
	$sinceLastSeen = ($goneAt - $lastSeen.ToUniversalTime()).TotalSeconds
	$sinceKill = ($goneAt - $killAt).TotalSeconds
	$invEntry = Get-Entry $hostURL 'inventory'
	$lostEvents = @(Get-Events $registryEvents | Where-Object { $_.Event -eq 'lost' -and $_.ModuleID -eq 'inventory' })
	# 下界放宽 1s：TTL 判定用宿主的单调时钟（time.Since），我们量的是墙上时钟差，
	# 两边差个百毫秒不该让「TTL 没被改短」这条断言变红；上界才是真正的约束。
	Check 'A3' '§10.2 停心跳后消失且在 TTL(15s)+清扫(5s) 内判 lost，事件链出现 lost' `
		($gone.Ok -and ($sinceLastSeen -ge ($ttlSec - 1)) -and ($sinceLastSeen -le ($lostBoundSec + 1)) -and ($lostEvents.Count -ge 1) -and ($invEntry.state -eq 'lost')) `
		("消失于距最后一次心跳 {0:N1}s / 距杀进程 {1:N1}s（杀时距心跳 {2:N1}s）；state={3}；lost 事件 {4} 条" -f $sinceLastSeen, $sinceKill, $lastSeenAgeAtKill, $invEntry.state, $lostEvents.Count)

	# ---- A4：§10.2 恢复侧：重启模块 → 回场 + 事件链 recovered ----
	$lostLine = if ($lostEvents.Count -gt 0) { $lostEvents[-1].Line } else { 0 }
	$script:moduleProcs['inventory'] = Start-Svc 'inventory' (ModuleEnv 'inventory') 'inventory-restart'
	$back = Wait-VisibleIds -baseURL $hostURL -expected @('inventory') -timeoutSec $RejoinTimeoutSec
	$recEvents = @(Get-Events $registryEvents | Where-Object { $_.ModuleID -eq 'inventory' -and $_.Line -gt $lostLine -and ($_.Event -eq 'recovered' -or $_.Event -eq 'registered') })
	Check 'A4' '§10.2 被杀模块重启后重新可见，事件链在 lost 之后出现 recovered/registered' `
		($back.Ok -and ($recEvents.Count -ge 1)) `
		("回场用时 {0:N1}s；lost 行 L{1} 之后的事件：{2}" -f $back.Elapsed, $lostLine, (Format-Events $recEvents))

	# ---- A5：§10.5 人工 disable 粘性 / enable 恢复 ----
	$r = Invoke-AdminPost $hostURL ('{"id":"inventory","enabled":false}')
	$gone = Wait-GoneId -baseURL $hostURL 'inventory' -timeoutSec 5
	$disabledEntry = Get-Entry $hostURL 'inventory'
	# 等过一个完整心跳周期：心跳还在正常刷新（state=alive），但可见性必须**不**回来。
	Start-Sleep -Seconds ($hbSec + 1)
	$stillGone = (Get-VisibleIds $hostURL).Ids -notcontains 'inventory'
	$afterHeartbeatEntry = Get-Entry $hostURL 'inventory'
	$r2 = Invoke-AdminPost $hostURL ('{"id":"inventory","enabled":true}')
	$back = Wait-VisibleIds -baseURL $hostURL -expected @('inventory') -timeoutSec 5
	$manualEvents = @(Get-Events $registryEvents | Where-Object { $_.ModuleID -eq 'inventory' -and ($_.Event -eq 'manual-disabled' -or $_.Event -eq 'manual-enabled') } | ForEach-Object { $_.Event })
	Check 'A5' '§10.5 disable 后不可见且心跳救不回来；enable 后立即可见' `
		(($r.Status -eq 200) -and $gone.Ok -and ($disabledEntry.enabled -eq $false) -and $stillGone -and ($afterHeartbeatEntry.state -eq 'alive') -and $back.Ok -and ($manualEvents -contains 'manual-disabled') -and ($manualEvents -contains 'manual-enabled')) `
		("disable 后 state=$($afterHeartbeatEntry.state)/enabled=$($afterHeartbeatEntry.enabled) 且等 $($hbSec + 1)s 仍不可见=$stillGone；enable 后 {0:N1}s 可见={1}；事件 [$(($manualEvents | Select-Object -Unique) -join ', ')]" -f $back.Elapsed, $back.Ok)

	# ---- A6：§10.4 宿主重启 → 空表 → 一个心跳周期内 409 重申报 ----
	$linesBefore = @(Get-Events $registryEvents).Count
	Stop-Svc $script:hostProc
	$script:hostProc = Start-Svc 'host' (HostEnv) 'host-restart'
	Wait-Ready 'host' "$hostURL/readyz"
	# 重启后第一件事：注册表必须是空的（D6 不落盘）。这一步同时证明「窗口存在」——
	# 若这里就看见 3 个模块，说明有某种持久化把 D6 破坏了。
	$immediate = Get-VisibleIds $hostURL
	$immediateCount = if ($immediate.Ok) { $immediate.Ids.Count } else { -1 }
	$recover = Wait-VisibleIds -baseURL $hostURL -expected $expectedModules -timeoutSec ($HostRestartLimitSec + 6)
	# 409 路径的直接证据：模块日志里出现「心跳被要求重申报（unknown-module）」。
	$unknownHits = 0
	foreach ($logName in 'inventory-restart', 'procurement', 'quote') {
		$f = Join-Path $logDir "$logName.err"
		if (Test-Path $f) {
			$unknownHits += @(Select-String -Path $f -Pattern 'unknown-module' -SimpleMatch).Count
		}
	}
	$eventsAfter = @(Get-Events $registryEvents)
	$newEvents = @($eventsAfter | Select-Object -Skip $linesBefore)
	$newRegistered = @($newEvents | Where-Object { $_.Event -eq 'registered' -and ($expectedModules -contains $_.ModuleID) })
	Check 'A6' '§10.4 宿主重启 → 注册表为空（窗口存在）→ 一个心跳周期内 409 重申报回场' `
		(($immediateCount -ge 0) -and ($immediateCount -lt 3) -and $recover.Ok -and ($recover.Elapsed -le $HostRestartLimitSec) -and ($unknownHits -ge 1) -and ($newRegistered.Count -ge 3)) `
		("重启后首个读数可见 {0} 个（<3 即窗口存在）；全部回场用时 {1:N1}s（上限 {2}s）；模块日志出现 unknown-module {3} 次；链新增 registered {4} 条" -f $immediateCount, $recover.Elapsed, $HostRestartLimitSec, $unknownHits, $newRegistered.Count)

	# ---- A7：§10.6 事件链完整性 + 篡改必被发现 ----
	$verify = Test-RegistryChain $registryEvents
	$events = Get-Events $registryEvents
	Check 'A7a' '§7 真实事件链逐行复算通过（规范 JSON + sha256 + prevHash 连续）' `
		($verify.Ok -and ($verify.Count -ge 6)) `
		("行数 $($verify.Count)；$(Format-Events $events)")

	# 篡改 1：改掉中间一行的 event 取值（记录内容变了、hash 没变）
	$lines = @(Read-SharedLines $registryEvents | Where-Object { $_.Trim() -ne '' })
	$mid = [int][math]::Floor($lines.Count / 2)
	if ($mid -lt 1) { $mid = 1 }
	$tampered = @($lines)
	$tampered[$mid] = $tampered[$mid] -replace '"event":"[^"]*"', '"event":"forged-event"'
	$changed = ($tampered[$mid] -ne $lines[$mid])
	$copyEvent = Join-Path $work 'registry-events.tampered-event.jsonl'
	[System.IO.File]::WriteAllLines($copyEvent, $tampered, (New-Object System.Text.UTF8Encoding($false)))
	$bad1 = Test-RegistryChain $copyEvent
	Check 'A7b' '§10.6 篡改某行的 event 取值 → 链校验失败' `
		($changed -and (-not $bad1.Ok)) `
		("篡改第 $($mid + 1) 行；校验结果 Ok=$($bad1.Ok) line=$($bad1.Line) reason=$($bad1.Reason)")

	# 篡改 2：删掉一行（prevHash 接不上）
	$dropped = @()
	if ($mid -gt 0) { $dropped += $lines[0..($mid - 1)] }
	if ($mid -lt ($lines.Count - 1)) { $dropped += $lines[($mid + 1)..($lines.Count - 1)] }
	$copyDrop = Join-Path $work 'registry-events.tampered-dropped.jsonl'
	[System.IO.File]::WriteAllLines($copyDrop, $dropped, (New-Object System.Text.UTF8Encoding($false)))
	$bad2 = Test-RegistryChain $copyDrop
	Check 'A7c' '§10.6 删除某行 → 链校验失败（prevHash 接不上）' `
		((-not $bad2.Ok) -and ($bad2.Reason -like '*prevHash*')) `
		("删掉第 $($mid + 1) 行；校验结果 Ok=$($bad2.Ok) line=$($bad2.Line) reason=$($bad2.Reason)")

	# ---- A8：§10.7 缺 HMAC 密钥 → 宿主启动失败 ----
	# 只给 PORT，不给 TENON_HMAC_SECRET_FILE：host 必须在装配期就拒绝启动（D7），
	# 而不是「起来了但注册端点永远 503」——后者会让运维以为一切正常。
	$exit1 = Start-HostAndWait @{ PORT = '8097'; TENON_TEST_MODE = '1' } 'host-nohmac'
	$err1 = ''
	$f1 = Join-Path $logDir 'host-nohmac.err'
	if (Test-Path $f1) { $err1 = ([System.IO.File]::ReadAllText($f1, [System.Text.Encoding]::UTF8)).Trim() }
	Check 'A8a' '§10.7 未设 TENON_HMAC_SECRET_FILE → 宿主启动失败（非 0 退出）并点名环境变量' `
		(($exit1 -ne 0) -and ($err1 -like '*TENON_HMAC_SECRET_FILE*')) `
		("退出码 $exit1；stderr：$($err1 -replace "`r?`n", ' | ')")

	$missingPath = Join-Path $work 'no-such-secret-file'
	$exit2 = Start-HostAndWait @{ PORT = '8097'; TENON_HMAC_SECRET_FILE = $missingPath } 'host-badhmac'
	$err2 = ''
	$f2 = Join-Path $logDir 'host-badhmac.err'
	if (Test-Path $f2) { $err2 = ([System.IO.File]::ReadAllText($f2, [System.Text.Encoding]::UTF8)).Trim() }
	Check 'A8b' '§10.7 密钥文件读不到 → 宿主启动失败（不是静默降级成「不挂端点」）' `
		(($exit2 -ne 0) -and ($err2 -like '*TENON_HMAC_SECRET_FILE*')) `
		("退出码 $exit2；stderr：$($err2 -replace "`r?`n", ' | ')")
}
finally {
	foreach ($id in @($script:moduleProcs.Keys)) { Stop-Svc $script:moduleProcs[$id] }
	Stop-Svc $script:hostProc
	foreach ($p in $script:procs) { if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } }
}

Write-Host ''
Write-Host '==== 断言汇总（spec §10 七条口径）====' -ForegroundColor Cyan
foreach ($res in $script:results) {
	$mark = '✗'
	$color = 'Red'
	if ($res.Ok) { $mark = '✓'; $color = 'Green' }
	Write-Host ("{0} [{1}] {2}" -f $mark, $res.Id, $res.Title) -ForegroundColor $color
	Write-Host ("      {0}" -f $res.Detail) -ForegroundColor Gray
}
$failed = @($script:results | Where-Object { -not $_.Ok })
Write-Host ''
if ($failed.Count -gt 0) {
	Write-Host "✗ 注册验收未通过：$($failed.Count)/$($script:results.Count) 条断言失败（失败项：$((@($failed | ForEach-Object { $_.Id })) -join ', ')）" -ForegroundColor Red
	Write-Host "  证据：$registryEvents、$logDir"
	exit 1
}
Write-Host "✓ 注册验收通过：$($script:results.Count) 条断言全部成立（spec §10 七条口径已逐条覆盖）" -ForegroundColor Green
Write-Host "  事件链：$registryEvents"
Write-Host "  日志：  $logDir"
exit 0
