import assert from 'node:assert/strict'
import { test } from 'node:test'

import { check, resetValidators, warmUp } from './core.js'
import { ValidationError } from './types.js'

const BASE = {
  tenon: '0.1',
  module: { id: 'inventory', version: '1.4.0', title: '库存' },
  actions: [],
  events: [],
  transports: ['http'],
  capabilities: ['dry-run'],
}

test('接收视图：未知字段被剥离且不报错，入参不被修改', () => {
  resetValidators()
  const input: Record<string, unknown> = { ...BASE, 未在契约里的字段: 1 }

  const out = check<Record<string, unknown>>('module-manifest', input)

  assert.equal('未在契约里的字段' in out, false, '返回值里不该有未知字段')
  assert.equal(input['未在契约里的字段'], 1, '入参必须保持原样（removeAdditional 会原地改对象，所以要先克隆）')
})

test('一致性视图：未知字段就是错误，根级 path 归一为 /', () => {
  resetValidators()
  let caught: unknown
  try {
    check('module-manifest', { ...BASE, 未在契约里的字段: 1 }, { strict: true })
  } catch (err) {
    caught = err
  }

  assert.ok(caught instanceof ValidationError, '必须抛 ValidationError')
  assert.equal(caught.contract, 'module-manifest')
  const issue = caught.issues.find((i) => i.keyword === 'additionalProperties')
  assert.ok(issue, `issues 里应有 additionalProperties：${JSON.stringify(caught.issues)}`)
  assert.equal(issue.path, '/')
})

test('类型错：path 指向出错字段（JSON Pointer）', () => {
  resetValidators()
  assert.throws(
    () => check('module-manifest', { ...BASE, actions: 'oops' }),
    (err: unknown) => {
      assert.ok(err instanceof ValidationError)
      const issue = err.issues.find((i) => i.keyword === 'type')
      assert.ok(issue, `issues 里应有 type：${JSON.stringify(err.issues)}`)
      assert.equal(issue.path, '/actions')
      return true
    },
  )
})

test('warmUp：9 类契约 × 两个视图都能编译（不抛出）', () => {
  resetValidators()
  assert.doesNotThrow(() => warmUp())
})
