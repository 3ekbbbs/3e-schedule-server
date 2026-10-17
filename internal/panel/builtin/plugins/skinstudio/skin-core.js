'use strict';
/*
 * 外观设计台（skin-studio）—— 手机端界面外观的**设计 + 打包 + 发布**工具。
 * 2026-10-14 建。
 *
 * ## 为什么是"零依赖 + 不构建"
 *
 * 和 `nav-studio.js` 同一条纪律：一个 .html + 一个 .js + 一个 .css，静态伺服即可
 * （`web/main.go` 直接伺服 `public/`），线上 `https://threees.cn:8444/skin-studio.html`，
 * 本地双击 `file://` 也能开。设计台坏掉不该影响任何人用 App，所以它连 npm 都不要。
 *
 * ## ★★ 和手机端"不要有差异"是怎么做到的
 *
 * **不是**把界面画得像，而是**同一套算法**。下面这些函数是从手机端逐行搬过来的，
 * 改了任何一边都必须改另一边（`tmp/verify-skin-studio.cjs` 会拿手机端的期望值回归）：
 *
 * | 这里的函数 | 手机端的来源 | 干什么 |
 * |---|---|---|
 * | `okToLab` / `labToOk` / `lightness` / `withLightness` | `ui/Palette.kt:309-372` | OKLab 明度换算（含**越界就降彩度**那个坑） |
 * | `themeFrom` / `DerivedTheme` | `ui/Palette.kt:226-296`、`:375-447` | 种子色 → 整套配色（含彩度分级 0.10/0.14/0.85） |
 * | `readableOn` / `searchTinted` | `ui/Palette.kt:179-224` | "保证读得清"的前景字色 |
 * | `contrast` / `luminance` / `linear` | `ui/Palette.kt:90-107` | WCAG 对比度 |
 * | `SKIN_ROLES` / `DECORS` | `ui/UiSkin.kt:359-379` | 可指定的 27 个配色角色、7 种纹理 |
 * | `decorPaint` | `ui/DecorLayer.kt:39-172` | 背景纹理（**固定 6% 不透明度**、固定网格） |
 *
 * ⚠️ 三处**不能漂移**的常量（漂了的话设计台看到的颜色和手机不一样）：
 *    `BG_CHROMA=0.10` / `SURFACE_CHROMA=0.14` / `PRIMARY_CHROMA=0.85` / `OUTLINE_CHROMA=0.10`，
 *    以及浅色主题的背景明度 `0.97`、表面 `0.97-0.035`、深色 `0.16 / 0.22`。
 */

// ============================================================ 一、颜色（移植自 Palette.kt）

/** sRGB → 线性。⚠️ 必须先线性化：直接拿 sRGB 值加权会**高估深色区的对比度**。 */
function linear(v) { return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }
function gammaC(v) { return v <= 0.0031308 ? 12.92 * v : 1.055 * Math.pow(v, 1 / 2.4) - 0.055; }

/** WCAG 相对亮度。 */
function luminance(c) {
  return 0.2126 * linear(c.r / 255) + 0.7152 * linear(c.g / 255) + 0.0722 * linear(c.b / 255);
}
/** WCAG 对比度（1.0~21.0）。 */
function contrast(a, b) {
  const la = luminance(a), lb = luminance(b);
  const hi = Math.max(la, lb), lo = Math.min(la, lb);
  return (hi + 0.05) / (lo + 0.05);
}
/** sRGB → OKLab。矩阵常量一个都不能改。 */
function toOklab(r, g, b) {
  const lr = linear(r), lg = linear(g), lb = linear(b);
  const l = 0.4122214708 * lr + 0.5363325363 * lg + 0.0514459929 * lb;
  const m = 0.2119034982 * lr + 0.6806995451 * lg + 0.1073969566 * lb;
  const s = 0.0883024619 * lr + 0.2817188376 * lg + 0.6299787005 * lb;
  const l_ = Math.cbrt(l), m_ = Math.cbrt(m), s_ = Math.cbrt(s);
  return [
    0.2104542553 * l_ + 0.7936177850 * m_ - 0.0040720468 * s_,
    1.9779984951 * l_ - 2.4285922050 * m_ + 0.4505937099 * s_,
    0.0259040371 * l_ + 0.7827717662 * m_ - 0.8086757660 * s_,
  ];
}
/**
 * OKLab → sRGB。
 * @returns {[object, boolean]} `[颜色, 是否越界]` —— **必须把越界报出来**，
 *   调用方要靠它决定降彩度；静默 clamp 的症状是"色相悄悄偏了"（蓝变青）。
 */
function fromOklab(L, a, b) {
  const l_ = L + 0.3963377774 * a + 0.2158037573 * b;
  const m_ = L - 0.1055613458 * a - 0.0638541728 * b;
  const s_ = L - 0.0894841775 * a - 1.2914855480 * b;
  const l = l_ * l_ * l_, m = m_ * m_ * m_, s = s_ * s_ * s_;
  const lr = +4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s;
  const lg = -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s;
  const lb = -0.0041960863 * l - 0.7034186147 * m + 1.7076147010 * s;
  const out = lr < -0.0001 || lr > 1.0001 || lg < -0.0001 || lg > 1.0001 || lb < -0.0001 || lb > 1.0001;
  return [
    {
      r: Math.round(Math.min(1, Math.max(0, gammaC(lr))) * 255),
      g: Math.round(Math.min(1, Math.max(0, gammaC(lg))) * 255),
      b: Math.round(Math.min(1, Math.max(0, gammaC(lb))) * 255),
    },
    out,
  ];
}
function lightness(c) { const lab = toOklab(c.r / 255, c.g / 255, c.b / 255); return lab[0]; }

