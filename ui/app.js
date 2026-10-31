'use strict';

/*
 * Tenon 待批看板（无构建链的纯静态页，**只读**）。
 *
 * 对话入口只有一个：dsh 容器（127.0.0.1:3080）。本页不发任何请求帧，
 * 只镜像宿主的只读状态——多入口最典型的故障是「同一个决定被点两次」，
 * 而只要这里没有写路径，它就不可能发生。
 *
 * 协议形状以宿主代码为准，这里不猜：
 *   POST /tenon/session   {user, password}                → {token, user, roles, exp}
 *   GET  /tenon/pending   Bearer token                     → {items:[{planId, requestId,
 *                                                            by, summary, items:[{label}],
 *                                                            requiredRoles, expiresAt, mine, state}]}
 *   GET  /tenon/intents/poll?after=                        → {frames:[{seq,frame}], nextCursor}
 *
 * 三条不能松的约定：
 *   1. 未知 state 一律当待批（fail-closed），绝不显示成已完成；
 *   2. 卡片一律不可操作（没有批准/驳回按钮）：批准只走 dsh，本页只显示「在 dsh 里怎么说」；
 *   3. 错误必须显示在页面上（tenonError.code/message 或原始响应体），不能只进 console。
 */

const POLL_MS = 2000;

// intentCursor 是 /tenon/intents/poll 的游标：持久化到 sessionStorage，
// 刷新页面不会把已经看过的结论再放一遍。
let intentCursor = Number(sessionStorage.getItem('tenon.intentCursor') || 0);
const SESSION_KEY = 'tenon.ui.session.v1';

// 终态集合来自协议。不在这里的 state（含空串、future 名字）一律按待批处理。
const TERMINAL_STATES = ['done', 'rejected', 'expired', 'stale', 'aborted'];
const TERMINAL = new Set(TERMINAL_STATES);

const STATE_LABELS = {
  awaiting_approval: '待批',
  done: '已完成',
  rejected: '已驳回',
  expired: '已过期',
  stale: '已失效',
  aborted: '已中断',
};

// ---------- 小工具 ----------

const $ = (id) => document.getElementById(id);

/** 建 DOM 节点：数据一律走 textContent，不拼 innerHTML（summary 来自 agent，不能当 HTML）。 */
function h(tag, props, ...kids) {
  const node = document.createElement(tag);
  if (props) {
    for (const key of Object.keys(props)) {
      const value = props[key];
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') node.className = value;
      else if (key === 'text') node.textContent = String(value);
      else if (key === 'dataset') Object.assign(node.dataset, value);
      else node.setAttribute(key, value === true ? '' : String(value));
    }
  }
  for (const kid of kids) {
    if (kid === undefined || kid === null || kid === false) continue;
    node.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return node;
}

function shorten(text, limit) {
  const s = String(text);
  return s.length <= limit ? s : s.slice(0, limit) + '…';
}

function durationText(ms) {
  const total = Math.ceil(ms / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours) return hours + ' 小时 ' + minutes + ' 分';
  if (minutes) return minutes + ' 分 ' + seconds + ' 秒';
  return seconds + ' 秒';
}

function clockText(date) {
  const pad = (n) => String(n).padStart(2, '0');
  return pad(date.getHours()) + ':' + pad(date.getMinutes()) + ':' + pad(date.getSeconds());
}

class HttpError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'HttpError';
    this.status = status || 0;
  }
}

// ---------- 状态 ----------

const state = {
  token: '',
  user: '',
  roles: [],
  exp: 0, // 秒（宿主签发的时间戳）
  items: [], // 最近一次 GET /tenon/pending 的 items
  // intents 是 agent 的结局帧（内存态，按游标增量拉）。它比公告板全：没有待批卡的
  // 回合（被闸门拒、auto 直接执行）也会有一条，界面上才不会「agent 一声不响」。
  // 这里**只读回放**：帧是 agent 说完的话，不是可点的操作。
  intents: [],
  pollTimer: null,
  refreshing: false,
  errorFromPoll: false,
};

// ---------- 提示条 ----------

function showError(message) {
  $('error-text').textContent = String(message);
  $('error-bar').hidden = false;
  // 页面上要看得见，同时留一份在 console 便于排查。
  console.error('[tenon]', message);
}

function clearError() {
  $('error-bar').hidden = true;
  $('error-text').textContent = '';
  state.errorFromPoll = false;
}

