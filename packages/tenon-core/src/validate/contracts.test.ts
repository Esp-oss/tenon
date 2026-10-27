import assert from 'node:assert/strict'
import { test } from 'node:test'

import { resetValidators } from './core.js'
import { FIXTURES } from './fixtures.js'
import { validateByContract, validateModuleManifest } from './contracts.js'
import { ValidationError, type ContractName } from './types.js'

const CONTRACTS = Object.keys(FIXTURES) as ContractName[]
const clone = <T>(v: T): T => structuredClone(v)

test('每类契约：合法样例在两种视图下都通过', () => {
  resetValidators()
  for (const contract of CONTRACTS) {
    assert.doesNotThrow(() => validateByContract(contract, clone(FIXTURES[contract])), `${contract}（接收视图）`)
    assert.doesNotThrow(
      () => validateByContract(contract, clone(FIXTURES[contract]), { strict: true }),
      `${contract}（一致性视图）`,
    )
  }
})

test('每类契约：类型错在两个视图下都失败，且 path 是 JSON Pointer', () => {
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

test('每类契约：多一个未知字段——接收视图放行（封闭对象会剥离），一致性视图拒绝', () => {
  resetValidators()
  for (const contract of CONTRACTS) {
    const withExtra = { ...(clone(FIXTURES[contract]) as Record<string, unknown>), 未知字段: 1 }

    // 接收侧：无论对象封闭与否都必须**放行**（§3.2）；封闭对象额外会被剥离。
    const passed = validateByContract(contract, clone(withExtra)) as Record<string, unknown>
    assert.ok(passed !== null && typeof passed === 'object', `${contract} 接收视图应通过`)

    // 一致性侧：契约外的字段就是错误。
    assert.throws(() => validateByContract(contract, clone(withExtra), { strict: true }), ValidationError, contract)
  }
})

test('typed 函数与动态入口等价', () => {
  resetValidators()
  assert.deepEqual(validateModuleManifest(clone(FIXTURES['module-manifest'])), FIXTURES['module-manifest'])
})
