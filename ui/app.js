'use strict';

/*
 * Tenon 对话入口（无构建链的纯静态页）。
 *
 * 协议形状以宿主代码为准，这里不猜：
 *   POST /tenon/session   {user, password}                → {token, user, roles, exp}
 *   POST /tenon/requests  tenon.request / tenon.confirm    → {ok, seq, durable}
 *   GET  /tenon/pending   Bearer token                     → {items:[{planId, requestId,
 *                                                            by, summary, items:[{label}],
 *                                                            requiredRoles, expiresAt, mine, state}]}
 *
 * 四条不能松的约定：
 *   1. requestId 由宿主签发，前端不生成；前端只生成 confirmId（^cfm_[A-Za-z0-9_-]{4,}$）；
 *   2. confirm 帧不放 roles —— 宿主先删后写（取 token 里的角色），填了也会被丢弃；
 *   3. 未知 state 一律当待批（fail-closed），绝不显示成已完成；
 *   4. 错误必须显示在页面上（tenonError.code/message 或原始响应体），不能只进 console。
 */

const POLL_MS = 2000;
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

function interleave(nodes) {
  const out = [];
  nodes.forEach((node, i) => {
    if (i) out.push(' · ');
    out.push(node);
  });
  return out;
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
  $('send').disabled = !logged;
  $('request-text').disabled = !logged;
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

// ---------- 说话 ----------

async function onSend() {
  clearError();
  if (!state.token) {
    showError('尚未登录：先登录再发送。');
    return;
  }

  const text = $('request-text').value.trim();
  if (!text) {
    showError('请先输入要对 agent 说的话。');
    return;
  }
  if (text.length > 2000) {
    showError('原话超过 2000 字符上限（当前 ' + text.length + '）。');
    return;
  }

  // 视图上下文三项必须一起填：只给一半构造不出合法 EntityRef，宁可在前端拦住。
  let view = null;
  if ($('ctx-toggle').checked) {
    const module = $('ctx-module').value.trim();
    const type = $('ctx-type').value.trim();
    const id = $('ctx-id').value.trim();
    if (!module || !type || !id) {
      showError('视图上下文要三项一起填（module / type / id），或者取消勾选「附带视图上下文」。');
      return;
    }
    view = { module, type, id };
  }

  // 不填 requestId：宿主签发。by 会被宿主用 token 覆写，这里给合法形状即可。
  const frame = { type: 'tenon.request', text, by: state.user };
  if (view) frame.context = { view };

  $('send').disabled = true;
  try {
    const data = await api('POST', '/tenon/requests', frame);
    $('request-text').value = '';
    setStatus(
      '已发送：seq ' + data.seq + '，' + (data.durable ? '已落盘' : '仅内存（未落盘，重启会丢）') + '。等 agent 处理中…'
    );
    await refreshBoard();
  } catch (err) {
    reportError(err);
  } finally {
    $('send').disabled = !state.token;
  }
}

// ---------- 批/驳 ----------

function newConfirmId() {
  let core;
  const cryptoObj = window.crypto || {};
  if (typeof cryptoObj.randomUUID === 'function') {
    core = cryptoObj.randomUUID();
  } else if (typeof cryptoObj.getRandomValues === 'function') {
    const bytes = new Uint8Array(16);
    cryptoObj.getRandomValues(bytes);
    core = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  } else {
    // 非安全上下文（例如用局域网 IP + http 打开）没有 WebCrypto：退化成时间+随机数，够用即可。
    core = Date.now().toString(36) + Math.random().toString(36).slice(2, 12);
  }
  return 'cfm_' + core.replace(/[^A-Za-z0-9_-]/g, '');
}

async function onConfirm(planId, decision, trigger) {
  const item = state.items.find((it) => it && it.planId === planId);
  if (!item) {
    showError('这张卡已经不在当前看板上了（可能刚被处理），请刷新后再试。');
    return;
  }

  // roles 不放：宿主会删掉前端填的 roles，再用 token 里的角色覆写。
  const frame = {
    type: 'tenon.confirm',
    confirmId: newConfirmId(),
    planId: item.planId,
    decision,
    patch: null,
    by: state.user, // 形状合法即可，宿主用 token 的 sub 覆写
    at: new Date().toISOString(),
  };

  clearError();
  const card = trigger && trigger.closest ? trigger.closest('.card') : null;
  const buttons = card ? Array.from(card.querySelectorAll('button')) : [];
  buttons.forEach((b) => {
    b.disabled = true;
  });

  try {
    const data = await api('POST', '/tenon/requests', frame);
    setStatus(
      (decision === 'approve' ? '已提交批准' : '已提交驳回') +
        '：' +
        item.planId +
        '（seq ' +
        data.seq +
        '，等待 agent 处理）'
    );
  } catch (err) {
    reportError(err);
    buttons.forEach((b) => {
      b.disabled = false;
    });
  }

  // 无论成败都立刻刷新一次：卡片状态以服务端为准，不靠前端猜。
  await refreshBoard({ manual: true });
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

function canApprove(item) {
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

  const meta = [];
  if (item.planId) meta.push(h('span', {}, 'planId ', h('code', { text: item.planId })));
  if (item.requestId) meta.push(h('span', {}, 'requestId ', h('code', { text: item.requestId })));
  if (item.by) meta.push(h('span', {}, '发起人 ', h('code', { text: item.by })));
  if (meta.length) card.append(h('p', { class: 'meta' }, ...interleave(meta)));

  const labels = itemLabels(item);
  if (labels.length) {
    const list = h('ul', { class: 'detail' });
    labels.forEach((label) => list.append(h('li', { text: label })));
    card.append(list);
  } else {
    card.append(h('p', { class: 'meta muted' }, '（没有分项明细）'));
  }

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

  if (pending) {
    const actions = h('div', { class: 'card-actions' });
    if (canApprove(item)) {
      actions.append(
        h('button', { type: 'button', class: 'primary', dataset: { act: 'approve' } }, '批准'),
        h('button', { type: 'button', class: 'danger', dataset: { act: 'reject' } }, '驳回'),
        h('span', { class: 'muted' }, '整份计划一次批准；服务端还会再判一次角色。')
      );
    } else {
      actions.append(
        h('span', {
          class: 'readonly',
          text: need.length
            ? '需要 ' + need.join(' / ') + ' 角色才能批；你的角色：' + (state.roles.join('、') || '（无）')
            : '该条目没有指定审批角色（requiredRoles 为空），任何人都不应批准。',
        })
      );
    }
    card.append(actions);
  } else {
    card.append(h('p', { class: 'meta muted' }, '已到终态，不接受操作；如需重做请让业务员重新发起。'));
  }

  return card;
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

  if (!pending.length) pendingList.append(h('p', { class: 'empty' }, '没有待批条目。'));
  pending.forEach((item) => pendingList.append(renderCard(item, true)));

  if (!processed.length) processedList.append(h('p', { class: 'empty' }, '还没有处理过的条目。'));
  processed.forEach((item) => processedList.append(renderCard(item, false)));

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
    refreshBoard();
  }, POLL_MS);
}

