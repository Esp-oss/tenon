#!/usr/bin/env node
/**
 * 把协议 schema 同步进包内。
 *
 * 为什么复制而不是直接 import 仓库路径：发布后的包不能引用 `../../spec`（路径会断），
 * 而 schema 必须与规范**逐字节一致**——所以复制 + 哈希比对，规范改了没同步就让 CI 红。
 *
 * 用法：node scripts/sync-schemas.mjs [--check]
 *   无参数  写入/更新 packages/tenon-core/schemas/
 *   --check 只比对，不一致则 exit 1（CI 用）
 */
import { createHash } from 'node:crypto'
import { copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const srcDir = join(here, '..', '..', '..', 'spec', '0.1', 'schemas')
const dstDir = join(here, '..', 'schemas')
const checkOnly = process.argv.includes('--check')

if (!existsSync(srcDir)) {
  console.error(`找不到规范 schema 目录：${srcDir}`)
  process.exit(2)
}

const files = readdirSync(srcDir)
  .filter((f) => f.endsWith('.schema.json'))
  .sort()
if (files.length === 0) {
  console.error(`${srcDir} 下没有 *.schema.json`)
  process.exit(2)
}

const hash = (buf) => createHash('sha256').update(buf).digest('hex')
const drift = []

if (!checkOnly) mkdirSync(dstDir, { recursive: true })

for (const file of files) {
  const src = join(srcDir, file)
  const dst = join(dstDir, file)
  const want = readFileSync(src)
  if (!checkOnly) {
    copyFileSync(src, dst)
    continue
  }
  if (!existsSync(dst)) {
    drift.push(`${file}：包内缺失`)
    continue
  }
  if (hash(readFileSync(dst)) !== hash(want)) {
    drift.push(`${file}：内容与规范不一致`)
  }
}

if (checkOnly) {
  if (drift.length > 0) {
    console.error('✗ schema 副本与 spec/0.1/schemas 不一致（跑 pnpm --filter @tenon/core run sync:schemas）：')
    for (const d of drift) console.error(`  - ${d}`)
    process.exit(1)
  }
  console.log(`✓ schema 副本与规范一致（${files.length} 份）`)
  process.exit(0)
}

console.log(`✓ 已同步 ${files.length} 份 schema → ${dstDir}`)
