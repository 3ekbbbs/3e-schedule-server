/* 导航标注台插件 · 外壳脚本
 *
 * 只做三件事：
 *   1. 标注 / 数据包 两个面板之间切换；
 *   2. 数据包的列表、上传、删除；
 *   3. 让标注台那页**不用再登录一次**。
 *
 * 第 3 点的做法值得说明：标注台原本是独立页面，发布前往 `/api/auth/login`
 * 要一对管理员账号密码。装进面板之后，面板已经有会话了，再让人登一次
 * 纯属多余。这里在同一个源上把它的令牌键填一个占位值，让它跳过登录表单；
 * 真正带上去的 Authorization 会被面板的 /plugin/<id>/api 代理**覆盖**成
 * 当前会话的令牌 —— 占位值不会到达服务端，也不会被浏览器存成真令牌。
 */
(function () {
  'use strict';

  var API = '/plugin/navstudio/api';

  // ---- 小工具 ----

  function $(sel, root) { return (root || document).querySelector(sel); }

  function esc(v) {
    return String(v == null ? '' : v).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  var toastTimer = null;
  function toast(msg, kind) {
    var el = $('#plToast');
    el.textContent = msg;
    el.className = 'pl-toast show' + (kind === 'err' ? ' err' : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.className = 'pl-toast'; }, 2600);
  }

  function fmtBytes(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(1) + ' MB';
  }

  function fmtTime(sec) {
    if (!sec) return '—';
    var d = new Date(sec * 1000);
    if (isNaN(d.getTime())) return '—';
    var p = function (x) { return x < 10 ? '0' + x : String(x); };
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  async function api(path, opt) {
    opt = opt || {};
    var res = await fetch(API + path, {
      method: opt.method || 'GET',
      headers: Object.assign(
        opt.body ? { 'Content-Type': 'application/json' } : {},
        opt.headers || {}
      ),
      body: opt.body ? JSON.stringify(opt.body) : undefined,
    });
    var text = await res.text();
    var data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
    if (!res.ok) {
      if (res.status === 401) throw new Error('面板会话过期了，回到面板重新登录一次');
      throw new Error((data && (data.error || data.message)) || ('HTTP ' + res.status));
    }
    return data;
  }

  async function copy(text, okMsg) {
    try {
      await navigator.clipboard.writeText(text);
      toast(okMsg || '已复制');
    } catch (e) {
      // 非安全上下文（http + 非 localhost）里 clipboard 不可用，退回到选中输入框。
      window.prompt('复制这段内容：', text);
    }
  }

  // ---- 标签切换 ----

  var tabs = document.querySelectorAll('.pl-tabs button');
  var hint = $('#plHint');
  var hintStudio = hint.textContent;
  tabs.forEach(function (btn) {
    btn.onclick = function () {
      tabs.forEach(function (b) { b.classList.toggle('on', b === btn); });
      var name = btn.dataset.tab;
      $('#pane-studio').hidden = name !== 'studio';
      $('#pane-packs').hidden = name !== 'packs';
      hint.textContent = name === 'packs'
        ? '授权码就是包标识。同学导入之后，本机原来的楼栋表会被整份替换'
        : hintStudio;
      if (name === 'packs') loadPacks();
    };
  });

  // ---- 数据包列表 ----

  function packRow(p) {
    var bits = [];
    if (p.with_coord < p.buildings) bits.push((p.buildings - p.with_coord) + ' 栋没坐标');
    if (p.floors) bits.push(p.floors + ' 栋有分层');
    if (p.rooms) bits.push(p.rooms + ' 间房');
    return '' +
      '<tr>' +
      '  <td><b>' + esc(p.name || p.pack_id) + '</b>' +
      (p.note ? '<div class="pl-src">' + esc(p.note) + '</div>' : '') +
      (bits.length ? '<div class="pl-src">' + esc(bits.join(' · ')) + '</div>' : '') +
      '</td>' +
      '  <td><code>' + esc(p.pack_id) + '</code></td>' +
      '  <td>v' + esc(p.version || '—') + '</td>' +
      '  <td>' + (p.buildings || 0) + ' 栋</td>' +
      '  <td>' + (p.size ? fmtBytes(p.size) : '—') + '</td>' +
      '  <td>' + (p.downloads || 0) + '</td>' +
      '  <td>' + fmtTime(p.updated_at) +
      (p.key_carried ? '<div><span class="pl-badge warn">带 Key</span></div>' : '') +
      '</td>' +
      '  <td>' +
      '    <button class="pl-btn sm" data-act="copy" data-id="' + esc(p.pack_id) + '">复制授权码</button> ' +
      '    <button class="pl-btn sm" data-act="note" data-id="' + esc(p.pack_id) + '">复制说明</button> ' +
      '    <button class="pl-btn sm danger" data-act="del" data-id="' + esc(p.pack_id) + '">删除</button>' +
      '  </td>' +
      '</tr>';
  }

  var lastPacks = [];

  async function loadPacks() {
    var body = $('#plPacksBody');
    body.innerHTML = '<div class="pl-empty">加载中…</div>';
    try {
      var d = await api('/admin/navpacks');
      var packs = (d && d.packs) || [];
      lastPacks = packs;
      if (!packs.length) {
        body.innerHTML = '<div class="pl-empty">还没有发布过数据包。到「标注」页标完楼，点右上角「一键发布」。</div>';
        return;
      }
      body.innerHTML =
        '<table class="pl-table"><thead><tr>' +
        '<th>名称</th><th>授权码</th><th>版本</th><th>楼栋</th><th>大小</th><th>下载</th><th>更新时间</th><th>操作</th>' +
        '</tr></thead><tbody>' + packs.map(packRow).join('') + '</tbody></table>' +
        '<div class="pl-src">' + esc((d && d.auth_note) || '') + '</div>';
    } catch (e) {
      body.innerHTML = '<div class="pl-empty">' + esc(e.message) + '</div>';
    }
  }

  document.addEventListener('click', async function (ev) {
    var btn = ev.target.closest && ev.target.closest('[data-act]');
    if (!btn) return;
    var id = btn.dataset.id;
    var act = btn.dataset.act;
    var p = lastPacks.filter(function (x) { return x.pack_id === id; })[0];
    if (!p) return;

    if (act === 'copy') {
      copy(p.pack_id, '授权码已复制');
      return;
    }
    if (act === 'note') {
      copy(
        '打开「导航」插件 → 从云端导入 → 输入授权码：' + p.pack_id +
        '\n（' + (p.name || '') + '，v' + (p.version || '') + '）',
        '导入说明已复制，可以直接发给同学'
      );
      return;
    }
    if (act === 'del') {
      if (!confirm('删除「' + (p.name || p.pack_id) + '」？\n\n删掉之后，还在用这个授权码的人下次导入会取不到包。')) return;
      btn.disabled = true;
      try {
        await api('/admin/navpacks/' + encodeURIComponent(id), { method: 'DELETE' });
        toast('已删除');
        loadPacks();
      } catch (e) {
        toast(e.message, 'err');
        btn.disabled = false;
      }
    }
  });

  // ---- 上传 ----

  $('#plPackUpload').onclick = async function () {
    var input = $('#plPackFile');
    var file = input.files && input.files[0];
    if (!file) { toast('先选一个 .navpack.json 文件', 'err'); return; }
    var replace = $('#plPackReplace').checked;
    var btn = this;
    btn.disabled = true;
    btn.textContent = '上传中…';
    try {
      var text = await file.text();
      var doc;
      try { doc = JSON.parse(text); }
      catch (e) { throw new Error('这个文件不是合法的 JSON'); }
      if (!doc || (doc.format !== 'schedule.navpack' && doc.format !== 'threees.navpack')) {
        throw new Error('不是 navpack 文件（format 字段应该是 schedule.navpack）');
      }
      await api('/admin/navpacks/upload' + (replace ? '?replace=1' : ''), { body: doc });
      toast('已上传：' + (doc.packId || ''));
      input.value = '';
      loadPacks();
    } catch (e) {
      toast(e.message, 'err');
    } finally {
      btn.disabled = false;
      btn.textContent = '上传';
    }
  };

  // ---- 让标注台跳过它自己的登录 ----
  //
  // 只在没有值时写。真正的鉴权由面板的代理完成，这个值只是"让界面认为
  // 已经登录过了"，它不会到达服务端。
  try {
    if (!localStorage.getItem('ns.pubtoken')) {
      localStorage.setItem('ns.pubtoken', 'panel-session');
    }
  } catch (e) { /* 隐私模式下 localStorage 可能不可用，不影响其它功能 */ }

  // 首屏就把列表拉一次：用户先点「数据包」时不用等。
  loadPacks();
})();
