/* 日程服务端 · 本地管理面板
 *
 * 不依赖任何前端框架或 CDN：所有资源都在 exe 里，服务器断网也能用。
 * 数据来源分两类：
 *   /api/panel/*         面板自身（进程控制、配置、日志、备份）
 *   /api/panel/proxy/*   转给服务核心的业务接口（账号、审核、分发……）
 */
(() => {
'use strict';

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const esc = (v) => String(v == null ? '' : v).replace(/[&<>"']/g, (c) => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
}[c]));

const fmtTime = (sec) => {
  if (!sec) return '—';
  const d = new Date(Number(sec) * 1000);
  if (Number.isNaN(d.getTime())) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
};

const fmtAgo = (sec) => {
  if (!sec) return '—';
  const diff = Math.max(0, Math.floor(Date.now() / 1000 - Number(sec)));
  if (diff < 60) return `${diff} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
};

const fmtDuration = (sec) => {
  const s = Math.max(0, Number(sec) || 0);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d) return `${d} 天 ${h} 小时`;
  if (h) return `${h} 小时 ${m} 分`;
  if (m) return `${m} 分`;
  return `${s} 秒`;
};

const fmtBytes = (n) => {
  const v = Number(n) || 0;
  if (v < 1024) return `${v} B`;
  if (v < 1024 ** 2) return `${(v / 1024).toFixed(1)} KB`;
  if (v < 1024 ** 3) return `${(v / 1024 ** 2).toFixed(1)} MB`;
  return `${(v / 1024 ** 3).toFixed(2)} GB`;
};

const S = {
  state: null,
  page: '',
  modTab: 'words',
  distTab: 'updates',
  maintTab: 'backup',
  logs: { kind: 'server', lines: 300, timer: null },
  busy: false,
};

// ---------------------------------------------------------------- 网络

async function raw(url, opt = {}) {
  const o = Object.assign({}, opt);
  o.headers = Object.assign({}, opt.headers || {});
  if (o.method && o.method !== 'GET') o.headers['X-Panel-Request'] = '1';
  if (o.body && !(o.body instanceof FormData) && typeof o.body !== 'string') {
    o.headers['Content-Type'] = 'application/json';
    o.body = JSON.stringify(o.body);
  }
  const res = await fetch(url, o);
  const ct = res.headers.get('content-type') || '';
  let data = null;
  if (ct.includes('application/json')) {
    data = await res.json().catch(() => ({}));
  } else {
    data = await res.text().catch(() => '');
  }
  return { res, data };
}

async function req(url, opt = {}) {
  const { res, data } = await raw(url, opt);
  if (res.status === 401) {
    await boot();
    throw new Error('登录已过期，请重新登录');
  }
  if (!res.ok) {
    const msg = (data && (data.error || data.message)) ||
      (typeof data === 'string' && data.slice(0, 200)) ||
      `请求失败（HTTP ${res.status}）`;
    throw new Error(msg);
  }
  return data;
}

const pget = (p) => req('/api/panel' + p);
const ppost = (p, body) => req('/api/panel' + p, { method: 'POST', body });
const pput = (p, body) => req('/api/panel' + p, { method: 'PUT', body });
const sget = (p) => req('/api/panel/proxy' + p);
const spost = (p, body) => req('/api/panel/proxy' + p, { method: 'POST', body });
const sput = (p, body) => req('/api/panel/proxy' + p, { method: 'PUT', body });
const sdel = (p) => req('/api/panel/proxy' + p, { method: 'DELETE' });

// ---------------------------------------------------------------- UI 基础

function toast(message, kind = '') {
  const host = $('#toasts');
  if (!host) return;
  const node = document.createElement('div');
  node.className = 'toast ' + kind;
  node.textContent = message;
  host.appendChild(node);
  setTimeout(() => node.remove(), 4200);
}

const fail = (e) => toast(e && e.message ? e.message : String(e), 'err');

function closeModal() {
  $('#modalHost').innerHTML = '';
}

function confirmBox(opts) {
  return new Promise((resolve) => {
    const host = $('#modalHost');
    host.innerHTML = `
      <div class="modal-mask">
        <div class="modal">
          <h3>${esc(opts.title || '确认操作')}</h3>
          <div class="modal-body">
            <div class="${opts.danger ? 'alert bad' : 'alert info'}">${esc(opts.text || '')}</div>
            ${opts.detail ? `<div class="small muted">${esc(opts.detail)}</div>` : ''}
          </div>
          <div class="modal-foot">
            <button class="btn" data-x="cancel">取消</button>
            <button class="btn ${opts.danger ? 'danger' : 'primary'}" data-x="ok">${esc(opts.ok || '确定')}</button>
          </div>
        </div>
      </div>`;
    const done = (v) => { closeModal(); resolve(v); };
    $('[data-x="cancel"]', host).onclick = () => done(false);
    $('[data-x="ok"]', host).onclick = () => done(true);
    $('.modal-mask', host).onclick = (e) => { if (e.target.classList.contains('modal-mask')) done(false); };
  });
}

function formModal(opts) {
  return new Promise((resolve) => {
    const fields = (opts.fields || []).map((f) => {
      let input = '';
      if (f.type === 'textarea') {
        input = `<textarea name="${esc(f.name)}" placeholder="${esc(f.placeholder || '')}">${esc(f.value || '')}</textarea>`;
      } else if (f.type === 'select') {
        input = `<select name="${esc(f.name)}">${(f.options || []).map((o) =>
          `<option value="${esc(o.value)}"${String(o.value) === String(f.value) ? ' selected' : ''}>${esc(o.label)}</option>`).join('')}</select>`;
      } else if (f.type === 'checkbox') {
        input = `<label class="check"><input type="checkbox" name="${esc(f.name)}"${f.value ? ' checked' : ''}> ${esc(f.hint || '启用')}</label>`;
      } else {
        input = `<input type="${esc(f.type || 'text')}" name="${esc(f.name)}" value="${esc(f.value == null ? '' : f.value)}" placeholder="${esc(f.placeholder || '')}">`;
      }
      return `<div class="field ${f.full ? 'full' : ''}">
        <label>${esc(f.label)}</label>${input}
        ${f.help ? `<span class="hint">${esc(f.help)}</span>` : ''}
      </div>`;
    }).join('');

    const host = $('#modalHost');
    host.innerHTML = `
      <div class="modal-mask">
        <div class="modal">
          <h3>${esc(opts.title || '编辑')}</h3>
          <form id="formModal">
            <div class="modal-body">
              ${opts.intro ? `<div class="small muted">${esc(opts.intro)}</div>` : ''}
              ${fields}
            </div>
            <div class="modal-foot">
              <button type="button" class="btn" data-x="cancel">取消</button>
              <button type="submit" class="btn primary">${esc(opts.ok || '保存')}</button>
            </div>
          </form>
        </div>
      </div>`;
    const finish = (v) => { closeModal(); resolve(v); };
    $('[data-x="cancel"]', host).onclick = () => finish(null);
    $('.modal-mask', host).onclick = (e) => { if (e.target.classList.contains('modal-mask')) finish(null); };
    $('#formModal', host).onsubmit = (e) => {
      e.preventDefault();
      const out = {};
      for (const f of opts.fields || []) {
        const el = $(`[name="${f.name}"]`, host);
        if (!el) continue;
        out[f.name] = f.type === 'checkbox' ? el.checked : el.value;
      }
      finish(out);
    };
  });
}

function card(title, body, opts = {}) {
  const actions = opts.actions ? `<div class="row">${opts.actions}</div>` : '';
  return `<section class="card">
    <div class="card-head"><h3>${esc(title)}</h3>${opts.sub ? `<span class="sub">${esc(opts.sub)}</span>` : ''}${actions}</div>
    ${opts.tight ? body : `<div class="card-body">${body}</div>`}
  </section>`;
}

const statCard = (label, value, foot, badge) => `
  <div class="card stat">
    <div class="label">${esc(label)}</div>
    <div class="value">${value}</div>
    <div class="foot">${badge || esc(foot || '')}</div>
  </div>`;

const emptyRow = (cols, text) => `<tr><td colspan="${cols}"><div class="empty">${esc(text)}</div></td></tr>`;

// ---------------------------------------------------------------- 启动与登录

async function boot() {
  stopLogTimer();
  let st = null;
  try {
    st = await pget('/state');
  } catch (e) {
    // 面板本身不可达时不要静默：这段通常会显示在控制台里。
    console.error('state failed', e);
  }
  S.state = st;
  if (!st || !st.session || !st.session.loggedIn) {
    showLogin(st);
    return;
  }
  $('#login').hidden = true;
  $('#app').hidden = false;
  $('#who').textContent = st.session.name || '';
  updateTop();
  if (!location.hash) location.hash = '#/overview';
  navigate();
}

function showLogin(st) {
  $('#app').hidden = true;
  const login = $('#login');
  login.hidden = false;
  const running = !!(st && st.server && st.server.running);
  const firstRun = !!(st && st.firstRun);
  const title = firstRun ? '创建管理员账号' : '登录管理面板';
  const intro = !running
    ? `<div class="alert warn" style="margin-bottom:16px">服务核心还没有运行。${esc((st && st.server && st.server.last_error) || '')}<div style="margin-top:10px"><button class="btn sm" id="loginStart">启动服务</button></div></div>`
    : (firstRun ? `<div class="login-hint">这是第一次启动，还没有任何账号。下面创建的账号将成为管理员。</div>` : '');

  login.innerHTML = `
    <div class="login-card">
      <div class="brand">
        <div class="brand-mark">S</div>
        <div><h1>日程服务端</h1><p>本地管理面板 · v${esc((st && st.panel && st.panel.version) || '')}</p></div>
      </div>
      <h2 style="font-size:17px;margin:0 0 14px">${esc(title)}</h2>
      ${intro}
      <form id="loginForm" autocomplete="on">
        <div class="field" style="margin-bottom:12px">
          <label>用户名</label>
          <input type="text" name="name" autocomplete="username" required autofocus>
        </div>
        <div class="field" style="margin-bottom:12px">
          <label>密码</label>
          <input type="password" name="pass" autocomplete="${firstRun ? 'new-password' : 'current-password'}" required>
        </div>
        ${firstRun ? `<div class="field" style="margin-bottom:12px">
          <label>确认密码</label><input type="password" name="pass2" required>
        </div>` : ''}
        <button class="btn primary" type="submit" style="width:100%;margin-top:6px">${firstRun ? '创建并进入' : '登录'}</button>
      </form>
      <div class="small muted" style="margin-top:16px">
        管理面板只监听本机地址。要远程访问，请用 SSH 隧道转发 9800 端口，或在配置里改成局域网地址。
      </div>
    </div>`;

  const startBtn = $('#loginStart');
  if (startBtn) {
    startBtn.onclick = async () => {
      startBtn.disabled = true;
      try {
        await ppost('/server/start');
        toast('服务已启动', 'ok');
        await boot();
      } catch (e) {
        fail(e);
        startBtn.disabled = false;
      }
    };
  }

  $('#loginForm').onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const name = String(fd.get('name') || '').trim();
    const pass = String(fd.get('pass') || '');
    if (firstRun && pass !== String(fd.get('pass2') || '')) {
      toast('两次输入的密码不一致', 'err');
      return;
    }
    const btn = $('button[type=submit]', e.target);
    btn.disabled = true;
    btn.textContent = '请稍候…';
    try {
      // 登录/注册的 401 是“账号密码不对”，不是“会话过期”，所以不走 req()。
      const path = firstRun ? '/api/panel/register' : '/api/panel/login';
      const { res, data } = await raw(path, { method: 'POST', body: { name, pass } });
      if (!res.ok) throw new Error((data && data.error) || '登录失败');
      await boot();
    } catch (err) {
      fail(err);
      btn.disabled = false;
      btn.textContent = firstRun ? '创建并进入' : '登录';
    }
  };
}

function updateTop() {
  const st = S.state;
  if (!st) return;
  const running = !!(st.server && st.server.running);
  const badge = $('#topStatus');
  badge.className = 'badge ' + (running ? 'ok' : 'bad');
  badge.textContent = running ? '服务运行中' : '服务已停止';
  const foot = $('#sideFoot');
  if (foot) {
    foot.innerHTML = `v${esc((st.panel && st.panel.version) || '')}<br>${esc((st.panel && st.panel.data_dir) || '')}`;
  }
}

async function refreshState() {
  const st = await pget('/state');
  S.state = st;
  updateTop();
  return st;
}

// ---------------------------------------------------------------- 路由

const PAGE_TITLE = {
  overview: '概览',
  server: '服务',
  accounts: '账号',
  moderation: '内容审核',
  distribution: '分发',
  maintenance: '维护',
  settings: '设置',
};

async function navigate() {
  if (!S.state || !S.state.session || !S.state.session.loggedIn) return;
  let page = (location.hash || '#/overview').replace(/^#\//, '');
  if (!PAGE_TITLE[page]) page = 'overview';
  S.page = page;
  stopLogTimer();
  $('#pageTitle').textContent = PAGE_TITLE[page];
  $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.page === page));
  const view = $('#view');
  view.innerHTML = '<div class="empty">加载中…</div>';
  try {
    const mod = PAGES[page];
    if (!mod) throw new Error('未知页面：' + page);
    const html = await mod.render();
    view.innerHTML = html;
    if (mod.mount) mod.mount();
  } catch (e) {
    console.error('render failed', e);
    view.innerHTML = `<div class="alert bad">页面加载失败：${esc(e.message || String(e))}</div>`;
  }
}

window.addEventListener('hashchange', navigate);

// ---------------------------------------------------------------- 概览

async function renderOverview() {
  const st = await refreshState();
  const h = st.server.health || {};
  const running = !!st.server.running;
  let admin = null;
  if (running) {
    try { admin = await sget('/api/admin/overview'); } catch (e) { admin = null; }
  }
  const users = h.users != null ? h.users : (admin ? admin.users : '—');
  const news = [
    ['举报待处理', admin ? admin.reports_pending : '—'],
    ['词库启用', admin ? `${admin.words_enabled} / ${admin.words_total}` : '—'],
    ['封禁账号', admin ? admin.users_banned : '—'],
    ['自动重启', st.config && st.config.auto_restart ? '已开启' : '已关闭'],
  ];

  return `
    <div class="grid cols-4">
      ${statCard('服务状态', running ? '运行中' : '已停止',
        running ? `PID ${st.server.pid} · ${fmtDuration(st.server.uptime)}` : '面板仍在运行，可随时启动',
        `<span class="badge ${running ? 'ok' : 'bad'}">${running ? '正常' : '停止'}</span>`)}
      ${statCard('账号', esc(users), '已注册用户', '')}
      ${statCard('日历', esc(h.calendars != null ? h.calendars : '—'), '未删除的日历', '')}
      ${statCard('日程', esc(h.events != null ? h.events : '—'), '未删除的日程', '')}
    </div>

    <div class="grid cols-2" style="margin-top:16px">
      ${card('快捷操作', `
        <div class="row">
          <button class="btn primary" data-act="start" ${running ? 'disabled' : ''}>启动服务</button>
          <button class="btn" data-act="stop" ${running ? '' : 'disabled'}>停止服务</button>
          <button class="btn" data-act="restart">重启服务</button>
          <button class="btn" data-act="backup" ${running ? '' : 'disabled'}>立即备份</button>
        </div>
        <div class="kv" style="margin-top:18px">
          <div class="k">版本</div><div class="v">${esc(st.panel.version)}</div>
          <div class="k">面板地址</div><div class="v mono">http://${esc(st.panel.url)}</div>
          <div class="k">服务地址</div><div class="v mono">${esc((st.config && st.config.addr) || '未启用 HTTPS')}</div>
          <div class="k">数据目录</div><div class="v mono">${esc(st.panel.data_dir)}</div>
          <div class="k">配置路径</div><div class="v mono">${esc(st.panel.config_path)}</div>
          ${st.server.last_error ? `<div class="k" style="color:var(--danger)">最近错误</div><div class="v">${esc(st.server.last_error)}</div>` : ''}
        </div>`, { sub: `已自动重启 ${st.server.restarts || 0} 次` })}
      ${card('运行概况', `
        <div class="kv">
          ${news.map(([k, v]) => `<div class="k">${esc(k)}</div><div class="v">${esc(v)}</div>`).join('')}
          <div class="k">运行时长</div><div class="v">${running ? esc(fmtDuration(st.server.uptime)) : '—'}</div>
          <div class="k">监听</div><div class="v mono">${esc(listenSummary(st.config))}</div>
          <div class="k">系统</div><div class="v">${esc(st.panel.os)} / ${esc(st.panel.arch)}</div>
        </div>`)}
    </div>

    ${card('最近日志', '<pre class="log" id="ovLog">加载中…</pre>', { tight: true, sub: '服务核心输出', actions: '<button class="btn sm" id="ovLogRefresh">刷新</button>' })}
  `;
}

function listenSummary(c) {
  if (!c) return '—';
  const parts = [];
  if (c.addr) parts.push('HTTPS ' + c.addr);
  if (c.http_addr) parts.push('HTTP ' + c.http_addr);
  if (c.wordbook_addr) parts.push('背词 ' + c.wordbook_addr);
  return parts.join(' · ') || '未配置';
}

async function mountOverview() {
  const load = async () => {
    try {
      const d = await pget(`/logs?kind=console&lines=40`);
      const box = $('#ovLog');
      if (box) box.textContent = d.text || '（暂无日志）';
    } catch (e) { /* 静默：概览页的日志只是辅助信息 */ }
  };
  await load();
  const r = $('#ovLogRefresh');
  if (r) r.onclick = load;

  $('#view').onclick = async (e) => {
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    const act = btn.dataset.act;
    btn.disabled = true;
    try {
      if (act === 'start') { await ppost('/server/start'); toast('服务已启动', 'ok'); }
      if (act === 'stop') {
        if (!(await confirmBox({ title: '停止服务', text: '停止后所有客户端将无法同步，直到再次启动。', ok: '停止', danger: true }))) return;
        await ppost('/server/stop'); toast('服务已停止', 'ok');
      }
      if (act === 'restart') { await ppost('/server/restart'); toast('服务已重启', 'ok'); }
      if (act === 'backup') {
        const d = await ppost('/backup');
        toast(`备份完成：${d.name}`, 'ok');
      }
      await navigate();
    } catch (err) { fail(err); btn.disabled = false; }
  };
}

// ---------------------------------------------------------------- 服务

function netInfoHTML(d) {
  if (!d) return '<div class="alert bad">没有拿到网络信息</div>';
  const copyBtn = (v) => `<button class="btn sm" data-copy="${esc(v)}">复制</button>`;
  const pub = d.public_ok
    ? `<span class="mono">${esc(d.url_public)}</span> ${copyBtn(d.url_public)}`
    : `<span class="muted">探测失败：${esc(d.public_error || '未知原因')}</span>`;
  const lan = (d.url_lan || []).length
    ? d.url_lan.map((u) => `<div style="margin-bottom:6px"><span class="mono">${esc(u)}</span> ${copyBtn(u)}</div>`).join('')
    : '<span class="muted">没有找到内网地址</span>';
  const ifaces = (d.local || []).map((a) =>
    `<div class="small muted mono">${esc(a.name)} ${esc(a.addr)}${a.loopback ? ' · 回环' : a.link_local ? ' · 链路本地，不可用' : a.private ? ' · 内网' : ' · 公网'}</div>`).join('');
  return `<div class="kv">
    <div class="k">公网地址</div><div class="v">${pub}</div>
    <div class="k">内网地址</div><div class="v">${lan}</div>
    <div class="k">本机</div><div class="v"><span class="mono">${esc(d.url_local || '')}</span></div>
    <div class="k">端口</div><div class="v">${esc(String(d.port || ''))}（${esc(d.scheme || 'https')}）</div>
  </div>
  ${ifaces ? `<details style="margin-top:12px"><summary class="small muted">所有网卡地址</summary>${ifaces}</details>` : ''}`;
}

async function renderServer() {
  const st = await refreshState();
  const c = st.config || {};
  const running = !!st.server.running;
  return `
    <div class="card" style="margin-bottom:16px">
      <div class="card-head">
        <h3>进程</h3>
        <span class="badge ${running ? 'ok' : 'bad'}">${running ? '运行中' : '已停止'}</span>
      </div>
      <div class="card-body">
        <div class="row">
          <button class="btn primary" data-act="start" ${running ? 'disabled' : ''}>启动</button>
          <button class="btn" data-act="stop" ${running ? '' : 'disabled'}>停止</button>
          <button class="btn" data-act="restart">重启</button>
          <span class="muted small">PID ${running ? st.server.pid : '—'} · 运行 ${running ? fmtDuration(st.server.uptime) : '—'} · 自动重启 ${c.auto_restart ? '开' : '关'}</span>
        </div>
        ${st.server.last_error ? `<div class="alert bad" style="margin-top:14px">最近错误：${esc(st.server.last_error)}</div>` : ''}
      </div>
    </div>

    ${card('访问地址', `
      <div id="netBox"><div class="empty">点下面的按钮探测本机地址</div></div>
      <div class="row" style="margin-top:12px">
        <button class="btn" data-act="net-probe">探测地址</button>
        <span class="muted small">公网地址给别人连；内网地址给同一个局域网里的设备用</span>
      </div>`, { sub: '部署和局域网同步时填这里' })}

    ${card('监听与目录', `
      <form id="cfgForm">
        <div class="form-grid">
          <div class="field"><label>HTTPS 监听</label><input type="text" name="addr" value="${esc(c.addr || '')}" placeholder=":8443"><span class="hint">留空表示不启用 HTTPS</span></div>
          <div class="field"><label>HTTP 监听</label><input type="text" name="http_addr" value="${esc(c.http_addr || '')}" placeholder=":8080（可选）"><span class="hint">生产环境建议留空，由反向代理终止 TLS</span></div>
          <div class="field"><label>背词同步监听</label><input type="text" name="wordbook_addr" value="${esc(c.wordbook_addr || '')}" placeholder=":8446（可选）"><span class="hint">不需要背词功能就留空</span></div>
          <div class="field"><label>内部管理地址</label><input type="text" name="internal_addr" value="${esc(c.internal_addr || '')}" placeholder="127.0.0.1:0"><span class="hint">仅供面板调用，保持回环即可</span></div>
          <div class="field"><label>数据目录</label><input type="text" name="data_dir" value="${esc(c.data_dir || '')}"><span class="hint">相对路径以 exe 所在目录为基准</span></div>
          <div class="field"><label>证书目录</label><input type="text" name="cert_dir" value="${esc(c.cert_dir || '')}"></div>
          <div class="field"><label>证书域名</label><input type="text" name="server_name" value="${esc(c.server_name || '')}" placeholder="可选，例如 calendar.example.com"><span class="hint">自动生成证书时签进 SAN</span></div>
          <div class="field"><label>运行选项</label>
            <label class="check"><input type="checkbox" name="auto_cert" ${c.auto_cert ? 'checked' : ''}> 证书缺失时自动生成自签证书</label>
            <label class="check"><input type="checkbox" name="auto_restart" ${c.auto_restart ? 'checked' : ''}> 进程意外退出时自动拉起</label>
          </div>
        </div>
        <div class="row end" style="margin-top:18px">
          <button type="submit" class="btn primary">保存配置</button>
        </div>
      </form>`, { sub: '修改监听需要重启服务' })}

    <div class="grid cols-2" style="margin-top:16px">
      ${card('证书', `
        <div class="row" style="margin-bottom:12px">
          <button class="btn" data-act="cert-gen">生成自签证书</button>
          <button class="btn" data-act="cert-upload">上传证书</button>
        </div>
        <div class="small muted">自签证书会同时生成 <span class="mono">ca.crt</span>。把它导入客户端后即可通过校验。正式环境建议换成受信任 CA 的证书。</div>`, { sub: c.cert_dir || '' })}
      ${card('面板', `
        <form id="panelForm">
          <div class="field" style="margin-bottom:12px"><label>面板监听地址</label><input type="text" name="panel_listen" value="${esc(c.panel_listen || '')}"><span class="hint">改成 0.0.0.0:9800 可局域网访问，请同时设置防火墙规则</span></div>
          <label class="check"><input type="checkbox" name="panel_auto_open" ${c.panel_auto_open ? 'checked' : ''}> 启动时自动打开浏览器</label>
          <div class="row end" style="margin-top:14px"><button class="btn" type="submit">保存</button></div>
        </form>`, { sub: '改动监听地址需重启管理器' })}
    </div>

    ${card('日志', `
      <div class="row" style="margin-bottom:12px">
        <div class="seg" id="logSeg">
          <button data-kind="server" class="active">服务日志</button>
          <button data-kind="console">启动控制台</button>
        </div>
        <select id="logLines" style="width:120px">
          <option value="100">100 行</option>
          <option value="300" selected>300 行</option>
          <option value="1000">1000 行</option>
        </select>
        <button class="btn sm" id="logRefresh">刷新</button>
        <label class="check small"><input type="checkbox" id="logAuto"> 自动刷新</label>
      </div>
      <pre class="log" id="logBox" style="border-radius:10px;max-height:460px">加载中…</pre>`, { tight: false })}
  `;
}

async function mountServer() {
  const view = $('#view');
  view.onclick = async (e) => {
    const cp = e.target.closest('[data-copy]');
    if (cp) {
      try {
        await navigator.clipboard.writeText(cp.dataset.copy);
        toast('已复制', 'ok');
      } catch (err) {
        toast('复制失败，请手动选中', 'err');
      }
      return;
    }
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    const act = btn.dataset.act;
    if (act === 'net-probe') {
      btn.disabled = true;
      btn.textContent = '探测中…';
      try {
        const d = await sget('/api/admin/netinfo');
        $('#netBox').innerHTML = netInfoHTML(d);
      } catch (err) {
        fail(err);
      }
      btn.disabled = false;
      btn.textContent = '探测地址';
      return;
    }
    if (act === 'start' || act === 'stop' || act === 'restart') {
      btn.disabled = true;
      try {
        if (act === 'stop' && !(await confirmBox({ title: '停止服务', text: '停止后客户端无法同步。', ok: '停止', danger: true }))) { btn.disabled = false; return; }
        await ppost('/server/' + act);
        toast('操作完成', 'ok');
        await navigate();
      } catch (err) { fail(err); btn.disabled = false; }
      return;
    }
    if (act === 'cert-gen') {
      const f = await formModal({
        title: '生成自签证书',
        fields: [{ name: 'server_name', label: '证书域名', value: (S.state.config || {}).server_name || '', placeholder: '可留空' }],
        ok: '生成',
        intro: '会覆盖证书目录下的 server.crt / server.key，并重建本地 CA。已有客户端需要重新导入 ca.crt。',
      });
      if (!f) return;
      try {
        const d = await ppost('/cert/regenerate', { server_name: f.server_name });
        toast('证书已生成，重启服务后生效', 'ok');
        console.log(d);
      } catch (err) { fail(err); }
      return;
    }
    if (act === 'cert-upload') {
      const input = document.createElement('input');
      input.type = 'file';
      input.accept = '.crt,.pem,.cer,.key';
      input.multiple = true;
      input.onchange = async () => {
        const files = Array.from(input.files || []);
        if (files.length < 2) { toast('请同时选择证书和私钥两个文件', 'err'); return; }
        const fd = new FormData();
        const cert = files.find((f) => /\.(crt|pem|cer)$/i.test(f.name));
        const key = files.find((f) => /\.(key|pem)$/i.test(f.name) && f !== cert);
        if (!cert || !key) { toast('无法区分证书与私钥，请分别命名为 .crt 和 .key', 'err'); return; }
        fd.append('cert', cert);
        fd.append('key', key);
        try {
          await req('/api/panel/cert/upload', { method: 'POST', body: fd });
          toast('证书已上传，重启服务后生效', 'ok');
        } catch (err) { fail(err); }
      };
      input.click();
    }
  };

  $('#cfgForm').onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const patch = {
      addr: String(fd.get('addr') || '').trim(),
      http_addr: String(fd.get('http_addr') || '').trim(),
      wordbook_addr: String(fd.get('wordbook_addr') || '').trim(),
      internal_addr: String(fd.get('internal_addr') || '').trim(),
      data_dir: String(fd.get('data_dir') || '').trim(),
      cert_dir: String(fd.get('cert_dir') || '').trim(),
      server_name: String(fd.get('server_name') || '').trim(),
      auto_cert: fd.get('auto_cert') === 'on',
      auto_restart: fd.get('auto_restart') === 'on',
    };
    try {
      const r = await pput('/config', patch);
      toast(r.restartRequired ? '配置已保存，重启服务后生效' : '配置已保存', 'ok');
      await navigate();
    } catch (err) { fail(err); }
  };

  $('#panelForm').onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    try {
      await pput('/config', {
        panel_listen: String(fd.get('panel_listen') || '').trim(),
        panel_auto_open: fd.get('panel_auto_open') === 'on',
      });
      toast('已保存。面板监听地址的改动需要重启管理器（关闭 exe 后重新打开）', 'ok');
    } catch (err) { fail(err); }
  };

  // 日志
  const loadLog = async () => {
    const box = $('#logBox');
    if (!box) return;
    try {
      const d = await pget(`/logs?kind=${S.logs.kind}&lines=${S.logs.lines}`);
      box.textContent = d.text || '（暂无日志）';
      box.scrollTop = box.scrollHeight;
    } catch (e) {
      box.textContent = '读取日志失败：' + e.message;
    }
  };
  $('#logRefresh').onclick = loadLog;
  $('#logLines').onchange = (e) => { S.logs.lines = Number(e.target.value); loadLog(); };
  $$('#logSeg button').forEach((b) => {
    b.onclick = () => {
      S.logs.kind = b.dataset.kind;
      $$('#logSeg button').forEach((x) => x.classList.toggle('active', x === b));
      loadLog();
    };
  });
  $('#logAuto').onchange = (e) => {
    stopLogTimer();
    if (e.target.checked) S.logs.timer = setInterval(loadLog, 3000);
  };
  await loadLog();
}

function stopLogTimer() {
  if (S.logs.timer) {
    clearInterval(S.logs.timer);
    S.logs.timer = null;
  }
}

// ---------------------------------------------------------------- 账号

async function renderAccounts() {
  await refreshState();
  if (!S.state.server.running) {
    return '<div class="alert warn">服务未运行，账号功能不可用。请先在「服务」页启动。</div>';
  }
  const [d, inv] = await Promise.all([
    sget('/api/admin/users'),
    sget('/api/invite').catch(() => null),
  ]);
  const users = d.users || [];
  const rows = users.length ? users.map((u) => {
    const isMe = u.id === d.me;
    return `<tr>
      <td><strong>${esc(u.name)}</strong>${isMe ? ' <span class="badge plain neutral">我</span>' : ''}</td>
      <td class="mono muted">${esc(u.id)}</td>
      <td>${u.is_admin ? '<span class="badge plain ok">管理员</span>' : '<span class="badge plain neutral">成员</span>'}</td>
      <td>${u.banned ? `<span class="badge bad">已封禁</span>${u.ban_reason ? `<div class="small muted">${esc(u.ban_reason)}</div>` : ''}` : '<span class="badge ok">正常</span>'}</td>
      <td class="nowrap">${esc(fmtTime(u.created_at))}</td>
      <td class="right nowrap">
        <button class="btn sm" data-act="detail" data-id="${esc(u.id)}">详情</button>
        <button class="btn sm" data-act="admin" data-id="${esc(u.id)}" data-name="${esc(u.name)}" data-value="${u.is_admin ? '0' : '1'}">${u.is_admin ? '取消管理员' : '设为管理员'}</button>
        <button class="btn sm" data-act="ban" data-id="${esc(u.id)}" data-name="${esc(u.name)}" data-value="${u.banned ? '0' : '1'}">${u.banned ? '解封' : '封禁'}</button>
        <button class="btn sm danger" data-act="del" data-id="${esc(u.id)}" data-name="${esc(u.name)}">删除</button>
      </td>
    </tr>`;
  }).join('') : emptyRow(6, '还没有账号。');

  return `
    <div class="grid cols-3">
      ${statCard('账号总数', esc(d.total), '', '')}
      ${statCard('管理员', esc(d.admin_count), '', '')}
      ${statCard('已封禁', esc(d.banned_count), '', '')}
    </div>

    ${card('邀请码', `
      <div class="row">
        <input type="text" readonly value="${esc(inv ? inv.invite_code : '（读取失败）')}" class="mono" style="max-width:320px" id="inviteCode">
        <button class="btn" id="copyInvite">复制</button>
        <span class="small muted">注册时使用；留空表示由服务端生成。</span>
      </div>`, { sub: '全站注册凭证' })}

    <div style="height:16px"></div>

    <section class="card">
      <div class="toolbar">
        <h3 style="margin:0;font-size:14px">账号列表</h3>
        <div class="grow"></div>
        <button class="btn sm" id="batchBtn">批量创建</button>
        <button class="btn sm" id="refreshUsers">刷新</button>
      </div>
      <div style="overflow:auto">
        <table>
          <thead><tr><th>用户名</th><th>ID</th><th>角色</th><th>状态</th><th>创建时间</th><th class="right">操作</th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>
      <div class="card-body small muted">${esc(d.admin_policy || '')} ${esc(d.ban_policy || '')}</div>
    </section>`;
}

async function mountAccounts() {
  const copy = $('#copyInvite');
  if (copy) {
    copy.onclick = async () => {
      const v = $('#inviteCode').value;
      try { await navigator.clipboard.writeText(v); toast('已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择文本', 'err'); }
    };
  }
  const rb = $('#refreshUsers');
  if (rb) rb.onclick = () => navigate();

  const batch = $('#batchBtn');
  if (batch) batch.onclick = async () => {
    const f = await formModal({
      title: '批量创建账号',
      intro: '每行一个用户名，使用同一个初始密码。也可以用「前缀 + 数量」生成。',
      fields: [
        { name: 'names', label: '用户名（每行一个）', type: 'textarea', full: true },
        { name: 'prefix', label: '或：前缀', placeholder: '例如 student' },
        { name: 'count', label: '数量', type: 'number', placeholder: '例如 30' },
        { name: 'pass', label: '初始密码', type: 'text', placeholder: '至少 4 位', full: true },
      ],
      ok: '创建',
    });
    if (!f) return;
    const body = { pass: f.pass };
    const names = String(f.names || '').split('\n').map((s) => s.trim()).filter(Boolean);
    if (names.length) body.names = names;
    else { body.prefix = String(f.prefix || '').trim(); body.count = Number(f.count || 0); }
    try {
      const r = await spost('/api/admin/users/batch', body);
      toast(`已创建 ${(r.created || []).length} 个账号`, 'ok');
      await navigate();
    } catch (e) { fail(e); }
  };

  $('#view').onclick = async (e) => {
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    const { act, id, name, value } = btn.dataset;
    if (act === 'detail') {
      try { await showUserDetail(id); } catch (err) { fail(err); }
      return;
    }
    if (act === 'admin') {
      const want = value === '1';
      if (!(await confirmBox({ title: want ? '设为管理员' : '取消管理员', text: `${name}：${want ? '将获得全部管理权限。' : '将失去管理权限。'}`, danger: !want }))) return;
      try { await spost(`/api/admin/users/${encodeURIComponent(id)}/admin`, { is_admin: want }); toast('已更新', 'ok'); await navigate(); }
      catch (err) { fail(err); }
    }
    if (act === 'ban') {
      if (value === '0') {
        if (!(await confirmBox({ title: '解封账号', text: `确定解封 ${name}？` }))) return;
        try { await spost(`/api/admin/users/${encodeURIComponent(id)}/ban`, { banned: false }); toast('已解封', 'ok'); await navigate(); }
        catch (err) { fail(err); }
        return;
      }
      const f = await formModal({
        title: `封禁 ${name}`,
        fields: [
          { name: 'reason', label: '封禁原因', type: 'text', full: true, placeholder: '会显示给本人' },
          { name: 'days', label: '封禁天数', type: 'number', value: '7', help: '填 0 表示永久' },
        ],
        ok: '封禁',
      });
      if (!f) return;
      try {
        await spost(`/api/admin/users/${encodeURIComponent(id)}/ban`, {
          banned: true, reason: f.reason, days: Number(f.days || 0),
        });
        toast('已封禁', 'ok');
        await navigate();
      } catch (err) { fail(err); }
    }
    if (act === 'del') {
      if (!(await confirmBox({ title: '删除账号', text: `删除 ${name} 后，该账号及其数据无法恢复。`, detail: '如果该账号拥有共享日历或团队，服务端会拒绝删除。', danger: true, ok: '删除' }))) return;
      try { await sdel(`/api/admin/users/${encodeURIComponent(id)}`); toast('已删除', 'ok'); await navigate(); }
      catch (err) { fail(err); }
    }
  };
}

// ---------------------------------------------------------------- 账号详情

// showUserDetail 是一个只读的信息页：这个人的日历、日程、设备、最近登录。
// 用它回答"删掉这个账号会连带删掉多少东西"。
async function showUserDetail(id) {
  const d = await sget('/api/admin/users/' + encodeURIComponent(id) + '/detail');
  const s = d.summary || {};
  const calRows = (d.calendars || []).length
    ? d.calendars.map((c) => `<tr>
        <td>${esc(c.name || '未命名')}</td>
        <td>${c.mine ? '<span class="badge plain ok">自己的</span>' : '<span class="badge plain neutral">共享来的</span>'}</td>
        <td class="mono small">${esc(c.kind || '')}</td>
        <td class="right">${c.events}</td>
      </tr>`).join('')
    : `<tr><td colspan="4"><div class="empty">没有日历</div></td></tr>`;
  const evRows = (d.recent_events || []).length
    ? d.recent_events.map((e) => `<tr>
        <td>${esc(e.title || '(无标题)')}${e.is_task ? ' <span class="badge plain neutral">待办</span>' : ''}</td>
        <td class="small muted">${esc(e.calendar)}</td>
        <td class="nowrap small">${e.start_at ? fmtTime(e.start_at) : '—'}</td>
      </tr>`).join('')
    : `<tr><td colspan="3"><div class="empty">没有日程</div></td></tr>`;
  const devRows = (d.devices_detail || []).length
    ? d.devices_detail.map((x) => `<tr>
        <td>${esc(x.name || '—')}</td>
        <td>${esc(x.platform || '')}</td>
        <td>${esc(x.version || '')}</td>
        <td class="nowrap small">${x.last_seen_at ? fmtAgo(x.last_seen_at) : '—'}</td>
      </tr>`).join('')
    : `<tr><td colspan="4"><div class="empty">没有设备上报</div></td></tr>`;
  const logRows = (d.recent_logins || []).length
    ? d.recent_logins.map((l) => `<tr>
        <td>${esc(l.event)}</td>
        <td>${l.result === 'success' ? '<span class="badge ok">成功</span>' : '<span class="badge bad">失败</span>'}</td>
        <td class="mono small">${esc(l.ip || '—')}</td>
        <td class="nowrap small">${fmtTime(l.at)}</td>
      </tr>`).join('')
    : `<tr><td colspan="4"><div class="empty">没有登录记录</div></td></tr>`;

  const ban = d.banned || {};
  const host = $('#modalHost');
  host.innerHTML = `<div class="modal-mask">
    <div class="modal" style="max-width:720px">
      <h3>${esc(d.name)}${d.is_admin ? ' <span class="badge plain ok">管理员</span>' : ''}${ban.active ? ' <span class="badge bad">已封禁</span>' : ''}</h3>
      <div class="modal-body" style="max-height:70vh;overflow:auto">
        <div class="grid cols-4">
          ${statCard('日历', String(s.calendars_owned || 0), '自己的', '')}
          ${statCard('日程', String(s.events_owned || 0), '自己的', '')}
          ${statCard('共享', String(s.calendars_shared || 0), '别人给的', '')}
          ${statCard('设备', String(s.devices || 0), '上报过', '')}
        </div>
        <div class="small muted">账号 ID <span class="mono">${esc(d.id)}</span> · 创建于 ${fmtTime(d.created_at)}</div>
        ${ban.active ? `<div class="alert bad">封禁原因：${esc(ban.reason || '未填写')}${ban.until ? '，到期 ' + fmtTime(ban.until) : '，永久'}</div>` : ''}

        <h3 style="font-size:13px;margin:8px 0 6px">日历</h3>
        <div style="overflow:auto"><table>
          <thead><tr><th>名称</th><th>归属</th><th>类型</th><th class="right">日程数</th></tr></thead>
          <tbody>${calRows}</tbody></table></div>

        <h3 style="font-size:13px;margin:14px 0 6px">最近日程</h3>
        <div style="overflow:auto"><table>
          <thead><tr><th>标题</th><th>日历</th><th>开始</th></tr></thead>
          <tbody>${evRows}</tbody></table></div>

        <h3 style="font-size:13px;margin:14px 0 6px">设备</h3>
        <div style="overflow:auto"><table>
          <thead><tr><th>名称</th><th>平台</th><th>版本</th><th>最近上报</th></tr></thead>
          <tbody>${devRows}</tbody></table></div>

        <h3 style="font-size:13px;margin:14px 0 6px">最近登录</h3>
        <div style="overflow:auto"><table>
          <thead><tr><th>事件</th><th>结果</th><th>IP</th><th>时间</th></tr></thead>
          <tbody>${logRows}</tbody></table></div>
      </div>
      <div class="modal-foot"><button type="button" class="btn" data-x="close">关闭</button></div>
    </div>
  </div>`;
  const close = () => closeModal();
  $('[data-x="close"]', host).onclick = close;
  $('.modal-mask', host).onclick = (e) => { if (e.target.classList.contains('modal-mask')) close(); };
}

// ---------------------------------------------------------------- 审核

async function renderModeration() {
  await refreshState();
  if (!S.state.server.running) return '<div class="alert warn">服务未运行，审核功能不可用。</div>';
  const tabs = `<div class="tabs" style="border:1px solid var(--border);border-bottom:none;border-radius:12px 12px 0 0">
    <button data-tab="words" class="${S.modTab === 'words' ? 'active' : ''}">违禁词</button>
    <button data-tab="reports" class="${S.modTab === 'reports' ? 'active' : ''}">举报</button>
    <button data-tab="ai" class="${S.modTab === 'ai' ? 'active' : ''}">AI 复核</button>
  </div>`;
  let body = '';
  if (S.modTab === 'words') body = await renderWords();
  else if (S.modTab === 'reports') body = await renderReports();
  else body = await renderAI();
  return tabs + `<div style="border:1px solid var(--border);border-top:none;border-radius:0 0 12px 12px;background:var(--card)">${body}</div>`;
}

async function renderWords() {
  const d = await sget('/api/admin/words');
  const words = d.words || [];
  const cats = d.categories || {};
  const rows = words.length ? words.map((w) => `<tr>
    <td><strong>${esc(w.raw || w.word)}</strong></td>
    <td>${esc(cats[w.category] || w.category || '—')}</td>
    <td>${severityBadge(w.severity)}</td>
    <td>${w.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge neutral">停用</span>'}</td>
    <td class="small muted">${esc(w.note || '')}</td>
    <td class="right nowrap">
      <button class="btn sm" data-act="word-edit" data-id="${esc(w.id)}" data-raw="${esc(w.raw || w.word)}" data-cat="${esc(w.category)}" data-sev="${w.severity}" data-note="${esc(w.note || '')}">编辑</button>
      <button class="btn sm" data-act="word-toggle" data-id="${esc(w.id)}" data-value="${w.enabled ? '0' : '1'}">${w.enabled ? '停用' : '启用'}</button>
      <button class="btn sm danger" data-act="word-del" data-id="${esc(w.id)}" data-raw="${esc(w.raw || w.word)}">删除</button>
    </td></tr>`).join('') : emptyRow(6, '词库为空。');
  return `<div class="toolbar">
      <strong style="font-size:14px">违禁词</strong>
      <span class="muted small">共 ${words.length} 条</span>
      <div class="grow"></div>
      <button class="btn sm primary" data-act="word-add">新增</button>
      <button class="btn sm" data-act="word-reload">刷新</button>
    </div>
    <div style="overflow:auto"><table>
      <thead><tr><th>词</th><th>分类</th><th>级别</th><th>状态</th><th>备注</th><th class="right">操作</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
}

function severityBadge(sev) {
  if (sev === 3) return '<span class="badge warn">风险（送 AI）</span>';
  if (sev === 2) return '<span class="badge bad">拦截</span>';
  return '<span class="badge neutral">提醒</span>';
}

async function renderReports() {
  const d = await sget('/api/admin/reports?status=pending');
  const list = d.reports || [];
  const rows = list.length ? list.map((r) => `<tr>
    <td><strong>${esc(r.target_title || r.target_kind || '—')}</strong><div class="small muted">${esc(r.target_excerpt || '')}</div></td>
    <td>${esc(r.reason_label || r.reason || '—')}<div class="small muted">${esc(r.reporter_name || '')} · ${esc(fmtTime(r.created_at))}</div></td>
    <td class="small">${esc(r.detail || '')}</td>
    <td class="right nowrap">
      <button class="btn sm" data-act="rep-ok" data-id="${esc(r.id)}">受理</button>
      <button class="btn sm danger" data-act="rep-del" data-id="${esc(r.id)}">删除内容</button>
    </td></tr>`).join('') : emptyRow(4, '没有待处理的举报。');
  return `<div class="toolbar">
      <strong style="font-size:14px">待处理举报</strong><span class="muted small">${list.length} 条</span>
      <div class="grow"></div><button class="btn sm" data-act="rep-reload">刷新</button>
    </div>
    <div style="overflow:auto"><table>
      <thead><tr><th>对象</th><th>原因</th><th>说明</th><th class="right">处理</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
}

async function renderAI() {
  const d = await sget('/api/admin/ai');
  return `<div class="card-body">
    <div class="alert info" style="margin-bottom:16px">
      内容审核 AI 用来看住“风险词命中但不确定”的内容。未配置或调用失败时一律放行，
      所以它不可用时不会影响正常存日程。
    </div>
    <form id="aiForm">
      <div class="form-grid">
        <div class="field"><label>启用</label><label class="check"><input type="checkbox" name="enabled" ${d.enabled ? 'checked' : ''}> 启用 AI 复核</label></div>
        <div class="field"><label>超时（毫秒）</label><input type="number" name="timeout_ms" value="${esc(d.timeout_ms || 8000)}"></div>
        <div class="field full"><label>接口地址</label><input type="text" name="base_url" value="${esc(d.base_url || d.default_base || '')}" placeholder="${esc(d.default_base || '')}"></div>
        <div class="field"><label>模型</label><input type="text" name="model" value="${esc(d.model || d.default_model || '')}"></div>
        <div class="field"><label>API Key</label><input type="text" name="api_key" placeholder="${d.has_key ? '已设置：' + esc(d.key_masked) + '（留空不修改）' : '未设置'}"></div>
      </div>
      <div class="row end" style="margin-top:16px">
        <button type="button" class="btn" id="aiTest">测试连接</button>
        <button type="submit" class="btn primary">保存</button>
      </div>
    </form>
    <div id="aiTestResult" style="margin-top:14px"></div>
  </div>`;
}

async function mountModeration() {
  const view = $('#view');
  view.onclick = async (e) => {
    const tb = e.target.closest('[data-tab]');
    if (tb) { S.modTab = tb.dataset.tab; await navigate(); return; }
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    const act = btn.dataset.act;
    try {
      if (act === 'word-reload' || act === 'rep-reload') { await navigate(); return; }
      if (act === 'word-add') {
        const f = await formModal({
          title: '新增违禁词',
          fields: [
            { name: 'raw', label: '词', type: 'text', full: true },
            { name: 'category', label: '分类', type: 'text', value: 'general', placeholder: 'general' },
            { name: 'severity', label: '级别', type: 'select', value: '2', options: [
              { value: '1', label: '提醒（不拦截）' }, { value: '2', label: '拦截' }, { value: '3', label: '风险（送 AI 复核）' },
            ] },
            { name: 'note', label: '备注', type: 'text', full: true },
          ],
          ok: '新增',
        });
        if (!f) return;
        await spost('/api/admin/words', { raw: f.raw, category: f.category, severity: Number(f.severity), note: f.note });
        toast('已新增', 'ok'); await navigate(); return;
      }
      if (act === 'word-edit') {
        const f = await formModal({
          title: '编辑违禁词',
          fields: [
            { name: 'raw', label: '词', type: 'text', value: btn.dataset.raw, full: true },
            { name: 'category', label: '分类', type: 'text', value: btn.dataset.cat },
            { name: 'severity', label: '级别', type: 'select', value: btn.dataset.sev, options: [
              { value: '1', label: '提醒' }, { value: '2', label: '拦截' }, { value: '3', label: '风险（送 AI）' },
            ] },
            { name: 'note', label: '备注', type: 'text', value: btn.dataset.note, full: true },
          ],
          ok: '保存',
        });
        if (!f) return;
        await sput(`/api/admin/words/${encodeURIComponent(btn.dataset.id)}`, {
          raw: f.raw, category: f.category, severity: Number(f.severity), note: f.note,
        });
        toast('已保存', 'ok'); await navigate(); return;
      }
      if (act === 'word-toggle') {
        await sput(`/api/admin/words/${encodeURIComponent(btn.dataset.id)}`, { enabled: btn.dataset.value === '1' });
        toast('已更新', 'ok'); await navigate(); return;
      }
      if (act === 'word-del') {
        if (!(await confirmBox({ title: '删除违禁词', text: `确定删除「${btn.dataset.raw}」？`, danger: true, ok: '删除' }))) return;
        await sdel(`/api/admin/words/${encodeURIComponent(btn.dataset.id)}`);
        toast('已删除', 'ok'); await navigate(); return;
      }
      if (act === 'rep-ok') {
        await spost(`/api/admin/reports/${encodeURIComponent(btn.dataset.id)}/handle`, { status: 'accepted', action: 'none' });
        toast('已受理', 'ok'); await navigate(); return;
      }
      if (act === 'rep-del') {
        if (!(await confirmBox({ title: '受理并删除内容', text: '会同时删除被举报的日程/日历内容，无法恢复。', danger: true, ok: '删除内容' }))) return;
        await spost(`/api/admin/reports/${encodeURIComponent(btn.dataset.id)}/handle`, { status: 'accepted', action: 'del_content' });
        toast('已处理', 'ok'); await navigate(); return;
      }
    } catch (err) { fail(err); }
  };

  const aiForm = $('#aiForm');
  if (aiForm) {
    aiForm.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(e.target);
      const body = {
        enabled: fd.get('enabled') === 'on',
        base_url: String(fd.get('base_url') || '').trim(),
        model: String(fd.get('model') || '').trim(),
        timeout_ms: Number(fd.get('timeout_ms') || 8000),
      };
      const key = String(fd.get('api_key') || '').trim();
      if (key) body.api_key = key;
      try { await sput('/api/admin/ai', body); toast('已保存', 'ok'); await navigate(); }
      catch (err) { fail(err); }
    };
    $('#aiTest').onclick = async () => {
      const box = $('#aiTestResult');
      box.innerHTML = '<div class="alert info">正在调用…</div>';
      try {
        const r = await spost('/api/admin/ai/test');
        box.innerHTML = `<div class="alert ${r.ok ? 'info' : 'warn'}">${esc(r.message || '')}${r.elapsed_ms ? ` · ${r.elapsed_ms} ms` : ''}</div>`;
      } catch (err) {
        box.innerHTML = `<div class="alert bad">${esc(err.message)}</div>`;
      }
    };
  }
}

// ---------------------------------------------------------------- 分发

async function renderDistribution() {
  await refreshState();
  if (!S.state.server.running) return '<div class="alert warn">服务未运行，分发功能不可用。</div>';
  const tab = S.distTab || 'updates';
  const tabs = `<div class="tabs" style="border:1px solid var(--border);border-bottom:none;border-radius:12px 12px 0 0">
    <button data-tab="updates" class="${tab === 'updates' ? 'active' : ''}">客户端更新</button>
    <button data-tab="plugins" class="${tab === 'plugins' ? 'active' : ''}">插件</button>
    <button data-tab="devices" class="${tab === 'devices' ? 'active' : ''}">设备</button>
    <button data-tab="federation" class="${tab === 'federation' ? 'active' : ''}">联动服务器</button>
  </div>`;
  let body = '';
  if (tab === 'updates') body = await renderUpdates();
  else if (tab === 'plugins') body = await renderPlugins();
  else if (tab === 'federation') body = await renderFederation();
  else body = await renderDevices();
  return tabs + `<div style="border:1px solid var(--border);border-top:none;border-radius:0 0 12px 12px;background:var(--card)">${body}</div>`;
}

// 一个平台的当前状态卡。enabled 缺省视为启用 —— 老 manifest 没这个键。
function updateCard(platform, label, a) {
  if (!a) {
    return `<div class="card">
      <div class="card-head"><h3>${label}</h3><span class="badge neutral">未发布</span></div>
      <div class="card-body"><div class="empty">还没有发布过这个平台的安装包</div></div>
    </div>`;
  }
  const on = a.enabled === undefined || a.enabled === null ? true : !!a.enabled;
  const size = a.file_exists ? a.file_size : a.size;
  return `<div class="card">
    <div class="card-head">
      <h3>${label}</h3>
      <span class="badge ${on ? 'ok' : 'neutral'}">${on ? '分发中' : '已停用'}</span>
      ${a.file_exists ? '' : '<span class="badge bad">文件缺失</span>'}
    </div>
    <div class="card-body">
      <div class="kv">
        <div class="k">版本</div>
        <div class="v">${esc(a.version)}${a.version_code ? ` <span class="muted">· code ${a.version_code}</span>` : ''}</div>
        <div class="k">文件</div>
        <div class="v mono small">${esc(a.file)}</div>
        <div class="k">大小</div>
        <div class="v">${size ? fmtBytes(size) : '—'}</div>
        <div class="k">SHA256</div>
        <div class="v mono small">${esc((a.sha256 || '').slice(0, 24))}${a.sha256 ? '…' : ''}</div>
        <div class="k">发布时间</div>
        <div class="v">${a.published_at ? fmtTime(a.published_at) : '—'}</div>
        <div class="k">来源</div>
        <div class="v">${a.source === 'clone' ? '从别处克隆' : '本机上传'}</div>
        <div class="k">下载地址</div>
        <div class="v mono small">${esc(a.download_url || '')}</div>
      </div>
      <div class="row" style="margin-top:14px">
        <button class="btn sm" data-act="upd-toggle" data-platform="${platform}" data-on="${on ? '0' : '1'}">${on ? '停止分发' : '恢复分发'}</button>
        <button class="btn sm" data-act="upd-edit" data-platform="${platform}">改版本说明</button>
        <button class="btn sm danger" data-act="upd-del" data-platform="${platform}">删除</button>
      </div>
    </div>
  </div>`;
}

async function renderUpdates() {
  let d = null;
  try { d = await sget('/api/admin/updates'); } catch (e) { d = null; }
  if (!d) return '<div class="card-body"><div class="alert bad">读不到分发状态</div></div>';
  return `<div class="card-body">
    <div class="grid cols-2">
      ${updateCard('android', 'Android', d.android)}
      ${updateCard('windows', 'Windows', d.windows)}
    </div>

    <div class="alert info" style="margin-top:16px">
      停用只影响分发，文件还在磁盘上。恢复分发不需要重新上传。
      ${d.cos_base ? `安装包地址当前指向对象存储：<span class="mono">${esc(d.cos_base)}</span>` : '安装包地址当前指向本机。配好存储桶后可以在「设置」里改。'}
    </div>

    <h3 style="font-size:14px;margin:26px 0 12px">从别的站点克隆</h3>
    <form id="cloneForm">
      <div class="form-grid">
        <div class="field"><label>源站地址</label><input type="text" name="source_url" placeholder="https://example.com" required><span class="hint">填官网地址。会读它的 /api/downloads</span></div>
        <div class="field"><label>平台</label><select name="platform">
          <option value="android">Android（APK）</option>
          <option value="windows">Windows</option>
        </select></div>
        <div class="field"><label>版本号</label><input type="text" name="version" placeholder="留空则从文件名识别"><span class="hint">识别不出来会报错，那时再手填</span></div>
        <div class="field"><label>版本代码</label><input type="number" name="version_code" placeholder="Android 必填"><span class="hint">只能比当前版本大</span></div>
        <div class="field full"><label>说明</label><input type="text" name="notes" placeholder="留空则记为来源地址"></div>
        <div class="field full">
          <label>源站令牌</label>
          <input type="password" name="token" placeholder="克隆官网时留空；克隆另一台本程序时必填">
          <span class="hint">对方的部署令牌，或管理员账号令牌</span>
        </div>
        <div class="field full">
          <label class="check"><input type="checkbox" name="insecure"> 跳过证书校验</label>
          <span class="hint">对方是自签证书时勾选。公网站点不要勾</span>
        </div>
      </div>
      <div class="row end" style="margin-top:14px">
        <button class="btn primary" type="submit">克隆到本机</button>
      </div>
    </form>

    <h3 style="font-size:14px;margin:26px 0 12px">本机上传</h3>
    <form id="updForm">
      <div class="form-grid">
        <div class="field"><label>平台</label><select name="platform">
          <option value="android">Android（APK）</option>
          <option value="windows">Windows（asar / zip）</option>
        </select></div>
        <div class="field"><label>版本号</label><input type="text" name="version" placeholder="例如 1.2.0" required></div>
        <div class="field"><label>版本代码</label><input type="number" name="version_code" placeholder="Android 必填，只能增"><span class="hint">Android 靠它判断新旧；Windows 比较版本字符串</span></div>
        <div class="field"><label>文件名</label><input type="text" name="filename" placeholder="例如 android-1.2.0.apk"><span class="hint">必须带版本号，CDN 按 URL 缓存</span></div>
        <div class="field full"><label>发布说明</label><textarea name="notes" placeholder="这次改了什么"></textarea></div>
        <div class="field full"><label>安装包</label><input type="file" name="file" required></div>
      </div>
      <div class="row end" style="margin-top:16px"><button class="btn primary" type="submit">发布更新</button></div>
    </form>
    <div class="alert info" style="margin-top:16px">
      发布后客户端通过 <span class="mono">/api/updates</span> 检查。已经用过的版本号不要再发，
      文件名也不要复用。
    </div>
  </div>`;
}

// 联动服务器：对端清单 + 路由规则 + 快照导入导出。
async function renderFederation() {
  let peers = [], routes = [];
  try {
    const [p, r] = await Promise.all([
      sget('/api/admin/federation/peers'),
      sget('/api/admin/federation/routes'),
    ]);
    peers = (p && p.peers) || [];
    routes = (r && r.routes) || [];
  } catch (e) {
    return '<div class="card-body"><div class="alert bad">读不到联动配置</div></div>';
  }
  const peerName = (id) => {
    const p = peers.find((x) => x.id === id);
    return p ? p.name : '(已删除)';
  };
  const peerRows = peers.length ? peers.map((p) => `<tr>
    <td>${esc(p.name)}<div class="small muted mono">${esc(p.base_url)}</div></td>
    <td>${p.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge neutral">停用</span>'}</td>
    <td>${p.has_token ? '<span class="badge ok">已配</span>' : '<span class="badge warn">未配</span>'}</td>
    <td class="small">${p.last_ok_at ? '最近连通 ' + fmtAgo(p.last_ok_at) : '从未连通'}</td>
    <td class="small muted">${esc(p.last_error || '')}</td>
    <td class="right nowrap">
      <button class="btn sm" data-act="peer-test" data-id="${p.id}">测连通</button>
      <button class="btn sm" data-act="peer-edit" data-id="${p.id}">编辑</button>
      <button class="btn sm danger" data-act="peer-del" data-id="${p.id}">删除</button>
    </td></tr>`).join('') : emptyRow(6, '还没有添加联动服务器。');

  const routeRows = routes.length ? routes.map((r) => `<tr>
    <td>${kindLabel(r.kind)}</td>
    <td class="mono small">${esc(r.match_key || '（全部）')}</td>
    <td>${esc(peerName(r.peer_id))}</td>
    <td>${r.mode === 'mirror' ? '<span class="badge warn">缓存到本机</span>' : '<span class="badge ok">直接跳转</span>'}</td>
    <td class="small">${r.priority}</td>
    <td>${r.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge neutral">停用</span>'}</td>
    <td class="right nowrap">
      <button class="btn sm" data-act="route-toggle" data-id="${r.id}" data-on="${r.enabled ? '0' : '1'}">${r.enabled ? '停用' : '启用'}</button>
      <button class="btn sm danger" data-act="route-del" data-id="${r.id}">删除</button>
    </td></tr>`).join('') : emptyRow(7, '还没有路由规则。不配规则时全部走本机。');

  return `<div class="card-body">
    <div class="alert info">
      联动只同步分发的文件，账号永远只属于本机。
      <br>「直接跳转」把用户的下载请求 302 给对方，本机一个字节都不过手，对方必须公网可达。
      「缓存到本机」由本机先从对方拉一份存下来再发给用户，适合对方在局域网里或者链路很慢。
    </div>

    <h3 style="font-size:14px;margin:22px 0 12px">对端服务器</h3>
    <div style="overflow:auto"><table>
      <thead><tr><th>名称 / 地址</th><th>状态</th><th>令牌</th><th>最近连通</th><th>最近错误</th><th class="right">操作</th></tr></thead>
      <tbody>${peerRows}</tbody>
    </table></div>
    <div class="row" style="margin-top:12px">
      <button class="btn primary" data-act="peer-add">添加服务器</button>
    </div>

    <h3 style="font-size:14px;margin:26px 0 12px">分发路由</h3>
    <div style="overflow:auto"><table>
      <thead><tr><th>内容</th><th>匹配</th><th>走哪台</th><th>模式</th><th>优先级</th><th>状态</th><th class="right">操作</th></tr></thead>
      <tbody>${routeRows}</tbody>
    </table></div>
    <div class="row" style="margin-top:12px">
      <button class="btn primary" data-act="route-add" ${peers.length ? '' : 'disabled'}>添加路由</button>
      ${peers.length ? '' : '<span class="muted small">先添加一台服务器</span>'}
    </div>

    <h3 style="font-size:14px;margin:26px 0 12px">配置快照</h3>
    <div class="row">
      <button class="btn" data-act="fed-export">导出快照</button>
      <button class="btn" data-act="fed-import">导入快照</button>
      <span class="muted small">快照里没有账号、密码和令牌</span>
    </div>
    <div class="alert info" style="margin-top:14px">
      把这台的快照发给另一台导入，对端清单和路由规则会一起过去。
      版本记录不会覆盖，因为对方磁盘上没有那个文件。
    </div>
  </div>`;
}

function kindLabel(k) {
  switch (k) {
    case 'update': return '客户端更新';
    case 'plugin': return '插件';
    case 'navpack': return '导航包';
    case 'skinpack': return '皮肤包';
    default: return '全部';
  }
}


async function renderPlugins() {
  let published = null, pending = null;
  try { published = await sget('/api/admin/plugins/published'); } catch (e) { published = null; }
  try { pending = await sget('/api/admin/plugins/pending'); } catch (e) { pending = null; }
  const pubs = (published && published.plugins) || [];
  const pends = (pending && (pending.items || pending.submissions || pending.pending)) || [];
  const pubRows = pubs.length ? pubs.map((p) => `<tr>
      <td><strong>${esc(p.name || p.id)}</strong><div class="small muted mono">${esc(p.id)}</div></td>
      <td>${esc(p.version || '')}</td>
      <td>${esc(p.platform || '')}</td>
      <td class="small muted">${esc(p.file || '')}</td>
    </tr>`).join('') : emptyRow(4, '还没有已发布的插件。');
  const pendRows = pends.length ? pends.map((p) => `<tr>
      <td><strong>${esc(p.name || p.id)}</strong><div class="small muted mono">${esc(p.id)}</div></td>
      <td>${esc(p.version || '')}</td>
      <td>${esc(p.platform || '')}</td>
      <td class="right nowrap">
        <button class="btn sm" data-act="plug-ok" data-id="${esc(p.id)}" data-platform="${esc(p.platform || 'android')}">通过</button>
        <button class="btn sm danger" data-act="plug-no" data-id="${esc(p.id)}" data-platform="${esc(p.platform || 'android')}">拒绝</button>
      </td></tr>`).join('') : emptyRow(4, '没有待审核的提交。');

  return `<div class="toolbar"><strong style="font-size:14px">待审核</strong><span class="muted small">${pends.length} 条</span>
      <div class="grow"></div><button class="btn sm" data-act="plug-reload">刷新</button></div>
    <div style="overflow:auto"><table>
      <thead><tr><th>插件</th><th>版本</th><th>平台</th><th class="right">操作</th></tr></thead>
      <tbody>${pendRows}</tbody></table></div>
    <div class="toolbar" style="border-top:1px solid var(--border)"><strong style="font-size:14px">已发布</strong><span class="muted small">${pubs.length} 个</span></div>
    <div style="overflow:auto"><table>
      <thead><tr><th>插件</th><th>版本</th><th>平台</th><th>文件</th></tr></thead>
      <tbody>${pubRows}</tbody></table></div>`;
}

async function renderDevices() {
  const d = await sget('/api/devices');
  const list = d.devices || [];
  const rows = list.length ? list.map((x) => `<tr>
      <td><strong>${esc(x.name || '—')}</strong></td>
      <td>${esc(x.platform || '')}</td>
      <td>${esc(x.version || '')}</td>
      <td>${esc(x.user || '—')}</td>
      <td class="nowrap">${esc(fmtAgo(x.last_seen_at))}</td>
    </tr>`).join('') : emptyRow(5, '还没有设备上报。');
  return `<div class="toolbar"><strong style="font-size:14px">设备</strong><span class="muted small">${list.length} 台</span>
      <div class="grow"></div><button class="btn sm" data-act="dev-reload">刷新</button></div>
    <div style="overflow:auto"><table>
      <thead><tr><th>设备</th><th>平台</th><th>版本</th><th>账号</th><th>最近上报</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
}

async function mountDistribution() {
  const view = $('#view');
  view.onclick = async (e) => {
    const tb = e.target.closest('[data-tab]');
    if (tb) { S.distTab = tb.dataset.tab; await navigate(); return; }
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    const act = btn.dataset.act;
    try {
      if (act === 'plug-reload' || act === 'dev-reload') { await navigate(); return; }

      // ---- 安装包状态 ----
      if (act === 'upd-toggle') {
        const on = btn.dataset.on === '1';
        await spost('/api/admin/updates/' + btn.dataset.platform + '/toggle', { enabled: on });
        toast(on ? '已恢复分发' : '已停止分发', 'ok');
        await navigate();
        return;
      }
      if (act === 'upd-edit') {
        const d = await sget('/api/admin/updates');
        const a = d[btn.dataset.platform];
        if (!a) throw new Error('这个平台还没有发布过包');
        const f = await formModal({
          title: '改版本说明',
          fields: [
            { name: 'version', label: '版本号', value: a.version },
            { name: 'version_code', label: '版本代码', type: 'number', value: a.version_code || '' },
            { name: 'notes', label: '发布说明', type: 'textarea', full: true, value: a.notes || '' },
            { name: 'mandatory', label: '强制更新', type: 'checkbox', value: a.mandatory },
          ],
        });
        if (!f) return;
        await sput('/api/admin/updates/' + btn.dataset.platform, {
          version: f.version,
          version_code: Number(f.version_code) || 0,
          notes: f.notes,
          mandatory: f.mandatory,
        });
        toast('已保存', 'ok');
        await navigate();
        return;
      }
      if (act === 'upd-del') {
        const ok = await confirmBox({ title: '删除安装包', text: '磁盘上的文件也会删掉，客户端将不再收到这个平台的更新。', ok: '删除', danger: true });
        if (!ok) return;
        await sdel('/api/admin/updates/' + btn.dataset.platform);
        toast('已删除', 'ok');
        await navigate();
        return;
      }

      // ---- 联动服务器 ----
      if (act === 'peer-add' || act === 'peer-edit') {
        const editing = act === 'peer-edit';
        let cur = null;
        if (editing) {
          const d = await sget('/api/admin/federation/peers');
          cur = (d.peers || []).find((x) => x.id === btn.dataset.id);
          if (!cur) throw new Error('这台服务器已经不在列表里了');
        }
        const f = await formModal({
          title: editing ? '编辑服务器' : '添加服务器',
          intro: '填对方的根地址，不要带路径。令牌用对方的 deploy-token.txt。',
          fields: [
            { name: 'name', label: '名称', value: cur ? cur.name : '', placeholder: '例如 机房那台' },
            { name: 'base_url', label: '地址', full: true, value: cur ? cur.base_url : '', placeholder: 'https://example.com:8443' },
            { name: 'token', label: '令牌', full: true, type: 'password', placeholder: editing ? '留空表示不更换' : '对方的部署令牌' },
            { name: 'enabled', label: '启用', type: 'checkbox', value: cur ? cur.enabled : true },
            { name: 'insecure', label: '跳过证书校验', type: 'checkbox', value: cur ? cur.insecure : false, hint: '对方是自签证书时勾选' },
          ],
        });
        if (!f) return;
        if (!String(f.base_url || '').trim()) { toast('地址不能为空', 'err'); return; }
        if (editing) {
          const body = { name: f.name, base_url: f.base_url, enabled: f.enabled, insecure: f.insecure };
          if (String(f.token || '').trim()) body.token = f.token;
          await sput('/api/admin/federation/peers/' + btn.dataset.id, body);
        } else {
          await spost('/api/admin/federation/peers', f);
        }
        toast(editing ? '已保存' : '已添加', 'ok');
        await navigate();
        return;
      }
      if (act === 'peer-del') {
        const ok = await confirmBox({ title: '删除服务器', text: '指向它的路由规则也会一起删掉。', ok: '删除', danger: true });
        if (!ok) return;
        await sdel('/api/admin/federation/peers/' + btn.dataset.id);
        toast('已删除', 'ok');
        await navigate();
        return;
      }
      if (act === 'peer-test') {
        btn.disabled = true;
        const r = await spost('/api/admin/federation/peers/' + btn.dataset.id + '/test', {});
        toast(r && r.ok ? '连通正常' : '连不上：' + ((r && r.error) || '未知原因'), r && r.ok ? 'ok' : 'err');
        await navigate();
        return;
      }
      if (act === 'route-add') {
        const d = await sget('/api/admin/federation/peers');
        const peers = d.peers || [];
        if (!peers.length) throw new Error('还没有可以路由的服务器');
        const f = await formModal({
          title: '添加路由',
          intro: '匹配项留空表示这一类全部走那台。',
          fields: [
            { name: 'kind', label: '内容', type: 'select', value: 'update', options: [
              { value: 'update', label: '客户端更新' },
              { value: 'plugin', label: '插件' },
              { value: 'navpack', label: '导航包' },
              { value: 'skinpack', label: '皮肤包' },
              { value: 'all', label: '全部内容' },
            ] },
            { name: 'match_key', label: '匹配项', placeholder: 'android / windows / 插件 id' },
            { name: 'peer_id', label: '走哪台', type: 'select', value: peers[0].id,
              options: peers.map((p) => ({ value: p.id, label: p.name })) },
            { name: 'mode', label: '模式', type: 'select', value: 'route', options: [
              { value: 'route', label: '直接跳转（302）' },
              { value: 'mirror', label: '缓存到本机再发' },
            ] },
            { name: 'priority', label: '优先级', type: 'number', value: 100 },
          ],
        });
        if (!f) return;
        await spost('/api/admin/federation/routes', {
          kind: f.kind, match_key: f.match_key, peer_id: f.peer_id,
          mode: f.mode, priority: Number(f.priority) || 100,
        });
        toast('已添加', 'ok');
        await navigate();
        return;
      }
      if (act === 'route-toggle') {
        await sput('/api/admin/federation/routes/' + btn.dataset.id, { enabled: btn.dataset.on === '1' });
        await navigate();
        return;
      }
      if (act === 'route-del') {
        const ok = await confirmBox({ title: '删除路由', text: '删掉之后这类内容会回到本机分发。', ok: '删除', danger: true });
        if (!ok) return;
        await sdel('/api/admin/federation/routes/' + btn.dataset.id);
        toast('已删除', 'ok');
        await navigate();
        return;
      }
      if (act === 'fed-export') {
        const res = await fetch('/api/panel/proxy/api/admin/federation/export', {
          headers: { 'X-Panel-Request': '1' },
          credentials: 'same-origin',
        });
        if (!res.ok) throw new Error('导出失败：' + res.status);
        const blob = await res.blob();
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = 'federation-snapshot.json';
        a.click();
        setTimeout(() => URL.revokeObjectURL(url), 2000);
        return;
      }
      if (act === 'fed-import') {
        const input = document.createElement('input');
        input.type = 'file';
        input.accept = 'application/json,.json';
        input.onchange = async () => {
          const file = input.files && input.files[0];
          if (!file) return;
          try {
            const text = await file.text();
            const data = JSON.parse(text);
            const r = await spost('/api/admin/federation/import', data);
            toast(`已导入：新增 ${r.added_peers} 台服务器、${r.added_routes} 条路由`, 'ok');
            await navigate();
          } catch (err) { fail(err); }
        };
        input.click();
        return;
      }

      if (act === 'plug-ok' || act === 'plug-no') {
        const approve = act === 'plug-ok';
        const f = await formModal({
          title: approve ? '通过审核' : '拒绝提交',
          fields: [{ name: 'note', label: '备注', type: 'text', full: true, placeholder: approve ? '可留空' : '说明拒绝原因' }],
          ok: approve ? '通过' : '拒绝',
        });
        if (!f) return;
        await spost('/api/admin/plugins/review', {
          platform: btn.dataset.platform, id: btn.dataset.id,
          action: approve ? 'approve' : 'reject', note: f.note,
        });
        toast('已处理', 'ok'); await navigate();
      }
    } catch (err) { fail(err); }
  };

  const cloneForm = $('#cloneForm');
  if (cloneForm) {
    cloneForm.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(cloneForm);
      const body = {
        source_url: String(fd.get('source_url') || '').trim(),
        platform: String(fd.get('platform') || ''),
        version: String(fd.get('version') || '').trim(),
        version_code: Number(fd.get('version_code') || 0),
        notes: String(fd.get('notes') || ''),
        token: String(fd.get('token') || '').trim(),
        insecure: !!fd.get('insecure'),
      };
      if (body.platform === 'android' && !body.version_code) {
        toast('Android 包必须填版本代码', 'err');
        return;
      }
      const sub = cloneForm.querySelector('button[type=submit]');
      sub.disabled = true;
      sub.textContent = '正在克隆…';
      try {
        const r = await spost('/api/admin/updates/clone', body);
        toast('已克隆版本 ' + ((r.artifact && r.artifact.version) || ''), 'ok');
        await navigate();
      } catch (err) {
        fail(err);
        sub.disabled = false;
        sub.textContent = '克隆到本机';
      }
    };
  }

  const form = $('#updForm');
  if (form) {
    form.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(form);
      const file = fd.get('file');
      if (!file || !file.size) { toast('请选择安装包', 'err'); return; }
      const platform = String(fd.get('platform'));
      const version = String(fd.get('version') || '').trim();
      const code = String(fd.get('version_code') || '').trim();
      const name = String(fd.get('filename') || '').trim();
      const notes = String(fd.get('notes') || '');
      if (platform === 'android' && !code) { toast('Android 必须填写版本代码', 'err'); return; }
      const qs = new URLSearchParams({ platform, version });
      if (code) qs.set('version_code', code);
      if (name) qs.set('filename', name);
      if (notes) qs.set('notes', notes);
      try {
        await req('/api/panel/proxy/api/updates/publish?' + qs.toString(), { method: 'POST', body: file });
        toast('已发布', 'ok');
        form.reset();
      } catch (err) { fail(err); }
    };
  }
}

// ---------------------------------------------------------------- 维护

async function renderMaintenance() {
  const st = await refreshState();
  const tabs = `<div class="tabs" style="border:1px solid var(--border);border-bottom:none;border-radius:12px 12px 0 0">
    <button data-tab="backup" class="${S.maintTab === 'backup' ? 'active' : ''}">备份与恢复</button>
    <button data-tab="repair" class="${S.maintTab === 'repair' ? 'active' : ''}">数据修复</button>
    <button data-tab="logs" class="${S.maintTab === 'logs' ? 'active' : ''}">账号日志</button>
    <button data-tab="audit" class="${S.maintTab === 'audit' ? 'active' : ''}">AI 审计</button>
    <button data-tab="system" class="${S.maintTab === 'system' ? 'active' : ''}">系统信息</button>
  </div>`;
  let body = '';
  if (S.maintTab === 'backup') body = await renderBackup();
  else if (S.maintTab === 'repair') body = renderRepair(st);
  else if (S.maintTab === 'logs') body = await renderLoginLogs();
  else if (S.maintTab === 'audit') body = await renderAudit();
  else body = await renderSystem();
  return tabs + `<div style="border:1px solid var(--border);border-top:none;border-radius:0 0 12px 12px;background:var(--card)">${body}</div>`;
}

async function renderBackup() {
  let d = { backups: [] };
  try { d = await pget('/backups'); } catch (e) { /* 首次没有目录 */ }
  const list = d.backups || [];
  const rows = list.length ? list.map((b) => `<tr>
      <td class="mono">${esc(b.name)}</td>
      <td>${esc(fmtBytes(b.size))}</td>
      <td class="nowrap">${esc(fmtTime(b.mod_time))}</td>
      <td class="right"><a class="btn sm" href="/api/panel/backup/download?name=${encodeURIComponent(b.name)}">下载</a></td>
    </tr>`).join('') : emptyRow(4, '还没有备份。');

  return `<div class="card-body">
      <div class="row" style="margin-bottom:16px">
        <button class="btn primary" data-act="mkbackup">生成备份</button>
        <span class="small muted">备份用 VACUUM INTO 生成，是一致性快照，服务运行中也可以做。</span>
      </div>
      <div style="overflow:auto"><table>
        <thead><tr><th>文件</th><th>大小</th><th>时间</th><th class="right"></th></tr></thead>
        <tbody>${rows}</tbody></table></div>

      <div style="border-top:1px solid var(--border);margin:20px -18px 0;padding:18px">
        <h3 style="margin:0 0 8px;font-size:14px">恢复数据库</h3>
        <div class="alert warn" style="margin-bottom:12px">
          恢复会停掉服务、替换当前数据库并重新启动。当前库会另存为 <span class="mono">schedule.db.bak-时间</span>。
        </div>
        <input type="file" id="restoreFile" accept=".db">
        <div class="row" style="margin-top:12px"><button class="btn danger" data-act="restore">上传并恢复</button></div>
      </div>
    </div>`;
}

function renderRepair(st) {
  const running = st.server.running;
  return `<div class="card-body">
    <p class="small muted">这些操作由服务核心执行，都是显式触发，不会自动运行。建议先扫描，确认问题数量后再修复。</p>
    <div class="row" style="margin-bottom:18px">
      <button class="btn" data-act="scan" ${running ? '' : 'disabled'}>扫描孤儿数据</button>
      <button class="btn danger" data-act="repair" ${running ? '' : 'disabled'}>执行修复</button>
    </div>
    <div class="row" style="margin-bottom:18px">
      <button class="btn" data-act="dedupe-dry" ${running ? '' : 'disabled'}>扫描重复日程</button>
      <button class="btn danger" data-act="dedupe-run" ${running ? '' : 'disabled'}>清理重复日程</button>
    </div>
    <pre class="log" id="repairOut" style="border-radius:10px;min-height:120px">等待操作…</pre>
  </div>`;
}

async function renderLoginLogs() {
  let d = { logs: [] };
  try { d = await sget('/api/admin/login-logs?limit=100'); } catch (e) { d = { logs: [], error: e.message }; }
  const list = d.logs || d.items || [];
  const rows = list.length ? list.map((x) => `<tr>
      <td class="nowrap">${esc(fmtTime(x.created_at))}</td>
      <td>${esc(x.user_name || x.user_id || '—')}</td>
      <td>${esc(x.event || '')}</td>
      <td>${esc(x.result || '')}</td>
      <td class="small muted">${esc(x.reason || x.detail || '')}</td>
      <td class="mono small">${esc(x.ip || '')}</td>
    </tr>`).join('') : emptyRow(6, d.error ? '读取失败：' + d.error : '暂无记录。');
  return `<div class="toolbar"><strong style="font-size:14px">账号事件</strong><span class="muted small">最近 100 条</span>
      <div class="grow"></div><button class="btn sm" data-act="logs-reload">刷新</button></div>
    <div style="overflow:auto"><table>
      <thead><tr><th>时间</th><th>账号</th><th>事件</th><th>结果</th><th>原因</th><th>IP</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
}

async function renderAudit() {
  let d = { audits: [] };
  try { d = await sget('/api/admin/moderation-audit?limit=100'); } catch (e) { d = { audits: [], error: e.message }; }
  const list = d.audits || [];
  const rows = list.length ? list.map((a) => {
    const verdict = a.violation
      ? '<span class="badge bad">违规</span>'
      : (a.source === 'error'
        ? '<span class="badge warn">调用失败</span>'
        : '<span class="badge ok">合规</span>');
    return `<tr>
      <td class="nowrap">${esc(fmtTime(a.created_at))}</td>
      <td>${esc(a.field || '—')}</td>
      <td class="small">${esc(a.excerpt || '')}</td>
      <td>${verdict}</td>
      <td>${esc(a.source || '')}</td>
      <td class="small muted">${esc(a.reason || a.error_text || '')}</td>
      <td class="nowrap">${esc(a.model || '')}${a.elapsed_ms ? ` · ${a.elapsed_ms}ms` : ''}</td>
    </tr>`;
  }).join('') : emptyRow(7, d.error ? '读取失败：' + d.error : '暂无记录。');
  const sum = `<div class="row" style="padding:14px 18px;border-bottom:1px solid var(--border)">
      <span class="badge bad">违规 ${esc(d.stat_violation || 0)}</span>
      <span class="badge warn">调用失败 ${esc(d.stat_error || 0)}</span>
      <span class="badge neutral">未启用 ${esc(d.stat_disabled || 0)}</span>
      <span class="small muted">共 ${esc(d.total || list.length)} 条</span>
    </div>`;
  return sum + `<div style="overflow:auto"><table>
      <thead><tr><th>时间</th><th>字段</th><th>内容摘要</th><th>结果</th><th>来源</th><th>说明</th><th>模型 / 耗时</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
}

async function renderSystem() {
  const d = await pget('/system');
  return `<div class="card-body"><div class="kv">
      <div class="k">操作系统</div><div class="v">${esc(d.os)} / ${esc(d.arch)}</div>
      <div class="k">Go 版本</div><div class="v">${esc(d.go)}</div>
      <div class="k">CPU 核心</div><div class="v">${esc(d.cpus)}</div>
      <div class="k">协程数</div><div class="v">${esc(d.goroutines)}</div>
      <div class="k">内存占用</div><div class="v">${esc(fmtBytes(d.mem_alloc))} / ${esc(fmtBytes(d.mem_sys))}</div>
      <div class="k">数据目录</div><div class="v mono">${esc(d.data_dir)}</div>
      <div class="k">数据大小</div><div class="v">${esc(fmtBytes(d.data_size))}</div>
      <div class="k">服务器时间</div><div class="v">${esc(d.time)}</div>
    </div></div>`;
}

async function mountMaintenance() {
  const view = $('#view');
  view.onclick = async (e) => {
    const tb = e.target.closest('[data-tab]');
    if (tb) { S.maintTab = tb.dataset.tab; await navigate(); return; }
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    const act = btn.dataset.act;
    const out = $('#repairOut');
    const log = (obj) => { if (out) out.textContent = JSON.stringify(obj, null, 2); };
    try {
      if (act === 'mkbackup') {
        btn.disabled = true;
        const d = await ppost('/backup');
        toast(`备份完成：${d.name}`, 'ok');
        await navigate();
        return;
      }
      if (act === 'restore') {
        const file = $('#restoreFile').files[0];
        if (!file) { toast('请选择 .db 文件', 'err'); return; }
        if (!(await confirmBox({ title: '恢复数据库', text: '服务会短暂停止，当前数据会被替换。', danger: true, ok: '恢复' }))) return;
        const fd = new FormData();
        fd.append('db', file);
        btn.disabled = true;
        const r = await req('/api/panel/restore', { method: 'POST', body: fd });
        toast('恢复完成，服务已重启', 'ok');
        console.log(r);
        await navigate();
        return;
      }
      if (act === 'scan') { log(await sget('/api/maintenance/scan')); return; }
      if (act === 'repair') {
        if (!(await confirmBox({ title: '执行修复', text: '会修改数据库中的孤儿记录，请先备份。', danger: true, ok: '执行' }))) return;
        log(await spost('/api/maintenance/repair')); return;
      }
      if (act === 'dedupe-dry') { log(await spost('/api/maintenance/dedupe?dry=1')); return; }
      if (act === 'dedupe-run') {
        if (!(await confirmBox({ title: '清理重复日程', text: '会删除判定为重复的日程，请先备份。', danger: true, ok: '清理' }))) return;
        log(await spost('/api/maintenance/dedupe')); return;
      }
      if (act === 'logs-reload') { await navigate(); return; }
    } catch (err) { fail(err); if (out) out.textContent = '失败：' + err.message; }
  };
}

// ---------------------------------------------------------------- 设置

async function renderSettings() {
  const st = await refreshState();
  const c = st.config || {};
  let loc = null, live = null, token = null;
  if (st.server.running) {
    try { loc = await sget('/api/weather/location'); } catch (e) {}
    try { live = await sget('/api/admin/live'); } catch (e) {}
  }
  try { token = await pget('/deploy-token'); } catch (e) {}

  return `
    ${card('业务配置', `
      <form id="bizForm">
        <div class="form-grid">
          <div class="field"><label>天气地点</label><input type="text" name="weather_place" value="${esc((loc && loc.place) || c.weather_place || '')}"><span class="hint">下发给客户端显示的默认地点</span></div>
          <div class="field"><label>天气坐标</label>
            <div class="row"><input type="number" step="0.0001" name="weather_lat" value="${esc(loc ? loc.lat : (c.weather_lat != null ? c.weather_lat : ''))}" placeholder="纬度"><input type="number" step="0.0001" name="weather_lon" value="${esc(loc ? loc.lon : (c.weather_lon != null ? c.weather_lon : ''))}" placeholder="经度"></div>
          </div>
          <div class="field"><label>更新下载前缀（可选）</label><input type="text" name="cos_base" value="${esc(c.cos_base || '')}" placeholder="https://cdn.example.com/updates"><span class="hint">留空则从本机下载更新包</span></div>
          <div class="field"><label>插件市场地址（可选）</label><input type="text" name="market_url" value="${esc(c.market_url || '')}" placeholder="https://example.com/plugins.html"></div>
          <div class="field full"><label>全站邀请码</label><input type="text" name="invite_code" value="${esc(c.invite_code || '')}" placeholder="留空则由服务端生成"><span class="hint">修改后重启服务生效</span></div>
        </div>
        <div class="row end" style="margin-top:16px"><button class="btn primary" type="submit">保存</button></div>
      </form>`, { sub: '存储在服务端数据库或配置文件里' })}

    <div style="height:16px"></div>

    ${card('存储桶与 CDN', `
      <div class="form-grid">
        <div class="field"><label>服务商</label><select id="cosProvider">
          <option value="tencent">腾讯云 COS</option>
        </select></div>
        <div class="field"><label>存储桶</label><input type="text" id="cosBucket" placeholder="my-bucket-1250000000"><span class="hint">格式是「桶名-APPID」</span></div>
        <div class="field"><label>地域</label><select id="cosRegion">
          <option value="ap-guangzhou">广州 ap-guangzhou</option>
          <option value="ap-shanghai">上海 ap-shanghai</option>
          <option value="ap-beijing">北京 ap-beijing</option>
          <option value="ap-chengdu">成都 ap-chengdu</option>
          <option value="ap-chongqing">重庆 ap-chongqing</option>
          <option value="ap-nanjing">南京 ap-nanjing</option>
          <option value="ap-hongkong">中国香港 ap-hongkong</option>
          <option value="ap-singapore">新加坡 ap-singapore</option>
        </select></div>
        <div class="field"><label>CDN 加速域名</label><input type="text" id="cosCdn" placeholder="cdn.example.com"><span class="hint">填了就用它，留空用 COS 源站</span></div>
      </div>
      <div class="row" style="margin-top:14px">
        <button class="btn" data-act="cos-build">生成地址并填入</button>
        <button class="btn ghost" data-act="cos-clear">清空</button>
      </div>
      <div class="alert info" style="margin-top:14px">
        生成的是根地址，安装包要按 <span class="mono">&lt;平台&gt;/&lt;文件名&gt;</span> 上传。
        例如 Android 包放 <span class="mono">android/android-1.2.0.apk</span>。
        <br>这里只生成地址，不会替你上传文件。密钥也不要填在这里 —— 服务端不保存对象存储密钥。
      </div>
    `, { sub: '生成更新包的外部下载前缀' })}

    <div style="height:16px"></div>

    <div class="grid cols-2">
      ${card('实时协作', `
        <form id="liveForm">
          <div class="form-grid one">
            <div class="field"><label>地址</label><input type="text" name="host" value="${esc(live ? live.host : '')}" placeholder="留空 = 与主服务同源"><span class="hint">${esc(live ? (live.sameOrigin ? '当前与主服务同源' : '当前指向独立地址') : '服务未运行，暂时读取不到')}</span></div>
            <div class="field"><label>端口</label><input type="text" name="port" value="${esc(live ? live.port : '')}" placeholder="留空 = 与主服务同端口"></div>
          </div>
          <div class="row end" style="margin-top:14px"><button class="btn" type="submit" ${live ? '' : 'disabled'}>保存</button></div>
        </form>`, { sub: '长连接协作' })}
      ${card('部署令牌', `
        <div class="field" style="margin-bottom:12px"><label>当前令牌</label>
          <input type="text" class="mono" readonly value="${esc(token && token.token || '（未设置）')}">
        </div>
        <div class="row">
          <button class="btn" data-act="token-new">生成新令牌</button>
          <button class="btn" data-act="token-copy">复制</button>
        </div>
        <div class="small muted" style="margin-top:10px">用于 <span class="mono">POST /api/deploy/server</span> 上传新的服务端 exe。轮换后旧令牌立即失效。</div>`, { sub: token && token.path || '' })}
    </div>

    <div style="height:16px"></div>

    ${card('关于', `
      <div class="kv">
        <div class="k">服务端版本</div><div class="v">${esc(st.panel.version)}</div>
        <div class="k">面板版本</div><div class="v">${esc(st.panel.version)}</div>
        <div class="k">配置文件</div><div class="v mono">${esc(st.panel.config_path)}</div>
        <div class="k">数据目录</div><div class="v mono">${esc(st.panel.data_dir)}</div>
        <div class="k">证书目录</div><div class="v mono">${esc(st.panel.cert_dir)}</div>
        <div class="k">运行平台</div><div class="v">${esc(st.panel.os)} / ${esc(st.panel.arch)}</div>
      </div>
      <div class="small muted" style="margin-top:14px">本项目以 MIT 许可证开源。服务端只做同步、分享与权限分发，不保存用户密码原文，不上传日程内容到第三方。</div>`)}
  `;
}

async function mountSettings() {
  const view = $('#view');

  $('#bizForm').onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const body = {
      weather_place: String(fd.get('weather_place') || '').trim(),
      weather_lat: Number(fd.get('weather_lat') || 0),
      weather_lon: Number(fd.get('weather_lon') || 0),
      cos_base: String(fd.get('cos_base') || '').trim(),
      market_url: String(fd.get('market_url') || '').trim(),
      invite_code: String(fd.get('invite_code') || '').trim(),
    };
    try {
      await pput('/config', body);
      if (S.state.server.running) {
        await sput('/api/weather/location', {
          place: body.weather_place, lat: body.weather_lat, lon: body.weather_lon,
        });
      }
      toast('已保存', 'ok');
      await navigate();
    } catch (err) { fail(err); }
  };

  const liveForm = $('#liveForm');
  if (liveForm) {
    liveForm.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(e.target);
      try {
        await spost('/api/admin/live', {
          host: String(fd.get('host') || '').trim(),
          port: String(fd.get('port') || '').trim(),
        });
        toast('已保存', 'ok');
        await navigate();
      } catch (err) { fail(err); }
    };
  }

  view.onclick = async (e) => {
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    if (btn.dataset.act === 'token-new') {
      if (!(await confirmBox({ title: '生成新部署令牌', text: '旧令牌会立即失效，正在使用它的部署脚本需要更新。', danger: true, ok: '生成' }))) return;
      try { const r = await ppost('/deploy-token'); toast('新令牌已生成', 'ok'); await navigate(); console.log(r); }
      catch (err) { fail(err); }
    }
    if (btn.dataset.act === 'token-copy') {
      const input = btn.closest('.card').querySelector('input[readonly]');
      try { await navigator.clipboard.writeText(input.value); toast('已复制', 'ok'); }
      catch (err) { toast('复制失败', 'err'); }
    }
    if (btn.dataset.act === 'cos-build') {
      const bucket = String(($('#cosBucket') || {}).value || '').trim();
      const region = String(($('#cosRegion') || {}).value || '').trim();
      const cdn = String(($('#cosCdn') || {}).value || '').trim().replace(/^https?:\/\//i, '').replace(/\/+$/, '');
      let base = '';
      if (cdn) {
        base = 'https://' + cdn;
      } else if (bucket && region) {
        base = 'https://' + bucket + '.cos.' + region + '.myqcloud.com';
      }
      if (!base) { toast('填存储桶和地域，或者填 CDN 域名', 'err'); return; }
      const target = $('#bizForm [name=cos_base]');
      if (target) target.value = base;
      try { await navigator.clipboard.writeText(base); } catch (err) {}
      toast('已填入并复制，记得点上面的「保存」', 'ok');
    }
    if (btn.dataset.act === 'cos-clear') {
      ['#cosBucket', '#cosCdn'].forEach((sel) => { const el = $(sel); if (el) el.value = ''; });
      const target = $('#bizForm [name=cos_base]');
      if (target) target.value = '';
    }
  };
}

// ---------------------------------------------------------------- 页面表

const PAGES = {
  overview: { render: renderOverview, mount: mountOverview },
  server: { render: renderServer, mount: mountServer },
  accounts: { render: renderAccounts, mount: mountAccounts },
  moderation: { render: renderModeration, mount: mountModeration },
  distribution: { render: renderDistribution, mount: mountDistribution },
  maintenance: { render: renderMaintenance, mount: mountMaintenance },
  settings: { render: renderSettings, mount: mountSettings },
};

// ---------------------------------------------------------------- 顶栏与启动

$('#btnLogout').onclick = async () => {
  try { await ppost('/logout'); } catch (e) { /* 服务端可能已重启 */ }
  await boot();
};

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && S.state && S.state.session && S.state.session.loggedIn) {
    refreshState().catch(() => {});
  }
});

boot();
})();