function setStatus(message) {
  $('status-line').textContent = message || '';
}

// ---------- HTTP ----------

function describeHttpError(status, data, raw) {
  const te = data && typeof data === 'object' ? data.tenonError : null;
  if (te && typeof te === 'object') {
    const parts = ['HTTP ' + status];
    parts.push((te.code || '未知错误码') + '：' + (te.message || '（无 message）'));
    if (te.class) parts.push('分类 ' + te.class);
    if (te.retryable) parts.push('可重试');
    if (te.details) parts.push('详情 ' + JSON.stringify(te.details));
    return parts.join(' · ');
  }
  if (raw && raw.trim()) return 'HTTP ' + status + '：' + shorten(raw.trim(), 400);
  return 'HTTP ' + status + '（空响应体）';
}

async function api(method, path, payload, withAuth = true) {
  const headers = {};
  if (payload !== undefined) headers['Content-Type'] = 'application/json';
  if (withAuth) {
    if (!state.token) throw new HttpError('尚未登录：请先登录再操作', 401);
    headers['Authorization'] = 'Bearer ' + state.token;
  }

  let res;
  try {
    res = await fetch(path, {
      method,
      headers,
      cache: 'no-store',
      body: payload === undefined ? undefined : JSON.stringify(payload),
    });
  } catch (err) {
    throw new HttpError(
      '网络请求失败（' + method + ' ' + path + '）：' + (err && err.message ? err.message : String(err)) +
        ' —— 宿主起了吗？页面与接口必须同源。',
      0
    );
  }

  let raw = '';
  try {
    raw = await res.text();
  } catch (_) {
    raw = '';
  }

  let data = null;
  if (raw.trim() !== '') {
    try {
      data = JSON.parse(raw);
    } catch (_) {
      data = null;
    }
  }

  if (!res.ok) throw new HttpError(describeHttpError(res.status, data, raw), res.status);
  if (data === null) throw new HttpError('响应不是 JSON（HTTP ' + res.status + '）：' + shorten(raw, 300), res.status);
  return data;
}

// ---------- 会话 ----------

function applySession(data, fallbackUser) {
  state.token = typeof data.token === 'string' ? data.token : '';
  state.user = typeof data.user === 'string' && data.user ? data.user : fallbackUser || '';
  state.roles = Array.isArray(data.roles) ? data.roles.map(String) : [];
  state.exp = Number(data.exp) || 0;
}

function sessionValid() {
  if (!state.token) return false;
  if (!state.exp) return true; // 没给 exp 就交给服务端 401 判定
  return state.exp * 1000 > Date.now();
}

function saveSession() {
  try {
    sessionStorage.setItem(
      SESSION_KEY,
      JSON.stringify({ token: state.token, user: state.user, roles: state.roles, exp: state.exp })
    );
  } catch (_) {
    /* 隐私模式禁用 storage 时退化成「刷新即重登」，不影响功能 */
  }
}

function loadSession() {
  try {
    const raw = sessionStorage.getItem(SESSION_KEY);
    if (!raw) return false;
    const data = JSON.parse(raw);
    if (!data || typeof data !== 'object') return false;
    applySession(data, '');
    return sessionValid();
  } catch (_) {
    return false;
  }
}

function dropSession() {
  state.token = '';
  state.user = '';
  state.roles = [];
  state.exp = 0;
  state.items = [];
  try {
    sessionStorage.removeItem(SESSION_KEY);
  } catch (_) {
    /* 同上 */
  }
}

function renderRoles(container, roles) {
  container.textContent = '';
  if (!roles.length) {
    container.append('（无角色）');
    return;
  }
  container.append('角色 ');
  roles.forEach((role, i) => {
    if (i) container.append(' ');
    container.append(h('span', { class: 'role', text: role }));
  });
}

function renderSession() {
  const logged = !!state.token;
  $('login-form').hidden = logged;
  $('session-info').hidden = !logged;
  // 本页没有输入框可禁用：说话与批准的入口都在 dsh，这里的登录只用来读公告板
  // （GET /tenon/pending 要 Bearer token）。
  if (logged) {
    $('session-user').textContent = state.user || '（未知用户）';
    renderRoles($('session-roles'), state.roles);
  } else {
    $('session-user').textContent = '—';
    $('session-roles').textContent = '';
  }
}

