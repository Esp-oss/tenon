#!/usr/bin/env powershell
# 停掉 scripts/dev-up.ps1 起的本地栈（按 out/dev/pids.txt 精确杀，不碰别的进程）。
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$pidFile = Join-Path $root 'out/dev/pids.txt'

if (-not (Test-Path $pidFile)) {
	Write-Host '没有 pids.txt：dev-up 没起过，或已经停过。'
	exit 0
}

foreach ($line in Get-Content $pidFile) {
	$parts = $line -split '='
	if ($parts.Count -ne 2) { continue }
	$name = $parts[0]
	$procId = 0
	if (-not [int]::TryParse($parts[1], [ref]$procId)) { continue }
	$p = Get-Process -Id $procId -ErrorAction SilentlyContinue
	if ($p) {
		Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue
		Write-Host "  已停 $name (PID $procId)"
	} else {
		Write-Host "  $name (PID $procId) 已不在"
	}
}
Remove-Item -Force $pidFile
Write-Host '✓ 本地栈已停止'
