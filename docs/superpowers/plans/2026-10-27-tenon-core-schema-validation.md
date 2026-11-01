# tenon-core schema 校验实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `@tenon/core` 能对 9 类契约做运行时校验：默认「接收侧宽松」（忽略并剥离未知字段），`{ strict: true }` 走「一致性侧严格」；错误统一成 JSON Pointer 路径。

**Architecture:** 同一批 `spec/0.1/schemas/*.json` 在构建时同步进包内（`schemas/`），运行时用 ajv 2020-12 懒编译成两个视图；对外是 9 个类型安全函数 + 一个按契约名分派的动态入口。

**Tech Stack:** TypeScript 7（NodeNext）、`ajv@8`（`ajv/dist/2020.js`）、`ajv-formats@3`、`node:test`。设计依据：[`docs/superpowers/specs/2026-10-27-tenon-core-schema-validation-design.md`](../specs/2026-10-27-tenon-core-schema-validation-design.md)。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `packages/tenon-core/scripts/sync-schemas.mjs` | 把 `spec/0.1/schemas/*.json` 复制/比对到 `packages/tenon-core/schemas/` |
| `packages/tenon-core/schemas/*.json` | 同步产物（git 跟踪，构建与运行时都只读它） |
| `packages/tenon-core/src/validate/types.ts` | `ContractName`、`ValidationIssue`、`ValidationError`、`ValidateOptions` |
| `packages/tenon-core/src/validate/core.ts` | ajv 实例、两个视图、`check()`、错误归一化 |
| `packages/tenon-core/src/validate/contracts.ts` | 9 个契约函数 + `validateByContract` |
| `packages/tenon-core/src/validate/fixtures.ts` | 9 类契约的最小合法样例（供测试与后续 conformance 复用） |

---

## Task 1: 依赖与 schema 同步脚本

**Files:**
- Create: `packages/tenon-core/scripts/sync-schemas.mjs`
- Modify: `packages/tenon-core/package.json`
- Create（脚本产物）: `packages/tenon-core/schemas/*.json`

- [ ] **Step 1: 装依赖**

Run:
```bash
pnpm --filter @tenon/core add ajv@^8 ajv-formats@^3
```
Expected: `packages/tenon-core/package.json` 的 `dependencies` 出现 `ajv` 与 `ajv-formats`；`pnpm-lock.yaml` 更新。

- [ ] **Step 2: 写同步脚本**

`packages/tenon-core/scripts/sync-schemas.mjs`：

```js
#!/usr/bin/env node
/**
 * 把协议 schema 同步进包内。
 *
 * 为什么复制而不是直接 import：发布后的包不能引用仓库相对路径（../../spec），
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

const files = readdirSync(srcDir).filter((f) => f.endsWith('.schema.json')).sort()
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
  const got = readFileSync(dst)
  if (hash(got) !== hash(want)) drift.push(`${file}：内容与规范不一致`)
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
```

- [ ] **Step 3: 加脚本、放行 `schemas/` 目录**

`packages/tenon-core/package.json`：

```json
  "files": ["dist", "schemas"],
  "scripts": {
    "build": "tsc -p tsconfig.json",
    "sync:schemas": "node scripts/sync-schemas.mjs",
    "sync:schemas:check": "node scripts/sync-schemas.mjs --check",
    "test": "npm run build && node --test \"dist/**/*.test.js\""
  },
```

- [ ] **Step 4: 同步并验证两种模式**

Run:
```bash
pnpm --filter @tenon/core run sync:schemas
pnpm --filter @tenon/core run sync:schemas:check
```
Expected: 先打印 `✓ 已同步 10 份 schema`，再打印 `✓ schema 副本与规范一致（10 份）`。

- [ ] **Step 5: 自证 `--check` 会抓到漂移**

Run（改一个副本，检查失败，再恢复）：
```bash
echo " " >> packages/tenon-core/schemas/plan.schema.json
pnpm --filter @tenon/core run sync:schemas:check; echo "exit=$?"
pnpm --filter @tenon/core run sync:schemas
pnpm --filter @tenon/core run sync:schemas:check
```
Expected: 第二次 `exit=1` 且列出 `plan.schema.json：内容与规范不一致`；重新同步后恢复通过。

- [ ] **Step 6: Commit**

