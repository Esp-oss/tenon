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
  // message 里带上首条问题，排障时不用再去翻 issues 数组。
  assert.match(err.message, /must be object/)
})

test('零 issues 也不会崩（例如上层手工抛出的场景）', () => {
  const err = new ValidationError('frames', [])
  assert.equal(err.issues.length, 0)
  assert.match(err.message, /0 处/)
})
