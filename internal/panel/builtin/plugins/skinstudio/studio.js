'use strict';
/*
 * 外观设计台 · 交互层 v3（2026-10-14：拖拽布局编辑器）。
 *
 * ## 这一版的交互模型（Photoshop 那类编辑器的工作流）
 *
 * ```
 *   左栏大纲    整机解剖 + 页面树（每页的块）—— 块拖到某页上 = 移到那一页
 *   中间画布    393×852 手机模拟器 —— 点哪个块选中哪个，拖拽直接换位
 *   右栏检查器  跟着选中对象走 —— 块 = 跨度/高度/配色/圆角；配色 = 27 角色
 * ```
 *
 * ## 三层职责（改代码时按这三层找）
 * ```
 *   skin-core.js    颜色怎么算 + 块的形状怎么归一（必须和手机端一致）
 *   skin-studio.js 皮肤怎么被编辑、怎么被打包（本文件）
 *   skin-studio.css 手机画面长什么样（.ph-* 那一堆）
 * ```
 *
 * ## 和手机端"不要有差异"在这里靠四件事
 * ① 颜色算法同一套（core）；
 * ② 手机画面的尺寸/圆角/字号全部走 CSS 变量，变量值来自皮肤；
 * ③ 页面 id 用注册表里那个 id（`PAGES`）；
 * ④ **布局块的类型、字段、夹范围、白名单全部对齐手机端**：
 *    主页块 = `HomeBlock`（span 1~2 / heightFactor 0.3~4 / fontSize 0~48 …），
 *    页面块 = `PageBlock`（6 种 kind / 每页最多 8 块 / actions 走白名单 /
 *    links 必须 http 开头 —— valid 判据和 `PageBlock.parse` 一字不差）。
 *    ⚠️ 内容是**样例数据** —— 设计台不连数据库，这一点在界面上明说了。
 *
 * ## 改动的两条通道（一体化的根基）
 * · `commit(label, fn)` —— 离散动作（按钮、下拉、跨页移动）：进历史 → 置脏 → 存盘 → **全量重画**。
 * · `live(label, fn)`   —— 连续输入（取色器拖动、滑杆、打字）：进历史（**700ms 内同标签合并成一步**，
 *   一拖 = 一次撤销）→ 重画手机/大纲/顶栏，**不重建检查器**（重建会打断正在拖的取色器）。
 */

const C = window.SkinCore;
const FC = window.FreeCanvas;      // ★ v5：自由画布引擎（free-canvas.js，本文件之前加载）
const LS_KEY = 'skin-studio.v2';
const LS_KEY_V1 = 'skin-studio.v1';   // v1 存档迁移用（v1 里没有 homeLayout，读进来自然是 null）
const TOKEN_KEY = 'skin-studio.pubtoken';
const SERVER = '';

// ============================================================ 状态

let S = {
  skin: C.emptySkin(),
  seed: '#2F6FB5',
  dark: false,
  pageId: 'home',           // 正在预览 / 正在设计的页面
  drawer: false,            // 是否把抽屉拉出来看（竖屏主页下拉）
  subTab: 'list',           // 日程那类有子标签的页，预览用
  land: false,              // 横屏预览（= LandscapeShell 左栏+内容+右栏那套）
  landMenu: false,          // 横屏树状菜单开着没有（切页即收，源码同款）
  srcOn: false,             // 源码渲染层开着没有（⌨；只读预览，不持久化）
  // ★ v5：编辑方式。`free` = 自由画布（默认，门槛低的那条路），`pro` = 上一版的块流。
  //   老设计**一样不丢** —— 两套数据（freeLayers / homeLayout+pageBlocks）并存，
  //   导出时按"这一页有没有自由画布"决定用哪套（见 buildPack）。
  mode: 'free',
  pack: { packId: '', name: '', version: '1.0.0', note: '', author: '' },
};

/** 自由画布的编辑器实例（懒建；切到 pro 模式时**不销毁**，切回来接着用）。 */
let FCED = null;
/** 代码模态当前编辑的是哪一页。 */
let FC_CODE_PAGE = '';

/** 当前选中的对象。null | {type:'theme'|…} | {type:'page',pageId} | {type:'hblock',id} | {type:'pblock',pageId,at,index} */
let SEL = null;

let HIST = [];        // 快照栈（{s: JSON, label}）
let HIDX = -1;        // 当前位置
let DIRTY = false;    // 有未导出/未发布的改动
let COLLAPSED = {};   // 检查器分节折叠状态
let ROLE_Q = '';      // 配色角色的搜索词
let AUTHCODE = '';    // 上一次发布成功的授权码
let OLOPEN = {};      // 大纲里哪些页展开了（默认展开当前页）
let DRAG = null;      // 拖拽中：{from:'home',id} | {from:'page',pageId,at,index,fullIdx}
let BLK_SEQ = 1;      // 块 id 序列（保证主页块 id 唯一，跨页移动后定位用）

function save() {
  try {
    localStorage.setItem(LS_KEY, JSON.stringify({
      skin: S.skin, seed: S.seed, dark: S.dark, land: S.land,
      pageId: S.pageId, mode: S.mode, pack: S.pack,
    }));
  } catch (e) { /* 隐私模式下写不了就算了，不影响用 */ }
}
function load() {
  try {
    let raw = localStorage.getItem(LS_KEY);
    if (!raw) raw = localStorage.getItem(LS_KEY_V1); // v1 存档迁移：没有布局，其余照常
    if (!raw) return;
    const o = JSON.parse(raw);
    S.skin = C.normalizeSkin(o.skin);
    if (typeof o.seed === 'string') S.seed = o.seed;
    S.dark = !!o.dark;
    if (typeof o.land === 'boolean') S.land = o.land;
    if (o.mode === 'pro' || o.mode === 'free') S.mode = o.mode;
    if (C.PAGE_BY_ID[o.pageId]) S.pageId = o.pageId;
    if (o.pack && typeof o.pack === 'object') Object.assign(S.pack, o.pack);
  } catch (e) { /* 坏存档当没有：设计台不该因为一份坏 localStorage 打不开 */ }
}

// ============================================================ 统一改动入口

function snap() { return JSON.stringify({ skin: S.skin, seed: S.seed, dark: S.dark }); }

/** 记一步历史（离散）。上限 120 步。 */
function pushHist(label) {
  HIST = HIST.slice(0, HIDX + 1);
  HIST.push({ s: snap(), label });
  if (HIST.length > 120) HIST.shift();
  HIDX = HIST.length - 1;
}

/**
 * 记一步历史（连续输入）。**同标签 700ms 内合并**：拖一次滑杆 = 一次撤销，
 * 而不是拖的过程中每 10ms 一步 —— 否则撤销会"永远撤不完"。
 */
let LAST_LIVE = { label: '', at: 0 };
function pushHistLive(label) {
  const now = Date.now();
  if (HIST.length && HIDX === HIST.length - 1 && LAST_LIVE.label === label && now - LAST_LIVE.at < 700) {
    HIST[HIDX] = { s: snap(), label };
  } else {
    pushHist(label);
  }
  LAST_LIVE = { label, at: now };
}

function restore(e) {
  const o = JSON.parse(e.s);
  S.skin = C.normalizeSkin(o.skin);
  S.seed = o.seed; S.dark = o.dark;
  DIRTY = true;
  LAST_LIVE = { label: '', at: 0 };
  save(); paint();
  // ★ v5：撤销/重做也要把**画布**倒回去（历史是统一的，画布数据在 skin.freeLayers 里）。
  if (S.mode === 'free') freeLoad();
}
function undo() {
  if (HIDX <= 0) { toast('没有可撤销的了'); return; }
  HIDX--; restore(HIST[HIDX]);
  toast('撤销：' + (HIST[HIDX].label || '改一步'), 'ok', 900);
}
function redo() {
  if (HIDX >= HIST.length - 1) { toast('没有可重做的了'); return; }
  HIDX++; restore(HIST[HIDX]);
  toast('重做：' + (HIST[HIDX].label || '改一步'), 'ok', 900);
}

/** 离散改动：fn → 历史 → 置脏 → 存盘 → 全量重画（含检查器重建）。 */
function commit(label, fn) {
  fn(S.skin);
  DIRTY = true;
  pushHist(label);
  save();
  pushRenderData();
  paint();
}
/** 连续输入改动：不重建检查器（正在拖的取色器不能被打断），其余照常。 */
function live(label, fn) {
  fn(S.skin);
  DIRTY = true;
  pushHistLive(label);
  save();
  pushRenderData();
  // ⚠️ 自由模式下**不**重画手机模拟器（它在另一套 DOM 里、这会儿是 hidden）——
  //    白画一遍既费时间，又会把源码渲染层那份数据推来推去。
  if (S.mode === 'free') { freeSyncColors(); renderLayersPane(); syncChrome(); }
  else { renderPhone(); renderOutline(); syncChrome(); refreshDerived(); }
}
function patchSkin(fn, label) { commit(label || '改一项设置', fn); }
function markClean() { DIRTY = false; save(); syncChrome(); }

/**
 * 全量重画。**按模式分发**（v5）：
 * · `free` —— 配色同步给画布 + 重画左右两栏 + 顶栏。**不重载画布数据**
 *   （重载会把用户正在拖/正在输入的状态清掉；要重载请显式调 `freeLoad()`）。
 * · `pro`  —— 手机模拟器 + 大纲 + 检查器 + 顶栏（= v3/v4 的老行为）。
 */
function paint() {
  if (S.mode === 'free') { freeSyncColors(); paintFreePanels(); syncChrome(); return; }
  renderPhone(); renderOutline(); renderInspector(); syncChrome();
}
/** 兼容旧调用点（applyPackText 用）。 */
function renderPanel() { renderOutline(); renderInspector(); }

/** 提示统一从底部冒。 */
function toast(msg, kind, ms) {
  const box = $('#ss-toasts');
  if (!box) return;
  const t = el('div', 'ss-toast' + (kind ? ' ' + kind : ''), msg);
  box.appendChild(t);
  setTimeout(() => { t.style.opacity = '0'; t.style.transition = 'opacity .2s'; }, (ms || 1900) - 200);
  setTimeout(() => t.remove(), ms || 1900);
}

/** 顶栏状态：脏标记 / 撤销重做 / 选择面包屑 —— 全部从状态派生。 */
function syncChrome() {
  const d = $('#ss-dirty');
  if (d) {
    d.textContent = DIRTY ? '有未导出的改动' : '已同步';
    d.className = 'ss-dirty' + (DIRTY ? '' : ' clean');
  }
  const u = $('#ss-undo'), r = $('#ss-redo');
  if (u) u.disabled = HIDX <= 0;
  if (r) r.disabled = HIDX >= HIST.length - 1;
  const p = $('#ss-selpath');
  if (p) p.textContent = (S.mode === 'free') ? freePathText() : selPathText();
  setDevLabel();
}

/** 手机框下面那行字：横竖屏尺寸，开了源码渲染还要说明那是**真源码**。 */
function setDevLabel() {
  const el = $('#ss-devlabel');
  if (!el) return;
  const size = S.land ? '852×393' : '393×852';
  el.textContent = S.srcOn ? '源码渲染（手机端同一份 Compose）· ' + size : size;
}

/**
 * 开 / 关源码渲染层（⌨）。
 *
 * ⚠️ iframe 的 `src` **只赋一次**：之后靠 wasm 侧轮询 localStorage 跟上。
 *    重设 src = 整页重载 wasm（一两秒 + 闪一下），"边拖边看"就废了。
 */
function setSrcView(on) {
  S.srcOn = !!on;
  const wrap = $('#ss-srcwrap'), fr = $('#ss-srcframe'), btn = $('#ss-srcview');
  if (!wrap || !fr) return;
  if (S.srcOn) {
    try { localStorage.setItem('shellRenderData', JSON.stringify(shellRenderPayload())); }
    catch (e) { toast('写渲染数据失败：' + e.message, 'bad'); }
    if (!fr.getAttribute('src')) fr.setAttribute('src', 'shell-render/');
    wrap.hidden = false;
  } else {
    wrap.hidden = true;
  }
  if (btn) btn.classList.toggle('on', S.srcOn);
  setDevLabel();
}

function selPathText() {
  if (!SEL) return '未选中 — 点画布或左侧大纲';
  if (SEL.type === 'page') return '页面 › ' + C.PAGE_BY_ID[SEL.pageId].label;
  if (SEL.type === 'hblock') return '主页块 › ' + hblockLabel(homeBlockById(SEL.id));
  if (SEL.type === 'pblock') {
    const b = pblockOf(SEL);
    return C.PAGE_BY_ID[SEL.pageId].label + ' › ' + (SEL.at === 'top' ? '顶部' : '底部') + '块 › ' + (b ? C.PAGE_BLOCK_LABEL[b.kind] : '');
  }
  const n = OUTLINE.find((x) => x.id === SEL.type);
  return '整机 › ' + (n ? n.label : SEL.type);
}

/** 起手式：先让人一眼看出"这个设计台能改到什么程度"。 */
const PRESETS = [
  { t: '极简白', d: '默认那副，最耐看', seed: '#2F6FB5', dark: false, patch: (sk) => { sk.decor = 'none'; } },
  { t: '深夜蓝', d: '深色 + 圆角收一点', seed: '#2F6FB5', dark: true, patch: (sk) => { sk.decor = 'dots'; sk.cornerScale = 0.8; } },
  { t: '糖果圆润', d: '大圆角 + 爱心纹理', seed: '#D4537E', dark: false, patch: (sk) => { sk.cardRadius = 24; sk.cornerScale = 1.6; sk.decor = 'hearts'; } },
  { t: '像素硬朗', d: '直角 + 星点 + 无衬线', seed: '#1D9E75', dark: true, patch: (sk) => { sk.cardRadius = 2; sk.cornerScale = 0; sk.decor = 'stars'; sk.fontFamily = 'monospace'; sk.iconShape = 'square'; } },
  { t: '墨绿纸张', d: '衬线 + 网格纸感', seed: '#5F7A5A', dark: false, patch: (sk) => { sk.decor = 'grid'; sk.fontFamily = 'serif'; sk.letterSpacing = 0.01; } },
  { t: '高对比', d: '无障碍取向：字更实、线更清', seed: '#101418', dark: true, patch: (sk) => { sk.decor = 'none'; sk.fontWeight = 'medium'; sk.colors = { primaryContainer: '#2B3A55', onPrimaryContainer: '#FFFFFF' }; } },
];
function applyPreset(p) {
  commit('套用预设「' + p.t + '」', (sk) => {
    S.seed = p.seed; S.dark = p.dark;
    const base = C.emptySkin();
    for (const k of Object.keys(base)) sk[k] = base[k];
    p.patch(sk);
  });
  const cb = $('#ss-dark'); if (cb) cb.checked = S.dark;
  toast('已套用「' + p.t + '」', 'ok');
}

// ============================================================ 小工具