```bash
git add packages/tenon-core/package.json packages/tenon-core/scripts packages/tenon-core/schemas pnpm-lock.yaml
git commit -m "feat(core): 依赖 ajv 并把协议 schema 同步进包内（--check 供 CI 防漂移）"
```

---

## Task 2: 错误模型

**Files:**
- Create: `packages/tenon-core/src/validate/types.ts`
- Test: `packages/tenon-core/src/validate/types.test.ts`

- [ ] **Step 1: 写失败用例**

`packages/tenon-core/src/validate/types.test.ts`：

```ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { ValidationError } from './types.js'

test('ValidationError 带上契约名与全部 issues，消息里含条数', () => {
  const err = new ValidationError('plan', [{ path: '/steps/0', keyword: 'type', message: 'must be object' }])
  assert.equal(err.name, 'ValidationError')
  assert.equal(err.contract, 'plan')
  assert.equal(err.issues.length, 1)
  assert.equal(err.issues[0]?.path, '/steps/0')
  assert.match(err.message, /plan/)
  assert.match(err.message, /1 处/)
})
```

- [ ] **Step 2: 跑用例确认失败**

Run: `pnpm --filter @tenon/core run build`
Expected: 编译失败 `Cannot find module './types.js'`

- [ ] **Step 3: 实现**

`packages/tenon-core/src/validate/types.ts`：

```ts
// 校验层的对外类型：契约名、单条问题、校验失败、校验选项。

/** 有根实例契约的 schema 名（`common.schema.json` 只有 $defs，不在此列）。 */
export type ContractName =
  | 'module-manifest'
  | 'action'
  | 'plan'
  | 'policy'
  | 'grant'
  | 'audit-event'
  | 'domain-event'
  | 'frames'
  | 'error'

/** 一条校验问题。`path` 是 JSON Pointer（根为 `/`），与 Go 侧 `at '/actions/0'` 同形。 */
export interface ValidationIssue {
  path: string
  /** ajv 的 keyword（type / required / additionalProperties / oneOf …），供程序判断，不必解析 message。 */
  keyword: string
  message: string
}

/** 校验失败。带 `contract` 是为了让调用方知道「是哪类报文不合法」。 */
export class ValidationError extends Error {
  readonly contract: ContractName
  readonly issues: ValidationIssue[]

  constructor(contract: ContractName, issues: ValidationIssue[]) {
    super(`${contract} 校验失败（${issues.length} 处）：${issues[0]?.message ?? ''}`)
    this.name = 'ValidationError'
    this.contract = contract
    this.issues = issues
  }
}

export interface ValidateOptions {
  /** true = 一致性视图（未知字段视为错误）；默认 false = 接收视图（剥离并忽略未知字段）。 */
  strict?: boolean
}
```

- [ ] **Step 4: 跑用例确认通过**

Run: `pnpm --filter @tenon/core test`
Expected: PASS（新增 1 条 + 原有 7 条）

- [ ] **Step 5: Commit**

```bash
git add packages/tenon-core/src/validate
git commit -m "feat(core): 校验错误模型（JSON Pointer 路径 + keyword + 契约名）"
```

---

## Task 3: 两个视图的核心校验

**Files:**
- Create: `packages/tenon-core/src/validate/core.ts`
- Test: `packages/tenon-core/src/validate/core.test.ts`

- [ ] **Step 1: 写失败用例**

`packages/tenon-core/src/validate/core.test.ts`：

```ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { check, resetValidators } from './core.js'
import { ValidationError } from './types.js'

const BASE = {
  tenon: '0.1',
  module: { id: 'inventory', version: '1.4.0', title: '库存' },
  actions: [],
  events: [],
  transports: ['http'],
  capabilities: ['dry-run'],
}

test('接收视图：未知字段被剥离且不报错', () => {
  resetValidators()
  const input = structuredClone({ ...BASE, 未在契约里的字段: 1 })
  const out = check<Record<string, unknown>>('module-manifest', input)
  assert.equal('未在契约里的字段' in out, false)
  assert.equal(input['未在契约里的字段'], 1) // 入参不被修改
})

test('一致性视图：未知字段就是错误，path 是 JSON Pointer', () => {
  resetValidators()
  let caught: unknown
  try {
    check('module-manifest', { ...BASE, 未在契约里的字段: 1 }, { strict: true })
  } catch (err) {
    caught = err
  }
  assert.ok(caught instanceof ValidationError)
  assert.equal(caught.contract, 'module-manifest')
  const issue = caught.issues.find((i) => i.keyword === 'additionalProperties')
  assert.ok(issue, `issues 里应有 additionalProperties：${JSON.stringify(caught.issues)}`)
  assert.equal(issue.path, '/')
})

test('类型错：path 指向出错的字段', () => {
  resetValidators()
  assert.throws(
    () => check('module-manifest', { ...BASE, actions: 'oops' }),
    (err: unknown) => {
      assert.ok(err instanceof ValidationError)
      const issue = err.issues.find((i) => i.keyword === 'type')
      assert.ok(issue)
      assert.equal(issue.path, '/actions')
      return true
    },
  )
})
```

