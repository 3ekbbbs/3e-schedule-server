/* 外观标注台插件 · 外壳脚本
 *
 * 与导航那一个同构：标签切换 + 外观包的列表/上传/删除 + 跳过标注台自己的登录。
 * 外观这块功能暂缓迭代，所以这里不做多余的编排，能管理、能分发就够。
 */
(function () {
  'use strict';

  var API = '/plugin/skinstudio/api/';

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
      window.prompt('复制这段内容：', text);
    }
  }

  var tabs = document.querySelectorAll('.pl-tabs button');
  tabs.forEach(function (btn) {
    btn.onclick = function () {
      tabs.forEach(function (b) { b.classList.toggle('on', b === btn); });
      var name = btn.dataset.tab;
      $('#pane-studio').hidden = name !== 'studio';
      $('#pane-packs').hidden = name !== 'packs';
      if (name === 'packs') loadPacks();
    };
  });

  var lastPacks = [];

  function packRow(p) {
    var bits = [];
    if (p.role_count) bits.push(p.role_count + ' 个配色');
    if (p.page_count) bits.push(p.page_count + ' 页');
    if (p.home_blocks) bits.push(p.home_blocks + ' 块布局');
    return '' +
      '<tr>' +
      '  <td><b>' + esc(p.name || p.pack_id) + '</b>' +
      (p.note ? '<div class="pl-src">' + esc(p.note) + '</div>' : '') +
      (bits.length ? '<div class="pl-src">' + esc(bits.join(' · ')) + '</div>' : '') +
      '</td>' +
      '  <td><code>' + esc(p.pack_id) + '</code></td>' +
      '  <td>v' + esc(p.version || '—') + '</td>' +
      '  <td>' + (p.dark ? '深色' : '浅色') + '</td>' +
      '  <td>' + (p.size ? fmtBytes(p.size) : '—') + '</td>' +
      '  <td>' + (p.downloads || 0) + '</td>' +
      '  <td>' + fmtTime(p.updated_at) + '</td>' +
      '  <td>' +
      '    <button class="pl-btn sm" data-act="copy" data-id="' + esc(p.pack_id) + '">复制授权码</button> ' +
      '    <button class="pl-btn sm danger" data-act="del" data-id="' + esc(p.pack_id) + '">删除</button>' +
      '  </td>' +
      '</tr>';
  }

  async function loadPacks() {
    var body = $('#plPacksBody');
    body.innerHTML = '<div class="pl-empty">加载中…</div>';
    try {
      var d = await api('admin/skinpacks');
      var packs = (d && d.packs) || [];
      lastPacks = packs;
      if (!packs.length) {
        body.innerHTML = '<div class="pl-empty">还没有外观包。到「标注台」页做一套，或者在下面传一份。</div>';
        return;
      }
      body.innerHTML =
        '<table class="pl-table"><thead><tr>' +
        '<th>名称</th><th>授权码</th><th>版本</th><th>明暗</th><th>大小</th><th>下载</th><th>更新时间</th><th>操作</th>' +
        '</tr></thead><tbody>' + packs.map(packRow).join('') + '</tbody></table>';
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
    if (act === 'copy') { copy(p.pack_id, '授权码已复制'); return; }
    if (act === 'del') {
      if (!confirm('删除「' + (p.name || p.pack_id) + '」？\n\n还在用这个授权码的人下次取包会失败。')) return;
      btn.disabled = true;
      try {
        await api('admin/skinpacks/' + encodeURIComponent(id), { method: 'DELETE' });
        toast('已删除');
        loadPacks();
      } catch (e) {
        toast(e.message, 'err');
        btn.disabled = false;
      }
    }
  });

  $('#plPackUpload').onclick = async function () {
    var input = $('#plPackFile');
    var file = input.files && input.files[0];
    if (!file) { toast('先选一个 .skinpack.json 文件', 'err'); return; }
    var replace = $('#plPackReplace').checked;
    var btn = this;
    btn.disabled = true;
    btn.textContent = '上传中…';
    try {
      var text = await file.text();
      var doc;
      try { doc = JSON.parse(text); }
      catch (e) { throw new Error('这个文件不是合法的 JSON'); }
      if (!doc || (doc.format !== 'schedule.skinpack' && doc.format !== 'threees.skinpack')) {
        throw new Error('不是 skinpack 文件（format 字段应该是 schedule.skinpack）');
      }
      await api('admin/skinpacks/upload' + (replace ? '?replace=1' : ''), { body: doc });
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

  try {
    if (!localStorage.getItem('skin-studio.pubtoken')) {
      localStorage.setItem('skin-studio.pubtoken', 'panel-session');
    }
  } catch (e) { /* 隐私模式 */ }

  loadPacks();
})();