const $ = (s) => document.querySelector(s);
// 第 4 参（html）会追加为 innerHTML —— h3 里塞 <span class="ss-tag"> 用。
// ⚠️ v1 的 el 只收 3 参，那些第 4 参被**静默丢弃**（tag 徽章全不显示）—— v3 补上。
const el = (tag, cls, txt, html) => {
  const d = document.createElement(tag);
  if (cls) d.className = cls;
  if (txt != null) d.textContent = txt;
  if (html) d.innerHTML += html;
  return d;
};
const esc = (s) => String(s == null ? '' : s).replace(/[&<>"]/g, (m) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[m]));

function row(label, node) {
  const r = el('div', 'ss-row');
  r.appendChild(el('label', null, label));
  const f = el('div', 'ss-field');
  if (typeof node === 'string') f.innerHTML = node; else f.appendChild(node);
  r.appendChild(f);
  return r;
}
function textInput(value, onInput, placeholder) {
  const i = document.createElement('input');
  i.type = 'text'; i.value = value || ''; if (placeholder) i.placeholder = placeholder;
  i.oninput = () => onInput(i.value);
  return i;
}
function numInput(value, min, max, step, onInput) {
  const i = document.createElement('input');
  i.type = 'number'; i.value = String(value); i.min = String(min); i.max = String(max); i.step = String(step);
  i.oninput = () => { const v = parseFloat(i.value); onInput(isNaN(v) ? min : v); };
  return i;
}
function slider(value, min, max, step, onInput) {
  const wrap = el('div', 'ss-field');
  const r = document.createElement('input');
  r.type = 'range'; r.min = String(min); r.max = String(max); r.step = String(step); r.value = String(value);
  const v = el('span', 'ss-val', String(value));
  r.oninput = () => { v.textContent = r.value; onInput(parseFloat(r.value)); };
  wrap.appendChild(r); wrap.appendChild(v);
  return wrap;
}
function select(value, options, onChange) {
  const s = document.createElement('select');
  for (const [val, label] of options) {
    const o = document.createElement('option');
    o.value = val; o.textContent = label;
    if (val === value) o.selected = true;
    s.appendChild(o);
  }
  s.onchange = () => onChange(s.value);
  return s;
}
function colorPicker(value, onPick, allowEmpty) {
  const wrap = el('div', 'ss-field');
  const i = document.createElement('input');
  i.type = 'color'; i.value = value || '#000000';
  i.oninput = () => { t.value = i.value; onPick(i.value); };
  wrap.appendChild(i);
  const t = textInput(value, (v) => onPick(v.trim()), allowEmpty ? '空=跟随主题' : '#RRGGBB');
  t.style.maxWidth = '110px';
  wrap.appendChild(t);
  if (allowEmpty) {
    const b = el('button', 'tiny', '清除');
    b.onclick = () => onPick('');
    wrap.appendChild(b);
  }
  return wrap;
}
function check(value, onChange) {
  const wrap = el('div', 'ss-field');
  const i = document.createElement('input');
  i.type = 'checkbox'; i.checked = !!value;
  i.onchange = () => onChange(i.checked);
  wrap.appendChild(i);
  return wrap;
}
/** 数字 + 提示的小控件（"0 = 默认"那类字段）。 */
function numHint(value, min, max, step, onInput, hint) {
  const wrap = el('div', 'ss-field');
  wrap.appendChild(numInput(value, min, max, step, onInput));
  wrap.appendChild(el('span', 'ss-note', hint || ''));
  return wrap;
}

/** 把检查器里所有 `.ss-sec` 统一升级成可折叠的卡（规则只写一处，新分节自动继承）。 */
function decorate(box) {
  [...box.querySelectorAll('.ss-sec')].forEach((sec) => {
    const h = sec.querySelector('h3');
    if (!h) return;
    const key = h.textContent.trim();
    const card = el('div', 'ss-card' + (COLLAPSED[key] ? ' closed' : ''));
    const head = el('div', 'ss-card-h');
    const tag = h.querySelector('.ss-tag');
    head.innerHTML = `<b>${esc(key)}</b>` + (tag ? tag.outerHTML : '') + '<span class="ss-caret">▾</span>';
    head.onclick = () => { COLLAPSED[key] = !COLLAPSED[key]; card.classList.toggle('closed'); };
    sec.removeChild(h);
    const body = el('div', 'ss-card-b');
    while (sec.firstChild) body.appendChild(sec.firstChild);
    card.appendChild(head);
    card.appendChild(body);
    sec.replaceWith(card);
  });
}
/** 27 个角色的筛选框。 */
function wireRoleSearch(box) {
  const rows = [...box.querySelectorAll('.ss-swatch')];
  if (!rows.length) return;
  const wrap = el('div', 'ss-search');
  const i = document.createElement('input');
  i.type = 'text'; i.placeholder = '过滤角色（中文名或英文名，如「卡片」/surface）';
  i.value = ROLE_Q;
  i.oninput = () => {
    ROLE_Q = i.value.trim().toLowerCase();
    rows.forEach((r) => {
      const txt = r.textContent.toLowerCase();
      r.classList.toggle('ss-hidden', !!ROLE_Q && !txt.includes(ROLE_Q));
    });
  };
  wrap.appendChild(i);
  const body = box.querySelector('.ss-card-b');
  if (body) body.insertBefore(wrap, body.firstChild);
  if (ROLE_Q) i.dispatchEvent(new Event('input'));
}

/** 检查器里派生量的就地刷新（live() 不重建检查器，这些量会停在旧值）。 */
function refreshDerived() {
  const box = $('#ss-inspector');
  if (!box) return;
  const col = C.resolveColors(S.skin, S.dark, S.seed);
  box.querySelectorAll('.ss-swatch[data-role]').forEach((line) => {
    const role = line.getAttribute('data-role');
    const chip = line.querySelector('.ss-chip');
    if (chip) chip.style.background = '#' + C.rgbToHex(col[role] || { r: 200, g: 200, b: 200 });
    const name = line.querySelector('.ss-name');
    if (name) {
      const label = line.getAttribute('data-role-label');
      name.textContent = `${label} · ${role}${S.skin.colors[role] ? '' : '（推导）'}`;
    }
  });
  const c1 = C.contrast(col.background, col.onBackground);
  const c2 = C.contrast(col.primary, col.onPrimary);
  const q1 = box.querySelector('[data-contrast="bg"]');
  if (q1) q1.textContent = `正文对比度 ${c1.toFixed(2)}:1${c1 >= 4.5 ? ' ✓' : ' ✗ 不到 4.5'}`;
  const q2 = box.querySelector('[data-contrast="primary"]');
  if (q2) q2.textContent = `主色上的字 ${c2.toFixed(2)}:1${c2 >= 4.5 ? ' ✓' : ' ✗'}`;
  const cb = $('#ss-insp-count');
  if (cb && SEL) cb.textContent = '已改 ' + editedCount(nodeOfSel()) + ' 项';
}

// ============================================================ 皮肤字段操作

function setRole(role, hex) {
  live(hex ? `覆盖配色「${role}」` : `恢复配色「${role}」的推导`, (sk) => {
    if (!hex) delete sk.colors[role];
    else sk.colors[role] = hex;
  });
}
function setPageBg(hex) {
  live(`${C.PAGE_BY_ID[S.pageId].label}·页面底色`, (sk) => {
    if (!hex) delete sk.pageBg[S.pageId];
    else sk.pageBg[S.pageId] = hex;
  });
}
function setPageTitle(v) {
  live(`${C.PAGE_BY_ID[S.pageId].label}·标题`, (sk) => {
    if (!v.trim()) delete sk.pageTitles[S.pageId];
    else sk.pageTitles[S.pageId] = v.trim();
  });
}

// ============================================================ 布局操作（v3 的核心）

// ---- 主页块（HomeLayout / HomeBlock） ----

function homeLayout() { return S.skin.homeLayout; }
/** 取主页布局，没有就先用 SIMPLE 起手（= 手机端「AI 主页」的默认那套）。 */
function homeEnsure() {
  if (!S.skin.homeLayout) S.skin.homeLayout = JSON.parse(JSON.stringify(C.HOME_LAYOUT_SIMPLE));
  return S.skin.homeLayout;
}
function homeBlockById(id) {
  return (homeLayout() && homeLayout().blocks.find((b) => b.id === id)) || null;
}
function newBlkId(prefix) { return prefix + '-' + (BLK_SEQ++) + '-' + Date.now().toString(36).slice(-3); }
function hblockLabel(b) {
  if (!b) return '';
  if (b.type === 'widget') { const w = C.HOME_WIDGET_BY_ID[b.widgetId]; return (w ? w.label : b.widgetId || '卡片'); }
  if (b.type === 'heading') return b.text ? b.text.slice(0, 10) : '标题';
  if (b.type === 'text') return b.text ? b.text.slice(0, 10) : '文字';
  if (b.type === 'divider') return '分隔线';
  if (b.type === 'spacer') return '留白 ' + (b.height || 12);
  if (b.type === 'html') return 'HTML 块';
  return b.type;
}
function homeAddBlock(type) {
  commit('主页加「' + C.HOME_BLOCK_LABEL[type] + '」块', () => {
    const hl = homeEnsure();
    const b = C.normalizeHomeBlock({
      type, id: newBlkId(type[0]),
      widgetId: type === 'widget' ? 'weather' : '',
      text: type === 'heading' ? '标题' : type === 'text' ? '一段说明文字' : '',
      height: type === 'spacer' ? 24 : 0,
      html: type === 'html' ? '<div style="font-size:13px;padding:6px">这里是 HTML —— 颜色形状不受组件库限制</div>' : '',
    });
    hl.blocks.push(b);
    SEL = { type: 'hblock', id: b.id };
  });
}
function homeMoveBlock(id, toIndex) {
  const hl = homeLayout(); if (!hl) return;
  const from = hl.blocks.findIndex((b) => b.id === id);
  if (from < 0) return;
  let idx = toIndex;
  if (from < idx) idx--;                    // 先移除再插入的经典校正
  idx = Math.max(0, Math.min(hl.blocks.length - 1, idx));
  if (from === idx) return;
  commit('主页块换位', () => {
    const [b] = hl.blocks.splice(from, 1);
    hl.blocks.splice(idx, 0, b);
  });
}
function homeDupBlock(id) {
  const hl = homeLayout(); if (!hl) return;
  const at = hl.blocks.findIndex((b) => b.id === id);
  if (at < 0) return;
  let newId = null;
  commit('复制主页块', () => {
    const copy = JSON.parse(JSON.stringify(hl.blocks[at]));
    copy.id = newBlkId(copy.type[0]);
    hl.blocks.splice(at + 1, 0, copy);
    newId = copy.id;
  });
  if (newId) selectHBlock(newId);
}
function homeDelBlock(id) {
  commit('删主页块', (sk) => {
    const hl = homeLayout(); if (!hl) return;
    hl.blocks = hl.blocks.filter((b) => b.id !== id);
    if (!hl.blocks.length) sk.homeLayout = null;   // 清空 = 回"不动主页"
  });
  if (SEL && SEL.type === 'hblock' && SEL.id === id) SEL = null;
}
function homeSetBlockField(id, field, value, label) {
  live(`主页块·${label || field}`, () => {
    const b = homeBlockById(id); if (!b) return;
    b[field] = value;
  });
}
function homeSetLayoutField(field, value, label) {
  live(`主页·${label || field}`, () => {
    const hl = homeEnsure();
    hl[field] = value;
  });
}
/** 主页块 → 页面块（只有三种能转：heading / text / divider）。 */
function homeBlockToPageBlock(b) {
  if (!b) return null;
  if (b.type === 'heading') return { at: 'top', kind: 'heading', text: b.text, items: [] };
  if (b.type === 'text') return { at: 'top', kind: 'text', text: b.text, items: [] };
  if (b.type === 'divider') return { at: 'top', kind: 'divider', text: '', items: [] };
  return null;   // widget / spacer / html 在页面块体系里没有对应物
}
/** 页面块 → 主页块（同样只有三种）。 */
function pageBlockToHomeBlock(b) {
  if (!b) return null;
  if (b.kind === 'heading') return C.normalizeHomeBlock({ type: 'heading', id: newBlkId('h'), text: b.text, pad: 0, fontSize: 19 });
  if (b.kind === 'text') return C.normalizeHomeBlock({ type: 'text', id: newBlkId('t'), text: b.text });
  if (b.kind === 'divider') return C.normalizeHomeBlock({ type: 'divider', id: newBlkId('d') });
  return null;   // actions / links / card 转过去会丢按钮 —— 宁可明说不行
}

// ---- 页面块（PageBlock） ----

function pageBlocksOf(pageId) { return S.skin.pageBlocks[pageId] || []; }
function pblockOf(sel) {
  const arr = pageBlocksOf(sel.pageId);
  return (sel.at === 'top' ? arr.filter((b) => b.at === 'top') : arr.filter((b) => b.at === 'bottom'))[sel.index] || null;
}
function pageZoneCount(pageId, at) { return pageBlocksOf(pageId).filter((b) => b.at === at).length; }

function pageAddBlock(pageId, at, kind) {
  if (pageZoneCount(pageId, 'top') + pageZoneCount(pageId, 'bottom') >= 8) {
    toast('每页最多 8 块（手机端的硬上限，超出的会被丢掉）', 'bad', 2600);
    return;
  }
  const label = C.PAGE_BY_ID[pageId].label + (at === 'top' ? '·顶部' : '·底部');
  commit(`${label}加「${C.PAGE_BLOCK_LABEL[kind]}」块`, (sk) => {
    const b = { at, kind, text: kind === 'heading' ? '标题' : kind === 'text' ? '一段说明文字' : '', items: [] };
    if (kind === 'actions') b.items = [['检查更新', 'check_update']];
    if (kind === 'links') b.items = [['官网', 'https://threees.cn']];
    if (kind === 'card') b.text = '一张卡片：标题 + 文字（可加按钮）';
    const arr = sk.pageBlocks[pageId] || (sk.pageBlocks[pageId] = []);
    // at=top 插到所有 top 之后（= 正文之前）；bottom 追加到末尾
    if (at === 'top') {
      let i = 0; while (i < arr.length && arr[i].at === 'top') i++;
      arr.splice(i, 0, b);
      SEL = { type: 'pblock', pageId, at, index: pageZoneCount(pageId, 'top') - 1 };
    } else {
      arr.push(b);
      SEL = { type: 'pblock', pageId, at, index: pageZoneCount(pageId, 'bottom') - 1 };
    }
  });
  openPage(pageId);
}
/**
 * 页内换位（含 top↔bottom 移动）。索引全部用**全量数组**的口径：
 * fromIdx 是当前数组下标；toAt/toIdx 是目标 zone 的可见下标（按 zone 过滤后的序号）。
 */
function pageMoveBlock(pageId, fromIdx, toAt, toIdx) {
  const arr = pageBlocksOf(pageId);
  if (fromIdx < 0 || fromIdx >= arr.length) return;
  const vis = arr.map((b, i) => ({ b, i })).filter((x) => x.b.at === toAt);
  let insertFull;
  if (toIdx >= vis.length) {
    insertFull = toAt === 'top'
      ? (arr.findIndex((b) => b.at === 'bottom') === -1 ? arr.length : arr.findIndex((b) => b.at === 'bottom'))
      : arr.length;
  } else {
    insertFull = vis[toIdx].i;
  }
  commit(`${C.PAGE_BY_ID[pageId].label}·块换位`, (sk) => {
    const a = sk.pageBlocks[pageId] || [];
    const m = a.splice(fromIdx, 1)[0];
    let ins = insertFull;
    if (fromIdx < ins) ins--;
    ins = Math.max(0, Math.min(a.length, ins));
    m.at = toAt;
    a.splice(ins, 0, m);
    SEL = { type: 'pblock', pageId, at: toAt, index: a.filter((b) => b.at === toAt).indexOf(m) };
  });
}
/** 跨页移动（同 kind 直移，追加到目标页顶部区末尾）。 */
function pageMoveToPage(fromPageId, fromIdx, toPageId) {
  const arr = pageBlocksOf(fromPageId);
  if (fromIdx < 0 || fromIdx >= arr.length) return;
  commit(`块移到「${C.PAGE_BY_ID[toPageId].label}」`, (sk) => {
    const from = sk.pageBlocks[fromPageId] || [];
    const m = from.splice(fromIdx, 1)[0];
    if (!from.length) delete sk.pageBlocks[fromPageId];
    const to = sk.pageBlocks[toPageId] || (sk.pageBlocks[toPageId] = []);
    let i = 0; while (i < to.length && to[i].at === 'top') i++;
    to.splice(i, 0, m);
    SEL = { type: 'pblock', pageId: toPageId, at: 'top', index: to.filter((b) => b.at === 'top').indexOf(m) };
  });
  openPage(toPageId);
}
function pageDelBlock(pageId, at, index) {
  const arr = pageBlocksOf(pageId);
  const fullIdx = arr.map((b, i) => ({ b, i })).filter((x) => x.b.at === at)[index];
  if (!fullIdx) return;
  commit(C.PAGE_BY_ID[pageId].label + '·删块', (sk) => {
    const a = sk.pageBlocks[pageId] || [];
    a.splice(fullIdx.i, 1);
    if (!a.length) delete sk.pageBlocks[pageId];
  });
  if (SEL && SEL.type === 'pblock' && SEL.pageId === pageId && SEL.at === at && SEL.index === index) SEL = null;
}
function pageDupBlock(pageId, at, index) {
  const arr = pageBlocksOf(pageId);
  const zone = arr.map((b, i) => ({ b, i })).filter((x) => x.b.at === at);
  const hit = zone[index];
  if (!hit) return;
  commit(C.PAGE_BY_ID[pageId].label + '·复制块', (sk) => {
    const a = sk.pageBlocks[pageId] || [];
    a.splice(hit.i + 1, 0, JSON.parse(JSON.stringify(hit.b)));
    SEL = { type: 'pblock', pageId, at, index: index + 1 };
  });
}
/** 页面块改字段（连续输入）。 */
function pageSetBlockField(sel, field, value, label) {
  live(`${C.PAGE_BY_ID[sel.pageId].label}·块·${label || field}`, (sk) => {
    const arr = sk.pageBlocks[sel.pageId] || [];
    const zone = arr.filter((x) => x.at === sel.at);
    const m = zone[sel.index];
    if (m) m[field] = value;
  });
}

// ============================================================ 自由画布（v5 · 默认模式）

// 这一节把 `free-canvas.js`（引擎：元素模型 / 拖放 / 编译）接到设计台的
// **状态**（`S.skin.freeLayers`）和**历史**（本文件的 `HIST`）上。
//
// 三条约定，改之前先读：
//
// ① **数据只存一份**：画布的每次改动都经 `freeOnChange` 落回 `S.skin.freeLayers`，
//    历史用本文件的 HIST（**不用**引擎内部那份）。这样 Ctrl+Z 能一次跨过
//    "改配色 → 摆元素 → 再改配色"，而不是"画布撤一步、皮肤撤一步"地来回跳。
//    （引擎那边靠 `ownHistory:false` 让路，见 free-canvas 的 onKey。）
// ② **载入不回调**：`freeLoad()` 之后引擎不会反过来触发 onChange ——
//    否则"载入即改动"，历史栈会被重复快照灌满。
// ③ **面板按需重建**：图层列表每次改动都重建（便宜、而且必须跟着变）；
//    属性面板只在**选中变化**时重建 —— 每次改动都重建会让正在输入的框失焦。

/** 取某一页的自由画布图层（可能没有 = 这一页还没设计过）。 */
function freeLayer(pageId) { return S.skin.freeLayers[pageId] || null; }

/** 皮肤解析出的角色色 —— 画布上"跟随外观"的元素靠它预览。 */
/**
 * 画布用的颜色（`--fc-*` 那一套，也是编译产物里注入的那 8 个变量）。
 *
 * ⚠️⚠️ **必须转成 hex 字符串**。`C.resolveColors()` 返回的是 `{r,g,b}` **对象**，
 *    直接塞进 CSS 变量会变成字符串 `"[object Object]"` —— 而它在
 *    `linear-gradient(135deg, [object Object], …)` 里是**无效值**，
 *    整条 `background` 声明被浏览器丢弃。
 *
 *    症状（2026-10-15 真机踩到）：天气卡本该是蓝色渐变，实际**是白底**；
 *    所有依赖 `var(--fc-*)` 的地方全部回落到"没有"。
 *    ⚠️ 它**不报任何错** —— 只有肉眼看截图才发现。所以有 `studio-live-check`
 *    里那条"卡片有背景色"的断言。
 */
/** `{r,g,b}` 或 `#rrggbb` → `#rrggbb`。**认得 rgbToHex 已经带的 `#`**（见 freeColors 的注释）。 */
function hexOf(x) {
  if (!x) return '';
  if (typeof x === 'string') return x;
  const t = C.rgbToHex(x);
  return t && t.charAt(0) === '#' ? t : '#' + t;
}

function freeColors() {
  try {
    const col = C.resolveColors(S.skin, S.dark, S.seed) || {};
    const hex = (x) => {
      if (!x) return '';
      if (typeof x === 'string') return x;
      const t = C.rgbToHex(x);
      // ⚠️ `rgbToHex` **自带 `#`** —— 再补一个会变成 `##225790`，CSS 里同样无效。
      return t && t.charAt(0) === '#' ? t : '#' + t;
    };
    return {
      primary: hex(col.primary),
      onPrimary: hex(col.onPrimary),
      background: hex(col.background),
      onBackground: hex(col.onBackground),
      surface: hex(col.surface),
      onSurface: hex(col.onSurface),
      secondaryContainer: hex(col.secondaryContainer),
      onSecondaryContainer: hex(col.onSecondaryContainer),
    };
  } catch (e) { return {}; }
}

/** 懒建编辑器。切到「高级」时**不销毁** —— 切回来接着用，省一次重建。 */
function ensureEditor() {
  if (FCED) return FCED;
  const host = $('#fc-canvas');
  if (!host) return null;
  // ★ 背景层的 wasm **一开机就读** `localStorage['shellRenderData']` ——
  //   所以必须在挂 iframe 之前把它写好，晚一步就是"第一次打开渲染的是演示数据"。
  try { localStorage.setItem('shellRenderData', JSON.stringify(shellRenderPayload())); } catch (e) { /* 隐私模式 */ }
  FCED = FC.mount(host, {
    colors: freeColors(),
    dark: S.dark,
    ownHistory: false,
    // ★★ 把手机端的渲染代码（shared-ui → WasmJS）铺成画布背景 ——
    //   "默认打开就是手机端默认的样子，所有页面都是这样"
    shellFrame: true,
    // 内容区参考线（画布是整屏，产物是内容区 —— 这条线把"内容真正开始的地方"画出来）
    shellTop: SHELL_TOP,
    shellBottom: SHELL_BOTTOM,
    contentTop: contentTopFor(S.pageId),
    onChange: freeOnChange,
    onSelect: () => { renderFreeInspector(); updateFreeSub(); },
    onMenu: (x, y, items) => openMenuAt(x, y, items),
    onCode: openFreeCode,
    onPreview: freePreview,
    // ★ 这两个由宿主实现 —— 引擎不知道"默认布局"是什么（那是外观设计台的概念）
    onClear: freeClearPage,
    onRestoreDefault: freeRestoreDefault,
    // ★ 从已有外观包导入：走发布模态里那条"按授权码载入"的路
    onImport: () => { openModal(); void doLoadFromCode(''); },
    // ★ 点中手机端画的某个块 → 右栏切成"块编辑器"
    onBlockSelect: (rect, i) => { freeSelBlock = i; renderFreeInspector(); },
  });
  // ⚠️ 只在这里设一次 `src`（重设 = 整页重载 wasm，一两秒 + 闪一下）；
  //    之后数据更新靠 wasm 自己每 400ms 轮询 localStorage。
  FCED.loadShell('shell-render/');
  // ★ 开始盯着手机端画出来的块位置 —— 好把它们变成可点选的控件
  FCED.watchBlocks();
  return FCED;
}

/** 画布改动 → 落回皮肤 + 记一步历史。 */
function freeOnChange(layer, label, isLive) {
  if (!layer) return;
  S.skin.freeLayers[S.pageId] = layer;
  DIRTY = true;
  if (isLive) pushHistLive(label || '改画布');
  else pushHist(label || '改画布');
  save();
  renderLayersPane();
  updateFreeSub();
  syncChrome();
}

/**
 * 载入当前页的画布（切页 / 撤销 / 导入 / 切回自由模式时调）。
 *
 * ★★ v5.3：**不再自动铺任何东西**。
 *
 * 之前那版会在主页"自动铺一套我写的 HTML 卡片"（天气卡 / 日程卡）。
 * 那是**照着手机端的样子复刻的一份**，不是手机端原本的代码 —— 用户的原话：
 * 「我让你**从原本的手机端代码**进行操作，你都会瞎创作什么」。
 *
 * 现在画布底下的背景层（`shell-render` = 手机端 `shared-ui` 编译的 WasmJS）
 * **就是手机端原本的代码**，它自己会把主页画出来（头部 + 布局里的块）。
 * 所以这里**什么都不铺** —— 用户看到的就是手机端的样子，
 * 要加东西他自己从元素库/预制块拖。
 */
/** 当前选中的"手机端画的块"下标（-1 = 没选）。 */
let freeSelBlock = -1;

/** 这一页主页布局里的块数组（没设过就是手机端默认那份）。 */
function homeBlocksNow() {
  const lay = S.skin.homeLayout;
  const blocks = (lay && lay.blocks) || PHONE_DEFAULT_HOME_LAYOUT.blocks;
  return JSON.parse(JSON.stringify(blocks));
}

/** 改第 i 个块的一个字段 → 写回 skin → wasm 自动重画。 */
function setBlockField(i, key, val) {
  const blocks = homeBlocksNow();
  if (!blocks[i]) return;
  blocks[i][key] = val;
  commit('改块「' + (blocks[i].id || i) + '」的 ' + key, () => {
    // 一旦动过，就把整份布局落到 skin.homeLayout（不再走"手机端默认"那条回落）
    S.skin.homeLayout = Object.assign({ padding: 14, gap: 12 }, S.skin.homeLayout || {}, { blocks: blocks });
  });
  pushRenderData();
}

function freeLoad() {
  const ed = ensureEditor();
  if (!ed) return;

  // ★ 每个页面的"内容真正开始的地方"不一样（主页有头部行、别的页有子标签行）
  ed.setContentTop(contentTopFor(S.pageId));
  ed.load(freeLayer(S.pageId), {
    width: FC.DEFAULT_W, height: FC.DEFAULT_H,
    colors: freeColors(), dark: S.dark,
  });
  // ★ 背景层（wasm）要跟着切页 —— 它每 400ms 读一次这份数据
  pushRenderData();
  paintFreePanels();
}

/** 一键清空这一页的画布。 */
function freeClearPage() {
  if (!FCED) return;
  if (!FCED.layer.els.length && !(FCED.layer.code || '').trim()) { toast('这一页本来就是空的'); return; }
  if (!confirm('清空「' + pageLabel() + '」画布上的全部内容？（可以 Ctrl+Z 撤销）')) return;
  FCED.commit('清空这一页', () => { FCED.layer.els = []; FCED.layer.code = ''; });
  FCED.sel = [];
  FCED.render(); FCED.fireSel();
  toast('已清空（Ctrl+Z 可撤销）', 'ok');
}

/**
 * 一键恢复默认：把这一页恢复成**手机端默认布局**。
 *
 * ⚠️ 主页才有"手机端默认布局"这个概念；别的页面恢复默认 = 清空
 *    （因为它们的默认内容由插件自己画，外壳不该往上加东西）。
 */
function freeRestoreDefault() {
  if (!FCED) return;
  // ⚠️ "恢复默认" = **清空画布**，不是"铺一套我写的默认布局"。
  //    因为"手机端默认的样子"是**手机端自己的渲染代码**画在背景层上的
  //    （`shell-render` = shared-ui → WasmJS）—— 画布上本来就没有元素，
  //    所以清空就等于回到那个默认。我以前在这里铺 HTML 卡片，是**瞎创作**。
  if (!confirm('把「' + pageLabel() + '」画布清空，回到手机端默认的样子？（可以 Ctrl+Z 撤销）')) return;
  FCED.commit('恢复默认（清空画布）', () => { FCED.layer.els = []; FCED.layer.code = ''; });
  FCED.sel = []; FCED.render(); FCED.fireSel();
  toast('已清空 —— 现在看到的就是手机端默认的样子', 'ok', 2600);
}

/** 当前页的中文名（提示语里用）。 */
function pageLabel() {
  const p = C.PAGE_BY_ID[S.pageId];
  return p ? p.label : S.pageId;
}

/** 只把颜色同步过去（改配色时用；不动画布数据，免得打断正在拖的手）。 */
function freeSyncColors() {
  if (!FCED) return;
  FCED.setColors(freeColors(), S.dark);
}

function paintFreePanels() {
  if (!FCED) return;
  const lib = $('#fc-lib');
  if (lib) FC.renderLib(lib, FCED);
  renderLayersPane();
  renderFreeInspector();
  updateFreeSub();
  renderPageSel();
}

function renderLayersPane() {
  const box = $('#fc-layers');
  if (box && FCED) FC.renderLayers(box, FCED);
}

function renderFreeInspector() {
  const box = $('#fc-inspector');
  if (!box || !FCED) return;
  // ★ 选中了**手机端画的某个块** → 右栏切成"块编辑器"
  if (freeSelBlock >= 0) { renderBlockInspector(box); return; }
  FC.renderInspector(box, FCED, { onOpenCode: openFreeCode, onOpenPreview: freePreview });
}

/**
 * **块编辑器** —— 改的是手机端自己的数据（`HomeLayout.blocks[i]`），
 * 改完 wasm 会自动重画（它每 400ms 读一次 `shellRenderData`）。
 *
 * 这是用户要的那条路：「它加载的那些东西根本就不能进行编辑，
 * **它应该是生成对应的控件**」—— 手机端画的块 → 点一下 → 变成可编辑的控件。
 */
function renderBlockInspector(box) {
  const F = FC;   // ⚠️ 那些 UI 辅助（fsec/frow/fnum/fbtns/noteBlock）直接挂在 FreeCanvas 上，没有 FC.ui 这一层
  const blocks = homeBlocksNow();
  const b = blocks[freeSelBlock];
  box.innerHTML = '';
  if (!b) { freeSelBlock = -1; renderFreeInspector(); return; }

  const head = document.createElement('div');
  head.className = 'fc-insphead';
  head.innerHTML = '<b>块 ' + (freeSelBlock + 1) + ' / ' + blocks.length + '</b>' +
    '<span class="fc-muted">手机端画的</span>';
  box.appendChild(head);

  const s1 = F.fsec('这一块是什么');
  s1.body.appendChild(F.noteBlock(
    '手机端主页布局里的第 ' + (freeSelBlock + 1) + ' 块。改这里 = 改手机端的数据，' +
    '画布会立刻跟着重画。<br>⚠️ 它是**原生组件**（不是画布元素），' +
    '所以改的是它的属性，不是拖它的位置。',
  ));
  box.appendChild(s1.node);

  const s2 = F.fsec('属性');
  // ⚠️⚠️ 这几个 UI 辅助的返回类型**不统一**，用之前必须看定义：
  //   `ftext` / `fnum` / `fselect` → `{ node, set }`      ← 要 `.node`
  //   `fbtns`                      → `{ node }`           ← 要 `.node`
  //   `fsec`                       → `{ node, body }`     ← 要 `.node` / `.body`
  //   `frow` / `noteBlock`         → 裸 element
  //   直接塞给 `frow` 会 `appendChild({…})` 崩掉，而**报错指向 appendChild**，
  //   看起来像 `frow` 的锅（2026-10-15 为此查了三轮）。
  s2.body.appendChild(F.frow('类型', F.ftext(b.type || 'widget', () => {}).node));
  if (b.widgetId) s2.body.appendChild(F.frow('组件', F.ftext(b.widgetId, () => {}).node));
  if (b.text !== undefined) {
    s2.body.appendChild(F.frow('文字', F.ftext(b.text || '', (v) => setBlockField(freeSelBlock, 'text', v)).node));
  }
  // ⚠️⚠️ `fnum` / `fselect` 返回的是 **`{ node, set }`**，不是裸元素 ——
  //    直接塞给 `frow` 会 `appendChild({...})` 崩掉（"parameter 1 is not of type 'Node'"），
  //    而**报错位置指向 appendChild**，看起来像 frow 的锅。
  //    而 `ftext` / `fbtns` 返回裸元素。这几个辅助的返回类型**不统一**，用的时候要看定义。
  s2.body.appendChild(F.frow('占几格',
    F.fselect(String(b.span || 2), [{ v: '2', label: '整宽' }, { v: '1', label: '半宽' }],
      (v) => setBlockField(freeSelBlock, 'span', Number(v))).node));
  // ⚠️ `fnum` 的签名是 **(value, min, max, step, onChange, suffix)** ——
  //    写成 (value, onChange, min, max) 会让 `min` 收到一个函数，
  //    参数全错位（2026-10-15 踩到：右栏渲染到一半就崩）。
  s2.body.appendChild(F.frow('最多几条',
    F.fnum(b.limit || 0, 0, 20, 1, (v) => setBlockField(freeSelBlock, 'limit', Math.round(v))).node));
  s2.body.appendChild(F.frow('高度（0 = 自动）',
    F.fnum(b.height || 0, 0, 600, 1, (v) => setBlockField(freeSelBlock, 'height', Math.round(v))).node));
  box.appendChild(s2.node);

  const s3 = F.fsec('位置');
  // ⚠️ `fbtns` 的项用 **`fn`**（不是 `onClick`）—— 写错的话按钮点了没反应，且不报错。
  s3.body.appendChild(F.fbtns([
    { label: '↑ 上移', fn: () => moveBlock(freeSelBlock, -1) },
    { label: '↓ 下移', fn: () => moveBlock(freeSelBlock, 1) },
  ]).node);
  s3.body.appendChild(F.fbtns([
    { label: '🗑 删掉这一块', fn: () => delBlock(freeSelBlock) },
    { label: '＋ 在后面加一块', fn: () => addBlock(freeSelBlock) },
  ]).node);
  box.appendChild(s3.node);

  const s4 = F.fsec('回到画布');
  s4.body.appendChild(F.fbtns([
    { label: '取消选中', fn: () => { FCED.selectBlock(-1); freeSelBlock = -1; renderFreeInspector(); } },
  ]).node);
  box.appendChild(s4.node);
}

/** 把第 i 块和第 j 块换个位置。 */
function moveBlock(i, d) {
  const blocks = homeBlocksNow();
  const j = i + d;
  if (j < 0 || j >= blocks.length) return;
  const t = blocks[i]; blocks[i] = blocks[j]; blocks[j] = t;
  commit('移动块', () => {
    S.skin.homeLayout = Object.assign({ padding: 14, gap: 12 }, S.skin.homeLayout || {}, { blocks: blocks });
  });
  freeSelBlock = j;
  pushRenderData();
  renderFreeInspector();
}

/** 删掉第 i 块。 */
function delBlock(i) {
  const blocks = homeBlocksNow();
  if (blocks.length <= 1) { toast('只剩一块了 —— 至少留一块', 'warn'); return; }
  if (!confirm('删掉第 ' + (i + 1) + ' 块？（可以 Ctrl+Z 撤销）')) return;
  blocks.splice(i, 1);
  commit('删块', () => {
    S.skin.homeLayout = Object.assign({ padding: 14, gap: 12 }, S.skin.homeLayout || {}, { blocks: blocks });
  });
  FCED.selectBlock(-1);
  freeSelBlock = -1;
  pushRenderData();
  renderFreeInspector();
}

/** 在第 i 块后面插一块（默认插一个日程组件）。 */
function addBlock(i) {
  const blocks = homeBlocksNow();
  blocks.splice(i + 1, 0, { type: 'widget', id: 'block-' + Date.now().toString(36), widgetId: 'today-agenda', span: 2, limit: 4 });
  commit('加一块', () => {
    S.skin.homeLayout = Object.assign({ padding: 14, gap: 12 }, S.skin.homeLayout || {}, { blocks: blocks });
  });
  pushRenderData();
  renderFreeInspector();
}

function updateFreeSub() {
  const sub = $('#fc-insp-sub');
  if (!sub || !FCED) return;
  const n = FCED.sel.length;
  // ⚠️ `elById` 可能返回 null（选中的元素刚好被删掉，而这条回调先到）——
  //    `elLabel(null)` 会崩，所以取到空就退化成"未选中"。
  const one = n === 1 ? FCED.elById(FCED.sel[0]) : null;
  sub.textContent = !n ? '未选中元素'
    : one ? FC.elLabel(one)
    : '已选 ' + n + ' 个';
}

/** 顶栏面包屑（自由模式那一句）。 */
function freePathText() {
  if (!FCED) return '自由画布';
  const p = C.PAGE_BY_ID[S.pageId];
  return (p ? p.label : S.pageId) + ' · ' + FCED.layer.els.length + ' 个元素' +
    (FCED.sel.length ? ' · 选中 ' + FCED.sel.length : '');
}

/** 顶栏那个"在哪个页面上画"的下拉。 */
function renderPageSel() {
  const sel = $('#fc-page');
  if (!sel) return;
  if (!sel.options.length) {
    C.PAGES.forEach((p) => {
      const o = document.createElement('option');
      o.value = p.id; o.textContent = p.label;
      sel.appendChild(o);
    });
  }
  sel.value = S.pageId;
}

/** 切编辑方式。 */
function setMode(m, quiet) {
  S.mode = (m === 'pro') ? 'pro' : 'free';
  save();
  const free = S.mode === 'free';
  const show = (id, on) => {
    const e = $(id);
    if (!e) return;
    // ⚠️ `#ss-drawer` 的 id 挂在一个 `<label>` 里面的 `<input>` 上 ——
    //    直接 `input.hidden = true` 只会藏掉那个小方块，"抽屉"两个字还留在顶栏上。
    const t = (e.tagName === 'INPUT' && e.closest('label')) || e;
    t.hidden = !on;
  };
  show('#fc-left', free); show('#ss-left-pro', !free);
  show('#fc-mid', free); show('#ss-mid-pro', !free);
  show('#fc-right', free); show('#ss-right-pro', !free);
  show('#fc-page', free);
  // ★ v5：这两个按钮作用于**手机模拟器**（⌨ 源码渲染 / 抽屉预览），而自由模式下
  //   模拟器是隐藏的 —— 留着它们就是"点了没反应"的坑（用户实测报的就是这一类）。
  //   ⚠️ 横竖屏（#ss-orient）**不在此列**：它在自由模式下改的是画布尺寸，是有用的。
  show('#ss-srcview', !free);
  show('#ss-drawer', !free);
  document.querySelectorAll('.ss-modebtn').forEach((b) => b.classList.toggle('on', b.dataset.mode === S.mode));
  if (free) freeLoad();
  else { renderPhone(); renderOutline(); renderInspector(); }
  syncChrome();
  if (!quiet) {
    toast(free ? '自由画布：拖拖拽拽就行，双击文字直接改'
      : '高级模式：上一版那套块流编辑（老设计都在这儿）', 'ok', 2600);
  }
}

// ---- 代码模式（画布 ⇄ HTML）----

function openFreeCode() {
  if (!FCED) return;
  FC_CODE_PAGE = S.pageId;
  const L = freeLayer(S.pageId) || FC.normalizeLayer(null, FC.DEFAULT_W, FC.DEFAULT_H);
  const ta = $('#fc-code-ta');
  if (ta) ta.value = L.code || FC.compile(L, { colors: freeColors(), shellTop: contentTopFor(S.pageId), shellBottom: SHELL_BOTTOM });
  const p = C.PAGE_BY_ID[S.pageId];
  const sub = $('#fc-code-sub');
  if (sub) sub.textContent = (p ? p.label : S.pageId) + ' · ' + (L.els || []).length + ' 个元素';
  const note = $('#fc-code-note');
  if (note) {
    note.innerHTML = L.code
      ? '⚠️ 这一页现在是 <b>代码接管</b>状态 —— 画布不再参与，拖拽改的就是上面这段。想再拖，点「改回画布」。'
      : '这是画布<b>编译出来</b>的 HTML（元素的位置/颜色全在这段里）。改完点「用这段代码」，这一页就交给代码。' +
        '<br>可用的桥：<code>threees.data(\'today-agenda\')</code> 取数据、' +
        '<code>threees.act(\'page\',\'schedule\')</code> 执行动作。' +
        '<br>⚠️ 手机端 <b>禁网</b> —— 图片只能用 <code>data:</code> 内联。';
  }
  const modal = $('#fc-code');
  if (modal) modal.hidden = false;
}
function closeFreeCode() { const m = $('#fc-code'); if (m) m.hidden = true; }

function applyFreeCode() {
  const ta = $('#fc-code-ta');
  if (!ta) return;
  const code = ta.value;
  const pid = FC_CODE_PAGE || S.pageId;
  const p = C.PAGE_BY_ID[pid];
  commit('用代码接管「' + (p ? p.label : pid) + '」', (sk) => {
    const L = sk.freeLayers[pid] || (sk.freeLayers[pid] = FC.normalizeLayer(null, FC.DEFAULT_W, FC.DEFAULT_H));
    L.code = code;
  });
  closeFreeCode();
  freeLoad();
  toast('已用代码接管这一页（点「改回画布」可以反悔）', 'ok', 3000);
}

function resetFreeCode() {
  const pid = FC_CODE_PAGE || S.pageId;
  commit('改回画布', (sk) => { const L = sk.freeLayers[pid]; if (L) L.code = ''; });
  freeLoad();
  openFreeCode();                 // 重新填内容（这时填的是刚编译出来的产物）
  toast('已改回画布', 'ok', 2200);
}

/** 预览：用 iframe 按真机那套跑一遍（动作会触发，只是打印出来）。 */
function freePreview() {
  if (!FCED) return;
  const mid = $('#fc-mid');
  if (!mid) return;
  const wrap = el('div', 'fc-pvwrap');
  const bar = el('div', 'fc-pvbar');
  bar.appendChild(el('span', null, '预览 · 动作会触发（只是打印出来，不会真的跳页）'));
  const close = el('button', 'ss-icobtn', '✕');
  close.title = '关闭预览';
  close.onclick = () => wrap.remove();
  bar.appendChild(close);
  const body = el('div', 'fc-pvbody');
  wrap.appendChild(bar);
  wrap.appendChild(body);
  mid.appendChild(wrap);
  FC.preview(body, FCED, { shellTop: contentTopFor(S.pageId), shellBottom: SHELL_BOTTOM });
}

// ============================================================ 选择

// ⚠️ 这个函数叫 pick 不叫 select —— 表单工厂里已经有一个 select(value, options, onChange)。
//    函数声明同名的后果是**后者悄悄覆盖前者**：检查器里每个下拉框都会变成"选中对象"，
//    症状是点开块检查器直接崩（nodeOfSel 认不出表单值当的 type）。
function pick(type) {
  SEL = type ? { type } : null;
  renderOutline(); renderInspector(); markPhoneSel(); syncChrome();
}
function openPage(id) {
  if (S.pageId !== id) {
    S.pageId = id;
    if (C.PAGE_BY_ID[id].tabs) S.subTab = C.PAGE_BY_ID[id].tabs[0];
    S.landMenu = false;   // 树状菜单不跨页保留（源码：切页就收起）
    save();
    renderPhone();
    pushRenderData();
  }
  SEL = { type: 'page', pageId: id };
  renderOutline(); renderInspector(); syncChrome();
}
function selectHBlock(id) {
  SEL = { type: 'hblock', id };
  renderOutline(); renderInspector(); markPhoneSel(); syncChrome();
}
function selectPBlock(pageId, at, index) {
  SEL = { type: 'pblock', pageId, at, index };
  renderOutline(); renderInspector(); markPhoneSel(); syncChrome();
}

// ============================================================ 大纲（左栏）

const OUTLINE = [
  { id: 'theme', icon: '🎨', label: '主题基调', sub: '种子色 · 深浅 · 起手式' },
  { id: 'colors', icon: '🌈', label: '配色角色', sub: '27 个角色 · 留空走推导' },
  { id: 'status', icon: '📶', label: '状态栏', sub: '' },
  { id: 'top', icon: '📌', label: '顶栏', sub: '' },
  { id: 'nav', icon: '🧭', label: '底部导航', sub: '' },
  { id: 'drawer', icon: '📂', label: '抽屉', sub: '' },
  { id: 'card', icon: '🃏', label: '卡片', sub: '圆角' },
  { id: 'text', icon: '🔤', label: '文字', sub: '字体 · 字号 · 字距' },
  { id: 'decor', icon: '🌫', label: '背景纹理', sub: '固定 6% 浓度' },
];

function nodeOfSel() {
  if (!SEL) return null;
  if (SEL.type === 'page') {
    const p = C.PAGE_BY_ID[SEL.pageId];
    return { page: p.id, icon: p.icon, label: p.label, sub: p.desc };
  }
  if (SEL.type === 'hblock') {
    const b = homeBlockById(SEL.id);
    return { id: 'hblock', icon: '🧩', label: hblockLabel(b), sub: b ? C.HOME_BLOCK_LABEL[b.type] : '' };
  }
  if (SEL.type === 'pblock') {
    const b = pblockOf(SEL);
    return { id: 'pblock', icon: b ? ({ heading: '🔠', text: '📝', actions: '⚡', links: '🔗', card: '🃏', divider: '➖' })[b.kind] : '📦', label: b ? C.PAGE_BLOCK_LABEL[b.kind] : '块', sub: C.PAGE_BY_ID[SEL.pageId].label };
  }
  return OUTLINE.find((n) => n.id === SEL.type) || null;
}

/** 每个对象"改了几项"（徽章数字）。 */
function editedCount(node) {
  if (!node) return 0;
  const sk = S.skin;
  if (node.page) {
    if (node.page === 'home') {
      return sk.homeLayout ? sk.homeLayout.blocks.length : 0;
    }
    return (sk.pageBlocks[node.page] || []).length
      + (sk.pageBg[node.page] ? 1 : 0) + (sk.pageTitles[node.page] ? 1 : 0)
      + (sk.hiddenTabs[node.page] || []).length;
  }
  switch (node.id) {
    case 'theme': return (S.seed !== '#2F6FB5' || S.dark) ? 1 : 0;
    case 'colors': return Object.keys(sk.colors).length;
    case 'status': return (sk.statusBar !== 'auto' ? 1 : 0) + (sk.statusBarColor ? 1 : 0);
    case 'top': return sk.topBarBg ? 1 : 0;
    case 'nav': return (sk.navBg ? 1 : 0) + (sk.navSelected ? 1 : 0) + (sk.navLabel === false ? 1 : 0)
      + (sk.iconShape !== 'none' ? 1 : 0) + (sk.iconBg ? 1 : 0) + (sk.iconSize > 0 ? 1 : 0);
    case 'drawer': return sk.drawerBg ? 1 : 0;
    case 'card': return (sk.cardRadius > 0 ? 1 : 0) + (sk.cornerScale > 0 ? 1 : 0);
    case 'text': return (sk.fontFamily ? 1 : 0) + (sk.fontWeight ? 1 : 0) + (sk.fontScale > 0 ? 1 : 0) + (sk.letterSpacing !== 0 ? 1 : 0);
    case 'decor': return (sk.decor !== 'none' ? 1 : 0) + (sk.decorColor ? 1 : 0);
  }
  return 0;
}

function outlineRow(o) {
  const b = el('button', 'ss-ol-item' + (o.on ? ' on' : '') + (o.blk ? ' ss-ol-blk' : ''));
  b.innerHTML = `<span class="ss-ol-ico">${o.icon}</span>` +
    `<span class="ss-ol-txt"><span class="ss-ol-t">${esc(o.label)}${o.prev ? '<i class="ss-ol-prev">预览中</i>' : ''}</span>` +
    (o.sub ? `<span class="ss-ol-s">${esc(o.sub)}</span>` : '') + `</span>` +
    (o.count ? `<span class="ss-ol-badge">${o.count}</span>` : '');
  b.onclick = o.onclick;
  if (o.page != null) b.setAttribute('data-page', o.page);   // 跨页拖放目标
  if (o.title) b.title = o.title;
  return b;
}

function renderOutline() {
  const box = $('#ss-outline');
  if (!box) return;
  box.innerHTML = '';
  box.appendChild(el('div', 'ss-ol-group', '整机 · 全局外观'));
  for (const n of OUTLINE) {
    box.appendChild(outlineRow({
      icon: n.icon, label: n.label, sub: n.sub,
      count: editedCount(n),
      on: SEL && SEL.type === n.id,
      onclick: () => pick(n.id),
    }));
  }
  box.appendChild(el('div', 'ss-ol-group', '页面 · 拖块到某页上 = 移过去'));
  for (const p of C.PAGES) {
    const open = !!OLOPEN[p.id] || p.id === S.pageId;
    let count, sub;
    if (p.id === 'home') {
      count = editedCount({ page: 'home' });
      sub = S.skin.homeLayout ? `自定义布局 · ${S.skin.homeLayout.blocks.length} 块` : '未自定义布局（原版卡片墙）';
    } else {
      const nb = (S.skin.pageBlocks[p.id] || []).length;
      count = editedCount({ page: p.id });
      sub = nb ? `${nb} 个页面块` : '';
    }
    box.appendChild(outlineRow({
      icon: p.icon, label: p.label, sub, count,
      page: p.id,
      on: SEL && SEL.type === 'page' && SEL.pageId === p.id,
      prev: p.id === S.pageId,
      title: p.desc,
      onclick: () => openPage(p.id),
    }));
    if (!open) continue;
    if (p.id === 'home') {
      const hl = S.skin.homeLayout;
      if (hl) {
        hl.blocks.forEach((b) => {
          box.appendChild(outlineRow({
            icon: ({ widget: '🧩', heading: '🔠', text: '📝', divider: '➖', spacer: '␣', html: '</>' })[b.type] || '📦',
            label: hblockLabel(b),
            sub: b.type === 'widget' ? (C.HOME_WIDGET_BY_ID[b.widgetId] ? '' : '⚠️ 认不出的卡片') : (b.span === 2 ? '整宽' : '半宽'),
            blk: true,
            on: SEL && SEL.type === 'hblock' && SEL.id === b.id,
            onclick: () => selectHBlock(b.id),
          }));
        });
      }
    } else {
      for (const at of ['top', 'bottom']) {
        pageBlocksOf(p.id).filter((b) => b.at === at).forEach((b, zi) => {
          box.appendChild(outlineRow({
            icon: ({ heading: '🔠', text: '📝', actions: '⚡', links: '🔗', card: '🃏', divider: '➖' })[b.kind] || '📦',
            label: (b.kind === 'divider' ? '分隔线' : b.text ? b.text.slice(0, 12) : C.PAGE_BLOCK_LABEL[b.kind]),
            sub: (at === 'top' ? '顶部' : '底部'),
            blk: true,
            on: SEL && SEL.type === 'pblock' && SEL.pageId === p.id && SEL.at === at && SEL.index === zi,
            onclick: () => selectPBlock(p.id, at, zi),
          }));
        });
      }
    }
  }
}

// ============================================================ 手机画面（画布）

function hexOr(hex, fallback) { return C.hexToRgb(hex) || fallback; }

/** 把皮肤算成 CSS 变量写在 `.ph` 上 —— **这是"手机画面跟着皮肤变"的唯一开关**。 */
function applyVars() {
  const ph = $('.ph');
  const sk = S.skin;
  const col = C.resolveColors(sk, S.dark, S.seed);
  const pageBgHex = sk.pageBg[S.pageId];
  // 状态栏是 edge-to-edge：底色**跟随顶栏/页面**（MainScreen 那条 topBar Column 带
  // statusBarsPadding，顶栏底色一路延伸到状态栏后面），不是独立色带。
  // 沉浸页（mdnote/mdnotelive）没有顶栏 → 直接贴页面底色。
  const immersive = C.IMMERSIVE_PAGES.includes(S.pageId);
  const statusBg = sk.statusBarColor
    || (!immersive && sk.topBarBg ? sk.topBarBg : null)
    || pageBgHex
    || '#' + C.rgbToHex(col.background);
  const statusFg = sk.statusBar === 'light' ? '#fff'
    : sk.statusBar === 'dark' ? '#111'
      : (C.lightness(hexOr(statusBg, col.background)) < 0.5 ? '#fff' : '#111');
  const v = {
    '--ph-bg': '#' + C.rgbToHex(col.background),
    '--ph-on-bg': '#' + C.rgbToHex(col.onBackground),
    '--ph-muted': '#' + C.rgbToHex(col.onSurfaceVariant),
    '--ph-primary': '#' + C.rgbToHex(col.primary),
    '--ph-on-primary': '#' + C.rgbToHex(col.onPrimary),
    '--ph-primary-container': '#' + C.rgbToHex(col.primaryContainer),
    '--ph-on-primary-container': '#' + C.rgbToHex(col.onPrimaryContainer),
    '--ph-tertiary-container': '#' + C.rgbToHex(col.tertiaryContainer),
    // 抽屉快捷入口格子（= DrawerEntryChip 的 secondaryContainer / onSecondaryContainer）
    '--ph-secondary-container': '#' + C.rgbToHex(col.secondaryContainer),
    '--ph-on-secondary-container': '#' + C.rgbToHex(col.onSecondaryContainer),
    '--ph-card': '#' + C.rgbToHex(col.surfaceContainerLow),
    '--ph-card2': '#' + C.rgbToHex(col.surfaceContainerHigh),
    '--ph-status-bg': statusBg,
    '--ph-status-fg': statusFg,
    '--ph-top-bg': sk.topBarBg ? sk.topBarBg : '#' + C.rgbToHex(col.background),
    '--ph-nav-bg': sk.navBg ? sk.navBg : '#' + C.rgbToHex(col.surface),
    '--ph-nav-sel': sk.navSelected ? sk.navSelected : '#' + C.rgbToHex(col.primary),
    // 抽屉默认 = surfaceVariant（比页面深一档 —— 那个深度差是"主页被推下去"的全部视觉依据）
    '--ph-drawer-bg': sk.drawerBg ? sk.drawerBg : '#' + C.rgbToHex(col.surfaceVariant),
    '--ph-icon-bg': sk.iconBg ? sk.iconBg : '#' + C.rgbToHex(col.primaryContainer),
    // 横屏两栏（LandscapeShell.kt）：左栏默认 surface（≠竖屏顶栏的 background！）、右栏默认 surface
    '--ph-rail-left-bg': sk.topBarBg ? sk.topBarBg : '#' + C.rgbToHex(col.surface),
    '--ph-rail-right-bg': sk.navBg ? sk.navBg : '#' + C.rgbToHex(col.surface),
    '--ph-sheet-bg': pageBgHex && S.pageId === 'home' ? pageBgHex : '#' + C.rgbToHex(col.background),
    '--ph-page-bg': pageBgHex ? pageBgHex : 'transparent',
    '--ph-radius': (sk.cardRadius > 0 ? sk.cardRadius : 16) + 'px',
    '--ph-corner-mul': String(sk.cornerScale > 0 ? sk.cornerScale : 1),
    '--ph-fs': String(C.fontScale(sk)),
    '--ph-ff': C.fontFamilyCss(sk),
    '--ph-fw': String(C.fontWeightCss(sk)),
    '--ph-ls': (sk.letterSpacing || 0) + 'em',
    '--ph-ico-size': String(sk.iconSize > 0 ? sk.iconSize : 22),
  };
  for (const k of Object.keys(v)) ph.style.setProperty(k, v[k]);

  // 底栏图标底形状（none 时不画底）
  ph.querySelectorAll('.ph-nav-ico').forEach((d) => {
    d.className = 'ph-nav-ico ' + (sk.iconShape || 'none');
    if (sk.iconShape === 'none') d.style.background = 'transparent';
    else d.style.background = 'var(--ph-icon-bg)';
  });

  // 装饰层：canvas 按 CSS 像素 × dpr 放大，否则在 2x 屏上糊
  const cv = $('.ph-decor');
  const dpr = Math.min(2, window.devicePixelRatio || 1);
  cv.width = 393 * dpr; cv.height = 852 * dpr;
  cv.style.width = '393px'; cv.style.height = '852px';
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  C.decorPaint(cv, sk, col);
}

function renderStatus() {
  $('.ph-status').innerHTML = '<span>12:30</span><span class="ph-right">📶 ᯤ 🔋</span>';
}
/** 顶栏 = Scaffold topBar 那条手画 Row（MainScreen.kt）：应用名 + 同步副标题 + [同步][⚙️]。
 *  ⚠️ 沉浸页（mdnote / mdnotelive）整条不渲染 —— 手机端 `!Plugins.isImmersive(pageId)`。 */
function renderTop() {
  const top = $('.ph-top');
  if (C.IMMERSIVE_PAGES.includes(S.pageId)) {
    top.style.display = 'none';
    return;
  }
  top.style.display = 'flex';
  top.innerHTML =
    '<span class="ph-app-col"><span class="ph-app">3e学习助手</span>' +
    '<span class="ph-sync">刚刚同步</span></span>' +
    '<span class="ph-tb-btn">同步</span><span class="ph-tb-btn">⚙️</span>';
}
/** 底栏 = 主页永远正中间 + 左右各 2 模块槽（BuiltinPlugins.DEFAULT_NAV 被 splitNav 拆开）。
 *  ⚠️ settings 不进底栏（顶栏 ⚙️ 常驻）。 */
function renderNav() {
  const nav = $('.ph-nav');
  nav.innerHTML = '';
  // ★ 保真（= 手机端 ModuleNavItem/NavigationBarItem）：竖屏底栏的格子是**纯文字**
  //    （icon = {} 本来就空）—— 图标形状/底色那些皮肤只作用于抽屉模块格和横屏 RailButton。
  for (const id of C.NAV_BAR) {
    const p = C.PAGE_BY_ID[id];
    const item = el('div', 'ph-nav-item' + (id === S.pageId ? ' on' : ''));
    if (S.skin.navLabel) item.appendChild(el('div', null, p.label.slice(0, 4)));
    item.onclick = () => openPage(id);   // 点底栏 = 切页预览（也选中那一页）
    nav.appendChild(item);
  }
}

// ---- 主页块画布 ----

/** 主页画布：flex-wrap 的块墙 —— 两个半宽块自动并排、整宽块独占一行（= 手机端 packRows 的观感）。 */
function homeBlockEl(b) {
  const hl = homeLayout();
  const gap = hl ? hl.gap : 10;
  const d = el('div', 'ss-blk t-' + b.type);
  d.setAttribute('data-blk', b.id);
  d.draggable = true;
  d.style.flex = b.span === 2 ? '0 0 100%' : `0 0 calc(50% - ${(gap / 2).toFixed(1)}px)`;
  if (b.bg) d.style.background = b.bg;
  if (b.fg) d.style.color = b.fg;
  if (b.radius >= 0 && b.type !== 'divider' && b.type !== 'spacer') d.style.borderRadius = b.radius + 'px';
  if (b.pad >= 0 && b.type !== 'divider' && b.type !== 'spacer') d.style.padding = b.pad + 'px';
  if (b.height > 0 && (b.type === 'html' || b.type === 'spacer')) d.style.minHeight = (b.height * (b.heightFactor || 1)) + 'px';
  if (b.fontSize > 0 && (b.type === 'heading' || b.type === 'text' || b.type === 'widget')) d.style.fontSize = b.fontSize + 'px';
  if (b.align === 'center') d.style.textAlign = 'center';
  else if (b.align === 'right') d.style.textAlign = 'right';

  if (b.type === 'widget') {
    // ★ 按卡片类型的**真实样式**渲染（HomeWidgets.kt 各卡片的迷你版，不是标签占位）
    d.appendChild(widgetPreview(b.widgetId, b.limit));
  } else if (b.type === 'heading') {
    d.appendChild(el('div', 'ss-blk-h', b.text || ' '));
  } else if (b.type === 'text') {
    d.appendChild(el('div', 'ss-blk-p', b.text || ' '));
  } else if (b.type === 'divider') {
    d.appendChild(el('div', 'ss-blk-line'));
  } else if (b.type === 'spacer') {
    d.appendChild(el('div', 'ss-blk-meta', '留白 ' + (b.height || 12) + 'dp'));
  } else if (b.type === 'html') {
    d.appendChild(el('div', null, (b.html || '').replace(/\s+/g, ' ').slice(0, 90)));
  }
  d.title = '拖拽换位 · 点选编辑';
  return d;
}

/** widget 块的真实样式预览（= HomeWidgets.kt 各卡片的迷你版）。
 *  外壳那几张逐字段对照；插件卡片用通用样式（各插件的 renderWidget 在插件包里）。 */
function widgetPreview(widgetId, limit) {
  const w = C.HOME_WIDGET_BY_ID[widgetId];
  const box = el('div');
  box.style.display = 'flex'; box.style.flexDirection = 'column'; box.style.gap = '6px'; box.style.width = '100%';
  const head = (title, more) => {
    const h = el('div', 'ph-wx-head');
    h.innerHTML = `<span class="ph-wx-title">${esc(title)}</span>` + (more ? `<span class="ph-more-btn">${esc(more)}</span>` : '');
    return h;
  };
  switch (widgetId) {
    case 'weather': // = WeatherCard（surfaceVariant 底 + 大温度 + 12h 横滚 + 5 天行）
      box.innerHTML = `
        <div class="ph-w-now"><span class="ph-w-place">泉州</span></div>
        <div class="ph-w-now"><span class="ph-w-temp">26°C</span><span class="ph-w-desc">多云</span></div>
        <div class="ph-w-hours">
          <span class="ph-w-hour"><span>14:00</span><span>26°</span><span>多云</span></span>
          <span class="ph-w-hour rain"><span>16:00</span><span>25°</span><span>小雨</span></span>
          <span class="ph-w-hour"><span>18:00</span><span>23°</span><span>多云</span></span>
        </div>
        <div class="ph-w-day"><span style="width:52px">周三</span><span style="width:60px">多云</span><span>22~29°</span></div>`;
      break;
    case 'today-agenda': { // = WidgetCard("今日日程") + MiniEventRow×n + 新建按钮
      box.appendChild(head('今日日程', '全部'));
      for (const [dot, time, title, loc] of [['var(--ph-primary)', '08:00', '高等数学', '教三 201'], ['var(--ph-tertiary-container)', '10:00', '物理实验', '实验楼 B']]) {
        const r = el('div', 'ph-mini-row');
        r.innerHTML = `<span class="ph-dot" style="background:${dot}"></span><span class="ph-mini-time">${time}</span><span class="ph-mini-col"><span>${title}</span><span class="ph-w-place">${loc}</span></span>`;
        box.appendChild(r);
      }
      box.appendChild(el('div', 'ph-obtn', '＋ 新建日程'));
      break;
    }
    case 'week-timetable': { // = WidgetCard("本周课表") + MiniTimetable 7 天小格
      box.appendChild(head('本周课表', '课表'));
      const row = el('div');
      row.style.cssText = 'display:flex;gap:3px';
      for (const c of ['var(--ph-primary-container)', 'var(--ph-card2)', 'var(--ph-card2)', 'var(--ph-tertiary-container)', 'var(--ph-card2)', 'var(--ph-primary-container)', 'var(--ph-card2)']) {
        const g = el('div');
        g.style.cssText = 'flex:1;height:44px;border-radius:6px;background:' + c;
        row.appendChild(g);
      }
      box.appendChild(row);
      break;
    }
    case 'calendar-overview': { // = WidgetCard("日历一览")：日历色点 + 本周 N 件事
      box.appendChild(head('日历一览', '管理'));
      for (const [dot, name, n] of [['var(--ph-primary)', '课程', 6], ['var(--ph-tertiary-container)', '考试', 2], ['var(--ph-card2)', '个人', 4]]) {
        const r = el('div', 'ph-w-day');
        r.innerHTML = `<span class="ph-dot" style="background:${dot}"></span><span style="flex:1">${name}</span><span class="ph-w-place">本周 ${n} 件事</span>`;
        box.appendChild(r);
      }
      break;
    }
    case 'inbox-unread': // = WidgetCard("收件箱")
      box.appendChild(head('收件箱', '全部'));
      box.appendChild(el('div', 'ph-w-place', '3 条待处理 · 1 条共享邀请'));
      break;
    default: { // 插件卡片（appearance-hero / notes-recent / mdnote-recent / nav-next / notelink-today / wordbook-today / lanlink-status / 认不出）
      if (!w) {
        box.appendChild(el('div', 'ss-blk-p', '⚠️ 认不出的卡片「' + (widgetId || '?') + '」—— 手机端会画成提示'));
      } else {
        box.appendChild(el('div', 'ss-blk-h', (w.module ? '〔' + w.module + '〕' : '〔外壳〕') + w.label));
        box.appendChild(el('div', 'ss-blk-p', '插件自己画这张卡（renderWidget）—— 外壳只给位置'));
      }
      break;
    }
  }
  if (limit > 0) box.appendChild(el('div', 'ss-blk-meta', '最多列 ' + limit + ' 条'));
  return box;
}

/**
 * = 手机端 `DrawerEntry.BUILTIN` —— 外壳自带的两条快捷入口。
 *
 * ⚠️ 口径逐条对齐：`pinned = true`（渲染成「🧩 📌」），`builtin = true`（长按不给删除）。
 *    以前画布上是三条无图标的假胶囊（还多了个「打开官网」），跟手机端根本对不上。
 */
const DESK_CHIPS = [
  { id: 'builtin.plugins', icon: '🧩', label: '管理插件', desc: '动作：管理插件', pinned: true, builtin: true },
  { id: 'builtin.update', icon: '⬆️', label: '检查更新', desc: '动作：检查更新', pinned: true, builtin: true },
];

/** 抽屉里的模块格（= 手机端 `Modules.enabled(prefs).filter { it.id != "settings" }`）。 */
function deskModules() {
  return C.PAGES.filter((p) => p.id !== 'settings')
    .map((p) => ({ id: p.id, label: S.skin.pageTitles[p.id] || p.label, icon: p.icon }));
}

/**
 * 喂给 WasmJS **源码渲染**画布的那份数据（localStorage 键 `shellRenderData`）。
 *
 * ⚠️ 形状必须与 `web-compose/src/wasmJsMain/kotlin/cn/threees/shell/main.kt` 的读取口径
 *    一致 —— 改了这边一定要看那边（它读不到的字段会走自己的兜底，表现是"画布少了一块"）。
 */
function shellRenderPayload() {
  const col = C.resolveColors(S.skin, S.dark, S.seed);
  const tabs = (C.PAGE_BY_ID[S.pageId] && C.PAGE_BY_ID[S.pageId].tabs) || [S.pageId];
  return {
    dark: S.dark,
    land: S.land,
    page: S.pageId,
    // ★★ 两条系统栏的高度（= 手机端的 WindowInsets）。浏览器画布没有系统栏，
    //   而手机端那两条是**系统给**的 —— 画布只能照数字画。wasm 侧把它们当**数据**
    //   （`main.kt`：默认 34/24，键在就以键为准，写 0 也认）。
    //   ⚠️ 必须写：不写 wasm 走兜底、而 `shell-render-check` 会红
    //   （它拿 wasm 读的键去对设计台写的键，差一个就报）。
    statusInset: SHELL_STATUS_INSET,
    navInset: SHELL_NAV_INSET,
    // ★ 点页面报给外层的那一档（2026-10-17）。设计台**不**用 —— 它要的是
    //   "点了就换到那一页"（`tappedPage` 那一档）。这里写 `false` 有两个原因：
    //   ① `shell-render-check` ① 要求"wasm 读的键设计台都得写"，差一个就红；
    //   ② 把"设计台走的是哪一档"写在明面上，比留在注释里可靠。
    reportPagePick: false,
    // ★ 真插件源码那一档（2026-10-17）：设计台**不**用 —— 它要自己画那一页，
    //   真插件页在画布上没有热区/框选那些设计台能力。写空串 = 关。
    livePage: '',
    // ★ 非主页时身体区让位给真机帧的那一档（2026-10-17）。设计台**不**用：
    //   它要的就是"在画布上真画那一页"（`PromoPages`），否则设计台里点一下底栏
    //   就只剩一块空白。写 `false` 的理由同上：wasm 读的键必须有显式取值。
    bodyFromShot: false,
    // 底栏（真实口径：主页居中，左右各 2 —— 与手机端 splitNav 同形状）
    nav: C.NAV_BAR.map((id) => ({ id, label: C.PAGE_BY_ID[id].label, icon: C.PAGE_BY_ID[id].icon })),
    // 下拉抽屉的两张表（= 手机端 Modules.enabled / DrawerEntry.BUILTIN + 用户那几条）
    modules: deskModules(),
    chips: DESK_CHIPS,
    // 页面壳（非主页时用）：子标签 + 当前视图 + 页面块
    // （= 手机端 PluginPageHost 的 views / viewLabels / current / conflictCount + PageBlock）
    views: tabs,
    view: (S.subTab && tabs.indexOf(S.subTab) >= 0) ? S.subTab : tabs[0],
    labels: {},
    conflictCount: 0,
    blocks: pageBlocksOf(S.pageId).map((b) => ({ at: b.at, kind: b.kind, text: b.text || '', items: b.items || [] })),
    pageIcon: (C.PAGE_BY_ID[S.pageId] && C.PAGE_BY_ID[S.pageId].icon) || '',
    colors: {
      // ⚠️ 用 hexOf（它认得 rgbToHex 已经带的 `#`）—— 写 `'#' + rgbToHex(...)`
      //    会得到 `##225790`，wasm 那边 `parseHexColor` 解析失败 → **静默回落默认色**，
      //    于是"设计台里设的颜色，画布上看不出来"。
      primary: hexOf(col.primary),
      background: hexOf(col.background),
      surface: hexOf(col.surface),
      surfaceVariant: hexOf(col.surfaceVariant),
      // ★ 顶栏 / 底栏的底色覆盖。空串 = "没设过" → wasm 那边 `parseHexColor("")`
      //   得到 null，走它自己的默认色（和手机端 `skin.topBarBg ?: …` 一个语义）。
      //   ⚠️ 必须**写这个键**（哪怕是空串）：`shell-render-check` 拿 wasm 读的键
      //   去对设计台写的键，少一个就红 —— 而它红得有道理，因为"少键"意味着
      //   wasm 会静默走兜底、画出来的颜色和手机端不一样。
      topBarBg: S.skin.topBarBg || '',
      navBg: S.skin.navBg || '',
    },
    // ★★ 没设过主页布局 → 用**手机端原本的默认**（`DefaultHomeLayouts.SIMPLE`），
    //   让 wasm 用手机端自己的渲染代码画出来。
    //   ⚠️ 原来这里是 `{ blocks: [] }` —— wasm 拿到空布局就只画一个头部，
    //      内容区一片空白，看起来像"设计台没模拟手机"。
    layout: S.skin.homeLayout || PHONE_DEFAULT_HOME_LAYOUT,
  };
}

/** 写一次渲染数据。**只有源码渲染层开着时才写** —— 没开就别去动 localStorage。 */
// ============================================================ 手机端**原本的**默认主页布局

/**
 * = `DefaultHomeLayouts.SIMPLE`，**逐字段照抄**。
 *
 * 出处：`android/app/src/main/java/cn/threees/assistant/ui/HomeLayout.kt`
 * ```
 *   object DefaultHomeLayouts {
 *     val SIMPLE = HomeLayout(padding = 14, gap = 12, blocks = listOf(
 *       HomeBlock(type = WIDGET, id = "weather",      widgetId = "weather",      span = 2),
 *       HomeBlock(type = WIDGET, id = "today-agenda", widgetId = "today-agenda", span = 2, limit = 5),
 *     ))
 *   }
 * ```
 *
 * ⚠️⚠️ 这是**从手机端代码抄过来的数据**，不是我"设计"的版式。
 *    它的唯一用途：把"手机端默认长什么样"告诉画布背景层（wasm），
 *    让**手机端自己的渲染代码**去画。
 *
 *    之前那版我**自己写了一套 HTML 卡片**当默认布局 —— 那是"照着手机端的样子画一份"，
 *    不是手机端原本的代码。用户的原话：
 *    「我让你**从原本的手机端代码**进行操作，你都会瞎创作什么」。
 *
 * ⚠️ 手机端那边改了 `SIMPLE` 要同步这里。更好的做法是让它随包/接口发过来，
 *    但那是下一步 —— 现在先保证"设计台画的就是手机端画的那一份数据"。
 */
const PHONE_DEFAULT_HOME_LAYOUT = {
  padding: 14,
  gap: 12,
  blocks: [
    { type: 'widget', id: 'weather', widgetId: 'weather', span: 2 },
    { type: 'widget', id: 'today-agenda', widgetId: 'today-agenda', span: 2, limit: 5 },
  ],
};

// ============================================================ 外壳偏移（画布整屏 ↔ 产物内容区）

// 手机端的竖屏结构（= wasm 侧 `main.kt` 那段 Column，逐段）：
//
//   ┌─ 状态栏色带  statusInset(34)  ┐  ← 系统给的高度，画布照数字画
//   ├─ 顶栏        ShellTopBar(≈59) ┤  ← padding(vertical 10×2) + 两行文字
//   ├─ 内容区      ← 元素放这儿      │
//   ├─ 底栏        ShellBottomNav(80)┤  ← Material3 NavigationBar 默认高
//   └─ 手势条色带  navInset(24)      ┘
//
// 画布画的是**整屏**（这样和源码渲染背景层精确重合），产物要塞进**内容区** ——
// 所以编译时要减掉上下两段。顶栏那个 59 是**算出来的**（不是量的）：字体度量因平台
// 略有差异，误差几 px 只影响产物顶部的留白，不影响元素之间的相对位置。
// 画布上那条虚线参考线就是把这个边界画给用户看。
const SHELL_STATUS_INSET = 34;
const SHELL_NAV_INSET = 24;
const SHELL_TOPBAR_H = 59;
const SHELL_BOTTOMBAR_H = 80;

/** 外壳（状态栏 + 顶栏 / 底栏 + 手势条）在整屏画布里的上下两段。 */
const SHELL_TOP = SHELL_STATUS_INSET + SHELL_TOPBAR_H;          // 93
const SHELL_BOTTOM = SHELL_BOTTOMBAR_H + SHELL_NAV_INSET;       // 104

// ★★ 「内容**真正**开始的地方」比外壳下沿还要低一截 —— 每个页面不一样：
//
//   · 主页：`HomePage.kt` 里先画 `HomeHeaderRow`（日期 + "↓ 查看全部功能" + 编辑），
//     再画 `HomeBlocksColumn`（它的 padding 也在上面）。两者合计 ≈ 55。
//     ⚠️ 这条是**踩过的**：一开始只按外壳下沿（93）铺默认布局，
//     结果卡片**压在"↓ 查看全部功能"上面**（用户截图里那句和"泉州"叠在一起）。
//   · 别的页面：`PageViewTabs`（子标签行）≈ 48。
//
// ⚠️ 这几个数是**按结构算的**，不是量的。误差几 px 只影响产物顶部的留白，
//    **不影响元素之间的相对位置**。画布上那条虚线标的就是这个边界。
const HOME_HEADER_H = 55;
const PAGE_TABS_H = 48;

/** 某个页面的"内容真正开始的地方"在整屏画布里的 y。 */
function contentTopFor(pageId) {
  return SHELL_TOP + (pageId === 'home' ? HOME_HEADER_H : PAGE_TABS_H);
}

function pushRenderData() {
  // ★ 两种消费者：⌨ 源码渲染那一层（`S.srcOn`），和**自由画布的背景层**
  //   （它一直开着，所以不能只看 `S.srcOn`）。
  if (!S.srcOn && !(S.mode === 'free' && FCED)) return;
  try { localStorage.setItem('shellRenderData', JSON.stringify(shellRenderPayload())); } catch (e) { /* 满了/被禁 */ }
}

/** 抽屉桌子的内容（对照 AllFunctionsDrawer：标题 + 横滚 chips + 提示 + 模块 4 列格 + 两按钮）。 */
function buildDesk(desk) {
  desk.appendChild(el('div', 'ph-d-title', '全部功能'));
  const chips = el('div', 'ph-d-chips');
  for (const c of DESK_CHIPS) {
    // = DrawerEntryChip：第一行「图标 (+ 📌)」、第二行标签，两行都是 maxLines = 1
    const chip = el('div', 'ph-d-chip');
    chip.appendChild(el('div', 'ph-d-chip-ico', c.icon + (c.pinned ? ' 📌' : '')));
    chip.appendChild(el('div', 'ph-d-chip-label', c.label));
    chips.appendChild(chip);
  }
  desk.appendChild(chips);
  desk.appendChild(el('div', 'ph-d-hint', '关掉的模块不会出现在这里（在「设置 → 模块」里开）。长按上面的入口可以置顶或删除。'));
  const grid = el('div', 'ph-d-grid');
  for (const m of deskModules()) {
    const item = el('div', 'ph-d-item');
    const ico = el('div', 'ph-nav-ico ' + (S.skin.iconShape || 'none'), m.icon);
    if (S.skin.iconShape === 'none') ico.style.background = 'transparent';
    item.appendChild(ico);
    item.appendChild(el('div', null, m.label));
    item.onclick = () => { setPull(0); const cb = $('#ss-drawer'); if (cb) cb.checked = false; S.drawer = false; openPage(m.id); };
    grid.appendChild(item);
  }
  desk.appendChild(grid);
  const btns = el('div', 'ph-d-btns');
  const bSettings = el('div', 'ph-d-btn', '设置');
  bSettings.onclick = () => { setPull(0); const cb = $('#ss-drawer'); if (cb) cb.checked = false; S.drawer = false; openPage('settings'); };
  const bEdit = el('div', 'ph-d-btn', '自定义主页');
  btns.appendChild(bSettings); btns.appendChild(bEdit);
  desk.appendChild(btns);
}

// ---- 下拉手势（HomePage.kt 的 pull 逻辑复刻：跟手 + 圆角渐现 + 过半吸附 + 回弹） ----
const PULL_DETENT = 300;   // 手机端 DRAWER_H = 344dp，画布等比
let PULL = 0;               // 当前的下拉位移（px）
let PULL_ANIM = null;       // 吸附动画帧句柄

/** 应用一次位移：sheet 跟手移动 + 顶部圆角随 progress 渐现 + 桌子 alpha 渐入。 */
function applyPull() {
  const sheet = $('.ph-sheet'), desk = $('.ph-desk');
  if (!sheet || !desk) return;
  const p = Math.min(1, PULL / PULL_DETENT);
  sheet.style.transform = 'translateY(' + PULL + 'px)';
  sheet.style.borderRadius = (18 * p) + 'px ' + (18 * p) + 'px 0 0';
  desk.style.opacity = String(p);
  desk.style.pointerEvents = PULL > 4 ? 'auto' : 'none';
}
/** 吸附到目标（带缓动，= 手机端 spring(0.85, 420) 的手感近似）。 */
function setPull(target, instant) {
  const sheet = $('.ph-sheet');
  if (!sheet) { PULL = target; return; }
  cancelAnimationFrame(PULL_ANIM);
  if (instant) { sheet.classList.remove('anim'); PULL = target; applyPull(); return; }
  sheet.classList.add('anim');
  const from = PULL, t0 = performance.now(), dur = 320;
  const step = (now) => {
    const k = Math.min(1, (now - t0) / dur);
    const e = 1 - Math.pow(1 - k, 3); // easeOutCubic
    PULL = from + (target - from) * e;
    applyPull();
    if (k < 1) PULL_ANIM = requestAnimationFrame(step);
    else sheet.classList.remove('anim');
  };
  PULL_ANIM = requestAnimationFrame(step);
}
/** 手势接线：只在「主页头部」向下拉（= 手机端 nestedScroll 滚到顶才下拉；也不和块拖拽打架）。 */
function wirePull(box) {
  if (S.land) return; // 横屏没有下拉抽屉（左栏 ☰ 树状菜单替代）
  const head = box.querySelector('.ph-home-head');
  const sheet = box.querySelector('.ph-sheet');
  if (!head || !sheet) return;
  let startY = 0, startPull = 0, dragging = false;
  head.addEventListener('pointerdown', (e) => {
    if (e.target.closest('.ph-edit-btn')) return; // 「编辑」是按钮不是拖把
    dragging = true; startY = e.clientY; startPull = PULL;
    sheet.classList.remove('anim');
    head.setPointerCapture(e.pointerId);
  });
  head.addEventListener('pointermove', (e) => {
    if (!dragging) return;
    const dy = e.clientY - startY;
    PULL = Math.max(0, Math.min(PULL_DETENT, startPull + dy));
    applyPull();
  });
  const finish = (e) => {
    if (!dragging) return;
    dragging = false;
    // 松手：过半展开、不过半弹回（= detentPx * 0.5 那条判据）
    const open = PULL > PULL_DETENT * 0.5;
    setPull(open ? PULL_DETENT : 0);
    S.drawer = open;
    const cb = $('#ss-drawer'); if (cb) cb.checked = open;
  };
  head.addEventListener('pointerup', finish);
  head.addEventListener('pointercancel', finish);
  // 展开时：点主页内容 = 收回（手机端"点一下就回去了"）
  sheet.addEventListener('click', (e) => {
    if (PULL > 4 && !e.target.closest('.ph-home-head')) { setPull(0); S.drawer = false; const cb = $('#ss-drawer'); if (cb) cb.checked = false; }
  });
}

function renderHomeCanvas(box) {
  const hl = S.skin.homeLayout;
  box.classList.add('as-home');
  box.style.background = '';
  box.style.padding = '';
  // 下层：抽屉桌子（永远在，alpha 由下拉位移驱动）
  const desk = el('div', 'ph-desk');
  if (!S.land) { buildDesk(desk); box.appendChild(desk); } // 横屏无下拉抽屉
  // 上层：主页内容 sheet（跟手位移、顶部圆角、贴主题底色）
  const sheet = el('div', 'ph-sheet');
  sheet.style.background = 'var(--ph-sheet-bg)';
  sheet.style.padding = '10px 12px 14px';
  if (S.skin.pageBg.home) sheet.style.background = S.skin.pageBg.home;
  // 主页头部（HomePage.kt 那一行，也是下拉手柄）
  const head = el('div', 'ph-home-head');
  head.innerHTML =
    '<span class="ph-date-col"><span class="ph-date">10月14日 周三</span>' +
    '<span class="ph-pull-hint">' + (S.land ? '☰ 在左上角，里面有全部功能' : '↓ 下拉看全部功能') + '</span></span>' +
    '<span class="ph-edit-btn">编辑</span>';
  head.querySelector('.ph-edit-btn').onclick = () => {
    if (!hl) { commit('开始设计主页布局', () => { homeEnsure(); }); toast('已放入天气 + 今日日程两块 —— 拖拽换位，右栏改样式', 'ok', 2600); }
    else selectPage('home');
  };
  sheet.appendChild(head);
  if (!hl) {
    // 空态：不设计 = 手机显示**原版卡片墙**（HomeWidgets.kt 的默认渲染，逐段对照）。
    const wall = el('div');
    wall.style.display = 'flex'; wall.style.flexDirection = 'column'; wall.style.gap = '10px';
    wall.innerHTML = `
      <!-- WeatherCard（surfaceVariant 底，和别的卡不一样）-->
      <div class="ph-card" style="background:var(--ph-card2)">
        <div class="ph-w-now">
          <div style="flex:1;display:flex;flex-direction:column;gap:2px">
            <span class="ph-w-place">泉州</span>
            <div class="ph-w-now"><span class="ph-w-temp">26°C</span><span class="ph-w-desc">多云</span></div>
            <span class="ph-w-place">湿度 60% · 风 3 m/s · 中国天气</span>
          </div>
          <span class="ph-more-btn">刷新</span>
        </div>
        <div class="ph-w-hours">
          <span class="ph-w-hour"><span>14:00</span><span>26°</span><span>多云</span></span>
          <span class="ph-w-hour"><span>15:00</span><span>26°</span><span>多云</span></span>
          <span class="ph-w-hour rain"><span>16:00</span><span>25°</span><span>小雨</span></span>
          <span class="ph-w-hour"><span>17:00</span><span>24°</span><span>阴</span></span>
          <span class="ph-w-hour"><span>18:00</span><span>23°</span><span>多云</span></span>
        </div>
        <div class="ph-w-day"><span style="width:52px">周三</span><span style="width:60px">多云</span><span>22~29°</span></div>
        <div class="ph-w-day"><span style="width:52px">周四</span><span style="width:60px">小雨</span><span>21~27°</span></div>
      </div>
      <!-- 今日日程（WidgetCard：surface 底 + 标题 + [全部] + MiniEventRow×n + 整宽新建按钮）-->
      <div class="ph-card">
        <div class="ph-wx-head"><span class="ph-wx-title">今日日程</span><span class="ph-more-btn">全部</span></div>
        <div class="ph-mini-row"><span class="ph-dot"></span><span class="ph-mini-time">08:00</span>
          <span class="ph-mini-col"><span>高等数学</span><span class="ph-w-place">教三 201</span></span></div>
        <div class="ph-mini-row"><span class="ph-dot" style="background:var(--ph-tertiary-container)"></span><span class="ph-mini-time">10:00</span>
          <span class="ph-mini-col"><span>物理实验</span><span class="ph-w-place">实验楼 B</span></span></div>
        <div class="ph-obtn">＋ 新建日程</div>
      </div>
      <!-- 本周课表（MiniTimetable：7 天小格）-->
      <div class="ph-card">
        <div class="ph-wx-head"><span class="ph-wx-title">本周课表</span><span class="ph-more-btn">课表</span></div>
        <div style="display:flex;gap:3px">
          <div style="flex:1;height:64px;border-radius:6px;background:var(--ph-primary-container)"></div>
          <div style="flex:1;height:64px;border-radius:6px;background:var(--ph-card2)"></div>
          <div style="flex:1;height:64px;border-radius:6px;background:var(--ph-card2)"></div>
          <div style="flex:1;height:64px;border-radius:6px;background:var(--ph-tertiary-container)"></div>
          <div style="flex:1;height:64px;border-radius:6px;background:var(--ph-card2)"></div>
          <div style="flex:1;height:64px;border-radius:6px;background:var(--ph-primary-container);opacity:.5"></div>
          <div style="flex:1;height:64px;border-radius:6px;background:var(--ph-card2)"></div>
        </div>
      </div>`;
    sheet.appendChild(wall);
    const empty = el('div', 'ss-home-empty');
    empty.appendChild(el('div', null, '上面是「未设计布局」时手机显示的**原版卡片墙**（示意）'));
    empty.appendChild(el('div', null, '皮肤不会动它 —— 想重排主页就从这里开始：'));
    const b = el('button', null, '开始设计主页布局（天气 + 今日日程起步）');
    b.onclick = () => {
      commit('开始设计主页布局', () => { homeEnsure(); });
      toast('已放入天气 + 今日日程两块 —— 拖拽换位，右栏改样式', 'ok', 2600);
    };
    empty.appendChild(b);
    sheet.appendChild(empty);
  } else {
    if (hl.background) sheet.style.background = hl.background;
    const home = el('div', 'ph-home');
    home.style.gap = (hl.gap || 0) + 'px';
    for (const b of hl.blocks) home.appendChild(homeBlockEl(b));
    sheet.appendChild(home);
    const add = el('button', 'ss-addblk', '＋ 加一块');
    add.onclick = (ev) => openMenu(ev.target, C.HOME_BLOCK_TYPES.map((t) => ({
      label: C.HOME_BLOCK_LABEL[t] + (t === 'widget' ? '（原生卡片）' : t === 'html' ? '（自由 HTML）' : ''),
      fn: () => homeAddBlock(t),
    })));
    sheet.appendChild(add);
  }
  box.appendChild(sheet);
  wirePull(box);
  // 恢复下拉状态（勾着「抽屉」时直接展开）
  PULL = (!S.land && S.drawer) ? PULL_DETENT : 0;
  applyPull();
}

// ---- 页面块区（top / bottom） ----

function pblockEl(b, pageId, at, zi, fullIdx) {
  const d = el('div', 'ss-pblk k-' + b.kind);
  d.setAttribute('data-pblk', zi);
  d.setAttribute('data-at', at);
  d.setAttribute('data-idx', fullIdx);
  d.setAttribute('data-page', pageId);
  d.draggable = true;
  d.title = '拖拽换位 / 跨页（拖到左栏某页上）';
  if (b.kind === 'divider') { d.appendChild(el('div', 'ss-blk-line')); return d; }
  if (b.kind === 'heading' || b.kind === 'text') { d.textContent = b.text || ' '; return d; }
  if (b.kind === 'card') {
    d.appendChild(el('div', null, b.text || ' '));
    if ((b.items || []).length) {
      const r = el('div', null);
      r.style.display = 'flex'; r.style.gap = '6px'; r.style.flexWrap = 'wrap';
      for (const [t] of b.items) r.appendChild(el('span', 'ss-chip2', t));
      d.appendChild(r);
    }
    return d;
  }
  // actions / links：一排小胶囊
  d.appendChild(el('span', 'ss-pblk-ico', b.kind === 'actions' ? '⚡' : '🔗'));
  for (const [t] of (b.items || [])) d.appendChild(el('span', 'ss-chip2', t));
  return d;
}

function pblockZone(pageId, at) {
  const zone = el('div', 'ph-pzone');
  zone.setAttribute('data-zone', at);
  zone.setAttribute('data-page', pageId);
  const arr = pageBlocksOf(pageId);
  let zi = 0;
  arr.forEach((b, fullIdx) => {
    if (b.at !== at) return;
    zone.appendChild(pblockEl(b, pageId, at, zi, fullIdx));
    zi++;
  });
  const add = el('button', 'ss-addblk', at === 'top' ? '＋ 顶部加块' : '＋ 底部加块');
  add.onclick = (ev) => openMenu(ev.target, C.PAGE_BLOCK_KINDS.map((k) => ({
    label: C.PAGE_BLOCK_LABEL[k],
    fn: () => pageAddBlock(pageId, at, k),
  })));
  zone.appendChild(add);
  return zone;
}

/** 各页面的**样例内容**（中间那块是"插件自己画的"占位，不可编辑）。 */
function pageSampleHtml(page) {
  const hidden = S.skin.hiddenTabs[S.pageId] || [];
  let body = '';
  if (page.tabs) {
    const tabs = page.tabs.filter((t) => !hidden.includes(t));
    const label = { list: '列表', month: '月历', timetable: '课表', conflicts: '冲突' };
    if (!tabs.includes(S.subTab)) S.subTab = tabs[0];
    body += `<div class="ph-tabs">${tabs.map((t) => `<span class="ph-tab${t === S.subTab ? ' on' : ''}">${label[t] || t}</span>`).join('')}</div>`;
  }
  switch (page.kind) {
    case 'chat':
      body += `
        <div class="ph-card hi"><div class="ph-row"><span class="ph-grow ph-t b">🤖 把这周的高数重点整理成五条</span></div>
          <div class="ph-m">对话在下面 —— 输入框、工具调用都在这一页</div></div>
        <div class="ph-card"><div class="ph-t b">回答</div>
          <div class="ph-m">① 极限的ε-δ定义 ② …（对话气泡由插件画，这里只是示意）</div></div>`;
      break;
    case 'list':
      body += `
        <div class="ph-card"><div class="ph-row"><div class="ph-accent"></div><div class="ph-grow">
          <div class="ph-t b">高等数学</div><div class="ph-m">08:00 → 09:40 · 教三 201</div></div></div></div>
        <div class="ph-card"><div class="ph-row"><div class="ph-accent"></div><div class="ph-grow">
          <div class="ph-t b">物理实验</div><div class="ph-m">10:00 → 11:40 · 实验楼 B</div></div></div></div>
        <div class="ph-card"><div class="ph-row"><div class="ph-accent"></div><div class="ph-grow">
          <div class="ph-t b">晚自习</div><div class="ph-m">19:00 → 21:00</div></div>
          <span class="ph-badge p">🎙 2 段</span></div></div>`;
      break;
    case 'grid':
      body += `<div class="ph-card"><div class="ph-t b">我的日历</div>
        <div class="ph-row" style="flex-wrap:wrap;gap:6px">
          <span class="ph-badge p">课程</span><span class="ph-badge">考试</span>
          <span class="ph-badge">社团</span><span class="ph-badge">个人</span></div></div>
        <div class="ph-card"><div class="ph-t b">团队</div><div class="ph-m">3 个成员 · 1 个共享日历</div></div>`;
      break;
    case 'notes':
      body += `
        <div class="ph-card hi"><div class="ph-row"><span class="ph-t b">录一段</span><span class="ph-m">点这里开始</span></div></div>
        <div class="ph-row" style="gap:6px"><span class="ph-tab on">全部</span><span class="ph-tab">未分类</span>
          <span class="ph-tab">🔗 已入日程</span><span class="ph-tab">数学</span></div>
        <div class="ph-card"><div class="ph-row"><div class="ph-accent"></div><div class="ph-grow">
          <div class="ph-t b">期末复习重点</div>
          <div class="ph-m">🕒 12:30 · 👥 3</div>
          <div class="ph-m">🔗 高等数学 · 10-13 08:00</div></div></div></div>`;
      break;
    case 'doc':
      body += `<div class="ph-card"><div class="ph-t b">期末复习大纲</div>
        <div class="ph-m">Markdown · 1.2k 字 · 3 张导图</div>
        <div class="ph-row" style="gap:6px"><span class="ph-badge">#数学</span><span class="ph-badge">#重点</span></div></div>
        <div class="ph-card"><div class="ph-m">一、极限与连续<br>二、导数与微分<br>三、积分</div></div>`;
      break;
    case 'plot':
      body += `<div class="ph-card"><div class="ph-t b">y = sin(x) / x</div>
        <div class="ph-m">平面 · 极坐标 · 空间 自动识别</div></div>
        <div class="ph-card" style="height:150px;align-items:center;justify-content:center">
          <div class="ph-m">（这里是函数图像）</div></div>`;
      break;
    case 'map':
      body += `<div class="ph-card"><div class="ph-t b">我在哪</div><div class="ph-m">教学楼 A · 3 层</div></div>
        <div class="ph-card" style="height:170px;align-items:center;justify-content:center">
          <div class="ph-m">（这里是校园地图 / 3D 楼块）</div></div>`;
      break;
    case 'word':
      body += `<div class="ph-card hi"><div class="ph-t b">今天要背 24 个</div>
        <div class="ph-m">四六级 + 你自己导入的教材词库</div></div>
        <div class="ph-card"><div class="ph-t b">abandon</div><div class="ph-m">/əˈbændən/ · v. 放弃，抛弃</div></div>
        <div class="ph-row" style="gap:6px"><span class="ph-tab on">认识</span><span class="ph-tab">模糊</span><span class="ph-tab">忘了</span></div>`;
      break;
    default: // settings
      body += `
        <div class="ph-card"><div class="ph-row"><span class="ph-grow ph-t">外观（外观包）</span><span class="ph-m">已启用</span></div></div>
        <div class="ph-card"><div class="ph-row"><span class="ph-grow ph-t">深色模式</span>
          <span class="ph-switch ${S.dark ? 'on' : ''}"></span></div></div>
        <div class="ph-card"><div class="ph-row"><span class="ph-grow ph-t">模块</span><span class="ph-m">11 个</span></div></div>
        <div class="ph-card"><div class="ph-row"><span class="ph-grow ph-t">账号</span><span class="ph-m">zzy</span></div></div>`;
  }
  return body;
}

function renderContent() {
  const page = C.PAGE_BY_ID[S.pageId];
  const box = $('.ph-content');
  box.innerHTML = '';
  box.classList.remove('as-home');
  box.style.padding = '';
  box.style.background = 'var(--ph-page-bg)';
  if (S.pageId === 'home') {
    renderHomeCanvas(box);
    return;
  }
  // 其他页：top 块区（可编辑）→ 样例内容（插件画的，占位）→ bottom 块区
  box.appendChild(pblockZone(S.pageId, 'top'));
  const note = el('div', 'ph-slotnote', '── 上面这块是插件画的内容（外壳改不了，块只能加在上下）──');
  box.appendChild(note);
  const slot = el('div', null);
  slot.style.display = 'flex';
  slot.style.flexDirection = 'column';
  slot.style.gap = '10px';
  slot.innerHTML = pageSampleHtml(page);
  box.appendChild(slot);
  box.appendChild(pblockZone(S.pageId, 'bottom'));
}

/** 抽屉已并入主页两层结构（v3.3：renderHomeCanvas 里的 .ph-desk + wirePull 手势），
 *  这个函数保留只为旧调用点不炸。 */
function renderDrawer() { /* desk 由 renderHomeCanvas 负责渲染 */ }

/** 给画布元素挂 data-sel / 角标 / 选中描边（点选模型）。 */
function markPhoneSel() {
  const ph = $('.ph');
  if (!ph) return;
  const set = (n, v, label) => { if (!n) return; n.setAttribute('data-sel', v); n.setAttribute('data-sel-label', label); };
  set(ph, 'theme', '主题基调');
  set($('.ph-status'), 'status', '状态栏');
  set($('.ph-top'), 'top', '顶栏');
  set($('.ph-leftrail'), 'top', '左栏（原顶栏 · 横屏）');
  set($('.ph-rightrail'), 'nav', '右栏（原底栏 · 横屏）');
  set($('.ph-nav'), 'nav', '底部导航');
  set($('.ph-content'), 'page:' + S.pageId, C.PAGE_BY_ID[S.pageId].label);
  set(ph.querySelector('.ph-desk'), 'drawer', '抽屉（下拉那层桌子）');
  ph.querySelectorAll('.ss-blk').forEach((n) => {
    const b = homeBlockById(n.getAttribute('data-blk'));
    set(n, 'hblock:' + n.getAttribute('data-blk'), hblockLabel(b));
  });
  ph.querySelectorAll('.ss-pblk').forEach((n) => {
    const b = pblockOf({ type: 'pblock', pageId: n.getAttribute('data-page'), at: n.getAttribute('data-at'), index: +n.getAttribute('data-pblk') });
    set(n, 'pblock:' + n.getAttribute('data-page') + ':' + n.getAttribute('data-at') + ':' + n.getAttribute('data-pblk'), b ? C.PAGE_BLOCK_LABEL[b.kind] : '块');
  });
  ph.querySelectorAll('.ph-nav-item').forEach((n, i) => set(n, 'page:' + C.NAV_BAR[i], C.PAGE_BY_ID[C.NAV_BAR[i]].label));
  ph.querySelectorAll('.ph-d-item').forEach((n, i) => {
    const p = C.PAGES.filter((x) => x.id !== 'settings')[i];
    if (p) set(n, 'page:' + p.id, p.label);
  });

  ph.querySelectorAll('.sel-on').forEach((n) => n.classList.remove('sel-on'));
  if (!SEL) return;
  let want;
  if (SEL.type === 'page') want = 'page:' + SEL.pageId;
  else if (SEL.type === 'hblock') want = 'hblock:' + SEL.id;
  else if (SEL.type === 'pblock') want = `pblock:${SEL.pageId}:${SEL.at}:${SEL.index}`;
  else want = SEL.type;
  ph.querySelectorAll(`[data-sel="${want}"]`).forEach((n) => n.classList.add('sel-on'));
  if (SEL.type === 'colors' || SEL.type === 'decor') ph.classList.add('sel-on');
}

function renderPhone() {
  // 横屏切换（v3.4）：整棵 .ph 变 row，竖屏的顶栏/底栏由左右栏取代（CSS .ph.land）
  const ph = $('.ph');
  if (ph) ph.classList.toggle('land', !!S.land);
  applyVars(); renderStatus(); renderTop(); renderNav(); renderContent(); renderDrawer();
  renderRails(); renderTreeMenu();
  markPhoneSel();
  const lbl = $('.ss-device-label');
  if (lbl) lbl.textContent = `${C.PAGE_BY_ID[S.pageId].label} · ${S.dark ? '深色' : '浅色'} · ${S.land ? '852×393 横屏' : '393×852'}`;
}

// ============================================================ 横屏外壳（= LandscapeShell.kt 逐段移植）

/** RailButton（= LandscapeShell.kt RailButton：72dp 宽、圆角 12、选中 primaryContainer）。 */
function railBtn(icon, label, opts, onclick) {
  const b = el('div', 'ph-railbtn' + (opts && opts.on ? ' on' : ''));
  b.appendChild(el('div', 'ic', icon));
  b.appendChild(el('div', 'lb', label));
  b.onclick = onclick || (() => {});
  return b;
}
/** 左栏（= LeftRail：3e/助手 标识 + ⟳同步 + 同步状态 + ☰全部功能 + ⚙️设置；bg=topBarBg?:surface）。 */
function renderLeftRail() {
  let rail = $('.ph-leftrail');
  if (!rail) { rail = el('div', 'ph-leftrail'); $('.ph').insertBefore(rail, $('.ph-status')); }
  rail.innerHTML = '';
  const id = el('div', 'ph-rail-id');
  id.innerHTML = '<span class="t">3e</span><span class="s">助手</span>';
  rail.appendChild(id);
  rail.appendChild(el('div', 'ph-rail-div'));
  rail.appendChild(railBtn('⟳', '同步', null, () => {}));
  rail.appendChild(el('div', 'ph-rail-sub', '刚刚同步'));
  rail.appendChild(el('div', 'ph-rail-div'));
  rail.appendChild(railBtn('☰', '全部功能', { on: !!S.landMenu }, () => { S.landMenu = !S.landMenu; renderTreeMenu(); renderLeftRail(); }));
  rail.appendChild(railBtn('⚙️', '设置', { on: S.pageId === 'settings' }, () => openPage('settings')));
  rail.appendChild(el('div', 'ph-rail-spring'));
}
/** 右栏（= RightRail：左 2 模块 + 弹性 + 🏠主页（居中）+ 弹性 + 右 2 模块；bg=navBg?:surface）。 */
function renderRightRail() {
  let rail = $('.ph-rightrail');
  if (!rail) { rail = el('div', 'ph-rightrail'); $('.ph').appendChild(rail); }
  rail.innerHTML = '';
  const nav = C.NAV_BAR; // [calendars, schedule, home, inbox, ai] —— 主页居中
  const left = nav.slice(0, 2), home = nav[2], right = nav.slice(3);
  for (const id of left) rail.appendChild(railBtn(C.PAGE_BY_ID[id].icon, C.PAGE_BY_ID[id].label, { on: id === S.pageId }, () => openPage(id)));
  rail.appendChild(el('div', 'ph-rail-spring'));
  rail.appendChild(railBtn('🏠', '主页', { on: home === S.pageId }, () => openPage(home)));
  rail.appendChild(el('div', 'ph-rail-spring'));
  for (const id of right) rail.appendChild(railBtn(C.PAGE_BY_ID[id].icon, C.PAGE_BY_ID[id].label, { on: id === S.pageId }, () => openPage(id)));
}
function renderRails() {
  renderLeftRail();
  renderRightRail();
}
/** 树状菜单（= TreeMenuHost：从内容区顶部滑下，只盖内容区；模块树导航）。 */
function renderTreeMenu() {
  const content = $('.ph-content');
  if (!content) return;
  let tm = content.querySelector('.ph-treemenu');
  if (!S.land || !S.landMenu) { if (tm) tm.remove(); return; }
  if (!tm) {
    tm = el('div', 'ph-treemenu');
    content.appendChild(tm);
  }
  tm.innerHTML = '';
  tm.appendChild(el('div', 'tm-title', '全部功能'));
  for (const p of C.PAGES) {
    if (p.id === 'settings') continue;
    const row = el('div', 'tm-row' + (p.id === S.pageId ? ' on' : ''));
    row.innerHTML = `<span>${p.icon}</span><span>${esc(S.skin.pageTitles[p.id] || p.label)}</span><span style="margin-left:auto;font-size:10px;color:var(--ph-muted)">${esc(p.desc || '')}</span>`;
    row.onclick = () => { S.landMenu = false; renderTreeMenu(); renderLeftRail(); openPage(p.id); };
    tm.appendChild(row);
  }
}

// ============================================================ 块类型菜单（加块时弹出）

let MENU = null;
function closeMenu() { if (MENU) { MENU.remove(); MENU = null; } }
function openMenu(anchor, items) { openMenuAt(anchor.getBoundingClientRect().left, anchor.getBoundingClientRect().bottom, items); }
/** 按坐标弹菜单（右键菜单在鼠标位置弹；加块按钮在按钮下方弹）。 */
function openMenuAt(x, y, items) {
  closeMenu();
  const m = el('div', 'ss-menu');
  for (const it of items) {
    // ★ v5：支持分隔线（`sep`）和危险项（`danger`，删除那类）—— 自由画布的右键菜单要用。
    if (it.sep) { m.appendChild(el('div', 'ss-menu-sep')); continue; }
    const b = el('button', it.danger ? 'danger' : null, it.label);
    if (it.disabled) { b.disabled = true; b.style.opacity = '.4'; b.style.cursor = 'default'; }
    else b.onclick = () => { closeMenu(); it.fn(); };
    m.appendChild(b);
  }
  document.body.appendChild(m);
  const mw = 176, mh = items.length * 32 + 10;
  m.style.left = Math.max(8, Math.min(window.innerWidth - mw - 8, x)) + 'px';
  m.style.top = (y + mh > window.innerHeight - 8 ? Math.max(8, y - mh - 20) : y) + 'px';
  MENU = m;
  setTimeout(() => {
    document.addEventListener('pointerdown', function once(ev) {
      if (MENU && !MENU.contains(ev.target)) closeMenu();
      document.removeEventListener('pointerdown', once);
    });
  }, 0);
}

// ============================================================ 右键菜单（v3.2）
//
// 画布里**右键任何可选对象**：编辑 / 复制 / 删除 / 移到某页 —— 和拖拽、检查器
// 是同一批操作函数的第三个入口（不是另一套实现，没有第二份逻辑）。

/** 主页块 → 其他页（dropToPageRow 情况三抽出来的公用实现）。返回 false=不可转。 */
function homeBlockToPageById(id, toPageId) {
  const src = homeBlockById(id);
  const conv = homeBlockToPageBlock(src);
  if (!conv) { toast('「卡片 / 留白 / HTML」块放不到别的页 —— 先转成标题 / 文字 / 分隔线', 'bad', 2800); return false; }
  if ((pageBlocksOf(toPageId)).length >= 8) { toast('「' + C.PAGE_BY_ID[toPageId].label + '」的块已到上限 8（手机端的硬约束）', 'bad', 2600); return false; }
  commit('主页块移到「' + C.PAGE_BY_ID[toPageId].label + '」', (sk) => {
    const h = homeLayout();
    h.blocks = h.blocks.filter((x) => x.id !== id);
    if (!h.blocks.length) sk.homeLayout = null;
    const to = sk.pageBlocks[toPageId] || (sk.pageBlocks[toPageId] = []);
    let i = 0; while (i < to.length && to[i].at === 'top') i++;
    to.splice(i, 0, conv);
    SEL = { type: 'pblock', pageId: toPageId, at: 'top', index: to.filter((x) => x.at === 'top').indexOf(conv) };
  });
  openPage(toPageId);
  return true;
}
/** 页面块 → 主页（追加到末尾）。返回 false=不可转。 */
function pageBlockToHomePage(pageId, at, index) {
  const src = pblockOf({ type: 'pblock', pageId, at, index });
  const conv = pageBlockToHomeBlock(src);
  if (!conv) { toast('「动作 / 链接 / 卡片」块放不进主页 —— 先转成标题 / 文字 / 分隔线', 'bad', 2800); return false; }
  commit('页面块移入主页', (sk) => {
    homeEnsure();
    const arr = sk.pageBlocks[pageId] || [];
    const m = arr.filter((x) => x.at === at)[index];
    if (m) {
      const fi = arr.indexOf(m);
      arr.splice(fi, 1);
      if (!arr.length) delete sk.pageBlocks[pageId];
    }
    conv.id = newBlkId('h');
    sk.homeLayout.blocks.push(conv);
    SEL = { type: 'hblock', id: conv.id };
  });
  openPage('home');
  return true;
}

/** 右键菜单的内容（按对象类型）。 */
function contextItemsFor(v) {
  if (v.indexOf('hblock:') === 0) {
    const id = v.slice(7);
    const b = homeBlockById(id);
    return [
      { label: '✏️ 编辑属性', fn: () => selectHBlock(id) },
      { label: '⧉ 复制', fn: () => homeDupBlock(id) },
      { label: '⤒ 移到最前', fn: () => homeMoveBlock(id, 0) },
      { label: '⤓ 移到最后', fn: () => homeMoveBlock(id, (homeLayout() ? homeLayout().blocks.length : 0)) },
      { label: '➡️ 移到某页…', fn: (x, y) => openMenuAt(x, y, C.PAGES.filter((p) => p.id !== 'home').map((p) => ({
        label: p.icon + ' ' + p.label,
        fn: () => homeBlockToPageById(id, p.id),
      }))) },
      { label: '🗑 删除', fn: () => homeDelBlock(id) },
    ];
  }
  if (v.indexOf('pblock:') === 0) {
    const parts = v.split(':');
    const pageId = parts[1], at = parts[2], index = +parts[3];
    const b = pblockOf({ type: 'pblock', pageId, at, index });
    const fullIdx = (S.skin.pageBlocks[pageId] || []).indexOf(b);
    const otherAt = at === 'top' ? 'bottom' : 'top';
    return [
      { label: '✏️ 编辑属性', fn: () => selectPBlock(pageId, at, index) },
      { label: '⧉ 复制', fn: () => pageDupBlock(pageId, at, index) },
      { label: at === 'top' ? '⤓ 移到页面底部区' : '⤒ 移到页面顶部区', fn: () => pageMoveBlock(pageId, fullIdx, otherAt, pageZoneCount(pageId, otherAt)) },
      { label: '➡️ 移到某页…', fn: (x, y) => openMenuAt(x, y, [
        { label: '🏠 主页（转为文字/标题块）', fn: () => pageBlockToHomePage(pageId, at, index), disabled: !pageBlockToHomeBlock(b) },
      ].concat(C.PAGES.filter((p) => p.id !== pageId && p.id !== 'home').map((p) => ({
        label: p.icon + ' ' + p.label,
        disabled: pageBlocksOf(p.id).length >= 8,
        fn: () => pageMoveToPage(pageId, fullIdx, p.id),
      })))) },
      { label: '🏠 移到主页', fn: () => pageBlockToHomePage(pageId, at, index), disabled: !pageBlockToHomeBlock(b) },
      { label: '🗑 删除', fn: () => pageDelBlock(pageId, at, index) },
    ];
  }
  // 整机对象 / 页面：右键 = 选中它去编辑
  if (v.indexOf('page:') === 0) return [{ label: '✏️ 编辑这一页', fn: () => openPage(v.slice(5)) }];
  return [{ label: '✏️ 编辑', fn: () => pick(v) }];
}

function wireContextMenu() {
  $('.ph').addEventListener('contextmenu', (e) => {
    const t = e.target.closest('[data-sel]');
    if (!t) return; // 没点在可选对象上 → 保留浏览器默认菜单
    e.preventDefault();
    const v = t.getAttribute('data-sel');
    const items = contextItemsFor(v);
    if (!items.length) return;
    openMenuAt(e.clientX, e.clientY, items);
  });
}

// ============================================================ 拖放引擎（换位 / 跨页 / 体系转换）

function clearDropMarks() {
  document.querySelectorAll('.ss-drop-before, .ss-drop-end').forEach((n) => n.classList.remove('ss-drop-before', 'ss-drop-end'));
  document.querySelectorAll('.ss-ol-item.dropok').forEach((n) => n.classList.remove('dropok'));
}
function clearDragging() {
  document.querySelectorAll('.dragging').forEach((n) => n.classList.remove('dragging'));
}

function wireDnd() {
  document.addEventListener('dragstart', (e) => {
    const hb = e.target.closest ? e.target.closest('.ss-blk') : null;
    const pb = !hb && e.target.closest ? e.target.closest('.ss-pblk') : null;
    if (!hb && !pb) return;
    e.target.classList.add('dragging');
    if (hb) DRAG = { from: 'home', id: hb.getAttribute('data-blk') };
    else DRAG = { from: 'page', pageId: pb.getAttribute('data-page'), at: pb.getAttribute('data-at'), index: +pb.getAttribute('data-pblk'), fullIdx: +pb.getAttribute('data-idx') };
    try {
      e.dataTransfer.setData('text/plain', 'ss-block');
      e.dataTransfer.effectAllowed = 'move';
    } catch (err) { /* 老浏览器 */ }
  });
  document.addEventListener('dragend', () => {
    DRAG = null;
    clearDragging(); clearDropMarks();
  });
  document.addEventListener('dragover', (e) => {
    if (!DRAG) return;
    const home = e.target.closest ? e.target.closest('.ph-home') : null;
    const zone = e.target.closest ? e.target.closest('.ph-pzone') : null;
    const pageRow = e.target.closest ? e.target.closest('.ss-ol-item[data-page]') : null;
    if (!home && !zone && !pageRow) { clearDropMarks(); return; }
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    clearDropMarks();
    if (home) {
      const blocks = [...home.querySelectorAll('.ss-blk')];
      let marked = false;
      for (const b of blocks) {
        const r = b.getBoundingClientRect();
        if (e.clientY < r.top + r.height / 2) { b.classList.add('ss-drop-before'); marked = true; break; }
      }
      if (!marked) home.classList.add('ss-drop-end');
    } else if (zone) {
      const blocks = [...zone.querySelectorAll('.ss-pblk')];
      let marked = false;
      for (const b of blocks) {
        const r = b.getBoundingClientRect();
        if (e.clientY < r.top + r.height / 2) { b.classList.add('ss-drop-before'); marked = true; break; }
      }
      if (!marked) zone.classList.add('ss-drop-end');
    } else if (pageRow) {
      pageRow.classList.add('dropok');
    }
  });
  document.addEventListener('drop', (e) => {
    if (!DRAG) return;
    e.preventDefault();
    const home = e.target.closest ? e.target.closest('.ph-home') : null;
    const zone = e.target.closest ? e.target.closest('.ph-pzone') : null;
    const pageRow = e.target.closest ? e.target.closest('.ss-ol-item[data-page]') : null;
    const drag = DRAG;
    DRAG = null;
    clearDropMarks(); clearDragging();
    if (!home && !zone && !pageRow) return;

    // ---- 情况一：落在主页画布 ----
    if (home) {
      const idx = homeDropIndex(home, e.clientY);
      if (drag.from === 'home') {
        homeMoveBlock(drag.id, idx);
      } else {
        // 页面块 → 主页块（只有 heading/text/divider 能转）
        const b = pblockOf({ type: 'pblock', pageId: drag.pageId, at: drag.at, index: drag.index });
        const conv = pageBlockToHomeBlock(b);
        if (!conv) { toast('这种块转不成主页块（按钮会丢）—— 先把它改成文字块', 'bad', 2800); return; }
        commit('页面块移入主页', (sk) => {
          const h = homeEnsure();
          const arr = sk.pageBlocks[drag.pageId] || [];
          const zoneArr = arr.filter((x) => x.at === drag.at);
          const m = zoneArr[drag.index];
          if (m) {
            const fi = arr.indexOf(m);
            arr.splice(fi, 1);
            if (!arr.length) delete sk.pageBlocks[drag.pageId];
          }
          h.blocks.splice(Math.max(0, Math.min(h.blocks.length, idx)), 0, conv);
          SEL = { type: 'hblock', id: conv.id };
        });
        openPage('home');
      }
      return;
    }
    // ---- 情况二：落在某个页面的 top/bottom 区 ----
    if (zone) {
      const toPage = zone.getAttribute('data-page');
      const toAt = zone.getAttribute('data-zone');
      const blocks = [...zone.querySelectorAll('.ss-pblk')];
      let toIdx = blocks.length;
      for (const b of blocks) {
        const r = b.getBoundingClientRect();
        if (e.clientY < r.top + r.height / 2) { toIdx = blocks.indexOf(b); break; }
      }
      if (drag.from === 'page') {
        if (drag.pageId === toPage) {
          pageMoveBlock(toPage, drag.fullIdx, toAt, toIdx);
        } else {
          // 跨页直落 zone：移动 + 改 at
          commit(`块移到「${C.PAGE_BY_ID[toPage].label}」`, (sk) => {
            const from = sk.pageBlocks[drag.pageId] || [];
            const m = from.splice(drag.fullIdx, 1)[0];
            if (!from.length) delete sk.pageBlocks[drag.pageId];
            m.at = toAt;
            const to = sk.pageBlocks[toPage] || (sk.pageBlocks[toPage] = []);
            const vis = to.map((b, i) => ({ b, i })).filter((x) => x.b.at === toAt);
            let ins;
            if (toIdx >= vis.length) {
              ins = toAt === 'top'
                ? (to.findIndex((b) => b.at === 'bottom') === -1 ? to.length : to.findIndex((b) => b.at === 'bottom'))
                : to.length;
            } else ins = vis[toIdx].i;
            to.splice(ins, 0, m);
            SEL = { type: 'pblock', pageId: toPage, at: toAt, index: to.filter((b) => b.at === toAt).indexOf(m) };
          });
          openPage(toPage);
        }
      } else {
        // 主页块 → 页面块（只有 heading/text/divider 能转）
        const b = homeBlockById(drag.id);
        const conv = homeBlockToPageBlock(b);
        if (!conv) { toast('这种块在页面里没有对应物（widget/留白/HTML 是主页专属）', 'bad', 2800); return; }
        commit('主页块移到「' + C.PAGE_BY_ID[toPage].label + '」', (sk) => {
          const h = homeLayout();
          h.blocks = h.blocks.filter((x) => x.id !== drag.id);
          if (!h.blocks.length) sk.homeLayout = null;
          conv.at = toAt;
          const to = sk.pageBlocks[toPage] || (sk.pageBlocks[toPage] = []);
          const vis = to.map((bb, i) => ({ bb, i })).filter((x) => x.bb.at === toAt);
          let ins;
          if (toIdx >= vis.length) {
            ins = toAt === 'top'
              ? (to.findIndex((bb) => bb.at === 'bottom') === -1 ? to.length : to.findIndex((bb) => bb.at === 'bottom'))
              : to.length;
          } else ins = vis[toIdx].i;
          to.splice(ins, 0, conv);
          SEL = { type: 'pblock', pageId: toPage, at: toAt, index: to.filter((bb) => bb.at === toAt).indexOf(conv) };
        });
        openPage(toPage);
      }
      return;
    }
    // ---- 情况三：落在左栏某一行 = 跨页移动 ----
    if (pageRow) {
      const toPage = pageRow.getAttribute('data-page');
      if (drag.from === 'page') {
        if (toPage === drag.pageId) { toast('它就在这页上'); return; }
        pageMoveToPage(drag.pageId, drag.fullIdx, toPage);
      } else {
        if (toPage === 'home') { toast('它就在主页上'); return; }
        const b = homeBlockById(drag.id);
        const conv = homeBlockToPageBlock(b);
        if (!conv) { toast('这种块在页面里没有对应物（widget/留白/HTML 是主页专属）', 'bad', 2800); return; }
        commit('主页块移到「' + C.PAGE_BY_ID[toPage].label + '」', (sk) => {
          const h = homeLayout();
          h.blocks = h.blocks.filter((x) => x.id !== drag.id);
          if (!h.blocks.length) sk.homeLayout = null;
          const to = sk.pageBlocks[toPage] || (sk.pageBlocks[toPage] = []);
          let i = 0; while (i < to.length && to[i].at === 'top') i++;
          to.splice(i, 0, conv);
          SEL = { type: 'pblock', pageId: toPage, at: 'top', index: to.filter((x) => x.at === 'top').indexOf(conv) };
        });
        openPage(toPage);
      }
    }
  });
}

/** 主页画布上的插入位：返回"插到第几块之前"（= blocks.length 表示追加）。 */
function homeDropIndex(home, y) {
  const blocks = [...home.querySelectorAll('.ss-blk')];
  for (const b of blocks) {
    const r = b.getBoundingClientRect();
    if (y < r.top + r.height / 2) return blocks.indexOf(b);
  }
  return blocks.length;
}

// ============================================================ 检查器（右栏）

function inspHead(box, node, extraBtns) {
  const head = el('div', 'ss-insp-head');
  head.innerHTML = `<span class="ss-insp-ico">${node.icon}</span>` +
    `<span class="ss-insp-meta"><span class="ss-insp-name">${esc(node.label)}</span>` +
    `<span class="ss-insp-count" id="ss-insp-count">已改 ${editedCount(node)} 项</span></span>`;
  for (const b of (extraBtns || [])) head.appendChild(b);
  box.appendChild(head);
}

function presetsSec() {
  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, '起手式', '<span class="ss-tag">一键套用，可撤销</span>'));
  const grid = el('div', 'ss-presets');
  for (const p of PRESETS) {
    const card = el('div', 'ss-preset');
    const dots = el('div', 'ss-dots');
    const col = C.resolveColors(C.emptySkin(), p.dark, p.seed);
    for (const k of ['background', 'surface', 'primary', 'onBackground']) {
      const d = el('i'); d.style.background = '#' + C.rgbToHex(col[k]); dots.appendChild(d);
    }
    card.appendChild(dots);
    card.appendChild(el('div', 'ss-preset-t', p.t));
    card.appendChild(el('div', 'ss-preset-d', p.d));
    card.onclick = () => applyPreset(p);
    grid.appendChild(card);
  }
  sec.appendChild(grid);
  return sec;
}