/**
 * 保留色相、改明度（+ 可选降彩度）。
 * ⚠️ 越界就**继续退让**直到不越界 —— 只把 L 挪走、a/b 原样带过去会得到超色域的颜色，
 *   截断之后色相是偏的（中蓝推到 L=0.97 会变成亮青）。
 */
function withLightness(c, l, chroma = 1.0) {
  const lab = toOklab(c.r / 255, c.g / 255, c.b / 255);
  const targetL = Math.min(1, Math.max(0, l));
  let scale = Math.min(1, Math.max(0, chroma));
  let r = fromOklab(targetL, lab[1] * scale, lab[2] * scale);
  let guard = 0;
  while (r[1] && guard++ < 14) { scale *= 0.85; r = fromOklab(targetL, lab[1] * scale, lab[2] * scale); }
  return r[0];
}

// ---- 常量（必须和 Palette.kt 一致）----
const RATIO_TEXT = 4.5;
const RATIO_LARGE = 3.0;
const BG_CHROMA = 0.10;
const SURFACE_CHROMA = 0.14;
const OUTLINE_CHROMA = 0.10;
const PRIMARY_CHROMA = 0.85;
const DEFAULT_SEED = { r: 0x2F, g: 0x6F, b: 0xB5 };

/** 同色系里找一个达标的前景：从背景明度出发逐步走，第一个达标的就返回。 */
function searchTinted(bg, towardsDark, minRatio) {
  let l = lightness(bg);
  const step = towardsDark ? -0.02 : 0.02;
  let guard = 0;
  while (l >= 0.02 && l <= 0.98 && guard++ < 60) {
    l += step;
    const c = withLightness(bg, l);
    if (contrast(bg, c) >= minRatio) return c;
  }
  return null;
}
/** 给背景挑一个保证读得清的前景（同色系优先，兜底纯白/纯黑）。 */
function readableOn(bg, minRatio = RATIO_TEXT) {
  const towardsDark = lightness(bg) > 0.5;
  const tinted = searchTinted(bg, towardsDark, minRatio);
  if (tinted) return tinted;
  const white = { r: 255, g: 255, b: 255 }, black = { r: 0, g: 0, b: 0 };
  return contrast(bg, white) >= contrast(bg, black) ? white : black;
}

/** 从一个种子色推导整套配色（= `Palette.themeFrom`）。 */
function themeFrom(seed, dark) {
  const bgL = dark ? 0.16 : 0.97;
  const bg = withLightness(seed, bgL, BG_CHROMA);
  const surface = withLightness(seed, dark ? bgL + 0.06 : bgL - 0.035, SURFACE_CHROMA);
  const onBg = readableOn(bg, RATIO_TEXT);
  const onSurface = readableOn(surface, RATIO_TEXT);
  const muted = readableOn(bg, RATIO_LARGE);
  const primary = withLightness(seed, dark ? 0.74 : 0.45, PRIMARY_CHROMA);
  const onPrimary = readableOn(primary, RATIO_TEXT);
  const outline = withLightness(surface, dark ? bgL + 0.22 : bgL - 0.18, OUTLINE_CHROMA);
  return { background: bg, surface, onBackground: onBg, onSurface, muted, primary, onPrimary, outline, dark };
}

/** 容器色 = primary 的更淡/更深一档（**同一色相**，所以"选中"看起来是同一颜色的浅版本）。 */
function containerOf(primary, dark) { return withLightness(primary, dark ? 0.30 : 0.90); }

// ============================================================ 二、皮肤模型（移植自 UiSkin.kt）

/** 可指定的配色角色（白名单，认不出的**忽略**）。顺序 = 面板里的展示顺序。 */
const SKIN_ROLES = [
  ['background', '页面底'], ['onBackground', '页面字'],
  ['surface', '表面'], ['onSurface', '表面字'], ['surfaceVariant', '次表面'], ['onSurfaceVariant', '次字'],
  ['surfaceContainerLowest', '卡片·最浅'], ['surfaceContainerLow', '卡片（**默认档**）'],
  ['surfaceContainer', '卡片·中'], ['surfaceContainerHigh', '卡片·高'], ['surfaceContainerHighest', '卡片·最高'],
  ['primary', '主色'], ['onPrimary', '主色上的字'], ['primaryContainer', '主色容器'], ['onPrimaryContainer', '主色容器字'],
  ['secondary', '次色'], ['onSecondary', '次色上的字'], ['secondaryContainer', '次色容器'], ['onSecondaryContainer', '次色容器字'],
  ['tertiary', '第三色'], ['onTertiary', '第三色上的字'], ['tertiaryContainer', '第三色容器'],
  ['error', '错误'], ['onError', '错误上的字'], ['errorContainer', '错误容器'],
  ['outline', '线条'], ['outlineVariant', '细线'],
];
const SKIN_ROLE_KEYS = SKIN_ROLES.map((x) => x[0]);
/** 背景纹理（7 种，和 `UiSkin.DECORS` 一致）。 */
const DECORS = ['none', 'dots', 'grid', 'bubbles', 'hearts', 'stars', 'confetti'];
const DECOR_LABEL = {
  none: '无', dots: '圆点', grid: '网格', bubbles: '气泡',
  hearts: '爱心', stars: '星点', confetti: '彩纸屑',
};
const ICON_SHAPES = ['none', 'circle', 'rounded', 'square'];
const FONT_FAMILIES = ['', 'serif', 'monospace', 'sans'];
const FONT_WEIGHTS = ['', 'light', 'medium', 'bold'];

