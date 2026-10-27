// 校验核心：同一批 schema 编成两个视图（接收侧宽松 / 一致性侧严格），懒编译 + 缓存。
//
// 为什么是「两个视图」而不是两份 schema：`spec/0.1/schemas/*.json` 是唯一真源，
// 复制一份「宽松版 schema」就等于引入第二份真源，迟早漂移。视图只是 ajv 的配置差异。
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { Ajv2020 } from 'ajv/dist/2020.js'
import type { ValidateFunction } from 'ajv'

import { ValidationError, type ContractName, type ValidateOptions, type ValidationIssue } from './types.js'

/**
 * 全仓 schema 只用到一个 `format`：`date-time`（`common.schema.json` 的 Iso8601Timestamp）。
 * 所以不引 `ajv-formats`（55 KB + 一个 CJS 互操作窟窿），自己注册一个 RFC 3339 校验器。
 *
 * 正则与 `common.schema.json` 里 Iso8601Timestamp 的 pattern 保持同源；`Date.parse` 再兜一层
 * 「结构对但日期不存在」（如 2026-02-30）。
 */
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$/

function addFormats(ajv: Ajv2020): void {
  ajv.addFormat('date-time', (value: string) => RFC3339.test(value) && !Number.isNaN(Date.parse(value)))
}

/** 包内 `schemas/` 目录：编译后是 dist/validate/core.js，所以上两级回到包根。 */
const SCHEMA_DIR = fileURLToPath(new URL('../../schemas/', import.meta.url))

const ALL_CONTRACTS: ContractName[] = [
  'module-manifest',
  'action',
  'plan',
  'policy',
  'grant',
  'audit-event',
  'domain-event',
  'frames',
  'error',
]

/** 缓存键 `契约名:视图`；两个视图各一个 ajv 实例，互不干扰。 */
const cache = new Map<string, ValidateFunction>()

let ajvLenient: Ajv2020 | undefined
let ajvStrict: Ajv2020 | undefined

function build(strict: boolean): Ajv2020 {
  const ajv = new Ajv2020({
    allErrors: true,
    // 不开 ajv 的 schema lint（strict）：它检查的是「schema 该怎么写」，不是「实例合不合法」。
    // 本仓库的 schema 由 Go 校验器（santhosh-tekuri v6）与 scripts/validate-spec.mjs 把关；
    // 在编译期开 lint 会拒掉合法写法——实测三处误伤：
    //   action#/allOf/0/then 的 required 不在本分支 properties（strictRequired）
    //   action#/properties/inputSchema 的 union type（strictTypes）
    //   audit-event#/allOf/0/if/properties/decision 缺 type: object（strictTypes）
    // 所以严格性只体现在**实例**上，靠下面的 removeAdditional 区分两个视图。
    strict: false,
    // 两个视图的唯一差别：接收侧剥离未知字段（versioning.md §3.2 未知字段必须忽略）；
    // 一致性侧保留未知字段，好让 additionalProperties 报错。
    removeAdditional: strict ? false : 'all',
  })
  addFormats(ajv)

  // 按目录通配而不是硬编码文件名：新增一份 schema 不需要改这里。
  for (const file of readdirSync(SCHEMA_DIR).filter((f) => f.endsWith('.schema.json'))) {
    const schema = JSON.parse(readFileSync(join(SCHEMA_DIR, file), 'utf8')) as { $id: string }
    ajv.addSchema(schema, schema.$id)
  }
  return ajv
}

function validator(contract: ContractName, opts: ValidateOptions): ValidateFunction {
  const strict = opts.strict === true
  const key = `${contract}:${strict ? 'strict' : 'lenient'}`
  const cached = cache.get(key)
  if (cached !== undefined) return cached

  ajvLenient ??= build(false)
  ajvStrict ??= build(true)
  const ajv = strict ? ajvStrict : ajvLenient
  const id = `https://tenon.dev/spec/0.1/schemas/${contract}.schema.json`
  const compiled = ajv.getSchema(id)
  if (compiled === undefined) {
    throw new Error(`schema 未注册：${id}（检查 packages/tenon-core/schemas/ 是否已同步）`)
  }
  cache.set(key, compiled)
  return compiled
}

function toIssues(errors: ValidateFunction['errors']): ValidationIssue[] {
  return (errors ?? []).map((e) => ({
    // ajv 的 instancePath 本身就是 JSON Pointer；根级为空串，对外统一成 '/'。
    path: e.instancePath === '' ? '/' : e.instancePath,
    keyword: e.keyword,
    message: e.message ?? '',
  }))
}

/**
 * 校验并返回**校验后的值**。
 *
 * 接收视图用 ajv 的 `removeAdditional` 剥离未知字段，而它是**原地修改**对象的——
 * 所以先 `structuredClone` 再校验。调用方传进来的对象必须保持原样，
 * 否则「校验」就成了「静默改数据」，这种副作用在业务代码里极难排查。
 */
export function check<T>(contract: ContractName, value: unknown, opts: ValidateOptions = {}): T {
  const validate = validator(contract, opts)
  const candidate = opts.strict === true ? value : structuredClone(value)
  if (validate(candidate)) return candidate as T
  throw new ValidationError(contract, toIssues(validate.errors))
}

/**
 * 预热：把指定契约（缺省 = 全部 9 类）的两个视图都编译好。
 * 不预热也能工作（首次使用时懒编译）；预热只是把 65–90 ms 的编译成本挪到启动期。
 */
export function warmUp(contracts?: ContractName[]): void {
  for (const contract of contracts ?? ALL_CONTRACTS) {
    validator(contract, {})
    validator(contract, { strict: true })
  }
}

/** 清空缓存（测试专用；生产代码不该调用）。 */
export function resetValidators(): void {
  cache.clear()
  ajvLenient = undefined
  ajvStrict = undefined
}