async function onLogin(ev) {
  ev.preventDefault();
  clearError();
  setStatus('');

  const user = $('login-user').value.trim();
  const password = $('login-password').value;
  if (!user || !password) {
    showError('用户名与口令都不能为空。开发账号见 config/users.dev.json（如 alice / bob）。');
    return;
  }

  $('login-submit').disabled = true;
  try {
    const data = await api('POST', '/tenon/session', { user, password }, false);
    if (!data || typeof data.token !== 'string' || !data.token) {
      throw new HttpError('登录响应里没有 token：' + shorten(JSON.stringify(data), 200), 0);
    }
    applySession(data, user);
    saveSession();
    $('login-password').value = '';
    renderSession();
    startPolling();
    await refreshBoard({ manual: true });
    setStatus('已登录：' + state.user + '（看板每 2 秒刷新）');
  } catch (err) {
    showError(err && err.message ? err.message : String(err));
  } finally {
    $('login-submit').disabled = false;
  }
}

function onLogout() {
  stopPolling();
  dropSession();
  renderSession();
  renderBoard();
  clearError();
  setStatus('已退出。');
}

// ---------- 看板 ----------

function stateOf(item) {
  const raw = item && typeof item.state === 'string' ? item.state.trim() : '';
  // 缺 state 视为待批：协议层缺省就是 awaiting_approval，空串既不是待批也不是终态。
  return raw || 'awaiting_approval';
}

function isPending(item) {
  // 不在终态集合里的一律当待批（fail-closed）。
  return !TERMINAL.has(stateOf(item));
}

// canDecide 只用来**提示**这张卡你是不是那个能批的人；本页没有批准按钮，
// 真正的角色判定在 pending_decide（MCP）与宿主那一侧。
function canDecide(item) {
  const need = Array.isArray(item.requiredRoles) ? item.requiredRoles : [];
  if (!need.length) return false; // 没指定审批角色 ⇒ 没人能认领
  return need.some((role) => state.roles.includes(role));
}

function badgeClass(st) {
  if (st === 'awaiting_approval') return 'badge-pending';
  if (st === 'done') return 'badge-done';
  if (st === 'rejected' || st === 'aborted') return 'badge-bad';
  if (st === 'expired' || st === 'stale') return 'badge-warn';
  return 'badge-unknown';
}

function itemLabels(item) {
  if (!Array.isArray(item.items)) return [];
  return item.items
    .map((entry) => {
      if (entry && typeof entry === 'object' && typeof entry.label === 'string') return entry.label;
      if (typeof entry === 'string') return entry;
      return '';
    })
    .filter((label) => label.trim() !== '');
}