function renderOverview(box) {
  const p0 = el('p', 'ss-note');
  p0.innerHTML = '在中间画布里<b>点任意元素</b>，或从左侧大纲选一个对象 —— 右边就会列出它全部能改的属性。<b>拖拽块</b>可以换位、拖到左栏某页上可以跨页移动。Esc 取消选择。';
  box.appendChild(p0);
  const roles = Object.keys(S.skin.colors).length;
  const pages = new Set([
    ...Object.keys(S.skin.pageBg), ...Object.keys(S.skin.pageTitles),
    ...Object.keys(S.skin.pageBlocks), ...Object.keys(S.skin.hiddenTabs),
  ]);
  const pblocks = Object.keys(S.skin.pageBlocks).reduce((n, k) => n + S.skin.pageBlocks[k].length, 0);
  const p1 = el('p', 'ss-note');
  p1.innerHTML = `当前设计：覆盖 <b>${roles}</b> 个配色角色 · <b>${pages.size}</b> 个页面 · <b>${pblocks}</b> 个页面块 · 主页布局 <b>${S.skin.homeLayout ? S.skin.homeLayout.blocks.length + ' 块' : '未自定义'}</b>。`;
  box.appendChild(p1);
  box.appendChild(presetsSec());
  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, '从哪开始'));
  const btns = el('div', 'ss-btns');
  const b1 = el('button', null, '🎨 主题基调');
  b1.onclick = () => pick('theme');
  const b2 = el('button', null, '🧩 主页布局');
  b2.onclick = () => openPage('home');
  const b3 = el('button', null, '🌈 配色角色');
  b3.onclick = () => pick('colors');
  btns.appendChild(b1); btns.appendChild(b2); btns.appendChild(b3);
  sec.appendChild(btns);
  box.appendChild(sec);
}