function stopPolling() {
  if (state.pollTimer !== null) {
    clearInterval(state.pollTimer);
    state.pollTimer = null;
  }
}

// ---------- 事件绑定 ----------

function onBoardClick(ev) {
  const target = ev.target instanceof Element ? ev.target : null;
  const button = target ? target.closest('button[data-act]') : null;
  if (!button) return;
  const card = button.closest('.card');
  if (!card) return;

  const planId = card.dataset.planId || '';
  const decision = button.dataset.act;
  if (decision !== 'approve' && decision !== 'reject') return;
  if (!planId) {
    showError('这张卡没有 planId，无法提交决定。');
    return;
  }
  if (!state.token) {
    showError('尚未登录：请先登录。');
    return;
  }

  const question =
    decision === 'approve' ? '批准计划 ' + planId + '？' : '驳回计划 ' + planId + '？';
  if (!window.confirm(question)) return;
  onConfirm(planId, decision, button);
}

function init() {
  $('login-form').addEventListener('submit', onLogin);
  $('logout').addEventListener('click', onLogout);
  $('send').addEventListener('click', onSend);
  $('error-close').addEventListener('click', clearError);

  $('ctx-toggle').addEventListener('change', () => {
    $('ctx-fields').hidden = !$('ctx-toggle').checked;
  });

  // Ctrl/Cmd+Enter 直接发送，避免长文本里回车被吃掉。
  $('request-text').addEventListener('keydown', (ev) => {
    if ((ev.ctrlKey || ev.metaKey) && ev.key === 'Enter') onSend();
  });

  $('pending-list').addEventListener('click', onBoardClick);
  $('processed-list').addEventListener('click', onBoardClick);

  setInterval(tickExpiry, 1000);

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
