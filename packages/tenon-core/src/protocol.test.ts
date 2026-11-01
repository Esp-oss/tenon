// 交叉校验测试：这些期望值**不是手算的**，是用 Go 参考实现跑出来的
// （`protocol.Sign` / `IdempotencyKey` / `EntityRef.String`）。
// 两个实现必须字节一致，否则同一份数据在两侧一边合法一边非法。
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { EntityRefError, formatEntityRef, parseEntityRef, toEntityRef } from './entity-ref.js';
import { idempotencyKey } from './idempotency.js';
import { sha256Digest, sha256Hex } from './hash.js';
import { SIGNATURE_TOLERANCE_SECONDS, SignatureError, sign, verify } from './hmac.js';

// 由 Go 侧生成：
//   protocol.Sign([]byte("s3cr3t"), 1700000000, []byte(`{"a":1}`))
const GO_SIGNATURE = 't=1700000000,v1=8dbbbbf4523b10bbb793e74d854144c45acccc2d233667b1c06b805b6ded8a84';

test('实体寻址：字符串简写与对象形式互转（含 Go 向量）', () => {
  assert.equal(formatEntityRef({ module: 'inventory', type: 'SKU', id: 'STD-1001' }), 'inventory:SKU/STD-1001');
  assert.deepEqual(parseEntityRef('inventory:SKU/STD-1001'), {
    module: 'inventory',
    type: 'SKU',
    id: 'STD-1001',
  });
  // id 里允许出现 `:` 与 `/` 之外的一切字符；出现 `:` 时按「第一个冒号 + 其后第一个斜杠」切。
  assert.deepEqual(parseEntityRef('inventory:SKU/A:B'), { module: 'inventory', type: 'SKU', id: 'A:B' });
});

test('实体寻址：三类非法输入与 Go 判据一致', () => {
  for (const bad of ['inventory/SKU', ':SKU/1', 'inventory:/1', 'inventory:SKU/']) {
    assert.throws(() => parseEntityRef(bad), EntityRefError, `应拒绝 ${bad}`);
  }
  // 错误消息要说明缺的是哪一段，否则调用方只能猜。
  assert.match(String(catchError(() => parseEntityRef('inventory/SKU'))), /缺少 module 前缀/);
  assert.match(String(catchError(() => parseEntityRef('inventory:/1'))), /缺少 Type\/id 段/);
  assert.match(String(catchError(() => parseEntityRef('inventory:SKU/'))), /id 为空/);
});

test('toEntityRef：对象形式也做非空检查，字符串直接解析', () => {
  assert.deepEqual(toEntityRef('inventory:SKU/STD-1001'), { module: 'inventory', type: 'SKU', id: 'STD-1001' });
  assert.deepEqual(toEntityRef({ module: 'quote', type: 'Quote', id: 'Q-1' }), {
    module: 'quote',
    type: 'Quote',
    id: 'Q-1',
  });
  assert.throws(() => toEntityRef({ module: 'quote', type: '', id: 'Q-1' }), EntityRefError);
});

test('幂等键（Go 向量）', () => {
  assert.equal(idempotencyKey('pln_1', 's1'), 'tenon:pln_1/s1');
});

test('sha256 摘要：已知向量 + 规范前缀', () => {
  assert.equal(sha256Hex('abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
  assert.equal(sha256Digest('abc'), 'sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
});

test('HMAC 签名：与 Go 逐字节一致', () => {
  const body = '{"a":1}';
  assert.equal(sign('s3cr3t', 1700000000, body), GO_SIGNATURE);
});

test('HMAC 验签：通过、超时、篡改、格式错各一条', () => {
  const secret = 's3cr3t';
  const body = '{"a":1}';
  const header = sign(secret, 1700000000, body);

  // 容差边界：±300s 之内通过（含端点）。
  verify(secret, header, body, 1700000000);
  verify(secret, header, body, 1700000000 + SIGNATURE_TOLERANCE_SECONDS);
  verify(secret, header, body, 1700000000 - SIGNATURE_TOLERANCE_SECONDS);

  // 超出容差 1 秒即拒（防重放）。
  assert.equal(errorKind(() => verify(secret, header, body, 1700000000 + SIGNATURE_TOLERANCE_SECONDS + 1)), 'expired');

  // 改了 body 必须验不过——这是签名的全部意义。
  assert.equal(errorKind(() => verify(secret, header, '{"a":2}', 1700000000)), 'invalid');

  // 格式错：空头、缺段、非十六进制、t 不是整数。
  assert.equal(errorKind(() => verify(secret, '', body, 1700000000)), 'format');
  assert.equal(errorKind(() => verify(secret, 't=1700000000', body, 1700000000)), 'format');
  assert.equal(errorKind(() => verify(secret, 't=1700000000,v1=zz', body, 1700000000)), 'format');
  assert.equal(errorKind(() => verify(secret, 't=abc,v1=00', body, 1700000000)), 'format');
  assert.equal(errorKind(() => verify(secret, 'garbage', body, 1700000000)), 'format');
});

function catchError(fn: () => unknown): unknown {
  try {
    fn();
  } catch (err) {
    return err;
  }
  return undefined;
}

function errorKind(fn: () => unknown): string | undefined {
  const err = catchError(fn);
  return err instanceof SignatureError ? err.kind : undefined;
}