// ---- 全局对象的检查器 ----

function inspTheme(box) {
  box.appendChild(presetsSec());
  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, '基调'));
  sec.appendChild(row('种子色', colorPicker(S.seed, (v) => { if (v) { S.seed = v; live('换种子色 ' + v, () => {}); } })));
  sec.appendChild(row('深浅', select(S.dark ? 'dark' : 'light', [['light', '浅色（背景推到 0.97）'], ['dark', '深色（推到 0.16）']], (v) => {
    S.dark = v === 'dark';
    const cb = $('#ss-dark'); if (cb) cb.checked = S.dark;
    commit(S.dark ? '切深色' : '切浅色', () => {});
  })));
  const col = C.resolveColors(S.skin, S.dark, S.seed);
  const c1 = C.contrast(col.background, col.onBackground);
  const c2 = C.contrast(col.primary, col.onPrimary);
  const note = el('p', 'ss-note');
  note.innerHTML = `<span data-contrast="bg">正文对比度 ${c1.toFixed(2)}:1${c1 >= 4.5 ? ' ✓' : ' ✗ 不到 4.5'}</span> ｜ <span data-contrast="primary">主色上的字 ${c2.toFixed(2)}:1${c2 >= 4.5 ? ' ✓' : ' ✗'}</span>`;
  sec.appendChild(note);
  const p = el('p', 'ss-note');
  p.innerHTML = '改种子色 = 换整套色调：背景/表面/主色/字色全部按手机端同一套算法重算，对比度始终达标。';
  sec.appendChild(p);
  box.appendChild(sec);
}

