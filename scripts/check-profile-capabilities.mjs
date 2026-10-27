#!/usr/bin/env node
/**
 * 生产档位门禁（安全威胁模型 §3 档位 A）：检查某个 DSH profile 组合出来的插件树里
 * 有没有「越界能力」——shell、文件写、出网、起子代理/工作流、动态挂插件。
 *
 * 为什么要有这个脚本：ADR-0001 §6.2 把「收窄工具面」从优化升级为**安全前置条件**，
 * 而收窄本身很容易被后续的 profile patch 悄悄加回来。门禁必须能一条命令证明「没加」。
 *
 * 用法：
 *   node scripts/check-profile-capabilities.mjs <profile>        # 档位 A 检查
 *   node scripts/check-profile-capabilities.mjs <profile> --json # 机器可读
 * 退出码：0 合规 / 1 有越界能力 / 2 检查本身失败（如 dsh 不可用）
 */
import { spawnSync } from 'node:child_process'

const profile = process.argv[2]
const asJson = process.argv.includes('--json')

if (!profile) {
  console.error('用法：node scripts/check-profile-capabilities.mjs <profile> [--json]')
  process.exit(2)
}

// profile 名会进 shell 命令行：只允许安全字符，避免把检查脚本变成注入点。
if (!/^[A-Za-z0-9._-]+$/.test(profile)) {
  console.error(`✗ profile 名含非法字符：${JSON.stringify(profile)}`)
  process.exit(2)
}

// 档位 A：生产只允许「业务工具」，下列能力一律不许出现在组合配置里。
// 每条都对应威胁模型里的一条威胁，不要随手删。
const FORBIDDEN = [
  ['@deepseek-ai/dsh-tool-pwsh', 'T1 任意命令执行'],
  ['@deepseek-ai/dsh-tool-pwsh-persistent', 'T1 持久 shell'],
  ['@deepseek-ai/dsh-tool-bash', 'T1 任意命令执行'],
  ['@deepseek-ai/dsh-tool-bash-persistent', 'T1 持久 shell'],
  ['@deepseek-ai/dsh-terminal', 'T1 终端'],
  ['@deepseek-ai/dsh-terminal-bash', 'T1 终端'],
  ['@deepseek-ai/dsh-tool-fs', 'M1 文件写'],
  ['@deepseek-ai/dsh-tool-fs-search', 'M1 文件读（可读凭据，违反 M6）'],
  ['@deepseek-ai/dsh-tool-web', 'M4 出网'],
  ['@deepseek-ai/dsh-web-fetch-http', 'M4 出网'],
  ['@deepseek-ai/dsh-web-search-deepseek', 'M4 出网'],
  ['@deepseek-ai/dsh-tool-subagent', 'T1 派生代理（继承能力）'],
  ['@deepseek-ai/dsh-tool-subagent-control', 'T1 派生代理'],
  ['@deepseek-ai/dsh-subagent-spawn-in-process', 'T1 派生代理'],
  ['@deepseek-ai/dsh-subagent-fork-in-process', 'T1 派生代理'],
  ['@deepseek-ai/dsh-tool-workflow', 'T1 编排代理'],
  ['@deepseek-ai/dsh-workflow-ptc', 'T1 编排代理'],
  ['@deepseek-ai/dsh-tool-cordis', 'T6 运行时改插件树'],
  ['@deepseek-ai/dsh-plugin-manager', 'T6 装插件'],
  ['@deepseek-ai/dsh-mcp-client', 'T6 外部 MCP 可带任意能力'],
]

// Windows 上 dsh 是 .cmd，execFile 系列找不到它；统一走 shell 执行（profile 名已做白名单校验）。
const res = spawnSync(`dsh --profile ${profile} --dump-config`, {
  shell: true,
  encoding: 'utf8',
  env: { ...process.env, NO_COLOR: '1' },
})
if (res.error || typeof res.stdout !== 'string' || res.stdout.length === 0) {
  console.error(`✗ 无法读取 profile ${profile} 的组合配置（dsh 不可用或 profile 不存在）`)
  console.error(String(res.error ?? res.stderr ?? '').slice(0, 400))
  process.exit(2)
}
const dump = res.stdout

const present = new Set([...dump.matchAll(/name:\s*'([^']+)'/g)].map((m) => m[1]))
const violations = FORBIDDEN.filter(([name]) => present.has(name)).map(([name, threat]) => ({ name, threat }))

// M2：权限模式不得为 danger-full-access（在 deploy 环境里检查的是将要注入的环境变量）。
const permissionMode = process.env.DSH_PERMISSION_MODE ?? ''
const modeViolation = permissionMode === 'danger-full-access' ? permissionMode : null

const toolPlugins = [...present].filter((n) => n.startsWith('@deepseek-ai/dsh-tool-')).sort()

if (asJson) {
  console.log(JSON.stringify({ profile, violations, permissionMode: modeViolation, toolPlugins }, null, 2))
} else {
  console.log(`profile=${profile}`)
  console.log(`工具插件（${toolPlugins.length}）：${toolPlugins.join(', ') || '（无）'}`)
  if (violations.length === 0) {
    console.log('✓ 档位 A：未发现越界能力')
  } else {
    console.error(`✗ 档位 A 违规 ${violations.length} 项：`)
    for (const v of violations) console.error(`  - ${v.name}  ← ${v.threat}`)
  }
  if (modeViolation) {
    console.error('✗ M2 违规：DSH_PERMISSION_MODE=danger-full-access（审批会被降级为 never）')
  }
}

process.exit(violations.length === 0 && modeViolation === null ? 0 : 1)