function renderCard(item, pending) {
  const st = stateOf(item);
  const card = h('article', {
    class: 'card' + (pending ? '' : ' card-terminal'),
    dataset: { planId: String(item.planId || '') },
  });

  const head = h(
    'div',
    { class: 'card-head' },
    h('h3', { class: 'summary', text: item.summary || '（summary 为空）' }),
    h('span', { class: 'badge ' + badgeClass(st), text: STATE_LABELS[st] || st })
  );
  if (!STATE_LABELS[st]) {
    head.append(
      h('span', {
        class: 'badge badge-unknown',
        title: '协议未定义这个 state；按待批处理，不显示成已完成',
        text: '按待批处理',
      })
    );
  }
  card.append(head);

  const need = Array.isArray(item.requiredRoles) ? item.requiredRoles.filter((r) => typeof r === 'string' && r) : [];
  const needLine = h('p', { class: 'meta' }, '需要角色：');
  if (need.length) {
    need.forEach((role, i) => {
      if (i) needLine.append(' ');
      needLine.append(h('span', { class: 'role', text: role }));
    });
  } else {
    needLine.append(h('span', { class: 'muted' }, '（未指定审批角色）'));
  }
  card.append(needLine);

  const expiryLine = h('p', { class: 'meta' }, '到期：');
  expiryLine.append(
    h('span', {
      class: 'expiry',
      dataset: { exp: typeof item.expiresAt === 'string' ? item.expiresAt : '' },
      text: '计算中…',
    })
  );
  if (item.expiresAt) expiryLine.append(' ', h('code', { text: item.expiresAt }));
  card.append(expiryLine);

  // 技术详情（R5）：planId / requestId / 发起人 / 每步的 action（args）只在这里出现。
  // 用原生 <details>：零 JS 状态、零构建链，默认收起，卡面只剩一句人话。
  const labels = itemLabels(item);
  const tech = h('details', { class: 'tech' });
  tech.append(h('summary', {}, '详情 ▾'));
  const techList = h('ul', { class: 'tech-list' });
  if (item.planId) techList.append(h('li', {}, h('span', { text: 'planId ' }), h('code', { text: item.planId })));
  if (item.requestId) techList.append(h('li', {}, h('span', { text: 'requestId ' }), h('code', { text: item.requestId })));
  if (item.by) techList.append(h('li', {}, h('span', { text: '发起人 ' }), h('code', { text: item.by })));
  labels.forEach((label, i) => {
    const entry = Array.isArray(item.items) ? item.items[i] : null;
    // 老 agent 不推 detail：退回这条 item 自身的 JSON，至少让详情区不是空的。
    const detail = entry && typeof entry.detail === 'string' && entry.detail.trim() !== ''
      ? entry.detail
      : JSON.stringify(entry || { label });
    techList.append(h('li', {}, h('code', { text: detail })));
  });
  if (!techList.childNodes.length) {
    techList.append(h('li', { class: 'muted', text: '（没有技术明细）' }));
  }
  tech.append(techList);
  card.append(tech);

  if (pending) {
    // 只看不点：批准的唯一路径是 dsh 里的选项卡片 → pending_decide → 宿主签票。
    // 这里若再放一对按钮，同一份计划就有了第二个写入口，审计里也分不清是谁点的。
    const actions = h('div', { class: 'card-actions' });
    actions.append(
      h('span', {
        class: 'readonly',
        text: canDecide(item)
          ? '你有审批角色。去 dsh 对话框里选「允许执行」，或用下面的指令让 agent 提交决定。'
          : need.length
            ? '需要 ' + need.join(' / ') + ' 角色才能批；你的角色：' + (state.roles.join('、') || '（无）')
            : '该条目没有指定审批角色（requiredRoles 为空），任何人都不应批准。',
      })
    );
    if (item.planId) {
      actions.append(h('code', { class: 'decide-hint', text: '批准 ' + item.planId + ' / 驳回 ' + item.planId }));
    }
    card.append(actions);
  } else {
    card.append(h('p', { class: 'meta muted' }, '已到终态，不接受操作；如需重做请让业务员重新发起。'));
  }

  // 「回到业务系统核对」：审批人真正想知道的是「批准之后库存到底减了没有」。
  // 链接按这张卡涉及的模块给，不多给——一张只动库存的卡不该出现报价入口。
  const mods = involvedModules(item);
  if (mods.length) {
    const links = h('p', { class: 'meta module-links' }, '去业务系统核对：');
    mods.forEach((mod, i) => {
      if (i > 0) links.append(' · ');
      links.append(h('a', { href: MODULE_PAGES[mod], target: '_blank', rel: 'noreferrer' }, MODULE_NAMES[mod] + '系统'));
    });
    card.append(links);
  }

  return card;
}

// 三个业务系统的页面地址与名字（模块 id 前缀 → 页面）。
const MODULE_PAGES = {
  inventory: 'http://127.0.0.1:8081/',
  procurement: 'http://127.0.0.1:8082/',
  quote: 'http://127.0.0.1:8083/',
};
const MODULE_NAMES = { inventory: '库存', procurement: '备货', quote: '报价' };

// involvedModules 从一张卡的步骤里取出去重后的模块 id（action 形如 "inventory.DeductStock"）。
function involvedModules(item) {
  const seen = [];
  (Array.isArray(item.items) ? item.items : []).forEach((entry) => {
    const action = entry && typeof entry.action === 'string' ? entry.action : '';
    const mod = action.split('.')[0];
    if (MODULE_PAGES[mod] && seen.indexOf(mod) < 0) seen.push(mod);
  });
  return seen;
}