function inspColors(box) {
  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, '全部角色', '<span class="ss-tag">留空 = 推导</span>'));
  const col = C.resolveColors(S.skin, S.dark, S.seed);
  for (const [role, label] of C.SKIN_ROLES) {
    const cur = S.skin.colors[role] || '';
    const line = el('div', 'ss-swatch');
    line.setAttribute('data-role', role);
    line.setAttribute('data-role-label', label);
    const chip = el('div', 'ss-chip');
    chip.style.background = '#' + C.rgbToHex(col[role] || { r: 200, g: 200, b: 200 });
    line.appendChild(chip);
    line.appendChild(el('div', 'ss-name', `${label} · ${role}${cur ? '' : '（推导）'}`));
    const pick = document.createElement('input');
    pick.type = 'color'; pick.value = cur || '#' + C.rgbToHex(col[role] || { r: 200, g: 200, b: 200 });
    pick.oninput = () => setRole(role, pick.value);
    line.appendChild(pick);
    const clear = el('button', 'tiny', '↺');
    clear.title = '恢复推导';
    clear.onclick = () => { setRole(role, ''); renderInspector(); };
    line.appendChild(clear);
    sec.appendChild(line);
  }
  const p = el('p', 'ss-note');
  p.innerHTML = '⚠️ 只有这 27 个角色能直接指定（手机端白名单，写错的名字会被忽略）。「卡片（默认档）」最关键 —— Material 的 Card 取的就是它。';
  sec.appendChild(p);
  box.appendChild(sec);
}