- [ ] **Step 2: 跑用例确认失败**

Run: `pnpm --filter @tenon/core test`
Expected: 编译失败 `Cannot find module './core.js'`

- [ ] **Step 3: 实现**

`packages/tenon-core/src/validate/core.ts`：

```ts
// 校验核心：同一批 schema 编成两个视图（接收侧宽松 / 一致性侧严格），懒编译 + 缓存。
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import Ajv2020 from 'ajv/dist/2020.js'
import addFormats from 'ajv-formats'
import type { ValidateFunction } from 'ajv'

import { ValidationError, type ContractName, type ValidateOptions, type ValidationIssue } from './types.js'

/** 包内 schemas/ 目录（dist/validate/core.js → ../../schemas）。 */
const SCHEMA_DIR = fileURLToPath(new URL('../../schemas/', import.meta.url))

/** 每个视图一个 ajv 实例；键是 `契约名:视图`。 */
const cache = new Map<string, ValidateFunction>()

let ajvLenient: Ajv2020 | undefined
let ajvStrict: Ajv2020 | undefined

function build(strict: boolean): Ajv2020 {
  const ajv = new Ajv2020({
    allErrors: true,
    // 一致性视图用 ajv 的严格模式（schema 本身若有可疑写法则编译期就报）；
    // 接收视图关掉它，因为 2020-12 的若干关键字在严格模式下会对「忽略未知字段」策略发难。
    strict,
    // 关键：接收视图剥离未知字段（§3.2 未知字段必须忽略）；一致性视图保留它以便报错。
    removeAdditional: strict ? false : 'all',
  })
  addFormats(ajv)
  for (const file of readdirSync(SCHEMA_DIR).filter((f) => f.endsWith('.schema.json'))) {
    const schema = JSON.parse(readFileSync(join(SCHEMA_DIR, file), 'utf8')) as { $id: string }
    ajv.addSchema(schema, schema.$id)
  }
  return ajv
}

function validator(contract: ContractName, opts: ValidateOptions): ValidateFunction {
  const strict = opts.strict === true
  const key = `${contract}:${strict ? 'strict' : 'lenient'}`
  const hit = cache.get(key)
  if (hit) return hit
  ajvLenient ??= build(false)
  ajvStrict ??= build(true)
  const ajv = strict ? ajvStrict : ajvLenient
  const id = `https://tenon.dev/spec/0.1/schemas/${contract}.schema.json`
  const compiled = ajv.getSchema(id)
  if (!compiled) {
    throw new Error(`schema 未注册：${id}（检查 packages/tenon-core/schemas/ 是否已同步）`)
  }
  cache.set(key, compiled)
  return compiled
}

function toIssues(errors: ValidateFunction['errors']): ValidationIssue[] {
  return (errors ?? []).map((e) => ({
    // ajv 的 instancePath 就是 JSON Pointer；根为 ''，对外统一成 '/'。
    path: e.instancePath === '' ? '/' : e.instancePath,
    keyword: e.keyword,
    message: e.message ?? '',
  }))
}

/**
 * 校验并返回**校验后的值**。
 *
 * 接收视图会剥离未知字段（ajv 原地改对象），所以先克隆再校验——调用方传进来的对象
 * 必须保持原样，否则「校验」变成了「静默改数据」。
 */
export function check<T>(contract: ContractName, value: unknown, opts: ValidateOptions = {}): T {
  const validate = validator(contract, opts)
  const candidate = opts.strict === true ? value : structuredClone(value)
  if (validate(candidate)) return candidate as T
  throw new ValidationError(contract, toIssues(validate.errors))
}