// pollIntents 拉取 agent → 界面的结局帧。
//
// 这条通道本来就有（GET /tenon/intents/poll），只是界面一直没用它——于是「agent 说了
// 什么」只能从公告板推断，而没有卡片的回合（被闸门拒、auto 直接执行）就完全看不见。
async function pollIntents() {
  try {
    const data = await api('GET', '/tenon/intents/poll?after=' + intentCursor, undefined, false);
    const frames = (data && data.frames) || [];
    let changed = false;
    frames.forEach((entry) => {
      // 宿主返回的是 {"seq":N,"frame":{...}}：帧本体在 frame 里，外面还包了一层序号。
      // 直接按平铺读会全部丢掉（取到 undefined），而界面表现为「agent 从来不说话」。
      const frame = (entry && entry.frame) || entry;
      if (!frame || frame.type !== 'tenon.intent' || !frame.intentId) return;
      if (state.intents.some((it) => it.intentId === frame.intentId)) return;
      state.intents.push({
        intentId: frame.intentId,
        planId: frame.planId || '',
        requestId: frame.requestId || '',
        outcome: frame.outcome || '',
        message: frame.message || '',
        at: Date.now(),
      });
      changed = true;
    });
    if (typeof data.nextCursor === 'number' && data.nextCursor !== intentCursor) {
      intentCursor = data.nextCursor;
      sessionStorage.setItem('tenon.intentCursor', String(intentCursor));
    }
    // 只留最近 100 条：界面是回放窗口，不是审计库。
    if (state.intents.length > 100) state.intents = state.intents.slice(-100);
    if (changed) renderIntents();
  } catch (_) {
    /* 结局帧拉不到不该影响主流程：公告板仍然可用 */
  }
}

// 结局 → 左边框语气。completed 用 ok，needs_human 用 warn，failed 用 bad。
const OUTCOME_TONE = { completed: 'ok', needs_human: 'warn', failed: 'bad' };

/**
 * renderIntents 把 agent 的帧按时间顺序铺开，**只读**。
 *
 * 这里刻意不挂任何按钮：批准要落到宿主签票与审计链上，那条路径只有 dsh 里的
 * pending_decide 走得到。看板再放一个「批准」按钮，就等于给同一份计划开第二个写入口。
 */
function renderIntents() {
  const log = $('intent-log');
  if (!log) return;
  log.textContent = '';

  if (!state.intents.length) {
    log.append(h('p', { class: 'empty' },
      '还没有帧。agent 处理完一个回合后，这里会出现它的结论。'));
    return;
  }

  state.intents
    .slice()
    .sort((a, b) => a.at - b.at)
    .forEach((intent) => {
      const row = h('div', { class: 'bubble agent ' + (OUTCOME_TONE[intent.outcome] || 'wait') });
      row.append(h('div', { class: 'bubble-who' }, 'agent · ' + timeOf(intent.at)));
      row.append(h('div', { class: 'bubble-text' }, intent.message || '（这一回合没有给出结论）'));
      const meta = [];
      if (intent.outcome) meta.push(intent.outcome);
      if (intent.planId) meta.push(intent.planId);
      if (meta.length) row.append(h('div', { class: 'bubble-meta' }, meta.join(' · ')));
      log.append(row);
    });
}

function timeOf(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  const pad = (n) => String(n).padStart(2, '0');
  return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
}

function renderBoard() {
  const pendingList = $('pending-list');
  const processedList = $('processed-list');
  pendingList.textContent = '';
  processedList.textContent = '';

  if (!state.token) {
    pendingList.append(h('p', { class: 'empty' }, '登录后显示待批条目'));
    processedList.append(h('p', { class: 'empty' }, '登录后显示已处理条目'));
    $('pending-count').textContent = '';
    $('processed-count').textContent = '';
    // 帧不需要登录也能读：agent 说过的话不能因为没登录就从界面上消失。
    renderIntents();
    return;
  }

  // 公告板条目应当都是对象；坏数据直接跳过，别让一张畸形卡把整块看板渲染挂掉。
  const items = (Array.isArray(state.items) ? state.items : []).filter(
    (it) => it && typeof it === 'object'
  );
  const pending = items.filter(isPending);
  // 已处理区把最近处理的放前面（服务端按 expiresAt 升序给的是待批优先的次序）。
  const processed = items.filter((it) => !isPending(it)).slice().reverse();

  $('pending-count').textContent = '（' + pending.length + '）';
  $('processed-count').textContent = '（' + processed.length + '）';

  if (!pending.length) {
    // 「板子是空的」有三个完全不同的原因，别让人以为功能坏了：agent 还在规划（真模型
    // 一个回合要 10–40 秒）、计划根本不需要审批（auto 直接执行了）、或者卡片已过期
    // （15 分钟 TTL，自检会把它归档）。把三条都写出来，比一句「没有待批条目」有用。
    pendingList.append(
      h('p', { class: 'empty' },
        '没有待批条目。可能的原因：① agent 正在规划（真模型一个回合约 10–40 秒，稍等再看）；' +
        '② 这份计划不需要审批（策略判 auto，已直接执行，去「已处理」看）；' +
        '③ 卡片已过期（15 分钟 TTL，过期即归档）。')
    );
  }
  pending.forEach((item) => pendingList.append(renderCard(item, true)));

  if (!processed.length) processedList.append(h('p', { class: 'empty' }, '还没有处理过的条目。'));
  processed.forEach((item) => processedList.append(renderCard(item, false)));

  // 帧区每次都跟着公告板重算：agent 说过什么是服务端事实，不是本地缓存。
  renderIntents();

  tickExpiry();
}