function inspSimple(box, secTitle, rows, noteHtml) {
  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, secTitle));
  for (const r of rows) sec.appendChild(r);
  if (noteHtml) { const p = el('p', 'ss-note'); p.innerHTML = noteHtml; sec.appendChild(p); }
  box.appendChild(sec);
}

function inspStatus(box) {
  inspSimple(box, '状态栏', [
    row('图标明暗', select(S.skin.statusBar, [['auto', '自动（按背景算）'], ['light', '强制浅图标'], ['dark', '强制深图标']], (v) => patchSkin((sk) => { sk.statusBar = v; }))),
    row('底色', colorPicker(S.skin.statusBarColor, (v) => live('状态栏底色', (sk) => { sk.statusBarColor = v; }), true)),
  ], '留空 = 跟随主题背景（沉浸式）。');
}
function inspTop(box) {
  inspSimple(box, '顶栏', [
    row('底色', colorPicker(S.skin.topBarBg, (v) => live('顶栏底色', (sk) => { sk.topBarBg = v; }), true)),
  ], '应用名 + 同步 + ⚙ 那一条。笔记/导图页声明了沉浸，没有顶栏。想改某一页的标题 → 选中那一页改「页面标题」。');
}
function inspNav(box) {
  inspSimple(box, '底部导航', [
    row('底色', colorPicker(S.skin.navBg, (v) => live('底栏底色', (sk) => { sk.navBg = v; }), true)),
    row('选中色', colorPicker(S.skin.navSelected, (v) => live('底栏选中色', (sk) => { sk.navSelected = v; }), true)),
    row('文字标签', check(S.skin.navLabel, (v) => patchSkin((sk) => { sk.navLabel = v; }))),
    row('图标形状', select(S.skin.iconShape, C.ICON_SHAPES.map((s) => [s, { none: '不画底', circle: '圆形底', rounded: '圆角方底', square: '直角方底' }[s]]), (v) => patchSkin((sk) => { sk.iconShape = v; }))),
    row('图标底色', colorPicker(S.skin.iconBg, (v) => live('图标底色', (sk) => { sk.iconBg = v; }), true)),
    row('图标大小', numInput(S.skin.iconSize, 0, 34, 1, (v) => live('图标大小', (sk) => { sk.iconSize = v; }))),
  ], '改的是图标<b>下面那块底</b>，不是图标本身（那些是 emoji）。');
}
function inspDrawer(box) {
  inspSimple(box, '抽屉', [
    row('底色', colorPicker(S.skin.drawerBg, (v) => live('抽屉底色', (sk) => { sk.drawerBg = v; }), true)),
    row('预览', check(S.drawer, (v) => { S.drawer = v; const cb = $('#ss-drawer'); if (cb) cb.checked = v; renderPhone(); })),
  ], '主页下拉露出来的那层功能网格。默认别覆盖它的底色 —— 那个微妙的深度差是"下面还有一层"的全部视觉依据。');
}
function inspCard(box) {
  inspSimple(box, '卡片', [
    row('圆角', numHint(S.skin.cardRadius, 0, 32, 1, (v) => live('卡片圆角', (sk) => { sk.cardRadius = v; }), 'dp')),
    row('全局圆角缩放', slider(S.skin.cornerScale, 0, 3, 0.1, (v) => live('圆角缩放', (sk) => { sk.cornerScale = v; }))),
  ], '缩放管<b>所有</b>圆角（按钮/chip/输入框…）；圆角只管卡片。');
}
function inspText(box) {
  inspSimple(box, '文字与字体', [
    row('字体族', select(S.skin.fontFamily, C.FONT_FAMILIES.map((f) => [f, { '': '系统默认', serif: '衬线', monospace: '等宽', sans: '无衬线' }[f]]), (v) => patchSkin((sk) => { sk.fontFamily = v; }))),
    row('字重', select(S.skin.fontWeight, C.FONT_WEIGHTS.map((f) => [f, { '': '默认', light: '细', medium: '中', bold: '粗' }[f]]), (v) => patchSkin((sk) => { sk.fontWeight = v; }))),
    row('字号缩放', slider(S.skin.fontScale, 0, 1.4, 0.05, (v) => live('字号缩放', (sk) => { sk.fontScale = v; }))),
    row('字间距', slider(S.skin.letterSpacing, -0.02, 0.1, 0.005, (v) => live('字间距', (sk) => { sk.letterSpacing = v; }))),
  ], '字号只能 0.8~1.4、间距 -0.02~0.10 —— 手机端夹过的范围。');
}
function inspDecor(box) {
  inspSimple(box, '背景纹理', [
    row('纹理', select(S.skin.decor, C.DECORS.map((d) => [d, C.DECOR_LABEL[d]]), (v) => patchSkin((sk) => { sk.decor = v; }, '换纹理'))),
    row('纹理色', colorPicker(S.skin.decorColor, (v) => live('纹理色', (sk) => { sk.decorColor = v; }), true)),
  ], '纹理<b>必须极淡</b>（手机端固定 6% 不透明度、不开放配置）。');
}

