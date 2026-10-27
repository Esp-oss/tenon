#!/usr/bin/env node
/**
 * 校验 spec/0.1 的 schema 与文档完整性。
 *
 * 不引入任何依赖：只做「结构合法性 + 交叉引用完整性」的静态检查，
 * 目的是让 CI 能挡住手改 JSON 造成的低级错误。完整实例校验（ajv）
 * 属于 conformance 套件的职责，不在本脚本范围内。
 *
 * 退出码：0 全绿 / 1 有失败项
 */
import { readFileSync, readdirSync, existsSync } from 'node:fs'
import { join, dirname, basename } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const SPEC = join(ROOT, 'spec', '0.1')
const SCHEMAS = join(SPEC, 'schemas')
const DRAFT = 'https://json-schema.org/draft/2020-12/schema'
const REQUIRED_META = ['$schema', '$id', 'title', 'description', 'type']
const COMMON = 'common.schema.json'

const REQUIRED_DOCS = [
  'README.md',
  'CHANGELOG.md',
  'state-machines.md',
  'semantics.md',
  'versioning.md',
  'errors.md',
]

const failures = []
const notes = []

const fail = (file, msg) => failures.push(`${file}: ${msg}`)
const ok = (file, msg) => notes.push(`  ok  ${file} — ${msg}`)

/** 取出某文件内所有 `#/$defs/Name` 引用 */
function localDefRefs(node, out = []) {
  if (Array.isArray(node)) {
    for (const v of node) localDefRefs(v, out)
  } else if (node && typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) {
      if (k === '$ref' && typeof v === 'string') {
        const m = v.match(/^#\/\$defs\/([A-Za-z0-9_.-]+)$/)
        if (m) out.push(m[1])
      } else {
        localDefRefs(v, out)
      }
    }
  }
  return out
}

/** 取出所有指向 common.schema.json 的 def 引用 */
function commonDefRefs(node, out = []) {
  if (Array.isArray(node)) {
    for (const v of node) commonDefRefs(v, out)
  } else if (node && typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) {
      if (k === '$ref' && typeof v === 'string') {
        const m = v.match(/common\.schema\.json#\/\$defs\/([A-Za-z0-9_.-]+)$/)
        if (m) out.push(m[1])
      } else {
        commonDefRefs(v, out)
      }
    }
  }
  return out
}

function main() {
  if (!existsSync(SCHEMAS)) {
    fail('spec/0.1/schemas', '目录不存在')
    return report()
  }

  // 1. 必需文档
  for (const doc of REQUIRED_DOCS) {
    const p = join(SPEC, doc)
    if (!existsSync(p)) fail(`spec/0.1/${doc}`, '缺少必需文档')
  }

  // 2. 逐个 schema
  const files = readdirSync(SCHEMAS).filter((f) => f.endsWith('.json')).sort()
  if (files.length === 0) fail('spec/0.1/schemas', '没有任何 schema 文件')

  const parsed = new Map()
  for (const f of files) {
    const p = join(SCHEMAS, f)
    let doc
    try {
      doc = JSON.parse(readFileSync(p, 'utf8'))
    } catch (e) {
      fail(`schemas/${f}`, `JSON 解析失败：${e.message}`)
      continue
    }
    parsed.set(f, doc)

    for (const key of REQUIRED_META) {
      if (!(key in doc)) fail(`schemas/${f}`, `缺少顶层字段 ${key}`)
    }
    if (doc.$schema !== DRAFT) {
      fail(`schemas/${f}`, `$schema 必须是 ${DRAFT}，实际 ${doc.$schema}`)
    }
    if (typeof doc.$id === 'string' && !doc.$id.startsWith('https://tenon.dev/spec/0.1/schemas/')) {
      fail(`schemas/${f}`, `$id 必须位于 https://tenon.dev/spec/0.1/schemas/ 下，实际 ${doc.$id}`)
    }
    if (typeof doc.$id === 'string' && !doc.$id.endsWith(`/${f}`)) {
      fail(`schemas/${f}`, `$id 文件名与文件名不一致：${doc.$id}`)
    }

    // 同文件内 $ref 必须能在 $defs 找到
    const defs = doc.$defs && typeof doc.$defs === 'object' ? Object.keys(doc.$defs) : []
    for (const need of localDefRefs(doc)) {
      if (!defs.includes(need)) fail(`schemas/${f}`, `引用了不存在的本地 $defs/${need}`)
    }
    ok(`schemas/${f}`, `${defs.length} 个本地 defs`)
  }

  // 3. 跨文件引用必须指向 common 里真实存在的 def
  const common = parsed.get(COMMON)
  if (!common) {
    fail(`schemas/${COMMON}`, '通用 schema 缺失')
  } else {
    const commonDefs = common.$defs && typeof common.$defs === 'object' ? Object.keys(common.$defs) : []
    for (const [f, doc] of parsed) {
      if (f === COMMON) continue
      for (const need of commonDefRefs(doc)) {
        if (!commonDefs.includes(need)) {
          fail(`schemas/${f}`, `引用了 common 里不存在的 $defs/${need}`)
        }
      }
    }
    ok(`schemas/${COMMON}`, `${commonDefs.length} 个通用 defs`)
  }

  // 4. action.schema.json 必须表达 kind=write → idempotency 必填
  const action = parsed.get('action.schema.json')
  if (action) {
    const text = JSON.stringify(action)
    if (!text.includes('"if"') || !text.includes('"then"')) {
      fail('schemas/action.schema.json', '缺少 if/then 条件：kind=write 时 idempotency 必填')
    }
    if (!text.includes('"write"')) {
      fail('schemas/action.schema.json', '缺少 kind 枚举值 write')
    }
  } else {
    fail('schemas/action.schema.json', '缺失（九类契约之一）')
  }

  // 5. 九类契约的 schema 必须齐备
  const EXPECTED = [
    'module-manifest.schema.json',
    'action.schema.json',
    'domain-event.schema.json',
    'plan.schema.json',
    'frames.schema.json',
    'grant.schema.json',
    'policy.schema.json',
    'audit-event.schema.json',
    'error.schema.json',
  ]
  for (const e of EXPECTED) {
    if (!parsed.has(e)) fail(`schemas/${e}`, '九类契约之一的 schema 缺失')
  }

  report()
}

function report() {
  for (const n of notes) console.log(n)
  if (failures.length === 0) {
    console.log(`\n✓ spec 校验通过：${notes.length} 项检查，0 失败`)
    process.exit(0)
  }
  console.error(`\n✗ spec 校验失败：${failures.length} 项`)
  for (const f of failures) console.error(`  - ${f}`)
  process.exit(1)
}

main()