// ---------------- v2 布局模型（2026-10-14，对齐手机端 HomeLayout / PageBlock） ----------------
//
// ⚠️ 这一节的形状必须和手机端**一字不差**（夹范围、白名单、valid 判据全部对齐）：
//    · 主页块 → `HomeBlock`（ui/HomeLayout.kt）
//    · 页面块 → `PageBlock`（ui/PageBlocks.kt，6 种 kind —— v1 网页端只建模了 2 种，
//      这就是"设计台里没有、手机上其实有"的第一处错位，v2 补齐）

/** 主页块的类型（= 手机端 HomeBlockType.ALL）。 */
const HOME_BLOCK_TYPES = ['widget', 'heading', 'text', 'divider', 'spacer', 'html'];
const HOME_BLOCK_LABEL = {
  widget: '原生卡片', heading: '标题', text: '文字', divider: '分隔线', spacer: '留白', html: 'HTML',
};

/**
 * 主页可用的原生卡片 —— = 手机端注册表（外壳 SHELL_WIDGETS + 各插件声明的 widgets）。
 * ⚠️ widgetId 不在这张表里的块，手机端会画成"认不出这张卡片"的提示 ——
 *    设计台在 preflight 里就用这张表拦住。
 */
const HOME_WIDGETS = [
  { id: 'weather', label: '天气', module: '' },
  { id: 'today-agenda', label: '今日日程', module: 'calendar' },
  { id: 'week-timetable', label: '本周课表', module: 'calendar' },
  { id: 'calendar-overview', label: '日历一览', module: 'calendar' },
  { id: 'inbox-unread', label: '收件箱', module: 'calendar' },
  { id: 'appearance-hero', label: '外观头图', module: 'appearance' },
  { id: 'appearance-picker', label: '外观选择', module: 'appearance' },
  { id: 'notes-recent', label: '随心记', module: 'notes' },
  { id: 'mdnote-recent', label: '最近笔记', module: 'mdnote' },
  { id: 'notelink-today', label: '日程录音', module: 'notelink' },
  { id: 'nav-next', label: '导航', module: 'nav' },
  { id: 'wordbook-today', label: '今日背词', module: 'wordbook' },
  { id: 'lanlink-status', label: '局域网同步', module: 'lanlink' },
];
const HOME_WIDGET_BY_ID = Object.fromEntries(HOME_WIDGETS.map((w) => [w.id, w]));

/** 页面块的动作白名单（= 手机端 DrawerActions.ALL）。 */
const PAGE_ACTIONS = ['manage_plugins', 'check_update', 'open_settings', 'edit_home', 'sync_now'];
const PAGE_ACTION_LABEL = {
  manage_plugins: '管理插件', check_update: '检查更新', open_settings: '打开设置',
  edit_home: '自定义主页', sync_now: '立即同步',
};

/**
 * 页面块的类型（= 手机端 `PageBlock.KINDS`）。
 *
 * ★ v5 加 `html`：**自由画布**的落点 —— 一块里装一段自包含 HTML，
 *   由手机端 WebView 画出来（复用早就有的 `HomeHtmlBlock`）。
 *   有了它，"任意页面都能自由画"不需要给手机端发明新渲染器。
 */
const PAGE_BLOCK_KINDS = ['heading', 'text', 'actions', 'links', 'card', 'divider', 'html'];
const PAGE_BLOCK_LABEL = {
  heading: '标题', text: '文字', actions: '动作按钮', links: '链接按钮', card: '卡片',
  divider: '分隔线', html: '自由画布',
};
/** html 块的默认高度（= 自由画布默认画布高；单位 px = 手机 dp）。 */
const PAGE_HTML_DEFAULT_H = 620;

function clampNum(v, min, max, d) {
  const n = typeof v === 'number' && isFinite(v) ? v : d;
  return Math.min(max, Math.max(min, n));
}