/** 清空缓存（测试用；生产不需要）。 */
export function resetValidators(): void {
  cache.clear()
  ajvLenient = undefined
  ajvStrict = undefined
}

/**
 * 预热：把指定契约（缺省=全部 9 类）的校验函数先编译好，供服务启动时调用。
 * 不预热也能工作——首次使用时会懒编译；预热只是把 65–90 ms 的编译成本挪到启动期。
 */
export function warmUp(contracts?: ContractName[]): void {
  const all: ContractName[] = [
    'module-manifest', 'action', 'plan', 'policy', 'grant',
    'audit-event', 'domain-event', 'frames', 'error',
  ]
  for (const contract of contracts ?? all) {
    validator(contract, {})
    validator(contract, { strict: true })
  }
}
```

- [ ] **Step 4: 跑用例确认通过**

Run: `pnpm --filter @tenon/core test`
Expected: 3 条新用例 PASS；原有的 7 条仍 PASS。

若失败在 `removeAdditional` 或 `structuredClone` 的类型上，按实际报错修，**不要**改成 `any`。

- [ ] **Step 5: Commit**

```bash
git add packages/tenon-core/src/validate
git commit -m "feat(core): 两个校验视图（接收侧剥离未知字段 / 一致性侧严格）+ 懒编译缓存"
```

---

## Task 4: 9 个契约函数与动态入口

**Files:**
- Create: `packages/tenon-core/src/validate/contracts.ts`
- Create: `packages/tenon-core/src/validate/fixtures.ts`
- Test: `packages/tenon-core/src/validate/contracts.test.ts`

> **样例从哪来**：仓库里没有按契约的合法样例（`conformance/fixtures/mini-erp.json` 是策略夹具）。所以本任务里样例**以校验器为 oracle**：先写候选，跑测试，按 `issues` 修到绿。不要凭空猜字段。

- [ ] **Step 1: 确认类型名**

Run:
```bash
grep -n "^export \(interface\|type\) " packages/tenon-core/src/types.ts | grep -E "ModuleManifest|ActionDefinition|ActionSummary|Plan \{|PolicyRule|Grant \{|DomainEvent|TenonFrame|ErrorEnvelope|AuditEvent"
```
Expected: 能看到 `ModuleManifest`、`Plan`、`PolicyRule`、`Grant`、`DomainEvent`、`TenonFrame` 等。
**若 `ErrorEnvelope` / `AuditEvent` 不存在**：在 `types.ts` 里补对应 interface（字段照 `spec/0.1/schemas/error.schema.json` / `audit-event.schema.json` 的 `required` 与 `properties`），不要用 `any` 糊过去。

- [ ] **Step 2: 写失败用例（表驱动）**

`packages/tenon-core/src/validate/contracts.test.ts`：

```ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { resetValidators } from './core.js'
import { FIXTURES } from './fixtures.js'
import { validateByContract, validateModuleManifest } from './contracts.js'
import { ValidationError, type ContractName } from './types.js'

const CONTRACTS = Object.keys(FIXTURES) as ContractName[]

test('每类契约：合法样例在两种视图下都通过', () => {
  resetValidators()
  for (const contract of CONTRACTS) {
    assert.doesNotThrow(() => validateByContract(contract, FIXTURES[contract]), contract)
    assert.doesNotThrow(() => validateByContract(contract, FIXTURES[contract], { strict: true }), contract)
  }
})

