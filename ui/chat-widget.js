// ⚠ 已停用（2026-11-01，T11 对话入口收口）。
//
// 对话入口现在只有一个：dsh 容器（http://127.0.0.1:3080/），走 MCP 工具面
// （mcp__tenon__*）。本文件不再被任何页面挂载 —— registry.html 已移除 #agent-chat，
// 业务三页已改成「复制话术 + 打开 dsh」，共驾台已下线对话面板。
//
// 留着它只作为「页面内嵌对话」的参考实现，**不要重新挂载**：多一个对话入口就多一条
// 写路径，同一份计划可能被点两次，而审计里分不清是谁点的。要加对话能力，去 tenon-mcp 加工具。
//
// chat-widget.js —— 可嵌入的 agent 对话组件（同源，直接接宿主的对话 API）。
//
// 为什么不用 iframe 嵌 dsh（3081）：dsh 对浏览器有信任栅栏（非浏览器请求直接 401），
// 而且它是**另一个源**——嵌进来既拿不到登录态，也过不了 frameborder/CSP 那一关，
// 最后得到的是一个「能看见却点不动」的框。这里走同源 + 宿主 API：
// 与 dsh 的 MCP 工具（plan_preview / pending_decide）是同一个后端，只是入口不同。
//
// 挂载方式：
//   <div id="agent-chat"></div>
//   <script src="/ui/chat-widget.js"></script>
//   ChatWidget.mount('#agent-chat');
//
// 数据来源全部是宿主的真值：
//   POST /tenon/requests          人对 agent 说话
//   GET  /tenon/intents/poll       agent 每回合的结论（含拒绝理由）
//   GET  /tenon/pending           待批 / 已处理
//   POST /tenon/requests          tenon.confirm（批准/驳回）