// ---- 页面的检查器 ----

function inspHomePage(box) {
  const hl = S.skin.homeLayout;
  if (!hl) {
    const sec = el('div', 'ss-sec');
    sec.appendChild(el('h3', null, '主页布局', '<span class="ss-tag">未自定义</span>'));
    const p = el('p', 'ss-note');
    p.innerHTML = '主页还没有自定义布局 —— 手机上显示的是你自己排的卡片墙（「编辑」里挑的那种）。皮肤带了布局才会接管主页内容区。';
    sec.appendChild(p);
    const btns = el('div', 'ss-btns');
    const b = el('button', 'primary', '开始设计（放入天气 + 今日日程）');
    b.onclick = () => { commit('开始设计主页布局', () => { homeEnsure(); }); };
    btns.appendChild(b);
    sec.appendChild(btns);
    box.appendChild(sec);
    return;
  }
  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, '主页布局', `<span class="ss-tag">${hl.blocks.length} 块</span>`));
  sec.appendChild(row('页面底色', colorPicker(hl.background, (v) => homeSetLayoutField('background', v, '底色'), true)));
  sec.appendChild(row('内边距', numHint(hl.padding, 0, 48, 1, (v) => homeSetLayoutField('padding', v, '内边距'), 'dp')));
  sec.appendChild(row('块间距', numHint(hl.gap, 0, 48, 1, (v) => homeSetLayoutField('gap', v, '块间距'), 'dp')));
  const p = el('p', 'ss-note');
  p.innerHTML = '块在画布里<b>拖拽换位</b>；半宽两块自动并排、整宽独占一行（和手机端的排法一致）。';
  sec.appendChild(p);
  box.appendChild(sec);

  const sec2 = el('div', 'ss-sec');
  sec2.appendChild(el('h3', null, '这一套布局'));
  const btns = el('div', 'ss-btns');
  const add = el('button', null, '＋ 加一块');
  add.onclick = (ev) => openMenu(ev.target, C.HOME_BLOCK_TYPES.map((t) => ({
    label: C.HOME_BLOCK_LABEL[t], fn: () => homeAddBlock(t),
  })));
  const clear = el('button', 'danger', '清空布局（回原版卡片墙）');
  clear.onclick = () => {
    if (!confirm('清空主页布局？手机端会回到你自己排的卡片墙。（Ctrl+Z 可撤销）')) return;
    commit('清空主页布局', (sk) => { sk.homeLayout = null; });
  };
  btns.appendChild(add); btns.appendChild(clear);
  sec2.appendChild(btns);
  box.appendChild(sec2);
}

function inspOtherPage(box, page) {
  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, '这一页的设计', '<span class="ss-tag">只影响这个页面</span>'));
  sec.appendChild(row('页面底色', colorPicker(S.skin.pageBg[page.id] || '', (v) => setPageBg(v), true)));
  sec.appendChild(row('页面标题', textInput(S.skin.pageTitles[page.id] || '', (v) => setPageTitle(v), page.label)));
  const p = el('p', 'ss-note');
  p.innerHTML = `id = ${page.id} ｜ ${esc(page.desc)} ｜ 只覆盖外壳画的标题，插件自己画的字碰不到。`;
  sec.appendChild(p);
  box.appendChild(sec);

  const sec2 = el('div', 'ss-sec');
  const nTop = pageZoneCount(page.id, 'top'), nBot = pageZoneCount(page.id, 'bottom');
  sec2.appendChild(el('h3', null, '页面上下块', `<span class="ss-tag">上 ${nTop} · 下 ${nBot} / 8</span>`));
  const p2 = el('p', 'ss-note');
  p2.innerHTML = '在画布里<b>点块编辑、拖拽换位</b>；拖到左栏其他页上 = 移到那一页。块加在插件内容的上下 —— 不能遮插件自己画的东西。';
  sec2.appendChild(p2);
  const btns = el('div', 'ss-btns');
  for (const at of ['top', 'bottom']) {
    const b = el('button', null, (at === 'top' ? '＋ 顶部' : '＋ 底部'));
    b.onclick = (ev) => openMenu(ev.target, C.PAGE_BLOCK_KINDS.map((k) => ({
      label: C.PAGE_BLOCK_LABEL[k], fn: () => pageAddBlock(page.id, at, k),
    })));
    btns.appendChild(b);
  }
  sec2.appendChild(btns);
  box.appendChild(sec2);

  if (page.tabs) {
    const sec3 = el('div', 'ss-sec');
    sec3.appendChild(el('h3', null, '收起子标签'));
    const hidden = S.skin.hiddenTabs[page.id] || [];
    for (const t of page.tabs) {
      const on = hidden.includes(t);
      const b = el('button', 'tiny' + (on ? ' primary' : ''), { list: '列表', month: '月历', timetable: '课表', conflicts: '冲突' }[t] || t);
      b.onclick = () => patchSkin((sk) => {
        const cur = sk.hiddenTabs[page.id] || [];
        const next = on ? cur.filter((x) => x !== t) : cur.concat(t);
        if (next.length) sk.hiddenTabs[page.id] = next; else delete sk.hiddenTabs[page.id];
      }, (on ? '恢复子标签 ' : '收起子标签 ') + t);
      sec3.appendChild(b);
    }
    const p3 = el('p', 'ss-note');
    p3.innerHTML = '是"收起来"不是"删掉"：被收的视图仍可通过 viewId 打开（它可能挂着通知）。';
    sec3.appendChild(p3);
    box.appendChild(sec3);
  }
}

// ---- 主页块的检查器 ----

function inspHBlock(box, b) {
  const dup = el('button', 'tiny', '⧉ 复制');
  dup.onclick = () => homeDupBlock(b.id);
  const del = el('button', 'tiny danger', '🗑 删除');
  del.onclick = () => homeDelBlock(b.id);
  inspHead(box, { icon: '🧩', label: hblockLabel(b) }, [dup, del]);

  const sec0 = el('div', 'ss-sec');
  sec0.appendChild(el('h3', null, '类型', `<span class="ss-tag">${C.HOME_BLOCK_LABEL[b.type]}</span>`));
  sec0.appendChild(row('类型', select(b.type, C.HOME_BLOCK_TYPES.map((t) => [t, C.HOME_BLOCK_LABEL[t]]), (v) => {
    commit('改块类型 → ' + C.HOME_BLOCK_LABEL[v], () => {
      const blk = homeBlockById(b.id); if (!blk) return;
      blk.type = v;
      if (v === 'widget' && !blk.widgetId) blk.widgetId = 'weather';
      if (v === 'html' && !blk.html) blk.html = '<div style="font-size:13px;padding:6px">HTML</div>';
      if (v === 'spacer' && !blk.height) blk.height = 24;
    });
  })));
  if (b.type === 'widget') {
    sec0.appendChild(row('卡片', select(b.widgetId, C.HOME_WIDGETS.map((w) => [w.id, w.label + (w.module ? '（' + w.module + '）' : '（外壳）')]), (v) => commit('换卡片 → ' + v, () => {
      const blk = homeBlockById(b.id); if (blk) blk.widgetId = v;
    }))));
    sec0.appendChild(row('最多列几条', numHint(b.limit, 0, 50, 1, (v) => homeSetBlockField(b.id, 'limit', v, '条数'), '0=默认')));
    sec0.appendChild(row('显示哪些', textInput((b.show || []).join(','), (v) => homeSetBlockField(b.id, 'show', v.split(',').map((x) => x.trim()).filter(Boolean), '显示项'), '逗号分隔，空=卡片默认')));
  }
  if (b.type === 'heading' || b.type === 'text') {
    const ta = document.createElement('textarea');
    ta.value = b.text;
    ta.oninput = () => homeSetBlockField(b.id, 'text', ta.value, '文字');
    sec0.appendChild(row('文字', ta));
    sec0.appendChild(row('字号', numHint(b.fontSize, 0, 48, 1, (v) => homeSetBlockField(b.id, 'fontSize', v, '字号'), 'sp，0=默认')));
  }
  if (b.type === 'spacer') {
    sec0.appendChild(row('高度', numHint(b.height, 0, 400, 1, (v) => homeSetBlockField(b.id, 'height', v, '高度'), 'dp')));
  }
  if (b.type === 'html') {
    const ta = document.createElement('textarea');
    ta.value = b.html;
    ta.style.minHeight = '120px';
    ta.oninput = () => homeSetBlockField(b.id, 'html', ta.value, 'HTML');
    sec0.appendChild(row('HTML', ta));
    sec0.appendChild(row('高度', numHint(b.height, 0, 600, 10, (v) => homeSetBlockField(b.id, 'height', v, '高度'), 'dp')));
    const ph = el('p', 'ss-note');
    ph.innerHTML = '手机端 WebView：禁网 + 禁文件 + 只读数据桥（window.threees.data/call）。图片要自包含（data: 或内联 SVG）。';
    sec0.appendChild(ph);
  }
  box.appendChild(sec0);

  const sec = el('div', 'ss-sec');
  sec.appendChild(el('h3', null, '布局与外观'));
  sec.appendChild(row('跨度', select(String(b.span), [['1', '半宽（两块并排）'], ['2', '整宽（独占一行）']], (v) => commit('改跨度', () => {
    const blk = homeBlockById(b.id); if (blk) blk.span = +v;
  }))));
  sec.appendChild(row('高度倍率', slider(b.heightFactor, 0.3, 4, 0.1, (v) => homeSetBlockField(b.id, 'heightFactor', v, '高度倍率'))));
  sec.appendChild(row('背景色', colorPicker(b.bg, (v) => homeSetBlockField(b.id, 'bg', v, '背景'), true)));
  sec.appendChild(row('文字色', colorPicker(b.fg, (v) => homeSetBlockField(b.id, 'fg', v, '文字色'), true)));
  sec.appendChild(row('圆角', numHint(b.radius, 0, 48, 1, (v) => homeSetBlockField(b.id, 'radius', v, '圆角'), 'dp')));
  sec.appendChild(row('内边距', numHint(b.pad, -1, 48, 1, (v) => homeSetBlockField(b.id, 'pad', v, '内边距'), '-1=默认')));
  sec.appendChild(row('对齐', select(b.align, [['', '默认'], ['left', '左'], ['center', '中'], ['right', '右']], (v) => commit('改对齐', () => {
    const blk = homeBlockById(b.id); if (blk) blk.align = v;
  }))));
  const pw = el('p', 'ss-note');
  pw.innerHTML = b.type === 'widget'
    ? '⚠️ 原生卡片的背景只能套在外层容器（卡片有自己的配色）；想让卡片换配色，用 HTML 块自己画一张。'
    : '颜色留空 = 跟随主题。';
  sec.appendChild(pw);
  box.appendChild(sec);
}

// ---- 页面块的检查器 ----

function setItem(items, i, slot, v) {
  const out = (items || []).map((x) => [x[0], x[1]]);
  if (out[i]) out[i][slot] = v;
  return out;
}

function inspPBlock(box, b, sel) {
  const page = C.PAGE_BY_ID[sel.pageId];
  const dup = el('button', 'tiny', '⧉ 复制');
  dup.onclick = () => pageDupBlock(sel.pageId, sel.at, sel.index);
  const del = el('button', 'tiny danger', '🗑 删除');
  del.onclick = () => pageDelBlock(sel.pageId, sel.at, sel.index);
  inspHead(box, { icon: '📦', label: C.PAGE_BLOCK_LABEL[b.kind] + ' · ' + page.label + (sel.at === 'top' ? '·顶部' : '·底部') }, [dup, del]);

  const sec0 = el('div', 'ss-sec');
  sec0.appendChild(el('h3', null, '这一块'));
  sec0.appendChild(row('类型', select(b.kind, C.PAGE_BLOCK_KINDS.map((k) => [k, C.PAGE_BLOCK_LABEL[k]]), (v) => {
    if (v === b.kind) return;
    commit('页面块改类型 → ' + C.PAGE_BLOCK_LABEL[v], (sk) => {
      const arr = sk.pageBlocks[sel.pageId] || [];
      const zone = arr.filter((x) => x.at === sel.at);
      const m = zone[sel.index];
      if (!m) return;
      m.kind = v;
      if (v === 'actions' && !(m.items || []).length) m.items = [['检查更新', 'check_update']];
      if (v === 'links' && !(m.items || []).length) m.items = [['官网', 'https://threees.cn']];
      if (v === 'heading' && !m.text) m.text = '标题';
    });
  })));
  sec0.appendChild(row('位置', select(sel.at, [['top', '顶部（正文之前）'], ['bottom', '底部（正文之后）']], (v) => {
    if (v === sel.at) return;
    pageMoveBlock(sel.pageId, pageBlocksOf(sel.pageId).indexOf(b), v, pageZoneCount(sel.pageId, v));
  })));
  if (b.kind !== 'divider' && b.kind !== 'actions' && b.kind !== 'links') {
    const ta = document.createElement('textarea');
    ta.value = b.text;
    ta.oninput = () => pageSetBlockField(sel, 'text', ta.value, '文字');
    sec0.appendChild(row('文字', ta));
  }
  if (b.kind === 'actions' || b.kind === 'links' || b.kind === 'card') {
    const sub = el('div', 'ss-note', b.kind === 'links' ? '按钮项（点了打开网址）' : b.kind === 'actions' ? '按钮项（点了执行动作）' : '卡片里的小按钮');
    sec0.appendChild(sub);
    const list = el('div');
    const renderItems = () => {
      list.innerHTML = '';
      (b.items || []).forEach(([t, u], i) => {
        const r = el('div', 'ss-row');
        r.appendChild(el('label', null, '按钮 ' + (i + 1)));
        const f = el('div', 'ss-field');
        f.appendChild(textInput(t, (v) => pageSetBlockField(sel, 'items', setItem(b.items, i, 0, v), '按钮文字'), '文字'));
        if (b.kind === 'actions') {
          f.appendChild(select(u, C.PAGE_ACTIONS.map((a) => [a, C.PAGE_ACTION_LABEL[a]]), (v) => pageSetBlockField(sel, 'items', setItem(b.items, i, 1, v), '按钮动作')));
        } else {
          f.appendChild(textInput(u, (v) => pageSetBlockField(sel, 'items', setItem(b.items, i, 1, v), '按钮网址'), 'https://…'));
        }
        const x = el('button', 'tiny danger', '✕');
        x.onclick = () => commit('删一个按钮', () => {
          const arr = S.skin.pageBlocks[sel.pageId] || [];
          const zone = arr.filter((x2) => x2.at === sel.at);
          const m = zone[sel.index];
          if (m) m.items = m.items.filter((_, j) => j !== i);
        });
        f.appendChild(x);
        r.appendChild(f);
        list.appendChild(r);
      });
    };
    renderItems();
    sec0.appendChild(list);
    const addI = el('button', 'tiny', '＋ 加一个按钮');
    addI.onclick = () => commit('加一个按钮', () => {
      const arr = S.skin.pageBlocks[sel.pageId] || [];
      const zone = arr.filter((x) => x.at === sel.at);
      const m = zone[sel.index];
      if (!m) return;
      m.items = (m.items || []).concat([[b.kind === 'links' ? '链接' : '按钮', b.kind === 'links' ? 'https://threees.cn' : 'check_update']]);
    });
    sec0.appendChild(addI);
  }
  const p0 = el('p', 'ss-note');
  p0.innerHTML = '块加在插件内容的上下。把它拖到左栏其他页上 = 移到那一页。';
  sec0.appendChild(p0);
  box.appendChild(sec0);
}

// ---- 检查器入口 ----

function renderInspector() {
  const box = $('#ss-inspector');
  const sub = $('#ss-insp-sub');
  box.innerHTML = '';
  if (!SEL) {
    if (sub) sub.textContent = '未选中对象';
    renderOverview(box);
    decorate(box);
    return;
  }
  const node = nodeOfSel();
  if (sub) sub.textContent = (node && (node.sub || '')) || '';
  if (SEL.type === 'page') {
    inspHead(box, node, null);
    if (SEL.pageId === 'home') inspHomePage(box);
    else inspOtherPage(box, C.PAGE_BY_ID[SEL.pageId]);
  } else if (SEL.type === 'hblock') {
    const b = homeBlockById(SEL.id);
    if (!b) { SEL = null; renderInspector(); return; }
    inspHBlock(box, b);
  } else if (SEL.type === 'pblock') {
    const b = pblockOf(SEL);
    if (!b) { SEL = null; renderInspector(); return; }
    inspPBlock(box, b, SEL);
  } else {
    inspHead(box, node, null);
    switch (SEL.type) {
      case 'theme': inspTheme(box); break;
      case 'colors': inspColors(box); break;
      case 'status': inspStatus(box); break;
      case 'top': inspTop(box); break;
      case 'nav': inspNav(box); break;
      case 'drawer': inspDrawer(box); break;
      case 'card': inspCard(box); break;
      case 'text': inspText(box); break;
      case 'decor': inspDecor(box); break;
    }
  }
  decorate(box);
  if (SEL && SEL.type === 'colors') wireRoleSearch(box);
}

// ============================================================ 打包 / 发布 / 导入（formatVersion 2）

/** 组装外观包。皮肤正文用手机端 UiSkin.parse 的 snake_case 口径。 */
function buildPack() {
  const sk = S.skin;
  const skin = {
    status_bar: sk.statusBar,
    status_bar_color: sk.statusBarColor,
    top_bar_bg: sk.topBarBg,
    nav_bg: sk.navBg,
    nav_selected: sk.navSelected,
    nav_label: sk.navLabel,
    drawer_bg: sk.drawerBg,
    card_radius: sk.cardRadius,
    icon_shape: sk.iconShape,
    icon_bg: sk.iconBg,
    icon_size: sk.iconSize,
    font_scale: sk.fontScale,
    font_weight: sk.fontWeight,
    letter_spacing: sk.letterSpacing,
    font_family: sk.fontFamily,
    page_bg: sk.pageBg,
    colors: sk.colors,
    decor: sk.decor,
    decor_color: sk.decorColor,
    corner_scale: sk.cornerScale,
    page_titles: sk.pageTitles,
    page_blocks: sk.pageBlocks,
    hidden_tabs: sk.hiddenTabs,
  };
  // ★★ v5：自由画布 → **编译成 html 块**。
  //
  //   为什么是 html 块而不是给手机端发明一套"元素"：手机端早就有一个
  //   WebView 渲染的 html 块（`HomeBlock(type=html)` / 新加的 `PageBlock(kind=html)`），
  //   它禁网、禁文件、只给一个只读数据桥 —— 正好是"用户自己画的东西"该待的沙箱。
  //   于是**两端不存在"画布模型要同步"的维护负担**：手机端只认一段 HTML。
  //
  //   ⚠️ 覆盖规则：**只有真的画过的页面**才覆盖。
  //      没画过的页面照旧走它原来的块流 —— 这样"自由画布取代块流"不会
  //      把用户以前做好的老设计一次性删掉（想删就去那一页清空画布）。
  const fl = sk.freeLayers || {};
  const flColors = (() => { try { return C.resolveColors(S.skin, S.dark, S.seed); } catch (e) { return {}; } })();
  const flPages = Object.keys(fl).filter((pid) => {
    const L = fl[pid];
    return L && (L.els.length > 0 || (L.code || '').trim() !== '');
  });
  if (flPages.length) {
    skin.page_blocks = Object.assign({}, sk.pageBlocks);
    if (flPages.includes('home')) {
      const L = fl.home;
      // 主页走 homeLayout：**整页就一块 html**（padding/gap 归零，画布自己铺满）。
      skin.home_layout = {
        background: '', padding: 0, gap: 0,
        blocks: [{
          type: 'html', id: 'fc-home', span: 2,
          height: Math.round(L.h), heightFactor: 1,
          bg: '', fg: '', fontSize: 0, radius: 0, pad: 0, align: '',
          text: '', widgetId: '', show: [], limit: 0,
          html: FC.compile(L, { colors: flColors, shellTop: contentTopFor(S.pageId), shellBottom: SHELL_BOTTOM }),
        }],
      };
    }
    for (const pid of flPages) {
      if (pid === 'home') continue;
      const L = fl[pid];
      skin.page_blocks[pid] = [{
        at: 'top', kind: 'html', text: '', items: [],
        html: FC.compile(L, { colors: flColors, shellTop: SHELL_TOP, shellBottom: SHELL_BOTTOM }),
        height: Math.round(L.h),
      }];
    }
  }
  if (sk.homeLayout && !skin.home_layout) skin.home_layout = sk.homeLayout;   // 对象形态（手机端两种都吃）

  // ★ v5：**画布本身**也要进包（不只是编译产物）。
  //   理由：管理台「在设计台里改 ↗」是把包送回设计台的 —— 只带编译产物的话，
  //   再打开时看到的是"一段 HTML"，用户就再也拖不动了（只能进代码模式）。
  //   带上 freeLayers 才能"打开还能接着拖"。
  //   ⚠️ 手机端不认识这个字段，会原样忽略（它是设计台的编辑态，不是渲染输入）。
  //      代价是包大一点（元素数组 + 编译产物各一份）—— 换的是"可再编辑"，值。
  if (Object.keys(sk.freeLayers || {}).length) skin.free_layers = sk.freeLayers;
  const p = {
    format: 'threees.skinpack',
    formatVersion: 2,
    packId: (S.pack.packId || '').trim(),
    name: (S.pack.name || '').trim(),
    version: (S.pack.version || '1.0.0').trim(),
    updatedAt: Math.floor(Date.now() / 1000),
    seed: S.seed,
    dark: S.dark,
    skin,
  };
  if ((S.pack.note || '').trim()) p.note = S.pack.note.trim();
  if ((S.pack.author || '').trim()) p.author = S.pack.author.trim();
  return p;
}