/** 归一一个主页块（夹范围与手机端 HomeBlock.parse 一致）。 */
function normalizeHomeBlock(b) {
  if (!b || typeof b !== 'object') b = {};
  return {
    type: HOME_BLOCK_TYPES.includes(b.type) ? b.type : 'text',
    id: typeof b.id === 'string' ? b.id : '',
    span: clampNum(b.span, 1, 2, 1),
    height: clampNum(b.height, 0, 2000, 0),
    heightFactor: clampNum(b.heightFactor, 0.3, 4, 1),
    bg: typeof b.bg === 'string' ? b.bg : '',
    fg: typeof b.fg === 'string' ? b.fg : '',
    fontSize: clampNum(b.fontSize, 0, 48, 0),
    radius: clampNum(b.radius, 0, 48, 16),
    pad: clampNum(b.pad, -1, 48, -1),
    align: ['left', 'center', 'right'].includes(b.align) ? b.align : '',
    text: typeof b.text === 'string' ? b.text : '',
    widgetId: typeof b.widgetId === 'string' ? b.widgetId : '',
    show: Array.isArray(b.show) ? b.show.map(String).filter(Boolean) : [],
    limit: clampNum(b.limit, 0, 50, 0),
    html: typeof b.html === 'string' ? b.html : '',
  };
}

/** 归一主页布局（HomeLayout 形状）。没有 blocks 数组 → null（= 不动主页）。 */
function normalizeHomeLayout(raw) {
  if (!raw || typeof raw !== 'object' || !Array.isArray(raw.blocks)) return null;
  return {
    background: typeof raw.background === 'string' ? raw.background : '',
    padding: clampNum(raw.padding, 0, 48, 12),
    gap: clampNum(raw.gap, 0, 48, 10),
    blocks: raw.blocks.filter((b) => b && typeof b === 'object').map(normalizeHomeBlock),
  };
}

/** 手机端 SIMPLE 默认布局的等价物 —— 设计台"开始设计主页"的起点。 */
const HOME_LAYOUT_SIMPLE = {
  background: '', padding: 12, gap: 10,
  blocks: [
    { type: 'widget', id: 'weather', widgetId: 'weather', span: 2, height: 0, heightFactor: 1, bg: '', fg: '', fontSize: 0, radius: 16, pad: -1, align: '', text: '', show: [], limit: 0, html: '' },
    { type: 'widget', id: 'today-agenda', widgetId: 'today-agenda', span: 2, height: 0, heightFactor: 1, bg: '', fg: '', fontSize: 0, radius: 16, pad: -1, align: '', text: '', show: [], limit: 5, html: '' },
  ],
};

/** 空皮肤 = 全部走推导（这就是"默认主题"的样子）。v2：homeLayout 为 null = 不动主页。 */
function emptySkin() {
  return {
    statusBar: 'auto', statusBarColor: '', topBarBg: '', navBg: '', navSelected: '',
    navLabel: true, drawerBg: '', cardRadius: 0,
    colors: {}, decor: 'none', decorColor: '', cornerScale: 0,
    iconShape: 'none', iconBg: '', iconSize: 0,
    fontScale: 0, fontWeight: '', letterSpacing: 0, fontFamily: '',
    pageBg: {}, pageTitles: {}, pageBlocks: {}, hiddenTabs: {},
    homeLayout: null,
    // ★ v5（2026-10-15）：**自由画布**图层 —— `{页面id: {w,h,bg,code,els:[…]}}`。
    //   和 homeLayout / pageBlocks 是**并列**的第三套布局表达：
    //   · homeLayout  主页块流（v2，高级模式还在用）
    //   · pageBlocks  页面上下块（v2）
    //   · freeLayers  绝对定位的自由画布（v5，默认）
    //   导出时 freeLayers 优先 —— 有它的页面就编译成 html 块，没它的页面照旧走块流
    //   （这样"自由画布取代块流"不会把用户已经做好的老设计删掉）。
    freeLayers: {},
  };
}

/**
 * 归一自由画布图层。
 *
 * ⚠️ 元素级的归一在 `FreeCanvas.normalizeLayer` 里（那一份是权威，本文件不复制第二份）。
 *    这里只做**结构**层面的事：认不出的页面 id 丢掉（同 pageBlocks 口径）、
 *    空图层不留（= "这一页没设计过"）。
 *
 * ⚠️ `window.FreeCanvas` 在函数**运行时**取，不在模块加载时取 ——
 *    `skin-core.js` 比 `free-canvas.js` 先加载，模块级引用会拿到 undefined。
 */
function normalizeFreeLayers(raw) {
  const out = {};
  if (!raw || typeof raw !== 'object') return out;
  const FCn = (typeof window !== 'undefined') ? window.FreeCanvas : null;
  for (const k of Object.keys(raw)) {
    if (!PAGE_BY_ID[k]) continue;
    const v = raw[k];
    if (!v || typeof v !== 'object') continue;
    const L = FCn
      ? FCn.normalizeLayer(v, v.w, v.h)
      : { w: 393, h: 620, bg: '', code: '', els: [] };
    if (!L.els.length && !L.code && !L.bg) continue;
    out[k] = L;
  }
  return out;
}