(function () {
  'use strict';

  const TOKEN_KEY = 'tenon.token';
  const CHAT_KEY = 'tenon.ui.chat.v1';
  // 结论帧也要落本地：游标是持久化的，刷新后服务端不会再重复投递那些帧。
  // 不存的话，一刷新就只剩兜底文案——而兜底文案原本写着「结论见上方」，上方却没有了。
  const INTENTS_KEY = 'tenon.ui.intents.v1';
  const POLL_MS = 3000;
  const TERMINAL = new Set(['done', 'rejected', 'expired', 'failed', 'stale']);

  const STATE_LABEL = {
    awaiting_approval: '等你批准',
    done: '已执行',
    rejected: '已驳回',
    expired: '已过期',
    stale: '已失效',
    failed: '执行失败',
  };
  const OUTCOME_TONE = { completed: 'ok', needs_human: 'warn', failed: 'bad' };

  function el(tag, attrs, text) {
    const node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach((k) => {
        if (k === 'class') node.className = attrs[k];
        else if (k === 'onclick') node.addEventListener('click', attrs[k]);
        else node.setAttribute(k, attrs[k]);
      });
    }
    if (text !== undefined && text !== null) node.textContent = text;
    return node;
  }

  function timeOf(ms) {
    if (!ms) return '';
    const d = new Date(ms);
    const pad = (n) => String(n).padStart(2, '0');
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function loadChat() {
    try {
      const raw = sessionStorage.getItem(CHAT_KEY);
      const parsed = raw ? JSON.parse(raw) : [];
      return Array.isArray(parsed) ? parsed.filter((m) => m && typeof m.text === 'string') : [];
    } catch (_) {
      return [];
    }
  }

  function loadIntents() {
    try {
      const raw = sessionStorage.getItem(INTENTS_KEY);
      const parsed = raw ? JSON.parse(raw) : [];
      return Array.isArray(parsed) ? parsed.filter((it) => it && it.intentId) : [];
    } catch (_) {
      return [];
    }
  }

  function saveIntents(list) {
    try { sessionStorage.setItem(INTENTS_KEY, JSON.stringify(list.slice(-100))); } catch (_) { /* 隐私模式 */ }
  }

  async function api(method, path, payload, token) {
    const headers = {};
    if (payload !== undefined) headers['Content-Type'] = 'application/json';
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const res = await fetch(path, {
      method,
      headers,
      body: payload === undefined ? undefined : JSON.stringify(payload),
    });
    const raw = await res.text();
    let data = null;
    try { data = raw ? JSON.parse(raw) : null; } catch (_) { /* 非 JSON 也能报错 */ }
    if (!res.ok) {
      const msg = (data && data.tenonError && data.tenonError.message) || raw || ('HTTP ' + res.status);
      throw new Error('HTTP ' + res.status + '：' + msg);
    }
    return data;
  }

  function mount(selector) {
    const root = typeof selector === 'string' ? document.querySelector(selector) : selector;
    if (!root) return null;

    const state = { token: sessionStorage.getItem(TOKEN_KEY) || '', user: '', chat: loadChat(), intents: loadIntents(), items: [] };
    let cursor = Number(sessionStorage.getItem('tenon.intentCursor') || 0);
    let timer = null;

    // ---- DOM ----
    root.className = 'chat-widget';
    root.innerHTML = '';

    const bar = el('div', { class: 'cw-bar' });
    const userInput = el('input', { class: 'cw-user', value: 'alice', size: '8', 'aria-label': '用户名' });
    const passInput = el('input', { class: 'cw-pass', type: 'password', value: 'alice-dev', size: '10', 'aria-label': '口令' });
    const loginBtn = el('button', { class: 'cw-ghost' }, '登录');
    const who = el('span', { class: 'cw-status' }, state.token ? '已登录（沿用本页会话）' : '未登录：发送前先登录');
    bar.append(userInput, passInput, loginBtn, who);

    const log = el('div', { class: 'cw-log' });
    const compose = el('div', { class: 'cw-compose' });
    const text = el('textarea', { rows: '2', placeholder: '例：把 STD-1001 的 12 件库存扣掉，给 Q-1 发货' });
    const sendBtn = el('button', { class: 'cw-primary' }, '发送');
    const hint = el('span', { class: 'cw-hint' }, '');
    // 注意：el() 的第三个参数是**文本**（它写 textContent）。把节点当第 3/4 个参数传进去
    // 会被字符串化成 "[object HTMLButtonElement]"，按钮节点根本不会进 DOM——症状是
    // 「发送按钮不见了，但 Ctrl+Enter 又能发」（事件挂在游离节点上）。
    const composeRow = el('div', { class: 'cw-row' });
    composeRow.append(sendBtn, hint);
    compose.append(text, composeRow);

    const board = el('div', { class: 'cw-board' });
    const error = el('div', { class: 'cw-error' });
    error.style.display = 'none';

    root.append(bar, error, log, compose, board);

    function showError(msg) { error.textContent = msg; error.style.display = 'block'; }
    function clearError() { error.style.display = 'none'; }

    function saveChat() {
      try { sessionStorage.setItem(CHAT_KEY, JSON.stringify(state.chat.slice(-50))); } catch (_) { /* 隐私模式 */ }
    }

    // ---- 渲染对话 ----
    // lastFor 取**最后**一条匹配的结论，而不是第一条。
    //
    // 同一笔业务会产生多条 Intent：挂起时一条（「已挂起等待审批」），批准后 agent 恢复
    // 执行又一条（「已执行 / 库存 108」）。取第一条的后果是：人点完批准，气泡仍停在
    // 「等待审批」，看上去像没生效（浏览器实测就是这个症状）。
    function lastFor(predicate) {
      for (let i = state.intents.length - 1; i >= 0; i--) {
        if (predicate(state.intents[i])) return state.intents[i];
      }
      return null;
    }

    function replyFor(message) {
      const card = message.requestId ? state.items.find((it) => it && it.requestId === message.requestId) : null;
      const byRequest = message.requestId
        ? lastFor((it) => it.requestId === message.requestId)
        : null;
      const byPlan = card && card.planId ? lastFor((it) => it.planId === card.planId) : null;
      return { intent: byRequest || byPlan, card };
    }

    function renderChat() {
      log.innerHTML = '';
      if (!state.chat.length) {
        log.append(el('p', { class: 'cw-empty' },
          '还没有对话。写一句话发出去，agent 的结论会挂在这条消息下面（含被闸门拒绝的理由）。'));
        return;
      }

      state.chat.forEach((message) => {
        const ub = el('div', { class: 'cw-bubble cw-user' });
        ub.append(el('div', { class: 'cw-who' }, (message.by || '我') + ' · ' + timeOf(message.at)));
        ub.append(el('div', { class: 'cw-text' }, message.text));
        if (message.requestId) ub.append(el('div', { class: 'cw-meta' }, message.requestId));
        log.append(ub);

        const { intent, card } = replyFor(message);
        if (intent) {
          // 卡片已终结时，卡片状态比那一轮的 outcome 更准：挂起那一刻是 needs_human，
          // 但人批完之后它就是 done。配色跟着卡片走，才和文案一致。
          const tone = card && TERMINAL.has(card.state)
            ? (card.state === 'done' ? 'ok' : card.state === 'failed' ? 'bad' : 'warn')
            : (OUTCOME_TONE[intent.outcome] || 'wait');
          const ab = el('div', { class: 'cw-bubble cw-agent ' + tone });
          ab.append(el('div', { class: 'cw-who' }, 'agent · ' + timeOf(intent.at)));
          ab.append(el('div', { class: 'cw-text' }, intent.message || '（这一回合没有给出结论）'));
          const meta = [intent.outcome, intent.planId].filter(Boolean).join(' · ');
          if (meta) ab.append(el('div', { class: 'cw-meta' }, meta));
          // 卡片已终结时补一行真实状态：恢复执行不一定产生新的 Intent（例如 agent 在
          // 另一路已经跑完），只靠结论帧会让「已执行」停在「等待审批」上。
          if (card && TERMINAL.has(card.state)) {
            ab.append(el('div', { class: 'cw-meta' }, '当前状态：' + (STATE_LABEL[card.state] || card.state)));
          }
          log.append(ab);
          if (card && !TERMINAL.has(card.state)) {
            log.append(el('p', { class: 'cw-meta' }, '↓ 在下面「待批」里批准或驳回'));
          }
          return;
        }

        if (card) {
          // 卡片状态决定语气：已执行=绿、已驳回/过期=黄、失败=红。全用 wait 会让
          // 「已执行」看起来仍是待办。
          const tone = card.state === 'done' ? 'ok'
            : card.state === 'failed' ? 'bad'
            : TERMINAL.has(card.state) ? 'warn' : 'wait';
          const ab = el('div', { class: 'cw-bubble cw-agent ' + tone });
          ab.append(el('div', { class: 'cw-who' }, 'agent'));
          ab.append(el('div', { class: 'cw-text' }, (card.summary || '（无摘要）') + '　—　' +
            (STATE_LABEL[card.state] || card.state)));
          log.append(ab);
          return;
        }

        const age = (Date.now() - (message.at || 0)) / 1000;
        const ab = el('div', { class: 'cw-bubble cw-agent ' + (age < 45 ? 'wait' : 'warn') });
        ab.append(el('div', { class: 'cw-who' }, 'agent'));
        ab.append(el('div', { class: 'cw-text' }, age < 45
          ? '处理中…（真模型一个回合约 10–40 秒）'
          : '这一条没有留下结论，也没有对应到公告板条目：可能这一轮被闸门拒了（拒绝理由在宿主日志 audit-host.jsonl 与 agent 日志里），也可能这份计划不需要审批、已直接执行。'));
        log.append(ab);
      });
      log.scrollTop = log.scrollHeight;
    }

    // ---- 渲染待批 ----
    function decide(planId, decision) {
      return async () => {
        clearError();
        if (!state.token) { showError('先登录再审批。'); return; }
        try {
          const frame = {
            type: 'tenon.confirm',
            confirmId: 'cfm_' + Date.now() + '_' + Math.random().toString(36).slice(2, 8),
            planId,
            decision,
            patch: null,
            at: new Date().toISOString(),
          };
          await api('POST', '/tenon/requests', frame, state.token);
          hint.textContent = (decision === 'approve' ? '已提交批准' : '已提交驳回') + '：' + planId;
          await refresh();
        } catch (err) {
          showError('审批失败：' + err.message);
        }
      };
    }

    function renderBoard() {
      board.innerHTML = '';
      const pending = state.items.filter((it) => it && !TERMINAL.has(it.state));
      const done = state.items.filter((it) => it && TERMINAL.has(it.state));

      board.append(el('h3', { class: 'cw-h3' }, '待批 ' + (pending.length ? '(' + pending.length + ')' : '')));
      if (!pending.length) {
        board.append(el('p', { class: 'cw-empty' },
          '没有待批条目。可能：① agent 正在规划；② 计划判 auto 已直接执行；③ 已过期（15 分钟 TTL）。'));
      }
      pending.forEach((item) => {
        const card = el('div', { class: 'cw-card' });
        card.append(el('div', { class: 'cw-card-title' }, item.summary || item.planId));
        card.append(el('div', { class: 'cw-meta' },
          item.planId + (item.requiredRoles && item.requiredRoles.length ? ' · 需要 ' + item.requiredRoles.join(', ') : '')));
        const row = el('div', { class: 'cw-row' });
        const ok = el('button', { class: 'cw-primary' }, '允许执行');
        ok.addEventListener('click', decide(item.planId, 'approve'));
        const no = el('button', { class: 'cw-ghost' }, '驳回');
        no.addEventListener('click', decide(item.planId, 'reject'));
        row.append(ok, no);
        card.append(row);
        board.append(card);
      });

      if (done.length) {
        board.append(el('h3', { class: 'cw-h3' }, '已处理'));
        done.slice(-5).reverse().forEach((item) => {
          const row = el('div', { class: 'cw-done' });
          row.append(el('span', { class: 'cw-tag ' + (item.state === 'done' ? 'ok' : 'warn') },
            STATE_LABEL[item.state] || item.state));
          row.append(el('span', { class: 'cw-done-text' }, item.summary || item.planId));
          board.append(row);
        });
      }
    }

    // ---- 轮询 ----
    async function refresh() {
      try {
        const [poll, boardData] = await Promise.all([
          api('GET', '/tenon/intents/poll?after=' + cursor).catch(() => null),
          state.token ? api('GET', '/tenon/pending', undefined, state.token).catch(() => null) : Promise.resolve(null),
        ]);

        if (poll && Array.isArray(poll.frames)) {
          poll.frames.forEach((entry) => {
            // 宿主返回 {"seq":N,"frame":{...}}：帧本体在 frame 里。按平铺读会全部丢掉，
            // 表现为「agent 从来不说话」。
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
          });
          if (typeof poll.nextCursor === 'number' && poll.nextCursor !== cursor) {
            cursor = poll.nextCursor;
            sessionStorage.setItem('tenon.intentCursor', String(cursor));
          }
          if (state.intents.length > 100) state.intents = state.intents.slice(-100);
          saveIntents(state.intents);
        }

        if (boardData && Array.isArray(boardData.items)) state.items = boardData.items;

        clearError();
        renderChat();
        renderBoard();
      } catch (err) {
        showError('刷新失败：' + err.message);
      }
    }

    // ---- 交互 ----
    loginBtn.addEventListener('click', async () => {
      clearError();
      try {
        const data = await api('POST', '/tenon/session', {
          user: userInput.value.trim(),
          password: passInput.value,
        });
        state.token = data.token || '';
        state.user = data.user || userInput.value.trim();
        sessionStorage.setItem(TOKEN_KEY, state.token);
        who.textContent = '已登录 ' + state.user + '（' + ((data.roles || []).join(' ') || '无角色') + '）';
        await refresh();
      } catch (err) {
        who.textContent = '未登录';
        showError('登录失败：' + err.message);
      }
    });

    sendBtn.addEventListener('click', async () => {
      clearError();
      const value = text.value.trim();
      if (!value) { showError('先写一句话。'); return; }
      if (!state.token) { showError('先登录：请求要挂在某个人名下（宿主会签发 requestId）。'); return; }
      try {
        const data = await api('POST', '/tenon/requests', { type: 'tenon.request', text: value }, state.token);
        text.value = '';
        state.chat.push({ text: value, by: state.user, at: Date.now(), requestId: data.requestId || '', seq: data.seq });
        saveChat();
        hint.textContent = '已投递（seq ' + data.seq + '）';
        renderChat();
        await refresh();
      } catch (err) {
        showError('发送失败：' + err.message);
      }
    });

    text.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) sendBtn.click();
    });

    refresh();
    timer = setInterval(refresh, POLL_MS);

    return {
      stop: () => { if (timer) clearInterval(timer); },
      refresh,
    };
  }

  window.ChatWidget = { mount };
})();