/** 导出前体检（不是第二份权威校验，最终判据在服务端/手机端）。 */
function preflight(p) {
  const out = [];
  const bad = (m) => out.push({ level: 'bad', msg: m });
  const warn = (m) => out.push({ level: 'warn', msg: m });
  const info = (m) => out.push({ level: 'ok', msg: m });
  if (!/^[a-z0-9][a-z0-9-]{2,31}$/.test(p.packId)) bad('包标识（= 授权码）要是 3~32 位小写字母/数字/中划线，且以字母数字开头');
  if (!p.name) bad('名称不能空');
  else if (p.name.length > 24) warn('名称超过 24 字，手机端列表里会被省略');
  if (!/^\d+\.\d+(\.\d+)?$/.test(p.version)) warn('版本号建议写成 1.0.0 这样');
  if (!C.hexToRgb(p.seed)) bad('种子色不是 #RRGGBB');
  const col = C.resolveColors(S.skin, p.dark, p.seed);
  if (C.contrast(col.background, col.onBackground) < 4.5) bad('正文对比度不到 4.5:1');
  else info(`正文对比度 ${C.contrast(col.background, col.onBackground).toFixed(2)}:1 ✓`);
  if (C.contrast(col.primary, col.onPrimary) < 4.5) bad('主色上的字对比度不到 4.5:1');
  else info(`主色上的字 ${C.contrast(col.primary, col.onPrimary).toFixed(2)}:1 ✓`);
  const unknown = Object.keys(S.skin.pageBg).concat(Object.keys(S.skin.pageTitles))
    .concat(Object.keys(S.skin.pageBlocks)).concat(Object.keys(S.skin.hiddenTabs))
    .filter((id) => !C.PAGE_BY_ID[id]);
  if (unknown.length) warn('认不出的页面 id（手机端会忽略，等于没设）：' + [...new Set(unknown)].join('、'));
  const roles = Object.keys(S.skin.colors).filter((k) => !C.SKIN_ROLE_KEYS.includes(k));
  if (roles.length) warn('认不出的配色角色（手机端会忽略）：' + roles.join('、'));

  // ---- v2：布局体检 ----
  const hl = S.skin.homeLayout;
  if (hl) {
    info(`主页布局：${hl.blocks.length} 块`);
    const ws = hl.blocks.filter((b) => b.type === 'widget' && !C.HOME_WIDGET_BY_ID[b.widgetId]);
    if (ws.length) warn('认不出的卡片（手机端会显示"认不出这张卡片"）：' + ws.map((b) => b.widgetId).join('、'));
    const ids = hl.blocks.map((b) => b.id).filter(Boolean);
    const dup = ids.filter((x, i) => ids.indexOf(x) !== i);
    if (dup.length) warn('主页块有重复 id：' + [...new Set(dup)].join('、'));
    if (!hl.blocks.length) warn('主页布局是空的 —— 手机端会当"不带布局"处理');
  }
  for (const [pg, arr] of Object.entries(S.skin.pageBlocks)) {
    if (arr.length > 8) warn(`「${C.PAGE_BY_ID[pg] ? C.PAGE_BY_ID[pg].label : pg}」有 ${arr.length} 块 —— 手机端只取前 8 块`);
    for (const b of arr) {
      if (b.kind === 'actions') {
        const badA = (b.items || []).filter(([, t]) => !C.PAGE_ACTIONS.includes(t));
        if (badA.length) bad(`动作目标不在白名单（整块会被手机端丢弃）：${badA.map(([, t]) => t).join('、')}`);
      }
      if (b.kind === 'links') {
        const badL = (b.items || []).filter(([, u]) => !/^https?:\/\//.test(u));
        if (badL.length) bad(`链接必须 http(s) 开头（整块会被手机端丢弃）：${badL.map(([, u]) => u).join('、')}`);
      }
    }
  }
  // ---- v5：自由画布体检 ----
  const fl2 = S.skin.freeLayers || {};
  const flKeys = Object.keys(fl2).filter((k) => fl2[k] && (fl2[k].els.length || (fl2[k].code || '').trim()));
  if (flKeys.length) {
    let total = 0, bytes = 0, actNoArg = 0, badAct = 0;
    for (const pid of flKeys) {
      const L = fl2[pid];
      total += L.els.length;
      for (const e of L.els) {
        if (e.src && e.src.length > bytes) bytes += e.src.length;
        if (e.act && e.act !== 'js' && !String(e.arg || '').trim()) actNoArg++;
        if (e.act === 'url' && !/^https?:\/\//.test(String(e.arg || ''))) badAct++;
      }
    }
    info(`自由画布：${flKeys.length} 个页面、共 ${total} 个元素`);
    // ⚠️ 这条必须说：html 块是**新版手机端**才认的 kind。旧版（≤ 0.12.29）
    //    的 `PageBlock.parse` 会把它当"认不出的 kind"直接丢掉 —— **不报错**，
    //    用户看到的是"我设计的页面没生效"。所以在这里先讲清楚。
    info('⚠️ 这一包用了自由画布（html 块）—— 手机端需要 0.12.30 及以上；旧版本会静默丢掉这些块');
    if (bytes > 600 * 1024) {
      warn(`内联图片合计约 ${Math.round(bytes / 1024)} KB —— 外观包会明显变大（手机端禁网，图片只能内联）`);
    }
    if (actNoArg) warn(`${actNoArg} 个元素绑了动作但没填参数（点下去不会有反应）`);
    if (badAct) bad(`${badAct} 个「打开网址」的目标不是 http(s) 开头（点下去不会有反应）`);
    const miss = flKeys.filter((k) => !C.PAGE_BY_ID[k]);
    if (miss.length) warn('认不出的页面 id（手机端会忽略）：' + miss.join('、'));
  }

  const pages = new Set(Object.keys(S.skin.pageBg).concat(Object.keys(S.skin.pageTitles))
    .concat(Object.keys(S.skin.pageBlocks)).concat(Object.keys(S.skin.hiddenTabs)));
  if (pages.size === 0 && !hl) warn('没有任何页面级设计 —— 这一包只改了全局外观');
  else info(`覆盖了 ${pages.size} 个页面${hl ? ' + 主页布局' : ''}`);
  return out;
}

function renderPackPanel() {
  const box = $('#ss-panel-pack');
  if (!box) return;
  box.innerHTML = '';
  if (AUTHCODE) {
    box.appendChild(el('div', 'ss-code-big', AUTHCODE));
    const ph = el('p', 'ss-note');
    ph.innerHTML = '上次发布成功的授权码 —— 手机端「更多外观」→ 按授权码下载 → 加载。';
    box.appendChild(ph);
  }
  const sec = el('div');
  sec.appendChild(el('h3', null, '这一包是谁'));
  sec.appendChild(row('包标识', textInput(S.pack.packId, (v) => { S.pack.packId = v; save(); }, '如 maths-blue')));
  sec.appendChild(row('名称', textInput(S.pack.name, (v) => { S.pack.name = v; save(); }, '如 数学蓝')));
  sec.appendChild(row('版本', textInput(S.pack.version, (v) => { S.pack.version = v; save(); }, '1.0.0')));
  sec.appendChild(row('作者', textInput(S.pack.author, (v) => { S.pack.author = v; save(); }, '可空')));
  sec.appendChild(row('说明', textInput(S.pack.note, (v) => { S.pack.note = v; save(); }, '可空')));
  const p0 = el('p', 'ss-note');
  p0.innerHTML = '<b>包标识就是授权码</b> —— 手机端"按授权码下载"输的就是它（和导航数据包同一条口径）。';
  sec.appendChild(p0);
  box.appendChild(sec);

  const sec2 = el('div', 'ss-sec');
  sec2.appendChild(el('h3', null, '体检'));
  const list = el('div');
  for (const r of preflight(buildPack())) {
    const cls = r.level === 'bad' ? 'ss-bad' : r.level === 'warn' ? 'ss-note' : 'ss-ok';
    list.appendChild(el('div', cls, (r.level === 'bad' ? '✗ ' : r.level === 'warn' ? '! ' : '✓ ') + r.msg));
  }
  sec2.appendChild(list);
  box.appendChild(sec2);

  const sec3 = el('div', 'ss-sec');
  sec3.appendChild(el('h3', null, '导出 / 导入'));
  const btns = el('div', 'ss-btns');
  const bExport = el('button', 'primary', '⬇ 导出外观包');
  bExport.onclick = doExport;
  const bCopy = el('button', null, '复制 JSON');
  bCopy.onclick = () => { navigator.clipboard.writeText(JSON.stringify(buildPack(), null, 2)); bCopy.textContent = '已复制 ✓'; setTimeout(() => { bCopy.textContent = '复制 JSON'; }, 1400); };
  const bImport = el('button', null, '📂 打开包文件');
  const file = document.createElement('input');
  file.type = 'file'; file.accept = '.json,.skinpack';
  file.style.display = 'none';
  file.onchange = () => {
    const f = file.files && file.files[0];
    if (!f) return;
    const fr = new FileReader();
    fr.onload = () => applyPackText(String(fr.result), '文件');
    fr.readAsText(f);
  };
  bImport.onclick = () => file.click();
  btns.appendChild(bExport); btns.appendChild(bCopy); btns.appendChild(bImport); btns.appendChild(file);
  sec3.appendChild(btns);
  const ta = document.createElement('textarea');
  ta.placeholder = '也可以把外观包 JSON 粘在这里，然后点「载入」';
  ta.style.marginTop = '8px';
  const bLoad = el('button', 'tiny', '载入粘贴的内容');
  bLoad.onclick = () => applyPackText(ta.value, '粘贴');
  sec3.appendChild(ta); sec3.appendChild(bLoad);
  box.appendChild(sec3);

  const sec4 = el('div', 'ss-sec');
  sec4.appendChild(el('h3', null, '发布', '<span class="ss-tag">管理员</span>'));
  sec4.appendChild(row('账号', textInput(localStorage.getItem('skin-studio.user') || '', (v) => localStorage.setItem('skin-studio.user', v), '管理员账号')));
  sec4.appendChild(row('密码', '<input type="password" id="ss-pass" style="width:100%;background:var(--ss-panel2);color:var(--ss-text);border:1px solid var(--ss-line);border-radius:8px;padding:5px 7px">'));
  const btns2 = el('div', 'ss-btns');
  const bPub = el('button', 'primary', '⬆ 发布并拿授权码');
  bPub.onclick = () => doPublish(false);
  const bReplace = el('button', null, '覆盖同 id 的旧包');
  bReplace.onclick = () => doPublish(true);
  const bFromCode = el('button', null, '🔑 按授权码载入');
  bFromCode.onclick = doLoadFromCode;
  btns2.appendChild(bPub); btns2.appendChild(bReplace); btns2.appendChild(bFromCode);
  sec4.appendChild(btns2);
  const p4 = el('p', 'ss-note');
  p4.innerHTML = `服务端：<b>面板代理</b> ｜ 端点：POST /api/admin/skinpacks/upload、GET /api/skinpacks/download/&lt;码&gt;（与 navpack 同构）。`;
  sec4.appendChild(p4);
  box.appendChild(sec4);

  const log = el('div', 'ss-code');
  log.id = 'ss-log';
  log.textContent = '（发布日志会出现在这里）';
  box.appendChild(log);
}

function doExport() {
  const pack = buildPack();
  const errs = preflight(pack).filter((r) => r.level === 'bad');
  if (errs.length) { alert('先修掉体检里的 ✗：\n\n' + errs.map((e) => e.msg).join('\n')); return; }
  const blob = new Blob([JSON.stringify(pack, null, 2)], { type: 'application/json' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = `${pack.packId || 'skinpack'}-${pack.version}.skinpack.json`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 4000);
  markClean();
  toast('已导出 ' + a.download, 'ok');
}

/** 导入一份包文本（文件 / 粘贴 / 从服务端下载都用它）。 */
function applyPackText(text, from) {
  let o;
  try { o = JSON.parse(text); } catch (e) { alert('这份 JSON 解析不了：' + e.message); return; }
  if (!o || o.format !== 'threees.skinpack') { alert('这不是外观包（format 应该是 threees.skinpack）'); return; }
  if (o.formatVersion > 2) { alert(`这一包的格式版本是 ${o.formatVersion}，设计台只认 2`); return; }
  S.skin = C.normalizeSkin(o.skin);
  if (typeof o.seed === 'string') S.seed = o.seed;
  S.dark = !!o.dark;
  S.pack = { packId: o.packId || '', name: o.name || '', version: o.version || '1.0.0', note: o.note || '', author: o.author || '' };
  SEL = null;
  save(); renderPackPanel();
  if (S.mode === 'free') freeLoad(); else { renderPhone(); renderPanel(); }
  syncChrome();
  HIST = [{ s: snap(), label: '载入 ' + (o.packId || '外观包') }];
  HIDX = 0;
  markClean();
  const cb = $('#ss-dark'); if (cb) cb.checked = S.dark;
  logLine(`已从${from}载入「${o.name || o.packId}」v${o.version}`);
  toast('已载入「' + (o.name || o.packId) + '」', 'ok');
}
function logLine(s) {
  const log = $('#ss-log');
  if (log) log.textContent = s + '\n' + (log.textContent === '（发布日志会出现在这里）' ? '' : log.textContent);
}

async function doPublish(replace) {
  const pack = buildPack();
  const errs = preflight(pack).filter((r) => r.level === 'bad');
  if (errs.length) { logLine('✗ 体检没过：' + errs.map((e) => e.msg).join('；')); return; }
  const user = (localStorage.getItem('skin-studio.user') || '').trim();
  const pass = ($('#ss-pass') || {}).value || '';
  if (!user || !pass) { logLine('✗ 先填账号密码（发布要管理员）'); return; }
  try {
    logLine('正在登录…');
    const lr = await fetch(SERVER + '/plugin/skinstudio/api/auth/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ user, password: pass }),
    });
    const lj = await lr.json().catch(() => ({}));
    const token = lj.token || lj.Token;
    if (!token) { logLine('✗ 登录失败：' + (lj.error || lj.message || lr.status)); return; }
    localStorage.setItem(TOKEN_KEY, token);
    logLine('登录成功，正在上传…');
    const url = SERVER + '/plugin/skinstudio/api/admin/skinpacks/upload' + (replace ? '?replace=1' : '');
    const ur = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + token },
      body: JSON.stringify(pack),
    });
    const uj = await ur.json().catch(() => ({}));
    if (!ur.ok) { logLine(`✗ 上传失败(${ur.status})：${uj.error || uj.message || ''}`); return; }
    logLine(`✓ 已发布「${pack.name}」v${pack.version} —— 授权码就是「${pack.packId}」`);
    AUTHCODE = pack.packId;
    markClean();
    renderPackPanel();
    toast('已发布，授权码 ' + AUTHCODE, 'ok', 2600);
  } catch (e) {
    logLine('✗ 连不上服务器：' + e.message);
  }
}

/** 按授权码从服务端下载一个包，载入成当前设计。 */
async function doLoadFromCode(code) {
  if (!code) code = prompt('输入授权码（= 包标识）');
  if (!code) return;
  try {
    logLine('正在下载 ' + code + ' …');
    const r = await fetch(SERVER + '/plugin/skinstudio/api/skinpacks/download/' + encodeURIComponent(code.trim()), {
      headers: (function () { const t = localStorage.getItem(TOKEN_KEY); return t ? { Authorization: 'Bearer ' + t } : {}; })(),
    });
    if (!r.ok) { logLine(`✗ 下载失败(${r.status})：` + (r.status === 404 ? '没有这个授权码对应的包' : '登录过期或服务器问题')); return; }
    applyPackText(await r.text(), '服务端');
  } catch (e) {
    logLine('✗ 连不上服务器：' + e.message);
  }
}

// ============================================================ 模态

function openModal() { const m = $('#ss-modal'); if (m) m.hidden = false; renderPackPanel(); }
function closeModal() { const m = $('#ss-modal'); if (m) m.hidden = true; }

// ============================================================ 启动

function boot() {
  load();
  HIST = [{ s: snap(), label: '起始' }];
  HIDX = 0;

  $('#ss-undo').onclick = undo;
  $('#ss-redo').onclick = redo;
  $('#ss-publish').onclick = openModal;
  $('#ss-modal-close').onclick = closeModal;
  $('#ss-modal-mask').onclick = closeModal;
  // 横屏 / 竖屏切换（= LandscapeShell 那套外壳：左栏 + 内容 + 右栏）
  //
  // ★ v5：**两种模式下它的作用对象不一样** —— 这是原来"点了没反应"的根因。
  //   · 高级模式：切手机模拟器的横竖屏（老行为，没动）。
  //   · 自由画布：模拟器是隐藏的，所以它得**切画布本身的尺寸**
  //     （393×620 ↔ 852×393）。不然用户点了半天，屏幕上一点变化都没有 ——
  //     因为变的那个东西根本看不见。
  $('#ss-orient').onclick = () => {
    S.land = !S.land; S.landMenu = false;
    if (S.land) { S.drawer = false; const dcb = $('#ss-drawer'); if (dcb) dcb.checked = false; }
    save();
    if (S.mode === 'free' && FCED) {
      // `resize()` 内部会 `fire()` → 落回 skin.freeLayers + 记一步历史
      FCED.resize(S.land ? 852 : 393, S.land ? 393 : 620);
    } else {
      renderPhone(); pushRenderData();
    }
    setDevLabel();
  };
  // ★★ 源码渲染层（⌨）：把**手机端编译的同一份 Compose**（WasmJS）就地盖在手机框上。
  //
  //   ⚠️ 以前这个按钮是 `window.open('shell-render/')` —— 开新标签。两个问题：
  //      ① 视线要来回切，"改一下看一眼"根本没法用；
  //      ② `/shell-render/` 那时还被服务端的 SPA 回落吃掉了（打开的是**主站首页**：
  //         文件在、状态码 200、只有内容是错的）—— 见 web/main.go 的修复。
  //      现在改成**就地一层 iframe**，并且边改边跟（wasm 侧每 400ms 对一次 localStorage）。
  //
  //   ⚠️ 这一层是**只读预览**：它盖住了可编辑的 DOM 画布，所以拖拽/点选在这一层不做。
  //      想编辑就关掉 ⌨ —— 边界划清楚，比"两层都能点、都点半天"强。
  //   ⚠️ 右键 = 老行为（新标签打开），留给"想并排看"的场合。
  $('#ss-srcview').onclick = () => setSrcView(!S.srcOn);
  $('#ss-srcview').oncontextmenu = (e) => {
    e.preventDefault();
    try {
      localStorage.setItem('shellRenderData', JSON.stringify(shellRenderPayload()));
      window.open('shell-render/', '_blank');
    } catch (err) { toast('写渲染数据失败：' + err.message, 'bad'); }
  };
  // 抽屉只属于主页（下拉手势在主页）—— 勾上时切过去并把抽屉拉开（带动画）
  $('#ss-drawer').onchange = (e) => {
    S.drawer = e.target.checked;
    if (S.drawer && S.pageId !== 'home') { openPage('home'); S.drawer = true; }
    setPull(S.drawer ? PULL_DETENT : 0);
  };
  $('#ss-dark').onchange = (e) => { S.dark = e.target.checked; commit(S.dark ? '切深色' : '切浅色', () => {}); };
  $('#ss-reset').onclick = () => {
    if (!confirm('把当前设计清空（含布局），回到"什么都没覆盖"的状态？（可以 Ctrl+Z 撤销）')) return;
    SEL = null;
    commit('清空设计', (sk) => {
      const base = C.emptySkin();
      for (const k of Object.keys(base)) sk[k] = base[k];
      S.seed = '#2F6FB5'; S.dark = false;
    });
    const cb = $('#ss-dark'); if (cb) cb.checked = S.dark;
    if (S.mode === 'free') freeLoad();     // ★ v5：清空也要把画布倒空
  };

  // ★★ v5：模式切换 / 页面选择 / 代码模态 -------------------------------------------------
  //
  //   默认进**自由画布**（门槛低的那条路放前面）。「高级」= 上一版那套块流，
  //   一点没删 —— 两套数据（freeLayers / homeLayout+pageBlocks）并存，导出时按页决定。
  document.querySelectorAll('.ss-modebtn').forEach((b) => {
    b.onclick = () => { if (S.mode !== b.dataset.mode) setMode(b.dataset.mode); };
  });
  const psel = $('#fc-page');
  if (psel) {
    psel.onchange = () => {
      S.pageId = psel.value;
      save();
      if (S.mode === 'free') freeLoad();
      else { renderPhone(); renderOutline(); renderInspector(); }
      syncChrome();
    };
  }
  $('#fc-code-close').onclick = closeFreeCode;
  $('#fc-code-mask').onclick = closeFreeCode;
  $('#fc-code-apply').onclick = applyFreeCode;
  $('#fc-code-reset').onclick = resetFreeCode;

  // ---- 画布点选（事件委托：一次挂上，渲染换多少轮都不用重挂） ----
  $('.ph').addEventListener('click', (e) => {
    if (e.target.closest('.ph-scrim')) return;               // 关抽屉的遮罩不参与选
    const t = e.target.closest('[data-sel]');
    if (!t) return;
    const v = t.getAttribute('data-sel');
    if (v.indexOf('page:') === 0) openPage(v.slice(5));
    else if (v.indexOf('hblock:') === 0) selectHBlock(v.slice(7));
    else if (v.indexOf('pblock:') === 0) {
      const parts = v.split(':');
      selectPBlock(parts[1], parts[2], +parts[3]);
    } else pick(v);
  });

  // ---- 拖放（换位 / 跨页 / 体系转换）----
  wireDnd();
  // ---- 右键菜单（编辑 / 复制 / 删除 / 移到某页 —— 与拖拽、检查器同一批操作函数）----
  wireContextMenu();

  // ---- 键盘：撤销/重做/导出 + Esc 取消 + Delete 删块 ----
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      if (MENU) { closeMenu(); return; }
      const m = $('#ss-modal');
      if (m && !m.hidden) { closeModal(); return; }
      const cm = $('#fc-code');
      if (cm && !cm.hidden) { closeFreeCode(); return; }
      if (S.mode === 'free') return;         // 自由模式的 Esc 归画布（清掉选择）
      if (SEL) pick(null);
      return;
    }
    const mod0 = e.ctrlKey || e.metaKey;
    // Ctrl+S = 导出，两种模式都要能用 —— 所以在"让路"之前处理掉。
    if (mod0 && (e.key === 's' || e.key === 'S')) { e.preventDefault(); doExport(); return; }
    // ★ v5：自由模式下，元素级的按键（Ctrl+Z / Delete / 方向键…）**全部归画布引擎**
    //   （它在自己的 onKey 里处理）。这里让路，否则 Ctrl+Z 会两边同时撤、一次退两步。
    if (S.mode === 'free') return;
    if ((e.key === 'Delete' || e.key === 'Backspace') && SEL) {
      const a = document.activeElement;
      if (a && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA' || a.tagName === 'SELECT' || a.isContentEditable)) return;
      if (SEL.type === 'hblock') { e.preventDefault(); homeDelBlock(SEL.id); toast('已删除块', 'ok', 900); return; }
      if (SEL.type === 'pblock') { e.preventDefault(); pageDelBlock(SEL.pageId, SEL.at, SEL.index); toast('已删除块', 'ok', 900); return; }
    }
    const mod = e.ctrlKey || e.metaKey;
    if (!mod) return;
    if (e.key === 'z' || e.key === 'Z') { e.preventDefault(); if (e.shiftKey) redo(); else undo(); }
    else if (e.key === 'y') { e.preventDefault(); redo(); }
  });

  // ★ v5：按记住的模式起手（首次打开 = 自由画布）。`quiet` = 启动时别弹模式提示。
  setMode(S.mode, true);
  renderPackPanel();
  $('#ss-dark').checked = S.dark;
  syncChrome();

  // ★ `?code=<授权码>` 深链 —— 管理台「外观包」页的「在设计台里改」就是这么送包过来的。
  //   ⚠️ 失败不拦启动：包没载进来时设计台仍然是个能用的空白设计台，错误写进发布模态的日志。
  let deepCode = '';
  try {
    deepCode = new URLSearchParams(location.search).get('code') || '';
  } catch {
    /* 老浏览器没有 URLSearchParams：当作没有深链 */
  }
  if (deepCode.trim()) void doLoadFromCode(deepCode.trim());
  toast(S.mode === 'free'
    ? '自由画布：左边点元素加进来 → 拖到想要的位置；双击文字直接改；选中后右栏给它绑动作'
    : '高级模式：画布里点块编辑、拖拽换位；拖到左栏某页 = 跨页移动', 'ok', 4600);
}
document.addEventListener('DOMContentLoaded', boot);

// 测试钩子（jsdom 冒烟用；对页面无影响）。
if (typeof window !== 'undefined') {
  window.SkinStudio = {
    get S() { return S; }, get SEL() { return SEL; }, get HIST() { return HIST; },
    pick, openPage, selectHBlock, selectPBlock,
    commit, live, buildPack, preflight, applyPackText,
    homeEnsure, homeAddBlock, homeMoveBlock, homeDupBlock, homeDelBlock, homeSetBlockField,
    pageAddBlock, pageMoveBlock, pageMoveToPage, pageDelBlock, pageDupBlock,
    homeBlockToPageBlock, pageBlockToHomeBlock, pblockOf, pageBlocksOf,
    renderOutline, renderInspector, renderPhone, boot,
    // ★ v5 自由画布
    setMode, freeLayer, freeLoad, freeOnChange, freeColors, freePathText, renderPageSel,
    openFreeCode, closeFreeCode, applyFreeCode, resetFreeCode, freePreview,
    ensureEditor, pushHist,          // pushHist：测试要"把历史栈对齐到当前状态"
    freeClearPage, freeRestoreDefault, pageLabel,
    contentTopFor, SHELL_TOP, SHELL_BOTTOM,   // 测试要核对内容从哪开始
    get FCED() { return FCED; }, get FC() { return FC; },
  };
}