function tickExpiry() {
  const nodes = document.querySelectorAll('.expiry');
  for (const node of nodes) {
    const raw = node.dataset.exp || '';
    if (!raw) {
      node.textContent = '（未给 expiresAt）';
      node.classList.remove('expired');
      continue;
    }
    const at = Date.parse(raw);
    if (Number.isNaN(at)) {
      node.textContent = '（expiresAt 无法解析，原样显示）';
      node.classList.remove('expired');
      continue;
    }
    const left = at - Date.now();
    if (left <= 0) {
      node.textContent = '已过期（等宿主剔除）';
      node.classList.add('expired');
      continue;
    }
    node.classList.remove('expired');
    node.textContent = '剩余 ' + durationText(left);
  }
}

function reportError(err) {
  if (err && err.status === 401) {
    stopPolling();
    dropSession();
    renderSession();
    renderBoard();
    showError('登录已失效（HTTP 401），请重新登录。' + (err.message ? '（' + err.message + '）' : ''));
    return;
  }
  showError(err && err.message ? err.message : String(err));
}

async function refreshBoard(options) {
  const manual = !!(options && options.manual);
  if (!state.token) {
    renderBoard();
    return;
  }
  if (state.refreshing) return; // 上一轮还没回来：跳过，不排队
  state.refreshing = true;

  try {
    const data = await api('GET', '/tenon/pending');
    state.items = Array.isArray(data.items) ? data.items : [];
    if (state.errorFromPoll) clearError();
    renderBoard();
    $('board-updated').textContent = clockText(new Date());
  } catch (err) {
    if (err && err.status === 401) {
      reportError(err);
      return;
    }
    state.errorFromPoll = !manual;
    showError(
      (manual ? '看板刷新失败：' : '看板自动刷新失败：') + (err && err.message ? err.message : String(err))
    );
  } finally {
    state.refreshing = false;
  }
}

function startPolling() {
  stopPolling();
  state.pollTimer = setInterval(() => {
    // 公告板与结局帧一起拉：前者是「等你批的」，后者是「agent 说了什么」。
    // 两者都不带人的登录身份也能读，所以未登录时也在跑。
    refreshBoard();
    pollIntents();
  }, POLL_MS);
}

function stopPolling() {
  if (state.pollTimer !== null) {
    clearInterval(state.pollTimer);
    state.pollTimer = null;
  }
}

// ---------- 事件绑定 ----------

function init() {
  renderIntents();
  $('login-form').addEventListener('submit', onLogin);
  $('logout').addEventListener('click', onLogout);
  $('error-close').addEventListener('click', clearError);

  // 看板没有可点的动作：待批卡上只有文字与一句「去 dsh 里怎么说」的指令，
  // 所以这里不再绑 click 处理器（旧版绑过 onBoardClick，那是第二个写入口）。
  setInterval(tickExpiry, 1000);
  // 结局帧先拉一次：刷新页面后立刻补上「agent 说过什么」，不用等第一个轮询周期。
  pollIntents();

  // 留一个可观测的接管标记：无头/人工排查时用它区分「脚本没跑」与「脚本跑了但没数据」。
  document.documentElement.dataset.app = 'ready';

  if (loadSession() && sessionValid()) {
    renderSession();
    startPolling();
    setStatus('已恢复登录态：' + state.user);
    refreshBoard({ manual: true });
  } else {
    dropSession();
    renderSession();
    renderBoard();
  }
}

init();
