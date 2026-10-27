// 用 Edge 的 CDP 做一次真浏览器验证（零依赖：Node 24 自带 WebSocket）。
//
// 断言的是「折叠是否真的生效」——这些结论读代码只能猜，必须让浏览器把 DOM 渲染出来：
//   1. 卡面可见文本里有业务人话（扣减 … 库存 12 件），
//   2. 卡面可见文本里**没有** action id / planId（R5），
//   3. <details class="tech"> 存在且默认收起（open=false），
//   4. 展开后技术原文里才有 action id。
// 顺带截一张图，人工复核构图。
//
// 用法：node out/cdp-check.mjs <baseUrl> <debugPort> <token> <screenshotPath> [user] [rolesCsv] [expectApprove]
// roles 用逗号分隔而不是 JSON：Windows 的 PowerShell 把参数交给原生程序时会剥掉双引号（实测）。
import fs from 'node:fs';

const [
  baseUrl = 'http://127.0.0.1:8080',
  debugPort = '9222',
  token = '',
  shotPath = 'out/ui-card.png',
  user = 'bob',
  rolesCsv = 'role:warehouse_manager',
  expectApprove = '1',
] = process.argv.slice(2);

if (!token) {
  console.error('缺少 token');
  process.exit(2);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function targets() {
  const res = await fetch(`http://127.0.0.1:${debugPort}/json/list`);
  return res.json();
}

// 等 Edge 把页面 target 挂出来（启动有几百毫秒的空窗）。
let page = null;
for (let i = 0; i < 40; i++) {
  try {
    const list = await targets();
    page = list.find((t) => t.type === 'page' && t.webSocketDebuggerUrl);
    if (page) break;
  } catch (_) {
    /* 还没起来 */
  }
  await sleep(250);
}
if (!page) {
  console.error('找不到 Edge 的 page target（CDP 没起来）');
  process.exit(2);
}

const ws = new WebSocket(page.webSocketDebuggerUrl);
const pending = new Map();
let seq = 0;

ws.addEventListener('message', (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id && pending.has(msg.id)) {
    pending.get(msg.id)(msg);
    pending.delete(msg.id);
  }
});
await new Promise((resolve, reject) => {
  ws.addEventListener('open', resolve, { once: true });
  ws.addEventListener('error', reject, { once: true });
});

function send(method, params = {}) {
  return new Promise((resolve) => {
    const id = ++seq;
    pending.set(id, resolve);
    ws.send(JSON.stringify({ id, method, params }));
  });
}

await send('Page.enable');
await send('Runtime.enable');

// 先落在同源页面上，才能写 sessionStorage。
await send('Page.navigate', { url: `${baseUrl}/` });
await sleep(1200);

const session = JSON.stringify({
  token,
  user,
  roles: rolesCsv.split(',').map((s) => s.trim()).filter(Boolean),
  exp: Math.floor(Date.now() / 1000) + 3600,
});
const setExpr = `sessionStorage.setItem('tenon.ui.session.v1', ${JSON.stringify(session)}); 'ok'`;
const setRes = await send('Runtime.evaluate', { expression: setExpr, returnByValue: true });
if (setRes.result?.exceptionDetails) {
  console.error('写 sessionStorage 失败：', JSON.stringify(setRes.result.exceptionDetails));
  process.exit(2);
}

// reload 后启动路径会 loadSession() → refreshBoard()，卡面才会渲染出来。
await send('Page.reload', {});
await sleep(2500);

// 固定视口 + 把待批卡滚到视野中心：截图要能说明问题，不能只截到卡片的一半。
await send('Emulation.setDeviceMetricsOverride', { width: 1200, height: 900, deviceScaleFactor: 1, mobile: false });
await send('Runtime.evaluate', {
  expression: `(() => { const c = document.querySelector('article.card'); if (c) c.scrollIntoView({ block: 'center' }); return 'ok'; })()`,
  returnByValue: true,
});
await sleep(400);

const inspectExpr = `(() => {
  const tech = document.querySelector('details.tech');
  const cards = document.querySelectorAll('article.card');
  return JSON.stringify({
    cardCount: cards.length,
    techCount: document.querySelectorAll('details.tech').length,
    techOpen: tech ? tech.open : null,
    approveButtons: document.querySelectorAll('button[data-act="approve"]').length,
    visibleText: document.body.innerText
  });
})()`;
const inspect = await send('Runtime.evaluate', { expression: inspectExpr, returnByValue: true });
const data = JSON.parse(inspect.result.result.value);

const visible = data.visibleText || '';
const wantApprove = expectApprove === '1';
const checks = {
  hasCard: data.cardCount >= 1,
  hasPlain: visible.includes('扣减') && visible.includes('库存') && visible.includes('STD-1001'),
  noActionIdVisible: !visible.includes('DeductStock') && !visible.includes('inventory.'),
  noPlanIdVisible: !visible.includes('pln_'),
  detailCollapsed: data.techOpen === false,
  // 审批人看到按钮；非审批人（clerk）只能用文字被告知「需要什么角色」，不该有可点的批准。
  approveMatchesRole: wantApprove ? data.approveButtons >= 1 : data.approveButtons === 0,
};

const shot = await send('Page.captureScreenshot', { format: 'png' });
if (shot.result?.data) {
  fs.writeFileSync(shotPath, Buffer.from(shot.result.data, 'base64'));
}

// 展开一次，证明「点了之后技术信息才出现」——这是 R5 的另一半。
// 注意：收起状态的 <details> 里 innerText 本来就取不到内容，必须在展开后再读一次。
const openRes = await send('Runtime.evaluate', {
  expression: `(() => { const d = document.querySelector('details.tech'); if (!d) return 'no-details'; d.open = true; return d.open ? 'opened' : 'failed'; })()`,
  returnByValue: true,
});
const opened = openRes.result.result.value === 'opened';
const foldRes = await send('Runtime.evaluate', {
  expression: `(() => { const d = document.querySelector('details.tech'); return d ? d.innerText : ''; })()`,
  returnByValue: true,
});
const fold = foldRes.result.result.value || '';
checks.foldHasActionId = fold.includes('DeductStock') && fold.includes('inventory.DeductStock（');
const shot2 = await send('Page.captureScreenshot', { format: 'png' });
if (shot2.result?.data) {
  fs.writeFileSync(shotPath.replace(/\.png$/, '-open.png'), Buffer.from(shot2.result.data, 'base64'));
}

const ok = Object.values(checks).every(Boolean) && opened;
console.log(
  JSON.stringify(
    {
      ok,
      opened,
      checks,
      screenshot: shotPath,
      visibleSnippet: visible.replace(/\s+/g, ' ').slice(0, 300),
      foldSnippet: fold.replace(/\s+/g, ' ').slice(0, 200),
    },
    null,
    2
  )
);
process.exit(ok ? 0 : 1);
