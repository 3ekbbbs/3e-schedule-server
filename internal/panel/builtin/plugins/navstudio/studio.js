/* ==========================================================================
   导航标注台 · 逻辑（2026-10-12）
   ==========================================================================

   格式定义见 `docs/格式-导航数据包-navpack-2026-10-12.md` —— 那份是权威，
   这里只实现它。

   ## 几个不可退让的约束

   ① **零依赖、不构建**。本机工具可能半年才用一次，任何构建产物到那时都会过期。
   ② **地图 JS API 是异步加载的**，而且加载失败的原因五花八门
      （Key 错、没勾 JS API、被 CSP 拦、公司网络挡了 map.qq.com）。
      所以加载失败必须**能说出是哪一种**，不能只显示"地图加载失败"。
   ③ **绝不丢用户数据**。编辑中的内容实时写进 localStorage，
      刷新页面/误关标签都还在。
   ④ **导出前体检**。标飞的点（离原点几十公里）要让用户看见并确认 ——
      数据一旦分发出去，导错楼是所有人一起错的。
   ========================================================================== */

/* ------------------------------------------------------------------ 常量 */

const NS_FORMAT = 'threees.navpack';
const NS_FORMAT_VERSION = 2;
/**
 * 导入时**接受**的版本。
 *
 * ★ 2026-10-12 加：v2 是 v1 的超集（新字段全部可选），所以 v1 文件照样能读
 * ——「升级主程序不该让用户手里的旧包失效」（文档 §9.5）。
 *   但 `> 2` 必须拒绝：旧端解不了新字段会**安静地做错事**。
 */
const NS_ACCEPT_VERSIONS = [1, 2];
const NS_STORE_KEY = 'threees.navstudio.state.v1';
const NS_MAPKEY_KEY = 'threees.navstudio.mapkey';

/** 楼栋坐标与标定原点的最大容许距离（米）。超过就提示"可能标飞了"。 */
const NS_FAR_METERS = 30000;

/**
 * 楼栋分类的**约定值**。
 *
 * ⚠️ 这是 `select` 的选项，但**不是枚举** —— 格式里 `type` 是自由文本，
 *    导入端遇到不认识的归类到 `other` 并**保留原值**（文档 §9.1）。
 *    所以这里出现"不在表里"的旧值时要能原样显示，不能悄悄改成 other。
 */
const NS_TYPES = [
  ['', '未分类'],
  ['teaching', '教学楼'],
  ['dorm', '宿舍'],
  ['canteen', '食堂'],
  ['office', '办公楼'],
  ['library', '图书馆'],
  ['sport', '体育场馆'],
  ['gate', '门'],
  ['other', '其他'],
];

/** 房间类型的约定值（同上，自由文本 + 约定值）。 */
const NS_ROOM_TYPES = [
  ['', '未分类'],
  ['classroom', '教室'],
  ['lecture', '阶梯教室'],
  ['lab', '实验室'],
  ['office', '办公室'],
  ['toilet', '卫生间'],
  ['stairs', '楼梯'],
  ['other', '其他'],
];

/**
 * 房块的缺省宽高。
 *
 * ★ **必须和插件端 `Room.DEFAULT_W` / `DEFAULT_H` 一致** ——
 *   两边不一致的话：标注台画出来是这个大小、手机上显示成另一个大小，
 *   而用户在标注台里看到的"预览"就失去了意义。
 */
const NS_ROOM_DEFAULT_W = 0.16;
const NS_ROOM_DEFAULT_H = 0.12;

/** 房块的最小尺寸（拖动缩放时的下限，太小了点不中）。 */
const NS_ROOM_MIN_W = 0.04;
const NS_ROOM_MIN_H = 0.04;

/* ------------------------------------------------------------------ 状态 */

/**
 * 整个工具的状态。
 *
 * ⚠️ **没有框架**，所以"改了状态 → 重画"要靠自己调 render 系列。
 *    这是刻意的：这个工具的界面小到不值得引框架，而引了之后
 *    "半年后再打开，依赖装不上"就是一个真实的风险。
 */
const ns = {
  packId: '',
  name: '',
  note: '',
  version: '1.0.0',
  origin: null,          // {lat, lng} | null
  tencentKey: '',        // ★ 分发包里带的 Key（默认空 —— 用户要求默认不带）
  referer: '',
  buildings: [],         // [{id,name,alias:[],lat,lng,campus,note,type,floors}]
  activeId: '',          // 当前编辑的那一条
  /**
   * 上一次"清掉没名字的"删掉的那些（给提示条上的"撤销"用）。
   *
   * ⚠️ **只活在内存里**，不进 `localStorage`（见 [nsSave] 的字段白名单）——
   *    撤销是"我手滑了，退回去"的即时动作，刷新页面之后还留着反而是个陷阱：
   *    用户已经不记得自己删过什么了，而那个撤销按钮会让他以为能退更早的状态。
   */
  undoPrune: null,

  /**
   * 上一次「一键清空」删掉的那些（`{buildings, origin}`）。
   *
   * ⚠️ 和 [undoPrune] 一样**只活在内存里** —— 刷新之后那个「撤销」链接
   *    本来也就不在了，留着反而会让人以为能退更早的状态。
   *    想跨刷新找回，用的是另一条退路：Ctrl+Z 那套（也只在本次会话内）。
   */
  undoClear: null,
  /**
   * 楼层编辑器当前停在第几层（`activeBuilding.floors` 的下标）。
   *
   * ⚠️ 它是**界面状态**，不进导出文件。切楼栋时如果越界要夹回去，
   *    否则"在 6 层的楼上看到 3F、切到只有 1 层的楼"会画出空白且不报错。
   */
  floorIndex: 0,
  /** 下一次单击地图的用途：'building'（填当前条）| 'origin'（设原点）。 */
  pickMode: 'building',
  mapReady: false,
  /**
   * 用户明确选了"**先不用地图，直接填坐标**"。
   *
   * ★ 2026-10-12 加。没有它的话，`nsMapFailed` 会在失败时把 Key 输入层
   *   **重新弹回来** —— 而用户刚刚才选了"不用地图"，3 秒后又被挡住，
   *   看起来就是"这个按钮没用"。
   *
   * ⚠️ 它**只活在内存里**（不落 localStorage）：刷新页面应该回到"再试一次地图"，
   *    而不是永久记住"这个人不要地图"（他可能过两天就申请了 Key）。
   */
  noMapMode: false,

  /**
   * 地图现在是 3D 还是 2D。
   *
   * ★ 2026-10-13 加。默认 **3D** —— 因为标注的对象是**三维建筑**：
   * 在平地上凭想象描一个立体楼的外墙，又慢又描不准（用户的原话是
   * "我的标注工作做得太辛苦了，而且还不能标注 3D 建筑"）。
   */
  is3d: true,

  /**
   * ★★★ 当前工具（2026-10-13 第二轮重做，取代了原来的布尔 `pickNew`）。
   *
   * ## 为什么要把"点地图"从隐式变显式
   *
   * 原来只有一个 `pickNew`，**而且默认开着** —— 于是点哪儿都建楼：
   * 想"点一下选中旁边那栋看看"、想"把这个标记拖到正确位置"，
   * 都会先凭空建出一条空楼来。
   *
   * 用户的原话：
   *   > "要加入选择工具、标点工具以及批量工具之类的，允许我进行更精细的操作，
   *   >  而不是我点一下就新建一个点"
   *
   * 现在工具是显式的五个，见 [NS_TOOLS]；`select` 那个函数是**唯一入口**。
   *
   * ⚠️ **默认是 `select` 不是 `add`。** 上一版的默认行为就是用户抱怨的那件事。
   */
  tool: 'select',

  /**
   * 批量模式下勾选的那些楼的 id。
   *
   * ⚠️ 用**数组**不用 `Set`：草稿要 `JSON.stringify` 存盘，`Set` 会存成 `{}`，
   *    于是"刷新页面之后勾选全没了"—— 而那种事看起来像"批量功能坏了"。
   */
  batchIds: [],

  /** 正在拖的那一栋（`{id, moved}`）。null = 没在拖。 */
  dragging: null,

  /**
   * 在标记上**按下去、但还没开始拖**的那一下（`{id, x, y, armed}`）。
   *
   * ## ★★★ 为什么要和 [dragging] 分开（用户报的"视角锁定"的根因）
   *
   * 用户的原话：
   *
   * > "当我选定一个建筑以后，**视角就锁定了** …… 视角锁定以后就动不了了。
   * >  正常来讲，应该是跳到那边然后可以移动。"
   *
   * 根因是旧的写法在 `mousedown` 里**一命中标记就** `setDraggable(false)`，
   * 而恢复只挂在 SDK 的 `mouseup` 上。于是：
   *
   * ```
   *   按下（命中标记）→ 地图拖不动了
   *   ↑ 这一松手的事件只要没派发（鼠标移出容器才松、窗口失焦、触摸板手势）
   *   └─► ns.dragging 永不清空 ──► setDraggable(true) 永远不执行 ──► **地图永久锁死**
   * ```
   *
   * 现在拆成三步：
   *
   * ```
   *   mousedown   只记下"可能拖"，**完全不动**地图的拖拽能力
   *   mousemove   超过 [NS_ARM_PX] 像素才真的进入拖动（**这时才** setDraggable(false)）
   *   mouseup     **无论如何**都把拖拽能力还回去
   * ```
   *
   * 这样"单击选中"这件事全程不碰地图的拖拽开关 —— 既不会再锁死，
   * 也不会再打断 SDK 的手势识别（那正是用户说的"点了建筑好像也没选中"的另一半原因）。
   */
  press: null,

  /** 「二次识别地名」正在跑（防重入）。 */
  poiRunning: false,

  /**
   * 确认面板上正在处理的那些 `[{id, cands}]`（空 = 面板没开）。
   *
   * ⚠️ **只活在内存里**，不进草稿 —— 刷新页面时面板本来就该关着，
   *    而把一堆候选存进 localStorage 只会让草稿变大、还会过期。
   */
  poiReview: [],

  /**
   * 地图标注（2026-10-13 深夜）。见文件末尾那节 `NS_MARK_KINDS`。
   *
   * ⚠️ 它是**独立的一层**，不进 `buildings` —— 楼栋有名字/有轮廓/日程能引用，
   *    而这些只是"地图上写着的字"。混在一起的话手机端"按类别筛楼"会把它们筛进去。
   */
  marks: [],

  /** 现在在编辑哪一条标注（'' = 没选）。和 `activeId`（楼栋）**互斥**。 */
  activeMarkId: '',

  /** 当前用「标注」工具点下去时，默认放哪一种（右侧面板可切）。 */
  markKind: 'text',

  /**
   * 撤销 / 重做的栈。**只活在内存里**，不进草稿 —— 刷新页面本身就是
   * "从这里重新开始"，把两套历史放进 localStorage 只会让草稿膨胀。
   */
  history: { undo: [], redo: [], tags: [], tagsRedo: [], at: 0 },

  /**
   * 上一次选的类别。
   *
   * ⚠️ 建新楼时**默认沿用它** —— 用户通常连着标好几栋教学楼，
   *    每栋都重选一次类别是无谓的重复。
   */
  lastType: '',

  /** 形状微调用的当前旋转角（度）。只活在内存里，不导出。 */
  // ⚠️ 已废弃（2026-10-13 深夜）：它是**全局**的旋转角，切楼会串。
  //    现在微调走「基准快照 + 比例」（见 nsShapeBase），旋转是**相对基准**的。
  //    留着这个字段只是为了让老草稿不报错，别再用它。
  shapeRot: 0,
};

/* ------------------------------------------------------------------ 工具 */

const $ = (id) => document.getElementById(id);

let nsToastTimer = null;
function nsToast(msg) {
  const el = $('ns-toast');
  if (!el) return;
  el.textContent = msg;
  el.classList.remove('hidden');
  if (nsToastTimer) clearTimeout(nsToastTimer);
  nsToastTimer = setTimeout(() => el.classList.add('hidden'), 2600);
}

/**
 * 带 HTML 的提示条（2026-10-13）。
 *
 * ⚠️ **为什么另开一个函数而不是把 [nsToast] 改成 innerHTML**：
 *    `nsToast` 现在被几十处调用，其中不少会把**用户输入**拼进去
 *    （包名、楼名、错误原文）。整条改成 `innerHTML` 等于把那些地方
 *    全变成注入点 —— 而其中任何一处都不该由"提示条"来负责转义。
 *    所以：要标签的地方显式用这个，并且**拼进去的动态文本自己过 [nsEsc]**。
 *
 * ⚠️ 停留时间**比 [nsToast] 长**（6 秒）：它只用在"撤销"这种需要用户
 *    读一下再决定的地方，2.6 秒根本来不及看清上面写了什么。
 */
function nsToastHtml(html) {
  const el = $('ns-toast');
  if (!el) return;
  el.innerHTML = html;
  el.classList.remove('hidden');
  if (nsToastTimer) clearTimeout(nsToastTimer);
  nsToastTimer = setTimeout(() => el.classList.add('hidden'), 6000);
}

function nsStatus(msg) {
  const el = $('ns-status');
  if (el) el.textContent = msg;
}

/**
 * HTML 转义。
 *
 * ★ 2026-10-12 加：原来这个页面**没有**转义工具 —— 而它的错误提示要把
 * "腾讯原话"拼进 `innerHTML`。那句话里出现 `<` 是完全可能的（比如
 * 报错里带参数占位符），不转义就会**把提示语本身切坏**（更糟的情况
 * 是拼出一个错误的结构，让整段说明都看不见）。
 *
 * ⚠️ 只用在"插进 innerHTML 的动态文本"上。静态的说明文字该用标签就用标签，
 *    别为了统一把 `<b>` 也转义掉。
 */
function nsEsc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/** 生成一个本包内唯一的 id。 */
function nsNewId() {
  let n = 1;
  const used = new Set(ns.buildings.map((b) => b.id));
  while (used.has('b_' + n)) n += 1;
  return 'b_' + n;
}

/* ------------------------------------------------------------------ 地图视野 */

/**
 * 地图视野（中心 + 缩放）的持久化键。
 *
 * ⚠️ 和草稿（`NS_STORE_KEY`）**分开存**：草稿是"用户标的数据"，
 *    视野是"界面状态"。混在一起的话，用户想重置视野就得连数据一起清。
 */
const NS_VIEW_KEY = 'threees.navstudio.view';

/**
 * 默认视野 —— 用户给的那个点。
 *
 * ★ 用户的原话："默认给我跳到这个经纬度去,每次重新刷新都要一直拉一直拉,烦死了
 *              21.149328,110.301408"
 *
 * ⚠️ 这只是**第一次打开**时的落点。之后一律回到上次离开的位置（见 [nsSaveView]）——
 *    只做"默认点"的话，用户标到校园另一头、刷新一下又被打回原点，还是要拉。
 */
const NS_DEFAULT_VIEW = { lat: 21.149328, lng: 110.301408, zoom: 17 };

/** 读上次的视野；没有就用默认点。 */
function nsLoadView() {
  try {
    const raw = localStorage.getItem(NS_VIEW_KEY);
    if (!raw) return Object.assign({}, NS_DEFAULT_VIEW);
    const v = JSON.parse(raw);
    // ⚠️ 逐字段校验：坏值（NaN / 越界）会让地图**直接建不起来**，
    //    而用户只会看到"标注台打不开"。宁可退回默认点。
    if (!Number.isFinite(v.lat) || !Number.isFinite(v.lng) || !Number.isFinite(v.zoom)) {
      return Object.assign({}, NS_DEFAULT_VIEW);
    }
    if (v.lat < 3 || v.lat > 54 || v.lng < 73 || v.lng > 136) {
      return Object.assign({}, NS_DEFAULT_VIEW);
    }
    return {
      lat: v.lat,
      lng: v.lng,
      zoom: Math.min(20, Math.max(4, v.zoom)),
    };
  } catch (e) {
    return Object.assign({}, NS_DEFAULT_VIEW);
  }
}

/** 把当前视野存下来（地图 `moveend` / `zoomend` 时调）。 */
function nsSaveView() {
  if (!nsMap) return;
  try {
    const c = nsMap.getCenter();
    localStorage.setItem(NS_VIEW_KEY, JSON.stringify({
      lat: Number(c.getLat().toFixed(6)),
      lng: Number(c.getLng().toFixed(6)),
      zoom: Number(nsMap.getZoom().toFixed(2)),
    }));
  } catch (e) { /* 存不下就算了，不影响标注 */ }
}

/** 去掉所有空白 + 转小写（和插件侧的 norm 保持一致）。 */
const nsNorm = (s) => String(s || '').replace(/\s+/g, '').toLowerCase();

/**
 * 解析用户手填的坐标。
 *
 * ⚠️ 和插件侧 `parseCoord` 同一套规则（**容忍各种粘贴格式** +
 *    拒绝"经纬度写反"）。两处规则不一致的话，会出现
 *    "工具里看着没问题、导进插件后不见了"这种最难查的 bug。
 *
 * @returns {{lat:number,lng:number}|null}
 */
function nsParseCoord(raw) {
  if (!raw) return null;
  const cleaned = String(raw).replace(/，/g, ',').replace(/；/g, ';')
    .replace(/。/g, '.').replace(/　/g, ' ');
  const parts = cleaned.split(/[,;\s]+/).map((x) => x.trim()).filter(Boolean);
  if (parts.length < 2) return null;
  const lat = Number(parts[0]);
  const lng = Number(parts[1]);
  if (!Number.isFinite(lat) || !Number.isFinite(lng)) return null;
  if (lat < -90 || lat > 90 || lng < -180 || lng > 180) return null;
  if (lat === 0 && lng === 0) return null;
  // 中国经度 73~136 —— 纬度落在这个区间里基本就是写反了
  if (lat >= 73 && lat <= 136 && lng >= 3 && lng <= 54) return null;
  return { lat, lng };
}

const nsFmtCoord = (lat, lng) => `${Number(lat).toFixed(6)},${Number(lng).toFixed(6)}`;

/** 两点距离（米）。Haversine —— 手写，不引库。 */
function nsDistance(a, b) {
  if (!a || !b) return 0;
  const R = 6371000;
  const dLat = (b.lat - a.lat) * Math.PI / 180;
  const dLng = (b.lng - a.lng) * Math.PI / 180;
  const la1 = a.lat * Math.PI / 180;
  const la2 = b.lat * Math.PI / 180;
  const h = Math.sin(dLat / 2) ** 2 + Math.cos(la1) * Math.cos(la2) * Math.sin(dLng / 2) ** 2;
  return 2 * R * Math.asin(Math.min(1, Math.sqrt(h)));
}

/* ------------------------------------------------------------------ 持久化 */

function nsSave(tag) {
  try {
    localStorage.setItem(NS_STORE_KEY, JSON.stringify({
      packId: ns.packId, name: ns.name, note: ns.note, version: ns.version,
      origin: ns.origin, tencentKey: ns.tencentKey, referer: ns.referer,
      buildings: ns.buildings, activeId: ns.activeId,
      marks: ns.marks, activeMarkId: ns.activeMarkId,
    }));
  } catch (_) {
    // 存不下（隐私模式 / 配额满）不该打断标注 —— 顶多刷新后丢草稿
  }
  // ★★★ 历史栈的唯一入口（2026-10-13 晚）
  //
  // 数据变更点有四十多处（导入 / 补轮廓 / 删楼 / 改名 / 改类别 / 拖动 / 批量…）。
  // 逐个去调 `nsCommit()` 的话，**漏一个就是"那一步退不回去"** ——
  // 而用户不会知道他漏了，只会以为撤销坏了。
  nsHistoryAfterSave(tag || '');
}

/* ==========================================================================
   撤销 / 重做（2026-10-13 晚加）
   ==========================================================================

   用户的原话：

   > "这一次顺便把整个 UI 交互逻辑进行一次重构，**加入撤回重做这些功能**，
   >  让它更像一个合格的编辑器。"

   实现是**快照式**，不是"反向操作"：

   * 这个标注台的"文档"就是 `ns.buildings` 那一坨 —— 几百栋楼的量级
   * 每次 `JSON.stringify` 一次也就几十 KB，比维护一套"每类操作都带反向逻辑"
     的系统小两个数量级
   * 快照式天然支持所有操作（包括别人的新操作），不会出现
     "删楼能退、挪楼不能退"这种各凭运气的状况

   ========================================================================== */

/** 进历史栈的字段。**刻意不含 `activeId`**（理由见 [nsSnapKey]）。 */
const NS_SNAP_KEYS = ['packId', 'name', 'note', 'version', 'origin', 'buildings'];

/** 历史栈上限 —— 再多就是拿内存换一个没人会翻回去的过去。 */
const NS_HISTORY_MAX = 80;

/** 同一个 tag 的连续变更在这么多毫秒内合成一步（打字时用的）。 */
const NS_COALESCE_MS = 900;

/** 当前文档的一份完整快照。 */
function nsSnapshot() {
  const o = {};
  NS_SNAP_KEYS.forEach((k) => {
    o[k] = JSON.parse(JSON.stringify(ns[k] === undefined ? null : ns[k]));
  });
  return o;
}

/**
 * 比"有没有变"用的指纹。
 *
 * ⚠️ **不含 `activeId`**：点来点去换选中项不是编辑动作。把它算进去的话，
 *    用户点十栋楼就把十条历史占满了 —— 表现是"按了 Ctrl+Z 什么也没发生"、
 *    或者退回去的只是选中项而不是数据。
 */
function nsFingerprint(s) {
  return JSON.stringify([s.packId, s.name, s.note, s.version, s.origin, s.buildings]);
}

/** 上一次记进历史的文档状态（模块级：它不该跟着草稿被存进 localStorage）。 */
let nsSnap = null;
let nsSnapKey = '';

/**
 * 存盘之后把这一笔记进历史。
 *
 * @param tag 这步在干什么（'楼名' / '删除楼栋' …）。同一个 tag 在
 *   [NS_COALESCE_MS] 内的连续变更会**合并成一步** ——
 *   "明德楼"三个字一格格打出来不该变成三条历史。
 * @returns 真的记了一条吗
 */
function nsHistoryAfterSave(tag) {
  if (!nsSnap) { nsSnap = nsSnapshot(); nsSnapKey = nsFingerprint(nsSnap); return false; }
  const snap = nsSnapshot();
  const key = nsFingerprint(snap);
  if (key === nsSnapKey) return false;          // 数据没变（例如只改了选中项）

  const now = Date.now();
  const h = ns.history;
  const same = tag && h.undo.length
    && h.tags[h.undo.length - 1] === tag
    && (now - h.at) < NS_COALESCE_MS;

  if (!same) {
    // ⚠️ 压栈的是**改动前**那一份 —— 撤销要回它，而不是回半当中。
    h.undo.push(nsSnap);
    h.tags.push(tag || '改动');
    if (h.undo.length > NS_HISTORY_MAX) { h.undo.shift(); h.tags.shift(); }
  }
  h.redo = [];
  h.tagsRedo = [];
  h.at = now;
  nsSnap = snap;
  nsSnapKey = key;
  nsRenderHistory();
  return true;
}

/** 把一份快照**装回**文档（撤销/重做共用）。 */
function nsApplySnap(snap) {
  NS_SNAP_KEYS.forEach((k) => {
    ns[k] = JSON.parse(JSON.stringify(snap[k] === undefined ? null : snap[k]));
  });
  // ⚠️ activeId **不恢复**（它不属于"编辑内容"，而且在栈里存它会让撤销
  //    把用户的选择也退回去，很怪）。但要清掉那些指向"已经不存在的楼"的
  //    死 id —— 不然右边那一栏显示"还没选中"，而用户明明点了东西。
  if (!ns.buildings.some((b) => b.id === ns.activeId)) ns.activeId = '';
  nsSnap = nsSnapshot();
  nsSnapKey = nsFingerprint(nsSnap);
  nsSave();
  nsRenderAll();
  nsRenderHistory();
}

/** 撤销一步。 */
function nsUndo() {
  const h = ns.history;
  if (!h.undo.length) { nsToast('没有可撤销的了'); return; }
  const tag = h.tags[h.tags.length - 1] || '改动';
  h.redo.push(nsSnap);
  h.tagsRedo.push('当前');
  const snap = h.undo.pop();
  h.tags.pop();
  nsApplySnap(snap);
  nsToast('已撤销：' + tag + '　（Ctrl+Shift+Z 退回来）');
}

/** 重做一步。 */
function nsRedo() {
  const h = ns.history;
  if (!h.redo.length) { nsToast('没有可重做的了'); return; }
  h.undo.push(nsSnap);
  h.tags.push('改动');
  const snap = h.redo.pop();
  h.tagsRedo.pop();
  nsApplySnap(snap);
  nsToast('已重做');
}

/**
 * 根据栈的深浅更新顶栏那两个按钮（能不能点 + 悬停时提示撤销什么）。
 *
 * ⚠️ 每次进历史 / 撤销 / 重做都要调 —— 不调的话，两个按钮一直是"能点"的样子，
 *    用户点下去只看到一句"没有可撤销的了"。
 */
function nsRenderHistory() {
  const u = $('ns-btn-undo');
  const r = $('ns-btn-redo');
  const tags = ns.history.tags;
  const canU = tags.length > 0;
  if (u) {
    u.disabled = !canU;
    u.title = canU
      ? ('撤销「' + tags[tags.length - 1] + '」（Ctrl+Z）')
      : '还没有可撤销的（Ctrl+Z）';
  }
  if (r) r.disabled = !(ns.history.tagsRedo.length > 0);
}

/** 把历史栈里那份"当前状态"初始化（加载草稿 / 导入文件之后调）。 */
function nsHistoryReset() {
  nsSnap = nsSnapshot();
  nsSnapKey = nsFingerprint(nsSnap);
  ns.history.undo = [];
  ns.history.redo = [];
  ns.history.tags = [];
  ns.history.tagsRedo = [];
  ns.history.at = 0;
  nsRenderHistory();
}

function nsLoad() {
  try {
    const raw = localStorage.getItem(NS_STORE_KEY);
    if (!raw) return;
    const s = JSON.parse(raw);
    ns.packId = String(s.packId || '');
    ns.name = String(s.name || '');
    ns.note = String(s.note || '');
    ns.version = String(s.version || '1.0.0');
    ns.origin = s.origin && Number.isFinite(s.origin.lat) ? s.origin : null;
    ns.tencentKey = String(s.tencentKey || '');
    ns.referer = String(s.referer || '');
    // ⚠️ 过一遍 nsNormalizeBuilding：**老草稿**（v2 之前存的）里没有
    //    `type` / `floors`，不补的话后面 `b.floors.length` 直接抛错，
    //    表现是"打开标注台白屏" —— 而用户的草稿一点都没坏，是我们没兼容。
    ns.buildings = Array.isArray(s.buildings)
      ? s.buildings.filter((b) => b && b.name).map(nsNormalizeBuilding) : [];
    ns.activeId = String(s.activeId || '');
    // ⚠️ 只有"**指向一条已经不存在的楼**"才清掉 —— 那是坏数据，不是"没选"。
    //
    // ★ 2026-10-13 晚改：这里原来还有半句"夹到存在的第一条上"，也就是
    //    activeId 为空时**自动选中第一栋**。用户明确说过"我没办法取消选择一个建筑"，
    //    而那句话的另一半就是这里 —— 他取消掉之后，一刷新又被自动选上了，
    //    于是"取消"看起来就是没生效。现在**不兜底**：草稿里记的是空就真的是空。
    if (ns.activeId && !ns.buildings.some((b) => b.id === ns.activeId)) ns.activeId = '';
    // ★ 标注（2026-10-13 深夜）：老草稿没有这个字段 → 空数组，不报错。
    ns.marks = Array.isArray(s.marks) ? s.marks.map(nsNormalizeMark) : [];
    ns.activeMarkId = String(s.activeMarkId || '');
    if (ns.activeMarkId && !ns.marks.some((m) => m.id === ns.activeMarkId)) ns.activeMarkId = '';
  } catch (_) {
    // 草稿坏了当没有 —— 宁可让用户重来，也不要让他对着一个打不开的页面
  }
}

/* ------------------------------------------------------------------ 地图 */

let nsMap = null;
let nsMarkers = new Map();     // id -> marker
let nsOriginMarker = null;

/**
 * 这次打开地图用的视野（中心 + 缩放）。
 *
 * ⚠️ 它是**建图之前**就要定好的（`new TMap.Map({center, zoom})` 只吃一次），
 *    而读 localStorage 要等 `nsBoot` —— 所以在这里先占个位，
 *    `nsBoot` 里用 [nsLoadView] 覆盖它。
 */
let view = { lat: 21.149328, lng: 110.301408, zoom: 17 };

/**
 * 底面轮廓那个图层（2026-10-12 深夜加）。
 *
 * 手机端那个"真 3D 楼块"就是把这块轮廓**往上拔**（腾讯 GL 的
 * `ExtrudablePolygonStyle`）。所以在标注台这边要能把轮廓画出来、
 * 并且**当场看到它长什么样** —— 画歪了在手机上是"一栋歪楼"，
 * 而那时候已经导出分发了。
 */
let nsShapeLayer = null;

/**
 * 正在描轮廓的那栋楼（null = 没在描）。
 *
 * ⚠️ 它和 `ns.activeId` 分开存：用户可能描到一半去点左边的列表换一栋看，
 *    `activeId` 一变，正在描的点就会**画到另一栋楼上**。
 *    存下"描的是哪一栋"，切走的时候就能把这次描到一半的丢掉（或者留着）。
 */
let nsShapeTargetId = null;

/**
 * 动态加载腾讯地图 JS API。
 *
 * ⚠️ 为什么用 `<script>` 注入而不是静态写一个 `<script src>`：
 *    Key 是用户**现场填**的（存 localStorage），静态标签在页面加载时就要有 Key。
 *
 * ## ★★ 2026-10-12 修：这里原来会**假装成功**
 *
 * 腾讯 GL 的**加载阶段完全不校验 Key**（实测：拿 `INVALIDKEY123456` 请求，
 * 照样 HTTP 200 + 2.2MB 完整 SDK，`window.TMap` 与 `window.TMap.Map` 都在，
 * `callback=` 也确实会被调）。鉴权失败只打在 **console** 里：
 *
 *     鉴权失败：当前产品鉴权失败，请检查KEY及相关信息是否准确。
 *
 * 而 `new TMap.Map(...)` **同步不抛异常、也不触发 error 事件** —— 地图就是黑的。
 * 原来的实现只验 `window.TMap` 存不存在（那永远为真），于是"Key 错"会一路
 * 走到 `.then()` 里被当成成功，界面显示「地图就绪」而地图全黑。
 * **静默失败被伪装成成功，是最坏的一种失败。**
 *
 * 现在加载完**必须**再验一次 `TMap.Map` 是个函数，并把 SDK 自己的鉴权
 * 报错截下来（见 [nsCaptureAuthErrors]）。
 *
 * @returns {Promise<void>} 加载成功 resolve；失败 reject（带人话原因）
 */
function nsLoadMapApi(key) {
  return new Promise((resolve, reject) => {
    // 已经就绪（用户改 Key 重试时走这里）—— 但仍要验 `.Map` 在不在
    if (nsMapApiUsable()) return resolve();

    const cbName = 'nsMapInitCallback';
    // ★ 腾讯 JS API 支持 `callback=` 参数：加载完会调这个全局函数。
    //   比"轮询 window.TMap 有没有出现"可靠得多（后者在慢网络下要等很久，
    //   而且分不清"还在下载"和"下载失败了"）。
    let settled = false;
    const settle = (fn, arg) => { if (!settled) { settled = true; fn(arg); } };

    window[cbName] = () => {
      // ★★ 关键：callback 被调到**不等于** SDK 能用。
      //    置一个标记，然后在下一帧验（SDK 是同步建 TMap 的，同一帧里就绪）。
      if (nsMapApiUsable()) settle(resolve);
      else settle(reject, new Error('SDK_INCOMPLETE'));
    };

    const s = document.createElement('script');
    s.src = `https://map.qq.com/api/gljs?v=1.exp&key=${encodeURIComponent(key)}&callback=${cbName}`;
    s.onerror = () => settle(reject, new Error('NETWORK'));
    document.head.appendChild(s);

    // 兜底超时：网络层不报错、但脚本永远卡在半路（公司网络拦了 map.qq.com 就是这样）
    //
    // ⚠️ 判据必须是 `nsMapApiUsable()`，不能只是 `window.TMap` 存在 ——
    //    后者在"脚本下载到一半"时也可能为真，于是超时守卫**永远不触发**。
    setTimeout(() => {
      if (!nsMapApiUsable()) settle(reject, new Error('TIMEOUT'));
    }, 12000);
  });
}

/**
 * 腾讯地图 SDK 是不是**真的能用**。
 *
 * 判据只有一个：`TMap.Map` 是个可调用的构造函数。
 * 只判断 `window.TMap` 存在是不够的 —— 实测无效 Key 下它也"存在"。
 */
function nsMapApiUsable() {
  return !!(window.TMap && typeof window.TMap.Map === 'function');
}

/**
 * 抓 **CSP 违规** —— 证据链的**第 0 条**（最强、最具体、且与 Key 无关）。
 *
 * ## ★★ 2026-10-12 加：用户贴出来的那段 console 说明必须有这条
 *
 * 用户报"打开还是一片白"，贴过来的报错是：
 *
 * ```
 *   Connecting to 'https://mapstyle.qpic.cn/…' violates the following
 *   Content Security Policy directive: "connect-src …"
 *   … 未知错误：获取样式文件失败。
 * ```
 *
 * 也就是**页面的 CSP 漏了一个域名**，SDK 拿不到样式文件 → 地图永远空白。
 * 而在这之前，我们的三条证据（鉴权 / 瓦片被拒 / 一张瓦片都没来）
 * **一条都指不出真正的原因** —— 最后只能猜成"Key 类型不对"，
 * 让用户去改一个**本来完全正确**的配置。
 *
 * ## 为什么这是最可靠的一条
 *
 * 浏览器**自己**会在 CSP 拦截时派发 `securitypolicyviolation` 事件，
 * 事件里带着 `blockedURI`（被挡的完整 URL）和 `violatedDirective`（哪条指令）。
 * 这不是猜、也不是从 SDK 的日志里捞 —— 是浏览器把事实直接交给我们。
 *
 * ⚠️ 它**必须覆盖"运行时才知道的域名"**：`mapstyle.qpic.cn` 在 2.2MB 的
 *    bundle 里**一次都没出现**（我全量扫过主机名），它是 SDK 从服务端下发的
 *    配置里拿到的。所以**静态补 CSP 永远可能漏**，这一条运行时防线才是根本。
 *
 * @returns {{ first: () => string|null, stop: Function }}
 */
function nsWatchCsp() {
  let first = null;

  const onViolation = (e) => {
    if (first) return;
    const uri = e.blockedURI || '（未知地址）';
    const dir = e.violatedDirective || e.effectiveDirective || '（未知指令）';
    first = `${dir} 挡住了 ${uri}`;
  };

  try {
    document.addEventListener('securitypolicyviolation', onViolation);
  } catch (_) {
    // 老浏览器没有这个事件 —— 那就只剩后面那三条证据，不影响主流程
  }

  return {
    first: () => first,
    stop: () => {
      try {
        document.removeEventListener('securitypolicyviolation', onViolation);
      } catch (_) { /* 忽略 */ }
    },
  };
}

/**
 * 抓 SDK 的**鉴权失败**报错。
 *
 * ## 为什么非得劫 console
 *
 * 腾讯 SDK 鉴权失败时**只 `console.error`**，既不抛异常、也没有任何回调、
 * 也没有 error 事件（全部实测确认）。要拿到"Key 不对"这个事实，
 * 除了截 console 没有第二条路。
 *
 * ## ★ 2026-10-12 加：顺便抓「获取样式文件失败」
 *
 * 那句 `未知错误：获取样式文件失败。` 是 SDK 在**样式文件下不下来**时的原话。
 * 它本身不说明原因（可能是 CSP、可能是网络、可能是 Key），
 * 但它是"地图为什么白"的**直接症状** —— 单独报出来，
 * 用户和我们都少猜一层。
 *
 * ⚠️ 用**一次性**的拦截：抓到就恢复原函数。常驻劫持会让开发者调试时
 *    看不到自己的日志，而这种工具最怕"排查时没有日志"。
 *
 * @returns {{ hit: () => string|null, styleFail: () => string|null, stop: Function }}
 */
function nsCaptureAuthErrors() {
  const orig = console.error;
  let hitText = null;
  let styleText = null;
  let stopped = false;

  console.error = function (...args) {
    const line = args.map((a) => {
      if (typeof a === 'string') return a;
      try { return JSON.stringify(a); } catch { return String(a); }
    }).join(' ');

    // 腾讯的说法是「鉴权失败：当前产品鉴权失败，请检查KEY及相关信息是否准确。」
    // 以及「鉴权失败：海外图鉴权失败」。只认「鉴权失败」这个前缀就够稳了。
    if (hitText === null && line.includes('鉴权失败')) hitText = line;
    // 「未知错误：获取样式文件失败。」—— 样式下不下来时的原话
    if (styleText === null && line.includes('样式文件')) styleText = line;

    return orig.apply(console, args);
  };

  return {
    hit: () => hitText,
    styleFail: () => styleText,
    stop: () => {
      if (stopped) return;
      stopped = true;
      console.error = orig;
    },
  };
}

/**
 * 观测**瓦片请求**是否真的成功 —— 鉴权失败的第二条证据链。
 *
 * ## 为什么光劫 console 还不够
 *
 * 劫 console 能抓到 SDK 主动喊出来的「鉴权失败：…」。但那句话是 SDK
 * **可选**的：不同版本、不同失败阶段（配置接口 / 瓦片接口 / 样式接口）
 * 会不会打日志并不一致。实测里"Key 被拒"和"页面拿到黑图"之间**不是**
 * 每次都有一句 console.error 夹在中间。
 *
 * 所以补一条**结果证据**：地图能不能拿到瓦片。拿得到 = 真的能用；
 * 一张都拿不到 / 全被拒 = 甭管 console 说没说，图就是黑的。
 *
 * ## 怎么观测
 *
 * 用 `PerformanceObserver` 看 `resource` 条目（浏览器自己记的，
 * 比包 `fetch`/`XMLHttpRequest` 稳 —— 腾讯 SDK 内部用什么发请求
 * 是它的实现细节，包法会随版本失效）。判据：
 *
 *   · 条目名命中瓦片域名（`map.qq.com` / `map.gtimg.com`）
 *   · `responseStatus >= 400`（Key 被拒一般是 401/403）
 *
 * ⚠️ `responseStatus` 不是所有条目都有（跨域资源常为 0），所以**只把
 *    "明确 >=400" 当失败证据**，拿不到状态就当作"没证据"，不倒扣。
 *    宁可漏报也不能把好 Key 冤枉成坏 Key —— 冤枉好 Key 会让用户
 *    去改一个本来正确的配置，那比漏报更难查。
 *
 * ## ★★ 2026-10-12 修：`sawTile()` 原来**返回了却没人用**
 *
 * 原来 `nsInitMap` 只看 `bad()`（HTTP >= 400）。于是
 * **"一张瓦片都没请求过"** 这种情况两条证据都不成立 → 界面照样报
 * 「地图就绪」，而地图是**全白**的。用户看到的就是
 * "一片空白，也不知道为什么"。
 *
 * 现在 `sawTile()` 也用上了：等够时间之后**一张瓦片都没来过**，同样是硬证据。
 * 它和 `bad()` 的区别是：`bad()` 说"来了但被拒"，`!sawTile()` 说"压根没来"。
 * 后者更常见于**这个 Key 没勾 JavaScript API GL**（不是"Key 类型不对"）。
 *
 * @returns {{ bad: () => string|null, sawTile: () => boolean, stop: Function }}
 */
function nsWatchTiles() {
  let badUrl = null;
  let saw = false;
  let obs = null;

  const isTile = (name) => {
    // ⚠️ SDK 脚本自己（`map.qq.com/api/gljs?...`）**不是瓦片**，先排掉。
    //    不排的话 `saw` 一加载就为真，"一张瓦片都没来"这条证据**永远不成立**
    //    —— 又是一个"看起来在检查、其实没检查"。
    //
    // ⚠️ 用 `indexOf` 而不是正则 `/\/api\//`：后者里的 `\/` 紧跟一个 `/`，
    //    会被**闸门的去注释工具**误判成 `//` 行注释，把后半行吃掉
    //    （`scripts/navpack-gate.cjs` 的 `stripComments` 不认正则字面量）。
    //    写成 `indexOf` 既更清楚，也不会踩那个坑。
    if (name.indexOf('/api/') >= 0) return false;
    // 瓦片 CDN（GL 版走 gtimg）
    if (/(^|\.)map\.gtimg\.com\//.test(name)) return true;
    // 老域名上的瓦片 / 样式 / 精灵图 / 字形
    return (
      /(^|\.)map\.qq\.com\//.test(name) &&
      /(tile|vector|style|sprite|glyph|\.pbf|\.png|\.jpg|\.webp)/i.test(name)
    );
  };

  const onEntry = (e) => {
    const list = e.getEntries ? e.getEntries() : [];
    for (const it of list) {
      if (!it || !isTile(it.name || '')) continue;
      saw = true;
      const st = typeof it.responseStatus === 'number' ? it.responseStatus : 0;
      if (st >= 400 && badUrl === null) badUrl = 'HTTP ' + st + ' · ' + it.name;
    }
  };

  try {
    if (typeof PerformanceObserver === 'function') {
      obs = new PerformanceObserver(onEntry);
      // `buffered: true` 不能少 —— 观测器是建图**之后**才挂的，
      // 不带上它就会漏掉建图那一刻已经发出去的那批瓦片请求。
      obs.observe({ type: 'resource', buffered: true });
    }
  } catch (_) {
    // 老浏览器没有这一套 —— 那就只剩 console 那条证据链，不影响主流程
    obs = null;
  }

  return {
    bad: () => badUrl,
    sawTile: () => saw,
    stop: () => { if (obs) { try { obs.disconnect(); } catch (_) {} obs = null; } },
  };
}

/**
 * 加载 SDK → 建图 → **确认地图真的活了**。
 *
 * ## ★★ 2026-10-12 修的核心：建图之后必须验证，不能盲报"就绪"
 *
 * 修之前这里是"建完就宣布成功"：
 *
 *     $('ns-key-gate').classList.add('hidden');   // 关掉 Key 输入层
 *     nsMap = new TMap.Map(...);                  // Key 错 → 黑图
 *     nsStatus('地图就绪');                        // ★ 骗人
 *
 * 而 Key 错的**唯一表现**就是地图一片黑 —— 用户会以为"这个工具坏了"。
 * 现在改成：建图后**同时挂两条证据链**，等 2.5 秒看有没有哪条报出问题：
 *
 *   ① `nsCaptureAuthErrors()` —— SDK 主动喊出「鉴权失败：…」（带腾讯原话）
 *   ② `nsWatchTiles()`        —— 瓦片请求被明确拒绝（HTTP 401/403）
 *
 * 任意一条成立就把 Key 输入层**弹回来**并讲清是哪一种。两条都不成立
 * 才认"地图就绪"。**光靠①是不够的** —— 详见 [nsWatchTiles] 那段注释。
 *
 * ⚠️ 为什么用"等一会儿再看"而不是"建图后同步判断"：
 *    鉴权是 SDK 内部异步发起的（它会去请求瓦片/配置），同步那一刻
 *    什么都还没发生。2.5 秒是实测能拿到报错的时间（本地实测 < 1s 就报）。
 */
function nsInitMap(key) {
  nsStatus('正在加载地图…');

  // ★ 四条证据链都要在**加载之前/建图之前**开好：
  //   ⓪ CSP 违规   —— 浏览器直接把"哪个域被哪条指令挡了"告诉我们（最强）
  //   ① 劫 console —— 抓 SDK 主动喊出来的「鉴权失败：…」
  //   ② 看瓦片     —— 抓"实际上根本没拿到图"这个结果
  //   ③ 没瓦片     —— 抓"压根没请求过"
  const csp = nsWatchCsp();
  const auth = nsCaptureAuthErrors();
  const tiles = nsWatchTiles();
  const finishWatch = () => { csp.stop(); auth.stop(); tiles.stop(); };

  nsLoadMapApi(key)
    .then(() => {
      $('ns-key-gate').classList.add('hidden');
      // 地图真的起来了 —— 把"填坐标模式"那块说明收掉，提示条放回来
      // （用户可能是从那个模式点"重新试一次地图"过来的）
      $('ns-map-fallback').classList.add('hidden');
      $('ns-map-tip').classList.remove('hidden');

      try {
        nsMap = new window.TMap.Map($('ns-map'), {
          // ★★ 2026-10-13：**记住上次看的位置**。
          //
          // 用户的原话："默认给我跳到这个经纬度去,每次重新刷新都要一直拉一直拉,烦死了
          //              21.149328,110.301408"
          //
          // 两件事一起做：
          //   ① 第一次打开 → 用用户给的默认点（他学校那一带）
          //   ② 之后每次打开 → 回到**上次离开时看的地方**（拖动/缩放都会被记住）
          // 只做 ① 的话，用户标到校园另一头、刷新一下又回到原点，还是要拉。
          center: new window.TMap.LatLng(view.lat, view.lng),
          zoom: view.zoom,
          // ★★★ 2026-10-13：**标注台也换成 3D 了**。
          //
          // 用户的原话："我的标注工作做得太辛苦了，而且还不能标注 3D 建筑"。
          //
          // 说得对。在这之前标注台是**平面地图**，而用户要描的是**三维建筑** ——
          // 在平地上凭想象描一个立体楼的外墙，当然辛苦、当然描不准。
          //
          // 官方《2D/3D模式切换》+《配置矢量底图》两份 demo 给了答案：
          //   `baseMap.features` 带 `building3d` → 底图**自己渲染真实的三维建筑**
          //   `setViewMode('3D')` + `setPitch()` → 把镜头压下去看到立体
          //
          // 于是用户是**照着那栋真楼描**，不是在空地上猜。
          //
          // ⚠️ `zoom` 必须 ≥ `buildingRange` 的下限，否则**一栋楼都不显示**
          //    （默认下限 16.5，第一版写了 16 —— 差 0.5 级，什么都没有，而且不报错）。
          baseMap: {
            type: 'vector',
            features: ['base', 'building3d', 'building2d', 'point', 'label'],
            buildingRange: [15, 20],
          },
        });
        // ★ 和官方 demo 一模一样的两个开关
        nsMap.setViewMode('3D');
        nsMap.setPitch(55);
        // 用户拖到哪儿、缩到几级，都记下来（下次打开直接回到这儿）
        nsMap.on('moveend', nsSaveView);
        nsMap.on('zoomend', nsSaveView);
      } catch (e) {
        finishWatch();
        nsMapFailed('建图直接抛了异常：' + e.message, 'SDK 版本或参数不兼容');
        return;
      }

      nsWireMapEvents();
      nsWireToolbar();
      nsShieldFromMap();
      // ★ 地图回来了，把那一圈浮层放回来（走「先不用地图」时收掉过）
      nsSetMapChrome(true);
      nsSyncMarkers();
      // ★★ 2026-10-13 修：**地图就绪之后必须再画一次 3D 图层**。
      //
      // 症状：3D 楼块一个都不出现（`nsShapeLayer` 一直是 null），
      //       而控制台**没有任何报错** —— 最难查的那一类。
      //
      // 根因是**时序**：`nsBoot()` 里先 `nsWireKey()`（它异步加载 SDK），
      // 紧接着就 `nsRenderAll()` —— 那一刻 `nsMap` 还是 null，
      // `nsSyncShapeLayer()` 第一行 `if (!nsMap) return` 直接退出了。
      // 而地图加载完的那条路（这里）原来只补了 `nsSyncMarkers()`，
      // **没有补 3D 图层** —— 于是那一层永远画不出来。
      //
      // ⚠️ 教训：**异步就绪之后要重画的东西，一处都不能漏**。
      //    修法是直接把"全部重画"再走一遍（`nsRenderAll`），
      //    而不是在这里手写一句 —— 手写的话，下次再加一种图层还会漏。
      nsRenderAll();
      ns.mapReady = true;
      nsStatus('正在确认地图真的画出来了…');

      // ★★ 最后一道：等 SDK 把结论**用四条证据之一**说出来。
      //
      //    ⓪ **CSP 违规** —— 浏览器说"这个域被这条指令挡了"（与 Key 无关）
      //    ① SDK 主动喊「鉴权失败：…」（带腾讯原话）
      //    ② 瓦片来了但被拒（HTTP 401/403）
      //    ③ **一张瓦片都没来过** ← 2026-10-12 补的
      //
      //    ⚠️ 第 ③ 条是用户报"一片空白"时**先**补的：原来只判 ②，
      //      而"Key 类型不对"这种情况**瓦片请求压根不会发出去**，② 不成立
      //      → 界面照样报「地图就绪」，用户对着白屏发呆。
      //
      //    ⚠️⚠️ 第 ⓪ 条是用户**贴出 console 之后**补的，而且它必须排在**最前**：
      //      真正的原因是 CSP 漏了 `mapstyle.qpic.cn`，可 ①②③ 一条都指不出来，
      //      最后只能猜成"Key 类型不对" —— 让用户去改一个**本来完全正确**的配置。
      //      **猜错的原因比不给原因更坏。**
      setTimeout(() => {
        const blocked = csp.first();   // 证据⓪：CSP 挡了某个域
        const said = auth.hit();       // 证据①：SDK 自己承认了
        const styleFail = auth.styleFail(); // 证据①'：SDK 说样式下不下来
        const tile = tiles.bad();      // 证据②：瓦片被明确拒绝
        const saw = tiles.sawTile();   // 证据③的原料：到底有没有瓦片来过
        const box = $('ns-map');
        const bw = box ? box.clientWidth : 0;
        const bh = box ? box.clientHeight : 0;
        finishWatch();

        // ---- 证据⓪：CSP。**必须排在最前** —— 它最具体，而且不是 Key 的问题。
        if (blocked) {
          ns.mapReady = false;
          nsMapFailed(
            '地图被浏览器的安全策略挡住了',
            blocked,
            null,
            '页面的 CSP 白名单里漏了一个地图要用到的域名 —— <b>这一条和 Key 无关</b>，' +
              '换成再正确的 Key 也一样白。<br />' +
              '这属于工具的配置问题：<b>请把上面这句原话发给开发者</b>' +
              '（它带着"哪个域 + 哪条指令"，照着补进 <code>nav-studio.html</code> 的 CSP 就行）。',
          );
          return;
        }

        // ---- 证据①：SDK 自己说鉴权失败（带腾讯原话，排查最有用）
        if (said) {
          ns.mapReady = false;
          nsMapFailed('地图数据没能加载', 'Key 鉴权失败', said);
          return;
        }
        if (tile) {
          ns.mapReady = false;
          nsMapFailed('地图数据没能加载', 'Key 鉴权失败（瓦片被拒）', tile);
          return;
        }

        // ---- 证据①'：样式文件下不下来（原因待定，但症状明确）
        if (styleFail) {
          ns.mapReady = false;
          nsMapFailed(
            '地图样式没能加载',
            '样式文件下不下来',
            styleFail,
            '腾讯的地图样式文件没取到。常见原因：网络挡了腾讯的 CDN，' +
              '或者浏览器插件拦了请求。<b>这一条和 Key 无关</b>。<br />' +
              '按 F12 打开控制台，把里面那条红色的 CSP / 网络报错发给我。',
          );
          return;
        }

        // ★ 证据③：容器被压成了 0 —— 这不是 Key 的问题，是**布局**的问题。
        //    必须先判它，否则会冤枉成一个好 Key（"宁可漏报也不能冤枉"）。
        if (bw < 120 || bh < 120) {
          ns.mapReady = false;
          nsMapFailed(
            '地图区域太小，画不出来',
            `地图容器只有 ${bw}×${bh} 像素`,
            null,
            '窗口太窄了 —— 三栏布局（左列表 / 中地图 / 右编辑）一共要 520px 以上的固定宽度。' +
              '把浏览器窗口拉宽，或者用手机横屏。<br />' +
              '（这一条与 Key 无关，换 Key 也没用。）',
          );
          return;
        }

        // ★ 证据③本体：容器够大、也没报错，但**一张瓦片都没来**。
        if (!saw) {
          ns.mapReady = false;
          nsMapFailed(
            '地图一片空白',
            '一张地图瓦片都没加载出来',
            null,
            '最常见的原因是<b>这个 Key 没勾「JavaScript API GL」</b> —— ' +
              '怎么判断：把这个 Key 粘到 <a href="https://lbs.qq.com/dev/console/key/manage" ' +
              'target="_blank" rel="noopener noreferrer">lbs.qq.com 控制台</a> 里看一眼，' +
              '没勾就补勾（同一把 Key，不用重新申请）。<br />' +
              '其次：网络挡了 <code>map.qq.com</code> / <code>map.gtimg.com</code>；' +
              '或者 Key 配了来源限制而当前域名不在白名单里。<br />' +
              '<b>拿不准的话</b>：按 F12 打开控制台，把红色报错整段发给我。',
          );
          return;
        }

        // 四条证据都没有 —— 那就是真的好了
        nsStatus('地图就绪');
      }, 3000);
    })
    .catch((err) => {
      finishWatch();
      const why = {
        NETWORK: '脚本没下载下来 —— 多半是网络挡了 map.qq.com，或者浏览器插件拦了',
        TIMEOUT: '加载超时 —— 网络太慢，或者这个 Key 没勾「JavaScript API GL」',
        SDK_INCOMPLETE: '脚本下下来了，但 SDK 没装配完（`TMap.Map` 不存在）—— ' +
          '多半是 Key 的类型不对',
      }[err.message] || '未知原因';
      nsMapFailed('地图没能加载', why);
    });
}

/**
 * 地图起不来时的**统一收尾**：退回 Key 输入层，并把"是哪一种错"讲清楚。
 *
 * ⚠️ 一定要把 Key 输入层**重新显示出来**：原来它在 `.then()` 里被隐藏了，
 *    失败后再不显示，用户就没有任何地方能改 Key —— 那个页面会变成死路。
 *
 * ## ★★ 2026-10-12 加：`fixHtml` —— 不同原因的**修法完全不同**
 *
 * 原来这里按"是不是鉴权失败"分两种话。但这一轮新识别出两种**跟 Key 无关**的
 * 原因（地图容器被压成 0 尺寸 / 一张瓦片都没来），把它们也塞进"去改 Key"
 * 那段话里，用户会去改一个本来正确的配置 —— 那比不说更坏。
 *
 * 所以调用方可以直接给一段"该怎么办"，覆盖掉默认话术。
 *
 * @param {string} headline  大标题（干什么失败了）
 * @param {string} why       哪一种原因
 * @param {string} [detail]  SDK 自己吐的那句话（有就原样给用户看）
 * @param {string} [fixHtml]  "该怎么办"那一段（HTML）。不给就按 `why` 猜。
 */
function nsMapFailed(headline, why, detail, fixHtml) {
  nsStatus(headline + '：' + why);
  // ⚠️ 地图没起来时同样要收起那一圈 —— 否则它们被 Key 层盖着，
  //    用户看到的是"有工具条但点不动"（而不是"这里还没有地图"）。
  nsSetMapChrome(false);

  // ★ 用户已经选了"不用地图"—— 就**别再弹回来**。
  //   不判这一下的话，他刚点完"先不用地图"，3 秒后 Key 层又盖上来，
  //   看起来就是"那个按钮没用"。
  if (ns.noMapMode) return;

  const box = $('ns-key-gate');
  if (!box) return;
  box.classList.remove('hidden');

  const h3 = box.querySelector('h3');
  const p = box.querySelector('p');
  if (h3) h3.textContent = headline;

  if (p) {
    // 调用方给了就用它的；否则按原因挑一段默认话术
    const body = fixHtml
      ? fixHtml
      : why.indexOf('鉴权') >= 0
        ? '这个 Key 被腾讯拒了。请确认：① 它是 <b>JavaScript API GL</b> 类型' +
          '（Key 本身不用换，去 lbs.qq.com 控制台给它<b>补勾</b>「JavaScript API GL」）；' +
          '② 已勾选「JavaScript API GL」这一项服务；' +
          '③ 如果 Key 配了来源限制，当前域名/本机页面要在白名单里。'
        : `${why}。<br />请确认：① 这个 Key 在控制台勾了 <b>JavaScript API GL</b>；` +
          '② 网络能打开 map.qq.com。改完再点一次下面的按钮。';

    p.innerHTML =
      body +
      (detail ? `<br /><span class="ns-err-detail">腾讯原话：${nsEsc(detail)}</span>` : '');
  }

  // ★ 失败时把按钮文案改成"重试" —— 用户刚改完 Key，要的就是再点一次
  const btn = $('ns-btn-mapkey');
  if (btn) btn.textContent = '重试';
}

/**
 * 浮在地图上的那些控件，**不许把点击漏给地图**（2026-09-13 实测抓到的真 bug）。
 *
 * ## 现象
 *
 * 在「加点」工具下点一下工具条上的「加点」按钮 —— **会凭空多出一栋楼**。
 *
 * ## 原因
 *
 * 地图 SDK 把监听挂在**容器**上，而我们的工具条 / 批量条 / 右上角那些按钮
 * 都是绝对定位在容器**里面**的。于是点按钮时：
 *
 * ```
 *   ① 按钮自己的处理器跑   → 工具从"选择"切成"加点"
 *   ② 同一次点击继续冒泡到容器 → SDK 派发 click → 现在的工具是"加点"
 *                              → 在按钮那个位置建了一栋楼
 * ```
 *
 * 顺序恰好让"刚切过去的工具"立刻被执行一次 —— 所以每次点工具条都会多一栋。
 *
 * ## 两道保险
 *
 * ```
 *   ① `nsShieldFromMap()` 在这些容器上 stopPropagation（便宜，能挡大多数情况）
 *   ② `nsClickOnOverlay()` 在地图处理器里再看一眼"这一下是不是落在浮层上"
 *      —— ②不依赖 SDK 用捕获还是冒泡，是真正定死的那个
 * ```
 *
 * ⚠️ 只写 ① 是不够的：SDK 万一用**捕获**阶段监听，容器会先于我们的元素收到事件，
 *    `stopPropagation` 就来不及了。两道的成本都很低，就都留着。
 */
function nsShieldFromMap() {
  ['ns-toolbar', 'ns-batch-bar', 'ns-map-tools', 'ns-map-tip', 'ns-shape-bar'].forEach((id) => {
    const el = $(id);
    if (!el || el.dataset.shielded) return;
    el.dataset.shielded = '1';
    ['mousedown', 'mouseup', 'click', 'dblclick', 'touchstart', 'touchend'].forEach((t) => {
      el.addEventListener(t, (ev) => ev.stopPropagation());
    });
  });
}

/**
 * 地图区那一圈浮层控件（工具条 / 批量条 / 右上角两个按钮 / 底部提示）的显隐。
 *
 * ## ★★★ 为什么要有它（用户报的"左上角那个功能不可用"）
 *
 * 用户的原话：
 *
 * > "左上角那个选择加点描轮廓设原点的那个功能**不可用**。"
 *
 * 查下来，那套工具**本身是好的** —— 两条 e2e 专门验过它：
 * ```
 *   tmp/e2e-studio-editor.cjs   接线 / 高亮 / 逐个切工具 / 加点真的建楼
 *   tmp/e2e-studio-boot.cjs     走**真实**的"配 Key → 加载 SDK → 建图"那条链
 * ```
 * 问题是它们**在不该出现的时候也画在那儿**：
 *
 * ```
 *   走「先不用地图」那条路 → .ns-gate 盖住整个地图区（z-index 10）
 *   而工具条的 z-index 是 6   →  **看得见、点不动**
 * ```
 *
 * 那正是"不可用"最典型的样子。而且退一步说：**没有底图的时候，
 * 「加点 / 描轮廓 / 设原点」本来就没有意义** —— 它们该收起来，
 * 而不是留在那儿等人点、点完没反应。
 *
 * ⚠️ 所以这不是"把工具条藏起来糊弄过去"：地图回来的时候它们要**原样回来**
 *    （见 nsInitMap 成功回调里那句 `nsSetMapChrome(true)`）。
 */
function nsSetMapChrome(on) {
  ['ns-toolbar', 'ns-batch-bar', 'ns-map-tools', 'ns-map-tip', 'ns-shape-bar', 'ns-mark-layer'].forEach((id) => {
    const el = $(id);
    if (!el) return;
    if (id === 'ns-batch-bar') {
      // ⚠️ 批量条有它**自己的**显隐规则（只在「批量」工具下出现）——
      //    这里只把"地图这一层在不在"记在 dataset 上，由 nsRenderBatchBar 自己判，
      //    不然就把那条规则覆盖掉了。
      el.dataset.mapOff = on ? '' : '1';
      nsRenderBatchBar();
      return;
    }
    el.classList.toggle('hidden', !on);
  });
}

/**
 * 这一下点击是不是落在"浮在地图上的控件"上？是的话就不该当地图操作。
 *
 * ## 为什么不能只看 `originalEvent.target`
 *
 * 第一版就是这么写的，**没挡住**（实测：点一下工具条按钮仍然多出一栋楼）。
 * 两个原因，都值得记：
 *
 * ```
 *   ① SDK 在**捕获阶段**就收了事件 —— 我们元素上的 stopPropagation 来不及
 *      （捕获是自上而下，容器先收到）
 *   ② 于是它派发的 click 事件里 `originalEvent.target` 是**容器**，
 *      而不是那个按钮 —— `closest('#ns-toolbar')` 永远匹配不到
 * ```
 *
 * ## 所以按**坐标**判
 *
 * 地图事件带 `ev.point`（相对容器的像素，和 `projectToContainer` 同坐标系，
 * 实测两者对同一点都给 440,411）。把浮层控件的矩形换算到同一个坐标系里比一下 ——
 * 这条路**不依赖 SDK 用捕获还是冒泡、也不依赖它怎么填 originalEvent**。
 */
function nsClickOnOverlay(ev) {
  const t = ev && ev.originalEvent && ev.originalEvent.target;
  if (t && t.closest && t.closest('#ns-toolbar, #ns-batch-bar, #ns-map-tools, #ns-map-tip, #ns-shape-bar')) {
    return true;
  }
  const pt = ev && ev.point;
  const host = $('ns-map');
  if (!pt || !host) return false;
  const base = host.getBoundingClientRect();

  // ★★ 标注层要**逐个标注**判，不能把整层算进去（2026-10-13 深夜踩的）。
  //
  // ⚠️ `#ns-mark-layer` 是 `inset: 0` **铺满整个地图容器**的 —— 把它当成
  //    一个"浮层控件"按矩形判的话，**任何坐标都落在它里面**，
  //    于是所有地图点击（加点、描轮廓、设原点）全部被吞掉。
  //    实测就是这么把"描轮廓"整组测试干掉的。
  //
  // 层本身是 `pointer-events: none`（不吃事件），真正要排除的是里面那些
  // `.ns-mark` —— 点它们不该顺带在这一点新建一栋楼。
  const marks = document.querySelectorAll('#ns-mark-layer .ns-mark');
  for (let i = 0; i < marks.length; i += 1) {
    const r = marks[i].getBoundingClientRect();
    if (!r.width || !r.height) continue;
    const mx0 = r.left - base.left;
    const mx1 = r.right - base.left;
    const my0 = r.top - base.top;
    const my1 = r.bottom - base.top;
    if (pt.x >= mx0 && pt.x <= mx1 && pt.y >= my0 && pt.y <= my1) return true;
  }
  return ['ns-toolbar', 'ns-batch-bar', 'ns-map-tools', 'ns-map-tip', 'ns-shape-bar'].some((id) => {
    const el = $(id);
    if (!el || el.classList.contains('hidden')) return false;
    const r = el.getBoundingClientRect();
    if (!r.width || !r.height) return false;
    // `ev.point` 是相对容器的像素 → 把视口坐标的矩形也换算过来
    const x0 = r.left - base.left;
    const x1 = r.right - base.left;
    const y0 = r.top - base.top;
    const y1 = r.bottom - base.top;
    return pt.x >= x0 && pt.x <= x1 && pt.y >= y0 && pt.y <= y1;
  });
}

/**
 * 手的抖动阈值（像素）。
 *
 * 按下之后移动**超过**这么多，才算"我在拖这栋楼"；不到就算"点了一下"。
 * 4px 是常见的手抖范围，比它大一点点就能把"点击"和"拖拽"分开 ——
 * 而这个区分很重要：它决定了要不要关掉地图的拖拽能力（见 [ns.press]）。
 */
const NS_ARM_PX = 4;

/**
 * 把"正在拖"这件事收干净 —— **无论如何都要把地图的拖拽能力还回去**。
 *
 * ## 它是这一轮最重要的一个函数
 *
 * 用户的原话：**"视角锁定以后就动不了了"**。
 *
 * 根因就是这个复位**只挂在一条会丢的事件上**（SDK 的 `mouseup`）。现在：
 *
 * ```
 *   谁调它都行（mousedown 开头先自愈、mouseup、blur、切工具、Esc）
 *   它自己是幂等的（没在拖也照样把 draggable 设回 true）
 * ```
 *
 * ⚠️ `setDraggable(true)` **不能省** —— "没在拖"不等于"地图的拖拽是开着的"：
 *    上一次程序中断可能把开关留在了 false 上，而那时 `ns.dragging` 已经清空，
 *    光看状态是完全正常的。**开关归位比状态归位重要。**
 */
function nsReleaseDrag() {
  ns.press = null;
  ns.dragging = null;
  try { if (nsMap) nsMap.setDraggable(true); } catch (e) { /* 老 SDK 没这个能力 */ }
}

/**
 * 松开鼠标 —— **地图点击和"拖完一栋楼"共用这一个出口**。
 *
 * ⚠️ 它会被**两种**事件调到：
 *    · 地图事件（有 `ev.point`）—— 正常那条
 *    · `document` / `pointerup` 兜底（没有 `ev.point`，甚至没有 ev）
 *    所以**不能依赖 ev 的任何字段**，判断一律用 [ns.press]。
 *
 * ★ "点击选中"在这里做，而不是只等 SDK 的 `click`：
 *   用户报"点击一个建筑，它好像也没有选择"，而 `click` 那一路要过 SDK 的
 *   手势识别（它可能因为各种原因不派发）。`mouseup` 是更靠底层的信号。
 *   （`click` 那一路**保留**着当兜底，两条路都走 `nsSelect`，而它是幂等的。）
 */
function nsMouseUp() {
  const p = ns.press;
  if (!p) { nsReleaseDrag(); return; }        // 兜底那两处走到这里就是空转
  const id = p.id;
  const wasDrag = !!p.armed || !!ns.dragging;
  nsReleaseDrag();
  const b = ns.buildings.find((x) => x.id === id);

  if (!wasDrag) {
    if (b) nsSelect(id, false);              // 没拖动 = 一次"点击选中"
    return;
  }
  nsSave();
  nsRenderList();
  nsStatus('已把「' + ((b && b.name) || '（未命名）') + '」挪到 '
    + (b ? nsFmtCoord(b.lat, b.lng) : '') + '。轮廓会跟着一起移。');
}

function nsWireMapEvents() {
  if (!nsMap) return;

  // ------------------------------------------------------------------ 按下
  //
  // ★★★ "按住标记拖 = 把它挪到别处"（2026-10-13）。
  //
  // 用户的原话："尽量可以在地图上就对模型进行移动" —— 在这之前只能改
  // 右边那个坐标格里的数字，而"这栋楼其实在那边"是**眼睛看得出来、手也想直接拖**的事。
  //
  // 后来用户专门夸了这一条：
  //   "那个地图上面那个选点功能挺好用的，就是可以拖动那个点进行重新定位，
  //    这个挺不错的。"
  // —— 所以这个能力**要保住**，这一轮只是把它的副作用修掉。
  //
  // ⚠️⚠️ 这里**不关地图的拖拽开关** —— 关的时机推迟到 `mousemove` 超过阈值那一刻。
  //     用户报"视角锁定 / 点了建筑没选中"，就是"一按下就关"造成的，两条都算：
  //
  //     · 一按下就关，"单击选中"也会把它关一次；而恢复只挂在 SDK 的 mouseup 上，
  //       那个事件一丢（鼠标移出容器才松手、窗口失焦、触摸板手势），
  //       地图就**永久拖不动**，而且看不出原因
  //     · 一按下就关，还会打断 SDK 自己的手势识别 → 它不再派发 click →
  //       表现就是"我点击一个建筑，它好像也没有选择"
  nsMap.on('mousedown', (ev) => {
    nsReleaseDrag();                   // ★ 自愈：上一次万一半路断了，先复位
    if (nsClickOnOverlay(ev)) return;  // 点的是浮层上的控件，不是地图
    if (ns.tool !== 'select') return;
    const hit = nsHitTest(ev.point);
    if (!hit) return;
    ns.press = { id: hit.id, x: ev.point.x, y: ev.point.y, armed: false };
  });

  nsMap.on('mousemove', (ev) => {
    const p = ns.press;
    if (!p) return;
    if (!p.armed) {
      const dx = ev.point.x - p.x;
      const dy = ev.point.y - p.y;
      if (dx * dx + dy * dy < NS_ARM_PX * NS_ARM_PX) return;   // 手抖不算拖
      p.armed = true;
      ns.dragging = { id: p.id, moved: true };
      try { nsMap.setDraggable(false); } catch (e) { /* 老 SDK 没这个能力就别关 */ }
    }
    const b = ns.buildings.find((x) => x.id === p.id);
    if (!b) { nsReleaseDrag(); return; }
    b.lat = Number(ev.latLng.getLat().toFixed(6));
    b.lng = Number(ev.latLng.getLng().toFixed(6));
    nsSyncMarkers();          // 标记实时跟手
    nsRenderShapeHint();      // 底面轮廓那一层也跟着走
    nsRenderEditor();         // 坐标格里的数字同步变，用户能看着数字挪
  });

  // ---- 松开：**地图点击和"拖完一栋楼"共用这一个出口**
  nsMap.on('mouseup', nsMouseUp);
  // ⚠️⚠️ 三道兜底，缺一条地图就可能永久锁死（理由见上面那段）：
  //     ① `document` 上的 mouseup —— 鼠标在容器外松开
  //     ② `pointerup`          —— 触摸板 / 触摸屏的序列未必派发 mouseup
  //     ③ 窗口失焦             —— 切到别的应用之后松手
  document.addEventListener('mouseup', nsMouseUp);
  document.addEventListener('pointerup', nsMouseUp);
  window.addEventListener('blur', nsReleaseDrag);

  // ★ 地图一动手，标注要跟着走（2026-10-13 深夜）。
  //   ⚠️ 只重排、不重建 DOM —— 拖动时每帧都重建会卡（几十个标注就看得出来）。
  ['move', 'pan', 'zoom', 'rotate', 'pitch', 'idle', 'bounds_changed',
    'moveend', 'zoomend'].forEach((n) => {
    try { nsMap.on(n, nsPlaceMarks); } catch (e) { /* 老 SDK 少一两个事件而已 */ }
  });
  // Esc：正在拖就先收尾；否则**取消选择**（用户明确要的，见 nsDeselect）
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape') return;
    // ⚠️ 确认面板开着的时候，Esc 归它 —— 不然后面那句会把用户的选中也取消掉，
    //    表现是"关掉面板之后右边那栏也空了"，像是把东西弄丢了。
    const rv = $('ns-poi-review');
    if (rv && !rv.classList.contains('hidden')) return;
    if (ns.press || ns.dragging) { nsReleaseDrag(); return; }
    nsDeselect();
  });

  // ------------------------------------------------------------------ 单击
  //
  // ★★★ 按**当前工具**分流（2026-10-13）。
  //
  // 上一版这里是两个布尔（`pickNew` / `shapeTargetId`）互相抢，
  // 而"点空白处 = 把坐标给当前选中那条"这个行为**没有任何提示** ——
  // 用户点一下发现楼跑了，还不知道是自己碰的。
  nsMap.on('click', (ev) => {
    // ★ 浮层上的点击**不是地图操作** —— 少了这一句，在「加点」工具下
    //   点一下工具条按钮就会就地多出一栋楼（实测抓到的）。
    if (nsClickOnOverlay(ev)) return;
    const lat = ev.latLng.getLat();
    const lng = ev.latLng.getLng();

    // ① 描轮廓（点的是**顶点**，不是楼的位置）
    //    ⚠️ 必须排在最前面 —— 排在后面的话，描每一笔都会顺带改掉楼的位置。
    if (nsShapeTargetId || ns.tool === 'draw') {
      if (!nsShapeTargetId) {
        nsToast('先选一栋楼，再开始描轮廓');
        return;
      }
      nsShapeAddPoint(lat, lng);
      return;
    }

    // ② 设原点
    if (ns.pickMode === 'origin' || ns.tool === 'origin') {
      ns.origin = { lat, lng };
      ns.pickMode = 'building';
      $('ns-pack-origin').value = nsFmtCoord(lat, lng);
      nsSave();
      nsRenderAll();
      nsToast('标定原点已设为 ' + nsFmtCoord(lat, lng));
      return;
    }

    // ③ 加点：在那个位置新建一栋
    if (ns.tool === 'add') {
      nsCreateAt(lat, lng, (ev.poi && ev.poi.name) || '');
      return;
    }

    // ③.5 标注：在那个位置放一个（2026-10-13 深夜）
    //
    // ⚠️ 排在"命中楼栋"**之前** —— 否则在楼上放个门牌会被当成"选中那栋楼"。
    if (ns.tool === 'mark') {
      nsAddMark(ns.markKind || 'text', lat, lng);
      return;
    }

    // ④ 选择 / 批量：**命中哪一栋就操作哪一栋**
    //    （不再有"点空白 = 改当前那条的坐标"这件事）
    const hit = nsHitTest(ev.point);
    if (!hit) {
      if (ns.tool === 'select') {
        // ★★ 点空白 = **取消选择**。
        //
        // 用户的原话："我没办法取消选择一个建筑，这也是一个非常大的问题。"
        //
        // 在这之前点空白只是弹一句"这一点附近没有楼" —— 于是选了一栋之后
        // **没有任何办法退出选中状态**（右边那栏一直占着、地图上那一栋一直是高亮的绿色）。
        if (nsIsSelected()) nsDeselect();
        else nsStatus('这一点附近没有楼。要在这里新建一栋，先切到地图左上角的「加点」。');
      }
      return;
    }
    if (ns.tool === 'batch') {
      nsBatchToggle(hit.id);
      return;
    }
    nsSelect(hit.id, false);
  });

  // ------------------------------------------------------------------ 双击
  //
  // ⚠️ 双击的语义**跟着工具走**：
  //    选择态下双击 = "飞过去把它放大看清"（用户双击一个标记，想要的是看清它）；
  //    加点态下才是"在这里建一栋"。
  //    上一版双击**永远**是新建 —— 于是在选择态下想放大看一栋楼，
  //    会多出一条空楼，而用户完全不知道那是哪一下点出来的。
  nsMap.on('dblclick', (ev) => {
    if (nsClickOnOverlay(ev)) return;
    const hit = nsHitTest(ev.point);
    if (ns.tool === 'add') {
      nsAddBuilding(ev.latLng.getLat(), ev.latLng.getLng());
      return;
    }
    if (hit) {
      nsSelect(hit.id, true);
      return;
    }
    if (ns.tool === 'select') {
      // 双击空白：就地放大一级（地图自己的行为已经做了，这里只提示一下）
      nsStatus('双击标记 = 飞过去放大看它。要新建楼请切到地图左上角的「加点」。');
    }
  });
}

const nsBuildings = () => ns.buildings;

/** 在地图上重建全部标记（增删改之后调）。 */
function nsSyncMarkers() {
  if (!nsMap || !window.TMap) return;

  const TMap = window.TMap;
  const alive = new Set();

  ns.buildings.forEach((b, i) => {
    alive.add(b.id);
    const pos = new TMap.LatLng(b.lat, b.lng);
    const label = `${i + 1}. ${b.name || '（未命名）'}`;

    let mk = nsMarkers.get(b.id);
    if (!mk) {
      mk = new TMap.MultiMarker({
        map: nsMap,
        styles: {
          def: new TMap.MarkerStyle({
            width: 22, height: 32, anchor: { x: 11, y: 32 },
            src: 'data:image/svg+xml;base64,' + nsPinSvg('#185fa5', '#ffffff'),
          }),
          active: new TMap.MarkerStyle({
            width: 30, height: 42, anchor: { x: 15, y: 42 },
            src: 'data:image/svg+xml;base64,' + nsPinSvg('#0f6e56', '#ffffff'),
          }),
        },
        geometries: [],
      });
      nsMarkers.set(b.id, mk);
    }
    mk.setGeometries([{
      id: b.id,
      styleId: b.id === ns.activeId ? 'active' : 'def',
      position: pos,
      properties: { title: label },
    }]);
  });

  // 删掉已经不存在的
  nsMarkers.forEach((mk, id) => {
    if (!alive.has(id)) {
      mk.setMap(null);
      nsMarkers.delete(id);
    }
  });

  // 原点
  const originPos = ns.origin ? new TMap.LatLng(ns.origin.lat, ns.origin.lng) : null;
  if (originPos) {
    if (!nsOriginMarker) {
      nsOriginMarker = new TMap.MultiMarker({
        map: nsMap,
        styles: {
          def: new TMap.MarkerStyle({
            width: 26, height: 36, anchor: { x: 13, y: 36 },
            src: 'data:image/svg+xml;base64,' + nsPinSvg('#a32d2d', '#ffffff'),
          }),
        },
        geometries: [],
      });
    }
    nsOriginMarker.setGeometries([{ id: 'origin', styleId: 'def', position: originPos }]);
  } else if (nsOriginMarker) {
    nsOriginMarker.setMap(null);
    nsOriginMarker = null;
  }
}

/**
 * 一枚 map pin 的 SVG（转 base64 给 MarkerStyle 用）。
 *
 * ⚠️ `btoa` 只吃 Latin-1，中文会抛 `InvalidCharacterError`。
 *    这里拼的都是 ASCII（颜色 + 路径数字），所以安全 ——
 *    但**别往这个函数里加中文**（楼名标签是用 `properties.title` 走的，不在这里画）。
 */
function nsPinSvg(fill, stroke) {
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" width="30" height="42" viewBox="0 0 30 42">` +
    `<path d="M15 41C15 41 29 24 29 15A14 14 0 1 0 1 15C1 24 15 41 15 41Z" ` +
    `fill="${fill}" stroke="${stroke}" stroke-width="2"/>` +
    `<circle cx="15" cy="15" r="5" fill="${stroke}"/></svg>`;
  return btoa(svg);
}

/* ------------------------------------------------------------------ 从 OSM 导入建筑 */

/**
 * **从 OpenStreetMap 导入建筑轮廓**（2026-10-13）。
 *
 * ## ★★★ 这是这一轮真正的答案
 *
 * 用户前后问了四次"能不能从地图里直接提取、而不是我挨个重画"。走通过的路：
 *
 * ```
 *   腾讯底图的建筑几何      ✗ 私有编码，且**扒商业地图数据违反其条款**（会让用户自己的 Key 被封）
 *   政府开放数据            △ 要申请，且不一定覆盖湛江这一带
 *   ★ OpenStreetMap         ✓ **公开数据、协议允许（ODbL）、带真名字、带几何**
 * ```
 *
 * 实测广东海洋大学湖光校区那一带：**155 栋建筑，全部带几何**，其中 20 栋带名字
 * （广东海洋大学教学主楼 / 图书馆 / 体育馆 / 钟海楼 / 第一食堂 / 兴教楼 / 校医院 …）。
 *
 * ## 为什么这件事在**浏览器里**就能做
 *
 * Overpass API 的 CORS 是**全开**的（实测 `Access-Control-Allow-Origin: *`，
 * 且允许 POST）—— 不需要任何后端中转，标注台直接 fetch 就行。
 *
 * ## ⚠️ 署名是**协议要求**，不是客套
 *
 * ODbL 要求署名。所以界面上（导入按钮下面那段）写了
 * "© OpenStreetMap contributors" —— **别删**。
 *
 * ## 合并策略：按 OSM id 去重
 *
 * 用户可能导一次、过两个月再导一次（校园新盖了楼）。所以每个导入的楼都带
 * `osm` 字段（OSM 的 way id）：
 * ```
 *   已经有的（同 osm id）  →  跳过，**保留用户改过的名字和类别**
 *   新的                  →  加进来
 * ```
 * ⚠️ 不能"重新导就整体覆盖" —— 用户辛苦改的名字和分类会被冲掉。
 */

/** Overpass 的实例（一个不通换下一个 —— 实测 `overpass-api.de` 会 504）。 */
const NS_OVERPASS = [
  'https://overpass.kumi.systems/api/interpreter',
  'https://overpass-api.de/api/interpreter',
];

/**
 * "服务端到上游这条路刚才是断的" —— 本次会话的短路开关（2026-10-13）。
 *
 * ⚠️ 为什么要有它：Overpass 会**整体拥堵**（实测四个镜像同时挂住 100 秒全超时），
 *    而服务端失败一次要**100 秒**。用户拖到第二个没烘焙过的校区再点一次，
 *    又是 100 秒 —— 而结果多半还是失败。
 *
 * ⚠️ **它不代表"这条路永远不通"。** 实测拥堵过去之后，同一个接口 6.2 秒就通了。
 *    所以它**只活在这一个页面会话里**（不是 localStorage）：
 *    重新打开页面就再试一次。存进 localStorage 会让"上游恢复了"
 *    也永远不再走那条路 —— 那是把一个暂时的现象固化成永久的。
 */
let nsServerDown = false;

/**
 * 从 OSM 的标签**猜类别**。
 *
 * ★ 这一步是"用户只需要确认"的关键：155 栋楼里 135 栋没名字，
 *   但它们的 `building=*` 标签仍然有信息（`university` / `apartments` / `dormitory`…）。
 *   猜对了用户就不用改，猜错了点一下色块就行 —— 比"一栋一栋从空开始"省太多。
 *
 * ⚠️ 顺序有讲究：**具体的先判**。"广东海洋大学图书馆"里既有"大学"又有"图书馆"，
 *    先判 teaching 的话它会变成教学楼。
 */
function nsGuessType(tags) {
  const t = tags || {};
  const name = String(t.name || '') + String(t['name:zh'] || '');
  const kind = [
    t.building, t.amenity, t.leisure, t.shop, t.office, t.healthcare, t.tourism,
  ].filter(Boolean).join(' ');

  if (/图书馆|library/i.test(name + ' ' + kind)) return 'library';
  if (/食堂|饭堂|餐厅|canteen|restaurant|food/i.test(name + ' ' + kind)) return 'canteen';
  if (/体育馆|体育场|操场|球场|游泳|sport|stadium|pitch|gym/i.test(name + ' ' + kind)) return 'sport';
  if (/宿舍|公寓|dorm|apartment|residential/i.test(name + ' ' + kind)) return 'dorm';
  if (/门$|校门|大门|gate|entrance/i.test(name)) return 'gate';
  if (/行政|办公|office|admin|hospital|校医院|医院/i.test(name + ' ' + kind)) return 'office';
  // ★ 2026-10-13 补的两类。
  //
  // ⚠️ 判据里 `arts` 那一段是**从实测里来的**：湖光校区那 20 栋带名字的楼里
  //    有「舞蹈楼」「音乐楼」两栋没被任何规则接住，落进了"未分类" ——
  //    而它们在手机上就是一堆没法筛选的灰块。艺术/音乐/舞蹈/美术/琴房
  //    本来就是这个学校里的**教学用房**，归 teaching 是对的。
  //
  // ⚠️ 顺序：这条必须排在下面的 `teaching` 那条**之前**吗？不必 ——
  //    两条都返回 'teaching'，谁先谁后结果一样。
  //    但它必须排在 `sport` **之后**：带"琴"的楼里偶尔有"琴房体育馆"
  //    这种怪名字，而 sport 是更具体的那个。
  if (/舞蹈|音乐|艺术|美术|琴房|画室|剧场|礼堂|演出|art|music|dance|theat/i.test(name + ' ' + kind)) {
    return 'teaching';
  }
  if (/教学楼|教学|实验|学院|科技|university|college|school|faculty/i.test(name + ' ' + kind)) {
    return 'teaching';
  }
  return '';
}

/** 从 `building:levels` 造出对应层数的空分层（手机端按层数算楼高）。 */
function nsFloorsFromTags(tags) {
  const raw = Number((tags || {})['building:levels']);
  // ⚠️ 夹在 1~30：OSM 里偶尔有 `building:levels=99` 这种明显的错值，
  //    照单全收会让那栋楼在 3D 里变成一根通天柱。
  if (!Number.isFinite(raw) || raw < 1) return [];
  const n = Math.min(30, Math.round(raw));
  const out = [];
  for (let i = 1; i <= n; i += 1) out.push(nsNewFloor(i + 'F'));
  return out;
}

/** 当前地图视野的 bbox（south, west, north, east）。 */
function nsMapBbox() {
  const b = nsMap.getBounds();
  const sw = b.getSouthWest();
  const ne = b.getNorthEast();
  return [sw.getLat(), sw.getLng(), ne.getLat(), ne.getLng()];
}

// ---------------------------------------------------------------------------
// ★★ 烘焙好的数据（第一优先，零网络）
//
// ## 为什么要这一层
//
// 服务端那条路（`/plugin/navstudio/api/nav/osm-buildings`）**能用**，但**靠不住**：
// Overpass 的公共实例会整体拥堵 —— 实测某一段时间四个镜像**同时**挂住
// 100 秒全超时（同一时刻本机拉 overpass-api.de 拿到的是 504）。
// 拥堵时用户要**干等 100 秒**才看到失败，换个校区再来一次又是 100 秒。
//
// 而烘焙下来的东西：**瞬时、离线可用、换机器也还在**。
// 所以改成：**在本机拉一次、烘焙成静态文件**，
// 跟着标注台一起发上去（`scripts/osm-bake.mjs`）。之后导入是**零网络**的 ——
// 从自己的域上取一个 JSON，几十毫秒完成。
//
// ## 合法吗 —— 合法，而且这正是 OSM 官方希望你做的
//
// OSM 的数据是 **ODbL 1.0**，明确允许**下载、存储、离线使用、缓存**
// （Overpass 的使用条款原文就是"别反复查同一片区域，缓存它"）。
// 唯一要求是**署名** —— 所以下面 `ns-osm-hint` 里那行
// "© OpenStreetMap contributors" **别删**。
//
// ## 判定规则
//
// 视野**中心点**落在某个烘焙校区的 bbox 里 → 用那一份。
// 用中心点而不是"完全包含"，是因为用户拖到校区中间点一下最自然，
// 而"完全包含"会在稍微放大一点时就失效（视野反而比校区大）。
// ---------------------------------------------------------------------------

/** 烘焙清单（懒加载一次，之后常驻）。 */
let nsBakedIndex = null;
let nsBakedIndexPromise = null;

function nsBakedIndexLoad() {
  if (nsBakedIndex) return Promise.resolve(nsBakedIndex);
  if (!nsBakedIndexPromise) {
    nsBakedIndexPromise = fetch('nav-osm/index.json', { headers: { Accept: 'application/json' } })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => {
        nsBakedIndex = j && Array.isArray(j.campuses) ? j : { campuses: [] };
        return nsBakedIndex;
      })
      .catch(() => {
        // ⚠️ 拿不到清单**不是错误** —— 可能这一版还没烘焙任何校区，
        //    也可能用户在自己机器上开了个旧的静态目录。静默降级到"没有烘焙数据"。
        nsBakedIndex = { campuses: [] };
        return nsBakedIndex;
      });
  }
  return nsBakedIndexPromise;
}

/**
 * 点 (lat,lng) 是否落在 bbox `[s,w,n,e]` 里。 */
function nsInBox(box, lat, lng) {
  return lat >= box[0] && lat <= box[2] && lng >= box[1] && lng <= box[3];
}

/* ==========================================================================
   ★★★ 坐标系换算：WGS-84 ⇄ GCJ-02（2026-10-13 补，这一条差点毁掉整份数据）
   ==========================================================================

   ## 为什么必须有

   格式文档《三、坐标系》写着：

   > `lat` / `lng` **一律是 GCJ-02**（腾讯地图那一套）。
   > **不要**拿 WGS-84（GPS 原始值）来填 —— 两者在同一栋楼上差几百米，
   > 够把人导到隔壁小区。

   而 **OpenStreetMap 的数据是 WGS-84**（GPS 原始值）。
   我导入的时候**原样存了**，一个字符都没换算 ——
   于是整份包偏了几百米：手机上那些彩色楼块跑到了校区外面，
   用户看到的是腾讯底图自带的灰方块，于是报"还是默认状态"。

   ⚠️ 这是**最难自查的一类错**：每一环单独看都是对的（轮廓是真的、
      名字对、类别对），只有把它们放到一起、跟底图比，才看得出偏了。
      实跑截图才发现的（`tmp/shot-phone3d.cjs`）。

   ## 三条边界，缺一条就偏

   ```
     OSM 拿回来的几何   →  WGS-84  →  存进 ns.buildings 前必须转成 GCJ-02
     地图上点出来的点    →  GCJ-02  →  去 OSM 里找之前必须转成 WGS-84
     发给服务端的 bbox  →  GCJ-02  →  查 Overpass 之前必须转成 WGS-84
   ```

   ⚠️ 转换是**非线性**的（每一点的偏移量都不一样），所以**逐点转**，
      不能整个多边形加一个固定偏移量。

   ## 算法

   国测局的那个公开算法（各大地图 SDK 里都是这一份）。
   中国境外不偏移（`nsOutOfChina`）—— 境外加了会**凭空造出**偏移。
   ========================================================================== */

const GCJ_A = 6378245.0;              // 克拉索夫斯基椭球长半轴
const GCJ_EE = 0.00669342162296594323; // 第一偏心率平方

/** 在国外（不做偏移）。判定范围和各家 SDK 用的一致。 */
function nsOutOfChina(lat, lng) {
  return lng < 72.004 || lng > 137.8347 || lat < 0.8293 || lat > 55.8271;
}

function nsGcjDLat(x, y) {
  let ret = -100.0 + 2.0 * x + 3.0 * y + 0.2 * y * y + 0.1 * x * y + 0.2 * Math.sqrt(Math.abs(x));
  ret += ((20.0 * Math.sin(6.0 * x * Math.PI) + 20.0 * Math.sin(2.0 * x * Math.PI)) * 2.0) / 3.0;
  ret += ((20.0 * Math.sin(y * Math.PI) + 40.0 * Math.sin((y / 3.0) * Math.PI)) * 2.0) / 3.0;
  ret += ((160.0 * Math.sin((y / 12.0) * Math.PI) + 320 * Math.sin(((y * Math.PI) / 30.0))) * 2.0) / 3.0;
  return ret;
}

function nsGcjDLng(x, y) {
  let ret = 300.0 + x + 2.0 * y + 0.1 * x * x + 0.1 * x * y + 0.1 * Math.sqrt(Math.abs(x));
  ret += ((20.0 * Math.sin(6.0 * x * Math.PI) + 20.0 * Math.sin(2.0 * x * Math.PI)) * 2.0) / 3.0;
  ret += ((20.0 * Math.sin(x * Math.PI) + 40.0 * Math.sin((x / 3.0) * Math.PI)) * 2.0) / 3.0;
  ret += ((150.0 * Math.sin((x / 12.0) * Math.PI) + 300.0 * Math.sin((x / 30.0) * Math.PI)) * 2.0) / 3.0;
  return ret;
}

/** WGS-84 → GCJ-02（GPS 原始值 → 腾讯/高德那一套）。 */
function nsWgsToGcj(lat, lng) {
  if (nsOutOfChina(lat, lng)) return [lat, lng];
  let dLat = nsGcjDLat(lng - 105.0, lat - 35.0);
  let dLng = nsGcjDLng(lng - 105.0, lat - 35.0);
  const radLat = (lat / 180.0) * Math.PI;
  let magic = Math.sin(radLat);
  magic = 1 - GCJ_EE * magic * magic;
  const sqrtMagic = Math.sqrt(magic);
  dLat = (dLat * 180.0) / (((GCJ_A * (1 - GCJ_EE)) / (magic * sqrtMagic)) * Math.PI);
  dLng = (dLng * 180.0) / ((GCJ_A / sqrtMagic) * Math.cos(radLat) * Math.PI);
  return [lat + dLat, lng + dLng];
}

/**
 * GCJ-02 → WGS-84（反过来）。
 *
 * ⚠️ 正变换没有解析反函数，所以用**迭代逼近**：拿 WGS 猜一个值、
 *    正变换过去、看差多少、把差补回去，重复几次。
 *    实测 5 次就收敛到厘米级（那个"两倍减一下"的常见近似法误差有 1~2 米，
 *    对单栋楼还行，但我们要拿它去和 OSM 的多边形做**点在内部**判定，
 *    边界上的楼会因此判错边）。
 */
function nsGcjToWgs(lat, lng) {
  if (nsOutOfChina(lat, lng)) return [lat, lng];
  let wLat = lat;
  let wLng = lng;
  for (let i = 0; i < 5; i += 1) {
    const g = nsWgsToGcj(wLat, wLng);
    wLat += lat - g[0];
    wLng += lng - g[1];
  }
  return [wLat, wLng];
}

/** 把一串 `[[lat,lng], …]` 逐点做 wgs→gcj，并保留 6 位小数。 */
function nsPolyWgsToGcj(poly) {
  return (poly || []).map((p) => {
    const g = nsWgsToGcj(p[0], p[1]);
    return [Number(g[0].toFixed(6)), Number(g[1].toFixed(6))];
  });
}


/**
 * 烘焙文件的 elements，**按校区缓存**（一个校区只取一次）。
 *
 * ⚠️ 缓存是必须的，不是优化：下面 [nsFootprintAt] 会被**逐栋**调用
 *    （"给没轮廓的楼补轮廓"可能连着调一百多次）。不缓存的话就是
 *    一百多次 HTTP —— 每次几十毫秒，用户会以为卡死了。
 *
 * ⚠️ 取不到就缓存**空数组**（而不是不缓存）：同一个校区失败两次的代价
 *    是白等两次网络超时，而结果是同一个。
 */
const nsBakedEls = new Map();

async function nsBakedElements(campus) {
  if (nsBakedEls.has(campus.slug)) return nsBakedEls.get(campus.slug);
  let els = [];
  try {
    const res = await fetch('nav-osm/' + campus.file, { headers: { Accept: 'application/json' } });
    if (res.ok) {
      const j = await res.json();
      if (j && Array.isArray(j.elements)) els = j.elements;
    }
  } catch {
    /* 取不到就当这个校区没有烘焙数据 */
  }
  nsBakedEls.set(campus.slug, els);
  return els;
}

/**
 * 这一带有没有烘焙好的数据？有就返回 `{ elements, campus }`，没有返回 `null`。
 */
async function nsBakedFetch(bbox) {
  const lat = (bbox[0] + bbox[2]) / 2;
  const lng = (bbox[1] + bbox[3]) / 2;
  const idx = await nsBakedIndexLoad();
  const hit = (idx.campuses || []).find((c) => Array.isArray(c.bbox) && nsInBox(c.bbox, lat, lng));
  if (!hit) return null;
  const elements = await nsBakedElements(hit);
  if (!elements.length) return null;
  return { elements, campus: hit };
}

/* ------------------------------------------------------- 几何：找"这一点的那个轮廓" */

/**
 * 射线法：点是否在多边形里。
 *
 * ⚠️ 参数顺序是 `(lat, lng)`，而多边形点也是 `[lat, lng]` ——
 *    这和 GeoJSON 的 `[lng, lat]` **相反**，是我们自己全链路统一的约定
 *    （见 navpack 的 `shape` 字段）。写反了的表现是"判定结果毫无规律地错"，
 *    因为经纬度数量级不同，不会报错、也不会全错。
 */
function nsPointInPoly(lat, lng, poly) {
  let inside = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i, i += 1) {
    const yi = poly[i][0];
    const xi = poly[i][1];
    const yj = poly[j][0];
    const xj = poly[j][1];
    if ((yi > lat) !== (yj > lat) && lng < ((xj - xi) * (lat - yi)) / (yj - yi) + xi) {
      inside = !inside;
    }
  }
  return inside;
}

/** 多边形面积（平方米，够用的近似）。用鞋带公式 + 按纬度把经度折成米。 */
function nsPolyAreaM2(poly) {
  let s = 0;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i, i += 1) {
    s += poly[j][1] * poly[i][0] - poly[i][1] * poly[j][0];
  }
  const mLat = 110540;
  const mLng = 111320 * Math.cos((poly[0][0] * Math.PI) / 180);
  return Math.abs(s / 2) * mLat * mLng;
}

/** 两个坐标之间的大致距离（米）。 */
function nsDistM(lat1, lng1, lat2, lng2) {
  const dLat = (lat2 - lat1) * 110540;
  const dLng = (lng2 - lng1) * 111320 * Math.cos((lat1 * Math.PI) / 180);
  return Math.sqrt(dLat * dLat + dLng * dLng);
}

/**
 * 点到**线段**的距离（米）。
 *
 * ⚠️ 先把两端点换算成"以这个点为原点的米"，再算点到线段 ——
 *    直接拿经纬度算距离在纬度 21° 附近会差 7%（经度被压扁），
 *    而这里的判据是"40 米以内"，7% 就是 3 米的误差，不值得省这一步。
 */
function nsDistToSegM(lat, lng, a, b) {
  const kLat = 110540;
  const kLng = 111320 * Math.cos((lat * Math.PI) / 180);
  const ax = (a[1] - lng) * kLng;
  const ay = (a[0] - lat) * kLat;
  const bx = (b[1] - lng) * kLng;
  const by = (b[0] - lat) * kLat;
  const dx = bx - ax;
  const dy = by - ay;
  const len2 = dx * dx + dy * dy;
  // 退化成一个点时（OSM 里偶尔有重复顶点），投影系数取 0
  let t = len2 > 0 ? -(ax * dx + ay * dy) / len2 : 0;
  t = Math.max(0, Math.min(1, t));
  const cx = ax + t * dx;
  const cy = ay + t * dy;
  return Math.sqrt(cx * cx + cy * cy);
}

/**
 * 点到**多边形**的最近距离（米）。点在内部返回 0。
 *
 * ⚠️ 为什么不是"到第一个顶点的距离"（第一版就是那么写的）：
 *    多边形的**第一个顶点**和"这栋楼离我多近"没有任何关系 ——
 *    实测一个 35 个顶点的楼，点在它旁边 2 米，到第一个顶点却有 40 多米，
 *    于是兜底逻辑会去认领**街对面**那栋更"近"的楼。
 */
function nsDistToPolyM(lat, lng, poly) {
  if (nsPointInPoly(lat, lng, poly)) return 0;
  let best = Infinity;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i, i += 1) {
    const d = nsDistToSegM(lat, lng, poly[j], poly[i]);
    if (d < best) best = d;
  }
  return best;
}

/**
 * 把一段几何点解析成 `[[lat, lng], …]`。
 *
 * ## ★★★ 为什么必须有它 —— 同一个概念在项目里有**两种**形状
 *
 * ```
 *   Overpass 的**原始响应**    geometry: [{ lat: 21.1, lon: 110.3 }, …]
 *   我们自己的**烘焙文件**      geometry: [[21.1, 110.3], …]
 * ```
 *
 * 两者都合法地存在于这个项目里，而**读法完全不同**：
 * 读 `p.lat` 的代码碰上数组得到 `undefined`，读 `p[0]` 的代码碰上对象
 * 也得到 `undefined`。**两种错都不会报"格式不对"，只会安静地崩或产出垃圾。**
 *
 * ## 实际踩到的后果（2026-10-13）
 *
 * ```
 *   · nsImportOsm 读 p.lat/p.lon  →  烘焙文件喂进来时 undefined.toFixed() 抛错
 *                                    →  "从 OSM 导入"在湖光校区**一栋都导不进来**
 *                                    →  用户看到自己以前那些矩形，以为是"没真轮廓"
 *   · nsPickFootprint 读 p[0]/p[1] →  服务端那条路喂进来时得到 [undefined, undefined]
 *                                    →  多边形判定全是垃圾（不报错，只是找不到）
 * ```
 *
 * 一个读对象的、一个读数组的，**两条路各错一半**。
 *
 * ⚠️ 所以：**任何**要读 Overpass 风格几何的地方都必须过这个函数，
 *    不许再手写 `p[0]` 或 `p.lat`。两处各写一遍就一定会有一处先过期。
 */
function nsGeomPoints(geom) {
  if (!Array.isArray(geom)) return [];
  return geom
    .map((p) => {
      if (Array.isArray(p)) return [Number(p[0]), Number(p[1])];
      if (p && typeof p === 'object') return [Number(p.lat), Number(p.lon)];
      return null;
    })
    // ⚠️ 坏点**逐点丢**（和 nsNormalizeBuilding 同一套规矩）：
    //    整条丢掉的话，用户/上游给了一个坏点就没了整栋楼。
    .filter((p) => p && Number.isFinite(p[0]) && Number.isFinite(p[1]))
    // 只留中国境内的点（和 nsParseCoord 同一个判据）
    .filter((p) => p[0] >= 3 && p[0] <= 54 && p[1] >= 73 && p[1] <= 136)
    .map((p) => [Number(p[0].toFixed(6)), Number(p[1].toFixed(6))]);
}

/**
 * 从一堆 OSM 要素里挑出"**点 (lat,lng) 所在的那一栋**"。
 *
 * ## 判据（顺序有讲究）
 *
 * ```
 *   ① 点在多边形**内部**        → 就是它（多栋命中时取**面积最小**的）
 *   ② 没有命中，但 40 米内有楼   → 取最近的那栋
 *   ③ 都没有                     → null（交给调用方保留矩形）
 * ```
 *
 * ⚠️ ①里为什么取**面积最小**的：OSM 里"一个院子"和"院子里那栋楼"是**两个
 *    都带 building 标签的 way**，点在楼里时两个都命中。取大的那个，
 *    用户点的是教学楼，画出来却是整个院区 —— 而那个轮廓看起来"没错"，
 *    只是大了一圈，最难发现。
 *
 * ⚠️ ②的 40 米不是随手定的：用户点的是地图上一个大概位置，而腾讯 POI 的
 *    点位和 OSM 的楼心差十几米是常事。太严（比如 5 米）会让"明明有楼"
 *    的情况一直找不到；太松（比如 200 米）会把隔壁楼认成本楼 ——
 *    而认错楼比"没认出来"严重，因为它**看起来是成功的**。
 *
 * ⚠️ ②按**到多边形的最近距离**取，不是"到第一个顶点的距离"（第一版是后者）。
 *    后者和"这栋楼离我多近"没有关系：实测一栋 35 个顶点的楼，
 *    点在它旁边几米，到第一个顶点却有 40 多米 —— 于是会去认领街对面那栋。
 */
function nsPickFootprint(els, lat, lng) {
  const polys = (els || [])
    .map((e) => ({ tags: (e && e.tags) || {}, poly: nsGeomPoints(e && e.geometry) }))
    // ⚠️ 少于 3 个点画不出面
    .filter((x) => x.poly.length >= 3);

  // ① 落在内部 —— 取面积最小的（最具体的那个）
  const hits = polys.filter((x) => nsPointInPoly(lat, lng, x.poly));
  if (hits.length) {
    hits.sort((a, b) => nsPolyAreaM2(a.poly) - nsPolyAreaM2(b.poly));
    return hits[0];
  }

  // ② 附近 40 米内**最近**的一栋（按到轮廓的距离，不是到某个顶点）
  let best = null;
  let bestD = 40;
  polys.forEach((x) => {
    const d = nsDistToPolyM(lat, lng, x.poly);
    // 距离并列时取**面积小**的那个 —— 和 ① 同一个口径：
    // "院子"和"院子里的楼"都在 40 米内且都贴着你，该选楼。
    if (d < bestD || (d === bestD && best && nsPolyAreaM2(x.poly) < nsPolyAreaM2(best.poly))) {
      bestD = d;
      best = x;
    }
  });
  return best;
}

/**
 * 在多边形里找一个**离 (lat,lng) 最近的内部点**。
 *
 * ## 为什么需要它（2026-10-13）
 *
 * 老数据（WGS 坐标被当成 GCJ 存的那批）整份偏了几百米：
 * 楼的**中心点**和它的**真实轮廓**不在一个地方。
 * 修轮廓的时候如果不动中心点，结果就是"轮廓对了、图钉还在隔壁"——
 * 而那个状态看起来是"修好了"，更难发现。
 *
 * ⚠️ 点在内部就直接返回原值 —— **不要**动不动就把用户手工摆的点挪走。
 *    用户把点放在楼门口是他的选择，不该被"规范化"掉。
 *
 * ⚠️ 网格扫描而不是"顶点平均值"：凹多边形（口字形、L 形）的顶点平均值
 *    会落在**外面**。实测湖光校区的「行政楼」（13 点）和「舞蹈楼」（35 点）
 *    都属于这种，用重心会得到楼外的点。
 */
function nsNearestInsidePoint(poly, lat, lng) {
  if (!Array.isArray(poly) || poly.length < 3) return [lat, lng];
  if (nsPointInPoly(lat, lng, poly)) return [lat, lng];

  const lats = poly.map((p) => p[0]);
  const lngs = poly.map((p) => p[1]);
  const s = Math.min.apply(null, lats);
  const N = Math.max.apply(null, lats);
  const w = Math.min.apply(null, lngs);
  const E = Math.max.apply(null, lngs);

  // 20×20 的网格：对一栋楼来说够密（校区那种 100 米的楼，格子 5 米见方），
  // 而且最多 400 次点在多边形判定 —— 逐栋补轮廓时完全没有感知。
  let best = null;
  let bestD = Infinity;
  for (let i = 0; i <= 20; i += 1) {
    for (let j = 0; j <= 20; j += 1) {
      const y = s + ((N - s) * i) / 20;
      const x = w + ((E - w) * j) / 20;
      if (!nsPointInPoly(y, x, poly)) continue;
      const d = nsDistM(lat, lng, y, x);
      if (d < bestD) { bestD = d; best = [Number(y.toFixed(6)), Number(x.toFixed(6))]; }
    }
  }
  // 网格没扫到（多边形比像素还细）→ 退回顶点平均值，至少是个数
  return best || [lat, lng];
}

/**
 * 按 **OSM way id** 直接找出那一栋（2026-10-13）。
 *
 * ## 为什么需要它，而且比按位置找更该优先
 *
 * 从 OSM 导入过的楼都记着 `b.osm`（way id）。有了它就是一个**唯一标识**，
 * 根本不用去猜"哪个多边形盖住了我这个点"。
 *
 * ⚠️ 更要紧的是：**它是修老数据的唯一正确办法。**
 *
 * 老数据（2026-10-13 之前导入的）整份偏了 ~530 米 ——
 * 因为 OSM 的 WGS-84 坐标被当成 GCJ-02 存了。这时候按位置去找：
 *
 * ```
 *   存着的点是 W（真 WGS），代码以为是 GCJ
 *   → 转成 WGS 得到 gcjToWgs(W) ≈ W - 530 米
 *   → 在**离那栋楼 530 米的另一个地方**搜楼
 *   → 找到的是**别的楼**（或者什么都找不到）
 * ```
 *
 * 也就是说：**按位置找不但修不好，还会把 A 楼的轮廓安到 B 楼上** ——
 * 而那是"看起来修好了"的错，比没修更坏。
 * 按 way id 找就没有这个问题：id 对得上就是同一栋。
 */
async function nsFootprintByOsmId(osmId) {
  const want = String(osmId || '').trim();
  if (!want) return null;
  try {
    const idx = await nsBakedIndexLoad();
    for (const c of idx.campuses || []) {
      // eslint-disable-next-line no-await-in-loop -- 校区只有几个，顺序取可读性更好
      const els = await nsBakedElements(c);
      const hit = (els || []).find((e) => String(e.id || '') === want);
      if (hit) {
        const poly = nsGeomPoints(hit.geometry);
        if (poly.length >= 3) {
          // ⚠️ 别忘了这一步：烘焙文件是 WGS，存进包里必须是 GCJ
          return { poly: nsPolyWgsToGcj(poly), tags: hit.tags || {} };
        }
      }
    }
  } catch {
    /* 烘焙数据坏了就退回按位置找 */
  }
  return null;
}

/**
 * 给一个坐标找**真实轮廓**（2026-10-13）。
 *
 * ## 它解决什么
 *
 * 用户的原话：
 *
 * > **"地名可以自动识别，点击一下自动弹出对应的地名，但是对应的模型没有
 * >  自动产生，还要我自己去建，这是很不好的。"**
 *
 * 在它之前，"点选建楼"只能给一个**矩形**（当时的注释写着"拿不到真实轮廓"）——
 * 而那是在接入 OSM **之前**的结论，早就不成立了。
 *
 * ## 两条路（先本地，后网络）
 *
 * ```
 *   ① 点落在某个**烘焙校区**的 bbox 里  →  在已经取回来的那份数据里找（零网络）
 *   ② 否则                            →  让服务端按一个**很小的 bbox** 拉一次
 * ```
 *
 * ⚠️ ①是主路，而且它覆盖的正是"用户在给自己学校标楼"这个场景 ——
 *    155 栋一次取回来，之后每一栋都是内存里查，瞬时的。
 *
 * @returns `{ poly, tags }` 或 `null`
 */
async function nsFootprintAt(lat, lng) {
  // ★★★ 入参是 **GCJ-02**（地图上点出来的点），而 OSM 里的一切都是 **WGS-84**。
  //    所以先倒过去，最后再把找到的轮廓正回来 —— 不能混着用。
  const w = nsGcjToWgs(lat, lng);
  const wLat = w[0];
  const wLng = w[1];

  // ---- ① 烘焙数据（零网络）
  try {
    const idx = await nsBakedIndexLoad();
    // ⚠️ 判"在不在这个校区里"也要用 **WGS 点** —— 校区 bbox 是烘焙脚本
    //    用 WGS 坐标算出来的。拿 GCJ 点去判，边缘的楼会整片判成"不在"。
    const campus = (idx.campuses || []).find((c) => Array.isArray(c.bbox) && nsInBox(c.bbox, wLat, wLng));
    if (campus) {
      const hit = nsPickFootprint(await nsBakedElements(campus), wLat, wLng);
      if (hit) return { poly: nsPolyWgsToGcj(hit.poly), tags: hit.tags };
    }
  } catch {
    /* 烘焙那条坏了不该挡住下面那条 */
  }

  // ---- ② 服务端按小 bbox 拉一次
  //
  // ⚠️ 半径 60 米：够盖住"POI 点位和楼心差十几米"这种情况，又不至于把
  //    隔壁楼拉进来（② 那 40 米的判据会再筛一次）。
  // ⚠️ 复用 `/plugin/navstudio/api/nav/osm-buildings`（不是新接口）—— 它本来就有
  //    两层缓存和熔断，而且这个 bbox 足够小，落在"中国境内 + 跨度上限"里。
  // ⚠️ bbox 必须用 **WGS** 算（那是发给 Overpass 的），不然查的是错的一片。
  if (nsServerDown) return null;
  try {
    const d = 0.0006; // ≈ 66 米
    const u = '/plugin/navstudio/api/nav/osm-buildings'
      + '?south=' + (wLat - d) + '&west=' + (wLng - d)
      + '&north=' + (wLat + d) + '&east=' + (wLng + d);
    const res = await fetch(u, { headers: { Accept: 'application/json' } });
    if (!res.ok) {
      if (res.status !== 400) nsServerDown = true;
      return null;
    }
    const j = await res.json();
    const hit = nsPickFootprint(j && j.elements, wLat, wLng);
    return hit ? { poly: nsPolyWgsToGcj(hit.poly), tags: hit.tags } : null;
  } catch {
    nsServerDown = true;
    return null;
  }
}

/**
 * 拉一次建筑轮廓。
 *
 * ## ★★ 走**自己的服务端**，不是直连 Overpass
 *
 * 实测：浏览器**连不上** Overpass（导航到 `overpass.kumi.systems` 直接超时），
 * 而同一台机器的 Node 拉同一个地址是好的 —— **本机的 Kaspersky 只拦浏览器**。
 *
 * 所以让服务端去拉（它连得上），浏览器只跟**自己的域**打交道：
 *
 * ```
 *   浏览器 → /api/nav/osm-buildings → Overpass → 原样回
 * ```
 *
 * ⚠️ 而且标注台和服务端**是同一个进程伺服的两个端口**（实测两边 `/api/health`
 *    返回同一份），所以这是个**相对路径**调用 —— **同源，连 CORS 都不用管**。
 *
 * ⚠️ 直连那条路**留着当兜底**：服务端没更新、或者用户在别的环境下打开时，
 *    直连可能反而是通的。两条都试过才报错。
 *
 * @param bbox `[south, west, north, east]`
 * @returns `{ elements, from, campus? }` —— `from` 是 `'baked' | 'server' | 'direct'`；
 *          失败抛错（带人话原因）
 */
async function nsFetchOsm(gcjBbox) {
  // ★★★ 从地图拿到的是 **GCJ-02**（腾讯那一套），而 Overpass / OSM 要的是
  //    **WGS-84**。所以这里统一转一次，下面所有"发给外面"的东西都用 WGS 版。
  //
  //    ⚠️ 不转的后果不是"报错"，是**查错了一片地方**（偏几百米），
  //       而结果看起来完全正常（有楼、有名字、有轮廓）—— 最难自查的那类。
  //    ⚠️ 只转**中心点**再按原来的跨度撑开：四个角各转一次的话，
  //       得到的是个歪掉的四边形，取它的外接框会把范围撑得过大。
  //       中心的偏移量在这一片是均匀的（同一城市内变化极小），够用。
  const [south0, west0, north0, east0] = gcjBbox;
  const cw = nsGcjToWgs((south0 + north0) / 2, (west0 + east0) / 2);
  const dLat = cw[0] - (south0 + north0) / 2;
  const dLng = cw[1] - (west0 + east0) / 2;
  const south = south0 + dLat;
  const north = north0 + dLat;
  const west = west0 + dLng;
  const east = east0 + dLng;

  const q = '[out:json][timeout:40];\n'
    + 'way["building"](' + [south, west, north, east].join(',') + ');\n'
    + 'out geom tags;';

  // ---- ⓪ 烘焙好的静态数据（**零网络，瞬时** —— 首选）
  //
  // 见上面那段注释：服务端那条路靠不住（上游会拥堵），所以这一层才是主路。
  // ⚠️ 传 WGS 的 bbox —— 烘焙文件的索引里存的就是 WGS 坐标。
  try {
    const baked = await nsBakedFetch([south, west, north, east]);
    if (baked) return { elements: baked.elements, from: 'baked', campus: baked.campus };
  } catch {
    /* 烘焙数据坏了不该挡住后面两条路 */
  }

  // ---- ① 自己的服务端（次选）
  //
  // ⚠️ `nsServerDown` 是**本次会话的短路开关**：服务端要么 5xx 要么超时过，
  //    就别再让它把用户晾一次（那一次是 100 秒）。见下面赋值的注释。
  if (!nsServerDown) {
    try {
      const u = '/plugin/navstudio/api/nav/osm-buildings'
        + '?south=' + south + '&west=' + west + '&north=' + north + '&east=' + east;
      const res = await fetch(u, { headers: { Accept: 'application/json' } });
      if (res.ok) {
        const j = await res.json();
        if (j && Array.isArray(j.elements)) return { elements: j.elements, from: 'server' };
      } else if (res.status === 400) {
        // ⚠️ 400 是**我们的参数被拒**（范围太大 / 不在中国境内），
        //    这种错直连也没用 —— 直接把服务端那句话透出去，别让它变成"拉取失败"。
        const j = await res.json().catch(() => null);
        throw new Error((j && (j.error || j.message)) || '范围不合法');
      } else {
        // 5xx（含熔断的 503）：**服务端到上游这条路刚才是断的**（上游拥堵或熔断中）。
        // 记下来，本次会话不再走它 —— 否则用户每导一个没烘焙过的地方
        // 都要再等一次 100 秒，而结果必然是同一个。
        nsServerDown = true;
      }
    } catch (e) {
      if (e && e.message && !/fetch|network|Failed/i.test(e.message)) throw e;
      nsServerDown = true;
    }
  }

  // ---- ② 兜底：直连 Overpass（有些环境反而是通的）
  //
  // ⚠️ **必须自己设超时**（`AbortController`）：`fetch` 默认**没有超时**，
  //    一个连上了但不回的镜像能让这个 await 挂好几分钟 ——
  //    而用户看到的是"按钮转圈转到天荒地老"，连失败都等不到。
  //    45 秒是有意的：实测本机拉一个校区 10~77 秒，但那是**服务端**在拉；
  //    浏览器直连如果 45 秒还没回，多半就是不通，再等没意义。
  let lastErr = '';
  for (const url of NS_OVERPASS) {
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), 45000);
    try {
      const res = await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: 'data=' + encodeURIComponent(q),
        signal: ac.signal,
      });
      if (!res.ok) { lastErr = 'HTTP ' + res.status; continue; }
      const j = await res.json();
      if (j && Array.isArray(j.elements)) return { elements: j.elements, from: 'direct' };
      lastErr = '返回的不是预期结构';
    } catch (e) {
      lastErr = (e && e.name === 'AbortError') ? '超时（45 秒）' : ((e && e.message) ? e.message : String(e));
    } finally {
      clearTimeout(timer);
    }
  }
  throw new Error(lastErr || '拉取失败');
}

/**
 * 导入。**整个过程在按钮的点击处理里跑**，状态写回 `#ns-osm-hint`。
 *
 * ⚠️ 它可能跑十几秒（Overpass 有时很慢）—— 所以必须**先把状态文字改成"正在拉取"**
 *    再 await，否则用户会以为按钮坏了然后连点。
 */
async function nsImportOsm() {
  if (!nsMap) {
    nsToast('地图还没就绪');
    return;
  }
  const hint = $('ns-osm-hint');
  const btn = $('ns-btn-osm');
  const setHint = (html) => { if (hint) hint.innerHTML = html; };

  if (btn) btn.disabled = true;
  setHint('正在找这一带的建筑…');

  let got;
  try {
    got = await nsFetchOsm(nsMapBbox());
  } catch (e) {
    if (btn) btn.disabled = false;
    setHint('拉取失败：' + String(e.message || e) +
      '<br />这一带如果还没烘焙过，就得到国外服务器现拉 —— 而它经常超时。'
      + '稳妥的办法是在本机跑 <code>node scripts/osm-bake.mjs</code> 把这片烘焙下来。');
    return;
  }
  const els = got.elements;

  // ---- 合并
  const existing = new Set(ns.buildings.map((b) => String(b.osm || '')).filter(Boolean));
  let added = 0;
  let named = 0;
  let skipped = 0;

  els.forEach((e) => {
    const osmId = String(e.id || '');
    // ★★ 必须过 `nsGeomPoints`，不能读 `p.lat` ——
    //    烘焙文件存的是 `[lat, lng]` 数组，读 `p.lat` 会 undefined.toFixed() 抛错，
    //    表现是"在湖光校区点导入，一栋都没进来"（而不报风格上的错）。
    const raw = nsGeomPoints(e.geometry);
    // ⚠️ 少于 3 个点画不出面（有些 way 是残缺的）——在**转换之前**判，
    //    因为转换不改变点数，先判可以少算几个点。
    if (raw.length < 3) { skipped += 1; return; }
    if (osmId && existing.has(osmId)) { skipped += 1; return; }

    // ★★★ WGS-84 → GCJ-02（2026-10-13）。
    //
    // OSM 的数据是 **GPS 原始值（WGS-84）**，而这份格式规定 `lat/lng`
    // **一律是 GCJ-02**（腾讯地图那一套）。不换算的话整份包偏几百米 ——
    // 手机上彩色楼块跑到校区外面，用户看到的是腾讯底图自带的灰方块，
    // 于是报"还是默认状态"。
    //
    // ⚠️ 逐点转，不能整个多边形加一个固定偏移量（偏移量是位置相关的）。
    const shape = nsPolyWgsToGcj(raw);

    const tags = e.tags || {};
    // 楼的位置 = 轮廓的重心（不是第一个点 —— 那样标记会挂在墙角上）
    const lat = shape.reduce((s, p) => s + p[0], 0) / shape.length;
    const lng = shape.reduce((s, p) => s + p[1], 0) / shape.length;

    const name = String(tags.name || tags['name:zh'] || '').trim();
    const b = nsNormalizeBuilding({
      id: nsNewId(),
      name: name,
      alias: [],
      lat: Number(lat.toFixed(6)),
      lng: Number(lng.toFixed(6)),
      campus: '',
      note: '',
      type: nsGuessType(tags),
      floors: nsFloorsFromTags(tags),
      shape: shape,
      // ★ 记下 OSM 的 id：下次再导同一栋时跳过，**保住用户改过的名字和类别**
      osm: osmId,
    });
    ns.buildings.push(b);
    added += 1;
    if (name) named += 1;
  });

  if (btn) btn.disabled = false;
  nsSave();
  nsRenderAll();

  const fromText = got.from === 'baked'
    ? '（本地烘焙的 <b>' + String((got.campus && got.campus.name) || '') + '</b>，没走网络）'
    : got.from === 'server'
      ? '（服务端现拉的）'
      : '（浏览器直连 Overpass 拉的）';

  setHint('导入完成' + fromText + '：新增 <b>' + added + '</b> 栋'
    + (named ? '（其中 <b>' + named + '</b> 栋自带名字）' : '')
    + (skipped ? '，跳过 ' + skipped + ' 栋（已导入过或形状不全）' : '')
    + '。<br />没有名字的那些，选中后在右边填上就行 —— 轮廓已经是真的了。'
    + '<br />数据来自 <b>OpenStreetMap</b>（© OpenStreetMap contributors，ODbL 协议）。');

  nsStatus('已从 OSM 导入 ' + added + ' 栋建筑');
  nsToast(added ? ('导入 ' + added + ' 栋 —— 有 ' + named + ' 栋自带名字') : '这一带没有新建筑可导入');
}

/* ------------------------------------------------------------------ 点选建楼 */

/**
 * 在某个点建一栋楼（2026-10-13）。
 *
 * ## 它是怎么把"挨个重画"变成"点一下就建"的
 *
 * ```
 *   位置  ← 用户点的那一下（真实坐标）
 *   名字  ← ev.poi.name（地图上有名字的楼能直接拿到）
 *   轮廓  ← ★★ 自动去 OSM 找**真实轮廓**（见 nsFootprintAt）
 *   类别  ← 沿用"上一次选的那个类别"（用户通常连着标好几栋同类楼）
 * ```
 *
 * 用户要做的只剩：点一下 → 看一眼名字对不对 → 点下一栋。
 *
 * ## ★★ 轮廓：先给矩形，再**换成真的**（2026-10-13 改）
 *
 * 原来这里只给一个矩形，注释写着"拿不到真实轮廓"—— 那是**接入 OSM 之前**
 * 的结论。用户后来的原话是：
 *
 * > **"地名可以自动识别，点击一下自动弹出对应的地名，但是对应的模型没有
 * >  自动产生，还要我自己去建，这是很不好的。"**
 *
 * 所以现在：**先立刻放一个矩形**（用户马上看到"这栋立起来了"，不用等），
 * 然后异步去找真轮廓，找到就**替换掉**。
 *
 * ⚠️ 顺序是刻意的：先等网络再画的话，点一下要愣半秒才出东西 ——
 *    而"点了没反应"是这个项目里最难查的一类问题，不该自己制造。
 *
 * ⚠️ 找回来的轮廓要**校验 id 还在**：这中间用户可能已经把那一栋删了。
 *    不校验的话会往一个不存在的 id 上写数据（表现是"删了又自己回来了"）。
 */
async function nsCreateAt(lat, lng, poiName) {
  const b = nsNormalizeBuilding({
    id: nsNewId(),
    name: String(poiName || '').trim(),
    alias: [],
    lat: Number(lat.toFixed(6)),
    lng: Number(lng.toFixed(6)),
    campus: '',
    note: '',
    // ★ 沿用上次选的类别 —— 连着标好几栋教学楼时不用每次都点
    type: ns.lastType || '',
  });
  // 立刻给一个矩形轮廓，用户看到的就是"一栋立起来的楼"
  // ⚠️ 同时标成**占位**（`shapeExact = false`）—— 下面异步换成真轮廓时再翻成 true。
  //    不标的话手机端会把这个方盒子报成"真轮廓"。
  b.shape = nsRectAround(b);
  b.shapeExact = false;
  ns.buildings.push(b);
  ns.activeId = b.id;
  ns.floorIndex = 0;
  nsShapeTargetId = null;
  nsSave();
  nsRenderAll();

  // 焦点给楼名 —— 用户点完的下一步就是"名字对不对"，让他能直接改
  const nameEl = $('ns-b-name');
  if (nameEl) {
    nameEl.focus();
    nameEl.select();
  }
  nsStatus(poiName
    ? `已在「${poiName}」建了一栋 —— 正在找它的真实轮廓…`
    : '已建了一栋 —— 正在找它的真实轮廓…');

  // ---- 异步换成真轮廓
  const found = await nsFootprintAt(b.lat, b.lng);
  const live = ns.buildings.find((x) => x.id === b.id);
  if (!live) return; // 用户在这中间把它删了
  if (!found) {
    nsStatus(poiName
      ? `「${poiName}」已建好 —— OSM 上这一带没有现成轮廓，用的是默认矩形`
      : '已建了一栋 —— 这一带 OSM 上没有现成轮廓，用的是默认矩形');
    return;
  }
  live.shape = found.poly.map((p) => [Number(p[0].toFixed(6)), Number(p[1].toFixed(6))]);
  live.shapeExact = true;
  // ★ 名字也从 OSM 补：地图 POI 没名字、但 OSM 有名字的情况很常见
  const osmName = String(found.tags.name || found.tags['name:zh'] || '').trim();
  if (!live.name && osmName) live.name = osmName;
  if (!live.type) live.type = nsGuessType(found.tags);

  nsSave();
  nsRenderAll();
  nsStatus(`已建好「${live.name || '（未命名）'}」—— 轮廓取自 OpenStreetMap 的真实建筑`);
}

/** 以楼的坐标为中心，生成一个矩形轮廓（宽/深按分层数据推）。 */
function nsRectAround(b) {
  const size = nsFootprintMeters(b);
  const dLat = (size.d / 2) / 110540;
  const dLng = (size.w / 2) / (111320 * Math.cos((b.lat * Math.PI) / 180));
  return [
    [Number((b.lat - dLat).toFixed(6)), Number((b.lng - dLng).toFixed(6))],
    [Number((b.lat - dLat).toFixed(6)), Number((b.lng + dLng).toFixed(6))],
    [Number((b.lat + dLat).toFixed(6)), Number((b.lng + dLng).toFixed(6))],
    [Number((b.lat + dLat).toFixed(6)), Number((b.lng - dLng).toFixed(6))],
  ];
}

/**
 * 形状微调：宽 / 深 / 旋转（2026-10-13）。
 *
 * ## 为什么是按钮而不是拖角
 *
 * 拖角要自己实现命中测试 + 拖动状态机（TMap 的 marker 拖动还要另外验），
 * 而"点六下按钮"已经足够把体块调到贴合 —— **用户要的是"别让我描十个点"，
 * 不是"给我一个 CAD"**。多做的那些交互如果做不稳，比没有更糟。
 *
 * ⚠️ 微调以**当前矩形的外接框**为基准重算，不是累加偏移 ——
 *    累加的话，用户按十次"宽 +" 会因为浮点误差慢慢歪掉。
 */
/**
 * 微调用的**基准快照** —— 按下滑条那一刻的形状。`null` = 没在调。
 *
 * ## ★★★ 为什么必须有它（用户报的"怎么点都是模型加大"）
 *
 * 用户的原话：
 *
 * > "还有那个模型微调是什么鬼呀？**怎么点它都是模型加大**，然后也不能进行调整。"
 *
 * 老实现是这么算的：
 *
 * ```
 *   w = 当前形状的**外接框**宽        ← 这一步就是祸根
 *   w += 3
 *   b.shape = 以楼为中心重建矩形(w, d, rot)
 * ```
 *
 * 而矩形一旋转，它的**外接框**就变大（`w·cos15° + d·sin15° > w`）——
 * 于是下次微调读到的"宽"是**虚高的**，再加 3 米……**每点一次都比上次大一圈**。
 *
 * 现在一切以这份基准为准：
 * ```
 *   滑条给的是**目标尺寸**（绝对值，米）
 *   比例 = 目标 / 基准   →  基准点集 × 比例 + 旋转  →  新点集
 * ```
 * 每次 input 都**从基准算**，不从"上一次的结果"算 —— 那样会指数累积。
 */
let nsShapeBase = null;

/**
 * 一栋楼轮廓的宽 / 深（米）。
 *
 * ⚠️ **矩形用边长，不用外接框。**
 *    外接框会随旋转变大，而"这栋楼宽 24 米"不该因为转了个角度就变成 31 米 ——
 *    用户报的"怎么点它都是加大"根因就是这个。
 *    真轮廓（OSM 回来的、手描的）没有"边长"这个概念，用外接框是对的。
 */
function nsShapeMetrics(b) {
  const pts = Array.isArray(b.shape) ? b.shape : [];
  if (!b || pts.length < 3) return null;
  const lats = pts.map((p) => p[0]);
  const lngs = pts.map((p) => p[1]);
  const minLat = Math.min.apply(null, lats);
  const maxLat = Math.max.apply(null, lats);
  const minLng = Math.min.apply(null, lngs);
  const maxLng = Math.max.apply(null, lngs);
  const cx = (minLng + maxLng) / 2;
  const cy = (minLat + maxLat) / 2;
  const kLng = 111320 * Math.cos((cy * Math.PI) / 180);
  const isRect = pts.length === 4 && !b.shapeExact;
  if (isRect) {
    // ⚠️ 顶点顺序以 `nsRectAround` 为准：**第 0→1 点是宽（东西向），第 1→2 点是深**。
    //    （我第一版写反了 —— 于是"宽"读到的是深，滑条拖到 40 结果出来 14。）
    // ⚠️ 用**和 nsShapeFromBase 同一套平面近似**（111320 / 110540），
    //    不要用 nsDistM 的 Haversine —— 两者差约 0.3%，
    //    于是"设成 40 米"量回来是 39.9、连点六次少半米（实测就是这么发现的）。
    //    这里要的不是"地球上的真实距离"，而是**和缩放自洽的同一把尺子**。
    const w = Math.hypot((pts[1][1] - pts[0][1]) * kLng, (pts[1][0] - pts[0][0]) * 110540);
    const d = Math.hypot((pts[2][1] - pts[1][1]) * kLng, (pts[2][0] - pts[1][0]) * 110540);
    // 基准角：宽方向相对"正东"的角度。**旋转不会改变这个边长**，
    // 所以拖「角度」滑条时"宽"那个数字一点不动 —— 这正是用户要的。
    const ex = (pts[1][1] - pts[0][1]) * kLng;
    const ey = (pts[1][0] - pts[0][0]) * 110540;
    return { w: w, d: d, rot: Math.atan2(ey, ex), cx: cx, cy: cy, isRect: true };
  }
  return {
    w: (maxLng - minLng) * kLng,
    d: (maxLat - minLat) * 110540,
    rot: 0,
    cx: cx,
    cy: cy,
    isRect: false,
  };
}

/**
 * 基准点集 → 沿**矩形自己的宽/深方向**缩放、再绕中心转 deg 度 → 新点集。
 *
 * ## ⚠️ 为什么不能直接缩放经纬度轴
 *
 * 第一版是 `dx *= kx; dy *= ky;`（经度 × kx、纬度 × ky）—— 对**没转过**的矩形是对的，
 * 一转过 30° 就全乱：那时"经度方向的跨度"既不是宽也不是深，
 * 于是滑条拖到 40 米、量出来还是十几米（实测就是这么发现的）。
 *
 * 正确做法是三步：
 * ```
 *   ① 转到**基准的局部坐标**（把 base.rot 转回去）—— 这时 mx 才是"宽"、my 才是"深"
 *   ② 在局部里缩放 (kx, ky)
 *   ③ 再按"基准角 + 用户转的角度"转回世界坐标
 * ```
 */
function nsShapeFromBase(kx, ky, deg) {
  const base = nsShapeBase;
  if (!base) return null;
  const kLng = 111320 * Math.cos((base.cy * Math.PI) / 180);
  const cosB = Math.cos(base.rot);
  const sinB = Math.sin(base.rot);
  const th = base.rot + ((deg || 0) * Math.PI) / 180;
  const cos = Math.cos(th);
  const sin = Math.sin(th);
  return base.pts.map((p) => {
    const dx = (p[1] - base.cx) * kLng;      // 东（米）
    const dy = (p[0] - base.cy) * 110540;    // 北（米）
    // ① 世界 → 基准局部
    const mx = dx * cosB + dy * sinB;
    const my = -dx * sinB + dy * cosB;
    // ② 缩放
    const sx = mx * kx;
    const sy = my * ky;
    // ③ 局部 → 目标角度下的世界
    const wx = sx * cos - sy * sin;
    const wy = sx * sin + sy * cos;
    // ⚠️ 这里**不能** toFixed(6)：经度第 6 位小数约等于 0.11 米，
    //    每点一次微调就丢一点，"加 0.5 米"实测只加到 0.42（就是它干的）。
    //    8 位（≈1 毫米）足够干净，而且误差小到量不出来。
    //    导出时的取整由 nsBuildPack 统一做，不靠这里顺手 round。
    return [
      Number((base.cy + wy / 110540).toFixed(8)),
      Number((base.cx + wx / kLng).toFixed(8)),
    ];
  });
}

/** 把新点集装回去（滑条和 ± 按钮**都走这里**，只有一条路径）。 */
function nsShapeApply(kx, ky, deg) {
  const b = nsActiveBuilding();
  if (!b || !nsShapeBase) return;
  const next = nsShapeFromBase(kx, ky, deg);
  if (!next || next.length < 3) return;
  b.shape = next;
  // ⚠️ 微调出来的是**旋转矩形**，仍然是合成的 —— 不能因为"用户动过手"就当成真轮廓。
  //    但真轮廓（OSM / 手描）缩放之后**还是真轮廓**，别把它降级。
  b.shapeExact = nsShapeBase.isRect ? false : b.shapeExact;
  nsSave();
  nsSyncShapeLayer();
  nsRenderShapeHint();
}

/** 现在这一刻的基准尺寸（没在调就是当前形状的）。 */
function nsShapeNow() {
  const b = nsActiveBuilding();
  if (!b) return null;
  const m = nsShapeMetrics(b);
  if (!m) return null;
  if (!nsShapeBase) {
    nsShapeBase = {
      pts: JSON.parse(JSON.stringify(b.shape)),
      cx: m.cx,
      cy: m.cy,
      w: m.w,
      d: m.d,
      rot: m.rot || 0,
      isRect: m.isRect,
    };
  }
  return nsShapeBase;
}

/** 滑条在动：读三个值 → 从基准算比例 → 应用。 */
function nsShapeDimsInput() {
  const base = nsShapeNow();
  if (!base) return;
  const w = Math.max(1, Number(($('ns-dim-w') || {}).value) || base.w);
  const d = Math.max(1, Number(($('ns-dim-d') || {}).value) || base.d);
  const r = Number(($('ns-dim-r') || {}).value) || 0;
  nsShapeApply(w / base.w, d / base.d, r);
}

/**
 * ± 按钮：在**当前尺寸**上加减一个固定量（宽深 0.5 米 / 角度 5°）。
 *
 * ⚠️ 每次都从**当前形状**起算（不累积）—— 这就是"± 0.5 米"该有的样子。
 *    老实现那套"从外接框反推再重建"会越点越大。
 */
function nsNudgeShape(kind) {
  const b = nsActiveBuilding();
  if (!b) {
    nsToast('先在左边选一条');
    return;
  }
  if (!nsHasCoord(b)) {
    nsToast('这条还没有坐标 —— 先在地图上点一下定位置');
    return;
  }
  if (!Array.isArray(b.shape) || b.shape.length < 3) {
    b.shape = nsRectAround(b);
    b.shapeExact = false;
    nsSave();
    nsRenderAll();
    nsToast('先按矩形生成，再微调');
    return;
  }

  nsShapeBase = null;
  const base = nsShapeNow();
  if (!base) return;

  const STEP_W = 0.5;
  const STEP_D = 0.5;
  const STEP_R = 5;
  let w = base.w;
  let d = base.d;
  let deg = 0;
  if (kind === 'w+') w += STEP_W;
  if (kind === 'w-') w = Math.max(1, w - STEP_W);
  if (kind === 'd+') d += STEP_D;
  if (kind === 'd-') d = Math.max(1, d - STEP_D);
  if (kind === 'r+') deg = STEP_R;
  if (kind === 'r-') deg = -STEP_R;

  nsShapeApply(w / base.w, d / base.d, deg);
  nsShapeBase = null;        // ± 是一次性的：下一次点击重新取基准
  // ⚠️ 必须**在这之后**再刷一次滑条 —— `nsShapeApply` 内部那次刷新发生在
  //    基准还活着的时候，而 `nsRenderShapeDims` 在拖动期间是"不跟用户抢滑条"
  //    直接返回的。少了这一句，点 ± 之后滑条还停在旧值上（实测）。
  nsRenderShapeDims();
}

/**
 * 刷新那三条滑条（值 + 数字）。
 *
 * ⚠️ 拖动期间（`nsShapeBase` 非空）**直接返回** —— 不跟用户抢滑条。
 *    这里把 value 写回去本身不触发 `input`，但会把用户正在拖的位置**拽回**
 *    到我们算出来的值上（浮点差一点点），手感就是"滑条在抖"。
 */
function nsRenderShapeDims() {
  // ⚠️ 拖动期间：**数字照更新，滑条不写回**。
  //    写回会把用户正在拖的位置拽到我们算出来的值上（浮点差一点点），
  //    手感就是"滑条在抖"；而数字不更新的话，用户拖了半天看不到变化
  //    （实测：拖到 40 米，旁边还写着 27 米）。
  const dragging = !!nsShapeBase;
  const b = nsActiveBuilding();
  const m = b ? nsShapeMetrics(b) : null;
  const put = (id, valId, v, txt) => {
    const el = $(id);
    const out = $(valId);
    if (el && !dragging) el.value = String(v);
    if (out) out.textContent = txt;
  };
  if (!m) {
    put('ns-dim-w', 'ns-dim-w-val', 24, '—');
    put('ns-dim-d', 'ns-dim-d-val', 14, '—');
    put('ns-dim-r', 'ns-dim-r-val', 0, '—');
    return;
  }
  put('ns-dim-w', 'ns-dim-w-val', Math.round(m.w * 10) / 10,
    (Math.round(m.w * 10) / 10) + ' 米');
  put('ns-dim-d', 'ns-dim-d-val', Math.round(m.d * 10) / 10,
    (Math.round(m.d * 10) / 10) + ' 米');
  put('ns-dim-r', 'ns-dim-r-val', 0, '0°');
}

/** 把三条滑条接上（页面初始化时调一次）。 */
function nsWireShapeDims() {
  ['w', 'd', 'r'].forEach((k) => {
    const el = $('ns-dim-' + k);
    if (!el || el.dataset.wired) return;
    el.dataset.wired = '1';
    // ⚠️ 按下去就清基准 —— 保证"这一次拖动"是从**当前形状**起算的
    el.addEventListener('pointerdown', () => { nsShapeBase = null; });
    el.addEventListener('input', () => nsShapeDimsInput());
    // 松手：基准作废（下一次拖动重新取），并落一次盘
    el.addEventListener('change', () => { nsShapeBase = null; nsSave(); });
  });
}

/** 以 (lat,lng) 为中心、宽 w 米 / 深 d 米、转 rot 度的矩形（四个角）。 */
function nsRotatedRect(lat, lng, w, d, rotDeg) {
  const kLng = 111320 * Math.cos((lat * Math.PI) / 180);
  const r = (rotDeg * Math.PI) / 180;
  const cos = Math.cos(r);
  const sin = Math.sin(r);
  const hw = w / 2;
  const hd = d / 2;
  // 局部坐标（米）→ 旋转 → 经纬度
  return [[-hw, -hd], [-hw, hd], [hw, hd], [hw, -hd]].map((p) => {
    const mx = p[0] * cos - p[1] * sin;
    const my = p[0] * sin + p[1] * cos;
    return [
      Number((lat - my / 110540).toFixed(6)),
      Number((lng + mx / kLng).toFixed(6)),
    ];
  });
}

/** 把轮廓重置成"以楼为中心、按分层推的矩形"。 */
function nsShapeRect() {
  const b = nsActiveBuilding();
  if (!b) {
    nsToast('先在左边选一条');
    return;
  }
  if (!nsHasCoord(b)) {
    nsToast('这条还没有坐标 —— 先在地图上点一下定位置');
    return;
  }
  b.shape = nsRectAround(b);
  nsShapeTargetId = null;
  nsSave();
  nsRenderAll();
  nsStatus('已按矩形重置轮廓');
}

/**
 * 渲染类别图例（点一下就改这一栋的类别）。
 *
 * ★ 用**颜色块**而不是下拉框：用户要的是"教学楼蓝、宿舍粉"，
 *   看到色块才知道点下去手机端会变成什么颜色。
 *   下拉框里只有字，选完还得想象。
 */
function nsRenderTypeLegend() {
  const box = $('ns-type-legend');
  if (!box) return;
  const b = nsActiveBuilding();
  const cur = b ? String(b.type || '') : '';
  const items = Object.keys(NS_TYPE_COLORS).concat(['']);
  // ★ 2026-10-13：做成**色卡**（而不是一排小色块）。
  //
  // 用户的原话："模型的颜色选择呢，也要更加人性化现代化一点。"
  // 原来是一行行小圆点 + 小字，几十个类别时看不清也不敢点。
  // 现在每张色卡是一块**大色块 + 名字**，选中的那个带一圈描边和 ✓ ——
  // "手机端会变成什么颜色"这件事一眼可见。
  box.innerHTML = items.map((t) => {
    const label = t ? (nsTypeLabel(t) || t) : '未分类';
    const c = t ? NS_TYPE_COLORS[t] : NS_TYPE_FALLBACK;
    const active = (t === cur);
    return '<button class="ns-legend-item' + (active ? ' active' : '') + '"'
      + ' data-type="' + t + '" title="' + (t ? (label + '（' + t + '）') : label) + '">'
      + '<i style="background:rgb(' + c.join(',') + ')">' + (active ? '✓' : '') + '</i>'
      + '<span>' + label + '</span>'
      + '</button>';
  }).join('');
  box.querySelectorAll('.ns-legend-item').forEach((el) => {
    el.addEventListener('click', () => {
      const t = el.getAttribute('data-type');
      const bb = nsActiveBuilding();
      if (!bb) {
        nsToast('先在左边选一条');
        return;
      }
      bb.type = t;
      ns.lastType = t;          // 记下来，下一栋默认用它
      nsSave();
      nsRenderAll();
    });
  });
}

/** 类型的展示名（和插件侧 `typeLabel` 同一套词）。 */
function nsTypeLabel(t) {
  return {
    teaching: '教学楼', dorm: '宿舍', canteen: '食堂', office: '办公楼',
    library: '图书馆', sport: '体育', gate: '门',
  }[t] || t;
}

/* ------------------------------------------------------------------ 3D 底面轮廓 */

/**
 * 类型 → 颜色。**必须和插件侧的 `Iso3D.typeColor` 是同一套**。
 *
 * ## ★★ 为什么要在标注台也画一遍颜色
 *
 * 用户的原话："我希望可以给那个 3D 建模加上颜色，例如说给所有的教学楼加上蓝色，
 * 所有的宿舍加上粉色之类的"。
 *
 * 而"描轮廓"这件事**只有在看得见结果时才不辛苦** —— 用户描完一圈，
 * 立刻在标注台上看到那栋楼被拔起来、染成对应的颜色，
 * 才知道自己描对没有。等导进手机再看，那已经晚了（包都分发出去了）。
 *
 * ⚠️⚠️ 两边的色值必须**一字不差**。不一致的表现是"工具里是蓝的、手机上是紫的"，
 *    而用户会以为是手机端的渲染问题 —— 实际上是两处各写了一份。
 *    （闸门 `navpack-gate` 会逐色比对这两处。）
 */
var NS_TYPE_COLORS = {
  teaching: [76, 141, 246],   // 教学楼 · 蓝
  dorm: [242, 122, 178],      // 宿舍   · 粉
  canteen: [67, 185, 106],    // 食堂   · 绿
  office: [155, 108, 240],    // 办公楼 · 紫
  library: [23, 162, 162],    // 图书馆 · 青
  sport: [224, 87, 79],       // 体育   · 红
  gate: [138, 143, 152],      // 门     · 灰
};
var NS_TYPE_FALLBACK = [107, 114, 128];   // 其他 / 没填

/** 类型 → `rgba(...)`。`alpha` 由调用方给（普通半透明、选中更实）。 */
function nsTypeRgba(type, alpha) {
  var c = NS_TYPE_COLORS[type] || NS_TYPE_FALLBACK;
  return 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + alpha + ')';
}

/** 一栋楼的拔起高度（米）—— **和插件侧 `Iso3D.height` 同一套**（每层 3.6 米）。 */
function nsExtrudeHeight(b) {
  var n = (b.floors && b.floors.length) ? b.floors.length : 1;
  return Math.max(1, n) * 3.6;
}

/**
 * 把所有楼栋的**底面轮廓**画到地图上 —— **拔起来 + 按类别上色**。
 *
 * ## ★★ 2026-10-13：从"平面描边"改成"立体楼块"
 *
 * 原来这里用的是 `TMap.PolygonStyle`（平的多边形）。现在换成
 * `TMap.ExtrudablePolygonStyle` —— 和手机端**同一个 API**。
 *
 * 好处是用户描的时候看到的就是**手机端最后长出来的样子**：
 * 同样的形状、同样的高度、同样的颜色。不用在脑子里换算。
 *
 * ## 三种视觉状态
 *
 * ```
 *   正在编辑的那一栋   → 颜色更实 + 白色边线（一眼看出"我在改的是这个"）
 *   有真轮廓的         → 半透明本色（知道"这栋搞定了"）
 *   还是占位的          → 灰色 + 更透明（知道"这栋还没补真轮廓"）
 * ```
 *
 * ⚠️ "还是占位的"判据是 **`shapeExact !== false`**，不是"有没有 shape"
 *    （2026-10-13 修）。"点选建楼"会自动生成一个矩形，它有 4 个点 ——
 *    只看点数的话，那些占位方块会被画成**彩色**，用户以为已经搞定了。
 *    而"哪几栋还没补轮廓"正是这个工具最需要回答的问题。
 *
 * ⚠️ 占位的楼**也要画** —— 不画的话用户看不出还有哪几栋没补。
 */
function nsSyncShapeLayer() {
  if (!nsMap || !window.TMap) return;

  const styles = {};
  const geoms = [];

  ns.buildings.forEach((b) => {
    if (!nsHasCoord(b)) return;   // 没坐标的楼在地球上没位置，画不出来

    const hasShape = Array.isArray(b.shape) && b.shape.length >= 3;
    // ★ "真轮廓"= 有轮廓**且**不是占位矩形
    const real = hasShape && b.shapeExact !== false;
    const editing = b.id === ns.activeId;
    const sid = 'st_' + (b.type || 'x') + (editing ? '_ed' : '') + (real ? '' : '_ghost');

    if (!styles[sid]) {
      if (!real) {
        // 占位：灰色 + 明显的描边 —— 一眼看出"这栋还没补，是个占位方块"
        //
        // ⚠️ 第一版填充只给了 0.22 的透明度，结果**几乎看不见** ——
        //    用户会以为"3D 那一层根本没画出来"（我自己就先这么以为了）。
        //    占位块的作用是"标出还有哪几栋没补"，看不见就等于没做。
        styles[sid] = new window.TMap.ExtrudablePolygonStyle({
          color: 'rgba(150,150,150,0.35)',
          extrudeHeight: nsExtrudeHeight(b),
          showBorder: true,
          borderColor: 'rgba(90,90,90,0.9)',
        });
      } else {
        styles[sid] = new window.TMap.ExtrudablePolygonStyle({
          color: nsTypeRgba(b.type, editing ? 0.88 : 0.72),
          extrudeHeight: nsExtrudeHeight(b),
          showBorder: true,
          // ⚠️ ExtrudablePolygonStyle 的边线**不支持设宽度**（永远 1 像素），
          //    所以"选中"只能靠**颜色**区分。
          borderColor: editing ? 'rgba(255,255,255,1)' : nsTypeRgba(b.type, 1),
        });
      }
    }

    // 没描过的用合成矩形（和插件侧同一套尺寸推算）
    const pts = hasShape ? b.shape : nsSynthRect(b);
    geoms.push({
      id: b.id,
      styleId: sid,
      paths: pts.map((p) => new window.TMap.LatLng(p[0], p[1])),
    });
  });

  if (!nsShapeLayer) {
    nsShapeLayer = new window.TMap.MultiPolygon({
      id: 'ns-shape-layer',
      map: nsMap,
      // ⚠️ zIndex 必须给 —— 不给的话这一层可能被底图的三维建筑盖住，
      //    表现是"图层建出来了、geometry 也在，但屏幕上什么都没有"。
      //    那种"数据对、渲染不出来"最难查（查数据全是好的）。
      zIndex: 100,
      styles,
      geometries: geoms,
    });
  } else {
    nsShapeLayer.setStyles(styles);
    nsShapeLayer.setGeometries(geoms);
  }
}

/** 没描轮廓时合成一个矩形（**和插件侧 `Campus3DWeb.buildPayload` 同一套算法**）。 */
function nsSynthRect(b) {
  const size = nsFootprintMeters(b);
  const dLat = (size.d / 2) / 110540;
  const dLng = (size.w / 2) / (111320 * Math.cos((b.lat * Math.PI) / 180));
  return [
    [b.lat - dLat, b.lng - dLng],
    [b.lat - dLat, b.lng + dLng],
    [b.lat + dLat, b.lng + dLng],
    [b.lat + dLat, b.lng - dLng],
  ];
}

/** 2D / 3D 切换（用的是地图**自带的** `setViewMode`）。 */
function nsToggleViewMode() {
  if (!nsMap) return;
  ns.is3d = !ns.is3d;
  try {
    if (ns.is3d) { nsMap.setViewMode('3D'); nsMap.setPitch(55); }
    else { nsMap.setViewMode('2D'); nsMap.setPitch(0); }
  } catch (e) { /* 老 SDK 没有这个能力就维持原样 */ }
  const btn = $('ns-btn-3d');
  if (btn) btn.textContent = ns.is3d ? '2D 视图' : '3D 视图';
}

/** 3D 建模那一块的文案（点数 + 下一步该干嘛）。 */
function nsRenderShapeHint() {
  // ★ 地图上那条提示和这里是同一件事的"外面那半"——
  //   这个函数是所有**模式变化**的汇聚点（点选开关、开始/结束描轮廓、
  //   设原点都调它），所以地图提示挂在这里，就不会出现
  //   "模式变了、地图上的字还是上一句"的情况。
  nsRenderMapTip();
  // ★ 工具条和批量条也挂在这个"模式汇聚点"上 ——
  //   `nsRenderShapeHint` 是所有模式变化的必经之处（切工具、选楼、描轮廓…），
  //   放这里就不会出现"模式变了、工具条还亮着上一个"。
  nsRenderToolbar();
  nsRenderBatchBar();
  // ★ 描轮廓那条浮动操作条（2026-10-13 晚）—— 和上面两个一样，
  //   挂在"模式汇聚点"上就不会出现"模式变了、它还亮着"。
  nsRenderShapeBar();
  // ★ 那三条形状滑条（2026-10-13 深夜）—— 同上，模式一变就跟着刷新
  nsRenderShapeDims();
  // ★ 标注层（2026-10-13 深夜）：模式一变就重画一遍
  //   （比如从「选择」切到「标注」，当前那条的高亮要跟着变）
  nsSyncMarks();

  const b = nsActiveBuilding();
  const n = b && Array.isArray(b.shape) ? b.shape.length : 0;
  const countEl = $('ns-shape-count');
  const hintEl = $('ns-shape-hint');
  const drawBtn = $('ns-btn-shape-draw');
  const pickBtn = $('ns-btn-picknew');

  if (countEl) countEl.textContent = n >= 3 ? (n + ' 点') : '矩形';
  if (drawBtn) drawBtn.textContent = nsShapeTargetId ? '结束描轮廓' : '开始描轮廓';

  // ★ 点选建楼那个大按钮：开着的时候要一眼看出来（它是"模式"，
  //   开着时点地图的含义和关着时完全不同 —— 不说清用户会误建一堆楼）
  if (pickBtn) {
    const on = (ns.tool === 'add');
    pickBtn.classList.toggle('on', on);
    const t = pickBtn.querySelector('.ns-cta-text small');
    if (t) {
      t.textContent = on
        ? '已开启 —— 在地图上点那栋楼就建一条（再点一下切回「选择」）'
        : 'OSM 里没有的楼 —— 点一下切到「加点」工具';
    }
  }

  if (!hintEl) return;
  if (nsShapeTargetId) {
    hintEl.innerHTML = n < 3
      ? `正在描 —— 已经点了 <b>${n}</b> 个点，至少还要再点 <b>${3 - n}</b> 个才能围出一个面。`
      : `正在描 —— 已经 <b>${n}</b> 个点。继续点可以加，或者点「结束描轮廓」。`;
    return;
  }
  hintEl.innerHTML = '沿着这栋楼的外墙点一圈，手机端就按这个形状拔起来。至少 3 个点。';
}

/** 开始/结束描轮廓。 */
function nsShapeToggle() {
  if (nsShapeTargetId) {
    nsShapeTargetId = null;
    nsSave();
    nsRenderShapeHint();
    nsSyncShapeLayer();
    nsStatus('轮廓描完了（' + ((nsActiveBuilding() || {}).shape || []).length + ' 个点）');
    return;
  }
  const b = nsActiveBuilding();
  if (!b) {
    nsToast('先在左边选一条，或在地图上双击新建一栋');
    return;
  }
  nsShapeTargetId = b.id;

  // ⚠️ **开始描之前，先把占位矩形清掉。**
  //
  // 不清的话新点会**追加**在矩形那 4 个角的后面 —— 描出来的是一个自交的
  // 怪形状（矩形套着用户画的一圈），而它看起来只是"有点怪"，
  // 用户多半会以为是自己的手抖，然后删掉重来。
  //
  // ⚠️ **真轮廓不清**：那可能是他上次辛苦描的，"我只是想再添一个点"是合理诉求。
  //    要整条重描，下面有"清空轮廓"。
  if (b.shapeExact === false && Array.isArray(b.shape) && b.shape.length) {
    b.shape = [];
    nsSave();
    nsRenderShapeHint();
    nsSyncShapeLayer();
    nsToast('原来的默认矩形已清掉 —— 在楼的外墙上一圈点下去（至少 3 个点）');
    return;
  }

  nsRenderShapeHint();
  nsToast('在楼的外墙上一圈点下去 —— 至少 3 个点');
}

/** 描轮廓时点一下地图 = 加一个顶点。 */
function nsShapeAddPoint(lat, lng) {
  const b = ns.buildings.find((x) => x.id === nsShapeTargetId);
  if (!b) {
    nsShapeTargetId = null;
    return;
  }
  if (!Array.isArray(b.shape)) b.shape = [];
  b.shape.push([Number(lat.toFixed(6)), Number(lng.toFixed(6))]);
  // ★ 够 3 个点就是**用户手描的真轮廓**了 —— 翻掉占位标记。
  //   不翻的话手机端会一直把它当"合成的"，用户描了半天却看到"轮廓是合成的"。
  if (b.shape.length >= 3) b.shapeExact = true;
  nsSave();
  nsRenderShapeHint();
  nsSyncShapeLayer();
  // ⚠️ 每加一个点都**不**改 `b.lat/lng` —— 楼的位置是"那一个点"，
  //    而轮廓是"一圈点"。混在一起的话，描完一圈楼会跑到最后一个顶点上。
  nsStatus(`轮廓 ${b.shape.length} 个点`);
}

/** 撤销最后一个顶点。 */
function nsShapeUndo() {
  // ⚠️ 描轮廓期间以 `nsShapeTargetId` 为准，**不是** `nsActiveBuilding()`。
  //    用户在描的过程中只要点了列表选了别的楼，后者就换人了 ——
  //    而浮动条上的「撤销一点」会撤到**另一栋**上去（静默地错）。
  const b = (nsShapeTargetId && ns.buildings.find((x) => x.id === nsShapeTargetId))
    || nsActiveBuilding();
  if (!b || !Array.isArray(b.shape) || !b.shape.length) {
    nsToast('这条还没有轮廓点');
    return;
  }
  b.shape.pop();
  nsSave();
  nsRenderShapeHint();
  nsSyncShapeLayer();
}


/**
 * 描轮廓那条浮动操作条（2026-10-13 晚）。
 *
 * ## 它解决的问题（用户："建模功能再优化优化，现在用起来不是很顺手"）
 *
 * ```
 *   原来：右栏「开始描轮廓」→ 回地图点一圈 → 回右栏「结束描轮廓」
 *         而过程中的反馈只在**页面最底下那行状态**里（`轮廓 5 个点`）
 * ```
 *
 * 描的时候眼睛和手都在地图上，那个来回本身就是不顺手；而底栏那行字
 * 描的人根本不会往那儿看。现在把它放在手边。
 *
 * ⚠️ 它由 [nsRenderShapeHint] 驱动 —— 那是所有**模式变化**的汇聚点
 *    （切工具 / 选楼 / 开始结束描轮廓都经过它），挂在那儿就不会出现
 *    "模式变了、这条还亮着"。
 */
function nsRenderShapeBar() {
  const bar = $('ns-shape-bar');
  if (!bar) return;
  const b = ns.buildings.find((x) => x.id === nsShapeTargetId);
  if (!b) { bar.classList.add('hidden'); return; }
  bar.classList.remove('hidden');
  const n = Array.isArray(b.shape) ? b.shape.length : 0;
  const txt = $('ns-shape-bar-text');
  if (txt) {
    txt.innerHTML = '描「<b>' + nsEsc(b.name || '（未命名）') + '</b>」的轮廓 · 已点 <b>'
      + n + '</b> 个点' + (n < 3 ? '（至少要 3 个）' : '');
  }
  // ⚠️ 点数不够时「完成」**禁用**（而不是点了才告诉用户不够）——
  //    灰着的按钮本身就是一句说明。
  const done = $('ns-shape-done');
  if (done) done.disabled = n < 3;
}

/** 描完了：退出描轮廓模式（**不删点**）。 */
function nsShapeFinish() {
  const b = ns.buildings.find((x) => x.id === nsShapeTargetId);
  const n = b && Array.isArray(b.shape) ? b.shape.length : 0;
  if (n < 3) { nsToast('至少要 3 个点才围得出一圈 —— 再点几个'); return; }
  nsShapeTargetId = null;
  nsSave();
  nsRenderAll();
  nsStatus('「' + ((b && b.name) || '（未命名）') + '」的轮廓描好了（' + n + ' 个点）');
  nsToast('轮廓描好了 —— 手机上就是它这个形状');
}

/** 把浮动条上那两个按钮接上（页面初始化时调一次）。 */
function nsWireShapeBar() {
  const undo = $('ns-shape-undo');
  const done = $('ns-shape-done');
  if (undo && !undo.dataset.wired) {
    undo.dataset.wired = '1';
    undo.addEventListener('click', () => nsShapeUndo());
  }
  if (done && !done.dataset.wired) {
    done.dataset.wired = '1';
    done.addEventListener('click', () => nsShapeFinish());
  }
}

/** 清空轮廓。 */
function nsShapeClear() {
  const b = nsActiveBuilding();
  if (!b) return;
  if (Array.isArray(b.shape) && b.shape.length &&
      !confirm('清空「' + (b.name || '（未命名）') + '」的轮廓？\n\n清掉之后手机端会合成一个矩形。')) return;
  b.shape = [];
  nsShapeTargetId = null;
  nsSave();
  nsRenderShapeHint();
  nsSyncShapeLayer();
  nsStatus('轮廓已清空');
}

/**

/**
 * 一栋楼的**地面尺寸**（米）—— 从分层数据推，推不出来就用默认值。
 *
 * ⚠️⚠️ **必须和插件侧的 `Iso3D.footprint()` 是同一套算法。**
 *    两边不一致的话会出现"工具里看着是方的、手机上是长的"，
 *    而用户会以为是 3D 渲染的问题 —— 实际上是两个地方各算了一遍。
 *    （对应的常量：默认 24×14 米，归一化跨度夹在 0.35~1.0。）
 */
function nsFootprintMeters(b) {
  const DEFAULT_W = 24;
  const DEFAULT_D = 14;
  const rooms = [];
  (b.floors || []).forEach((f) => (f.rooms || []).forEach((r) => rooms.push(r)));
  if (!rooms.length) return { w: DEFAULT_W, d: DEFAULT_D };
  let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
  rooms.forEach((r) => {
    minX = Math.min(minX, r.x - r.w / 2);
    maxX = Math.max(maxX, r.x + r.w / 2);
    minY = Math.min(minY, r.y - r.h / 2);
    maxY = Math.max(maxY, r.y + r.h / 2);
  });
  const spanX = Math.min(1, Math.max(0.35, maxX - minX));
  const spanY = Math.min(1, Math.max(0.35, maxY - minY));
  return { w: spanX * DEFAULT_W, d: spanY * DEFAULT_D };
}

/** 把轮廓那几个按钮接上。 */
function nsWireShape() {
  $('ns-btn-shape-draw').addEventListener('click', () => nsShapeToggle());
  $('ns-btn-shape-undo').addEventListener('click', () => nsShapeUndo());
  $('ns-btn-shape-clear').addEventListener('click', () => nsShapeClear());
  $('ns-btn-shape-rect').addEventListener('click', () => nsShapeRect());

  // ★ 点选建楼（2026-10-13 第二轮：它现在只是"加点"这个工具的一个入口）
  //
  // ⚠️ 它**不再是一个自己开合的开关**，而是"切到加点工具 / 切回选择工具"。
  //    两套开关并存的话（一个布尔 + 一个工具表），状态一定会漂 ——
  //    而"按钮亮着、行为却不是那个"是最难查的一类。
  const pickBtn = $('ns-btn-picknew');
  if (pickBtn) {
    pickBtn.addEventListener('click', () => {
      nsSetTool(ns.tool === 'add' ? 'select' : 'add');
    });
  }

  // ★★★ 二次识别地名（2026-10-13）
  //
  // 用户的原话："加入一个自动识别地名的功能……不是从那个 O 什么的那个公开数据
  // 里面提取，而是通过那个模型覆盖的地图的地名进行自动识别。"
  //
  // ⚠️ 用**类名**接（和 OSM 那几个按钮同一个套路）：左边面板和批量条里各有一个，
  //    用 id 接的话第二个点不动 —— 而"点了没反应"是最难查的一类。
  document.querySelectorAll('.ns-poi-btn').forEach((el) => {
    if (el.dataset.wired) return;
    el.dataset.wired = '1';
    el.addEventListener('click', () => { nsPoiIdentify(); });
  });

  // ★ 批量操作条上那几个按钮
  const batchBar = $('ns-batch-bar');
  if (batchBar && !batchBar.dataset.wired) {
    batchBar.dataset.wired = '1';
    batchBar.addEventListener('click', (ev) => {
      const b = ev.target.closest('[data-batch]');
      if (!b) return;
      nsBatchRun(b.getAttribute('data-batch'));
    });
  }

  // ★★★ 从 OSM 导入（**主路径**，2026-10-13）
  //
  // ⚠️ 用**类名**接，因为有两个入口（左边面板那个常驻的 + 编辑器里"再导一次"）。
  //    用 id 接的话第二个按钮点不动 —— 而"点了没反应"是最难查的一类。
  document.querySelectorAll('#ns-btn-osm, .ns-osm-again').forEach((el) => {
    el.addEventListener('click', () => { nsImportOsm(); });
  });

  // ★ 清掉没名字的（2026-10-13）—— 用户要的"只保留被标注的数据"。
  //    ⚠️ 这个按钮是 `hidden` 的（一栋都没得删时藏起来，见 nsRefreshPrune），
  //       所以监听器要在**绑定阶段**就挂上 —— 挂晚了它已经显示出来了才补挂，
  //       中间那段时间点它是没反应的。
  $('ns-btn-prune')?.addEventListener('click', () => { nsDropUnnamed(); });

  // ★ 自动补真实轮廓（2026-10-13）—— 用户要的"模型自动产生，不用我自己建"。
  $('ns-btn-fill')?.addEventListener('click', () => { nsFillShapes(); });

  // ★ 形状微调
  document.querySelectorAll('[data-nudge]').forEach((el) => {
    el.addEventListener('click', () => nsNudgeShape(el.getAttribute('data-nudge')));
  });

  // ★ 地图右上角那两个
  const btn3d = $('ns-btn-3d');
  if (btn3d) btn3d.addEventListener('click', () => nsToggleViewMode());
  const btnFit = $('ns-btn-fit');
  if (btnFit) {
    btnFit.addEventListener('click', () => {
      if (!nsMap) return;
      const pts = ns.buildings.filter(nsHasCoord);
      if (!pts.length) {
        nsToast('还没有标过坐标的楼');
        return;
      }
      try {
        const lats = pts.map((b) => b.lat);
        const lngs = pts.map((b) => b.lng);
        nsMap.fitBounds(new window.TMap.LatLngBounds(
          new window.TMap.LatLng(Math.min.apply(null, lats), Math.min.apply(null, lngs)),
          new window.TMap.LatLng(Math.max.apply(null, lats), Math.max.apply(null, lngs)),
        ), { padding: 80 });
      } catch (e) {
        nsToast('装不下 —— 手动缩一下');
      }
    });
  }
}

/* ------------------------------------------------------------------ 一键发布 */

/**
 * 管理员 token 在 localStorage 里的键。
 *
 * ⚠️ 它和"地图 Key"那个是两个完全不同的东西，键名必须能一眼分开：
 *    `ns.mapkey` 是给腾讯的（公开的、会被分发出去），
 *    `ns.pubtoken` 是**我们自己的管理员会话**（泄露 = 别人能发/删包）。
 */
/**
 * 发布弹层的**界面状态**。
 *
 * ## ⚠️ 为什么要有 `force` / `busy` 这两个标志，而不是直接改按钮的 onclick
 *
 * 第一版是「初始化时 addEventListener，之后再给按钮塞 onclick」——
 * 结果**一次点击跑了两遍** `nsPublishGo()`：
 *
 * ```
 *   第一遍 → 发成功，显示"授权码 xxx"
 *   第二遍 → 同一个包已经存在了 → 409 → 把"已经有一个包了"糊上去
 * ```
 *
 * 用户看到的是**成功和报错同时出现**，而真实结果是成功的 ——
 * 这种"假报错"比不报还坏：他会以为没发出去，然后再点一次。
 *
 * ⚠️ 所以**只留一个入口**（初始化时那一个 addEventListener），
 *    状态全放这里，按钮只管显示。
 */
const NS_PUB_TOKEN_KEY = 'ns.pubtoken';

const nsPub = {
  /** 这次点击是"确认覆盖"吗（409 之后用户再点一次）。 */
  force: false,
  /** 正在发（防连点：连点两下也是上面那个 bug 的形状）。 */
  busy: false,
  /** 上一个"确认覆盖"的点击是不是刚发生过（用来在成功后复位 force）。 */
  justPublished: false,
};

/**
 * 这一版**发过没有**（顶栏第二步的那个勾）。
 *
 * ⚠️ 只活在内存里（不进 localStorage）。理由：刷新页面之后用户看到的应该是
 *    "当前这份草稿的状态"，而不是"历史上发过"。发过的那一版和现在手上这份
 *    可能已经完全不一样了 —— 那时候还挂着勾是**在骗人**。
 */
let nsPublished = false;

/** 发布前的体检。 */
function nsPublishCheck() {
  const id = String(ns.packId || '').trim();
  const bs = ns.buildings.filter((b) => String(b.name || '').trim());
  const placeholder = bs.filter(
    (b) => !Array.isArray(b.shape) || b.shape.length < 3 || b.shapeExact === false,
  ).length;
  const problems = [];
  if (!/^[a-z][a-z0-9-]{1,47}$/.test(id)) {
    problems.push('「包标识」还没填好（要小写字母开头，a-z0-9- 组成，2~48 位）');
  }
  if (!bs.length) {
    problems.push('一栋有名字的楼都没有 —— 导出去用户也搜不到东西');
  }
  return { id, named: bs.length, placeholder, problems };
}

/** 打开发布弹层。 */
function nsPublishOpen() {
  const c = nsPublishCheck();
  $('ns-pub-id').textContent = c.id || '（还没填）';
  $('ns-pub-name').textContent = String(ns.name || '').trim() || '（还没填）';
  $('ns-pub-count').textContent = c.named ? (c.named + ' 栋有名字的') : '0 栋';

  // ⚠️ 体检结论**必须显眼**：占位矩形导出去就是"手机上一堆方盒子"，
  //    而那正是用户这一轮报的问题。发之前拦一下，比发完再解释便宜得多。
  const box = $('ns-pub-check');
  if (c.problems.length) {
    box.className = 'ns-pub-check warn';
    box.innerHTML = '还不能发布：<br />· ' + c.problems.map(nsEsc).join('<br />· ');
  } else if (c.placeholder) {
    box.className = 'ns-pub-check warn';
    box.innerHTML = '可以发。但有 <b>' + c.placeholder + '</b> 栋的轮廓还是<b>默认矩形</b>'
      + '（不是真形状）—— 手机上看就是方盒子。建议先点左边的「自动补上真实轮廓」。';
  } else {
    box.className = 'ns-pub-check';
    box.innerHTML = '可以发 —— ' + c.named + ' 栋，全部带真实轮廓。';
  }

  $('ns-pub-msg').textContent = '';
  $('ns-pub-msg').className = 'ns-pub-msg';
  $('ns-pub-done').classList.add('hidden');

  // 已经登录过就不显示登录那两块
  const logged = !!localStorage.getItem(NS_PUB_TOKEN_KEY);
  $('ns-pub-login').classList.toggle('hidden', logged);
  $('ns-btn-pub-go').textContent = logged ? '发布' : '登录并发布';
  $('ns-btn-pub-go').disabled = c.problems.length > 0;
  // ⚠️ 这里**不要**再塞 onclick —— 入口只有一个（初始化时那个 addEventListener）。
  nsPub.force = false;
  nsPub.busy = false;

  $('ns-publish').classList.remove('hidden');
  if (!logged) setTimeout(() => $('ns-pub-user').focus(), 60);
}

function nsPublishClose() {
  $('ns-publish').classList.add('hidden');
}

/** 往弹层里发一句提示（不是 toast —— 用户正在这个框里操作）。 */
function nsPubMsg(text, kind) {
  const el = $('ns-pub-msg');
  el.textContent = text;
  el.className = 'ns-pub-msg' + (kind ? ' ' + kind : '');
}

/**
 * 登录换 token。
 *
 * ⚠️ 用 `/plugin/navstudio/api/auth/login` 而不是让用户去别处复制一串会话令牌：
 *    粘贴长令牌对人不友好，而且它会过期 —— 过期的表现是"发布失败 401"，
 *    用户完全不知道该怎么办。用账号密码登，过期了重登一次，心智模型清楚。
 */
async function nsPubLogin() {
  const name = $('ns-pub-user').value.trim();
  const pass = $('ns-pub-pass').value;
  if (!name || !pass) {
    nsPubMsg('用户名和密码都要填', 'bad');
    return null;
  }
  try {
    const res = await fetch('/plugin/navstudio/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, pass }),
    });
    const j = await res.json().catch(() => null);
    if (!res.ok || !j || !j.token) {
      nsPubMsg(
        (j && (j.error || j.message)) || ('登录失败（HTTP ' + res.status + '）—— 用户名或密码不对？'),
        'bad',
      );
      return null;
    }
    if (!j.user || !j.user.is_admin) {
      // ⚠️ 这一条要单独说：能登录但**不是管理员**。
      //    笼统报"登录失败"会让人一直试密码，而问题根本不在密码上。
      nsPubMsg('这个账号不是管理员 —— 发布要管理员身份。找学校这边的管理员来发。', 'bad');
      return null;
    }
    localStorage.setItem(NS_PUB_TOKEN_KEY, j.token);
    $('ns-pub-pass').value = '';
    return j.token;
  } catch (e) {
    nsPubMsg('连不上服务器：' + (e && e.message ? e.message : e), 'bad');
    return null;
  }
}

/**
 * 真正发出去。
 *
 * ⚠️ **没有参数** —— 是不是"确认覆盖"看 [nsPub].force。
 *    第一版是给按钮塞 `onclick = () => nsPublishGo(true)`，
 *    而初始化时已经挂了一个 addEventListener —— 一次点击跑两遍。
 *    第一遍发成功、第二遍撞 409，于是"成功"和"已经有一个包了"同时显示。
 */
async function nsPublishGo() {
  // 已经发完了 → 这一下点击的含义就是"关掉"
  if (nsPub.justPublished) {
    nsPublishClose();
    return;
  }
  // 防连点：连点两下和上面那个 bug 是同一个形状
  if (nsPub.busy) return;

  const force = nsPub.force;
  const c = nsPublishCheck();
  if (c.problems.length) {
    nsPubMsg(c.problems.join('；'), 'bad');
    return;
  }

  const btn = $('ns-btn-pub-go');
  nsPub.busy = true;
  btn.disabled = true;
  const label = '发布';
  btn.textContent = '发布中…';

  try {
    let token = localStorage.getItem(NS_PUB_TOKEN_KEY);
    if (!token) {
      token = await nsPubLogin();
      if (!token) {
        nsPub.busy = false;
        btn.disabled = false;
        btn.textContent = '登录并发布';
        return;
      }
    }

    nsSave();
    const pack = nsBuildPack(true);   // 含 Key：分发给别人的那份要带着
    const res = await fetch('/plugin/navstudio/api/admin/navpacks/upload' + (force ? '?replace=1' : ''), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + token },
      body: JSON.stringify(pack),
    });
    const j = await res.json().catch(() => null);

    if (res.status === 409) {
      // ⚠️ 覆盖一个授权码 = **所有已经拿到这个码的人下次导入都会换成这一版**。
      //    所以要显式确认，并把"现在是哪一版"摊出来 ——
      //    只说"已存在"的话，用户不知道自己在覆盖什么。
      const prev = (j && j.detail && j.detail.prev) || (j && j.prev);
      nsPubMsg('授权码「' + c.id + '」已经有一个包了'
        + (prev ? '（现在是：' + prev.name + ' / v' + prev.version + '，' + prev.buildings + ' 条）' : '')
        + '。换掉它 = 所有拿这个码的人下次导入都会跟着换。确认要换就再点一次。', 'bad');
      nsPub.busy = false;
      btn.disabled = false;
      btn.textContent = '确认覆盖';
      nsPub.force = true;
      return;
    }

    if (!res.ok) {
      const why = (j && (j.error || j.message)) || ('HTTP ' + res.status);
      // 401/403 = token 过期。清掉它，下次点发布会重新要求登录；
      // 不清的话用户会一直看到同一个错，而完全不知道该干嘛。
      if (res.status === 401 || res.status === 403) {
        localStorage.removeItem(NS_PUB_TOKEN_KEY);
        nsPubMsg('登录过期了 —— 关掉这个框，重新点「一键发布」再登一次。', 'bad');
      } else {
        nsPubMsg('发布失败：' + why, 'bad');
      }
      nsPub.busy = false;
      btn.disabled = false;
      btn.textContent = label;
      return;
    }

    // ---- 成功
    const code = (j && j.auth_code) || c.id;
    const dl = (j && j.download) || '';
    $('ns-pub-code').textContent = code;
    $('ns-pub-done').classList.remove('hidden');
    $('ns-pub-how').innerHTML = '手机上打开「导航」插件 → 导入标注包 → 粘贴这串码即可。'
      + (dl ? '<br />（下载地址：<code>' + nsEsc(dl) + '</code>）' : '');
    nsPubMsg(force ? '已覆盖发布成功' : '发布成功', 'good');
    btn.textContent = '完成';
    btn.disabled = false;
    nsPub.busy = false;
    nsPub.force = false;
    nsPub.justPublished = true;
    nsToast('已发布「' + (ns.name || code) + '」');
    nsRenderSteps(true);   // 第二步打勾
  } catch (e) {
    nsPubMsg('发布失败：' + (e && e.message ? e.message : e), 'bad');
    nsPub.busy = false;
    btn.disabled = false;
    btn.textContent = label;
  }
}

/**
 * 顶栏那条两步进度。
 *
 * ⚠️ 它只反映**这一步做没做**，不假装知道用户想干什么 ——
 *    回答的是"我走到哪了"，不是"你必须先做这个"。
 */
function nsRenderSteps(published) {
  const el1 = $('ns-step-1');
  const el2 = $('ns-step-2');
  if (!el1 || !el2) return;
  // ⚠️ "这一版发过没有"要**记住**，不能靠参数传 ——
  //    nsRenderAll 也会调这个函数（楼栋一增删就要重画），
  //    参数默认 undefined 的话，发布完随便改栋楼，第二步的勾就掉了。
  if (published === true) nsPublished = true;
  const hasBuildings = ns.buildings.filter((b) => String(b.name || '').trim()).length > 0;
  el1.className = 'ns-step' + (hasBuildings ? ' done' : ' on');
  el2.className = 'ns-step' + (nsPublished ? ' done' : (hasBuildings ? ' on' : ''));
}

/**
 * 地图上那条操作提示（2026-10-13 改成**动态**的）。
 *
 * ## 为什么要改
 *
 * 原文是一句写死的话：
 *
 * > 单击地图 = 取这个点（填进正在编辑的那条） · 双击 = 在这个点新建一栋
 *
 * 而**当时实际行为不是那样** —— 那时候 `ns.pickNew` 默认就是**开着**的，
 * 单击地图 = **新建一栋**。用户照那句话做（以为只是取点），
 * 结果一路点下去建了一堆楼，而他不知道是自己点错了还是程序坏了。
 *
 * ★ 2026-10-13 第二轮：那个布尔已经**换成了工具表**（见 `NS_TOOLS`），
 *   而**默认工具是「选择」** —— 单击再也不会误建。
 *
 * ⚠️ 所以这条提示必须**根据当前模式说当前的话**，而不是背一段固定文案。
 *    "界面上写着 A、实际做的是 B" 是最伤人的一类问题：
 *    用户会把自己做错的事记成"这软件有问题"。
 */
function nsRenderMapTip() {
  const el = $('ns-map-tip');
  if (!el) return;
  if (nsShapeTargetId || ns.tool === 'draw') {
    const n = ((nsActiveBuilding() || {}).shape || []).length;
    el.innerHTML = '<b>描轮廓</b> —— 沿着外墙点一圈（已点 <b>' + n + '</b> 个）。'
      + '至少 3 个点；点左上角的「选择」可以退出这个模式。';
    return;
  }
  if (ns.pickMode === 'origin' || ns.tool === 'origin') {
    el.innerHTML = '<b>点一下设标定原点</b>（一般点校门口）。';
    return;
  }
  if (ns.tool === 'add') {
    el.innerHTML = '<b>点一下那栋楼</b> —— 会自动建成一条，并<b>自动找它的真实轮廓</b>。'
      + '（不想误建就切回左上角的「选择」）';
    return;
  }
  if (ns.tool === 'batch') {
    el.innerHTML = '<b>点标记勾选/取消</b> —— 勾完用下面那条批量操作。';
    return;
  }
  // ---- 默认：选择
  el.innerHTML = '<b>点标记 = 选中它</b>（右边的编辑面板会跟着变）· '
    + '<b>按住标记拖 = 把它挪到正确位置</b>。想新建就切到左上角的「加点」。';
}

/* ------------------------------------------------------------------ 顶栏接线 */

/** 「⋯ 更多」那个下拉。开/关 + 里面每一项回到原来的按钮上。 */
function nsWireMore() {
  const btn = $('ns-btn-more');
  const menu = $('ns-more-menu');
  if (!btn || !menu || btn.dataset.wired) return;
  btn.dataset.wired = '1';

  const close = () => {
    menu.classList.add('hidden');
    btn.setAttribute('aria-expanded', 'false');
  };
  btn.addEventListener('click', (ev) => {
    ev.stopPropagation();       // 别让 document 那个"点外面就关"立刻把它关掉
    const willOpen = menu.classList.contains('hidden');
    menu.classList.toggle('hidden', !willOpen);
    btn.setAttribute('aria-expanded', String(willOpen));
  });
  // ⚠️ 点菜单外面就收 —— 否则那个菜单会一直挂着，挡住下面的按钮
  document.addEventListener('click', (ev) => {
    if (menu.classList.contains('hidden')) return;
    if (menu.contains(ev.target)) return;
    close();
  });

  // 每一项都"转拨"给原来那个按钮 —— 逻辑一处不写第二遍。
  // ⚠️ 不能改成直接写新的处理函数：那些按钮的实现各有各的细节
  //    （发布要登录、导出要分含不含 Key），重复一遍必然漂。
  menu.querySelectorAll('[data-act]').forEach((b) => {
    b.addEventListener('click', () => {
      const target = {
        help: 'ns-btn-help',
        import: 'ns-btn-import',
        save: 'ns-btn-savejson',
        export: 'ns-btn-export-nokey',
        exportkey: 'ns-btn-export',
      }[b.getAttribute('data-act')];
      close();
      const el = $(target);
      if (el) el.click();          // 冒泡给真实的处理器
    });
  });
}

/** 顶栏那两个 ↶ ↷（和 Ctrl+Z 是同一件事的两个入口）。 */
function nsWireHistory() {
  const u = $('ns-btn-undo');
  const r = $('ns-btn-redo');
  if (u && !u.dataset.wired) { u.dataset.wired = '1'; u.addEventListener('click', () => nsUndo()); }
  if (r && !r.dataset.wired) { r.dataset.wired = '1'; r.addEventListener('click', () => nsRedo()); }
}

/** 左栏那个搜索框 —— 只重画列表，别的一动不动。 */
function nsWireListSearch() {
  const q = $('ns-list-q');
  if (!q || q.dataset.wired) return;
  q.dataset.wired = '1';
  // ⚠️ 只在**松手/回车**时过滤，不在 input 里过滤：155 栋时每个字重画上千个节点，
  //    字一多就卡。而这个框查的都是"确定要查的词"，不是边打边缩。
  q.addEventListener('change', () => nsRenderList());
  q.addEventListener('search', () => nsRenderList());
  q.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') { ev.preventDefault(); nsRenderList(); }
    if (ev.key === 'Escape') { q.value = ''; nsRenderList(); }
  });
}

/* ==========================================================================
   地图标注 marks（2026-10-13 深夜加）
   ==========================================================================

   用户的原话：

   > "还要允许我在地图上面添加文字说明……除此之外，还要让我添加一些批注信息，
   >  除了那种常见的气泡框以外呢，还要再加一个类似于**门牌**的那种小小的
   >  文字类型的叠加层，然后我就可以标注具体的楼号什么的，然后再添加一些
   >  **贴纸**，最好是**可拉伸**的那种……我还要再标注一下**入口以及楼梯**，
   >  包括上楼的楼梯以及路上的楼梯。"

   ## 六种 kind（一张表说完）

   ```
     text     纯文字（带描边，压在地图上任何地方都看得清）
     bubble   气泡框：圆角 + 小尾巴 —— 就是用户说的"常见的气泡框"
     plate    门牌：白底黑字的小牌子，**可拉伸**（标楼号 A3 / B2 用）
     sticker  贴纸：一个 emoji，**可拉伸 + 可旋转**
     entry    入口：绿点 + 文字（校门 / 侧门 / 车库入口）
     stairs   楼梯：阶梯图标 + 文字（"上二楼" / "下负一层"）
   ```

   ⚠️ 六种**共用同一套字段**，渲染时只是样式不同 —— 这样加第七种的时候
     不用动数据层、不用动格式、不用动门禁，只加一段 CSS。

   ## 为什么单独一层，不塞进 `buildings`

   ```
     楼栋    回答"这是哪栋楼"—— 有名字、有轮廓、能被日程引用
     marks   回答"这上面写了什么字"—— 没有名字、没有轮廓、日程里查不到
   ```

   混在一起的话，手机端"按类别筛楼"会把这些牌子一起筛进去。

   ## ⚠️ 单位：`w` / `h` 是**米**，0 表示"按内容自动"
   ========================================================================== */

/** 六种标注的类型表（`[值, 名字, 说明]`）—— 和 NS_TOOLS 同一个写法。 */
const NS_MARK_KINDS = [
  ['text', '文字', '就一行字，带描边，压在地图上哪都看得清'],
  ['bubble', '气泡', '圆角气泡 + 小尾巴，适合写一句说明'],
  ['plate', '门牌', '白底小牌子，可以拉宽 —— 标楼号（A3 / B2）用这个'],
  ['sticker', '贴纸', '一个图形（⭐ 🚻 🅿），可以拉伸、可以转角度'],
  ['entry', '入口', '校门 / 侧门 / 车库入口'],
  ['stairs', '楼梯', '上楼的、路上的都算'],
];

/** 每种标注的默认颜色（放下去就是这个色，之后能改）。 */
const NS_MARK_COLOR = {
  text: '#ffffff',
  bubble: '#f59e0b',
  plate: '#185fa5',
  sticker: '#185fa5',
  entry: '#0f6e56',
  stairs: '#7c3aed',
};

const nsMarkKindLabel = (k) =>
  (NS_MARK_KINDS.find((x) => x[0] === k) || ['', k, ''])[1];

/**
 * 建一条新标注（**唯一的构造点** —— 和 `nsNewRoom` 一样的规矩）。
 *
 * ⚠️ 默认值写在一处：以后加字段只改这里，不用满世界找 `{ kind: ... }`。
 */
function nsNewMark(kind, lat, lng) {
  const k = NS_MARK_KINDS.some((x) => x[0] === kind) ? kind : 'text';
  return {
    id: 'k_' + Date.now().toString(36) + Math.floor(Math.random() * 1000).toString(36),
    kind: k,
    lat: Number(Number(lat).toFixed(6)),
    lng: Number(Number(lng).toFixed(6)),
    text: '',
    w: 0,          // 米；0 = 按内容自动
    h: 0,
    rot: 0,        // 度
    color: NS_MARK_COLOR[k] || '#185fa5',
    icon: k === 'sticker' ? '⭐' : (k === 'entry' ? '🚪' : (k === 'stairs' ? '🪜' : '')),
  };
}

/** 过一遍规范化（读草稿 / 导入文件之后调，保证字段都在）。 */
function nsNormalizeMark(m) {
  const out = nsNewMark(m && m.kind, (m && m.lat) || 0, (m && m.lng) || 0);
  if (!m) return out;
  if (m.id) out.id = String(m.id);
  out.text = String(m.text == null ? '' : m.text);
  out.w = Number(m.w) || 0;
  out.h = Number(m.h) || 0;
  out.rot = Number(m.rot) || 0;
  if (m.color) out.color = String(m.color);
  if (m.icon != null) out.icon = String(m.icon);
  return out;
}

/** 现在选中的是哪条标注（'' = 没选）。 */
function nsActiveMark() {
  return ns.marks.find((m) => m.id === ns.activeMarkId) || null;
}

/** 新建一条标注（放在地图中心 / 或给定坐标），并选中它。 */
function nsAddMark(kind, lat, lng) {
  let la = lat;
  let ln = lng;
  if (!Number.isFinite(la) || !Number.isFinite(ln)) {
    // 没给坐标就放在**当前视野中心** —— 比"放在 0,0"有用得多
    try {
      const c = nsMap && nsMap.getCenter();
      la = c ? c.getLat() : 21.15;
      ln = c ? c.getLng() : 110.30;
    } catch (e) { la = 21.15; ln = 110.30; }
  }
  const m = nsNewMark(kind, la, ln);
  ns.marks.push(m);
  ns.activeMarkId = m.id;
  ns.activeId = '';            // 选标注就不选楼栋（两套编辑面板互斥）
  nsSave('加标注');
  nsRenderAll();
  nsSyncMarks();
  nsStatus('放了一个「' + nsMarkKindLabel(m.kind) + '」—— 在右边写内容，或者按住它拖到别处');
  return m;
}

/** 删掉一条标注。 */
function nsDeleteMark(id) {
  const m = ns.marks.find((x) => x.id === id);
  if (!m) return;
  const label = m.text || nsMarkKindLabel(m.kind);
  if (!confirm('删掉这个标注「' + label + '」？')) return;
  ns.marks = ns.marks.filter((x) => x.id !== id);
  if (ns.activeMarkId === id) ns.activeMarkId = '';
  nsSave('删标注');
  nsRenderAll();
  nsSyncMarks();
  nsStatus('已删掉标注「' + label + '」');
}

/** 选中一条标注（地图上点它、或在右侧切类型时用）。 */
function nsSelectMark(id) {
  ns.activeMarkId = id || '';
  if (id) ns.activeId = '';    // 互斥
  nsSave();
  nsRenderAll();
  nsSyncMarks();
}

/* ---------------------------------------------------------------- 标注渲染 */

/**
 * 一个标注在**当前缩放级别**下该画多大（像素）。
 *
 * ## 为什么不用"缩放级别 → 米/像素"的公式
 *
 * 腾讯那个换算表（zoom 18 ≈ 0.6 米/像素）是**他们自己的口径**，
 * 换个底图类型、换个视角（3D 俯仰）就不准了。而 `projectToContainer`
 * 是现成的、和屏幕上看到的一模一样：
 *
 * ```
 *   往东挪 1 米 → 投影 → 看横坐标差了多少像素
 * ```
 *
 * ⚠️ 经度方向的"1 米"要按当前纬度折算（`111320·cos(lat)`），
 *    在湛江（21°N）和漠河（53°N）差着一倍多。
 */
function nsMetersToPx(meters, lat) {
  if (!nsMap || !window.TMap || !meters) return 0;
  try {
    const c = nsMap.getCenter();
    const lng0 = c.getLng();
    const dLng = meters / (111320 * Math.cos((lat * Math.PI) / 180));
    const p1 = nsMap.projectToContainer(new TMap.LatLng(lat, lng0));
    const p2 = nsMap.projectToContainer(new TMap.LatLng(lat, lng0 + dLng));
    return Math.abs(p2.x - p1.x);
  } catch (e) { return 0; }
}

/** 一条标注的显示文字（没写内容时用类型名兜底，免得画出一片空白）。 */
function nsMarkLabel(m) {
  if (m.kind === 'sticker') return m.icon || '⭐';
  const t = String(m.text || '').trim();
  if (t) return t;
  if (m.kind === 'plate') return 'A1';
  return nsMarkKindLabel(m.kind);
}

/** 重建全部标注的 DOM（增删改之后调）。 */
function nsSyncMarks() {
  const layer = $('ns-mark-layer');
  if (!layer || !nsMap || !window.TMap) return;
  layer.textContent = '';
  ns.marks.forEach((m) => {
    const el = document.createElement('div');
    el.className = 'ns-mark ns-mark-' + m.kind + (m.id === ns.activeMarkId ? ' on' : '');
    el.dataset.id = m.id;
    el.textContent = nsMarkLabel(m);
    el.style.setProperty('--mk', m.color || '#185fa5');
    // 尺寸：只有写了 w/h 才设（0 = 按内容自动）
    if (Number(m.w) > 0) el.style.width = nsMetersToPx(Number(m.w), m.lat).toFixed(1) + 'px';
    if (Number(m.h) > 0) el.style.height = nsMetersToPx(Number(m.h), m.lat).toFixed(1) + 'px';
    const rot = Number(m.rot) || 0;
    el.style.transform = 'translate(-50%, -50%)' + (rot ? (' rotate(' + rot + 'deg)') : '');
    layer.appendChild(el);
  });
  nsPlaceMarks();
}

/**
 * 把每条标注摆到它的经纬度对应的屏幕位置。
 *
 * ⚠️ 单独一个函数：地图**每动一下**都要重排（拖、缩、转、倾斜），
 *    而重建 DOM 只有增删改才需要。两件事分开，拖动时才不卡。
 */
function nsPlaceMarks() {
  const layer = $('ns-mark-layer');
  if (!layer || !nsMap || !window.TMap) return;
  ns.marks.forEach((m) => {
    const el = layer.querySelector('[data-id="' + m.id + '"]');
    if (!el) return;
    let q = null;
    try { q = nsMap.projectToContainer(new TMap.LatLng(m.lat, m.lng)); } catch (e) { return; }
    if (!q) return;
    el.style.left = q.x.toFixed(1) + 'px';
    el.style.top = q.y.toFixed(1) + 'px';
  });
}

/**
 * 标注的拖动 + 点选（2026-10-13 深夜）。
 *
 * ## 为什么这次不用地图事件
 *
 * 楼栋那边必须用 `nsMap.on('mousedown')`，因为楼栋**没有自己的 DOM**
 * （它们是 GL 里的几何体）。而标注**就是 DOM 元素** —— 直接在它身上听事件
 * 更准、更简单，也不用再算命中。
 *
 * ⚠️ 拖动期间同样要 `setDraggable(false)`，而且**四路兜底**还回来
 *    （`document` 的 mouseup / pointerup / blur）—— 理由见 [nsReleaseDrag]：
 *    松手事件一丢，地图就永久拖不动了。这个坑今天踩过一次，不踩第二次。
 */
function nsWireMarkDrag() {
  const layer = $('ns-mark-layer');
  if (!layer || layer.dataset.wired) return;
  layer.dataset.wired = '1';

  let drag = null;
  const finish = () => {
    if (!drag) { nsReleaseDrag(); return; }
    const moved = drag.moved;
    const m = ns.marks.find((x) => x.id === drag.id);
    drag = null;
    nsReleaseDrag();
    if (moved && m) {
      nsSave('挪标注');
      nsStatus('标注挪到 ' + nsFmtCoord(m.lat, m.lng));
    }
  };

  layer.addEventListener('mousedown', (ev) => {
    const el = ev.target.closest && ev.target.closest('.ns-mark');
    if (!el) return;
    ev.stopPropagation();          // 别让它变成"在地图上点了一下"
    ev.preventDefault();
    const m = ns.marks.find((x) => x.id === el.dataset.id);
    if (!m) return;
    if (ns.activeMarkId !== m.id) nsSelectMark(m.id);
    drag = { id: m.id, x0: ev.clientX, y0: ev.clientY, lat0: m.lat, lng0: m.lng, moved: false };
    try { nsMap.setDraggable(false); } catch (e) { /* 老 SDK 没这个能力 */ }
  });

  layer.addEventListener('mousemove', (ev) => {
    if (!drag) return;
    const dx = ev.clientX - drag.x0;
    const dy = ev.clientY - drag.y0;
    if (!drag.moved && (dx * dx + dy * dy) < 16) return;   // 手抖不算拖
    drag.moved = true;
    const m = ns.marks.find((x) => x.id === drag.id);
    if (!m) return;
    const pxPerM = nsMetersToPx(1, drag.lat0) || 1;
    // 屏幕 y 向下 = 纬度变小
    m.lng = Number((drag.lng0 + (dx / pxPerM) / (111320 * Math.cos((drag.lat0 * Math.PI) / 180))).toFixed(6));
    m.lat = Number((drag.lat0 - (dy / pxPerM) / 110540).toFixed(6));
    nsPlaceMarks();
  });

  layer.addEventListener('mouseup', finish);
  // ⚠️ 三路兜底（同 nsReleaseDrag 的理由）
  document.addEventListener('mouseup', finish);
  document.addEventListener('pointerup', finish);
  window.addEventListener('blur', finish);
}

/* ---------------------------------------------------------------- 标注表单 */

/** 画标注的类型选择（六个小按钮，当前那个高亮）。 */
function nsRenderMarkKinds(cur) {
  const box = $('ns-mark-kinds');
  if (!box) return;
  box.innerHTML = NS_MARK_KINDS.map(([v, label, tip]) =>
    '<button class="ns-mark-kind' + (v === cur ? ' on' : '') + '" data-mk="' + v + '"'
      + ' title="' + nsEsc(tip) + '">' + label + '</button>').join('');
}

/**
 * 把标注表单填上当前那条的值。
 *
 * ⚠️ 每填一个框都要判空 —— 这个函数在**每次 nsRenderAll** 都会跑，
 *    而表单里任何一次 null.value 都会让整块渲染中断（右边一片空白）。
 */
function nsRenderMarkForm(m) {
  if (!m) return;
  const set = (id, v) => { const el = $(id); if (el) el.value = v; };
  const title = $('ns-mark-title');
  if (title) title.textContent = '编辑标注 · ' + nsMarkKindLabel(m.kind);
  nsRenderMarkKinds(m.kind);
  set('ns-mark-text', m.text || '');
  set('ns-mark-icon', m.icon || '');
  set('ns-mark-w', Number(m.w) > 0 ? m.w : '');
  set('ns-mark-h', Number(m.h) > 0 ? m.h : '');
  set('ns-mark-rot', Number(m.rot) ? m.rot : '');
  set('ns-mark-color', m.color || '#185fa5');

  // 只有「贴纸」有图标；只有「文字 / 气泡 / 门牌 / 入口 / 楼梯」有文字。
  // 贴纸的"文字"字段其实没意义，收起来免得用户填了没反应。
  const textField = $('ns-mark-text-field');
  const iconField = $('ns-mark-icon-field');
  if (textField) textField.classList.toggle('hidden', m.kind === 'sticker');
  if (iconField) iconField.classList.toggle('hidden', m.kind !== 'sticker');
  const hint = $('ns-mark-text-hint');
  if (hint) {
    hint.textContent = m.kind === 'plate'
      ? '牌子上的字 —— 楼号（A3）、房号、编号都行。'
      : (m.kind === 'entry'
        ? '入口的名字（东门 / 侧门 / 车库入口）。'
        : (m.kind === 'stairs' ? '写清楚是去哪儿的（上二楼 / 下负一层）。' : '不写也行 —— 先放个位置，回头再补。'));
  }
}

/** 改当前标注的某个字段（**唯一的写入口**，表单全走这里）。 */
function nsSetMarkField(key, val) {
  const m = nsActiveMark();
  if (!m) return;
  if (key === 'kind') {
    // ⚠️ 换类型时给一套**配得上它的默认值**（颜色、图标），
    //    但**不动**文字 —— 用户可能先写了字再换类型。
    m.kind = val;
    m.color = NS_MARK_COLOR[val] || m.color;
    if (val === 'sticker' && !m.icon) m.icon = '⭐';
    if (val === 'entry' && !m.icon) m.icon = '🚪';
    if (val === 'stairs' && !m.icon) m.icon = '🪜';
    ns.markKind = val;          // 顺手把"下次点地图放哪种"也切过去
  } else if (key === 'w' || key === 'h' || key === 'rot') {
    m[key] = Number(val) || 0;
  } else {
    m[key] = val;
  }
  nsSave('改标注');
  nsRenderAll();
  nsSyncMarks();
}

/** 把标注表单接上（页面初始化时调一次）。 */
function nsWireMarkForm() {
  const kinds = $('ns-mark-kinds');
  if (kinds && !kinds.dataset.wired) {
    kinds.dataset.wired = '1';
    kinds.addEventListener('click', (ev) => {
      const b = ev.target.closest && ev.target.closest('[data-mk]');
      if (b) nsSetMarkField('kind', b.getAttribute('data-mk'));
    });
  }
  const bind = (id, key, ev) => {
    const el = $(id);
    if (!el || el.dataset.wired) return;
    el.dataset.wired = '1';
    el.addEventListener(ev || 'input', () => nsSetMarkField(key, el.value));
  };
  bind('ns-mark-text', 'text');
  bind('ns-mark-icon', 'icon');
  bind('ns-mark-w', 'w');
  bind('ns-mark-h', 'h');
  bind('ns-mark-rot', 'rot');
  bind('ns-mark-color', 'color');
  const del = $('ns-mark-del');
  if (del && !del.dataset.wired) {
    del.dataset.wired = '1';
    del.addEventListener('click', () => nsDeleteMark(ns.activeMarkId));
  }
  const unsel = $('ns-mark-unsel');
  if (unsel && !unsel.dataset.wired) {
    unsel.dataset.wired = '1';
    unsel.addEventListener('click', () => nsSelectMark(''));
  }
}

/* ------------------------------------------------------------------ 楼栋增删改 */

function nsAddBuilding(lat, lng) {
  const b = nsNormalizeBuilding({
    id: nsNewId(), name: '', alias: [], lat, lng, campus: '', note: '',
  });
  ns.buildings.push(b);
  ns.activeId = b.id;
  // 新楼从第 0 层开始看 —— 不重置的话会继承上一栋的层号，大概率越界（虽然会被夹回去，
  // 但用户看到的是"跳到中间某层"，像是数据错了）
  ns.floorIndex = 0;
  nsSave();
  nsRenderAll();
  nsSyncMarkers();
  // 焦点直接给"楼名" —— 用户刚双击完，下一步就是打字
  const nameEl = $('ns-b-name');
  if (nameEl) nameEl.focus();
  nsStatus('新建了一条，填上楼名');
}

function nsDeleteBuilding(id) {
  const b = ns.buildings.find((x) => x.id === id);
  if (!b) return;
  const label = b.name || '（未命名）';
  if (!confirm(`删掉「${label}」？\n\n删了就没了（导出文件里也不会有它）。`)) return;
  ns.buildings = ns.buildings.filter((x) => x.id !== id);
  // ⚠️ 删的正好是**正在编的那一栋**时，选中态要**清空**而不是"顺延到下一栋"。
  //
  // 2026-10-13 晚改。原来写的是 `ns.activeId = ns.buildings[0].id` ——
  // 而用户这一轮明确说过"我没办法取消选择一个建筑，这也是一个非常大的问题"。
  // 顺延会让"取消选择"在这里白费：他刚取消，一栋楼，
  // 右边又塞给他另一栋的表单。
  // 删完之后他**想看的是结果**（地图上那栋还在不在），不是另一份表单。
  if (ns.activeId === id) ns.activeId = '';
  nsSave();
  nsRenderAll();
  nsSyncMarkers();
  nsStatus('已删掉「' + label + '」');
}

/** 把一条从"还没坐标"变成"有坐标"。 */
const nsHasCoord = (b) => b && (b.lat !== 0 || b.lng !== 0);

/* ------------------------------------------------------------------ 分层数据 */

/**
 * 一条房间记录 —— 工厂函数（**唯一**的构造点）。
 *
 * ⚠️ 房间的**权威字段**只有 `name` / `type` / `x` / `y` / `w` / `h`。
 *    界面上拖动的中间态、选中态都在 DOM 上，不往这里塞 ——
 *    一旦塞进来，导出时就得记得滤掉，早晚会漏。
 */
function nsNewRoom(name, type, x, y, w, h) {
  return {
    name: String(name || ''),
    type: String(type || ''),
    x: nsClampRoom(x, -0.5, 0.5),
    y: nsClampRoom(y, -0.5, 0.5),
    w: Math.max(NS_ROOM_MIN_W, Number(w) || NS_ROOM_DEFAULT_W),
    h: Math.max(NS_ROOM_MIN_H, Number(h) || NS_ROOM_DEFAULT_H),
  };
}

/** 楼层记录 —— 同上，工厂函数。 */
function nsNewFloor(name) {
  return { name: String(name || ''), rooms: [] };
}

/** 数值兜底：非数字一律当 0，再把 `NaN` 挡在 JSON 外面（`NaN` 序列化成 `null`）。 */
function nsClampRoom(v, lo, hi) {
  const n = Number(v);
  if (!Number.isFinite(n)) return 0;
  return Math.min(hi, Math.max(lo, n));
}

/**
 * 把一条楼栋补齐 v2 字段。
 *
 * ★ 为什么需要它：`nsLoad()` 从 localStorage 恢复的是**老草稿**，
 *   那时候还没有 `type` / `floors`。不补的话 `b.floors.push(...)` 会炸，
 *   而 `JSON.stringify(undefined)` 会让导出文件里**整个字段消失**。
 */
function nsNormalizeBuilding(b) {
  const out = b || {};
  out.type = String(out.type || '');
  out.floors = Array.isArray(out.floors) ? out.floors.map((f) => {
    const fl = nsNewFloor(f && f.name);
    fl.rooms = Array.isArray(f && f.rooms)
      ? f.rooms.filter((r) => r && String(r.name || '').trim()).map((r) => nsNewRoom(
        r.name, r.type,
        Number(r.x), Number(r.y),
        Number.isFinite(Number(r.w)) ? Number(r.w) : NS_ROOM_DEFAULT_W,
        Number.isFinite(Number(r.h)) ? Number(r.h) : NS_ROOM_DEFAULT_H))
      : [];
    return fl;
  }) : [];
  out.alias = Array.isArray(out.alias) ? out.alias.map(String) : [];
  // ★ 3D 底面轮廓（2026-10-12 深夜）：`[[lat,lng], …]`。
  //   ⚠️ 坏点**逐点丢**，和插件侧的 `Building.parse` 同一套规矩 ——
  //      两处不一致的话会出现"工具里看着有轮廓、导进手机不见了"。
  //      整条丢掉的代价太大（用户描了半天的楼变成一根针）。
  out.shape = (Array.isArray(out.shape) ? out.shape : [])
    .filter((p) => Array.isArray(p) && p.length >= 2)
    .map((p) => [Number(p[0]), Number(p[1])])
    .filter((p) => Number.isFinite(p[0]) && Number.isFinite(p[1]))
    // 只留中国境内的点（和 `nsParseCoord` 同一个判据：纬度 3~54、经度 73~136）
    .filter((p) => p[0] >= 3 && p[0] <= 54 && p[1] >= 73 && p[1] <= 136);

  // ★ 来源标记：从 OSM 导入的带 way id（2026-10-13）。
  //   ⚠️ 它**不导出**（那是我们自己的记账，对手机端没意义）——
  //      只用来"下次再导同一栋时跳过，保住用户改过的名字和类别"。
  out.osm = String(out.osm || '');

  // ★★ 轮廓是"真的"还是"占位矩形"（2026-10-13）。
  //
  // ## 为什么必须有这个标记
  //
  // 没有它，**一个自动生成的矩形和一个手描的真实轮廓在导出文件里长得一模一样**
  // （都是 4 个点 / N 个点，都是合法的 `shape`）。而手机端是这么判的：
  //
  // ```
  //   shape.size >= 3  →  显示"有真轮廓"
  // ```
  //
  // 于是"点选建楼"自动生成的矩形会被报成**真轮廓** —— 用户拿着一个
  // 纯正南正北的方盒子，界面上却写着"这是真轮廓"。这比不显示更坏：
  // 他不会再想去补它。
  //
  // 加这个功能（"给没轮廓的楼补真轮廓"）的前提正是能分清两者。
  //
  // ⚠️ **缺省是 `true`**（老草稿 / 老包里没有这个字段 → 当成真的）：
  //    因为历史上 `shape` 只可能来自"用户手描"或"OSM 导入"，
  //    两个都是真的。把它默认成 `false` 会让所有老数据都显示成"占位"。
  out.shapeExact = out.shapeExact !== false;
  return out;
}

/** 当前正在编辑的楼栋（没有就 null）。 */
const nsActiveBuilding = () => ns.buildings.find((x) => x.id === ns.activeId) || null;

/**
 * 当前正在编辑的楼层。
 *
 * ⚠️ `ns.floorIndex` 是**跨楼栋共享**的界面状态，切楼栋后可能越界 ——
 *    所以这里必须夹回去，而不是无条件 `floors[ns.floorIndex]`。
 *    返回 null 表示"这栋楼没有楼层"。
 */
function nsActiveFloor() {
  const b = nsActiveBuilding();
  if (!b || !b.floors.length) return null;
  if (ns.floorIndex >= b.floors.length) ns.floorIndex = b.floors.length - 1;
  if (ns.floorIndex < 0) ns.floorIndex = 0;
  return b.floors[ns.floorIndex];
}

/** 分类约定值 → 中文标签（认不出的原样返回，别抹掉）。 */
function nsTypeLabel(raw) {
  const v = String(raw || '');
  const hit = NS_TYPES.find((t) => t[0] === v);
  if (hit) return hit[1];
  return v || '未分类';
}

/**
 * 统计一共标了多少个房间。
 *
 * ⚠️ 同时吃"一个包"（`{buildings:[…]}`）和"一个楼栋数组"（草稿里的
 *    `ns.buildings`）—— 调用点两种都有，逼调用方自己包一层是最容易
 *    写错的地方（忘了包就会静默返回 0）。
 */
function nsRoomCount(packOrList) {
  const list = Array.isArray(packOrList) ? packOrList : ((packOrList || {}).buildings || []);
  let n = 0;
  list.forEach((b) => {
    (b.floors || []).forEach((f) => { n += (f.rooms || []).length; });
  });
  return n;
}

/* ------------------------------------------------------------------ 渲染 */

function nsRenderAll() {
  nsRenderPack();
  nsRenderList();
  nsRenderEditor();
  nsSyncMarkers();
  // ★ 底面轮廓跟着一起重画（换楼栋 / 换选中项时，两套样式要跟着换）
  nsSyncShapeLayer();
  nsRenderShapeHint();
  // ★ 类别图例（2026-10-13）：它是"改类别"的主入口，要跟着选中项刷新
  nsRenderTypeLegend();
  // ★ 数量变了"清掉没名字的"那行字也要跟着变（它上面写着要删几栋）
  nsRefreshPrune();
  // ★ 同理，"补真轮廓"上面写着要补几栋
  nsRefreshFill();
  // ★ 顶栏那条两步进度（2026-10-13）：楼栋增删之后要跟着变，
  //   否则用户删光了楼、那一格还亮着"已完成"，指示就是错的。
  nsRenderSteps();
  // ★ 「待确认 N」那个常驻入口（2026-10-13 下午）：名单一变它就要跟着变 ——
  //   确认完了不收起、或者识别完不出现，都会让用户以为还有事没做。
  nsRenderConfirmAll();
  // ★ 「清空全部」的计数（2026-10-13 晚）：按钮上写着要清几栋，
  //   所以楼栋数一变就得重算 —— 不重算的话它会一直写着进来时那个数。
  nsRenderClear();
  // ⚠️ 顺序要紧：tidy 读的是上面那两个按钮的显隐，必须放最后。
  nsRenderTidy();
}

/**
 * 刷新"清掉没名字的"那个按钮（2026-10-13）。
 *
 * ## 它解决的问题
 *
 * 用户的原话：**"只保留被标注的数据，其他的建筑统一清掉。"**
 *
 * 一次从 OSM 拉进来 155 栋，其中 135 栋**在 OSM 上本来就没名字** ——
 * 它们不是"用户标了但没填名"，是"OSM 只有轮廓、没有名字"。
 * 那 135 栋在手机上是一堆灰块，只会挡住他真正在意的那二十栋。
 *
 * ## ⚠️ 两条界面纪律
 *
 * ① **按钮上必须写清要删几栋**。它是破坏性的，而"清掉没名字的"这五个字
 *    不告诉用户代价。写成"清掉没名字的（135 栋）"，他一眼就知道要不要点。
 * ② **一栋都没得删时把按钮藏起来**（`hidden`），不是置灰 ——
 *    置灰的按钮会让人一直猜"我是不是还差一步才能点"。
 */
function nsRefreshPrune() {
  const btn = $('ns-btn-prune');
  const label = $('ns-prune-label');
  if (!btn) return;
  const dead = ns.buildings.filter((b) => !String(b.name || '').trim()).length;
  if (dead === 0) {
    btn.hidden = true;
    return;
  }
  btn.hidden = false;
  if (label) label.textContent = '清掉没名字的（' + dead + ' 栋）';
}

/**
 * 这栋楼的轮廓**需要补**吗（2026-10-13）。
 *
 * 判据：有坐标，且（没有轮廓 **或** 轮廓是占位矩形）。
 *
 * ⚠️ **必须只有这一处判据。** 原来 [nsRefreshFill]（按钮上那个数字）和
 *    [nsFillShapes]（真正干活的循环）各写了一遍同样的条件 ——
 *    而两份判据迟早会漂，漂的表现是**按钮说"补 12 栋"、点下去只补了 8 栋**，
 *    用户会以为程序漏了，然后反复点。
 *
 * ⚠️ 只看"没有轮廓"是不够的："点选建楼"会自动生成一个**占位矩形**，
 *    它有 4 个点、`shape` 是合法的 —— 只看点数的话那些永远补不上，
 *    而它们正是最需要补的（用户手上几十栋方盒子就是这么来的）。
 */
function nsNeedsShape(b) {
  return nsHasCoord(b) &&
    (!Array.isArray(b.shape) || b.shape.length < 3 || b.shapeExact === false);
}

/**
 * 刷新"给没轮廓的楼补真轮廓"那个按钮（2026-10-13）。
 */
function nsRefreshFill() {
  const btn = $('ns-btn-fill');
  const label = $('ns-fill-label');
  if (!btn) return;
  const n = ns.buildings.filter(nsNeedsShape).length;
  if (n === 0) {
    btn.hidden = true;
    return;
  }
  btn.hidden = false;
  if (label) label.textContent = '自动补上真实轮廓（' + n + ' 栋）';
}

/**
 * 给所有"只有占位矩形 / 没有轮廓"的楼，自动去 OSM 找真轮廓（2026-10-13）。
 *
 * ## 它回答的是哪句话
 *
 * > **"对应的模型没有自动产生，还要我自己去建，这是很不好的。"**
 *
 * 新点的楼已经会自动找轮廓了（见 [nsCreateAt]），但**以前标的那些**不会 ——
 * 用户手上可能已经有几十栋只有矩形的楼。这个按钮把它们一次补齐。
 *
 * ## 为什么不用怕慢
 *
 * 轮廓是从**烘焙数据**里查的（见 [nsFootprintAt]），而那份数据一个校区
 * 只取一次、之后常驻内存。所以整个循环是内存里查几十次，
 * 用户感知不到等待。
 *
 * ⚠️ 但**还是要给进度**：万一点位散落在烘焙范围之外，每一次都会走服务端 ——
 *    那时候就是几十次网络请求，不显示进度的话界面像卡死了。
 */
/**
 * 给"还没有真轮廓"的楼补上真实轮廓。
 *
 * @param onlyIds 只补这几栋（批量工具用）。不传 = 所有缺轮廓的。
 */
async function nsFillShapes(onlyIds) {
  // ⚠️ 用 `nsNeedsShape` 这**一个**判据，不要在这里再写一遍条件 ——
  //    和按钮上那个数字必须严格同源，否则"按钮说 12 栋、实际补 8 栋"。
  //
  // ⚠️ 批量调用时（`onlyIds`）**还要再过一遍 `nsNeedsShape`**：
  //    用户在批量条上勾的可能是已经有轮廓的楼，直接按 id 补会白跑一趟网络。
  // ⚠️ 判据必须**原样写 `filter(nsNeedsShape)`**（不要展开成内联条件）——
  //    按钮上那个数字和这里必须**严格同源**，而门禁就是靠这个写法钉住这件事的。
  //    批量的限定条件是**再叠一层**，不是把上面那个条件改掉。
  const targets = ns.buildings
    .filter(nsNeedsShape)
    .filter((b) => !onlyIds || onlyIds.includes(b.id));
  if (!targets.length) {
    if (onlyIds) nsToast('勾选的这些都已经有真轮廓了');
    return;
  }

  const btn = $('ns-btn-fill');
  const label = $('ns-fill-label');
  if (btn) btn.disabled = true;

  let filled = 0;
  let missed = 0;
  // 顺手纠正了位置的栋数（老数据整份偏了几百米那批）
  let moved = 0;
  for (let i = 0; i < targets.length; i += 1) {
    if (label) label.textContent = '正在找轮廓… ' + (i + 1) + '/' + targets.length;
    const b = targets[i];
    // ★★ 有 OSM way id 就**先按 id 找**（2026-10-13）。
    //
    // 不只是"更准"：老数据的坐标整份偏了 ~530 米，按位置去找会搜到
    // **别的地方**、把别的楼的轮廓安到这一栋上 —— 那是"看起来修好了"的错。
    // id 对得上就是同一栋，没有这个风险。
    // eslint-disable-next-line no-await-in-loop -- 顺序发是有意的：并发几十个请求会打爆上游
    let found = await nsFootprintByOsmId(b.osm);
    if (!found) {
      // eslint-disable-next-line no-await-in-loop -- 同上
      found = await nsFootprintAt(b.lat, b.lng);
    }
    if (!found) {
      missed += 1;
      continue;
    }
    b.shape = found.poly.map((p) => [Number(p[0].toFixed(6)), Number(p[1].toFixed(6))]);
    b.shapeExact = true;
    // ★★ 位置也跟着校对（2026-10-13）。
    //
    // 老数据整份偏了几百米（WGS 被当成 GCJ 存），只修轮廓的话结果是
    // "轮廓对了、图钉还在隔壁" —— 而那个状态**看起来是修好了**，最难发现。
    // 所以：中心点如果不在新轮廓里，把它拉到最近的内部点。
    //
    // ⚠️ 这一步对**老数据**是必需的（它们的位置是错的）。
    //    对**新导入的数据**则几乎不会触发 —— 新导入时坐标和轮廓
    //    一起转过 GCJ，中心点本来就落在自己的轮廓里。
    //
    // ⚠️ 点在轮廓里就**不动它** —— 用户把点摆在楼门口是他的选择。
    if (!nsPointInPoly(b.lat, b.lng, b.shape)) {
      const ni = nsNearestInsidePoint(b.shape, b.lat, b.lng);
      // 位移记下来，最后汇总告诉用户"顺手纠正了几栋的位置"
      if (nsDistM(b.lat, b.lng, ni[0], ni[1]) > 30) moved += 1;
      b.lat = ni[0];
      b.lng = ni[1];
    }
    // 顺带把类别补上（没分类的那些，OSM 的标签往往能猜出来）
    if (!b.type) b.type = nsGuessType(found.tags);
    filled += 1;
  }

  if (btn) btn.disabled = false;
  nsSave();
  nsRenderAll();
  nsStatus('轮廓补齐：' + filled + ' 栋用上了真实轮廓'
    + (moved ? '，其中 ' + moved + ' 栋的坐标也一并纠正了（原来偏了几百米）' : '')
    + (missed ? '，' + missed + ' 栋 OSM 上没有（保留默认矩形）' : ''));
  nsToast(filled
    ? ('补上 ' + filled + ' 栋真实轮廓' + (missed ? ' · ' + missed + ' 栋没有' : ''))
    : '这一带的 OSM 上没有现成轮廓');
}

/**
 * 清掉所有没名字的楼。
 *
 * ⚠️ **不问"确定吗"** —— 它和"删一栋楼"不一样：那些楼的名字是空的，
 *    在导出时**本来就会被丢掉**（见 [nsBuildPack] 的 `filter(b => b.name)`）。
 *    所以这一步只是把"导出会发生的事"提前给用户看一眼，
 *    不改变最终产物。代价是"列表短了、地图干净了"，收益是"看得清自己标了什么"。
 *
 * ⚠️ 但**还是要留一条撤销的路**：把删掉的那些放进 `ns.undoPrune`，
 *    提示条上给一个"撤销"。用户手滑点了一次不该丢一百多栋。
 */
function nsDropUnnamed() {
  const dead = ns.buildings.filter((b) => !String(b.name || '').trim());
  if (!dead.length) return;
  const keep = ns.buildings.filter((b) => String(b.name || '').trim());
  ns.undoPrune = dead;
  ns.buildings = keep;
  // ⚠️ 选中的那栋可能就在被删的那批里 —— 必须夹回去。
  //    不夹的话右边编辑器会指着一个不存在的 id，表现是"右边一片空白、
  //    改什么都没反应"，而用户刚点的是"清掉没名字的"，他不会往这上面想。
  //    夹法和 nsLoad 里那句一模一样（同一件事只有一个写法）。
  if (!ns.buildings.some((b) => b.id === ns.activeId)) {
    ns.activeId = ns.buildings.length ? ns.buildings[0].id : '';
  }
  nsSave();
  nsRenderAll();
  nsToastHtml('清掉了 <b>' + dead.length + '</b> 栋没名字的 · '
    + '<a href="#" id="ns-undo-prune" style="color:#7fd1ff">撤销</a>');
  const undo = $('ns-undo-prune');
  if (undo) {
    undo.addEventListener('click', (e) => {
      e.preventDefault();
      if (!ns.undoPrune || !ns.undoPrune.length) return;
      ns.buildings = ns.buildings.concat(ns.undoPrune);
      ns.undoPrune = null;
      nsSave();
      nsRenderAll();
      nsToast('已恢复');
    });
  }
}

/**
 * ★★★ 一键清空（2026-10-13 晚加，用户点名要的）。
 *
 * 用户的原话：
 *
 * > "加入一键清空按钮，可以直接清掉所有已添加的点，然后重新来过。"
 *
 * ## 它和「清掉没名字的」的区别
 *
 * ```
 *   清掉没名字的   有条件的：只删那些没名字的（OSM 一次导进来的一堆灰块）
 *   一键清空       无条件的：**全删**，连标定原点一起 —— "我要重来"
 * ```
 *
 * ## 三条纪律（这个项目里破坏性操作的老规矩）
 *
 * ① **确认框里写清楚要删几栋** —— "我以为删了 3 栋、其实删了 155 栋"
 *    是这里最容易出的事故（`nsDropUnnamed` 那条注释里也写着同一句话）
 * ② **当场给退路**：删之前把整份数据存进 `ns.undoClear`，提示条上给一个「撤销」
 * ③ **走 `nsSave('清空全部')`** → 自动进撤销栈，Ctrl+Z 也能回来
 *
 * ⚠️ 两条退路**都要有**，它们管的不是同一件事：
 *    · 提示条上那个「撤销」是给"我马上后悔"的（几秒钟内）
 *    · Ctrl+Z 是给"我过一会儿才发现"的（提示条早就没了）
 *
 * ⚠️ 它**不动包标识 / 名称 / 说明** —— 那些回答的是"这个包是什么"，
 *    不是"标了哪些点"。一起清掉的话，用户点一下就从「广东海洋大学」
 *    变成「未命名数据包」，还得重新填一遍 —— 而他只是想重标楼。
 */
function nsClearAll() {
  const n = ns.buildings.length;
  const hasOrigin = !!ns.origin;
  if (!n && !hasOrigin && !ns.marks.length) { nsToast('本来就是空的'); return; }
  const nm = ns.marks.length;
  const msg = '清空全部 ' + n + ' 栋楼'
    + (nm ? ('和 ' + nm + ' 个标注') : '')
    + (hasOrigin ? '和标定原点' : '') + '？\n\n'
    + '包标识和名称会留着。清完可以按 Ctrl+Z 撤销，提示条上也有「撤销」。';
  if (!window.confirm(msg)) return;

  // ★ 先留底（深拷贝：`ns.buildings` 后面会被整个换掉，浅拷贝等于没留）
  // ⚠️ **标注也要一起清**（2026-10-13 深夜补）。
  //
  // 用户的原话："那个**所有的标注清除**的功能呢，你也没做到。"
  //
  // 第一版只清了楼栋和原点 —— 因为那时还没有标注层。加了 marks 之后
  // 忘了同步这里，于是"一键清空"之后地图上**还挂着一堆牌子**，
  // 而用户以为已经清干净了。
  ns.undoClear = {
    buildings: JSON.parse(JSON.stringify(ns.buildings)),
    marks: JSON.parse(JSON.stringify(ns.marks)),
    origin: ns.origin ? Object.assign({}, ns.origin) : null,
  };
  ns.buildings = [];
  ns.marks = [];
  ns.activeMarkId = '';
  ns.origin = null;
  ns.activeId = '';
  ns.batchIds = [];
  ns.poiReview = [];
  nsSave('清空全部');
  nsRenderAll();
  nsSyncMarkers();
  nsToastHtml('已清空 <b>' + n + '</b> 栋 · '
    + '<a href="#" id="ns-undo-clear" style="color:#7fd1ff">撤销</a>');
  const undo = $('ns-undo-clear');
  if (undo) undo.addEventListener('click', (e) => { e.preventDefault(); nsUndoClear(); });
  nsStatus('已清空 —— 可以重新导入、或者从头标了');
}

/** 把「一键清空」退回去（提示条上那个链接用）。 */
function nsUndoClear() {
  const u = ns.undoClear;
  if (!u) return;
  ns.buildings = u.buildings;
  // ★ 标注也一起恢复（和清空那边对称）
  ns.marks = Array.isArray(u.marks) ? u.marks : [];
  ns.origin = u.origin;
  ns.undoClear = null;
  nsSave('撤销清空');
  nsRenderAll();
  nsSyncMarkers();
  nsToast('已恢复 ' + ns.buildings.length + ' 栋'
    + (ns.marks.length ? (' · ' + ns.marks.length + ' 个标注') : ''));
}

/** 刷新「清空全部」那个按钮（有东西才显示，并把要清几栋写在脸上）。 */
function nsRenderClear() {
  const btn = $('ns-btn-clear');
  if (!btn) return;
  const label = $('ns-clear-label');
  const n = ns.buildings.length;
  btn.hidden = n === 0 && !ns.origin && ns.marks.length === 0;
  if (label) {
    label.textContent = '清空全部（' + n + ' 栋'
      + (ns.marks.length ? (' + ' + ns.marks.length + ' 标注') : '')
      + (ns.origin ? ' + 原点' : '') + '）';
  }
}

/**
 * 「整理这一片」那个折叠块：里面一个可用的都没有时，**整块收起来**。
 *
 * ⚠️ 不这么做的话，一进来就看见一个「整理这一片 ▸」，点开里面空的 ——
 *    比不显示更让人困惑（用户会以为功能坏了）。
 */
function nsRenderTidy() {
  const box = $('ns-tidy');
  if (!box) return;
  const any = ['ns-btn-fill', 'ns-btn-prune', 'ns-btn-clear']
    .some((id) => { const el = $(id); return el && !el.hidden; });
  box.hidden = !any;
  if (!any) box.removeAttribute('open');
}

function nsWireClear() {
  const btn = $('ns-btn-clear');
  if (!btn || btn.dataset.wired) return;
  btn.dataset.wired = '1';
  btn.addEventListener('click', () => nsClearAll());
}

function nsRenderPack() {
  $('ns-pack-id').value = ns.packId;
  $('ns-pack-name').value = ns.name;
  $('ns-pack-note').value = ns.note;
  $('ns-pack-version').value = ns.version;
  $('ns-pack-origin').value = ns.origin
    ? nsFmtCoord(ns.origin.lat, ns.origin.lng) : '';

  $('ns-pack-title').textContent = ns.name || '未命名数据包';
  $('ns-count').textContent = String(ns.buildings.length);

  const idEl = $('ns-pack-id');
  const ok = /^[a-z][a-z0-9-]{1,47}$/.test(ns.packId);
  idEl.classList.toggle('bad', ns.packId.length > 0 && !ok);
  const hint = $('ns-pack-id-hint');
  if (ns.packId && !ok) {
    hint.textContent = '格式不对：要小写字母开头，只能有小写字母/数字/连字符，2~48 位。';
    hint.classList.add('warn');
  } else {
    hint.textContent = '小写字母 / 数字 / 连字符。用户凭它从云端拉这个包。';
    hint.classList.remove('warn');
  }
}

function nsRenderList() {
  const host = $('ns-list');
  const empty = $('ns-list-empty');
  host.textContent = '';

  // ★★ 搜索过滤（2026-10-13 晚）。
  //
  // 楼多的时候（实测一次导进 155 栋）在列表里滚到某一栋是体力活 ——
  // 而"改哪一栋"在标注里是高频动作。
  const q = nsNorm(($('ns-list-q') || {}).value || '');
  const shown = ns.buildings.filter((b) => {
    if (!q) return true;
    // ⚠️ 拿 **b.name / b.alias** 直接比，不要去调 `b.keys()` ——
    //    那是插件侧 Kotlin 的 `Building.keys()`（同名不同物），
    //    这里的楼就是普通对象字面量，没有这个方法。实测踩过：
    //    用户一打字就 "TypeError: b.keys is not a function"，
    //    列表渲染半途而废，整栏空白。
    if (nsNorm(b.name).indexOf(q) >= 0) return true;
    if ((b.alias || []).some((a) => nsNorm(a).indexOf(q) >= 0)) return true;
    // 房号也搜 —— "我知道教室号但不知道楼名"是个真实场景
    return (b.floors || []).some((f) => (f.rooms || [])
      .some((r) => nsNorm(r.name).indexOf(q) >= 0));
  });

  if (!shown.length) {
    empty.classList.remove('hidden');
    // ⚠️ 两种情况必须分开说。都写"还没有楼栋"的话，
    //    搜不到东西的人会以为自己的楼全丢了。
    empty.textContent = ns.buildings.length
      ? '没有匹配的楼栋 —— 换个词试试'
      : '还没有楼栋。\n点上面的「从 OSM 导入这一带的建筑」—— 大多数学校那一片都已经有人标好了。';
    return;
  }
  empty.classList.add('hidden');

  shown.forEach((b) => {
    const el = document.createElement('div');
    el.className = 'ns-item' + (b.id === ns.activeId ? ' active' : '')
      + (ns.tool === 'batch' && ns.batchIds.includes(b.id) ? ' picked' : '');

    // ★ 批量模式下的勾选框（2026-10-13）。
    //   放**最左边**：勾选是这一行的第一个动作，放后面会和"名字状态"那个标记打架。
    if (ns.tool === 'batch') {
      const cb = document.createElement('span');
      cb.className = 'ns-item-check' + (ns.batchIds.includes(b.id) ? ' on' : '');
      cb.textContent = ns.batchIds.includes(b.id) ? '✓' : '';
      el.appendChild(cb);
    }

    const main = document.createElement('div');
    main.className = 'ns-item-main';

    const nm = document.createElement('div');
    nm.className = 'ns-item-name';
    // ⚠️ 序号用**全表**的下标，不用「筛完之后」的下标 —— 不然搜一下
    //    序号就变了，面板里那个数字也对不上了。
    nm.textContent = `${ns.buildings.indexOf(b) + 1}. ${b.name || '（未命名）'}`;

    const sub = document.createElement('div');
    sub.className = 'ns-item-sub';
    const bits = [];
    if (b.alias && b.alias.length) bits.push('也叫 ' + b.alias.join('、'));
    if (nsHasCoord(b)) bits.push(nsFmtCoord(b.lat, b.lng));
    if (b.campus) bits.push(b.campus);
    // ★ v2：列表上直接能看出"这条分过类没有 / 标了几层几间" ——
    //   否则要一条条点进去才知道哪些还没标分层，几十栋楼时根本标不动
    if (b.type) bits.push(nsTypeLabel(b.type));
    const rooms = (b.floors || []).reduce((n, f) => n + (f.rooms || []).length, 0);
    if (b.floors && b.floors.length) {
      bits.push(`${b.floors.length} 层${rooms ? ' · ' + rooms + ' 间' : ''}`);
    }
    sub.textContent = bits.join(' · ') || '还没坐标';

    main.append(nm, sub);

    // ★★★ 名字状态那个小标记（2026-10-13 改成**四态**）。
    //
    // 用户的原话："默认还是按自动识别的结果，只不过你要标出来，
    // 例如说原本是绿色的勾，然后标记为黄色的感叹号，
    // 用户再点击一下，又会变成绿色的勾。"
    //
    // ⚠️ 它**可点**（黄叹号 → 绿勾）。不可点的话"确认"这件事就得多一个按钮、
    //    多一次点击，而用户想的就是"看一眼，对了，点一下"。
    const st = nsNameState(b);
    const flag = document.createElement('span');
    flag.className = 'ns-item-flag ' + st.cls;
    flag.textContent = st.mark;
    flag.title = st.title;
    if (st.cls === 'poi') {
      flag.addEventListener('click', (ev) => {
        ev.stopPropagation();          // 别把"点标记"也当成"点这一行"
        b.nameSrc = 'user';
        nsSave();
        nsRenderList();
        nsStatus('已确认「' + b.name + '」—— 标记变绿勾了');
      });
    }

    el.append(main, flag);
    el.addEventListener('click', () => {
      // ★★★ 点列表 → **视角自动飞过去**（2026-10-13）。
      //
      // 用户的原话："列表里面呢，我点击以后，应该把视角自动跳转过去，
      // 这才比较合理一点。"
      //
      // 在这之前只改 `activeId`，地图不动 —— 于是"在列表里找到那栋楼"
      // 和"在地图上看到它在哪"是两件事，用户还得自己拖过去找。
      //
      // ⚠️ 批量模式下点的含义不同：勾选，**不要**飞（连点十行的话
      //    视角会跳十次，根本没法用）。
      if (ns.tool === 'batch') {
        nsBatchToggle(b.id);
        ns.activeId = b.id;
        nsRenderEditor();
        return;
      }
      nsSelect(b.id, true);
    });
    host.appendChild(el);
  });
}

function nsRenderEditor() {
  const b = ns.buildings.find((x) => x.id === ns.activeId);
  const emptyBox = $('ns-editor-empty');
  const body = $('ns-editor-body');
  const markBody = $('ns-mark-body');

  // ★★★ 选中一条标注时，右边换成标注表单（2026-10-13 深夜）。
  //     两套表单**互斥** —— 同时显示的话用户不知道自己在改哪个，
  //     而且"删掉"按钮删的是谁也要靠猜。
  const mk = nsActiveMark();
  if (mk) {
    if (emptyBox) emptyBox.classList.add('hidden');
    if (body) body.classList.add('hidden');
    if (markBody) markBody.classList.remove('hidden');
    nsRenderMarkForm(mk);
    return;
  }
  if (markBody) markBody.classList.add('hidden');

  if (!b) {
    emptyBox.classList.remove('hidden');
    body.classList.add('hidden');
    return;
  }
  emptyBox.classList.add('hidden');
  body.classList.remove('hidden');

  $('ns-ed-title').textContent = b.name || '编辑楼栋';
  $('ns-b-name').value = b.name;
  $('ns-b-coord').value = nsHasCoord(b) ? nsFmtCoord(b.lat, b.lng) : '';
  $('ns-b-alias').value = (b.alias || []).join('，');
  $('ns-b-campus').value = b.campus || '';
  $('ns-b-note').value = b.note || '';

  nsRenderTypeSelect(b);
  nsRenderFloorEditor(b);

  // 离原点多远 —— 这是"标飞了"最直接的判据
  const far = $('ns-b-far');
  if (ns.origin && nsHasCoord(b)) {
    const d = nsDistance(ns.origin, b);
    if (d > NS_FAR_METERS) {
      far.className = 'ns-hint warn';
      far.textContent =
        `⚠️ 这一条离标定原点 ${(d / 1000).toFixed(1)} 公里 —— 是不是标飞了？` +
        `（校内楼一般都在原点几公里内）`;
    } else {
      far.className = 'ns-hint';
      far.textContent = `离标定原点 ${d < 1000 ? Math.round(d) + ' 米' : (d / 1000).toFixed(1) + ' 公里'}`;
    }
  } else if (!ns.origin) {
    far.className = 'ns-hint';
    far.textContent = '设了「标定原点」之后，这里会显示距离，帮你发现标飞的点。';
  } else {
    far.className = 'ns-hint warn';
    far.textContent = '这一条还没有坐标 —— 在地图上单击取一个。';
  }
}

/**
 * 填「分类」下拉。
 *
 * ⚠️ 包里的 `type` 可能是**约定值之外**的东西（格式里它是自由文本，
 *    别人从别的工具标进来的）。这种情况下**补一个同名 option 并选中**，
 *    而不是静默地显示成"未分类" —— 后者会让用户在编辑时无意中
 *    把原值改掉，属于"打开就丢数据"。
 */
function nsRenderTypeSelect(b) {
  const sel = $('ns-b-type');
  if (!sel) return;
  const raw = String(b.type || '');
  const known = NS_TYPES.some((t) => t[0] === raw);

  sel.textContent = '';
  NS_TYPES.forEach(([v, label]) => {
    const o = document.createElement('option');
    o.value = v;
    o.textContent = v ? `${label}（${v}）` : label;
    sel.appendChild(o);
  });
  if (raw && !known) {
    const o = document.createElement('option');
    o.value = raw;
    o.textContent = `${raw}（格式外的值，保留原样）`;
    sel.appendChild(o);
    $('ns-b-type-hint').textContent =
      `⚠️ 这个分类「${raw}」不在约定表里。插件会把它归到「其他」显示，但值不会丢。`;
    $('ns-b-type-hint').className = 'ns-hint warn';
  } else {
    $('ns-b-type-hint').textContent = '用于手机端按类型筛楼（教学楼 / 宿舍 / 食堂 …）。';
    $('ns-b-type-hint').className = 'ns-hint';
  }
  sel.value = raw;
}

/**
 * 画楼层编辑器：楼层选择条 + 左侧房间清单 + 右侧 1×1 画布。
 *
 * ★ 坐标约定（文档 §9.3）：以楼栋中心为原点，`x` 右为正、`y` **下**为正，
 *   整栋楼的外墙就是 `[-0.5, 0.5]`。所以画布上：
 *   `左 = (x - w/2) + 0.5`，`上 = (y - h/2) + 0.5`（`y` 向下，和屏幕一致，
 *   不需要翻转 —— 这正是当初选这个坐标系的原因之一）。
 */
function nsRenderFloorEditor(b) {
  const floors = b.floors || [];
  const bar = $('ns-floor-bar');
  const canvas = $('ns-floor-canvas');
  const roomList = $('ns-room-list');
  const hint = $('ns-floor-hint');
  if (!bar || !canvas) return;

  // --- 楼层选择条 ---
  bar.textContent = '';
  floors.forEach((f, i) => {
    const btn = document.createElement('button');
    btn.className = 'small ns-floor-tab' + (i === ns.floorIndex ? ' on' : '');
    btn.textContent = f.name || `第 ${i + 1} 层`;
    btn.addEventListener('click', () => {
      ns.floorIndex = i;
      nsRenderFloorEditor(b);
    });
    bar.appendChild(btn);
  });
  const addF = document.createElement('button');
  addF.className = 'small ghost';
  addF.textContent = '＋ 加一层';
  addF.addEventListener('click', () => nsAddFloor(b));
  bar.appendChild(addF);

  const cur = nsActiveFloor();
  const tools = $('ns-floor-tools');
  if (!cur) {
    canvas.classList.add('hidden');
    if (tools) tools.classList.add('hidden');
    roomList.textContent = '';
    hint.className = 'ns-hint';
    hint.textContent = '这栋楼还没标分层。点「＋ 加一层」开始 —— 房间只要在下面的方格里'
      + '拖出大概位置就行，不用填任何坐标。';
    return;
  }
  canvas.classList.remove('hidden');
  if (tools) tools.classList.remove('hidden');

  // --- 当前层的名字可以改 ---
  $('ns-floor-name').value = cur.name;

  // --- 画布：外框 + 每个房间一个块 ---
  canvas.textContent = '';
  const frame = document.createElement('div');
  frame.className = 'ns-room-frame';
  canvas.appendChild(frame);

  cur.rooms.forEach((r, i) => {
    frame.appendChild(nsMakeRoomBlock(b, cur, r, i));
  });

  hint.className = 'ns-hint';
  hint.textContent = cur.rooms.length
    ? `${cur.name || '这一层'}标了 ${cur.rooms.length} 间。拖动方块挪位置，拖右下角改大小。`
    : `${cur.name || '这一层'}还空着 —— 在方格里拖一下就拖出一间房。`;

  nsRenderRoomList(b, cur, roomList);
}

/** 房间清单（左侧）：显示名字 + 类型，点一下选中/高亮对应的方块。 */
function nsRenderRoomList(b, floor, host) {
  host.textContent = '';
  if (!floor.rooms.length) {
    const p = document.createElement('p');
    p.className = 'ns-hint';
    p.textContent = '（空的）';
    host.appendChild(p);
    return;
  }
  floor.rooms.forEach((r, i) => {
    const row = document.createElement('div');
    row.className = 'ns-room-row';
    const nm = document.createElement('input');
    nm.type = 'text';
    nm.value = r.name;
    nm.placeholder = '房号 / 名字';
    nm.addEventListener('input', () => {
      r.name = nm.value;
      // 同步更新画布上的文字（不重画整个画布 —— 那会打断正在输入的光标）
      const blk = $(`ns-room-${i}`);
      if (blk) blk.querySelector('.ns-room-label').textContent = r.name || '';
      nsSave();
      nsRenderList();
    });
    const ty = document.createElement('select');
    NS_ROOM_TYPES.forEach(([v, label]) => {
      const o = document.createElement('option');
      o.value = v;
      o.textContent = label;
      ty.appendChild(o);
    });
    // 同上：格式外的房间类型也要能显示出来，别静默改成"未分类"
    if (r.type && !NS_ROOM_TYPES.some((t) => t[0] === r.type)) {
      const o = document.createElement('option');
      o.value = r.type;
      o.textContent = r.type;
      ty.appendChild(o);
    }
    ty.value = r.type || '';
    ty.addEventListener('change', () => { r.type = ty.value; nsSave(); });

    const del = document.createElement('button');
    del.className = 'small ghost danger';
    del.textContent = '×';
    del.title = '删掉这一间';
    del.addEventListener('click', () => {
      floor.rooms.splice(i, 1);
      nsSave();
      nsRenderFloorEditor(b);
      nsRenderList();
    });

    row.append(nm, ty, del);
    host.appendChild(row);
  });
}

/**
 * 一个房间方块。
 *
 * ⚠️ 拖拽用 **pointer 事件**统一处理鼠标和触摸 —— 分开写 `mousedown`
 *    和 `touchstart` 的话，两者都会触发（触摸设备会合成鼠标事件），
 *    表现是"拖一下跳两倍距离"。`setPointerCapture` 保证指针移出方块
 *    甚至移出窗口时事件仍然回来，不然指针快了就会"松手了还在拖"。
 */
function nsMakeRoomBlock(b, floor, r, index) {
  const el = document.createElement('div');
  el.className = 'ns-room';
  el.id = `ns-room-${index}`;
  el.style.left = `${(r.x - r.w / 2 + 0.5) * 100}%`;
  el.style.top = `${(r.y - r.h / 2 + 0.5) * 100}%`;
  el.style.width = `${r.w * 100}%`;
  el.style.height = `${r.h * 100}%`;

  const lab = document.createElement('span');
  lab.className = 'ns-room-label';
  lab.textContent = r.name || '';
  el.appendChild(lab);

  const grip = document.createElement('span');
  grip.className = 'ns-room-grip';
  grip.title = '拖动改大小';
  el.appendChild(grip);

  nsBindRoomDrag(el, b, r, index, 'move');
  nsBindRoomDrag(grip, b, r, index, 'resize');
  return el;
}

/**
 * 给房间方块（或它的右下角把手）绑拖拽。
 *
 * `mode === 'move'` 挪中心；`'resize'` 改宽高。
 * 两种都用**百分数增量**算，所以画布大小随便变都不影响结果。
 */
function nsBindRoomDrag(el, b, r, index, mode) {
  let dragging = false;
  let x0 = 0;
  let y0 = 0;
  let r0 = null;

  el.addEventListener('pointerdown', (e) => {
    // 在输入框里按下不要开始拖（虽然方块里没有输入框，但把手可能挪到边上）
    if (e.button !== undefined && e.button !== 0) return;
    e.preventDefault();
    e.stopPropagation();
    dragging = true;
    x0 = e.clientX;
    y0 = e.clientY;
    r0 = { x: r.x, y: r.y, w: r.w, h: r.h };
    el.classList.add('dragging');
    try { el.setPointerCapture(e.pointerId); } catch (_) { /* 老浏览器没有，退回不 capture */ }
  });

  el.addEventListener('pointermove', (e) => {
    if (!dragging || !r0) return;
    e.preventDefault();
    const canvas = $('ns-floor-canvas');
    if (!canvas) return;
    const rect = canvas.getBoundingClientRect();
    // 画布是方的（CSS aspect-ratio: 1），所以宽高用同一个基准即可
    const dx = (e.clientX - x0) / Math.max(1, rect.width);
    const dy = (e.clientY - y0) / Math.max(1, rect.height);

    if (mode === 'move') {
      // ⚠️ 夹的是**中心**，且要留出半个房间的余量，否则方块会有一半跑到墙外
      r.x = nsClampRoom(r0.x + dx, -0.5 + r.w / 2, 0.5 - r.w / 2);
      r.y = nsClampRoom(r0.y + dy, -0.5 + r.h / 2, 0.5 - r.h / 2);
    } else {
      r.w = Math.min(0.5 - Math.abs(r.x) + 0.5, Math.max(NS_ROOM_MIN_W, r0.w + dx));
      r.h = Math.min(0.5 - Math.abs(r.y) + 0.5, Math.max(NS_ROOM_MIN_H, r0.h + dy));
      // 变宽之后中心可能就贴不住墙了，重新夹一次
      r.x = nsClampRoom(r.x, -0.5 + r.w / 2, 0.5 - r.w / 2);
      r.y = nsClampRoom(r.y, -0.5 + r.h / 2, 0.5 - r.h / 2);
    }

    el.style.left = `${(r.x - r.w / 2 + 0.5) * 100}%`;
    el.style.top = `${(r.y - r.h / 2 + 0.5) * 100}%`;
    el.style.width = `${r.w * 100}%`;
    el.style.height = `${r.h * 100}%`;
  });

  const finish = () => {
    if (!dragging) return;
    dragging = false;
    r0 = null;
    el.classList.remove('dragging');
    // 松手才存盘 —— 拖动过程中每帧写 localStorage 会让长按拖动变卡
    nsSave();
  };
  el.addEventListener('pointerup', finish);
  el.addEventListener('pointercancel', finish);
}

/** `＋ 加一层`：名字按"1F、2F…"顺下去，但**可改**（格式里楼层名是自由文本）。 */
function nsAddFloor(b) {
  const n = b.floors.length + 1;
  b.floors.push(nsNewFloor(`${n}F`));
  ns.floorIndex = b.floors.length - 1;
  nsSave();
  nsRenderFloorEditor(b);
  nsRenderList();
  nsStatus(`给「${b.name || '这栋楼'}」加了第 ${n} 层`);
}

/**
 * 在画布上"拖空白处"= 拉出一间新房。
 *
 * ★ 这是标注效率的关键：一间房只要**一次拖动**，不用填任何数。
 *   松手时如果拉出来的框太小（当成误点），就退回默认尺寸居中放 ——
 *   直接丢弃的话，用户"点一下想加一间"就什么都没发生，会以为坏了。
 */
function nsBindCanvasDraw(b) {
  const canvas = $('ns-floor-canvas');
  if (!canvas) return;
  let drawing = false;
  let sx = 0;
  let sy = 0;
  let ghost = null;

  canvas.addEventListener('pointerdown', (e) => {
    // 点在已有方块上是"拖那一间"，不是"画新的"
    if (e.target !== canvas && !e.target.classList.contains('ns-room-frame')) return;
    const floor = nsActiveFloor();
    if (!floor) return;
    e.preventDefault();
    drawing = true;
    const rect = canvas.getBoundingClientRect();
    sx = e.clientX;
    sy = e.clientY;
    ghost = document.createElement('div');
    ghost.className = 'ns-room ghost-room';
    canvas.appendChild(ghost);
    ghost.style.left = `${((e.clientX - rect.left) / Math.max(1, rect.width)) * 100}%`;
    ghost.style.top = `${((e.clientY - rect.top) / Math.max(1, rect.height)) * 100}%`;
    ghost.style.width = '0';
    ghost.style.height = '0';
    try { canvas.setPointerCapture(e.pointerId); } catch (_) { /* 同上 */ }
  });

  canvas.addEventListener('pointermove', (e) => {
    if (!drawing || !ghost) return;
    e.preventDefault();
    const rect = canvas.getBoundingClientRect();
    const x1 = (e.clientX - rect.left) / Math.max(1, rect.width);
    const y1 = (e.clientY - rect.top) / Math.max(1, rect.height);
    const x0 = (sx - rect.left) / Math.max(1, rect.width);
    const y0 = (sy - rect.top) / Math.max(1, rect.height);
    ghost.style.left = `${Math.min(x0, x1) * 100}%`;
    ghost.style.top = `${Math.min(y0, y1) * 100}%`;
    ghost.style.width = `${Math.abs(x1 - x0) * 100}%`;
    ghost.style.height = `${Math.abs(y1 - y0) * 100}%`;
  });

  const finish = (e) => {
    if (!drawing) return;
    drawing = false;
    const floor = nsActiveFloor();
    if (ghost) { ghost.remove(); ghost = null; }
    if (!floor) return;

    const rect = canvas.getBoundingClientRect();
    const x1 = (e.clientX - rect.left) / Math.max(1, rect.width) - 0.5;
    const y1 = (e.clientY - rect.top) / Math.max(1, rect.height) - 0.5;
    const x0 = (sx - rect.left) / Math.max(1, rect.width) - 0.5;
    const y0 = (sy - rect.top) / Math.max(1, rect.height) - 0.5;

    let w = Math.abs(x1 - x0);
    let h = Math.abs(y1 - y0);
    let cx = (x0 + x1) / 2;
    let cy = (y0 + y1) / 2;

    if (w < NS_ROOM_MIN_W || h < NS_ROOM_MIN_H) {
      // 误点：当作"在这儿加一间默认大小的"
      w = NS_ROOM_DEFAULT_W;
      h = NS_ROOM_DEFAULT_H;
      cx = x0;
      cy = y0;
    }

    const room = nsNewRoom('', '', cx, cy, w, h);
    floor.rooms.push(room);
    nsSave();
    nsRenderFloorEditor(b);
    nsRenderList();

    // 新房间的**名字**直接给焦点 —— 刚拉完框，下一步必然是打房号
    const idx = floor.rooms.length - 1;
    const host = $('ns-room-list');
    const inputs = host ? host.querySelectorAll('input') : [];
    if (inputs[idx]) inputs[idx].focus();
    nsStatus('拉出了一间房，填上房号');
  };
  canvas.addEventListener('pointerup', finish);
  canvas.addEventListener('pointercancel', () => {
    drawing = false;
    if (ghost) { ghost.remove(); ghost = null; }
  });
}

/* ------------------------------------------------------------------ 表单绑定 */

function nsWireForms() {
  $('ns-pack-id').addEventListener('input', (e) => {
    ns.packId = e.target.value.trim();
    nsSave();
    nsRenderPack();
  });
  $('ns-pack-name').addEventListener('input', (e) => {
    ns.name = e.target.value;
    nsSave();
    nsRenderPack();
  });
  $('ns-pack-note').addEventListener('input', (e) => {
    ns.note = e.target.value;
    nsSave();
  });
  $('ns-pack-version').addEventListener('input', (e) => {
    ns.version = e.target.value.trim() || '1.0.0';
    nsSave();
  });
  // 点「标定原点」那一格 = 进入拾取模式
  $('ns-pack-origin').addEventListener('click', () => {
    ns.pickMode = 'origin';
    nsStatus('现在到地图上点一下校门口（标定原点）');
    nsToast('在图上点一下，设成标定原点');
  });

  const bind = (id, key, transform) => {
    $(id).addEventListener('input', (e) => {
      const b = ns.buildings.find((x) => x.id === ns.activeId);
      if (!b) return;
      b[key] = transform ? transform(e.target.value) : e.target.value;
      nsSave();
      if (key === 'name') {
        nsRenderList();
        $('ns-ed-title').textContent = b.name || '编辑楼栋';
        nsSyncMarkers();
      }
    });
  };

  bind('ns-b-name', 'name');
  bind('ns-b-campus', 'campus');
  bind('ns-b-note', 'note');
  bind('ns-b-alias', 'alias', (v) =>
    String(v).split(/[，,、/]/).map((x) => x.trim()).filter(Boolean));

  // ★ 分类：`select` 只发 `change`，不发 `input` —— 不能用上面的 bind 助手
  $('ns-b-type').addEventListener('change', (e) => {
    const b = nsActiveBuilding();
    if (!b) return;
    b.type = e.target.value;
    nsSave();
    nsRenderList();
    nsRenderTypeSelect(b);
  });

  // 楼层改名（自由文本，所以是 input 不是 change）
  $('ns-floor-name').addEventListener('input', (e) => {
    const f = nsActiveFloor();
    if (!f) return;
    f.name = e.target.value;
    nsSave();
    // 只重画楼层标签条，不重画整个编辑器 —— 后者会打断输入
    const b = nsActiveBuilding();
    if (b) {
      const tabs = $('ns-floor-bar').querySelectorAll('.ns-floor-tab');
      if (tabs[ns.floorIndex]) {
        tabs[ns.floorIndex].textContent = f.name || `第 ${ns.floorIndex + 1} 层`;
      }
      nsRenderList();
    }
  });

  nsBindCanvasDraw(null);

  // 坐标：允许手打/粘贴，实时校验
  $('ns-b-coord').addEventListener('input', (e) => {
    const b = ns.buildings.find((x) => x.id === ns.activeId);
    if (!b) return;
    const raw = e.target.value.trim();
    const hint = $('ns-b-coord-hint');
    if (!raw) {
      b.lat = 0; b.lng = 0;
      e.target.classList.remove('bad');
      hint.className = 'ns-hint';
      hint.textContent = '纬度在前。在地图上单击就自动填进来。';
    } else {
      const p = nsParseCoord(raw);
      if (p) {
        b.lat = p.lat; b.lng = p.lng;
        e.target.classList.remove('bad');
        hint.className = 'ns-hint';
        hint.textContent = '✓ 识别为 ' + nsFmtCoord(p.lat, p.lng);
      } else {
        e.target.classList.add('bad');
        hint.className = 'ns-hint warn';
        hint.textContent = '格式不对（要「纬度,经度」），或者经纬度写反了。';
      }
    }
    nsSave();
    nsRenderList();
    nsSyncMarkers();
    nsRenderEditor();
  });

  $('ns-btn-newb').addEventListener('click', () => {
    // 新建时如果地图中心有点，就落在中心 —— 比落在 (0,0) 好（用户下一步就是拖/点）
    const c = nsMap && nsMap.getCenter ? nsMap.getCenter() : null;
    nsAddBuilding(c ? c.getLat() : 0, c ? c.getLng() : 0);
  });

  $('ns-btn-delb').addEventListener('click', () => nsDeleteBuilding(ns.activeId));
}

/* ------------------------------------------------------------------ 导入导出 */

/**
 * 组装导出对象。
 * @param {boolean} withKey 要不要把 Key 写进去
 */
function nsBuildPack(withKey) {
  return {
    format: NS_FORMAT,
    formatVersion: NS_FORMAT_VERSION,
    // ⚠️ id 加包前缀：两所学校各有一栋 `b_1` 是必然会发生的，
    //    加了前缀才可能在同一个用户的插件里共存。
    packId: ns.packId,
    name: ns.name,
    note: ns.note,
    version: ns.version,
    updatedAt: Math.floor(Date.now() / 1000),
    ...(withKey && ns.tencentKey ? { tencentKey: ns.tencentKey } : {}),
    ...(withKey && ns.referer ? { referer: ns.referer } : {}),
    ...(ns.origin ? { origin: { name: ns.name || ns.packId, lat: ns.origin.lat, lng: ns.origin.lng } } : {}),
    // ★ 地图标注（2026-10-13 深夜）。
    //
    // ⚠️ **有内容才写**（和 `type` / `floors` / `shape` 同一条规矩）：
    //    一个标注都没有时不写这个字段，老包和新包的 diff 才看得出差别。
    // ⚠️ 没文字的「文字/气泡/入口/楼梯」**也写** —— 用户可能先放个位置、
    //    回头再补字。而 `plate` / `sticker` 没内容时也留着（牌子本身就是内容）。
    ...(ns.marks.length ? {
      marks: ns.marks.map((m) => {
        const o = {
          id: (ns.packId || 'pack') + '_' + m.id,
          kind: m.kind,
          lat: Number(Number(m.lat).toFixed(6)),
          lng: Number(Number(m.lng).toFixed(6)),
          text: String(m.text || ''),
        };
        if (Number(m.w) > 0) o.w = Math.round(Number(m.w) * 10) / 10;
        if (Number(m.h) > 0) o.h = Math.round(Number(m.h) * 10) / 10;
        if (Number(m.rot)) o.rot = Math.round(Number(m.rot));
        if (m.color) o.color = String(m.color);
        if (m.icon) o.icon = String(m.icon);
        return o;
      }),
    } : {}),
    buildings: ns.buildings
      .filter((b) => String(b.name || '').trim())
      .map((b) => {
        const out = {
          id: (ns.packId || 'pack') + '_' + b.id,
          name: b.name.trim(),
          alias: (b.alias || []).filter(Boolean),
          lat: b.lat || 0,
          lng: b.lng || 0,
          campus: b.campus || '',
          note: b.note || '',
        };
        // ★ v2：分类与分层。
        //   ⚠️ 两条都是**有内容才写** —— 空 `type` / 空 `floors` 写进去
        //      只会让文件变长，而文档 §9.1 说 `type` 缺省就是 `other`、
        //      §9.2 说 `floors` 缺省就是"没标分层"。省掉能让 v1/v2 的
        //      diff 一眼看出"这条到底标了分层没有"。
        if (String(b.type || '').trim()) out.type = String(b.type).trim();

        // ★★ 3D 底面轮廓（2026-10-12 深夜）。
        //
        // ⚠️ **有内容才写**（和 type / floors 同一条规矩）：
        //    空数组写进去只会让文件变长，而"没有 shape"和"shape 是空的"
        //    在读取端本来就是同一件事（手机端会去合成一个矩形）。
        //
        // ⚠️ 少于 3 个点**不写** —— 两个点画不出面，写出去手机端会画出一条
        //    没有面积的"墙"，看起来像 bug 而不是像一栋楼。
        const shape = (b.shape || [])
          .filter((p) => Array.isArray(p) && p.length === 2)
          .map((p) => [Number(Number(p[0]).toFixed(6)), Number(Number(p[1]).toFixed(6))])
          .filter((p) => Number.isFinite(p[0]) && Number.isFinite(p[1]));
        if (shape.length >= 3) {
          out.shape = shape;
          // ★★ 只写 `false`（占位矩形），不写 `true`。
          //
          // ⚠️ 缺省就是"真的"（手机端 `optBoolean("shapeExact", true)`）——
          //    这样**老包**（没有这个字段）不会被误判成占位：
          //    历史上 `shape` 只可能来自"用户手描"或"OSM 导入"，两个都是真的。
          //    反过来（缺省 false）会让所有老数据都显示成"合成的"。
          //
          // ⚠️ 也只写"有轮廓的"：没轮廓的条目根本不写 `shape`，
          //    手机端会去合成矩形（那边 `exact=false`），不需要这里再说一遍。
          if (b.shapeExact === false) out.shapeExact = false;
        }

        const floors = (b.floors || [])
          .filter((f) => f)
          .map((f) => ({
            name: String(f.name || '').trim(),
            rooms: (f.rooms || [])
              .filter((r) => r && String(r.name || '').trim())
              .map((r) => {
                const item = {
                  name: String(r.name).trim(),
                  x: Number(nsClampRoom(r.x, -0.5, 0.5).toFixed(4)),
                  y: Number(nsClampRoom(r.y, -0.5, 0.5).toFixed(4)),
                  w: Number(Math.max(NS_ROOM_MIN_W, Number(r.w) || NS_ROOM_DEFAULT_W).toFixed(4)),
                  h: Number(Math.max(NS_ROOM_MIN_H, Number(r.h) || NS_ROOM_DEFAULT_H).toFixed(4)),
                };
                if (String(r.type || '').trim()) item.type = String(r.type).trim();
                return item;
              }),
          }))
          // 名字都空、房间也空的楼层没有信息量，别写出去
          .filter((f) => f.name || f.rooms.length);
        if (floors.length) out.floors = floors;
        return out;
      }),
  };
}

/** 导出前体检。返回 null = 通过；否则返回要用户确认的话。 */
function nsPreflight(pack) {
  const problems = [];

  if (!/^[a-z][a-z0-9-]{1,47}$/.test(ns.packId)) {
    problems.push('· 「包标识」没填或格式不对（要小写字母开头，2~48 位字母数字连字符）。用户在插件里输的就是它。');
  }
  if (!String(ns.name || '').trim()) {
    problems.push('· 「名称」没填 —— 用户导入时看到的就是这个名字。');
  }
  if (!pack.buildings.length) {
    problems.push('· 一条楼栋都没有，导出去是个空包。');
  }

  const noCoord = pack.buildings.filter((b) => b.lat === 0 && b.lng === 0);
  if (noCoord.length) {
    problems.push(`· 有 ${noCoord.length} 条还没坐标（导进去也导不了航）：` +
      noCoord.slice(0, 5).map((b) => b.name).join('、') +
      (noCoord.length > 5 ? ' 等' : ''));
  }

  if (ns.origin) {
    const far = pack.buildings.filter(
      (b) => (b.lat || b.lng) && nsDistance(ns.origin, b) > NS_FAR_METERS);
    if (far.length) {
      problems.push(`· 有 ${far.length} 条离标定原点超过 ${NS_FAR_METERS / 1000} 公里，` +
        `很可能标飞了：` + far.slice(0, 5).map((b) => b.name).join('、') +
        (far.length > 5 ? ' 等' : ''));
    }
  } else {
    problems.push('· 没设「标定原点」—— 建议设一个（校门口就行），导入的人会看到"这是哪个地方的包"。');
  }

  // ★ v2 体检：分层数据的"半成品"要提醒。
  //   这些都不是硬错误（包照样能用），所以走"可以继续"那一档。
  const lay = [];
  (pack.buildings || []).forEach((b) => {
    (b.floors || []).forEach((f) => {
      const nameless = (f.rooms || []).filter((r) => !String(r.name || '').trim()).length;
      if (nameless) {
        lay.push(`· 「${b.name}」的「${f.name || '未命名层'}」里有 ${nameless} 间房没填房号 —— 那些会被丢掉。`);
      }
      if (!(f.rooms || []).length) {
        lay.push(`· 「${b.name}」的「${f.name || '未命名层'}」是空的（一层都没标）。`);
      }
    });
    if (b.floors && !b.floors.length) {
      lay.push(`· 「${b.name}」有分层字段但一层都没有。`);
    }
  });
  if (lay.length) problems.push(...lay.slice(0, 6));
  if (lay.length > 6) problems.push(`· …还有 ${lay.length - 6} 条类似的分层问题。`);

  return problems.length ? problems.join('\n') : null;
}

function nsExport(withKey) {
  const pack = nsBuildPack(withKey);
  const problems = nsPreflight(pack);

  if (problems) {
    const head = problems.includes('包标识') || problems.includes('名称') ||
      problems.includes('一条楼栋都没有')
      ? '有几处必须先处理：\n\n'
      : '导出前提醒（可以继续）：\n\n';
    const hard = problems.includes('包标识') || problems.includes('名称') ||
      problems.includes('一条楼栋都没有');
    if (!confirm(head + problems + (hard ? '' : '\n\n还是要导出吗？'))) return;
    if (hard) return;
  }

  const text = JSON.stringify(pack, null, 2);
  const blob = new Blob([text], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  const stamp = new Date().toISOString().slice(0, 10);
  a.download = `${ns.packId || 'navpack'}-${ns.version}-${stamp}${withKey ? '-withkey' : ''}.json`;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  setTimeout(() => URL.revokeObjectURL(url), 4000);

  const rooms = nsRoomCount(pack);
  nsStatus(`已导出 ${pack.buildings.length} 条`
    + (rooms ? ` · ${rooms} 间房` : '') + `${withKey ? '（含 Key）' : '（不含 Key）'}`);
  nsToast(withKey
    ? `已导出 ${pack.buildings.length} 条 —— ⚠️ 文件里有你的 Key，别公开发`
    : `已导出 ${pack.buildings.length} 条（不含 Key）`);
}

/** 打开一个本地 .navpack.json，替换当前草稿。 */
function nsImportFile(file) {
  const reader = new FileReader();
  reader.onload = () => {
    let pack;
    try {
      pack = JSON.parse(String(reader.result));
    } catch (e) {
      nsToast('这个文件不是合法的 JSON');
      return;
    }
    if (!pack || pack.format !== NS_FORMAT) {
      nsToast('这不是导航数据包（format 字段对不上）');
      return;
    }
    // ★ v2 口径（文档 §9.5）：**接受 1 和 2** —— 升级工具不该让手里的旧包失效。
    //   但 `> 2` 必须拒绝：本版解不了更新的字段，硬解会安静地丢掉数据。
    const ver = Number(pack.formatVersion);
    if (!NS_ACCEPT_VERSIONS.includes(ver)) {
      nsToast(`包格式版本是 ${pack.formatVersion}，这个工具只认 ${NS_ACCEPT_VERSIONS.join(' 和 ')}`);
      return;
    }
    // v1 里不可能有分层数据，读回来一律按"没标分层"处理（别指望读到）
    const isV1 = ver === 1;

    const n = Array.isArray(pack.buildings) ? pack.buildings.length : 0;
    const nm = Array.isArray(pack.marks) ? pack.marks.length : 0;
    if (!confirm(`打开「${pack.name || pack.packId}」（${n} 条`
      + (nm ? ` + ${nm} 个标注` : '') + `，格式 v${ver}）？\n\n`
      + `当前草稿会被替换掉。`)) return;

    ns.packId = String(pack.packId || '');
    ns.name = String(pack.name || '');
    ns.note = String(pack.note || '');
    ns.version = String(pack.version || '1.0.0');
    ns.tencentKey = String(pack.tencentKey || '');
    ns.referer = String(pack.referer || '');
    ns.origin = pack.origin && Number.isFinite(pack.origin.lat)
      ? { lat: pack.origin.lat, lng: pack.origin.lng } : null;
    // ★★★ 标注也要读进来（2026-10-13 深夜补）。
    //
    // 用户的原话："我那些**写在文件里面的**这些也没做到。"
    //
    // `nsImportFile` 原来只读 `pack.buildings` —— 导出的包里明明有 `marks`
    // （导出端写了），打开文件时却**一个都不读**，于是"我标好的牌子，
    // 存了文件再打开就没了"。
    //
    // ⚠️ 前缀处理和楼栋那边**一模一样**：导出时加了 `<packId>_`，
    //    读回来要摘掉，否则同一个包反复导出/打开会一层层叠前缀。
    const prefixM = (pack.packId || 'pack') + '_';
    ns.marks = (Array.isArray(pack.marks) ? pack.marks : [])
      .map((m) => nsNormalizeMark(Object.assign({}, m, {
        id: String((m && m.id) || '').startsWith(prefixM)
          ? String(m.id).slice(prefixM.length) : ((m && m.id) || ''),
      })))
      .filter((m) => m.id);
    ns.activeMarkId = '';

    // 去掉导入时加的包前缀，让 id 回到本包内的短名
    const prefix = (pack.packId || 'pack') + '_';
    ns.buildings = (pack.buildings || [])
      .filter((b) => b && String(b.name || '').trim())
      .map((b) => nsNormalizeBuilding({
        id: String(b.id || '').startsWith(prefix) ? String(b.id).slice(prefix.length) : nsNewId(),
        name: String(b.name),
        alias: Array.isArray(b.alias) ? b.alias.map(String) : [],
        lat: Number(b.lat) || 0,
        lng: Number(b.lng) || 0,
        campus: String(b.campus || ''),
        note: String(b.note || ''),
        // v1 包里没有这两项，`nsNormalizeBuilding` 会给它们补上空值
        type: isV1 ? '' : b.type,
        floors: isV1 ? undefined : b.floors,
        // ★ 3D 底面轮廓（2026-10-12 深夜）。它是在 v2 里**加的可选字段**，
        //   所以老包读进来就是空数组 —— `nsNormalizeBuilding` 会兜住。
        //   ⚠️ 不给它做版本判断：可选字段的读写本来就不该跟版本挂钩
        //   （挂钩的话，将来再加字段就要再加一次版本判断，迟早漏）。
        shape: b.shape,
      }));
    ns.activeId = ns.buildings.length ? ns.buildings[0].id : '';
    ns.floorIndex = 0;

    nsSave();
    nsRenderAll();
    const rooms = nsRoomCount(ns.buildings);
    nsToast(`已打开「${ns.name || ns.packId}」，${ns.buildings.length} 条`
      + (rooms ? ` · ${rooms} 间房` : ''));
    nsStatus(isV1 ? '已载入文件（v1 包 —— 里面本来就没有分层数据）' : '已载入文件');
  };
  reader.readAsText(file, 'utf-8');
}

/* ------------------------------------------------------------------ Key 配置 */

function nsWireKey() {
  const saved = localStorage.getItem(NS_MAPKEY_KEY) || '';
  if (saved) {
    $('ns-mapkey').value = saved;
    nsInitMap(saved);
  }

  $('ns-btn-mapkey').addEventListener('click', () => {
    const k = $('ns-mapkey').value.trim();
    if (!k) {
      nsToast('先粘一个 Key 进来');
      return;
    }
    localStorage.setItem(NS_MAPKEY_KEY, k);
    ns.noMapMode = false;
    nsInitMap(k);
  });

  // ★★ 2026-10-12 加：**没有 JS Key 时的退路**。
  //
  // 这一页原来是个**死路**：地图起不来就只剩一个 Key 输入框，
  // 而用户手上那个 Key 如果**没勾 JavaScript API GL**，在这里就用不了 ——
  // 他于是卡在"一片空白、也不知道为什么"。
  //
  // ⚠️ 不是"要申请第二个 Key"：同一个 Key 在控制台勾上「JavaScript API GL」
  //    就行（用户明确纠正过这一点）。
  //
  // 但标注这件事**本来就不需要底图** —— 坐标可以从腾讯地图 App
  // 长按复制，右边的「坐标」格一直支持粘贴（`ns-b-coord`）。
  // 所以给他一条走得通的路，而不是让他对着 Key 输入框发呆。
  $('ns-btn-nomap').addEventListener('click', () => {
    ns.noMapMode = true;
    $('ns-key-gate').classList.add('hidden');
    // 地图区换成"填坐标模式"的说明 —— 否则那块就是一片没有解释的空白
    $('ns-map-fallback').classList.remove('hidden');
    // ★ 工具条 / 批量条 / 右上角那两个按钮**一起收** —— 没有底图的时候
    //   「加点 / 描轮廓 / 设原点」本来就没意义，留在那儿只会被遮罩盖住、
    //   变成"看得见点不动"（用户报的"不可用"就是这个）。
    nsSetMapChrome(false);

    // 一条楼栋都没有就先建一条 —— 否则右边的「坐标」格根本不显示
    // （编辑面板是"选中某一条"才出现的），用户会以为"填坐标"这条路也是死的。
    if (ns.buildings.length === 0) nsAddBuilding(0, 0);
    else if (!ns.activeId) ns.activeId = ns.buildings[0].id;

    nsRenderAll();
    const coord = $('ns-b-coord');
    if (coord) {
      coord.focus();
      coord.scrollIntoView({ block: 'nearest' });
    }
    nsStatus('已切到「填坐标」模式 —— 从腾讯地图 App 长按楼栋复制坐标，粘进右边的「坐标」格');
    nsToast('在腾讯地图 App 里长按那栋楼 → 复制坐标 → 粘进右边的「坐标」格');
  });

  // 反悔了，想再试一次地图
  $('ns-btn-retry-map').addEventListener('click', () => {
    ns.noMapMode = false;
    $('ns-map-fallback').classList.add('hidden');
    $('ns-map-tip').classList.remove('hidden');
    const k = ($('ns-mapkey').value || localStorage.getItem(NS_MAPKEY_KEY) || '').trim();
    if (!k) {
      $('ns-key-gate').classList.remove('hidden');
      nsStatus('填一个 JavaScript API GL 的 Key 就能看到底图');
      return;
    }
    nsInitMap(k);
  });

  // ★ 分发包里带的那个 Key（WebServiceAPI 的），和上面地图 Key 是两回事
  const host = $('ns-pack-note').parentElement.parentElement;
  const card = document.createElement('div');
  card.className = 'ns-card';
  card.style.marginTop = '8px';
  card.innerHTML =
    '<h2 class="ns-card-title">分发包里带的 Key</h2>' +
    '<label class="ns-field"><span>腾讯 WebServiceAPI Key</span>' +
    '<input id="ns-tencent-key" type="text" spellcheck="false" placeholder="留空 = 包里不带 Key" /></label>' +
    '<label class="ns-field"><span>应用名（referer，可空）</span>' +
    '<input id="ns-referer" type="text" placeholder="3e学习助手" /></label>' +
    '<p class="ns-hint warn">⚠️ 一旦导出成「含 Key」的文件并分发给别人，' +
    '这个 Key 就是明文 —— 谁拿到都能拿去刷你的额度。' +
    '请到腾讯控制台给它设用量告警，并想好随时吊销换新的。</p>';
  host.appendChild(card);

  const kEl = $('ns-tencent-key');
  const rEl = $('ns-referer');
  kEl.value = ns.tencentKey;
  rEl.value = ns.referer;
  kEl.addEventListener('input', (e) => { ns.tencentKey = e.target.value.trim(); nsSave(); });
  rEl.addEventListener('input', (e) => { ns.referer = e.target.value.trim(); nsSave(); });
}

/* ------------------------------------------------------------------ 启动 */

function nsWireTop() {
  $('ns-btn-export').addEventListener('click', () => nsExport(true));
  $('ns-btn-export-nokey').addEventListener('click', () => nsExport(false));

  $('ns-btn-import').addEventListener('click', () => {
    const input = document.createElement('input');
    input.type = 'file';
    input.accept = '.json,application/json';
    input.addEventListener('change', () => {
      if (input.files && input.files[0]) nsImportFile(input.files[0]);
    });
    input.click();
  });

  $('ns-btn-savejson').addEventListener('click', () => {
    nsSave();
    nsStatus('草稿已存在本机浏览器里（刷新不丢）');
    nsToast('草稿已保存');
  });

  $('ns-btn-help').addEventListener('click', () => $('ns-help').classList.remove('hidden'));

  // ★ 一键发布（2026-10-13）
  $('ns-btn-publish').addEventListener('click', () => nsPublishOpen());
  $('ns-btn-pub-cancel').addEventListener('click', () => nsPublishClose());
  $('ns-btn-pub-go').addEventListener('click', () => nsPublishGo());
  $('ns-btn-pub-copy').addEventListener('click', () => {
    const code = ($('ns-pub-code').textContent || '').trim();
    // ⚠️ `navigator.clipboard` 在**非安全上下文**（http 而不是 https / localhost）
    //    下是 undefined —— 直接 `.writeText` 会抛，而用户看到的是"点了没反应"。
    const okCopy = () => nsToast('授权码已复制');
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(code).then(okCopy).catch(() => nsToast('复制失败，手动选一下吧'));
    } else {
      nsToast('这个浏览器不让自动复制 —— 手动选中那串码吧');
    }
  });
  $('ns-btn-help-close').addEventListener('click', () => $('ns-help').classList.add('hidden'));
  $('ns-help').addEventListener('click', (e) => {
    if (e.target === $('ns-help')) $('ns-help').classList.add('hidden');
  });
}

function nsBoot() {
  nsLoad();
  nsHistoryReset();          // 撤销栈从这里起步
  nsWireTop();
  nsWireForms();
  nsWireKey();
  // ★★ 工具条**提前接线**（2026-10-13 晚）。
  //
  // 它原来只挂在 `nsInitMap` 的**成功回调**里 —— 也就是说"地图没起来 = 工具条
  // 点了没反应"。而工具条回答的是"点地图会发生什么"，这件事**不该依赖地图加载**：
  // 地图晚到几秒、或者加载失败，用户点它至少要有反馈（见 nsSetMapChrome 那段）。
  // 接线本身是幂等的（dataset.wired 去重），成功回调里那次照样保留。
  nsWireToolbar();
  // ★ 3D 底面轮廓那四个按钮（2026-10-12 深夜）。
  //   ⚠️ 必须在这里接 —— 少接一个的表现是"那个按钮点了没反应"，
  //      而那是最难查的一类（没有报错、也没有任何日志）。
  nsWireShape();
  // ★ 描轮廓浮动条上那两个按钮（2026-10-13 晚）
  nsWireShapeBar();
  // ★ 形状微调那三条滑条（2026-10-13 深夜）
  nsWireShapeDims();
  // ★ 标注层：拖动 + 点选（2026-10-13 深夜）
  nsWireMarkDrag();
  // ★ 标注表单（2026-10-13 深夜）
  nsWireMarkForm();
  // ★ 「! 待确认」那个常驻按钮（2026-10-13 下午）。
  //   ⚠️ 和别处一样：**少接一个的表现是"那个按钮点了没反应"** ——
  //      没有报错、没有日志，所以每个按钮都要在这里露一次脸。
  nsWireConfirmAll();
  // ★ 「取消选择」（2026-10-13 晚）：edit 面板上那个，和 Esc / 点空白是一件事。
  nsWireDeselect();
  // ★ 「清空全部」（2026-10-13 晚）：破坏性操作，接线和确认都在 nsClearAll 里
  nsWireClear();
  // ★ 确认名字那个面板（2026-10-13 晚）
  nsWirePoiReview();
  // ★ 顶栏：撤销 / 重做 / 「⋯ 更多」/ 搜索框（2026-10-13 晚界面重构）
  nsWireHistory();
  nsWireMore();
  nsWireListSearch();
  // ★ 地图视野（2026-10-13）：建图时读它、拖动时写它。
  //   放在 `nsWireKey` 之前 —— `nsInitMap` 里要用。
  view = nsLoadView();
  nsRenderAll();
  nsStatus(ns.buildings.length
    ? `已从本机草稿恢复 ${ns.buildings.length} 条`
    : '准备就绪 —— 先配地图 Key');

  // 页面关闭前再存一次（输入框的 input 事件已经存了，这是防漏网）
  window.addEventListener('beforeunload', nsSave);

  // Ctrl+S 存草稿
  window.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      nsSave();
      nsToast('草稿已保存');
    }
  });

  // ★★★ Ctrl+Z / Ctrl+Shift+Z（Ctrl+Y 也收）
  //
  // 用户的原话："加入撤回重做这些功能，让它更像一个合格的编辑器。"
  //
  // ⚠️ **不拦方向键、不拦单键 Z** —— 那会抢掉正常输入（用户正在楼名里打字）。
  // ⚠️ 用 `getModifierState('Shift')` 判断重做，因为 Ctrl+Shift+Z 里
  //    `e.key` 是 'Z'/'z'，shift 的信息得单独问。
  window.addEventListener('keydown', (e) => {
    if (!(e.ctrlKey || e.metaKey)) return;
    const k = (e.key || '').toLowerCase();
    if (k !== 'z' && k !== 'y') return;
    // ⚠️ 焦点在输入框里时，**让浏览器/系统自己的撤销先来**（那是"撤销一次打字"，
    //    比撤销一整步更符合直觉），不让它落到编辑器的撤销上。
    const t = e.target;
    if (t && (/input|textarea/i.test(t.tagName) || t.isContentEditable)) return;
    e.preventDefault();
    if (k === 'y' || e.shiftKey || e.getModifierState('Shift')) nsRedo();
    else nsUndo();
  });
}

document.addEventListener('DOMContentLoaded', nsBoot);

/* ==========================================================================
   工具条 / 地图上直接操作 / 批量 / 二次识别（2026-10-13 第二轮重做）
   ==========================================================================

   用户的原话：

   > "要加入选择工具、标点工具以及批量工具之类的，允许我进行更精细的操作，
   >  而不是我点一下就新建一个点。然后我选择也没办法在地图上直接选择点进行编辑，
   >  我需要在列表里面一一找，这不合适。而且列表里面呢，我点击以后，
   >  应该把视角自动跳转过去，这才比较合理一点。然后模型的颜色选择呢，
   >  也要更加人性化现代化一点，甚至尽量可以在地图上就对模型进行移动。
   >  还有就是再加入一个自动识别地名的功能……不是从那个 O 什么的那个公开数据
   >  里面提取，而是通过那个模型覆盖的地图的地名进行自动识别……
   >  默认还是按自动识别的结果，只不过你要标出来，例如说原本是绿色的勾，
   >  然后标记为黄色的感叹号，用户再点击一下，又会变成绿色的勾。"

   ## 这一版把"点地图"这件事**从隐式变显式**

   在这之前只有一个布尔 `pickNew`（默认还开着）—— 于是**点哪儿都建楼**：
   想选中旁边那栋看看、想拖一下位置，都会先建出一条空楼来。
   那正是"我点一下就新建一个点"抱怨的由来。

   现在工具是显式的五个，而**默认是「选择」**：

   ```
     select  选择   点标记=选中它；按住标记拖=把它挪到别处   ← 默认
     add     加点   点地图=在那个位置新建一栋
     draw    描轮廓 点地图=加一个轮廓顶点
     origin  设原点 点地图=设标定原点
     batch   批量   点标记=勾选/取消，底部出现批量操作条
   ```

   ⚠️ 工具之间**互斥**：切到某个工具时，其它"点地图"的模式要一起收干净 ——
      同时开两个的话，用户点一下到底是干嘛就说不清了（上一版踩过）。
   ========================================================================== */

/** 工具表：id / 名字 / 一句话说明（说明会进状态栏，用户不用猜）。 */
const NS_TOOLS = [
  ['select', '选择', '点标记=选中它；按住标记拖=把它挪到别处'],
  ['add', '加点', '点地图=在那个位置新建一栋（会自动找轮廓、自动填地名）'],
  ['draw', '描轮廓', '点地图=给选中的那栋加一个轮廓顶点'],
  ['origin', '设原点', '点地图=把那里设成标定原点（一般点校门口）'],
  ['batch', '批量', '点标记=勾选/取消，然后一次性改一批'],
  // ★ 标注工具（2026-10-13 深夜）。用户要的"文字说明 / 气泡 / 门牌 / 贴纸 / 入口 / 楼梯"
  //   全都落在这一个工具里 —— 点地图放一个，右边再挑具体是哪种。
  ['mark', '标注', '点地图=放一个标注（文字 / 气泡 / 门牌 / 贴纸 / 入口 / 楼梯）'],
];

/** 当前地图 Key（地点搜索要用它）。空串 = 还没配。 */
function nsMapKey() {
  const el = $('ns-mapkey');
  return ((el && el.value) || localStorage.getItem(NS_MAPKEY_KEY) || '').trim();
}

function nsToolLabel(t) {
  const hit = NS_TOOLS.find((x) => x[0] === t);
  return hit ? hit[1] : t;
}

/**
 * 切工具。
 *
 * ⚠️ 这里是"模式"的**唯一入口** —— 别在别处直接改 `ns.tool`，
 *    否则工具栏的高亮、地图上的提示、互斥模式会各说各话。
 */
function nsSetTool(t) {
  if (!NS_TOOLS.some((x) => x[0] === t)) t = 'select';
  ns.tool = t;

  // ---- 先把互斥的那些模式收干净
  if (t !== 'draw') nsShapeTargetId = null;
  if (t !== 'origin') ns.pickMode = 'building';
  if (t !== 'batch') ns.batchIds = [];
  // 拖到一半切工具的话，把"地图不能拖"这个状态还回去（不然地图变成死的）
  // ⚠️ 2026-10-13 晚：改用 nsReleaseDrag() —— 它比原来那两句多做了两件事：
  //    ① 连 `ns.press` 一起清（只清 dragging 的话，按下但还没拖的那一下会留个尾巴）
  //    ② 幂等，没在拖也照样把 draggable 设回 true（**开关归位比状态归位重要**）
  nsReleaseDrag();

  nsRenderToolbar();
  nsRenderBatchBar();
  nsRenderShapeHint();
  nsRenderAll();
  nsStatus(nsToolLabel(t) + '：' + (NS_TOOLS.find((x) => x[0] === t) || [])[2]);
}

/** 画工具条（当前工具高亮 + 批量勾选数）。 */
function nsRenderToolbar() {
  const box = $('ns-toolbar');
  if (!box) return;
  box.innerHTML = NS_TOOLS.map(([v, label]) => {
    const n = (v === 'batch' && ns.batchIds.length) ? '<b>' + ns.batchIds.length + '</b>' : '';
    return '<button class="ns-tool' + (ns.tool === v ? ' on' : '') + '" data-tool="' + v + '">'
      + label + n + '</button>';
  }).join('');
}

/** 工具条只接一次事件（用委托，所以重画不影响）。 */
function nsWireToolbar() {
  const box = $('ns-toolbar');
  if (!box || box.dataset.wired) return;
  box.dataset.wired = '1';
  box.addEventListener('click', (ev) => {
    const btn = ev.target.closest('.ns-tool');
    if (!btn) return;
    nsSetTool(btn.getAttribute('data-tool'));
  });
}

/* ---------------------------------------------------------------- 地图命中 */

/**
 * 屏幕上的一个点 → 离它最近的那一栋（容器像素，阈值 22px）。
 *
 * ## 为什么不靠标记自己的 click 事件
 *
 * `MultiMarker` 那层的 click 只在**正好点在针尖图形上**才触发 ——
 * 而用户想点的是"那栋楼"，不是"那个小图钉"。实测下来点十次能点中三次。
 * 自己按坐标算距离，宽容度是我们说了算（22px 差不多是鼠标/手指的自然误差）。
 *
 * ⚠️ 用的是 `projectToContainer` —— 它和地图事件的 `ev.point` **同一个坐标系**
 *    （都是相对容器的像素，实测两者对同一点都给出 440,411）。
 *    别混用 `ev.originalEvent.clientX`：那个是相对**视口**的，差一个滚动偏移。
 */
function nsHitTest(pt) {
  if (!nsMap || !pt || !window.TMap) return null;
  // ★★★ 判的是**图钉的整个包围盒**，不是"到一个点的半径"（2026-10-13 晚修）。
  //
  // 标记是一枚 **30×42 的图钉**，锚点在**底部尖端**（`anchor: {x:15, y:42}`）——
  // 也就是说 `lat/lng` 落在图钉**最下面那一点**，而图钉的大半个身子在它**上方** 42 像素里。
  //
  // 旧写法判的是"到 `lat/lng` 的距离 ≤ 22px" → **点图钉的头（那个圆）会落空**
  // （它在锚点上方 30px 左右，正好在 22 之外）。用户的原话：
  //
  //   "我点击一个建筑，它好像也没有选择"
  //
  // 现在按包围盒判，再带一点容差；命中多个时取**最近**的那个。
  const HALF_W = 16;   // 图钉 30 宽的一半 + 容差
  const UP = 46;       // 锚点往上的高度（42 + 容差）
  const DOWN = 10;     // 锚点往下也给一点（针尖略钝、用户常常点在它正下方）
  let best = null;
  let bestD = Infinity;
  ns.buildings.forEach((b) => {
    if (!nsHasCoord(b)) return;
    let q = null;
    try { q = nsMap.projectToContainer(new TMap.LatLng(b.lat, b.lng)); } catch (e) { return; }
    if (!q) return;
    const dx = pt.x - q.x;
    const dy = pt.y - q.y;
    if (dx < -HALF_W || dx > HALF_W) return;
    if (dy > DOWN || dy < -UP) return;
    const d = dx * dx + dy * dy;
    if (d < bestD) { bestD = d; best = b; }
  });
  return best;
}

/**
 * 把视角移到某一栋。
 *
 * ⚠️ 先试 `easeTo`（平滑，观感好），**失败或没动**就退回 `setCenter` + `setZoom`。
 *    只写 `easeTo` 是有风险的：那个 API 的选项形状我没法确证，
 *    万一"存在但不认这套参数"，地图就**不动** —— 而"点了没反应"是最难查的一类。
 *    所以给它 500ms 验一次，没动就用笨办法。
 */
function nsFlyTo(b, zoom) {
  if (!nsMap || !b || !nsHasCoord(b)) return;
  const to = new TMap.LatLng(b.lat, b.lng);
  const z = zoom || Math.max(nsMap.getZoom ? nsMap.getZoom() : 17, 18);
  const before = nsMap.getCenter ? nsMap.getCenter() : null;
  let moved = false;
  try {
    if (typeof nsMap.easeTo === 'function') {
      nsMap.easeTo({ center: to, zoom: z, duration: 420, pitch: ns.is3d ? 55 : 0 });
      moved = true;
    }
  } catch (e) { moved = false; }
  if (!moved) {
    try { nsMap.setCenter(to); nsMap.setZoom(z); } catch (e) { /* 就这样吧 */ }
    return;
  }
  setTimeout(() => {
    try {
      const c = nsMap.getCenter();
      const same = before && Math.abs(c.getLat() - before.getLat()) < 1e-6
        && Math.abs(c.getLng() - before.getLng()) < 1e-6;
      if (same) { nsMap.setCenter(to); nsMap.setZoom(z); }   // easeTo 没生效 → 兜底
    } catch (e) { /* 拿不到中心就算了，easeTo 多半已经在动 */ }
  }, 500);
}

/** 现在有没有选中哪一栋。 */
function nsIsSelected() {
  return !!ns.activeId;
}

/**
 * 选中某一栋（可选：把视角飞过去）。地图点击和列表点击**都走这里**。
 *
 * ⚠️ 它**幂等** —— 同一个 id 连调两次，第二次很轻。这是必须的：
 *    现在"点击选中"有**两条路**同时到（`mouseup` 那条主路 + SDK `click` 兜底），
 *    两条都会调它。
 */
function nsSelect(id, fly) {
  const b = ns.buildings.find((x) => x.id === id);
  if (!b) return;
  const same = ns.activeId === id && ns.pickMode === 'building';
  ns.activeId = id;
  ns.pickMode = 'building';
  if (fly) nsFlyTo(b);
  if (same && !fly) return;   // 状态没变、也不用飞 —— 不用白重画一遍
  nsSave();
  nsRenderAll();
}

/**
 * **取消选择**（用户明确要的）。
 *
 * 用户的原话：
 *
 * > "我没办法取消选择一个建筑，这也是一个非常大的问题。"
 *
 * 在这之前，选了一栋之后**没有任何出口**：右边那栏一直占着那一栋的表单、
 * 地图上那一栋一直是高亮的那个绿色图钉。用户能做的只有"去点另一栋"——
 * 而如果他只是想先看看全景、或者想把地图拖开一点，那一下高亮就一直在那儿。
 *
 * 现在三个入口都能到这儿：
 *
 * ```
 *   点地图空白处      按 Esc      右边编辑面板上的「取消选择」
 * ```
 *
 * ⚠️ 它**不删任何东西** —— 只是把"当前正在编辑哪一条"清空。
 */
function nsDeselect() {
  if (!ns.activeId) return;
  ns.activeId = '';
  ns.pickMode = 'building';
  nsSave();
  nsRenderAll();
  nsStatus('已取消选择。在左边列表里点一条、或在地图上点一个标记，就能接着编辑。');
}

/** 把「取消选择」那个按钮接上（页面初始化时调一次）。 */
function nsWireDeselect() {
  const btn = $('ns-btn-unsel');
  if (!btn || btn.dataset.wired) return;
  btn.dataset.wired = '1';
  btn.addEventListener('click', () => nsDeselect());
}

/* ---------------------------------------------------------------- 名字状态 */

/**
 * 一条楼的"名字状态" —— 列表右边那个小标记的三态。
 *
 * ## 三态是用户点名要的
 *
 *   ```
 *     ✓ 绿勾   有坐标 + 名字已确认（自己填的，或者点过确认）
 *     ! 黄叹号 名字是**二次识别**自动填的，等你看一眼     ← 默认就是这个
 *     ! 红叹号 连坐标都还没有（最严重，地图上画不出来）
 *     ? 灰问号 有坐标但还没名字
 *   ```
 *
 * 用户的原话："默认还是按自动识别的结果，只不过你要标出来，
 * 例如说原本是绿色的勾，然后标记为黄色的感叹号，用户再点击一下，
 * 又会变成绿色的勾。"
 */
function nsNameState(b) {
  if (!nsHasCoord(b)) {
    return { cls: 'no', mark: '!', title: '还没标坐标 —— 手机地图上画不出这栋' };
  }
  if (b.nameSrc === 'poi') {
    return {
      cls: 'poi',
      mark: '!',
      // ⚠️ 提示里必须**说在哪儿能一次全认**（2026-10-13 下午加）。
      //    只写"点这里变绿勾"的话，有九栋要确认的人只能一栋一栋点 ——
      //    而用户真正想要的是"一次全认"（他原话："用户再点击一下，
      //    又会变成绿色的勾"）。入口写进 title，是这条路上最后一道指路牌。
      title: '「二次识别」自动填的名字 —— 扫一眼，对了就点它变绿勾' +
        '（左边「楼栋」旁边那个「! 待确认」可以一次全认）',
    };
  }
  if (!String(b.name || '').trim()) {
    return { cls: 'noname', mark: '?', title: '有坐标，但还没有名字' };
  }
  return { cls: 'ok', mark: '✓', title: '已确认' };
}

/**
 * 还有几个名字是「二次识别自动填的、等你看一眼」（黄叹号）。
 *
 * ⚠️ 抽出来是因为它被**三处**用：提示条、左边那个常驻按钮、列表标记。
 *    三处各写一遍 `filter` 的话，判据迟早会漂。
 */
function nsPoiPending() {
  return ns.buildings.filter((b) => b.nameSrc === 'poi');
}

/**
 * ★★★ 一次确认**所有**自动识别的名字（黄叹号 → 绿勾）。
 *
 * ## 它为什么必须存在
 *
 * 用户的原话：**"没找到对应的控件或者说按钮。"**
 *
 * 他做完二次识别之后，屏幕上那句提示是：
 *
 * > 已填 9 个名字 —— 点黄叹号确认，或点「批量确认」一次全认
 *
 * 而这句话指的两个东西，他**一个都够不着**：
 *
 * ```
 *   黄叹号        只是列表里一个 11px 的小字（没底色、混在一行小字里）
 *   「批量确认」   界面上没有叫这个名字的按钮 —— 真名是「确认名字」，
 *                而且它住在 #ns-batch-bar 里，那条**只在「批量」工具下出现**，
 *                想用它得先切工具、再逐栋勾选
 * ```
 *
 * 所以这句话实际是在说："去点一个要先切工具、要逐栋勾选、而且名字还不对的
 * 按钮。" —— 用户找不到是**必然**的。
 *
 * 这个函数的定位就是把"一次全认"变成**一次点击**：不依赖模式、不需要勾选。
 * 这也正是用户的原意（"用户再点击一下，又会变成绿色的勾"）。
 *
 * ⚠️ 只动 `nameSrc === 'poi'` 的 —— 用户手填的（`'user'`）和空名字都不碰。
 *    这是在这个不可逆操作里唯一需要守住的边界。
 */
function nsConfirmAllPoi() {
  const list = nsPoiPending();
  if (!list.length) { nsToast('没有待确认的名字了'); return; }
  list.forEach((b) => { b.nameSrc = 'user'; });
  nsSave();
  nsRenderAll();
  nsStatus('已确认 ' + list.length + ' 个自动识别的名字 —— 都变绿勾了');
  const el = $('ns-toast');
  if (el) el.classList.add('hidden');
}

/**
 * 刷新左边那个「待确认 N」按钮（有才显示，没有就收起来）。
 *
 * ⚠️ 用 `hidden` 属性 + CSS 里那条 `.ns-confirm-all[hidden]{display:none}`：
 *    只写 `el.hidden = true` 是**不生效**的 —— `.ns-confirm-all` 有
 *    `display: inline-flex`，它和 UA 的 `[hidden]` 同优先级而作者样式在后，
 *    于是照样显示。这个坑项目里已经踩过一次（`.ns-cta` 那两个按钮），
 *    所以两处都要写。
 */
function nsRenderConfirmAll() {
  const btn = $('ns-btn-confirmall');
  if (!btn) return;
  const n = nsPoiPending().length;
  if (!n) { btn.hidden = true; return; }
  btn.hidden = false;
  btn.textContent = '! 待确认 ' + n;
}

/**
 * 把「待确认」那个按钮接上（页面初始化时调一次）。
 *
 * ⚠️ **绑定和渲染分开**，不在 `nsRenderConfirmAll` 里绑：
 *    那个函数每次 `nsRenderAll` 都会跑（很频繁），在里面绑就要靠
 *    `dataset.wired` 去重 —— 而 `textContent = …` 会把按钮里原有的子节点
 *    **连同监听一起清掉**，于是"绑过一次"的标记还在、监听却没了，
 *    表现是"点了没反应"，且只在第二次渲染之后出现。
 *    分开写就没有这个时序问题。
 */
function nsWireConfirmAll() {
  const btn = $('ns-btn-confirmall');
  if (!btn || btn.dataset.wired) return;
  btn.dataset.wired = '1';
  // ★ 2026-10-13 晚：它现在打开**确认面板**（可以逐条看、也能一键全用）——
  //   而不是直接闷头全确认。用户要的是"统一的确认界面"，
  //   而这个按钮就是他最常按的那个入口。
  //   「一键全确认」那条最快路径仍然在（识别完之后提示条上那个按钮）。
  btn.addEventListener('click', () => nsPoiReviewPending());
}

/* ---------------------------------------------------------------- 批量 */

function nsBatchToggle(id) {
  const i = ns.batchIds.indexOf(id);
  if (i >= 0) ns.batchIds.splice(i, 1);
  else ns.batchIds.push(id);
  nsRenderToolbar();
  nsRenderBatchBar();
  nsRenderList();
}

function nsBatchSelected() {
  return ns.buildings.filter((b) => ns.batchIds.includes(b.id));
}

/** 底部那条批量操作条（只在批量工具下出现）。 */
function nsRenderBatchBar() {
  const bar = $('ns-batch-bar');
  if (!bar) return;
  // ⚠️ 地图那一层整个收起来的时候（走「先不用地图」），批量条也要跟着收 ——
  //    它挂在图上，留着同样是「看得见点不动」。
  if (bar.dataset.mapOff) { bar.classList.add('hidden'); return; }
  if (ns.tool !== 'batch') { bar.classList.add('hidden'); return; }
  bar.classList.remove('hidden');
  const n = ns.batchIds.length;
  const c = $('ns-batch-n');
  if (c) c.textContent = n ? ('选中 ' + n + ' 栋') : '还没选（在地图上点标记，或在列表里点）';
  const sel = $('ns-batch-type');
  if (sel && !sel.options.length) {
    sel.innerHTML = NS_TYPES.map(([v, label]) =>
      '<option value="' + v + '">' + (label || v) + '</option>').join('');
  }
}

/**
 * 批量动作的总闸。
 *
 * ⚠️ 每个动作前都要**再说一次"选中了几栋"** —— 批量是破坏性操作的温床，
 *    而"我以为选了 3 栋、其实选了 30 栋"是这里最容易出的事故。
 */
async function nsBatchRun(kind) {
  const list = nsBatchSelected();
  if (!list.length) { nsToast('先在图上点标记，把要改的勾上'); return; }
  const ids = list.map((b) => b.id);

  if (kind === 'type') {
    const t = ($('ns-batch-type') || {}).value || '';
    list.forEach((b) => { b.type = t; });
    nsSave(); nsRenderAll();
    nsStatus('已把 ' + list.length + ' 栋的类别改成「' + (nsTypeLabel(t) || '未分类') + '」');
    return;
  }
  if (kind === 'poi') { await nsPoiIdentify(ids); return; }
  if (kind === 'confirm') {
    let n = 0;
    list.forEach((b) => { if (b.nameSrc === 'poi') { b.nameSrc = 'user'; n += 1; } });
    nsSave(); nsRenderAll();
    nsStatus(n ? ('已确认 ' + n + ' 个自动识别的名字') : '这几栋没有待确认的名字');
    return;
  }
  if (kind === 'shape') {
    await nsFillShapes(ids);
    return;
  }
  if (kind === 'delete') {
    // ⚠️ 破坏性操作必须**二次确认**，而且把数量说清楚
    if (!window.confirm('要删掉选中的 ' + list.length + ' 栋吗？删了只能靠撤销草稿找回。')) return;
    ns.batchIds.slice().forEach((id) => nsDeleteBuilding(id, true));
    ns.batchIds = [];
    nsSave(); nsRenderAll(); nsRenderBatchBar(); nsRenderToolbar();
    nsStatus('已删掉 ' + list.length + ' 栋');
    return;
  }
  if (kind === 'all') {
    ns.batchIds = ns.buildings.map((b) => b.id);
    nsRenderToolbar(); nsRenderBatchBar(); nsRenderList();
    return;
  }
  if (kind === 'none') {
    ns.batchIds = [];
    nsRenderToolbar(); nsRenderBatchBar(); nsRenderList();
  }
}

/* ---------------------------------------------------------------- 二次识别 */

/** 地名黑名单：这些**更近**，但它们是"楼旁边的设施"，不是楼本身。 */
const NS_POI_BAD =
  /停车场|停车位|收费|出入口|门禁|公厕|厕所|垃圾|绿化|路灯|井盖|消防|变压器|监控|信号|公交|站台|共享|充电|售票/;

/** 地名白名单：带这些字的**大概率就是一栋建筑**。 */
const NS_POI_GOOD = /楼|馆|厅|院|舍|苑|食堂|中心|教室|公寓|大厦|园|堂|场|厂|所|部|处|室|区|门/;

/** 一次最多给几个候选（再多用户就看花眼了）。 */
const NS_POI_TOPN = 3;

/**
 * 从附近 POI 里挑出**几个候选**，按"像这栋楼"的程度排序。
 *
 * ## ★★★ 为什么从"挑一个"改成"给几个"（2026-10-13 晚）
 *
 * 用户的原话：
 *
 * > "**识别效果特别差，也不知道在识别什么。**"
 *
 * 这两句是同一件事的两面：
 *
 * ```
 *   只挑一个   →  机器拍的板。它猜错的时候用户只能自己重新输名字 ——
 *                 而正确的那条**其实就在候选里**，只是被我们扔了
 *   不给候选   →  用户看不到"附近还有什么地名"，于是没法判断它凭什么选那条
 * ```
 *
 * 现在把前 3 个一起摆出来（名字 + 距离 + 类别），让用户自己点一个 ——
 * 他一眼就能看出"哦，那个才是对的"。**机器排序，人来拍板。**
 *
 * ## 为什么不取"最近的那个"
 *
 * 实测（湛江那个校区）最近的一条常常是「XX停车场 / 收费处 / 门禁」这类 ——
 * 它们**确实更近**，但它们是**楼旁边的设施**，不是楼本身。
 * 真正那栋楼可能在 40 米外，名字里带「楼 / 馆 / 食堂」。
 *
 * 所以按**打分**排：
 * ```
 *   +3   名字里有"楼/馆/厅/院/舍/食堂/中心…"（是建筑的概率大）
 *   +0~2 越近越好（60 米以外不加分）
 *   +0.5 名字不长（太长的大多是"XX公司XX分公司"这种门牌）
 *   排除 黑名单：停车场 / 收费 / 公厕 / 垃圾 / 消防 …
 * ```
 */
function nsPoiCandidates(items) {
  const out = [];
  (items || []).forEach((it) => {
    const n = String(it.name || '').trim();
    if (!n || NS_POI_BAD.test(n)) return;
    const d = Number(it.distance || 0);
    // ⚠️ 半径放到 **200 米**（原来是 150）。校园里一栋大楼的 POI 常常挂在
    //    它自己的某个边角上，150 会把"就在隔壁那栋"一起筛掉 ——
    //    而用户宁可看到多余的候选，也不想"附近明明有个地名，它就是不给我"。
    if (d > 200) return;
    let s = 0;
    if (NS_POI_GOOD.test(n)) s += 3;
    s += Math.max(0, 2 - d / 60);
    if (n.length <= 12) s += 0.5;
    out.push({
      name: n,
      distance: d,
      category: String(it.category || ''),
      score: s,
    });
  });
  out.sort((a, b) => (b.score - a.score) || (a.distance - b.distance));
  // 同名去重（腾讯偶尔会返回两条一模一样的，摆出来像是我们的 bug）
  const seen = new Set();
  return out
    .filter((c) => (seen.has(c.name) ? false : (seen.add(c.name), true)))
    .slice(0, NS_POI_TOPN);
}

/** 只想要"最像的那个"时用（打分的权威仍然是 [nsPoiCandidates]）。 */
function nsPickPoiName(items) {
  const c = nsPoiCandidates(items);
  return c.length ? c[0] : null;
}

/* ------------------------------------------------------------ 确认面板 */

/**
 * 打开「识别结果 / 确认名字」面板。
 *
 * @param rows `[{id, cands:[{name,distance,category}]}]`
 *   `cands` 为空 = "只确认现有的名字"（从左栏那个「! 待确认」进来就是这种）。
 */
function nsPoiReviewOpen(rows) {
  // ⚠️ `touched: false` 每行都要有 —— 「就按这些」靠它判断"这栋用户动过没有"。
  //    少了这个字段的话，打开面板直接按「就按这些」会把所有行都配上默认名，
  //    而用户明明什么都没挑（本轮就是抓到这个才加上的）。
  ns.poiReview = (rows || [])
    .filter((r) => ns.buildings.some((b) => b.id === r.id))
    .map((r) => ({ id: r.id, cands: r.cands || [], touched: !!r.touched }));
  if (!ns.poiReview.length) { nsToast('没有要确认的东西'); return; }
  nsRenderPoiReview();
  const box = $('ns-poi-review');
  if (box) box.classList.remove('hidden');
}

/**
 * 把面板画出来。
 *
 * ## 每一行长什么样（这是"不知道在识别什么"的正面回答）
 *
 * ```
 *   ┌────────────────────────────────────────────────┐
 *   │ ③  未命名        21.1500, 110.3000             │  ← 第几栋 / 现在叫什么 / 在哪
 *   │  ● 广东海洋大学教学主楼    18 米 · 教育         │  ← 候选（默认选第一个）
 *   │  ○ 教学主楼                45 米 · 教学楼       │
 *   │  ○ 先不管这一栋                                │
 *   └────────────────────────────────────────────────┘
 * ```
 *
 * ⚠️ **序号必须和左边列表里的序号是同一个数**（都是 `ns.buildings` 的下标 + 1）——
 *    不然用户对不上"面板里第 3 条"和"列表里第 3 条"是同一栋。
 */
function nsRenderPoiReview() {
  const box = $('ns-poi-list');
  if (!box) return;
  const title = $('ns-poi-title');
  const sub = $('ns-poi-sub');

  const withCand = ns.poiReview.filter((r) => r.cands && r.cands.length).length;
  if (title) title.textContent = withCand ? '识别结果 —— 挑一个名字' : '确认这些名字';
  if (sub) {
    sub.textContent = withCand
      ? '下面是每栋楼附近的地名（来自腾讯地图），越靠前越像它。点一条就是选它；不想用就选「先不管」。'
      : '这些名字是自动填的 —— 看一眼，对就确认，不对就选「先不管」（之后在右边自己改）。';
  }

  box.innerHTML = ns.poiReview.map((r, i) => {
    const b = ns.buildings.find((x) => x.id === r.id);
    if (!b) return '';
    const idx = ns.buildings.indexOf(b) + 1;
    const cur = b.name ? nsEsc(b.name) : '（还没名字）';
    const opts = [];

    (r.cands || []).forEach((c, k) => {
      const dist = c.distance ? (Math.round(c.distance) + ' 米') : '';
      const cat = c.category ? (' · ' + nsEsc(c.category)) : '';
      opts.push(
        '<label class="ns-poi-opt' + (k === 0 ? ' on' : '') + '">' +
          '<input type="radio" name="ns-poi-' + i + '" value="' + nsEsc(c.name) + '"' +
            (k === 0 ? ' checked' : '') + ' />' +
          '<b>' + nsEsc(c.name) + '</b>' +
          '<small>' + dist + cat + '</small>' +
        '</label>',
      );
    });

    // 没有候选（从"待确认"那边进来）：就把**现在这个名字**当成唯一选项
    if (!r.cands || !r.cands.length) {
      opts.push(
        '<label class="ns-poi-opt on">' +
          '<input type="radio" name="ns-poi-' + i + '" value="' + nsEsc(b.name || '') + '" checked />' +
          '<b>就是「' + cur + '」</b>' +
        '</label>',
      );
    }

    // ⚠️ 「先不管」的语义是**什么都不改**（不是"清掉这个名字"）。
    //    写成"清掉"太危险 —— 用户点它多半只是想跳过这一栋。
    opts.push(
      '<label class="ns-poi-opt">' +
        '<input type="radio" name="ns-poi-' + i + '" value="" />' +
        '<span>先不管这一栋</span>' +
      '</label>',
    );

    return '<div class="ns-poi-row">' +
      '<div class="ns-poi-who">' +
        '<span class="ns-poi-idx">' + idx + '</span>' +
        '<span class="ns-poi-cur">' + cur + '</span>' +
        '<span class="ns-poi-pos">' + nsFmtCoord(b.lat, b.lng) + '</span>' +
      '</div>' + opts.join('') +
    '</div>';
  }).join('');

  // ★ 选中态（`.on`）手动维护 —— CSS 里那句写了为什么不用 `:has()`。
  //
  // ⚠️ 顺手记一笔 `touched`：这栋**被用户动过**。
  // 「就按这些」只应用动过的那些（见 nsPoiReviewApply）——
  // 不然用户只想改一栋、按下去却把另外八栋全配上默认名，
  // 而他还以为"我只改了一栋"。
  box.querySelectorAll('input[type=radio]').forEach((inp) => {
    inp.addEventListener('change', () => {
      const nm = inp.getAttribute('name');
      const idx = Number(nm.replace(/^ns-poi-/, ''));
      if (Number.isFinite(idx) && ns.poiReview[idx]) ns.poiReview[idx].touched = true;
      box.querySelectorAll('input[name="' + nm + '"]').forEach((o) => {
        const lab = o.closest('.ns-poi-opt');
        if (lab) lab.classList.toggle('on', o.checked);
      });
    });
  });
}

/**
 * 应用这次确认。
 *
 * @param mode `'first'` = 每栋都用第一个候选（一键路径）；
 *             `'picked'` = 按用户现在选中的那些。
 */
function nsPoiReviewApply(mode) {
  const box = $('ns-poi-list');
  let n = 0;
  let touched = 0;
  ns.poiReview.forEach((r, i) => {
    const b = ns.buildings.find((x) => x.id === r.id);
    if (!b) return;
    let pick = '';
    if (mode === 'first') {
      pick = (r.cands && r.cands[0] && r.cands[0].name) || b.name || '';
    } else {
      // ⚠️ 只应用**用户动过**的那些行（`touched` 由 radio 的 change 记上）。
      if (!r.touched) return;
      touched += 1;
      const el = box && box.querySelector('input[name="ns-poi-' + i + '"]:checked');
      pick = el ? el.value : '';
    }
    if (!pick) return;                                     // 「先不管」= 什么都不改
    if (b.name === pick && b.nameSrc === 'user') return;   // 已经确认过、名字也一样
    b.name = pick;
    // ★ 用户在这儿**亲手拍板**了 → 直接是绿勾（不用再确认一次）。
    //   这也正是他要的："用户点击一下，又会变成绿色的勾。"
    b.nameSrc = 'user';
    n += 1;
  });
  nsPoiReviewClose();
  // ⚠️ 一栋都没碰过就别说"确认了 0 个" —— 那话没法用。告诉他现在能干嘛。
  if (mode === 'picked' && touched === 0) {
    nsToast('还没挑任何一栋 —— 点每行里的候选，或者用「全部用第一个」');
    return;
  }
  nsSave();
  nsRenderAll();
  nsStatus(n ? ('已确认 ' + n + ' 个名字（绿勾 = 确认过的）') : '这次没有改动');
  if (n) nsToast('确认了 ' + n + ' 个名字');
}

function nsPoiReviewClose() {
  ns.poiReview = [];
  const box = $('ns-poi-review');
  if (box) box.classList.add('hidden');
}

/**
 * 从左栏那个「! 待确认 N」进来 —— 把待确认的列出来逐条确认。
 *
 * ⚠️ 这条路**不联网**：列的就是"现在已经填着的名字"，用户只需要
 *    「就是它」或「先不管」。想重新找候选，走「二次识别地名」那条。
 *    两条路最后都落进**同一个面板** —— 这就是"统一的确认界面"。
 */
function nsPoiReviewPending() {
  const pend = nsPoiPending();
  if (!pend.length) { nsToast('没有待确认的名字了'); return; }
  nsPoiReviewOpen(pend.map((b) => ({ id: b.id, cands: [] })));
}

/** 把面板上那几个按钮接上（页面初始化时调一次）。 */
function nsWirePoiReview() {
  const box = $('ns-poi-review');
  if (!box || box.dataset.wired) return;
  box.dataset.wired = '1';
  // 点弹层外面的遮罩 = 关掉（和别的弹层一致）
  box.addEventListener('click', (ev) => { if (ev.target === box) nsPoiReviewClose(); });
  const f = $('ns-poi-first');
  if (f) f.addEventListener('click', () => nsPoiReviewApply('first'));
  const a = $('ns-poi-apply');
  if (a) a.addEventListener('click', () => nsPoiReviewApply('picked'));
  const c = $('ns-poi-cancel');
  if (c) c.addEventListener('click', () => nsPoiReviewClose());
}


/** 查一个坐标附近的地点（走**我们自己的**服务端代理 —— 腾讯那个接口没有 CORS 头）。 */
async function nsPoiAt(lat, lng, r) {
  const key = nsMapKey();
  if (!key) throw new Error('还没配地图 Key');
  const u = '/plugin/navstudio/api/nav/poi?key=' + encodeURIComponent(key)
    + '&lat=' + lat + '&lng=' + lng + '&r=' + (r || 120);
  const res = await fetch(u, { headers: { Accept: 'application/json' } });
  const j = await res.json().catch(() => null);
  if (!res.ok) {
    throw new Error((j && (j.error || j.message)) || ('地点搜索失败 HTTP ' + res.status));
  }
  return (j && j.items) || [];
}

/**
 * 二次识别地名。
 *
 * ## 它和"从 OSM 导入"是两个来源
 *
 * 用户特意说了："不是从那个 O 什么的那个公开数据里面提取，而是通过那个模型
 * 覆盖的地图的地名进行自动识别"。所以这一条查的是**腾讯地图的 POI**
 * （标注台底图就是腾讯，名字自然和用户看到的地图一致）。
 *
 * ⚠️ **不覆盖用户手填的名字**（`nameSrc === 'user'`）—— 那是他的劳动成果。
 *    只填空名字，和覆盖上一次自动填的（`nameSrc === 'poi'`，可以重跑刷新）。
 *
 * ⚠️ 结果标成 `nameSrc = 'poi'` → 列表上显示**黄叹号**（等确认）。
 *
 * @param onlyIds 只处理这几栋（批量工具用）。不传 = 所有有坐标的。
 */
async function nsPoiIdentify(onlyIds) {
  if (ns.poiRunning) { nsToast('已经在识别了'); return; }
  if (!nsMapKey()) { nsToast('先配一个地图 Key（地点搜索要用它）'); return; }

  const targets = ns.buildings.filter((b) =>
    nsHasCoord(b) && (!onlyIds || onlyIds.includes(b.id)) && b.nameSrc !== 'user');
  if (!targets.length) {
    nsToast('没有可识别的楼 —— 需要坐标，而且不会覆盖你自己填的名字');
    return;
  }

  ns.poiRunning = true;
  const btns = document.querySelectorAll('.ns-poi-btn');
  const setLabel = (t) => btns.forEach((el) => { el.textContent = t; });
  setLabel('识别中…');

  // ★ 攒下来的候选：每栋一条 {id, cands:[{name,distance,category}]}。
  //   **不直接改数据** —— 由用户在确认面板里拍板（见 nsPoiReviewOpen）。
  const rows = [];
  let done = 0; let filled = 0; let empty = 0; let failed = 0;
  let firstErr = '';

  // ⚠️ 并发 4：一次一栋的话 150 栋要等一分多钟（用户会以为卡死）；
  //    全并发又会被上游按"刷接口"掐。4 是这两者之间的折中。
  const CONC = 4;
  let cursor = 0;
  const worker = async () => {
    for (;;) {
      const i = cursor;
      cursor += 1;
      if (i >= targets.length) return;
      const b = targets[i];
      try {
        // ⚠️ 半径 200 米（原来是 120）。校园里一栋大楼的 POI 常常挂在它自己的
        //    某个边角上，半径太小会把正确的那条也筛掉 —— 而多出来的候选
        //    现在有人看（会摆到面板上），不再是个问题。
        const items = await nsPoiAt(b.lat, b.lng, 200);
        const cands = nsPoiCandidates(items);
        if (cands.length) {
          rows.push({ id: b.id, cands });
          filled += 1;
        } else {
          empty += 1;
        }
      } catch (e) {
        failed += 1;
        if (!firstErr) firstErr = String((e && e.message) || e);
        // ⚠️ 上游整体不通时**早点收手** —— 剩下的楼挨个失败只是白等，
        //    而且会把用户的地图 Key 配额磨掉（服务端那边也有熔断，这是双保险）。
        if (failed >= 3) return;
      }
      done += 1;
      setLabel('识别中 ' + done + '/' + targets.length);

      // ⚠️ 每栋之间**留一点间隔**：腾讯的免费配额是按 QPS 算的，
      //    4 个并发连着打很容易触发限流，而限流的表现是"有一部分楼识别不出来"，
      //    用户会以为那些楼没名字。
      await new Promise((r) => setTimeout(r, 120));
    }
  };
  await Promise.all(Array.from({ length: Math.min(CONC, targets.length) }, worker));

  ns.poiRunning = false;
  setLabel('二次识别地名');
  nsRenderAll();

  const bits = ['看了 ' + targets.length + ' 栋'];
  if (filled) bits.push('找到地名 ' + filled + ' 栋');
  if (empty) bits.push('附近没有地名 ' + empty + ' 栋');
  if (failed) bits.push('失败 ' + failed + ' 栋');
  nsStatus(bits.join(' · ') + (firstErr ? '　（第一个错误：' + firstErr + '）' : ''));

  // ★★★ 识别完**先把结果摆出来让用户挑**（2026-10-13 晚重做）。
  //
  // 用户的原话：
  //
  // > "**识别效果特别差，也不知道在识别什么。**还有就是确认的话
  // >  也**没有一个统一的界面**吧。整个操作非常的混乱。"
  //
  // 之前是"机器挑一个直接写进数据，再打个黄叹号等用户来点确认" ——
  // 于是用户既不知道它凭什么挑了那个名字，也没法换（除非自己重输一遍）。
  //
  // 现在改成：**把前 3 个候选摆出来，他点一个**。机器只负责排序，
  // 拍板的是人 —— 这才是"看得懂、改得动"。
  if (rows.length) nsPoiReviewOpen(rows);
  else if (failed) nsToast('没识别到东西 —— 看下面那行错误');
  else nsToast('附近没有可用的地名 —— 手动填一下，或者换个地图 Key');
}
