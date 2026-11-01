#!/usr/bin/env powershell
# 停掉 scripts/dev-up.ps1 起的本地栈（按 out/dev/pids.txt 精确杀，不碰别的进程）。
#
# **必须校验进程身份**：pid 会被系统复用，直接 Stop-Process 可能杀掉无关进程
# （2026-10-27 就撞上 stale 的 host=51420 已被 E:\Trae\Trae.exe 复用）。
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$pidFile = Join-Path $root 'out/dev/pids.txt'
$binDir = Join-Path $root 'out/dev/bin'

if (-not (Test-Path $pidFile)) {
	Write-Host '没有 pids.txt：dev-up 没起过，或已经停过。'
	exit 0
}

$skipped = 0
foreach ($line in Get-Content $pidFile) {
	$parts = $line -split '='
	if ($parts.Count -ne 2) { continue }
	$name = $parts[0]
	$procId = 0
	if (-not [int]::TryParse($parts[1], [ref]$procId)) { continue }

	$p = Get-Process -Id $procId -ErrorAction SilentlyContinue
	if (-not $p) {
		Write-Host "  $name (PID $procId) 已不在"
		continue
	}

	# 身份判据：可执行文件必须是我们自己 build 在 out/dev/bin 下的那个。
	# 取不到 Path（权限/已退出）或路径不符，一律跳过——宁可留残留进程，也不误杀。
	$exePath = $null
	try { $exePath = $p.Path } catch { $exePath = $null }
	$expected = Join-Path $binDir "$name.exe"
	if (-not $exePath -or ($exePath -ne $expected)) {
		Write-Host "  跳过 $name (PID $procId)：进程身份不符（实际=$exePath，期望=$expected）" -ForegroundColor Yellow
		$skipped++
		continue
	}

	Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue
	Write-Host "  已停 $name (PID $procId)"
}

Remove-Item -Force $pidFile
if ($skipped -gt 0) {
	Write-Host "✓ 本地栈已停止（跳过 $skipped 个身份不符的 pid，未误杀）"
} else {
	Write-Host '✓ 本地栈已停止'
}