test('每类契约：类型错在任一视图下都失败，且 path 是 JSON Pointer', () => {
  resetValidators()
  for (const contract of CONTRACTS) {
    assert.throws(
      () => validateByContract(contract, []),
      (err: unknown) => {
        assert.ok(err instanceof ValidationError, contract)
        assert.equal(err.contract, contract)
        assert.match(err.issues[0]?.path ?? '', /^\//)
        return true
      },
      contract,
    )
  }
})

test('每类契约：多一个未知字段——接收视图放行并剥离，一致性视图拒绝', () => {
  resetValidators()
  for (const contract of CONTRACTS) {
    const withExtra = { ...(FIXTURES[contract] as Record<string, unknown>), 未知字段: 1 }
    const passed = validateByContract(contract, withExtra) as Record<string, unknown>
    assert.equal('未知字段' in passed, false, `${contract} 接收视图应剥离未知字段`)
    assert.throws(() => validateByContract(contract, withExtra, { strict: true }), ValidationError, contract)
  }
})

test('typed 函数与动态入口等价', () => {
  resetValidators()
  assert.deepEqual(validateModuleManifest(FIXTURES['module-manifest']), FIXTURES['module-manifest'])
})
```

- [ ] **Step 3: 跑用例确认失败**

Run: `pnpm --filter @tenon/core test`
Expected: 编译失败 `Cannot find module './contracts.js'`

- [ ] **Step 4: 写 fixtures（以校验器为 oracle）**

`packages/tenon-core/src/validate/fixtures.ts` 起步骨架（module-manifest 是本仓库已验证过的形状；其余按 schema 的 `required` 补）：

```ts
import type { ContractName } from './types.js'

/** 9 类契约的最小合法样例。字段来自各 schema 的 required；改 schema 后这些样例必须跟着改。 */
export const FIXTURES: Record<ContractName, unknown> = {
  'module-manifest': {
    tenon: '0.1',
    module: { id: 'inventory', version: '1.4.0', title: '库存' },
    actions: [],
    events: [],
    transports: ['http'],
    capabilities: ['dry-run'],
  },
  // 下面 8 类先留空对象占位，跑测试后按 issues 逐个补到绿：
  action: {},
  plan: {},
  policy: {},
  grant: {},
  'audit-event': {},
  'domain-event': {},
  frames: {},
  error: {},
}
```

然后循环执行：`pnpm --filter @tenon/core test` → 读失败用例的 `issues[0]` → 补字段 → 再跑。直到 4 条用例全绿。

**完成判据（机械可验，缺一条都不算完成）**：

```bash
grep -c '{}' packages/tenon-core/src/validate/fixtures.ts   # 期望 0（没有占位空对象）
grep -n ' as any' packages/tenon-core/src/validate/*.ts      # 期望无输出
pnpm --filter @tenon/core test                               # 期望全绿
```

不允许用 `any`、不允许注释掉用例、不允许放宽 `strict` 视图来让样例过关。样例必须**通过 schema 的 required 与 additionalProperties 双重检验**——它们将来会被 conformance 套件复用，编出来的样例等于假证据。

- [ ] **Step 5: 实现 contracts.ts**

```ts
// 对外校验门面：每类契约一个类型安全函数 + 一个按契约名分派的动态入口。
import { check } from './core.js'
import { ValidationError, type ContractName, type ValidateOptions } from './types.js'
import type {
  ActionDefinition,
  AuditEvent,
  DomainEvent,
  ErrorEnvelope,
  Grant,
  ModuleManifest,
  Plan,
  PolicyRule,
  TenonFrame,
} from '../types.js'

// ValidationError 是**值**（class），必须值导出；其余三个是类型。
export { ValidationError }
export type { ContractName, ValidateOptions, ValidationIssue } from './types.js'

export function validateModuleManifest(v: unknown, o?: ValidateOptions): ModuleManifest {
  return check<ModuleManifest>('module-manifest', v, o)
}
export function validateAction(v: unknown, o?: ValidateOptions): ActionDefinition {
  return check<ActionDefinition>('action', v, o)
}
export function validatePlan(v: unknown, o?: ValidateOptions): Plan {
  return check<Plan>('plan', v, o)
}
export function validatePolicyRule(v: unknown, o?: ValidateOptions): PolicyRule {
  return check<PolicyRule>('policy', v, o)
}
export function validateGrant(v: unknown, o?: ValidateOptions): Grant {
  return check<Grant>('grant', v, o)
}
export function validateAuditEvent(v: unknown, o?: ValidateOptions): AuditEvent {
  return check<AuditEvent>('audit-event', v, o)
}
export function validateDomainEvent(v: unknown, o?: ValidateOptions): DomainEvent {
  return check<DomainEvent>('domain-event', v, o)
}
export function validateFrame(v: unknown, o?: ValidateOptions): TenonFrame {
  return check<TenonFrame>('frames', v, o)
}
export function validateError(v: unknown, o?: ValidateOptions): ErrorEnvelope {
  return check<ErrorEnvelope>('error', v, o)
}

/** wire 上按契约名分派（frame 的 `type` 就是契约名）。 */
export function validateByContract(contract: ContractName, v: unknown, o?: ValidateOptions): unknown {
  return check(contract, v, o)
}
```

- [ ] **Step 6: 跑用例确认通过**

Run: `pnpm --filter @tenon/core test`
Expected: 4 条新用例 PASS（若 fixtures 里还有占位 `{}`，会在第 1 条用例暴露，继续补）。

- [ ] **Step 7: Commit**

```bash
git add packages/tenon-core/src/validate
git commit -m "feat(core): 9 类契约校验函数与动态入口（样例以校验器为 oracle 对齐 schema）"
```

---

## Task 5: 接入 CI、文档与收口

**Files:**
- Modify: `package.json`（根：`check` 加 schema 同步检查）
- Modify: `spec/0.1/versioning.md`（§3.2 澄清）
- Modify: `README.md`（待对齐缺口）
- Modify: `packages/tenon-core/README.md`（阶段状态）

- [ ] **Step 1: 根 `check` 加防漂移**

`package.json`：

```json
    "check": "pnpm run validate && pnpm run typecheck && pnpm run schema:check && pnpm run test",
    "schema:check": "pnpm --filter @tenon/core run sync:schemas:check",
```

- [ ] **Step 2: `versioning.md` §3.2 加澄清**

在 §3.2「未知字段必须忽略」的正文末尾追加一段：

```markdown
**与 `additionalProperties: false` 的关系**：两者不冲突，约束的是不同一方——

- `additionalProperties: false` 约束**写入方**：这个对象不允许带契约外的字段，生产方必须按封闭形状产出；
- 本节「未知字段必须忽略」约束**读取方**：收到带多余字段的报文时，读取方必须忽略而不是拒绝，否则「只增不删」的向后兼容就不成立。

因此接收侧校验应**剥离**未知字段而不是报错；一致性套件在测**自产**报文时才用严格视图。
```

- [ ] **Step 3: README 记待对齐缺口**

在 README「还没做的」段落追加一条：

```markdown
**Go host 的接收侧仍按 `additionalProperties: false` 拒绝未知字段**，与 `versioning.md` §3.2「读取方必须忽略未知字段」不一致（TS 侧已按「生产严、消费宽」实现）。修它要动 `internal/spec` 的校验入口，需单独评估对既有用例的影响。
```

- [ ] **Step 4: 更新 `packages/tenon-core/README.md`**

把 M0 的「⬜ schema 校验运行时」改为：

```markdown
- ✅ `src/validate/`：schema 校验运行时（ajv 2020-12）。同一批 schema 编两个视图——接收侧 `removeAdditional: 'all'`（剥离未知字段，符合 §3.2），一致性侧 `strict: true`（未知字段即错）。9 类契约各一个类型安全函数 + `validateByContract` 动态入口。schema 由 `scripts/sync-schemas.mjs` 从 `spec/0.1/schemas/` 同步进包内，`--check` 在 CI 里防漂移。
```

- [ ] **Step 5: 全量回归**

Run:
```bash
pnpm check
go test ./... -count=1
gofmt -l .
```
Expected: `pnpm check` 四项全过（validate / typecheck / schema:check / test）；Go 侧无 FAIL；`gofmt` 无输出。

- [ ] **Step 6: Commit**

```bash
git add package.json spec/0.1/versioning.md README.md packages/tenon-core/README.md
git commit -m "chore(core): 校验接入 CI，补 versioning 澄清与 Go 侧待对齐缺口"
```

---

## 自检对照（spec → 任务）

| spec 要求 | 落在 |
|---|---|
| §2 选型 ajv + ajv-formats | Task 1 Step 1 |
| §3 两个视图（生产严/消费宽） | Task 3 |
| §3 文档澄清（versioning §3.2） | Task 5 Step 2 |
| §3 Go 侧缺口记账 | Task 5 Step 3 |
| §4 9 个类型安全函数 + 动态入口；`common` 不导出 | Task 4 Step 5 |
| §4 返回校验后的值 | Task 3 `check()` |
| §5 错误模型（JSON Pointer、keyword 可编程） | Task 2 + Task 3 `toIssues` |
| §6 schema 打包（同步 + 哈希比对） | Task 1 |
| §7 懒编译 + 缓存 + `warmUp` 可选 | Task 3（`warmUp` 未做，属可选，故不列任务） |
| §8 验收 1–5 | Task 4 四条用例 + Task 5 Step 5 |
| §9 风险（克隆避免污染入参） | Task 3 用例「入参不被修改」 |
