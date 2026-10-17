/*
 * 自由画布（free-canvas）—— 像做 PPT 一样设计手机界面。
 *
 * ## ★★ 为什么整个文件包在 IIFE 里（2026-10-15 真机踩到）
 *
 * 因为它和 `skin-studio.js` 是**两个经典的 `<script>`**，共享同一个全局词法环境。
 * 两边都声明了顶层 `function esc(...)` → 浏览器直接抛
 * `SyntaxError: Identifier 'esc' has already been declared`，
 * **`skin-studio.js` 整个文件拒绝执行** → 设计台完全打不开（元素库、画布、属性面板全是空的）。
 *
 * ⚠️ 这个错**单元测试抓不到**：`window.eval(code)` 是**间接 eval**，里面的
 * `function` 声明会变成全局对象的属性（可覆盖、不冲突），而 `<script>` 里的顶层
 * 声明是**全局词法声明**（重复即报错）。所以"eval 注入测试全绿、真机全废"。
 * 包进 IIFE 之后，本文件除了 `window.FreeCanvas` 之外**不往全局放任何东西**。
 */
(function () {
'use strict';

/*
 * ## 它解决什么
 *
 * 上一版的设计台是「块流」：主页 = 一块块往下排（卡片/标题/文字/留白），
 * 页面 = 顶部/底部各一串块。它能表达的东西被那 6 种块的形状钉死了，
 * 而且必须先理解"块 / zone / span / heightFactor"这些概念才敢动手 ——
 * 门槛高、自由度低。
 *
 * 这一版换成**绝对定位的画布**：把文字、形状、图片、图标、按钮、手绘、
 * 甚至一整段 HTML 拖到 393×852 的手机画面上，随便摆、随便缩放旋转、
 * 想点哪里就给它绑一个动作。门槛低到"拖过去、拖到想要的位置"。
 *
 * ## 产物是什么（这一点决定了它为什么能真的上手机）
 *
 * 画布**不是**新造一套渲染引擎 —— 它编译成**一段自包含的 HTML**，
 * 交给手机端早就有的 `HomeBlock(type=html)` / `PageBlock(kind=html)`，
 * 由 WebView 画出来（`AiHomePage.kt` 的 `HomeHtmlBlock`）。
 *
 * ```
 *   画布元素数组  ──compile()──▶  自包含 HTML  ──▶  手机端 WebView
 * ```
 *
 * 于是：
 * · **手机端不需要认识"元素"这个概念** —— 它只认识"一段 HTML"，
 *   两端不存在"画布模型要同步"的维护负担；
 * · 自由度是**无限**的：CSS 能画什么，画布就能画什么；
 * · 用户随时可以切到「代码」模式，直接改那段 HTML —— 两条路通向同一个东西。
 *
 * ## 三层限制（继承自手机端 WebView，必须知道）
 *
 * ① **禁网** —— `blockNetworkLoads = true`。所以图片**必须内联**
 *    （本文件把上传的图片转成 `data:` URI），字体要自包含。
 * ② **禁文件访问** —— `allowFileAccess = false`。
 * ③ **桥只有只读数据 + 白名单动作** —— `window.threees.data(name, args)`
 *    取数据，`window.threees.act(kind, arg)` 执行动作。见 `ACTION_KINDS`。
 *
 * ## 文件结构（改代码时按这十几节找）
 *
 * ```
 *   §1  常量与工具
 *   §2  元素模型（造一个 / 归一 / 复制）
 *   §3  编译：元素数组 → 自包含 HTML
 *   §4  画布渲染：元素数组 → DOM（编辑态，带手柄）
 *   §5  选择与命中
 *   §6  参考线与吸附
 *   §7  拖 / 缩 / 旋 / 框选
 *   §8  键盘与右键菜单
 *   §9  手绘笔刷
 *   §10 工具栏
 *   §11 元素库
 *   §12 属性面板
 *   §13 图层列表
 *   §14 历史
 *   §15 导出接口（mount / compile）
 * ```
 *
 * ⚠️ 零依赖、不构建（同 nav-studio / skin-studio 的纪律）——
 *    这一页要能直接双击打开、也能挂到服务器上，不能有任何打包步骤。
 */

// ============================================================ §1 常量与工具

/**
 * 画布默认尺寸。
 *
 * ★ v5.2：高度从 620 改成 **852（= 手机整屏）**。
 *
 * 因为画布底下铺的是**手机端的源码渲染**（`shellFrame`），而那个 iframe 会
 * **自动铺满容器** —— 画布给多少它就渲染多少。画布按**整屏**算，两边就**精确重合**，
 * 不需要猜任何偏移。产物那边靠 `SHELL_TOP` / `SHELL_BOTTOM` 减掉外壳两段
 * （见 `compile`），所以"画布是整屏、产物是内容区"这件事是对得上的。
 */
const DEFAULT_W = 393;
const DEFAULT_H = 852;

/** 吸附阈值（px）：离得比这个近就吸上去。 */
const SNAP = 6;

/**
 * 手机端外壳的上下两段高度（px = dp）—— 用来把**画布（整屏）**和**产物（内容区）**对齐。
 *
 * 竖屏结构（= wasm 侧 `main.kt` 那段 Column，逐段）：
 * ```
 *   ┌─ 状态栏色带  statusInset      34   ← 系统给的高度（画布照数字画）
 *   ├─ 顶栏        ShellTopBar    ≈ 59   ← padding(vertical 10×2) + 两行文字
 *   ├─ 内容区      ← 元素放这儿
 *   ├─ 底栏        ShellBottomNav   80   ← Material3 NavigationBar 默认高
 *   └─ 手势条色带  navInset         24
 * ```
 * 所以 顶部偏移 93 / 底部偏移 104。**默认值写在这里，实际以宿主传的为准**
 * （`skin-studio.js` 里有同名常量，那边是权威 —— 它才跟 `shellRenderData` 对得上）。
 *
 * ⚠️ 顶栏那个 59 是**算出来的**，不是量的。误差几 px 只影响"产物顶部的留白"，
 *    **不影响元素之间的相对位置** —— 画布上那条虚线参考线就是把这个边界给用户看。
 */
const SHELL_TOP = 93;
const SHELL_BOTTOM = 104;

/** 元素类型。`k` 字段就是这几个值。 */
const KINDS = ['text', 'box', 'ellipse', 'line', 'image', 'icon', 'button', 'draw', 'code'];

const KIND_LABEL = {
  text: '文字', box: '矩形', ellipse: '椭圆', line: '直线', image: '图片',
  icon: '图标', button: '按钮', draw: '手绘', code: '代码',
};

/** 动作类型（= 手机端 `HtmlBridge.act` 认的那几个）。 */
const ACTION_KINDS = [
  { v: '', label: '不绑定' },
  { v: 'page', label: '跳到页面', arg: 'schedule', hint: '页面 id：schedule / calendars / inbox / ai / settings / notes / mdnote / plot / home' },
  { v: 'app', label: '执行功能', arg: 'open_settings', hint: 'open_settings / edit_home / new_event / sync_now / check_update / manage_plugins' },
  { v: 'url', label: '打开网址', arg: 'https://', hint: '要 http(s) 开头' },
  { v: 'js', label: '自定义脚本', arg: '', hint: '点下去时执行的 JS（写在元素上，只有懂代码才用得上）' },
];

/** 内置图标（内联 SVG，24×24 视口，`stroke:currentColor` 所以跟着文字色走）。 */
const ICONS = {
  star: '<path d="M12 2.6l2.9 5.9 6.5.95-4.7 4.6 1.1 6.45L12 17.45 6.2 20.5l1.1-6.45-4.7-4.6 6.5-.95z"/>',
  heart: '<path d="M12 20.3l-1.45-1.32C5.4 14.24 2 11.16 2 7.4 2 4.42 4.42 2 7.4 2c1.74 0 3.41.81 4.6 2.09C13.19 2.81 14.86 2 16.6 2 19.58 2 22 4.42 22 7.4c0 3.76-3.4 6.84-8.55 11.59z"/>',
  home: '<path d="M12 3l9 8h-3v9h-5v-6H11v6H6v-9H3z"/>',
  gear: '<path d="M12 8a4 4 0 100 8 4 4 0 000-8zm9 4l-2.1-.6.9-2-2-2-2 .9L15.2 6 14.6 4h-2.8L11.2 6l-2.1.9-2-.9-2 2 .9 2L4 12l2.1.6-.9 2 2 2 2-.9 2.1.9.6 2h2.8l.6-2 2.1-.9 2 .9 2-2-.9-2z"/>',
  bell: '<path d="M12 22a2.5 2.5 0 002.45-2h-4.9A2.5 2.5 0 0012 22zm7-6V11a7 7 0 10-14 0v5l-2 2v1h18v-1z"/>',
  calendar: '<path d="M7 2v2H5a2 2 0 00-2 2v14a2 2 0 002 2h14a2 2 0 002-2V6a2 2 0 00-2-2h-2V2h-2v2H9V2zm12 8v10H5V10z"/>',
  clock: '<path d="M12 2a10 10 0 100 20 10 10 0 000-20zm1 5v6l5 3-1 1.6-6-3.6V7z"/>',
  book: '<path d="M4 3h11a3 3 0 013 3v15H7a3 3 0 01-3-3zm2 13a1 1 0 011-1h9V6a1 1 0 00-1-1H7a1 1 0 00-1 1z"/>',
  pencil: '<path d="M3 17.25V21h3.75L17.8 9.94l-3.75-3.75zm17.7-10.2a1 1 0 000-1.41l-2.34-2.34a1 1 0 00-1.41 0l-1.83 1.83 3.75 3.75z"/>',
  check: '<path d="M9 16.2l-3.5-3.5-1.4 1.4L9 19 20.3 7.7l-1.4-1.4z"/>',
  close: '<path d="M19 6.4L17.6 5 12 10.6 6.4 5 5 6.4 10.6 12 5 17.6 6.4 19 12 13.4 17.6 19 19 17.6 13.4 12z"/>',
  plus: '<path d="M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6z"/>',
  arrow: '<path d="M4 11h12.2l-5.6-5.6L12 4l8 8-8 8-1.4-1.4 5.6-5.6H4z"/>',
  search: '<path d="M15.5 14h-.8l-.3-.3a6.5 6.5 0 10-.7.7l.3.3v.8l5 5 1.5-1.5zm-6 0a4.5 4.5 0 110-9 4.5 4.5 0 010 9z"/>',
  camera: '<path d="M9 3l-1.8 2H4a2 2 0 00-2 2v12a2 2 0 002 2h16a2 2 0 002-2V7a2 2 0 00-2-2h-3.2L15 3zm3 5.5a5.5 5.5 0 110 11 5.5 5.5 0 010-11z"/>',
  cloud: '<path d="M19.4 10a7.5 7.5 0 00-14.2-2A5.5 5.5 0 006 18h13a4 4 0 00.4-8z"/>',
  flag: '<path d="M6 2v20h2v-8h11l-3-4 3-4H8V2z"/>',
  chat: '<path d="M4 3h16a2 2 0 012 2v10a2 2 0 01-2 2H8l-5 4V5a2 2 0 012-2z"/>',
  image: '<path d="M4 4h16a1 1 0 011 1v14a1 1 0 01-1 1H4a1 1 0 01-1-1V5a1 1 0 011-1zm2 12h12l-4-5-3 3.5L9 13z"/>',
  lock: '<path d="M12 2a5 5 0 00-5 5v3H5v12h14V10h-2V7a5 5 0 00-5-5zm0 2a3 3 0 013 3v3H9V7a3 3 0 013-3z"/>',
  play: '<path d="M8 5v14l11-7z"/>',
};

/** 常用色（属性面板里给一排快选，省得每次开取色器）。 */
const SWATCHES = ['#ffffff', '#000000', '#2F6FB5', '#E5484D', '#F5A524', '#30A46C', '#8E4EC6', '#6E7681'];

let SEQ = 1;
function uid(prefix) { return (prefix || 'e') + '-' + (SEQ++) + '-' + Date.now().toString(36).slice(-4); }
function clamp(v, a, b) { return v < a ? a : v > b ? b : v; }
function num(v, d) { const n = Number(v); return Number.isFinite(n) ? n : d; }
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
/** 属性值转成内联 CSS 里安全的一小段（挡掉 `;` `}` 这类能改写声明的字符）。 */
function cssv(s) { return String(s == null ? '' : s).replace(/[;{}<>"']/g, ''); }

function isHex(s) { return typeof s === 'string' && /^#[0-9a-fA-F]{3,8}$/.test(s.trim()); }
function hexA(hex, a) {
  if (!isHex(hex) || a >= 1) return hex || '';
  let h = hex.trim().slice(1);
  if (h.length === 3) h = h.split('').map((c) => c + c).join('');
  const r = parseInt(h.slice(0, 2), 16), g = parseInt(h.slice(2, 4), 16), b = parseInt(h.slice(4, 6), 16);
  return 'rgba(' + r + ',' + g + ',' + b + ',' + a + ')';
}

// ============================================================ §2 元素模型

/** 造一个元素（只带该类型用得上的默认值，避免每个元素都存一坨空字段）。 */
function createEl(kind, patch) {
  const e = {
    id: uid(kind[0]), k: kind, name: '',
    x: 24, y: 24, w: 160, h: 44, rot: 0, op: 1, lock: false, hide: false,
    fill: '', stroke: '', strokeW: 0, radius: 0, shadow: 0,
    text: '', font: '', size: 16, weight: 400, color: '', align: 'left',
    lh: 1.4, ls: 0, italic: false, underline: false,
    src: '', fit: 'cover', icon: 'star', path: '', html: '',
    act: '', arg: '',
  };
  const d = DEFAULTS[kind] || {};
  Object.assign(e, d);
  if (patch) Object.assign(e, patch);
  return e;
}

/** 每种类型的起手样子（拖进画布时就是这个模样，用户看到东西再改，比"先填表单"好）。 */
const DEFAULTS = {
  text: { w: 180, h: 28, text: '双击改文字', size: 17, color: '', lh: 1.4 },
  box: { w: 160, h: 90, fill: '#2F6FB5', radius: 14, shadow: 2 },
  ellipse: { w: 90, h: 90, fill: '#5B9DFF', shadow: 1 },
  line: { w: 140, h: 2, fill: '#6E7681', strokeW: 2 },
  image: { w: 160, h: 120, radius: 12, fit: 'cover' },
  icon: { w: 40, h: 40, icon: 'star', fill: '#2F6FB5' },
  button: { w: 150, h: 44, text: '点我', size: 15, weight: 600, align: 'center', color: '#ffffff', fill: '#2F6FB5', radius: 22, shadow: 2 },
  draw: { w: 160, h: 120, stroke: '#2F6FB5', strokeW: 3 },
  code: { w: 280, h: 120, html: '<div style="font:13px/1.5 system-ui;padding:8px;border:1px dashed #8b93a3;border-radius:10px">这里可以写任意 HTML / CSS / SVG</div>' },
};

/** 把外来数据（localStorage / 导入的包）归一成合法元素。坏数据不抛异常，只回退默认值。 */
function normalizeEl(raw) {
  if (!raw || typeof raw !== 'object') return null;
  const k = KINDS.includes(raw.k) ? raw.k : 'text';
  const e = createEl(k);
  for (const key of Object.keys(e)) {
    // ⚠️ `k` 必须跳过：它上面已经归一过了（认不出的类型 → `text`）。
    //    不跳的话下面那句会把 `'不存在'` 原样抄回去 —— 元素带着一个不存在的类型
    //    往下走，`elHtml` 落进 default 分支返回空串，**元素在画布上凭空消失**
    //    （而且不报任何错）。这是真踩到过的。
    if (key === 'k') continue;
    if (raw[key] === undefined || raw[key] === null) continue;
    if (typeof e[key] === 'boolean') e[key] = !!raw[key];
    else if (typeof e[key] === 'number') e[key] = num(raw[key], e[key]);
    else e[key] = String(raw[key]);
  }
  if (raw.id) e.id = String(raw.id);
  e.w = Math.max(2, e.w); e.h = Math.max(2, e.h);
  e.op = clamp(e.op, 0, 1);
  e.size = clamp(e.size, 6, 200);
  e.radius = clamp(e.radius, 0, 200);
  e.strokeW = clamp(e.strokeW, 0, 40);
  e.shadow = clamp(e.shadow, 0, 5);
  e.lh = clamp(e.lh, 0.6, 3);
  if (!['left', 'center', 'right'].includes(e.align)) e.align = 'left';
  if (!['cover', 'contain', 'fill'].includes(e.fit)) e.fit = 'cover';
  if (!ACTION_KINDS.some((a) => a.v === e.act)) e.act = '';
  return e;
}

/** 归一整个图层。 */
function normalizeLayer(raw, w, h) {
  const L = { w: num(w, DEFAULT_W), h: num(h, DEFAULT_H), bg: '', code: '', els: [] };
  if (raw && typeof raw === 'object') {
    if (raw.w) L.w = clamp(num(raw.w, L.w), 120, 1200);
    if (raw.h) L.h = clamp(num(raw.h, L.h), 80, 3000);
    if (raw.bg) L.bg = String(raw.bg);
    if (raw.code) L.code = String(raw.code);
    if (Array.isArray(raw.els)) L.els = raw.els.map(normalizeEl).filter(Boolean);
  }
  return L;
}

function cloneEl(e) { const c = JSON.parse(JSON.stringify(e)); c.id = uid(e.k[0]); return c; }

/** 元素在界面上显示的名字（图层列表用）。⚠️ 允许传 null（选中项刚被删掉时）。 */
function elLabel(e) {
  if (!e) return '';
  if (e.name) return e.name;
  if (e.k === 'text' || e.k === 'button') return (e.text || '').slice(0, 12) || KIND_LABEL[e.k];
  if (e.k === 'icon') return '图标·' + e.icon;
  if (e.k === 'image') return e.src ? '图片' : '图片（空）';
  if (e.k === 'code') return '代码块';
  return KIND_LABEL[e.k] || e.k;
}

// ============================================================ §3 编译：元素数组 → 自包含 HTML

/**
 * 一个元素的 HTML。
 *
 * ⚠️ 所有几何值都是**内联 style**（不用 class）—— 因为产物要能塞进
 *    手机端任意一段 HTML 里，不能依赖外部样式表（宿主那边没有我们的 CSS）。
 *
 * @param dy 外壳偏移（= 手机顶栏高度）。画布画的是**整屏**（含顶栏底栏，
 *           这样能和源码渲染背景精确重合），而产物是塞进**内容区**的 ——
 *           所以纵坐标要减掉顶栏那一段。0 = 不减（比如导出成整页 HTML 时）。
 */
function elHtml(e, z, dy) {
  const off = num(dy, 0);
  const pos = 'position:absolute;left:' + r2(e.x) + 'px;top:' + r2(e.y - off) + 'px;' +
    'width:' + r2(e.w) + 'px;height:' + r2(e.h) + 'px;' +
    (e.rot ? 'transform:rotate(' + r2(e.rot) + 'deg);' : '') +
    (e.op < 1 ? 'opacity:' + r2(e.op) + ';' : '') +
    'z-index:' + z + ';';
  const act = e.act ? ' data-act="' + esc(e.act) + '" data-arg="' + esc(e.arg) + '"' +
    (e.act === 'js' ? ' onclick="' + esc(e.arg) + '"' : '') +
    ' style="cursor:pointer"' : '';
  const shadow = e.shadow > 0 ? 'box-shadow:0 ' + e.shadow + 'px ' + (e.shadow * 2) + 'px rgba(0,0,0,' + (0.08 * e.shadow) + ');' : '';

  switch (e.k) {
    case 'text': {
      const st = 'font-size:' + r2(e.size) + 'px;font-weight:' + e.weight + ';line-height:' + r2(e.lh) + ';' +
        'letter-spacing:' + r2(e.ls) + 'px;text-align:' + e.align + ';color:' + (e.color || 'var(--fc-fg,#1b1b1f)') + ';' +
        (e.font ? 'font-family:' + cssv(e.font) + ';' : '') +
        (e.italic ? 'font-style:italic;' : '') + (e.underline ? 'text-decoration:underline;' : '') +
        'display:flex;align-items:' + (e.h <= e.size * 2 ? 'center' : 'flex-start') + ';' +
        'justify-content:' + (e.align === 'center' ? 'center' : e.align === 'right' ? 'flex-end' : 'flex-start') + ';' +
        'white-space:pre-wrap;word-break:break-word;overflow:hidden;';
      return '<div' + act + ' style="' + pos + st + '">' + esc(e.text) + '</div>';
    }
    case 'box':
      return '<div' + act + ' style="' + pos + 'background:' + (e.fill || 'var(--fc-primary,#2F6FB5)') + ';' +
        'border-radius:' + r2(e.radius) + 'px;' + shadow +
        (e.stroke ? 'border:' + r2(e.strokeW || 1) + 'px solid ' + cssv(e.stroke) + ';' : '') + '"></div>';

    case 'ellipse':
      return '<div' + act + ' style="' + pos + 'background:' + (e.fill || 'var(--fc-primary,#2F6FB5)') + ';' +
        'border-radius:50%;' + shadow +
        (e.stroke ? 'border:' + r2(e.strokeW || 1) + 'px solid ' + cssv(e.stroke) + ';' : '') + '"></div>';

    case 'line': {
      const col = e.fill || 'var(--fc-fg,#6E7681)';
      return '<div' + act + ' style="' + pos + '">' +
        '<svg width="100%" height="100%" viewBox="0 0 ' + r2(e.w) + ' ' + Math.max(2, r2(e.h)) + '" preserveAspectRatio="none">' +
        '<line x1="0" y1="' + (e.h / 2) + '" x2="' + r2(e.w) + '" y2="' + (e.h / 2) + '" stroke="' + cssv(col) + '" stroke-width="' + r2(e.strokeW || 2) + '" stroke-linecap="round"/>' +
        '</svg></div>';
    }
    case 'image':
      return '<div' + act + ' style="' + pos + 'border-radius:' + r2(e.radius) + 'px;overflow:hidden;' + shadow + '">' +
        (e.src
          ? '<img src="' + esc(e.src) + '" style="width:100%;height:100%;object-fit:' + e.fit + ';display:block">'
          : '<div style="width:100%;height:100%;background:rgba(127,127,127,.18);display:flex;align-items:center;justify-content:center;font-size:12px;color:#8b93a3">图片</div>') +
        '</div>';

    case 'icon':
      return '<div' + act + ' style="' + pos + 'color:' + (e.fill || 'var(--fc-primary,#2F6FB5)') + '">' +
        '<svg width="100%" height="100%" viewBox="0 0 24 24" fill="currentColor">' + (ICONS[e.icon] || ICONS.star) + '</svg></div>';

    case 'button': {
      const st = 'display:flex;align-items:center;justify-content:' +
        (e.align === 'center' ? 'center' : e.align === 'right' ? 'flex-end' : 'flex-start') + ';' +
        'background:' + (e.fill || 'var(--fc-primary,#2F6FB5)') + ';border-radius:' + r2(e.radius) + 'px;' +
        'color:' + (e.color || 'var(--fc-on-primary,#fff)') + ';font-size:' + r2(e.size) + 'px;font-weight:' + e.weight + ';' +
        'letter-spacing:' + r2(e.ls) + 'px;' + shadow +
        (e.stroke ? 'border:' + r2(e.strokeW || 1) + 'px solid ' + cssv(e.stroke) + ';' : '') +
        'white-space:pre-wrap;overflow:hidden;';
      return '<div' + act + ' style="' + pos + st + '">' + esc(e.text) + '</div>';
    }
    case 'draw': {
      const col = e.stroke || 'var(--fc-primary,#2F6FB5)';
      return '<div' + act + ' style="' + pos + '">' +
        '<svg width="100%" height="100%" viewBox="0 0 ' + r2(e.w) + ' ' + r2(e.h) + '">' +
        '<path d="' + esc(e.path) + '" fill="none" stroke="' + cssv(col) + '" stroke-width="' + r2(e.strokeW || 3) + '" ' +
        'stroke-linecap="round" stroke-linejoin="round"/></svg></div>';
    }
    case 'code':
      return '<div' + act + ' style="' + pos + 'overflow:hidden">' + (e.html || '') + '</div>';
    default:
      return '';
  }
}

function r2(v) { return Math.round(num(v, 0) * 100) / 100; }

/**
 * 点一下元素 → 交给手机端执行。
 *
 * ⚠️ 这段脚本是**内联进产物**的，因为产物会被塞进宿主任意一段 HTML 里，
 *    没有"我们自己的外部 JS"可用。
 *
 * ⚠️ 用**捕获阶段**（第三个参数 true）：用户拖进来的 HTML 里可能有
 *    `stopPropagation`，冒泡阶段会被吃掉，捕获阶段先拿到。
 *
 * ⚠️ `js` 类型不在这里处理（它写在元素的 `onclick` 上）—— 这里只处理
 *    需要过桥的那三种。
 */
const ACTION_SCRIPT = '<script>(function(){' +
  'if(window.__fcBound)return;window.__fcBound=1;' +
  'function find(n){while(n&&n.getAttribute){if(n.getAttribute("data-act"))return n;n=n.parentNode}return null}' +
  'document.addEventListener("click",function(ev){' +
  'var t=find(ev.target);if(!t)return;' +
  'var k=t.getAttribute("data-act"),a=t.getAttribute("data-arg")||"";' +
  'if(k==="js")return;' +
  'try{if(window.threees&&window.threees.act)window.threees.act(k,a)}catch(e){}' +
  'ev.preventDefault();' +
  '},true);})();<\/script>';

/**
 * 图层 → 自包含 HTML 片段。
 *
 * @param layer 归一后的图层
 * @param opt   { colors?: 皮肤解析出的角色色（拿来当 CSS 变量兜底） }
 */
function compile(layer, opt) {
  const o = opt || {};
  const c = o.colors || {};
  const L = normalizeLayer(layer, layer && layer.w, layer && layer.h);

  // 用户在「代码」模式里手改过 → 直接吐那段（画布不再参与，避免两套真相打架）
  if (L.code) return L.code;

  // ★ 外壳偏移：画布画的是**整屏**（含顶栏底栏，这样能和源码渲染背景精确重合），
  //   而产物要塞进手机端的**内容区** —— 所以容器高度要减掉上下两段，
  //   元素纵坐标也要减掉顶栏那一段。见 `elHtml` 的 `dy`。
  const top = Math.max(0, num(o.shellTop, SHELL_TOP));
  const bot = Math.max(0, num(o.shellBottom, SHELL_BOTTOM));
  const contentH = Math.max(40, L.h - top - bot);

  const vars = [
    c.onBackground ? '--fc-fg:' + cssv(c.onBackground) : '',
    c.background ? '--fc-bg:' + cssv(c.background) : '',
    c.primary ? '--fc-primary:' + cssv(c.primary) : '',
    c.onPrimary ? '--fc-on-primary:' + cssv(c.onPrimary) : '',
    c.surface ? '--fc-surface:' + cssv(c.surface) : '',
    c.onSurface ? '--fc-on-surface:' + cssv(c.onSurface) : '',
    c.secondaryContainer ? '--fc-container:' + cssv(c.secondaryContainer) : '',
    c.onSecondaryContainer ? '--fc-on-container:' + cssv(c.onSecondaryContainer) : '',
  ].filter(Boolean).join(';');

  const out = [];
  out.push('<div class="fc-root" style="position:relative;width:100%;height:' + r2(contentH) + 'px;' +
    'overflow:hidden;font-family:-apple-system,system-ui,"PingFang SC","Noto Sans CJK SC",sans-serif;' +
    '-webkit-text-size-adjust:100%;' + (vars ? vars + ';' : '') + '">');
  if (L.bg) out.push('<div style="position:absolute;inset:0;background:' + cssv(L.bg) + '"></div>');
  L.els.forEach((e, i) => { if (!e.hide) out.push(elHtml(e, i + 1, top)); });
  if (L.els.some((e) => e.act && e.act !== 'js')) out.push(ACTION_SCRIPT);
  out.push('</div>');
  return out.join('');
}

// ============================================================ §4 编辑器（挂载）

/**
 * 把一块画布挂到 [host] 上。
 *
 * @param host 容器元素
 * @param opts {
 *   onChange(layer)  每次改动后回调（skin-studio 拿它去置脏/存盘）
 *   onSelect(sel)    选中变化（右栏属性面板重画）
 *   colors           皮肤解析出的角色色（预览用 CSS 变量）
 *   width, height    画布尺寸
 * }
 */
function mount(host, opts) {
  const o = opts || {};
  const ed = {
    host: host, opts: o,
    layer: normalizeLayer(null, o.width, o.height),
    sel: [],            // 选中的元素 id
    hist: [], hidx: -1,
    scale: 1, mode: 'select', brush: null,
    colors: o.colors || {},
    dark: !!o.dark,
    spaceDown: false,
    _drag: null, _marquee: null, _live: { label: '', at: 0 },
  };

  // ---------- DOM ----------
  installMockBridge();          // ★ 让 HTML 块在设计台里就能画出效果（示例数据）
  host.classList.add('fc-host');
  host.innerHTML = '';
  const tools = document.createElement('div'); tools.className = 'fc-tools';
  const stage = document.createElement('div'); stage.className = 'fc-stage';
  // ⚠️ `.fc-fit` 是**给缩放用的**：`.fc-page` 用 `transform:scale()` 缩放，
  //    而 transform **不影响布局** —— 不套一层"尺寸 = 缩放后大小"的壳，
  //    缩小之后画布周围会留一大片空白（看起来像没居中），放大则会溢出裁掉。
  const fit = document.createElement('div'); fit.className = 'fc-fit';
  const page = document.createElement('div'); page.className = 'fc-page';
  const overlay = document.createElement('div'); overlay.className = 'fc-overlay';
  const guides = document.createElement('div'); guides.className = 'fc-guides';
  const marquee = document.createElement('div'); marquee.className = 'fc-marquee';
  marquee.hidden = true;

  // ★★ 源码渲染背景层 —— 把**手机端的渲染代码**（`android/shared-ui` → WasmJS，
  //    和真机编译的同一份 Compose 源码）铺在画布底下。
  //
  //    用户原话：「我让你搬的默认代码是指把手机端的渲染代码直接搬上来……
  //    默认打开，就是手机端默认的样子，所有页面都是这样」。
  //
  //    ⚠️ iframe 会**自动铺满容器**（`shell-render/index.html` 里按 window.innerWidth/
  //       innerHeight 定 canvas 尺寸），所以画布多高它就渲染多高 ——
  //       **不需要猜任何偏移就能精确重合**。这就是画布按"整屏"（含顶栏底栏）设计的原因。
  //    ⚠️ `pointer-events: none`（CSS 里）：它是背景，不能吃掉画布上的鼠标事件。
  let shellFrame = null;
  let shellLoadEl = null;      // ⚠️ 必须在 if 外面声明 —— `ed.el` 在后面才赋值，
                               //    不能在里面就写 `ed.el.shellLoad`（那会让 mount 整个崩掉）
  if (o.shellFrame) {
    const sw = document.createElement('div');
    sw.className = 'fc-shellwrap';
    shellFrame = document.createElement('iframe');
    shellFrame.className = 'fc-shellframe';
    shellFrame.setAttribute('title', '手机端源码渲染（只读背景）');
    shellFrame.setAttribute('scrolling', 'no');
    sw.appendChild(shellFrame);
    // ★ 加载提示 —— 用户原话："它没有任何进度提示，让我以为它没加载"。
    //   wasm 有 8MB，本地也要一两秒、第一次走网络更久；这段时间画布是**一片底色**，
    //   看起来和"设计台坏了"一模一样。所以盖一层会动的提示，加载完自己消失。
    const ld = document.createElement('div');
    ld.className = 'fc-shellload';
    ld.innerHTML = '<div class="fc-spin"></div>' +
      '<div class="fc-shellload-t">正在加载手机端渲染…</div>' +
      '<div class="fc-shellload-s">画布底下跑的是手机端同一份代码（WasmJS），第一次要几秒</div>';
    sw.appendChild(ld);
    shellLoadEl = ld;
    page.appendChild(sw);
  }

  // 外壳参考线：把**内容区**框出来（元素放在顶栏/底栏上会被原生 UI 盖住）
  const shellMark = document.createElement('div');
  shellMark.className = 'fc-shellmark';
  page.appendChild(shellMark);

  // ★★ 块热区层 —— 把**手机端画出来的块**变成可点选的控件。
  //
  //   用户原话：「它加载的那些东西根本就不能进行编辑，**它应该是生成对应的控件**」。
  //   手机端画的是 canvas（一个像素图），点不了。所以：wasm 侧把每个块的
  //   rect 报出来（`HomeBlocksColumn(onBlockRect=…)` → `window.__SHELL_BLOCKS__`），
  //   这里在同样位置铺一层**透明的热区** —— 点它就选中那个块。
  const hotLayer = document.createElement('div');
  hotLayer.className = 'fc-hotlayer';
  page.appendChild(hotLayer);

  page.appendChild(guides);
  page.appendChild(overlay);
  page.appendChild(marquee);
  fit.appendChild(page);
  stage.appendChild(fit);
  host.appendChild(tools);
  host.appendChild(stage);

  ed.el = { tools, stage, fit, page, overlay, guides, marquee, shellFrame, shellMark, hotLayer, shellLoad: shellLoadEl };

  /**
   * 首次给背景层设 `src`（之后**再也不动它**）。
   *
   * ⚠️ 重设 `src` = 整页重载 wasm（一两秒 + 闪一下），"边拖边看"就废了。
   *    wasm 侧每 400ms 自己读一次 `localStorage['shellRenderData']`，
   *    设计台改了数据它自动跟上 —— 所以这里只负责"第一次把它点着"。
   */
  ed.loadShell = function (url) {
    if (!shellFrame || shellFrame.getAttribute('src')) return;
    const ld = ed.el.shellLoad;
    const hide = () => { if (ld) { ld.classList.add('done'); setTimeout(() => ld.remove(), 400); } };
    // `load` 触发时 wasm 往往**还没画完**（8MB 编译要一会儿）—— 再等一小段。
    shellFrame.addEventListener('load', () => setTimeout(hide, 1200));
    // 兜底：无论如何 12 秒后撤掉（宁可提示早消失，也不要一直挡着画布）
    setTimeout(hide, 12000);
    shellFrame.setAttribute('src', url || 'shell-render/');
  };
  // ---------- 块热区（手机端画的块 → 可点选的控件） ----------
  let blockRects = [];
  let selBlock = -1;

  function renderHotzones() {
    if (!hotLayer) return;
    hotLayer.innerHTML = '';
    if (!o.shellFrame) return;
    blockRects.forEach((r, i) => {
      if (!r || !(r.w > 0) || !(r.h > 0)) return;
      const h = document.createElement('div');
      h.className = 'fc-hot' + (i === selBlock ? ' on' : '');
      h.style.left = r.x + 'px';
      h.style.top = r.y + 'px';
      h.style.width = r.w + 'px';
      h.style.height = r.h + 'px';
      h.title = '第 ' + (i + 1) + ' 块 —— 点一下选中它';
      h.onmousedown = (ev) => { ev.stopPropagation(); };
      h.onclick = (ev) => { ev.stopPropagation(); ed.selectBlock(i); };
      hotLayer.appendChild(h);
    });
  }

  /** 选中第 i 个块（-1 = 不选）。 */
  ed.selectBlock = function (i) {
    selBlock = i;
    ed.sel = [];                 // 选块和选元素是互斥的
    renderHotzones();
    renderEls();
    if (o.onBlockSelect) o.onBlockSelect(i >= 0 ? blockRects[i] : null, i);
  };
  ed.selBlockIndex = function () { return selBlock; };
  ed.blockRects = function () { return blockRects; };
  ed.setBlockRects = function (rects) {
    blockRects = rects || [];
    renderHotzones();
  };

  /**
   * 开始盯着 iframe 里的块位置。
   *
   * ⚠️ **轮询而不是事件**：wasm 那边每帧都可能重画（滚动、抽屉动画、数据刷新），
   *    而 `@JsFun` 是单向的（Kotlin → JS），没法"回调"回 Kotlin。
   *    500ms 一次、只比一个 JSON 字符串，代价可以忽略。
   */
  ed.watchBlocks = function () {
    if (!shellFrame || ed._hotTimer) return;
    ed._hotTimer = setInterval(() => {
      try {
        // ⚠️ 读的是**本窗口**的 `__SHELL_BLOCKS__`，不是 iframe 里的 ——
        //    wasm 那边明确写的是 `window.parent`（= 设计台这一层）。
        //    一开始读成 `shellFrame.contentWindow.__SHELL_BLOCKS__`，
        //    结果永远是 null、热区一个都画不出来。
        const b = window.__SHELL_BLOCKS__;
        if (!b) return;
        const next = JSON.stringify(b);
        if (next !== JSON.stringify(blockRects)) ed.setBlockRects(JSON.parse(next));
      } catch (e) { /* 还没就绪 / 跨域 */ }
    }, 500);
  };

  /** 画布按整屏算，所以外壳参考线要标出**内容区**（元素别放到顶栏/底栏上）。 */
  function layoutShellMark() {
    if (!shellMark) return;
    if (!o.shellFrame) { shellMark.style.display = 'none'; return; }
    shellMark.style.display = '';
    const t = num(o.contentTop, num(o.shellTop, SHELL_TOP));
    const b = num(o.shellBottom, SHELL_BOTTOM);
    shellMark.style.top = t + 'px';
    shellMark.style.height = Math.max(20, ed.layer.h - t - b) + 'px';
  }
  ed.layoutShellMark = layoutShellMark;

  // ---------- 工具函数 ----------
  /**
   * 内容区顶部在画布里的 y。
   *
   * ⚠️ 画布是**整屏**（含状态栏/顶栏/底栏，为了和源码渲染背景层精确重合），
   *    而模板、预制块、新元素的坐标都是按**内容区**写的 —— 加东西时必须补这一段，
   *    否则它们会落在顶栏上（用户看到的症状是"加进来的东西被顶栏盖住了"）。
   */
  function contentTop() { return o.shellFrame ? num(o.contentTop, num(o.shellTop, SHELL_TOP)) : 0; }
  ed.contentTop = contentTop;
  /** 宿主切页后调它 —— 每个页面的"内容真正开始的地方"不一样（主页有头部行、别的页有子标签行）。 */
  ed.setContentTop = function (v) { o.contentTop = num(v, SHELL_TOP); layoutShellMark(); };
  ed.contentHeight = function () {
    const t = contentTop();
    const b = num(o.shellBottom, SHELL_BOTTOM);
    return Math.max(40, ed.layer.h - t - b);
  };

  const elById = (id) => ed.layer.els.find((x) => x.id === id) || null;
  const selEls = () => ed.sel.map(elById).filter(Boolean);

  function snap0() { return JSON.stringify(ed.layer); }
  function push(label, live) {
    const now = Date.now();
    if (live && ed.hist.length && ed.hidx === ed.hist.length - 1 &&
        ed._live.label === label && now - ed._live.at < 700) {
      ed.hist[ed.hidx] = { s: snap0(), label };
    } else {
      ed.hist = ed.hist.slice(0, ed.hidx + 1);
      ed.hist.push({ s: snap0(), label });
      if (ed.hist.length > 150) ed.hist.shift();
      ed.hidx = ed.hist.length - 1;
    }
    ed._live = { label, at: now };
  }
  function commit(label, fn) {
    const before = snap0();
    fn && fn();
    if (snap0() === before) return;
    push(label, false);
    render(); fire(label, false);
  }
  function live(label, fn) {
    const before = snap0();
    fn && fn();
    if (snap0() === before) return;
    push(label, true);
    renderEls(); fire(label, true);
  }
  /**
   * 通知宿主。
   * @param label  这一步叫什么（宿主拿它当撤销栈的标签）
   * @param isLive 连续输入（拖滑杆/取色器）—— 宿主会把它和上一步**合并**，
   *               否则拖一次滑杆会留下几十步历史，撤销永远撤不完。
   */
  function fire(label, isLive) { o.onChange && o.onChange(ed.layer, label || '', !!isLive); }
  function fireSel() { o.onSelect && o.onSelect(ed.sel); }

  ed.commit = commit;
  ed.live = live;
  ed.fireSel = fireSel;
  ed.selEls = selEls;
  ed.elById = elById;

  ed.undo = function () {
    if (ed.hidx <= 0) return false;
    ed.hidx--; applyHist(); return true;
  };
  ed.redo = function () {
    if (ed.hidx >= ed.hist.length - 1) return false;
    ed.hidx++; applyHist(); return true;
  };
  function applyHist() {
    ed.layer = normalizeLayer(JSON.parse(ed.hist[ed.hidx].s), ed.layer.w, ed.layer.h);
    ed.sel = ed.sel.filter((id) => elById(id));
    render(); fire();
  }

  ed.canUndo = () => ed.hidx > 0;
  ed.canRedo = () => ed.hidx < ed.hist.length - 1;

  // ---------- 载入 / 取出 ----------
  ed.load = function (raw, ctx) {
    const c = ctx || {};
    ed.layer = normalizeLayer(raw, c.width || ed.layer.w, c.height || ed.layer.h);
    ed.colors = c.colors || ed.colors;
    if (c.dark !== undefined) ed.dark = !!c.dark;
    ed.sel = []; ed.hist = []; ed.hidx = -1;
    push('初始', false);
    // ⚠️ 载入**不**回调 `onChange` —— 数据本来就是从宿主那儿来的，
    //    回调等于"载入即改动"，会把宿主的历史栈灌满重复快照。
    render(); fireSel();
  };
  ed.dump = function () { return JSON.parse(JSON.stringify(ed.layer)); };
  ed.setColors = function (c, dark) { ed.colors = c || {}; if (dark !== undefined) ed.dark = !!dark; render(); };
  ed.resize = function (w, h) { ed.layer.w = clamp(w, 120, 1200); ed.layer.h = clamp(h, 80, 3000); render(); fire('改画布尺寸', false); };

  ed.select = function (ids, add) {
    const arr = (Array.isArray(ids) ? ids : [ids]).filter(Boolean);
    ed.sel = add ? [...new Set(ed.sel.concat(arr))] : arr;
    render(); fireSel();
  };
  ed.addEl = function (kind, at) {
    let e;
    commit('加「' + KIND_LABEL[kind] + '」', () => {
      e = createEl(kind);
      // 摆在画面中央（比摆在左上角更接近"拖进来"的感觉）
      e.x = Math.max(8, Math.round((ed.layer.w - e.w) / 2));
      const top = contentTop();
      e.y = Math.max(top + 8, at ? Math.round(at.y - e.h / 2) : Math.round(top + (ed.layer.h - top - e.h) / 3));
      ed.layer.els.push(e);
      ed.sel = [e.id];
    });
    fireSel();
    return e;
  };
  /**
   * 用模板铺满画布。**会替换现有元素** —— 调用方（元素库那个按钮）负责先确认。
   */
  ed.useTemplate = function (tpl) {
    if (!tpl || typeof tpl.build !== 'function') return;
    const top = contentTop();
    commit('用模板「' + tpl.name + '」', () => {
      ed.layer.els = tpl.build(ed.layer.w, ed.layer.h)
        .map((raw) => createEl(raw.k, raw))
        .filter(Boolean);
      // ★ 模板的坐标是**内容区坐标**，画布是**整屏坐标** —— 补上外壳偏移
      if (top) ed.layer.els.forEach((e) => { e.y += top; });
      ed.layer.code = '';        // 用模板 = 回到画布模式（代码接管状态会让人以为没生效）
      ed.sel = [];
    });
    fireSel();
  };

  ed.delSel = function () {
    if (!ed.sel.length) return;
    // ⚠️ **先清空选中再提交**：`commit` 会回调宿主，宿主那边会去 `elById(sel[0])`
    //    取名字画"已选 X" —— 那时元素已经没了，拿到 null 就崩。
    const ids = ed.sel.slice();
    ed.sel = [];
    commit('删除 ' + ids.length + ' 个元素', () => {
      ed.layer.els = ed.layer.els.filter((x) => !ids.includes(x.id));
    });
    render(); fireSel();
  };
  ed.dupSel = function () {
    if (!ed.sel.length) return;
    const made = [];
    commit('复制元素', () => {
      selEls().forEach((e) => { const c = cloneEl(e); c.x += 12; c.y += 12; ed.layer.els.push(c); made.push(c.id); });
    });
    if (made.length) { ed.sel = made; render(); fireSel(); }
  };
  ed.copySel = function () {
    ed.clip = JSON.stringify(selEls().map((e) => { const c = { ...e }; delete c.id; return c; }));
  };
  ed.paste = function () {
    if (!ed.clip) return;
    const made = [];
    commit('粘贴元素', () => {
      JSON.parse(ed.clip).forEach((raw) => {
        const e = createEl(raw.k, raw); e.id = uid(e.k[0]); e.x += 14; e.y += 14;
        ed.layer.els.push(e); made.push(e.id);
      });
    });
    if (made.length) { ed.sel = made; render(); fireSel(); }
  };
  /** 层级操作：top / bottom / up / down。 */
  ed.zorder = function (how) {
    if (!ed.sel.length) return;
    commit('调整层级', () => {
      const arr = ed.layer.els;
      const picked = arr.filter((x) => ed.sel.includes(x.id));
      const rest = arr.filter((x) => !ed.sel.includes(x.id));
      if (how === 'top') ed.layer.els = rest.concat(picked);
      else if (how === 'bottom') ed.layer.els = picked.concat(rest);
      else {
        const dir = how === 'up' ? 1 : -1;
        const idxs = picked.map((p) => arr.indexOf(p)).sort((a, b) => dir > 0 ? b - a : a - b);
        idxs.forEach((i) => {
          const j = i + dir;
          if (j < 0 || j >= arr.length || ed.sel.includes(arr[j].id)) return;
          const t = arr[i]; arr[i] = arr[j]; arr[j] = t;
        });
      }
    });
  };
  /** 对齐：多选时按整体包围盒对齐，单选时对齐画布。 */
  ed.align = function (how) {
    if (!ed.sel.length) return;
    commit('对齐', () => {
      const es = selEls();
      const single = es.length === 1;
      const box = single
        ? { x: 0, y: 0, w: ed.layer.w, h: ed.layer.h }
        : (() => {
            const x1 = Math.min(...es.map((e) => e.x)), y1 = Math.min(...es.map((e) => e.y));
            const x2 = Math.max(...es.map((e) => e.x + e.w)), y2 = Math.max(...es.map((e) => e.y + e.h));
            return { x: x1, y: y1, w: x2 - x1, h: y2 - y1 };
          })();
      es.forEach((e) => {
        if (how === 'left') e.x = box.x;
        else if (how === 'right') e.x = box.x + box.w - e.w;
        else if (how === 'hcenter') e.x = box.x + (box.w - e.w) / 2;
        else if (how === 'top') e.y = box.y;
        else if (how === 'bottom') e.y = box.y + box.h - e.h;
        else if (how === 'vcenter') e.y = box.y + (box.h - e.h) / 2;
      });
      es.forEach((e) => { e.x = Math.round(e.x); e.y = Math.round(e.y); });
    });
  };

  // ---------- 渲染 ----------
  ed.render = function () { render(); };
  function render() {
    page.style.width = ed.layer.w + 'px';
    page.style.height = ed.layer.h + 'px';
    applyVars();
    renderEls();
    layoutShellMark();   // ★ 画布尺寸可能变了 → 内容区参考线跟着走
    fitScale();          // ★ 画布尺寸可能变了 → 缩放跟着重算
    renderTools();
  }

  /**
   * 画布缩放（自动适配窗口）。
   *
   * ⚠️ 为什么必须自动：画布是 393×620（真机尺寸），而三栏 + 顶栏之后
   *    中间那一格在很多笔记本上**只有 400 多 px 高** —— 不缩放就得上下滚，
   *    而"把一个元素拖到画布底部"要先滚、再拖，手感直接崩掉。
   *
   * ⚠️ 缩放走 `transform`，所以 `ed.scale` 必须参与**所有**坐标换算。
   *    `pt()` 用 `getBoundingClientRect()` 反算，天然正确（rect 也是缩放后的）。
   */
  function fitScale() {
    const pad = 48;                       // .fc-stage 的 padding（24 × 2）
    const availW = Math.max(80, stage.clientWidth - pad);
    const availH = Math.max(80, stage.clientHeight - pad);
    const s = Math.min(availW / ed.layer.w, availH / ed.layer.h, 1);
    ed.scale = Math.max(0.25, Math.round(s * 100) / 100);
    applyScale();
  }
  function applyScale() {
    const s = ed.scale;
    page.style.transform = s === 1 ? '' : 'scale(' + s + ')';
    page.style.transformOrigin = 'top left';
    fit.style.width = (ed.layer.w * s) + 'px';
    fit.style.height = (ed.layer.h * s) + 'px';
    // 工具栏那个百分比（它由 renderTools 建，可能还没建出来）
    if (ed.el.zoom) ed.el.zoom.textContent = Math.round(s * 100) + '%';
  }
  ed.fitScale = fitScale;
  ed.applyScale = applyScale;

  // 容器尺寸变了（窗口缩放 / 切模式 / 侧栏收放）→ 重新适配
  if (typeof ResizeObserver !== 'undefined') {
    const ro = new ResizeObserver(() => fitScale());
    ro.observe(stage);
    ed._ro = ro;
  }

  function applyVars() {
    const c = ed.colors || {};
    const v = {
      '--fc-fg': c.onBackground || '#1b1b1f',
      '--fc-bg': c.background || '#f7f8fa',
      '--fc-primary': c.primary || '#2F6FB5',
      '--fc-on-primary': c.onPrimary || '#ffffff',
      '--fc-surface': c.surface || '#ffffff',
      '--fc-on-surface': c.onSurface || '#1b1b1f',
      '--fc-container': c.secondaryContainer || '#e6e9ef',
      '--fc-on-container': c.onSecondaryContainer || '#1b1b1f',
    };
    for (const k of Object.keys(v)) page.style.setProperty(k, v[k]);
    page.style.background = ed.layer.bg || v['--fc-bg'];
    page.classList.toggle('dark', ed.dark);
  }

  /** 只重画元素层（拖动/连续输入时用，不重建工具栏）。 */
  function renderEls() {
    const keep = new Set(ed.sel);
    page.querySelectorAll('.fc-el').forEach((n) => n.remove());
    ed.layer.els.forEach((e, i) => {
      // ⚠️ 元素**本身就是** elHtml 出来的那个 div（不再套一层包裹）——
      //    套了的话内层的 `position:absolute` 会相对包裹层定位，全部跑偏。
      const tmp = document.createElement('div');
      tmp.innerHTML = elHtml(e, i + 1);
      const node = tmp.firstElementChild;
      if (!node) return;
      node.classList.add('fc-el');
      if (keep.has(e.id)) node.classList.add('on');
      if (e.lock) node.classList.add('lock');
      if (e.hide) node.classList.add('hide');
      node.dataset.id = e.id;
      // ⚠️ 编辑态把动作摘掉：用户点一下想做的是"选中它"，不是"跑这个动作"。
      //    动作只在预览 / 手机上生效（CSS 那边还有一层 `*{pointer-events:none}`，
      //    挡住 code 元素里自己带的 onclick）。
      node.removeAttribute('data-act');
      node.removeAttribute('data-arg');
      node.removeAttribute('onclick');
      page.appendChild(node);
    });
    runCodeScripts();    // ★ 必须在这里：innerHTML 插进去的 <script> 不会自己跑
    renderHandles();
    renderBlankHint();
  }

  /**
   * 执行 `code` 元素里的 `<script>`。
   *
   * ## 为什么必须手动执行
   *
   * `renderEls` 用 `tmp.innerHTML = html` 建节点 —— 而 **`innerHTML` 插入的
   * `<script>` 按规范不会执行**。不手动跑的话，那些"用 `threees.data()` 画的卡片"
   * 在画布上**永远是空的**（只剩 `…` 占位符），用户看到的就是"我拖进去怎么没反应"。
   *
   * ## 两个容易踩的细节
   *
   * ① **原地替换**（`parentNode.replaceChild`），不是"挪到 body 再插"。
   *    因为卡片脚本靠 `document.currentScript.previousElementSibling` 找自己的根节点
   *    （不用 id —— 同一页可能放两块一样的卡）。挪走之后那个 sibling 就不是它了。
   *
   * ② **执行期间把 `setInterval` 降级成"只跑一次"**。
   *    卡片脚本里有 `setInterval(r, 30000)`；而 `renderEls` 每次改动都**重建全部元素**
   *    → 脚本再跑一遍 → 定时器越积越多（拖十次元素 = 十个定时器，而且都在往
   *    **已经被移除的旧节点**上写，纯属泄漏）。设计台里数据是固定的示例数据，
   *    本来也不需要定时刷新。**手机上不受影响**（那边是真浏览器，走原生 setInterval）。
   */
  function runCodeScripts() {
    const scripts = page.querySelectorAll('.fc-el script');
    if (!scripts.length) return;
    const realSetInterval = window.setInterval;
    window.setInterval = function (fn) {
      try { fn(); } catch (e) { /* 卡片脚本坏了不该拖垮整个画布 */ }
      return 0;
    };
    try {
      scripts.forEach((s) => {
        const n = document.createElement('script');
        n.textContent = s.textContent;
        s.parentNode.replaceChild(n, s);      // 原地替换才会执行（见上面 ①）
      });
    } finally {
      window.setInterval = realSetInterval;
    }
  }

  /**
   * 空白画布的提示。
   *
   * ★★ v5.3：**不再画那张卡**。
   *
   * 画布底下铺的是**手机端原本的渲染**（`shell-render` = shared-ui → WasmJS），
   * 它已经把这一页画出来了 —— 再往中间摆一张"这一页还是空的 / 点这里开始"
   * 会**挡住手机端的内容**（截图里正好压住日程块）。
   *
   * 用户要的是"打开就是手机端默认的样子"，不是"打开是一张说明卡"。
   * 想加东西：左栏的元素库 / 起手模板 / 预制块，都在那儿。
   */
  function renderBlankHint() {
    const old = page.querySelector('.fc-blank');
    if (old) old.remove();
    if (true) return;      // 保留函数与调用点：将来若要恢复"空画布提示"，改这里

    const w = document.createElement('div');
    w.className = 'fc-blank';
    const t = document.createElement('div');
    t.className = 'fc-blank-t';
    t.textContent = '这一页还是空的';
    const s = document.createElement('div');
    s.className = 'fc-blank-s';
    s.innerHTML = '用一套<b>起手模板</b>铺好，<br>或者从左栏拖一个元素进来。';
    const row = document.createElement('div');
    row.className = 'fc-blank-row';
    TEMPLATES.forEach((tpl) => {
      const b = document.createElement('button');
      b.className = 'fc-blank-btn';
      b.textContent = tpl.icon + ' ' + tpl.name;
      b.title = tpl.desc;
      b.onclick = () => ed.useTemplate(tpl);
      row.appendChild(b);
    });
    w.appendChild(t);
    w.appendChild(s);
    w.appendChild(row);
    page.appendChild(w);
  }

  /** 选中框 + 手柄 + 参考线（覆盖层，不参与元素流）。 */
  function renderHandles() {    overlay.innerHTML = '';
    const es = selEls();
    if (!es.length) return;
    es.forEach((e) => {
      const b = document.createElement('div');
      b.className = 'fc-selbox';
      b.style.cssText = 'position:absolute;left:' + e.x + 'px;top:' + e.y + 'px;width:' + e.w + 'px;height:' + e.h + 'px;' +
        (e.rot ? 'transform:rotate(' + e.rot + 'deg);' : '');
      overlay.appendChild(b);
    });
    if (es.length === 1 && !es[0].lock) {
      const e = es[0];
      const box = document.createElement('div');
      box.className = 'fc-hbox';
      box.style.cssText = 'position:absolute;left:' + e.x + 'px;top:' + e.y + 'px;width:' + e.w + 'px;height:' + e.h + 'px;' +
        (e.rot ? 'transform:rotate(' + e.rot + 'deg);' : '');
      ['nw', 'n', 'ne', 'e', 'se', 's', 'sw', 'w'].forEach((d) => {
        const h = document.createElement('i');
        h.className = 'fc-h fc-h-' + d; h.dataset.dir = d;
        box.appendChild(h);
      });
      const rot = document.createElement('i');
      rot.className = 'fc-h fc-rot'; rot.dataset.dir = 'rot';
      box.appendChild(rot);
      overlay.appendChild(box);
    }
  }
  ed.renderEls = renderEls;
  ed.renderHandles = renderHandles;
  ed.renderTools = function () { renderTools(); };

  // ---------- 事件：画布上的指针 ----------
  let down = null;

  function pt(ev) {
    const r = page.getBoundingClientRect();
    return { x: (ev.clientX - r.left) / ed.scale, y: (ev.clientY - r.top) / ed.scale };
  }
  ed.pt = pt;

  function onDown(ev) {
    if (ev.button !== 0) return;
    const t = ev.target;
    // 手柄
    if (t.classList && t.classList.contains('fc-h')) { startResize(ev, t.dataset.dir); return; }
    const host = t.closest ? t.closest('.fc-el') : null;
    const id = host ? host.dataset.id : null;

    if (ed.mode === 'brush') { startBrush(ev); return; }

    if (!id) {
      // 空白：开始框选（按住空格 = 平移画布，交给浏览器滚动）
      if (ed.spaceDown) return;
      if (!ev.shiftKey) { ed.sel = []; render(); fireSel(); }
      startMarquee(ev);
      return;
    }
    if (ev.shiftKey) {
      ed.sel = ed.sel.includes(id) ? ed.sel.filter((x) => x !== id) : ed.sel.concat(id);
      render(); fireSel();
      if (!ed.sel.includes(id)) return;
    } else if (!ed.sel.includes(id)) {
      ed.sel = [id]; render(); fireSel();
    }
    startDrag(ev);
  }

  function startDrag(ev) {
    const p0 = pt(ev);
    const es = selEls().filter((e) => !e.lock);
    if (!es.length) return;
    const base = es.map((e) => ({ id: e.id, x: e.x, y: e.y, w: e.w, h: e.h }));
    down = { kind: 'drag', p0, base };
    ev.preventDefault();
  }

  function startResize(ev, dir) {
    const e = selEls()[0];
    if (!e || e.lock) return;
    down = { kind: 'resize', dir, p0: pt(ev), base: { x: e.x, y: e.y, w: e.w, h: e.h, size: e.size }, id: e.id };
    ev.preventDefault();
    ev.stopPropagation();
  }

  function startMarquee(ev) {
    const p0 = pt(ev);
    down = { kind: 'marquee', p0, add: ev.shiftKey, init: ed.sel.slice() };
    ev.preventDefault();
  }

  function onMove(ev) {
    if (!down) return;
    const p = pt(ev);
    if (down.kind === 'drag') doDrag(p, ev);
    else if (down.kind === 'resize') doResize(p, ev);
    else if (down.kind === 'marquee') doMarquee(p);
    else if (down.kind === 'brush') doBrush(p);
  }

  function doDrag(p, ev) {
    let dx = p.x - down.p0.x, dy = p.y - down.p0.y;
    if (ev.shiftKey) {                       // 约束到水平/垂直
      if (Math.abs(dx) > Math.abs(dy)) dy = 0; else dx = 0;
    }
    const hit = snapMove(down.base, dx, dy);
    dx = hit.dx; dy = hit.dy;
    down.base.forEach((b) => {
      const e = elById(b.id); if (!e) return;
      e.x = Math.round(b.x + dx); e.y = Math.round(b.y + dy);
    });
    renderEls(); drawGuides(hit.lines);
  }

  function doResize(p, ev) {
    const e = elById(down.id); if (!e) return;
    const b = down.base, d = down.dir;
    // ⚠️ 旋转必须**先**判、并且立刻 return —— 否则会掉进下面的等比分支，
    //    把 w/h 按 shift 算一遍（转个角度顺手把元素拉变形）。
    if (d === 'rot') {
      const cx = b.x + b.w / 2, cy = b.y + b.h / 2;
      let a = Math.atan2(p.y - cy, p.x - cx) * 180 / Math.PI + 90;
      if (ev.shiftKey) a = Math.round(a / 15) * 15;      // 按住 Shift = 每 15° 一档
      e.rot = Math.round(a);
      renderEls(); return;
    }
    let x = b.x, y = b.y, w = b.w, h = b.h;
    const dx = p.x - down.p0.x, dy = p.y - down.p0.y;
    if (d.includes('e')) w = b.w + dx;
    if (d.includes('s')) h = b.h + dy;
    if (d.includes('w')) { x = b.x + dx; w = b.w - dx; }
    if (d.includes('n')) { y = b.y + dy; h = b.h - dy; }
    if (ev.shiftKey && w > 0 && h > 0) {     // 等比
      const r = b.w / b.h;
      if (Math.abs(dx) > Math.abs(dy)) h = w / r; else w = h * r;
      if (d.includes('w')) x = b.x + (b.w - w);
      if (d.includes('n')) y = b.y + (b.h - h);
    }
    // 文字/按钮**横向**拉大 → 顺手把字号也带上（PPT 里也是这个手感）；
    // 只拉高不拉宽（= 想让文字换行/多几行）不动字号，否则"多写两行"会变成"字变大"。
    if (e.k === 'text' || e.k === 'button') {
      if (Math.abs(w - b.w) > 1 && Math.abs(h - b.h) < 1 && b.w > 0) {
        e.size = clamp(Math.round(b.size * (w / b.w)), 6, 200);
      }
    }
    if (w < 4) w = 4;
    if (h < 4) h = 4;
    e.x = Math.round(x); e.y = Math.round(y); e.w = Math.round(w); e.h = Math.round(h);
    renderEls();
  }

  function doMarquee(p) {
    const x1 = Math.min(down.p0.x, p.x), y1 = Math.min(down.p0.y, p.y);
    const x2 = Math.max(down.p0.x, p.x), y2 = Math.max(down.p0.y, p.y);
    marquee.hidden = false;
    marquee.style.cssText = 'position:absolute;left:' + x1 + 'px;top:' + y1 + 'px;width:' + (x2 - x1) + 'px;height:' + (y2 - y1) + 'px;';
    const hit = ed.layer.els.filter((e) => !e.hide && e.x < x2 && e.x + e.w > x1 && e.y < y2 && e.y + e.h > y1)
      .map((e) => e.id);
    ed.sel = down.add ? [...new Set(down.init.concat(hit))] : hit;
    renderHandles();
    page.querySelectorAll('.fc-el').forEach((n) => n.classList.toggle('on', ed.sel.includes(n.dataset.id)));
  }

  function onUp() {
    if (!down) return;
    const k = down.kind;
    if (k === 'drag') { commit('移动元素', () => {}); }
    else if (k === 'resize') { commit('调整大小', () => {}); }
    else if (k === 'marquee') { marquee.hidden = true; fireSel(); }
    else if (k === 'brush') { endBrush(); }
    down = null;
    drawGuides([]);
  }

  // ---------- 参考线与吸附 ----------
  function snapMove(base, dx, dy) {
    if (ed.opts.noSnap) return { dx, dy, lines: [] };
    const lines = [];
    const edgesX = [0, ed.layer.w / 2, ed.layer.w];
    const edgesY = [0, ed.layer.h / 2, ed.layer.h];
    ed.layer.els.forEach((e) => {
      if (base.some((b) => b.id === e.id) || e.hide) return;
      edgesX.push(e.x, e.x + e.w / 2, e.x + e.w);
      edgesY.push(e.y, e.y + e.h / 2, e.y + e.h);
    });
    const moved = base.map((b) => ({ x1: b.x + dx, x2: b.x + b.w + dx, y1: b.y + dy, y2: b.y + b.h + dy }));
    let bestX = null, bestY = null;
    moved.forEach((m) => {
      [m.x1, (m.x1 + m.x2) / 2, m.x2].forEach((v) => {
        edgesX.forEach((t) => { const d = t - v; if (Math.abs(d) <= SNAP && (!bestX || Math.abs(d) < Math.abs(bestX.d))) bestX = { d, at: t }; });
      });
      [m.y1, (m.y1 + m.y2) / 2, m.y2].forEach((v) => {
        edgesY.forEach((t) => { const d = t - v; if (Math.abs(d) <= SNAP && (!bestY || Math.abs(d) < Math.abs(bestY.d))) bestY = { d, at: t }; });
      });
    });
    if (bestX) { dx += bestX.d; lines.push({ v: bestX.at }); }
    if (bestY) { dy += bestY.d; lines.push({ h: bestY.at }); }
    return { dx, dy, lines };
  }

  function drawGuides(lines) {
    guides.innerHTML = '';
    (lines || []).forEach((l) => {
      const d = document.createElement('div');
      if (l.v !== undefined) d.style.cssText = 'position:absolute;left:' + l.v + 'px;top:0;width:1px;height:100%;';
      else d.style.cssText = 'position:absolute;top:' + l.h + 'px;left:0;height:1px;width:100%;';
      d.className = 'fc-guide';
      guides.appendChild(d);
    });
  }

  // ---------- 手绘 ----------
  function startBrush(ev) {
    const p = pt(ev);
    ed.brush = { pts: [[p.x, p.y]] };
    down = { kind: 'brush' };
    ev.preventDefault();
  }
  function doBrush(p) {
    if (!ed.brush) return;
    ed.brush.pts.push([p.x, p.y]);
    // 实时画一条临时的
    guides.innerHTML = '';
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('width', ed.layer.w); svg.setAttribute('height', ed.layer.h);
    svg.style.cssText = 'position:absolute;inset:0;overflow:visible';
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', pathOf(ed.brush.pts));
    path.setAttribute('fill', 'none');
    path.setAttribute('stroke', ed.colors.primary || '#2F6FB5');
    path.setAttribute('stroke-width', '3');
    path.setAttribute('stroke-linecap', 'round');
    svg.appendChild(path);
    guides.appendChild(svg);
  }
  function endBrush() {
    const b = ed.brush; ed.brush = null;
    guides.innerHTML = '';
    if (!b || b.pts.length < 2) return;
    const xs = b.pts.map((p) => p[0]), ys = b.pts.map((p) => p[1]);
    const x1 = Math.min(...xs), y1 = Math.min(...ys), x2 = Math.max(...xs), y2 = Math.max(...ys);
    const w = Math.max(4, x2 - x1), h = Math.max(4, y2 - y1);
    const pts = b.pts.map((p) => [p[0] - x1, p[1] - y1]);
    commit('手绘', () => {
      const e = createEl('draw', { x: Math.round(x1), y: Math.round(y1), w: Math.round(w), h: Math.round(h), path: pathOf(pts) });
      ed.layer.els.push(e);
      ed.sel = [e.id];
    });
    fireSel();
  }
  /** 点集 → 平滑 SVG path（中点二次贝塞尔，比折线顺眼得多）。 */
  function pathOf(pts) {
    if (!pts.length) return '';
    if (pts.length < 3) return 'M' + pts.map((p) => r2(p[0]) + ' ' + r2(p[1])).join('L');
    let d = 'M' + r2(pts[0][0]) + ' ' + r2(pts[0][1]);
    for (let i = 1; i < pts.length - 1; i++) {
      const mx = (pts[i][0] + pts[i + 1][0]) / 2, my = (pts[i][1] + pts[i + 1][1]) / 2;
      d += 'Q' + r2(pts[i][0]) + ' ' + r2(pts[i][1]) + ' ' + r2(mx) + ' ' + r2(my);
    }
    const last = pts[pts.length - 1];
    d += 'L' + r2(last[0]) + ' ' + r2(last[1]);
    return d;
  }
  ed.pathOf = pathOf;

  // ---------- 绑定 ----------
  page.addEventListener('mousedown', onDown);
  window.addEventListener('mousemove', onMove);
  window.addEventListener('mouseup', onUp);
  page.addEventListener('dblclick', onDblClick);
  page.addEventListener('contextmenu', onContextMenu);
  ed._teardown = () => {
    window.removeEventListener('mousemove', onMove);
    window.removeEventListener('mouseup', onUp);
    document.removeEventListener('keydown', ed._onKey);
    if (ed._ro) { ed._ro.disconnect(); ed._ro = null; }
    if (ed._hotTimer) { clearInterval(ed._hotTimer); ed._hotTimer = null; }
  };
  ed._onKey = (ev) => onKey(ev);
  document.addEventListener('keydown', ed._onKey);

  /** 双击文字 → 原地编辑（PPT 手感里最不能少的一条）。 */
  function onDblClick(ev) {
    const host = ev.target.closest ? ev.target.closest('.fc-el') : null;
    if (!host) return;
    const e = elById(host.dataset.id);
    if (!e || e.lock) return;
    // 文字 / 按钮：直接在这一块里打字。其余类型（形状、图片、代码…）没有"就地改"的语义，
    // 双击 = 把焦点交给右栏属性面板（用户想改的是它的字段，不是它的内容）。
    if (e.k !== 'text' && e.k !== 'button') { fireSel(); return; }
    ev.preventDefault();
    host.contentEditable = 'true';
    host.classList.add('editing');
    host.focus();
    const r = document.createRange(); r.selectNodeContents(host);
    const s = window.getSelection(); s.removeAllRanges(); s.addRange(r);
    host.addEventListener('keydown', (k) => {
      if (k.key === 'Escape') { k.preventDefault(); host.blur(); }
      k.stopPropagation();                 // 别让 Backspace 传到画布的"删除元素"
    });
    // ⚠️ `{once:true}`：提交后会重画（这个节点被移除），监听跟着一起走。
    //    不 once 的话，改两次文字就会提交两次（第二次拿的是旧节点）。
    host.addEventListener('blur', () => {
      host.contentEditable = 'false';
      const txt = (host.innerText || '').replace(/\n$/, '');
      commit('改文字', () => { e.text = txt; });
    }, { once: true });
  }

  function onContextMenu(ev) {
    ev.preventDefault();
    const host = ev.target.closest ? ev.target.closest('.fc-el') : null;
    if (host) {
      const id = host.dataset.id;
      if (!ed.sel.includes(id)) { ed.sel = [id]; render(); fireSel(); }
    }
    const e = ed.sel.length === 1 ? selEls()[0] : null;
    const items = [];
    if (e) {
      items.push({ label: '复制 (Ctrl+C)', fn: () => ed.copySel() });
      items.push({ label: '粘贴 (Ctrl+V)', fn: () => ed.paste(), disabled: !ed.clip });
      items.push({ label: '再制 (Ctrl+D)', fn: () => ed.dupSel() });
      items.push({ sep: true });
      items.push({ label: '置顶', fn: () => ed.zorder('top') });
      items.push({ label: '上移一层', fn: () => ed.zorder('up') });
      items.push({ label: '下移一层', fn: () => ed.zorder('down') });
      items.push({ label: '置底', fn: () => ed.zorder('bottom') });
      items.push({ sep: true });
      items.push({ label: e.lock ? '解锁' : '锁定', fn: () => commit(e.lock ? '解锁' : '锁定', () => { e.lock = !e.lock; }) });
      items.push({ label: e.hide ? '显示' : '隐藏', fn: () => commit(e.hide ? '显示' : '隐藏', () => { e.hide = !e.hide; }) });
      items.push({ label: '改个名字', fn: () => { const n = prompt('图层名', e.name || elLabel(e)); if (n !== null) commit('改名', () => { e.name = n.trim(); }); } });
      items.push({ sep: true });
      items.push({ label: '删除 (Del)', danger: true, fn: () => ed.delSel() });
    } else {
      items.push({ label: '粘贴 (Ctrl+V)', fn: () => ed.paste(), disabled: !ed.clip });
      items.push({ label: '清空画布', danger: true, fn: () => { if (confirm('清空这一页的画布？')) commit('清空', () => { ed.layer.els = []; }); } });
    }
    o.onMenu && o.onMenu(ev.clientX, ev.clientY, items);
  }

  /** 键盘。⚠️ 输入框里打字时全部让路 —— 否则改个属性就把元素删了。 */
  function onKey(ev) {
    const t = ev.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable || t.tagName === 'SELECT')) return;
    const meta = ev.ctrlKey || ev.metaKey;
    const step = ev.shiftKey ? 10 : 1;
    // ⚠️ 撤销/重做可以交给宿主统一管（`opts.ownHistory === false`）——
    //    否则画布和宿主各有一份历史，Ctrl+Z 会**两边同时撤**，一次退两步。
    //    让路时必须**不** preventDefault，宿主那边才收得到。
    const own = o.ownHistory !== false;
    if (meta && ev.key.toLowerCase() === 'z') {
      if (!own) return;
      ev.preventDefault(); ev.shiftKey ? ed.redo() : ed.undo(); ed.fireSel(); return;
    }
    if (meta && ev.key.toLowerCase() === 'y') {
      if (!own) return;
      ev.preventDefault(); ed.redo(); ed.fireSel(); return;
    }
    if (meta && ev.key.toLowerCase() === 'c') { ed.copySel(); return; }
    if (meta && ev.key.toLowerCase() === 'v') { ed.paste(); return; }
    if (meta && ev.key.toLowerCase() === 'd') { ev.preventDefault(); ed.dupSel(); return; }
    if (meta && ev.key.toLowerCase() === 'a') {
      ev.preventDefault(); ed.sel = ed.layer.els.filter((e) => !e.hide && !e.lock).map((e) => e.id);
      render(); fireSel(); return;
    }
    if (ev.key === 'Delete' || ev.key === 'Backspace') { ev.preventDefault(); ed.delSel(); return; }
    if (ev.key === 'Escape') { ed.sel = []; render(); fireSel(); return; }
    const dirs = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1] };
    if (dirs[ev.key] && ed.sel.length) {
      ev.preventDefault();
      const [dx, dy] = dirs[ev.key];
      live('微调位置', () => selEls().forEach((e) => { if (!e.lock) { e.x += dx * step; e.y += dy * step; } }));
      ed.fireSel();
    }
  }

  // ---------- 工具栏 ----------
  function renderTools() {
    tools.innerHTML = '';
    const mk = (label, title, fn, cls) => {
      const b = document.createElement('button');
      b.className = 'fc-tb' + (cls ? ' ' + cls : '');
      b.innerHTML = label; b.title = title || '';
      b.onclick = fn;
      tools.appendChild(b);
      return b;
    };
    const sep = () => { const s = document.createElement('span'); s.className = 'fc-tsep'; tools.appendChild(s); };

    mk('↖', '选择（V）', () => setMode('select'), ed.mode === 'select' ? 'on' : '');
    mk('✎', '手绘（B）—— 在画布上按住拖动', () => setMode('brush'), ed.mode === 'brush' ? 'on' : '');
    sep();
    const a = (icon, how, title) => mk(icon, title, () => ed.align(how), 'sm');
    a('⇤', 'left', '左对齐'); a('↔', 'hcenter', '水平居中'); a('⇥', 'right', '右对齐');
    a('⤒', 'top', '顶对齐'); a('↕', 'vcenter', '垂直居中'); a('⤓', 'bottom', '底对齐');
    sep();
    mk('⤊', '置顶', () => ed.zorder('top'), 'sm');
    mk('⤋', '置底', () => ed.zorder('bottom'), 'sm');
    mk('⌫', '删除选中 (Del)', () => ed.delSel(), 'sm');
    sep();
    mk('⇕', '画布高度（px，= 手机 dp）', () => {
      const h = prompt('画布高度（px，= 手机 dp）\n\n提示：手机内容区大约 620，整屏约 800。', String(ed.layer.h));
      if (h !== null) ed.resize(ed.layer.w, num(h, ed.layer.h));
    }, 'sm');
    mk('▦', '画布底色', () => {
      const c = prompt('画布底色（#RRGGBB，留空 = 跟随外观）', ed.layer.bg || '');
      if (c !== null) commit('画布底色', () => { ed.layer.bg = c.trim(); });
    }, 'sm');
    const zl = document.createElement('span');
    zl.className = 'fc-zoom';
    zl.title = '画布缩放（自动适应窗口）';
    zl.textContent = Math.round(ed.scale * 100) + '%';
    ed.el.zoom = zl;
    tools.appendChild(zl);
    mk('⤢', '适应窗口（重算缩放）', () => fitScale(), 'sm');
    sep();
    mk('↶', '撤销 (Ctrl+Z)', () => { ed.undo(); ed.fireSel(); }, 'sm');
    mk('↷', '重做 (Ctrl+Shift+Z)', () => { ed.redo(); ed.fireSel(); }, 'sm');
    sep();
    // ★ 一键清空 / 一键恢复默认 —— 由宿主实现（它才知道"默认"是什么）
    mk('🗑', '清空这一页', () => o.onClear && o.onClear(), 'sm');
    mk('↺', '恢复成手机端默认布局', () => o.onRestoreDefault && o.onRestoreDefault(), 'sm');
    const grow = document.createElement('span'); grow.style.flex = '1'; tools.appendChild(grow);
    mk('⌨ 代码', '看/改这一页编译出来的 HTML', () => o.onCode && o.onCode(), 'sm');
    mk('👁 预览', '按真机方式跑一遍（动作会真的触发）', () => o.onPreview && o.onPreview(), 'sm');
  }
  function setMode(m) { ed.mode = m; renderTools(); page.classList.toggle('brush', m === 'brush'); }
  ed.setMode = setMode;
  ed.modeName = () => ed.mode;

  renderTools();
  return ed;
}

// ============================================================ §10 面板：小控件

/** 一行「标签 + 控件」。 */
function frow(label, node, hint) {
  const r = document.createElement('div');
  r.className = 'fc-row';
  const l = document.createElement('label');
  l.className = 'fc-lab'; l.textContent = label;
  if (hint) l.title = hint;
  r.appendChild(l);
  const b = document.createElement('div');
  b.className = 'fc-ctl';
  b.appendChild(node);
  r.appendChild(b);
  return r;
}

/** 数字输入。返回 { node, set } —— `set` 让画布拖动时能同步回来（不用重建面板）。 */
function fnum(value, min, max, step, onChange, suffix) {
  const w = document.createElement('div');
  w.className = 'fc-numwrap';
  const i = document.createElement('input');
  i.type = 'number'; i.className = 'fc-inp';
  i.value = Math.round(num(value, 0) * 100) / 100;
  if (min !== undefined) i.min = min;
  if (max !== undefined) i.max = max;
  i.step = step || 1;
  i.oninput = () => {
    let v = num(i.value, 0);
    if (min !== undefined) v = Math.max(min, v);
    if (max !== undefined) v = Math.min(max, v);
    onChange(v);
  };
  w.appendChild(i);
  if (suffix) { const s = document.createElement('span'); s.className = 'fc-suf'; s.textContent = suffix; w.appendChild(s); }
  return { node: w, set: (v) => { if (document.activeElement !== i) i.value = Math.round(num(v, 0) * 100) / 100; } };
}

function ftext(value, onChange, placeholder, multiline) {
  const i = document.createElement(multiline ? 'textarea' : 'input');
  i.className = 'fc-inp' + (multiline ? ' fc-area' : '');
  if (multiline) i.rows = multiline; else i.type = 'text';
  i.value = value == null ? '' : value;
  i.placeholder = placeholder || '';
  i.oninput = () => onChange(i.value);
  return { node: i, set: (v) => { if (document.activeElement !== i) i.value = v == null ? '' : v; } };
}

/** 取色器 + 一排常用色 + "跟随外观"（留空）。 */
function fcolor(value, onChange, allowEmpty) {
  const w = document.createElement('div');
  w.className = 'fc-colorwrap';
  const i = document.createElement('input');
  i.type = 'color'; i.className = 'fc-color';
  i.value = isHex(value) ? value : '#2F6FB5';
  i.oninput = () => onChange(i.value);
  w.appendChild(i);
  const sw = document.createElement('div');
  sw.className = 'fc-swatches';
  SWATCHES.forEach((c) => {
    const b = document.createElement('button');
    b.className = 'fc-sw'; b.style.background = c; b.title = c;
    b.onclick = () => { i.value = c; onChange(c); };
    sw.appendChild(b);
  });
  if (allowEmpty) {
    const b = document.createElement('button');
    b.className = 'fc-sw fc-swnone'; b.title = '跟随外观（不指定）'; b.textContent = '自';
    b.onclick = () => onChange('');
    sw.appendChild(b);
  }
  w.appendChild(sw);
  return { node: w, set: (v) => { if (isHex(v)) i.value = v; } };
}

function fselect(value, options, onChange) {
  const s = document.createElement('select');
  s.className = 'fc-inp';
  options.forEach((o) => {
    const op = document.createElement('option');
    op.value = o.v; op.textContent = o.label;
    s.appendChild(op);
  });
  s.value = value;
  s.onchange = () => onChange(s.value);
  return { node: s, set: (v) => { if (document.activeElement !== s) s.value = v; } };
}

function fcheck(value, label, onChange) {
  const w = document.createElement('label');
  w.className = 'fc-check';
  const i = document.createElement('input');
  i.type = 'checkbox'; i.checked = !!value;
  i.onchange = () => onChange(i.checked);
  w.appendChild(i);
  const s = document.createElement('span'); s.textContent = label; w.appendChild(s);
  return { node: w, set: (v) => { i.checked = !!v; } };
}

/** 一组小按钮（对齐、层级这类）。 */
function fbtns(items) {
  const w = document.createElement('div');
  w.className = 'fc-btns';
  items.forEach((it) => {
    const b = document.createElement('button');
    b.className = 'fc-btn' + (it.danger ? ' danger' : '');
    b.innerHTML = it.label; b.title = it.title || '';
    b.onclick = it.fn;
    w.appendChild(b);
  });
  return { node: w };
}

function fsec(title, collapsedDefault) {
  const d = document.createElement('details');
  d.className = 'fc-sec';
  d.open = !collapsedDefault;
  const s = document.createElement('summary');
  s.textContent = title;
  d.appendChild(s);
  const body = document.createElement('div');
  body.className = 'fc-secbody';
  d.appendChild(body);
  return { node: d, body };
}

// ============================================================ §10.5 起手模板

/**
 * 起手模板 —— 专治"打开一片空白，不知道从哪下手"。
 *
 * 每个模板就是一组元素（`build(W, H)` 返回数组，id 由 `createEl` 补）。
 *
 * ⚠️ 颜色一律写 `var(--fc-xxx)` 而不是具体色值 —— 编译时那 8 个变量会按
 *    **当前外观**注入（见 `compile` 的 vars），所以模板做出来的东西
 *    天生跟着用户的配色走（换深色也不会变成一块突兀的白）。
 *    具体色值只在"用户自己挑过色"的地方出现。
 */
const TEMPLATES = [
  {
    id: 'hero', name: '标题 + 按钮', icon: '🏠',
    desc: '最经典的一屏：一句话 + 一个按钮',
    build: (W) => [
      { k: 'text', x: 24, y: 56, w: W - 48, h: 42, text: '你好，今天也要加油', size: 26, weight: 700 },
      { k: 'text', x: 24, y: 104, w: W - 48, h: 44, text: '这里写一句想说的话 —— 双击就能改。', size: 14, lh: 1.6 },
      { k: 'button', x: 24, y: 168, w: 150, h: 46, text: '开始', size: 15, weight: 600, align: 'center', radius: 23, shadow: 2 },
      { k: 'button', x: 186, y: 168, w: 150, h: 46, text: '看日程', size: 15, align: 'center', radius: 23, fill: 'var(--fc-container)', color: 'var(--fc-on-container)', act: 'page', arg: 'schedule' },
    ],
  },
  {
    id: 'grid', name: '九宫格入口', icon: '▦',
    desc: '3×3 图标格 —— 自己做一屏入口',
    build: (W) => {
      const pad = 16, gap = 10;
      const cell = (W - pad * 2 - gap * 2) / 3;
      const icons = ['calendar', 'book', 'clock', 'bell', 'chat', 'camera', 'star', 'cloud', 'gear'];
      const labels = ['日程', '笔记', '待办', '提醒', '消息', '拍照', '收藏', '网盘', '设置'];
      const acts = ['schedule', 'mdnote', 'notes', '', '', '', '', '', 'settings'];
      const out = [];
      for (let i = 0; i < 9; i++) {
        const cx = pad + (i % 3) * (cell + gap);
        const cy = 40 + Math.floor(i / 3) * (cell + 28);
        const box = { k: 'box', x: cx, y: cy, w: cell, h: cell, radius: 18, fill: 'var(--fc-surface)', shadow: 1 };
        if (acts[i]) { box.act = 'page'; box.arg = acts[i]; }
        out.push(box);
        out.push({ k: 'icon', x: cx + cell / 2 - 13, y: cy + cell / 2 - 22, w: 26, h: 26, icon: icons[i], fill: 'var(--fc-primary)' });
        out.push({ k: 'text', x: cx, y: cy + cell / 2 + 10, w: cell, h: 18, text: labels[i], size: 12, align: 'center' });
      }
      return out;
    },
  },
  {
    id: 'card', name: '一张卡片', icon: '🃏',
    desc: '卡片 + 标题 + 说明 + 按钮',
    build: (W) => [
      { k: 'box', x: 16, y: 32, w: W - 32, h: 190, radius: 20, fill: 'var(--fc-surface)', shadow: 2 },
      { k: 'text', x: 34, y: 54, w: W - 68, h: 30, text: '今天的安排', size: 19, weight: 600 },
      { k: 'text', x: 34, y: 92, w: W - 68, h: 60, text: '把这里换成你想说的话。\n换行直接用回车。', size: 14, lh: 1.7 },
      { k: 'button', x: 34, y: 164, w: 120, h: 40, text: '去看看', size: 14, align: 'center', radius: 20 },
    ],
  },
  {
    id: 'banner', name: '大图 + 标题', icon: '🖼',
    desc: '顶部一张大图，下面压标题',
    build: (W) => [
      { k: 'image', x: 0, y: 0, w: W, h: 240, radius: 0, fit: 'cover' },
      { k: 'box', x: 0, y: 170, w: W, h: 90, fill: 'var(--fc-bg)', radius: 0, op: 0.86 },
      { k: 'text', x: 22, y: 190, w: W - 44, h: 34, text: '放一张图，压一行字', size: 21, weight: 700 },
      { k: 'text', x: 22, y: 226, w: W - 44, h: 24, text: '图片要自己上传（手机端不能联网取图）', size: 12 },
    ],
  },
];

// ============================================================ §10.6 示例数据桥

/**
 * **设计台里的假桥** —— 让 HTML 块在设计台里就能画出"看得见的效果"。
 *
 * ## 为什么必须有
 *
 * 手机端的 HTML 块靠 `window.threees.data('today-agenda')` 取真数据
 * （见 `AiHomePage.kt` 的 `HtmlBridge`）。**设计台里没有那个桥** ——
 * 于是同一个 HTML 在手机上是一张日程卡，在设计台里是**一片空白**。
 * 用户的原话：「我现在看不到具体编辑的效果怎么样」。
 *
 * 所以这里装一个**只读的假桥**，返回**固定的示例数据**。效果：
 * · 画布上、预览里，HTML 块都画出真实样子（有日程、有天气、有日期）；
 * · 用户拖一个「代码」元素进去，也能立刻看到 `threees.data()` 有没有写对；
 * · 导出到手机后，同一段代码换成真数据 —— **同一份代码，两个数据源**。
 *
 * ⚠️ 只在**没有真桥**时装（`if (window.threees) return`）——
 *    万一哪天这段代码被搬进宿主，它不会去覆盖真的。
 *
 * ⚠️ 示例数据要**像真的**：日期用"今天"算出来的，日程用学生真实会遇到的东西。
 *    给一堆 `item1 / item2` 的话，用户看不出"标题长了会不会挤"。
 */
const MOCK_DATA = (() => {
  const now = new Date();
  const wd = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'][now.getDay()];
  const pad2 = (n) => (n < 10 ? '0' + n : '' + n);
  const iso = now.getFullYear() + '-' + pad2(now.getMonth() + 1) + '-' + pad2(now.getDate());
  const hours = [];
  for (let i = 0; i < 6; i++) {
    const h = (now.getHours() + i) % 24;
    hours.push({
      time: pad2(h) + ':00', temp: 22 + Math.round(Math.sin(i) * 4),
      label: ['晴', '多云', '阴', '小雨', '晴', '多云'][i], symbol: '', rainy: i === 3,
      humidity: 55 + i * 3, wind: 2 + (i % 3),
    });
  }
  const days = [];
  const wds = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
  for (let i = 0; i < 5; i++) {
    const d = new Date(now.getTime() + i * 86400000);
    days.push({
      date: d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()),
      label: i === 0 ? '今天' : wds[d.getDay()],
      tMin: 18 + (i % 3), tMax: 27 + (i % 4),
      label2: '', symbol: '', precipSum: i % 4,
    });
  }
  return {
    now: {
      iso: iso + 'T' + pad2(now.getHours()) + ':' + pad2(now.getMinutes()) + ':00',
      date: iso, weekday: wd, weekdayIndex: now.getDay() === 0 ? 7 : now.getDay(),
      hour: now.getHours(), minute: now.getMinutes(),
    },
    'today-agenda': {
      date: iso, count: 3,
      items: [
        { id: 'demo-1', title: '高等数学', time: '08:00', endTime: '08:45', location: '教学楼 A302', calendarId: 'course', color: '#2F6FB5' },
        { id: 'demo-2', title: '英语听力', time: '10:00', endTime: '10:45', location: '外语楼 B201', calendarId: 'course', color: '#30A46C' },
        { id: 'demo-3', title: '小组讨论：电路实验报告', time: '14:30', endTime: '15:30', location: '图书馆 3F', calendarId: 'personal', color: '#F5A524' },
      ],
    },
    weather: {
      place: '泉州', stale: false, updatedAt: Math.floor(now.getTime() / 1000),
      note: '示例数据（设计台里看到的就是手机上的样子，只是数字不同）',
      hours: hours, days: days,
    },
  };
})();

function installMockBridge() {
  if (typeof window === 'undefined' || window.threees) return;
  window.threees = {
    data: function (name, argsJson) {
      const d = MOCK_DATA[name];
      if (!d) return JSON.stringify({ error: '认不出「' + name + '」。可用的有：now / today-agenda / weather' });
      // 支持 `{limit:n}`（和真桥一个语义：切前 n 条）
      const args = (() => { try { return JSON.parse(argsJson || '{}'); } catch (e) { return {}; } })();
      const out = JSON.parse(JSON.stringify(d));
      if (args && args.limit > 0 && out.items) out.items = out.items.slice(0, args.limit);
      return JSON.stringify(out);
    },
    names: function () { return Object.keys(MOCK_DATA).join(','); },
    // 设计台里点动作**不真的跳页**（跳了就回不来了），只提示一下
    act: function (kind, arg) {
      const t = document.createElement('div');
      t.className = 'fc-mocktoast';
      t.textContent = '（设计台里不真的执行）动作 → ' + kind + (arg ? ' : ' + arg : '');
      document.body.appendChild(t);
      setTimeout(() => t.remove(), 2200);
      return JSON.stringify({ ok: true, note: '设计台预览，未真的执行' });
    },
  };
}

// ============================================================ §10.7 手机端默认布局（复刻）

/*
 * ⚠️⚠️ 这几段 HTML 有**两条硬约束**，改的时候必须守住：
 *
 * ① **不许用 `id`** —— 用 `data-fc="xxx"` + `document.currentScript.previousElementSibling`
 *    去找自己的根节点。
 *    理由：同一页可以放**两块一样的卡**（比如「信息密集」里日程卡出现两次），
 *    用 `getElementById` 的话两块的脚本会抢同一个 id，第二块把第一块的内容盖掉。
 *
 * ② 脚本里**只用 `setInterval` 刷新**，不用别的方式 —— 因为设计台在执行这些脚本时
 *    会把 `setInterval` 临时降级成"只跑一次"（见 `runCodeScripts`），
 *    避免每次重画都留下一个永不清理的定时器（拖十次元素 = 十个定时器）。
 *
 * 还有一条**不是硬约束但很重要**：颜色一律写 `var(--fc-*)`。
 * 编译时按**当前外观**注入，所以换深色主题时它们跟着变，不会变成一块突兀的白。
 */

/** 天气卡（复刻手机端的 weather widget）。 */
const PRESET_WEATHER_HTML = [
  '<div style="height:100%;box-sizing:border-box;padding:14px;border-radius:18px;',
  'background:linear-gradient(135deg,var(--fc-primary,#2F6FB5),var(--fc-on-primary,#7A9FE0));',
  'color:var(--fc-on-primary,#fff);display:flex;flex-direction:column;justify-content:space-between">',
  '<div style="display:flex;justify-content:space-between;align-items:flex-start">',
  '<div><div data-fc="place" style="font-size:12px;opacity:.88">…</div>',
  '<div data-fc="temp" style="font-size:32px;font-weight:700;line-height:1.15">—</div></div>',
  '<div style="text-align:right"><div data-fc="label" style="font-size:13px">—</div>',
  '<div data-fc="range" style="font-size:11px;opacity:.85"></div></div></div>',
  '<div data-fc="hours" style="display:flex;gap:12px;font-size:11px;opacity:.92"></div></div>',
  '<script>(function(){var R=document.currentScript.previousElementSibling;',
  'function q(n){return R.querySelector("[data-fc=\\""+n+"\\"]")}',
  'function r(){try{var w=JSON.parse(window.threees.data("weather","{}"));if(w.error)return;',
  'var H=w.hours||[],D=w.days||[],h=H[0]||{},d=D[0]||{};',
  'q("place").textContent=w.place||"";',
  'q("temp").textContent=(h.temp!=null?Math.round(h.temp)+"°":"—");',
  'q("label").textContent=h.label||"";',
  'q("range").textContent=(d.tMin!=null?Math.round(d.tMin)+"° / "+Math.round(d.tMax)+"°":"");',
  'q("hours").innerHTML=H.slice(0,4).map(function(x){',
  'return "<span>"+String(x.time||"").slice(0,2)+"时 "+(x.temp!=null?Math.round(x.temp)+"°":"")+"</span>";}).join("");',
  '}catch(e){}}r();setInterval(r,30000);})();<\/script>',
].join('');

/** 日程卡（复刻手机端的 today-agenda widget）。 */
const PRESET_AGENDA_HTML = [
  '<div style="height:100%;box-sizing:border-box;padding:14px;border-radius:18px;',
  'background:var(--fc-surface,#fff);box-shadow:0 1px 4px rgba(0,0,0,.10);overflow:hidden">',
  '<div style="display:flex;justify-content:space-between;align-items:baseline;margin-bottom:6px">',
  '<span style="font-size:14px;font-weight:600;color:var(--fc-fg,#1b1b1f)">今日日程</span>',
  '<span data-fc="count" style="font-size:11px;color:var(--fc-fg,#6E7681);opacity:.7"></span></div>',
  '<div data-fc="list" style="font-size:13px;color:var(--fc-fg,#1b1b1f)">…</div></div>',
  '<script>(function(){var R=document.currentScript.previousElementSibling;',
  'function q(n){return R.querySelector("[data-fc=\\""+n+"\\"]")}',
  'function r(){try{var d=JSON.parse(window.threees.data("today-agenda","{}"));if(d.error)return;',
  'var it=(d.items||[]).slice(0,4);q("count").textContent=(d.count||0)+" 项";',
  'if(!it.length){q("list").innerHTML="<div style=\\"padding:10px 0;opacity:.6\\">今天没有安排</div>";return}',
  'q("list").innerHTML=it.map(function(x,i){return "<div style=\\"display:flex;gap:8px;align-items:baseline;padding:6px 0;"+',
  '(i?"border-top:1px solid rgba(127,127,127,.14)":"")+"\\">"+"<span style=\\"font-size:11px;opacity:.65;min-width:42px\\">"+',
  '(x.time||"")+"</span><span style=\\"flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap\\">"+(x.title||"")+',
  '"</span><span style=\\"font-size:11px;opacity:.55\\">"+(x.location||"")+"</span></div>";}).join("");',
  '}catch(e){}}r();setInterval(r,30000);})();<\/script>',
].join('');

/** 渐变 hero（复刻 HTML_SHOWCASE 那块）。 */
const PRESET_HERO_HTML = [
  '<div style="height:100%;box-sizing:border-box;display:flex;flex-direction:column;justify-content:center;',
  'padding:0 18px;border-radius:18px;background:linear-gradient(135deg,var(--fc-primary,#2F6FB5),#7A4FD0);',
  'color:var(--fc-on-primary,#fff)">',
  '<div data-fc="wd" style="font-size:12px;opacity:.85;letter-spacing:.06em">今天</div>',
  '<div data-fc="date" style="font-size:28px;font-weight:700;line-height:1.25"></div>',
  '<div style="font-size:12px;opacity:.85;margin-top:2px">这里可以随便画 —— 渐变、圆角、脚本都行</div></div>',
  '<script>(function(){var R=document.currentScript.previousElementSibling;',
  'function q(n){return R.querySelector("[data-fc=\\""+n+"\\"]")}',
  'function r(){try{var n=JSON.parse(window.threees.data("now","{}"));if(n.error)return;',
  'q("wd").textContent=n.weekday||"今天";var p=String(n.date||"").split("-");',
  'if(p.length===3)q("date").textContent=Number(p[1])+"月"+Number(p[2])+"日";}catch(e){}}r();setInterval(r,60000);})();<\/script>',
].join('');

/**
 * **手机端默认布局的复刻版** —— 打开设计台、或某一页没画过时，用它们起手，
 * 而不是给一张空布。
 *
 * `homeLayoutName` 对应手机端 `DefaultHomeLayouts.ALL` 的键，方便两边对上。
 */
const LAYOUT_PRESETS = [
  {
    id: 'simple', name: '简洁', icon: '🌤', homeLayoutName: 'simple',
    desc: '天气 + 今日日程（= 手机端默认那套）',
    build: (W) => [
      { k: 'code', x: 12, y: 12, w: W - 24, h: 132, html: PRESET_WEATHER_HTML },
      { k: 'code', x: 12, y: 154, w: W - 24, h: 250, html: PRESET_AGENDA_HTML },
    ],
  },
  {
    id: 'dashboard', name: '信息密集', icon: '📊', homeLayoutName: 'dashboard',
    desc: '标题 + 天气 + 日程 + 课表（把常用的都摆上）',
    build: (W) => {
      const half = (W - 12 * 2 - 10) / 2;
      return [
        { k: 'text', x: 14, y: 14, w: W - 28, h: 30, text: '今天', size: 20, weight: 600 },
        { k: 'code', x: 12, y: 54, w: half, h: 120, html: PRESET_WEATHER_HTML },
        { k: 'code', x: 12 + half + 10, y: 54, w: half, h: 120, html: PRESET_AGENDA_HTML },
        { k: 'code', x: 12, y: 184, w: W - 24, h: 150, html: PRESET_AGENDA_HTML },
        { k: 'text', x: 14, y: 344, w: W - 28, h: 22, text: '本周课表', size: 15, weight: 600 },
        { k: 'box', x: 12, y: 370, w: W - 24, h: 180, radius: 18, fill: 'var(--fc-surface)', shadow: 1 },
        { k: 'text', x: 28, y: 388, w: W - 56, h: 40, text: '（这里可以拖一块「代码」元素，\\n用 threees.data() 自己画课表）', size: 13, lh: 1.6 },
      ];
    },
  },
  {
    id: 'minimal', name: '极简', icon: '◻️', homeLayoutName: 'minimal',
    desc: '只留一块大的日程卡',
    build: (W) => [
      { k: 'code', x: 16, y: 60, w: W - 32, h: 320, html: PRESET_AGENDA_HTML },
    ],
  },
  {
    id: 'showcase', name: 'HTML 示例', icon: '✨', homeLayoutName: 'html',
    desc: '渐变卡片 + 日程 + 说明（看 HTML 能画到什么程度）',
    build: (W) => [
      { k: 'code', x: 12, y: 12, w: W - 24, h: 150, html: PRESET_HERO_HTML },
      { k: 'code', x: 12, y: 172, w: W - 24, h: 230, html: PRESET_AGENDA_HTML },
      { k: 'text', x: 16, y: 414, w: W - 32, h: 40, text: '这两块都是 HTML：配色和形状不受组件库限制。', size: 13, lh: 1.6 },
    ],
  },
];

/** 日期卡（大日期 + 星期，纯静态样式 + 一点脚本）。 */
const PRESET_DATE_HTML = [
  '<div style="height:100%;box-sizing:border-box;padding:14px;border-radius:18px;',
  'background:var(--fc-surface,#fff);box-shadow:0 1px 4px rgba(0,0,0,.10);display:flex;align-items:center;gap:14px">',
  '<div style="text-align:center;min-width:58px">',
  '<div data-fc="d" style="font-size:34px;font-weight:700;line-height:1;color:var(--fc-primary,#2F6FB5)">—</div>',
  '<div data-fc="m" style="font-size:11px;opacity:.6;margin-top:3px;color:var(--fc-fg,#1b1b1f)">—</div></div>',
  '<div style="flex:1;min-width:0">',
  '<div data-fc="wd" style="font-size:15px;font-weight:600;color:var(--fc-fg,#1b1b1f)">—</div>',
  '<div style="font-size:12px;opacity:.6;margin-top:3px;color:var(--fc-fg,#1b1b1f)">今天也要加油</div></div></div>',
  '<script>(function(){var R=document.currentScript.previousElementSibling;',
  'function q(n){return R.querySelector("[data-fc=\\""+n+"\\"]")}',
  'function r(){try{var n=JSON.parse(window.threees.data("now","{}"));if(n.error)return;',
  'var p=String(n.date||"").split("-");if(p.length!==3)return;',
  'q("d").textContent=Number(p[2]);q("m").textContent=Number(p[1])+"月";q("wd").textContent=n.weekday||"";',
  '}catch(e){}}r();setInterval(r,60000);})();<\/script>',
].join('');

/** 一行四个快捷入口（纯静态，但按钮**真的能绑动作**）。 */
const PRESET_QUICK_HTML = [
  '<div style="height:100%;box-sizing:border-box;display:flex;gap:8px">',
  ['📅|日程|schedule', '📝|随心记|notes', '📖|笔记|mdnote', '⚙️|设置|settings'].map((s) => {
    const a = s.split('|');
    return '<div data-act="page" data-arg="' + a[2] + '" style="flex:1;border-radius:14px;' +
      'background:var(--fc-container,#e6e9ef);color:var(--fc-on-container,#1b1b1f);' +
      'display:flex;flex-direction:column;align-items:center;justify-content:center;gap:5px;cursor:pointer">' +
      '<span style="font-size:19px">' + a[0] + '</span>' +
      '<span style="font-size:11px">' + a[1] + '</span></div>';
  }).join(''),
  '</div>',
].join('');

/** 标题 + 副标题（纯静态，双击改不了 —— 它是 HTML；想双击改就用「文字」元素）。 */
const PRESET_TITLE_HTML = [
  '<div style="height:100%;box-sizing:border-box;display:flex;flex-direction:column;justify-content:center">',
  '<div style="font-size:22px;font-weight:700;color:var(--fc-fg,#1b1b1f)">标题写这里</div>',
  '<div style="font-size:13px;opacity:.65;margin-top:4px;color:var(--fc-fg,#1b1b1f)">副标题 / 一句说明</div></div>',
].join('');

/** 两个并排按钮（**带动作** —— 点了真的会跳页）。 */
const PRESET_BTNS_HTML = [
  '<div style="height:100%;box-sizing:border-box;display:flex;gap:10px">',
  '<div data-act="page" data-arg="schedule" style="flex:1;border-radius:22px;background:var(--fc-primary,#2F6FB5);',
  'color:var(--fc-on-primary,#fff);display:flex;align-items:center;justify-content:center;',
  'font-size:14px;font-weight:600;cursor:pointer">看日程</div>',
  '<div data-act="page" data-arg="notes" style="flex:1;border-radius:22px;background:var(--fc-container,#e6e9ef);',
  'color:var(--fc-on-container,#1b1b1f);display:flex;align-items:center;justify-content:center;',
  'font-size:14px;cursor:pointer">随心记</div></div>',
].join('');

/**
 * **预制块** —— 单个组件，点一下插到画布中间（也可以从元素库那边理解成"高级元件"）。
 *
 * 和上面 `LAYOUT_PRESETS`（整套布局）的分工：
 * ```
 *   默认布局 / 起手模板  →  一次铺一整屏（"我不知道从哪开始"）
 *   预制块              →  往现有画面里补一块（"我就要一张日程卡"）
 * ```
 * 用户原话是「增加更多元的预制按钮」—— 这两组一起才叫"多元"。
 */
const WIDGET_PRESETS = [
  { id: 'agenda', name: '日程卡', icon: '📅', desc: '今日日程（自动取数据）', build: (W) => ({ k: 'code', w: W - 24, h: 250, html: PRESET_AGENDA_HTML }) },
  { id: 'weather', name: '天气卡', icon: '🌤', desc: '天气 + 逐时（自动取数据）', build: (W) => ({ k: 'code', w: W - 24, h: 132, html: PRESET_WEATHER_HTML }) },
  { id: 'date', name: '日期卡', icon: '📆', desc: '大日期 + 星期', build: (W) => ({ k: 'code', w: W - 24, h: 110, html: PRESET_DATE_HTML }) },
  { id: 'quick', name: '快捷入口', icon: '⚡', desc: '一行四个入口（带跳页动作）', build: (W) => ({ k: 'code', w: W - 24, h: 88, html: PRESET_QUICK_HTML }) },
  { id: 'title', name: '标题组', icon: '🅃', desc: '标题 + 副标题', build: (W) => ({ k: 'code', w: W - 24, h: 76, html: PRESET_TITLE_HTML }) },
  { id: 'btns', name: '按钮组', icon: '🔘', desc: '两个并排按钮（带跳页动作）', build: (W) => ({ k: 'code', w: W - 24, h: 56, html: PRESET_BTNS_HTML }) },
];

// ============================================================ §11 元素库

/** 左栏「拖进画布」那一块。返回节点（skin-studio 自己决定放哪）。 */
function renderLib(box, ed) {
  box.innerHTML = '';

  // ---- 从已有的外观包导入（放在最上面：它是"接着上次改"的入口，不是"从零开始"） ----
  if (ed.opts && ed.opts.onImport) {
    const imp = document.createElement('button');
    imp.className = 'fc-widebtn fc-importbtn';
    imp.textContent = '📥 从外观包导入（按授权码）';
    imp.title = '把已经发布到服务器的外观包拉下来接着改 —— 不用从零重做';
    imp.onclick = () => ed.opts.onImport();
    box.appendChild(imp);
  }

  const grid = document.createElement('div');
  grid.className = 'fc-libgrid';
  KINDS.forEach((k) => {
    const b = document.createElement('button');
    b.className = 'fc-libitem';
    b.innerHTML = '<span class="fc-libicon">' + LIB_ICON[k] + '</span><span>' + KIND_LABEL[k] + '</span>';
    b.title = '点一下加进画布（也可以直接拖到画布上的位置）';
    b.onclick = () => ed.addEl(k);
    // 拖到画布上 → 落在鼠标那个位置
    b.draggable = true;
    b.addEventListener('dragstart', (ev) => { ev.dataTransfer.setData('text/fc-kind', k); });
    grid.appendChild(b);
  });
  box.appendChild(grid);

  // ---- 预制块：单个组件，点一下插到画布中间（"我就要一张日程卡"） ----
  const wHead = document.createElement('div');
  wHead.className = 'fc-tplhead';
  wHead.style.marginTop = '14px';
  wHead.textContent = '预制块';
  box.appendChild(wHead);
  const wGrid = document.createElement('div');
  wGrid.className = 'fc-libgrid';
  WIDGET_PRESETS.forEach((w) => {
    const b = document.createElement('button');
    b.className = 'fc-libitem';
    b.innerHTML = '<span class="fc-libicon">' + w.icon + '</span><span>' + w.name + '</span>';
    b.title = w.desc + '（插到画布中间，再拖到想要的位置）';
    b.onclick = () => {
      const raw = w.build(ed.layer.w);
      const e = createEl(raw.k, raw);
      e.x = Math.max(8, Math.round((ed.layer.w - e.w) / 2));
      // ★ 插到**内容区**的靠上位置（不是整屏的 1/4 —— 那会落在顶栏上）
      const ct = ed.contentTop ? ed.contentTop() : 0;
      e.y = ct + Math.round((ed.layer.h - ct) / 5);
      ed.commit('加「' + w.name + '」', () => { ed.layer.els.push(e); ed.sel = [e.id]; });
      ed.fireSel();
    };
    wGrid.appendChild(b);
  });
  box.appendChild(wGrid);

  // ---- 两组建手：**手机端默认布局**（复刻）+ 起手模板 ----
  //
  // ⚠️ 顺序是有意的：「手机端默认布局」放前面 —— 用户说"你不能给一张空布"，
  //    而这几套**就是手机端原本长什么样**，是最自然的起点。
  const tplGroup = (title, list) => {
    const g = document.createElement('div');
    g.className = 'fc-tpls';
    const h = document.createElement('div');
    h.className = 'fc-tplhead';
    h.textContent = title;
    g.appendChild(h);
    list.forEach((t) => {
      const b = document.createElement('button');
      b.className = 'fc-tpl';
      b.innerHTML = '<span class="fc-tplicon">' + t.icon + '</span>' +
        '<span class="fc-tpltext"><b>' + t.name + '</b><i>' + t.desc + '</i></span>';
      b.onclick = () => {
        const has = ed.layer.els.length > 0 || (ed.layer.code || '').trim();
        if (has && !confirm('这会**替换**这一页画布上现有的东西（可以 Ctrl+Z 撤销）。继续？')) return;
        ed.useTemplate(t);
      };
      g.appendChild(b);
    });
    box.appendChild(g);
  };
  // ⚠️ 这里**没有**"手机端默认布局"那一组 —— 那是 2026-10-15 我自己写的 HTML 复刻，
  //    冒充"手机端默认的样子"，被用户当场否掉（"你都会瞎创作什么"）。
  //    手机端默认的样子由**画布底下的源码渲染层**（shell-render = 手机端原本的代码）画。
  //    下面这些是**明确标注为模板**的东西 —— 用户主动想加时才点。
  tplGroup('起手模板', TEMPLATES);

  const note = document.createElement('div');
  note.className = 'fc-note';
  note.innerHTML = '点一下加进画布，或者直接拖到想要的位置。<br>' +
    '<b>双击文字</b>就地改；<b>拖角上的方块</b>缩放；<b>上面那个圆点</b>旋转；' +
    '按住 <b>Shift</b> 多选，空白处<b>拖框</b>框选。';
  box.appendChild(note);

  const file = document.createElement('input');
  file.type = 'file'; file.accept = 'image/*'; file.style.display = 'none';
  file.onchange = () => {
    const f = file.files && file.files[0];
    if (!f) return;
    // ⚠️ 手机端 WebView **禁网**，图片只能内联。所以这里直接转成 data: URI 塞进产物。
    //    代价是包会变大（一张 200KB 的图 → 约 270KB 的 base64），所以给个体积提示。
    const rd = new FileReader();
    rd.onload = () => {
      const src = String(rd.result || '');
      if (src.length > 700 * 1024 && !confirm('这张图内联后有 ' + Math.round(src.length / 1024) + ' KB，' +
        '会让外观包明显变大（手机端不允许加载网络图片，只能内联）。继续吗？')) return;
      ed.commit('加图片', () => {
        const e = createEl('image', { src });
        e.x = Math.max(8, Math.round((ed.layer.w - e.w) / 2));
        e.y = Math.round(ed.layer.h / 4);
        ed.layer.els.push(e);
        ed.sel = [e.id];
      });
      ed.render(); ed.fireSel();
    };
    rd.readAsDataURL(f);
    file.value = '';
  };
  box.appendChild(file);
  const up = document.createElement('button');
  up.className = 'fc-widebtn';
  up.textContent = '⬆ 上传图片（内联进包）';
  up.onclick = () => file.click();
  box.appendChild(up);

  // 画布拖放落点
  const page = ed.el.page;
  if (!page.dataset.fcDrop) {
    page.dataset.fcDrop = '1';
    page.addEventListener('dragover', (ev) => { ev.preventDefault(); });
    page.addEventListener('drop', (ev) => {
      ev.preventDefault();
      const k = ev.dataTransfer.getData('text/fc-kind');
      if (!k || !KINDS.includes(k)) return;
      const p = ed.pt(ev);
      const e = ed.addEl(k, p);
      if (e) { ed.commit('摆位置', () => { e.x = Math.max(0, Math.round(p.x - e.w / 2)); e.y = Math.max(0, Math.round(p.y - e.h / 2)); }); }
    });
  }
  return box;
}

const LIB_ICON = {
  text: 'T', box: '▭', ellipse: '◯', line: '╱', image: '🖼',
  icon: '★', button: '🔘', draw: '✎', code: '⌨',
};

// ============================================================ §12 属性面板

/**
 * 右栏属性。跟着选中对象走。
 * @param box 容器
 * @param ed  编辑器
 * @param cb  { onOpenCode, onOpenPreview }
 */
function renderInspector(box, ed, cb) {
  box.innerHTML = '';
  const o = cb || {};
  const es = ed.selEls();

  // ---- 没选中 → 画布自己的属性 ----
  if (!es.length) {
    const s1 = fsec('画布', false);
    s1.body.appendChild(frow('宽度', fnum(ed.layer.w, 120, 1200, 1, (v) => ed.resize(v, ed.layer.h)).node, 'px（= 手机 dp）'));
    s1.body.appendChild(frow('高度', fnum(ed.layer.h, 80, 3000, 1, (v) => ed.resize(ed.layer.w, v)).node, 'px（= 手机 dp）'));
    s1.body.appendChild(frow('底色', fcolor(ed.layer.bg, (v) => ed.commit('画布底色', () => { ed.layer.bg = v; }), true).node));
    s1.body.appendChild(frow('元素数', (() => { const d = document.createElement('div'); d.className = 'fc-dim'; d.textContent = ed.layer.els.length + ' 个'; return d; })()));
    box.appendChild(s1.node);

    const s2 = fsec('这一页怎么用', false);
    s2.body.appendChild(noteBlock(
      '<b>画布底下是手机端的真实渲染</b> —— 手机端同一份代码画的，' +
      '所以你看到的和真机是一个样。<br><br>' +
      '· <b>你加的东西落在哪</b>：虚线框内就是能放东西的地方 —— ' +
      '主页是「编辑」那一行<b>下面</b>，其他页面是子标签行<b>下面</b>。' +
      '放到框外会被顶栏/底栏盖住。<br>' +
      '· 每个元素都能<b>绑动作</b>（跳页 / 执行功能 / 打开网址 / 自定义脚本）—— 选中后在下面的「动作」里设。<br>' +
      '· 想写代码就用「代码」元素，或点工具条的 <b>⌨ 代码</b> 整页手写。<br>' +
      '· 图片<b>只能内联</b>（手机端禁网），左下角那个按钮上传。<br>' +
      '· 想清空重来：工具条的 <b>🗑</b>；<b>↺</b> = 回到手机端默认的样子。',
    ));
    box.appendChild(s2.node);

    const s2b = fsec('不知道从哪开始？', false);
    s2b.body.appendChild(noteBlock(
      '左栏下面有 <b>4 个起手模板</b>（标题+按钮 / 九宫格 / 一张卡片 / 大图+标题）—— ' +
      '点一下就把一屏东西铺好，再在上面改。<br>' +
      '或者直接从元素库里点一个「文字」拖到中间。',
    ));
    box.appendChild(s2b.node);

    const s3 = fsec('这一页', false);
    s3.body.appendChild(fbtns([
      { label: '⌨ 看代码', fn: () => o.onOpenCode && o.onOpenCode() },
      { label: '👁 预览', fn: () => o.onOpenPreview && o.onOpenPreview() },
      { label: '清空', danger: true, fn: () => { if (confirm('清空这一页的画布？')) ed.commit('清空', () => { ed.layer.els = []; }); } },
    ]).node);
    box.appendChild(s3.node);
    return box;
  }

  // ---- 多选 ----
  if (es.length > 1) {
    const s = fsec('已选 ' + es.length + ' 个', false);
    s.body.appendChild(fbtns([
      { label: '左', fn: () => ed.align('left') }, { label: '中', fn: () => ed.align('hcenter') }, { label: '右', fn: () => ed.align('right') },
    ]).node);
    s.body.appendChild(fbtns([
      { label: '上', fn: () => ed.align('top') }, { label: '中', fn: () => ed.align('vcenter') }, { label: '下', fn: () => ed.align('bottom') },
    ]).node);
    s.body.appendChild(fbtns([
      { label: '置顶', fn: () => ed.zorder('top') }, { label: '置底', fn: () => ed.zorder('bottom') },
      { label: '复制', fn: () => ed.dupSel() }, { label: '删除', danger: true, fn: () => ed.delSel() },
    ]).node);
    const n = document.createElement('div');
    n.className = 'fc-note';
    n.textContent = '多选时对齐按整体包围盒算；想逐个改属性请只选一个。';
    s.body.appendChild(n);
    box.appendChild(s.node);
    return box;
  }

  // ---- 单选 ----
  const e = es[0];
  const upd = (label, key, cast) => (v) => ed.live(label, () => { e[key] = cast ? cast(v) : v; });

  const head = fsec(KIND_LABEL[e.k] + (e.name ? '·' + e.name : ''), false);
  head.body.appendChild(frow('名字', ftext(e.name, upd('改名', 'name'), '（留空用默认）').node));
  head.body.appendChild(fbtns([
    { label: e.lock ? '🔒 已锁' : '🔓 锁定', fn: () => { ed.commit('锁定', () => { e.lock = !e.lock; }); ed.fireSel(); } },
    { label: e.hide ? '👁 已藏' : '👁 隐藏', fn: () => { ed.commit('隐藏', () => { e.hide = !e.hide; }); ed.fireSel(); } },
    { label: '复制', fn: () => ed.dupSel() },
    { label: '删除', danger: true, fn: () => ed.delSel() },
  ]).node);
  box.appendChild(head.node);

  // 几何
  const geo = fsec('位置与大小', false);
  const nx = fnum(e.x, undefined, undefined, 1, upd('位置', 'x'));
  const ny = fnum(e.y, undefined, undefined, 1, upd('位置', 'y'));
  const nw = fnum(e.w, 4, 4000, 1, upd('大小', 'w'));
  const nh = fnum(e.h, 4, 4000, 1, upd('大小', 'h'));
  const two = (a, b, c, d) => {
    const r = document.createElement('div'); r.className = 'fc-two';
    r.appendChild(frow(a, b.node)); r.appendChild(frow(c, d.node)); return r;
  };
  geo.body.appendChild(two('X', nx, 'Y', ny));
  geo.body.appendChild(two('宽', nw, '高', nh));
  geo.body.appendChild(frow('旋转', fnum(e.rot, -180, 180, 1, upd('旋转', 'rot'), '°').node));
  geo.body.appendChild(frow('不透明', fnum(Math.round(e.op * 100), 0, 100, 1, (v) => ed.live('不透明度', () => { e.op = v / 100; }), '%').node));
  geo.body.appendChild(fbtns([
    { label: '水平居中', fn: () => { ed.commit('居中', () => { e.x = Math.round((ed.layer.w - e.w) / 2); }); ed.fireSel(); } },
    { label: '垂直居中', fn: () => { ed.commit('居中', () => { e.y = Math.round((ed.layer.h - e.h) / 2); }); ed.fireSel(); } },
  ]).node);
  box.appendChild(geo.node);
  // ⚠️ 拖动元素时同步这些输入框（不重建面板 —— 重建会让正在编辑的框失焦）
  ed._geomSync = () => { nx.set(e.x); ny.set(e.y); nw.set(e.w); nh.set(e.h); };

  // 类型专属
  if (e.k === 'text' || e.k === 'button') {
    const t = fsec('文字', false);
    t.body.appendChild(ftext(e.text, upd('改文字', 'text'), '写点什么', 3).node);
    t.body.appendChild(frow('字号', fnum(e.size, 6, 200, 1, upd('字号', 'size')).node));
    t.body.appendChild(frow('粗细', fselect(String(e.weight), [
      { v: '300', label: '细' }, { v: '400', label: '正常' }, { v: '500', label: '中' }, { v: '600', label: '半粗' }, { v: '700', label: '粗' },
    ], (v) => ed.live('粗细', () => { e.weight = Number(v); })).node));
    t.body.appendChild(frow('颜色', fcolor(e.color, (v) => ed.live('文字色', () => { e.color = v; }), true).node, '留空 = 跟随外观'));
    t.body.appendChild(frow('对齐', fselect(e.align, [
      { v: 'left', label: '左' }, { v: 'center', label: '中' }, { v: 'right', label: '右' },
    ], (v) => ed.live('对齐', () => { e.align = v; })).node));
    t.body.appendChild(frow('行高', fnum(e.lh, 0.6, 3, 0.05, upd('行高', 'lh')).node));
    t.body.appendChild(frow('字距', fnum(e.ls, -5, 20, 0.5, upd('字距', 'ls'), 'px').node));
    t.body.appendChild(fbtns([
      { label: '斜体', fn: () => { ed.commit('斜体', () => { e.italic = !e.italic; }); ed.fireSel(); } },
      { label: '下划线', fn: () => { ed.commit('下划线', () => { e.underline = !e.underline; }); ed.fireSel(); } },
    ]).node);
    box.appendChild(t.node);
  }

  if (e.k === 'box' || e.k === 'ellipse' || e.k === 'button' || e.k === 'icon') {
    const f = fsec('填充与描边', false);
    f.body.appendChild(frow('填充', fcolor(e.fill, (v) => ed.live('填充', () => { e.fill = v; }), true).node, '留空 = 跟随外观主色'));
    f.body.appendChild(frow('描边', fcolor(e.stroke, (v) => ed.live('描边', () => { e.stroke = v; }), true).node, '留空 = 不描边'));
    f.body.appendChild(frow('描边宽', fnum(e.strokeW, 0, 40, 1, upd('描边宽', 'strokeW'), 'px').node));
    if (e.k === 'box' || e.k === 'button') {
      f.body.appendChild(frow('圆角', fnum(e.radius, 0, 200, 1, upd('圆角', 'radius'), 'px').node));
    }
    f.body.appendChild(frow('阴影', fnum(e.shadow, 0, 5, 1, upd('阴影', 'shadow')).node));
    box.appendChild(f.node);
  }

  if (e.k === 'line' || e.k === 'draw') {
    const f = fsec('线条', false);
    f.body.appendChild(frow('颜色', fcolor(e.stroke, (v) => ed.live('线条色', () => { e.stroke = v; }), true).node));
    f.body.appendChild(frow('粗细', fnum(e.strokeW, 1, 40, 1, upd('粗细', 'strokeW'), 'px').node));
    if (e.k === 'draw') f.body.appendChild(noteBlock('手绘的路径已经存好了。想重画就把这块删掉、用工具条的 ✎ 重新画。'));
    box.appendChild(f.node);
  }

  if (e.k === 'image') {
    const f = fsec('图片', false);
    f.body.appendChild(noteBlock(e.src ? '已内联（' + Math.round(e.src.length / 1024) + ' KB）' : '还没有图 —— 用左下角「上传图片」'));
    f.body.appendChild(frow('裁剪', fselect(e.fit, [
      { v: 'cover', label: '填满（裁掉多余）' }, { v: 'contain', label: '完整显示（留边）' }, { v: 'fill', label: '拉伸铺满' },
    ], (v) => ed.live('裁剪', () => { e.fit = v; })).node));
    f.body.appendChild(frow('圆角', fnum(e.radius, 0, 200, 1, upd('圆角', 'radius'), 'px').node));
    f.body.appendChild(fbtns([
      { label: '换成别的图', fn: () => { const inp = document.querySelector('.fc-libgrid') && document.querySelector('input[type=file]'); if (inp) { inp.dataset.target = e.id; inp.click(); } } },
      { label: '清空图片', danger: true, fn: () => ed.commit('清空图片', () => { e.src = ''; }) },
    ]).node);
    box.appendChild(f.node);
  }

  if (e.k === 'icon') {
    const f = fsec('图标', false);
    const grid = document.createElement('div');
    grid.className = 'fc-icons';
    Object.keys(ICONS).forEach((k) => {
      const b = document.createElement('button');
      b.className = 'fc-iconbtn' + (e.icon === k ? ' on' : '');
      b.innerHTML = '<svg viewBox="0 0 24 24" fill="currentColor" width="20" height="20">' + ICONS[k] + '</svg>';
      b.title = k;
      b.onclick = () => { ed.commit('换图标', () => { e.icon = k; }); ed.fireSel(); };
      grid.appendChild(b);
    });
    f.body.appendChild(grid);
    box.appendChild(f.node);
  }

  if (e.k === 'code') {
    const f = fsec('代码', false);
    f.body.appendChild(noteBlock('这一块里可以写任意 HTML / CSS / SVG。它会被原样放进产物。<br>' +
      '<b>不能联网、不能读文件</b>；想拿数据用 <code>window.threees.data(\'today-agenda\')</code>。'));
    f.body.appendChild(ftext(e.html, upd('改代码', 'html'), '<div>…</div>', 10).node);
    box.appendChild(f.node);
  }

  // 动作
  const a = fsec('动作（点一下干什么）', false);
  const cur = ACTION_KINDS.find((x) => x.v === e.act) || ACTION_KINDS[0];
  a.body.appendChild(frow('类型', fselect(e.act, ACTION_KINDS.map((x) => ({ v: x.v, label: x.label })), (v) => {
    ed.commit('绑动作', () => { e.act = v; e.arg = (ACTION_KINDS.find((x) => x.v === v) || {}).arg || ''; });
    ed.fireSel();
  }).node));
  if (e.act) {
    a.body.appendChild(frow('参数', ftext(e.arg, upd('动作参数', 'arg'), cur.hint || '', e.act === 'js' ? 4 : 0).node, cur.hint));
    if (e.act === 'app') {
      a.body.appendChild(fbtns([
        { label: '打开设置', fn: () => { ed.commit('绑动作', () => { e.arg = 'open_settings'; }); ed.fireSel(); } },
        { label: '编辑主页', fn: () => { ed.commit('绑动作', () => { e.arg = 'edit_home'; }); ed.fireSel(); } },
        { label: '新建日程', fn: () => { ed.commit('绑动作', () => { e.arg = 'new_event'; }); ed.fireSel(); } },
      ]).node);
    }
    if (e.act === 'page') {
      a.body.appendChild(fbtns([
        { label: '日程', fn: () => { ed.commit('绑动作', () => { e.arg = 'schedule'; }); ed.fireSel(); } },
        { label: '笔记', fn: () => { ed.commit('绑动作', () => { e.arg = 'mdnote'; }); ed.fireSel(); } },
        { label: '随心记', fn: () => { ed.commit('绑动作', () => { e.arg = 'notes'; }); ed.fireSel(); } },
        { label: '设置', fn: () => { ed.commit('绑动作', () => { e.arg = 'settings'; }); ed.fireSel(); } },
      ]).node);
    }
    a.body.appendChild(noteBlock('动作在<b>手机端</b>才真的执行；设计台里点「👁 预览」可以看到它被触发。'));
  }
  box.appendChild(a.node);
  return box;
}

function noteBlock(html) {
  const d = document.createElement('div');
  d.className = 'fc-note';
  d.innerHTML = html;
  return d;
}

// ============================================================ §13 图层列表

/** 左栏下半「图层」：从上到下 = 从顶层到最底（和画面直觉一致，和数组顺序相反）。 */
function renderLayers(box, ed) {
  box.innerHTML = '';
  const list = ed.layer.els.slice().reverse();
  if (!list.length) {
    const n = document.createElement('div');
    n.className = 'fc-note';
    n.textContent = '这一页还是空的。从上面的元素库里点一个加进来。';
    box.appendChild(n);
    return box;
  }
  list.forEach((e) => {
    const r = document.createElement('div');
    r.className = 'fc-layer' + (ed.sel.includes(e.id) ? ' on' : '');
    const ic = document.createElement('span'); ic.className = 'fc-licon'; ic.textContent = LIB_ICON[e.k] || '·';
    const nm = document.createElement('span'); nm.className = 'fc-lname'; nm.textContent = elLabel(e);
    r.appendChild(ic); r.appendChild(nm);
    const mk = (txt, title, fn, cls) => {
      const b = document.createElement('button');
      b.className = 'fc-lbtn' + (cls ? ' ' + cls : ''); b.textContent = txt; b.title = title;
      b.onclick = (ev) => { ev.stopPropagation(); fn(); };
      r.appendChild(b);
    };
    mk(e.hide ? '◌' : '◉', e.hide ? '显示' : '隐藏', () => { ed.commit('显示/隐藏', () => { e.hide = !e.hide; }); ed.fireSel(); });
    mk(e.lock ? '🔒' : '🔓', e.lock ? '解锁' : '锁定', () => { ed.commit('锁定', () => { e.lock = !e.lock; }); ed.fireSel(); });
    mk('✕', '删除', () => { ed.sel = [e.id]; ed.delSel(); }, 'danger');
    r.onclick = () => { ed.sel = [e.id]; ed.render(); ed.fireSel(); };
    box.appendChild(r);
  });
  return box;
}

// ============================================================ §14 预览

/**
 * 用 iframe 按"真机那套"跑一遍：产物 + 一个假的 `window.threees`，
 * 动作真的会触发（只是打印出来，不会真的跳页）。
 */
function preview(host, ed, geom) {
  host.innerHTML = '';
  const g = geom || {};
  const html = compile(ed.layer, Object.assign({ colors: ed.colors }, g));
  // 预览里的桥 = **和画布同一份示例数据**（`MOCK_DATA`）。
  // ⚠️ 原来是 `data:function(){return "{}"}` —— 于是"预览"里 HTML 块也是空的，
  //    而"预览"恰恰是用户用来判断"做出来长什么样"的地方，空的就等于没预览。
  const mockJson = JSON.stringify(MOCK_DATA).replace(/</g, '\\u003c');
  const fake = '<script>window.__MOCK=' + mockJson + ';' +
    'window.threees={' +
    'data:function(n,a){var d=window.__MOCK[n];if(!d)return "{}";' +
    'var o=JSON.parse(JSON.stringify(d));' +
    'try{var g=JSON.parse(a||"{}");if(g.limit>0&&o.items)o.items=o.items.slice(0,g.limit)}catch(e){}' +
    'return JSON.stringify(o);},' +
    'names:function(){return Object.keys(window.__MOCK).join(",");},' +
    'act:function(k,a){var d=document.createElement("div");d.className="fc-pvtoast";' +
    'd.textContent="动作 → "+k+(a?(" : "+a):"");document.body.appendChild(d);' +
    'setTimeout(function(){d.remove();},2200);return "{}";}};<\/script>';
  const doc = '<!doctype html><html><head><meta charset="utf-8">' +
    '<meta name="viewport" content="width=device-width,initial-scale=1">' +
    '<style>html,body{margin:0;background:transparent}' +
    '.fc-pvtoast{position:fixed;left:8px;right:8px;bottom:10px;background:rgba(20,22,28,.92);color:#fff;' +
    'font:12px/1.4 system-ui;padding:8px 12px;border-radius:10px;z-index:9999}</style></head><body>' +
    html + fake + '</body></html>';
  const f = document.createElement('iframe');
  f.className = 'fc-pvframe';
  f.style.cssText = 'width:' + ed.layer.w + 'px;height:' + ed.layer.h + 'px;border:0;background:#fff;border-radius:14px';
  f.srcdoc = doc;
  host.appendChild(f);
  return html;
}

// ============================================================ 导出

window.FreeCanvas = {
  KINDS: KINDS, KIND_LABEL: KIND_LABEL, ACTION_KINDS: ACTION_KINDS, ICONS: ICONS, SWATCHES: SWATCHES,
  TEMPLATES: TEMPLATES,
  LAYOUT_PRESETS: LAYOUT_PRESETS,
  WIDGET_PRESETS: WIDGET_PRESETS,
  MOCK_DATA: MOCK_DATA, installMockBridge: installMockBridge,
  DEFAULT_W: DEFAULT_W, DEFAULT_H: DEFAULT_H,
  SHELL_TOP: SHELL_TOP, SHELL_BOTTOM: SHELL_BOTTOM,
  createEl: createEl, normalizeEl: normalizeEl, normalizeLayer: normalizeLayer,
  cloneEl: cloneEl, elLabel: elLabel, uid: uid, elHtml: elHtml,
  compile: compile, mount: mount, esc: esc, isHex: isHex,
  renderLib: renderLib, renderInspector: renderInspector, renderLayers: renderLayers,
  preview: preview, noteBlock: noteBlock,
  frow: frow, fnum: fnum, ftext: ftext, fcolor: fcolor, fselect: fselect, fcheck: fcheck, fbtns: fbtns, fsec: fsec,
};

})();   // ← 收 IIFE（见文件头那段：不这样做会和 skin-studio.js 抢全局名字，整个页面打不开）