/** 把一份（可能被改坏的）JSON 归一成合法皮肤。**不抛**：一个坏值不该让设计台打不开。 */
function normalizeSkin(rawIn) {
  const s = emptySkin();
  if (!rawIn || typeof rawIn !== 'object') return s;
  // ⚠️ 双口径折叠：手机端口径是 snake_case（page_blocks / status_bar…），v1 设计台
  //    导出的包是 camelCase（pageBlocks…）。两边都认、折叠成 camelCase 再往下走 ——
  //    否则导入另一种口径的包时那些字段会**静默全丢**。
  //    （浅拷贝再折叠：不污染调用方传进来的对象。）
  const raw = { ...rawIn };
  const ALIAS = {
    status_bar: 'statusBar', status_bar_color: 'statusBarColor', top_bar_bg: 'topBarBg',
    nav_bg: 'navBg', nav_selected: 'navSelected', nav_label: 'navLabel',
    drawer_bg: 'drawerBg', card_radius: 'cardRadius', icon_shape: 'iconShape',
    icon_bg: 'iconBg', icon_size: 'iconSize', font_scale: 'fontScale',
    font_weight: 'fontWeight', letter_spacing: 'letterSpacing', font_family: 'fontFamily',
    page_bg: 'pageBg', page_titles: 'pageTitles', page_blocks: 'pageBlocks',
    hidden_tabs: 'hiddenTabs', decor_color: 'decorColor', corner_scale: 'cornerScale',
    home_layout: 'homeLayout', free_layers: 'freeLayers',
  };
  for (const k of Object.keys(raw)) {
    const camel = ALIAS[k];
    if (camel && raw[camel] === undefined) raw[camel] = raw[k];
  }
  const str = (v, d = '') => (typeof v === 'string' ? v : d);
  const num = (v, d = 0) => (typeof v === 'number' && isFinite(v) ? v : d);
  s.statusBar = ['auto', 'light', 'dark'].includes(raw.statusBar) ? raw.statusBar : 'auto';
  s.statusBarColor = str(raw.statusBarColor);
  s.topBarBg = str(raw.topBarBg);
  s.navBg = str(raw.navBg);
  s.navSelected = str(raw.navSelected);
  s.navLabel = raw.navLabel !== false;
  s.drawerBg = str(raw.drawerBg);
  s.cardRadius = num(raw.cardRadius);
  if (raw.colors && typeof raw.colors === 'object') {
    for (const k of SKIN_ROLE_KEYS) {
      const v = raw.colors[k];
      if (typeof v === 'string' && v.trim()) s.colors[k] = v.trim();
    }
  }
  s.decor = DECORS.includes(raw.decor) ? raw.decor : 'none';
  s.decorColor = str(raw.decorColor);
  s.cornerScale = Math.min(3, Math.max(0, num(raw.cornerScale)));
  s.iconShape = ICON_SHAPES.includes(raw.iconShape) ? raw.iconShape : 'none';
  s.iconBg = str(raw.iconBg);
  s.iconSize = num(raw.iconSize);
  s.fontScale = Math.min(1.4, Math.max(0, num(raw.fontScale)));
  s.fontWeight = FONT_WEIGHTS.includes(raw.fontWeight) ? raw.fontWeight : '';
  s.letterSpacing = Math.min(0.10, Math.max(-0.02, num(raw.letterSpacing)));
  s.fontFamily = FONT_FAMILIES.includes(raw.fontFamily) ? raw.fontFamily : '';
  const strMap = (o) => {
    const out = {};
    if (o && typeof o === 'object') {
      for (const k of Object.keys(o)) if (typeof o[k] === 'string' && o[k].trim()) out[k] = o[k].trim();
    }
    return out;
  };
  s.pageBg = strMap(raw.pageBg);
  s.pageTitles = strMap(raw.pageTitles);
  if (raw.pageBlocks && typeof raw.pageBlocks === 'object') {
    for (const k of Object.keys(raw.pageBlocks)) {
      const arr = Array.isArray(raw.pageBlocks[k]) ? raw.pageBlocks[k] : [];
      const blocks = arr
        .filter((b) => b && typeof b === 'object')
        .map((b) => {
          const kind = PAGE_BLOCK_KINDS.includes(b.kind) ? b.kind : null;
          if (!kind) return null;
          const items = Array.isArray(b.items)
            ? b.items
                .filter((it) => Array.isArray(it) && it.length >= 2)
                .map((it) => [String(it[0]), String(it[1])])
                .filter((it) => it[0] && it[1])
            : [];
          const text = typeof b.text === 'string' ? b.text : '';
          const at = b.at === 'bottom' ? 'bottom' : 'top';
          // ★ v5：html 块（自由画布）带两个自己的字段。
          const html = typeof b.html === 'string' ? b.html : '';
          const height = clampNum(b.height, 40, 2000, PAGE_HTML_DEFAULT_H);
          // valid 判据与手机端 PageBlock.parse 一致 —— 无效块在这里就丢，
          // 别等装到手机上被那里丢（症状是"设计台里有、手机上没有"）。
          const valid = kind === 'divider' ? true
            : kind === 'html' ? html.trim() !== ''
            : kind === 'actions' ? items.length > 0 && items.every((it) => PAGE_ACTIONS.includes(it[1]))
            : kind === 'links' ? items.length > 0 && items.every((it) => /^https?:\/\//.test(it[1]))
            : text.trim() !== '';
          if (!valid) return null;
          return kind === 'html' ? { at, kind, text, items, html, height } : { at, kind, text, items };
        })
        .filter(Boolean)
        .slice(0, 8);
      if (blocks.length) s.pageBlocks[k] = blocks;
    }
  }
  // v2：主页块布局（坏输入 → null = 不动主页，同手机端 homeLayoutFromJson 的兜底）。
  s.homeLayout = normalizeHomeLayout(raw.homeLayout);
  // v5：自由画布图层（和 homeLayout / pageBlocks 并列的第三套布局表达）。
  s.freeLayers = normalizeFreeLayers(raw.freeLayers);
  if (raw.hiddenTabs && typeof raw.hiddenTabs === 'object') {
    for (const k of Object.keys(raw.hiddenTabs)) {
      if (Array.isArray(raw.hiddenTabs[k]) && raw.hiddenTabs[k].length) {
        s.hiddenTabs[k] = raw.hiddenTabs[k].map(String);
      }
    }
  }
  return s;
}

// ============================================================ 三、色值工具

function hexToRgb(hex) {
  if (typeof hex !== 'string') return null;
  const m = /^#?([0-9a-fA-F]{6})$/.exec(hex.trim());
  if (!m) return null;
  const v = parseInt(m[1], 16);
  return { r: (v >> 16) & 255, g: (v >> 8) & 255, b: v & 255 };
}
function rgbToHex(c) {
  const h = (n) => Math.round(Math.min(255, Math.max(0, n))).toString(16).padStart(2, '0');
  return '#' + h(c.r) + h(c.g) + h(c.b);
}
function rgba(c, a) { return `rgba(${Math.round(c.r)},${Math.round(c.g)},${Math.round(c.b)},${a})`; }

// ============================================================ 四、皮肤 → 实际用色（含 27 角色覆盖）

/**
 * 把皮肤摊成一份"CSS 里能直接用"的颜色表。
 *
 * **顺序和手机端一致**：先 `themeFrom` 推导 8 个基色，再 `toColorScheme` 补出
 * 容器/次表面那些，**最后**才是用户手写的 `colors` 覆盖 —— 所以"只改卡片、其余不动"
 * 是天然成立的（留空的角色仍然走推导）。
 */
function resolveColors(skin, dark, seedHex) {
  const seed = hexToRgb(seedHex) || DEFAULT_SEED;
  const d = themeFrom(seed, dark);
  const container = containerOf(d.primary, dark);
  // 卡片五档：手机端 `Card` 默认取 `surfaceContainerLow`（**这是"卡片底色改不动"的老坑**，
  // 见 UiSkin.colors 的注释），所以我们把五档都算出来，别让设计台也踩同一个坑。
  const base = {
    background: d.background,
    onBackground: d.onBackground,
    surface: d.surface,
    onSurface: d.onSurface,
    surfaceVariant: d.surface,
    onSurfaceVariant: d.muted,
    surfaceContainerLowest: withLightness(d.surface, dark ? 0.12 : 0.99),
    surfaceContainerLow: d.surface,
    surfaceContainer: withLightness(d.surface, dark ? 0.20 : 0.94),
    surfaceContainerHigh: withLightness(d.surface, dark ? 0.24 : 0.92),
    surfaceContainerHighest: withLightness(d.surface, dark ? 0.28 : 0.90),
    primary: d.primary,
    onPrimary: d.onPrimary,
    primaryContainer: container,
    onPrimaryContainer: readableOn(container, RATIO_TEXT),
    secondary: withLightness(d.primary, dark ? 0.68 : 0.52, 0.55),
    secondaryContainer: withLightness(d.primary, dark ? 0.32 : 0.88, 0.35),
    tertiary: withLightness(d.primary, dark ? 0.72 : 0.48, 0.30),
    tertiaryContainer: withLightness(d.primary, dark ? 0.34 : 0.90, 0.22),
    error: { r: 0xBA, g: 0x1A, b: 0x1A },
    errorContainer: dark ? { r: 0x93, g: 0x00, b: 0x00 } : { r: 0xFF, g: 0xDA, b: 0xD6 },
    outline: d.outline,
    outlineVariant: withLightness(d.outline, dark ? 0.34 : 0.86),
  };
  base.onSecondary = readableOn(base.secondary, RATIO_TEXT);
  base.onSecondaryContainer = readableOn(base.secondaryContainer, RATIO_TEXT);
  base.onTertiary = readableOn(base.tertiary, RATIO_TEXT);
  base.onError = readableOn(base.error, RATIO_TEXT);
  base.errorContainer = base.errorContainer;
  // 用户覆盖：只认白名单里的角色，坏名字静默忽略（和手机端一字不差）。
  for (const k of Object.keys(skin.colors)) {
    if (!SKIN_ROLE_KEYS.includes(k)) continue;
    const c = hexToRgb(skin.colors[k]);
    if (c) base[k] = c;
  }
  // 几个"派生自角色"的别名，方便 CSS 使用
  base.text = base.onBackground;
  base.muted = base.onSurfaceVariant;
  base.line = base.outlineVariant;
  return base;
}

/** 皮肤里的圆角缩放（`cornerDp`）：写死的圆角都要过这一层，否则"全局圆角"不是全局的。 */
function cornerDp(skin, dp) {
  const s = skin.cornerScale > 0 ? skin.cornerScale : 1;
  return Math.round(dp * s);
}
/** 字号缩放（0.8~1.4 之外没有意义：再小看不清、再大会溢出）。 */
function fontScale(skin) { return skin.fontScale > 0 ? skin.fontScale : 1; }
function fontFamilyCss(skin) {
  switch (skin.fontFamily) {
    case 'serif': return 'Georgia, "Songti SC", "SimSun", serif';
    case 'monospace': return 'ui-monospace, Menlo, Consolas, monospace';
    case 'sans': return '"Helvetica Neue", Arial, "PingFang SC", sans-serif';
    default: return '-apple-system, "Noto Sans SC", system-ui, sans-serif';
  }
}
function fontWeightCss(skin) {
  switch (skin.fontWeight) {
    case 'light': return 300;
    case 'medium': return 500;
    case 'bold': return 700;
    default: return 400;
  }
}

// ============================================================ 五、页面表（= 手机端的页面 id）

/**
 * 页面清单。`id` 必须**和手机端注册表里的一字不差** —— 皮肤里的
 * `pageBg` / `pageTitles` / `pageBlocks` / `hiddenTabs` 都以它为键，
 * 写错了那一项就是"改了没反应"（手机端对认不出的 id **静默忽略**）。
 *
 * ⚠️ 这份表要和三处对齐：各插件的 `threees-plugin.json`（navSlot 页面）、
 *   宿主自带的 `home` / `settings`、以及底栏（用户可自己排序）。
 */
const PAGES = [
  { id: 'home', label: '主页', icon: '🏠', kind: 'home', desc: '卡片墙：天气、日程、外观头图、随口一句' },
  { id: 'schedule', label: '日程', icon: '📅', kind: 'list', desc: '日程列表 / 月历 / 课表 / 融合时间轴', tabs: ['list', 'month', 'timetable', 'conflicts'] },
  { id: 'calendars', label: '日历', icon: '🗂', kind: 'grid', desc: '新建 / 改名 / 共享日历，以及团队' },
  { id: 'inbox', label: '收件箱', icon: '📥', kind: 'list', desc: '日历邀请与共享通知' },
  { id: 'ai', label: 'AI', icon: '🤖', kind: 'chat', desc: '问问题 / 出主意 / 改配置' },
  { id: 'notes', label: '随心记', icon: '🎙', kind: 'notes', desc: '录音只存在这台手机上' },
  { id: 'mdnote', label: '笔记', icon: '📝', kind: 'doc', desc: 'Markdown 文档与思维导图' },
  { id: 'mdnotelive', label: '实时笔记', icon: '👥', kind: 'doc', desc: '和同学一起实时编辑同一篇' },
  { id: 'plot', label: '函数', icon: '📈', kind: 'plot', desc: '输入函数看图像' },
  { id: 'nav', label: '导航', icon: '🧭', kind: 'map', desc: '校园地图：楼栋、3D 楼块、我在哪' },
  { id: 'wordbook', label: '背词', icon: '📚', kind: 'word', desc: '四六级与教材词库，按记忆曲线滚动' },
  { id: 'settings', label: '设置', icon: '⚙️', kind: 'settings', desc: '外观 / 模块 / 账号' },
];
const PAGE_BY_ID = Object.fromEntries(PAGES.map((p) => [p.id, p]));
/** 底栏默认前 5 格（用户可以自己排，所以皮肤里不要依赖下标）。 */
const NAV_DEFAULT = ['home', 'schedule', 'notes', 'mdnote', 'settings'];

/**
 * ★★ 画布底栏的**真实口径**（2026-10-14 v3.1 修）：手机端 `MainScreen` 的底栏是
 * 「主页永远正中间 + 左右各若干模块槽」，默认 = `BuiltinPlugins.DEFAULT_NAV`
 * （calendars / schedule / inbox / ai）被 `splitNav` 拆成左右各 2，主页插中间。
 * ⚠️ `settings` **不进底栏**（顶栏常驻 ⚙️ 那个入口）—— 上面那份 NAV_DEFAULT 是
 *    网页端历史遗留，和手机端不是一回事，画布渲染**必须用这份 NAV_BAR**。
 */
const NAV_BAR = ['calendars', 'schedule', 'home', 'inbox', 'ai'];

/** 声明了「沉浸」的页面（`PluginLoader` 的 immersive）：**没有顶栏**，状态栏直接贴页面。 */
const IMMERSIVE_PAGES = ['mdnote', 'mdnotelive'];

// ============================================================ 六、背景纹理（移植自 DecorLayer.kt）

/**
 * 画背景纹理。
 *
 * ⚠️ **固定 6% 不透明度** —— 这个数**不开放配置**：它是背景不是前景，
 *    浓一点就会和正文抢注意力，而"字看不清"是用户唯一会立刻卸载应用的理由。
 * ⚠️ 图案走**固定网格**、不随机：随机会让每次重画都不一样，看起来像"背景在闪"。
 */
function decorPaint(canvas, skin, colors) {
  const ctx = canvas.getContext('2d');
  const w = canvas.width, h = canvas.height;
  ctx.clearRect(0, 0, w, h);
  if (skin.decor === 'none') return;
  const base = hexToRgb(skin.decorColor) || colors.onBackground;
  const ink = rgba(base, 0.06);
  ctx.fillStyle = ink;
  ctx.strokeStyle = ink;
  const cols = (n) => w / n;   // ⚠️ 用宽度除以格数（写死 dp 会在窄屏上变过密）
  switch (skin.decor) {
    case 'dots': {
      const s = cols(14);
      for (let y = s / 2; y < h; y += s) {
        for (let x = s / 2; x < w; x += s) {
          ctx.beginPath(); ctx.arc(x, y, s * 0.10, 0, Math.PI * 2); ctx.fill();
        }
      }
      break;
    }
    case 'grid': {
      const s = cols(10);
      ctx.lineWidth = 1;
      for (let x = s; x < w; x += s) { ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, h); ctx.stroke(); }
      for (let y = s; y < h; y += s) { ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(w, y); ctx.stroke(); }
      break;
    }
    case 'bubbles': {
      const s = cols(6);
      for (let i = 0; i < 26; i++) {
        const x = ((i * 37) % 100) / 100 * w;
        const y = ((i * 61) % 100) / 100 * h;
        const r = s * (0.10 + ((i * 17) % 10) / 100);
        ctx.beginPath(); ctx.arc(x, y, r, 0, Math.PI * 2); ctx.stroke();
      }
      break;
    }
    case 'stars': {
      const s = cols(12);
      for (let y = s / 2; y < h; y += s) {
        for (let x = s / 2; x < w; x += s) {
          const r = s * 0.14;
          ctx.beginPath();
          ctx.moveTo(x, y - r); ctx.lineTo(x + r * 0.32, y - r * 0.32); ctx.lineTo(x + r, y);
          ctx.lineTo(x + r * 0.32, y + r * 0.32); ctx.lineTo(x, y + r);
          ctx.lineTo(x - r * 0.32, y + r * 0.32); ctx.lineTo(x - r, y);
          ctx.lineTo(x - r * 0.32, y - r * 0.32); ctx.closePath(); ctx.fill();
        }
      }
      break;
    }
    case 'hearts': {
      const s = cols(9);
      for (let y = s / 2; y < h; y += s) {
        for (let x = s / 2; x < w; x += s) {
          const r = s * 0.16;
          // 用两段贝塞尔拼一个心形（和 `DecorLayer.drawHearts` 同一套控制点走向）
          ctx.beginPath();
          ctx.moveTo(x, y + r * 0.9);
          ctx.bezierCurveTo(x - r * 1.6, y - r * 0.2, x - r * 0.6, y - r * 1.2, x, y - r * 0.35);
          ctx.bezierCurveTo(x + r * 0.6, y - r * 1.2, x + r * 1.6, y - r * 0.2, x, y + r * 0.9);
          ctx.closePath(); ctx.fill();
        }
      }
      break;
    }
    case 'confetti': {
      const s = cols(8);
      for (let y = s / 2; y < h; y += s) {
        for (let x = s / 2; x < w; x += s) {
          const k = ((x / s) | 0) + ((y / s) | 0);
          ctx.save();
          ctx.translate(x, y);
          ctx.rotate((k % 6) * 0.5);
          ctx.fillRect(-s * 0.06, -s * 0.14, s * 0.12, s * 0.28);
          ctx.restore();
        }
      }
      break;
    }
    default: break;
  }
}

