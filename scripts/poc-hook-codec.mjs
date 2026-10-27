// POC 探针：直接调用 DSH 自带的 parseHookOutput / mergeHookOutputs，测「什么才拦得住工具」。
//
// 用法：node scripts/poc-hook-codec.mjs [dsh-hook-protocol 的 lib/index.js 路径]
// 依赖：本机已安装 DSH（默认路径见 DEFAULT_ENTRY）。这是只读探针，不改任何状态。
//
// 为什么直接调库函数而不是起会话：这两个函数就是网关的全部判定逻辑
// （dsh-hooks-claude-code 的 lib/index.js 调用它们）。证据层级见
// docs/designs/dsh-hook-recon.md §5：源码 + 库函数实测；未做 live 会话验证。
import { pathToFileURL } from 'node:url';

const DEFAULT_ENTRY =
  'E:/npm-global/node_modules/@deepseek-ai/dsh/node_modules/@deepseek-ai/dsh-hook-protocol/lib/index.js';

const entry = process.argv[2] || DEFAULT_ENTRY;

let parseHookOutput;
let mergeHookOutputs;
try {
  ({ parseHookOutput, mergeHookOutputs } = await import(pathToFileURL(entry).href));
} catch (err) {
  console.error(`加载 hook-protocol 失败：${entry}`);
  console.error(String(err));
  console.error('提示：把 DSH 安装目录下 dsh-hook-protocol/lib/index.js 的路径作为第一个参数传进来。');
  process.exit(2);
}

const cases = [
  { name: '正常退出、无输出', exit: 0, out: '', err: '' },
  { name: '正常退出 + JSON allow', exit: 0, out: '{"decision":"allow"}', err: '' },
  { name: '正常退出 + JSON deny（方言不认）', exit: 0, out: '{"decision":"deny","reason":"x"}', err: '' },
  { name: '显式阻断（协议约定）', exit: 2, out: '', err: 'tenon: 审计写不进去，拒绝执行' },
  { name: 'JSON decision=block', exit: 0, out: '{"decision":"block","reason":"tenon 拒绝"}', err: '' },
  {
    name: 'JSON hookSpecificOutput.permissionDecision=deny',
    exit: 0,
    out: '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"tenon 拒绝"}}',
    err: '',
  },
  { name: 'JSON continue=false（停机）', exit: 0, out: '{"continue":false,"stopReason":"tenon 停机"}', err: '' },
  { name: '闸门脚本异常（exit 1）', exit: 1, out: '', err: 'throw: 意外错误' },
  { name: '闸门被杀（超时 exit 124）', exit: 124, out: '', err: '' },
  { name: '闸门根本没起来（spawn 失败）', exit: undefined, out: '', err: 'ENOENT' },
];

const rows = [];
let blockedCount = 0;

for (const c of cases) {
  const out = parseHookOutput(c.exit, c.out, c.err, 'PreToolUse');
  const merged = mergeHookOutputs([out]);
  // 关键判读：合并结论是 deny/ask 才算「拦住」；none / allow 都等于放行。
  const blocked = merged.decision === 'deny' || merged.decision === 'ask';
  if (blocked) blockedCount++;
  rows.push({
    场景: c.name,
    exit: c.exit === undefined ? '(undefined)' : c.exit,
    单钩子决策: out.decision ?? '(无)',
    合并决策: merged.decision,
    理由: merged.reason ?? '(无)',
    工具会被拦下: blocked ? '是' : '否 ← 放行',
  });
}

console.table(rows);
console.log('');
console.log(`结论：${rows.length} 个场景里只有 ${blockedCount} 个被拦下（exit 2 / JSON block / JSON deny）。`);
console.log(`其余 ${rows.length - blockedCount} 个（崩溃、超时、spawn 失败、方言不认的 JSON）一律放行 ⇒ 闸门故障时 fail-open。`);
process.exit(0);