// 浏览器直接可用时挂到 window（设计台是零依赖的普通脚本，不是模块）；
// Node 下（`tmp/verify-skin-studio.cjs` 的保真回归）也导出一份 —— 同一套代码两边跑。
const SKIN_CORE = {
  linear, gammaC, luminance, contrast, toOklab, fromOklab, lightness, withLightness,
  readableOn, themeFrom, containerOf,
  SKIN_ROLES, SKIN_ROLE_KEYS, DECORS, DECOR_LABEL, ICON_SHAPES, FONT_FAMILIES, FONT_WEIGHTS,
  emptySkin, normalizeSkin, normalizeFreeLayers, hexToRgb, rgbToHex, rgba, resolveColors,
  cornerDp, fontScale, fontFamilyCss, fontWeightCss, decorPaint,
  PAGES, PAGE_BY_ID, NAV_DEFAULT, NAV_BAR, IMMERSIVE_PAGES,
  HOME_BLOCK_TYPES, HOME_BLOCK_LABEL, HOME_WIDGETS, HOME_WIDGET_BY_ID,
  PAGE_ACTIONS, PAGE_ACTION_LABEL, PAGE_BLOCK_KINDS, PAGE_BLOCK_LABEL,
  clampNum, normalizeHomeBlock, normalizeHomeLayout, HOME_LAYOUT_SIMPLE,
  RATIO_TEXT, RATIO_LARGE, DEFAULT_SEED,
};
if (typeof window !== 'undefined') window.SkinCore = SKIN_CORE;
if (typeof module !== 'undefined' && module.exports) module.exports = SKIN_CORE;
