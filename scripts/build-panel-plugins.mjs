#!/usr/bin/env node
/**
 * 把标注台打包成面板插件。
 *
 * 源在 `family-schedule/web/public/`（网页端的静态目录），产物写到
 * `internal/panel/builtin/plugins/<id>/`，同时打一份 zip 到 `dist/plugins/`。
 *
 * 为什么要改写路径：标注台原本由网页端伺服，`/api/...` 和 `/nav-osm/...`
 * 都是站点根路径。装进面板之后它住在前缀 `/plugin/<id>/` 下面，根路径会
 * 打到面板自己的路由上 —— 表现是"页面能打开，但一片空白、控制台 404"。
 * 所以这里做一次**确定性的文本替换**，并在最后断言没有漏网的。
 *
 * 用法：
 *   node scripts/build-panel-plugins.mjs
 *   node scripts/build-panel-plugins.mjs --from "D:/FamilyShareProject/family-schedule/web/public"
 *   node scripts/build-panel-plugins.mjs --no-zip
 */

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const OUT_ROOT = path.join(ROOT, 'internal', 'panel', 'builtin', 'plugins');
const DIST = path.join(ROOT, 'dist', 'plugins');

const args = process.argv.slice(2);
function argValue(name, fallback) {
  const i = args.indexOf(name);
  return i >= 0 && args[i + 1] ? args[i + 1] : fallback;
}
const SOURCE = path.resolve(
  argValue('--from', 'D:/FamilyShareProject/family-schedule/web/public')
);
const MAKE_ZIP = !args.includes('--no-zip');

if (!fs.existsSync(SOURCE)) {
  console.error(`找不到源目录：${SOURCE}\n用 --from 指定 family-schedule/web/public 的位置。`);
  process.exit(1);
}

/** 逐条规则统计替换次数，最后汇总打印 —— "改了几处"要能对得上。 */
function rewrite(text, rules) {
  let out = text;
  const counts = {};
  for (const rule of rules) {
    const label = rule.label || String(rule.re);
    const n = rule.expect == null ? null : rule.expect;
    let hit = 0;
    out = out.replace(rule.re, (...m) => {
      hit++;
      return typeof rule.to === 'function' ? rule.to(...m) : rule.to;
    });
    counts[label] = (counts[label] || 0) + hit;
    if (n != null && hit !== n) {
      throw new Error(`${label} 命中 ${hit} 处，预期 ${n} 处 —— 源文件变了，先看看再改这条规则`);
    }
  }
  return { out, counts };
}

const total = {};
function bump(counts) {
  for (const [k, v] of Object.entries(counts)) total[k] = (total[k] || 0) + v;
}

function copyText(src, dest, rules) {
  const text = fs.readFileSync(src, 'utf8');
  const { out, counts } = rewrite(text, rules);
  bump(counts);
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.writeFileSync(dest, out);
  return counts;
}

function copyBinary(src, dest) {
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.copyFileSync(src, dest);
}

function copyDir(src, dest) {
  for (const e of fs.readdirSync(src, { withFileTypes: true })) {
    const s = path.join(src, e.name);
    const d = path.join(dest, e.name);
    if (e.isDirectory()) copyDir(s, d);
    else copyBinary(s, d);
  }
}

/** 页内把脚本名换掉：`nav-studio.js` → `studio.js` 这一类。 */
function renameRefs(html, map) {
  let out = html;
  for (const [from, to] of Object.entries(map)) {
    out = out.split(from).join(to);
  }
  return out;
}

function buildNavStudio() {
  const out = path.join(OUT_ROOT, 'navstudio');
  const prefix = '/plugin/navstudio/api/';

  // ⚠️ 不能把 `'/api/` 无脑全文替换掉：nav-studio.js 里有一处
  //    `name.indexOf('/api/')`，那是用来排掉腾讯 SDK 脚本（`map.qq.com/api/gljs`）
  //    的资源过滤器。改了它，SDK 会被当成瓦片处理，地图直接不显示。
  //    所以只替换**接口前缀**这几种。
  const rulesJs = [
    {
      label: 'api 前缀',
      re: /(['"`])\/api\/(auth\/|admin\/|nav\/)/g,
      to: (m, q, rest) => q + prefix + rest,
      expect: 8,
    },
    {
      label: 'nav-osm 路径',
      re: /(['"`])\/nav-osm\//g,
      to: (m, q) => q + 'nav-osm/',
      expect: 2,
    },
  ];

  copyText(
    path.join(SOURCE, 'nav-studio.js'),
    path.join(out, 'studio.js'),
    rulesJs
  );

  copyText(
    path.join(SOURCE, 'nav-studio.css'),
    path.join(out, 'studio.css'),
    []
  );

  let html = fs.readFileSync(path.join(SOURCE, 'nav-studio.html'), 'utf8');
  html = renameRefs(html, {
    'href="nav-studio.css"': 'href="studio.css"',
    'src="nav-studio.js"': 'src="studio.js"',
  });
  fs.mkdirSync(out, { recursive: true });
  fs.writeFileSync(path.join(out, 'studio.html'), html);

  const osm = path.join(SOURCE, 'nav-osm');
  if (fs.existsSync(osm)) copyDir(osm, path.join(out, 'nav-osm'));

  // 断言：改写之后不应该再有根路径的 /api/ 或 /nav-osm/。
  // 只查"接口前缀"这一类。`indexOf('/api/')` 那个资源过滤器要原样保留，
  // 用宽泛的 /\/api\// 去断言会把正确写法判成错误。
  const checks = [
    ['studio.js', /(['"`])\/api\/(auth\/|admin\/|nav\/)/],
    ['studio.js', /(['"`])\/nav-osm\//],
  ];
  const bad = [];
  for (const [file, re] of checks) {
    const text = fs.readFileSync(path.join(out, file), 'utf8');
    const m = text.match(re);
    if (m) bad.push(`${file} 里还有根路径引用：${m[0]}`);
  }
  if (bad.length) {
    console.error('构建失败：\n  ' + bad.join('\n  '));
    process.exit(1);
  }

  return out;
}

function buildSkinStudio() {
  const out = path.join(OUT_ROOT, 'skinstudio');
  const prefix = '/plugin/skinstudio/api/';
  const rulesJs = [
    {
      // ★ 这一条是必须的：源文件里写死了 https://threees.cn:8443，
      // 留着它，插件在别人的机器上会往我们的服务器发包。
      label: '硬编码服务端',
      re: /const SERVER = 'https:\/\/threees\.cn:8443';/g,
      to: "const SERVER = '';",
      expect: 1,
    },
    {
      label: '服务端展示文案',
      re: /服务端：<b>\$\{SERVER\}<\/b>/g,
      to: '服务端：<b>面板代理</b>',
      expect: 1,
    },
    {
      label: 'skin api 前缀',
      re: /(['"`])\/api\/(auth\/|admin\/|skinpacks\/)/g,
      to: (m, q, rest) => q + prefix + rest,
      expect: 3,
    },
  ];

  // 只有 skin-studio.js 里带接口调用，另两个是纯画布/主题库，原样搬。
  copyText(path.join(SOURCE, 'skin-studio.js'), path.join(out, 'studio.js'), rulesJs);
  for (const f of ['skin-core.js', 'free-canvas.js']) {
    copyText(path.join(SOURCE, f), path.join(out, f), []);
  }
  for (const f of ['skin-studio.css', 'free-canvas.css']) {
    const name = f === 'skin-studio.css' ? 'studio.css' : f;
    copyText(path.join(SOURCE, f), path.join(out, name), []);
  }

  let html = fs.readFileSync(path.join(SOURCE, 'skin-studio.html'), 'utf8');
  html = renameRefs(html, {
    'href="skin-studio.css"': 'href="studio.css"',
    'src="skin-studio.js"': 'src="studio.js"',
  });
  fs.mkdirSync(out, { recursive: true });
  fs.writeFileSync(path.join(out, 'studio.html'), html);

  const text = fs.readFileSync(path.join(out, 'studio.js'), 'utf8');
  const m = text.match(/(['"`])\/api\/(auth\/|admin\/|skinpacks\/)/);
  if (m) {
    console.error(`构建失败：skinstudio/studio.js 里还有根路径引用：${m[0]}`);
    process.exit(1);
  }
  return out;
}

const built = [];
built.push(buildNavStudio());
built.push(buildSkinStudio());

// 只改写了 JS，外壳页（index.html / wrap.js / manifest.json）是手写的，
// 这里只做存在性检查 —— 少了它们插件点进去就是 404。
for (const dir of built) {
  const id = path.basename(dir);
  for (const must of ['manifest.json', 'index.html']) {
    if (!fs.existsSync(path.join(dir, must))) {
      console.error(`缺少 ${id}/${must}`);
      process.exit(1);
    }
  }
  if (id === 'navstudio' && !fs.existsSync(path.join(dir, 'studio.html'))) {
    console.error('缺少 navstudio/studio.html');
    process.exit(1);
  }
}

console.log('路径改写：');
for (const [k, v] of Object.entries(total)) {
  console.log(`  ${JSON.stringify(k)} → ${v} 处`);
}

function sizeOf(dir) {
  let n = 0;
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else n += fs.statSync(p).size;
    }
  };
  walk(dir);
  return n;
}

for (const dir of built) {
  console.log(`  ${path.basename(dir)}: ${(sizeOf(dir) / 1024).toFixed(0)} KB`);
}

if (MAKE_ZIP) {
  fs.mkdirSync(DIST, { recursive: true });
  const py = process.env.PYTHON || 'python';
  for (const dir of built) {
    const id = path.basename(dir);
    const ver = JSON.parse(fs.readFileSync(path.join(dir, 'manifest.json'), 'utf8')).version || '0';
    const zipPath = path.join(DIST, `${id}-${ver}.zip`);
    if (fs.existsSync(zipPath)) fs.rmSync(zipPath);
    // 用 Python 打 zip：Node 标准库没有 zip 写入器，为这一个功能装依赖不值得。
    execFileSync(
      py,
      [
        '-c',
        'import os,sys,zipfile;' +
        'root,out=sys.argv[1],sys.argv[2];' +
        'z=zipfile.ZipFile(out,"w",zipfile.ZIP_DEFLATED);' +
        '[z.write(os.path.join(dp,f),os.path.relpath(os.path.join(dp,f),root)) ' +
        'for dp,_,fs in os.walk(root) for f in fs];' +
        'z.close()',
        dir,
        zipPath,
      ],
      { stdio: 'inherit' }
    );
    console.log(`  zip: ${path.relative(ROOT, zipPath)} ${(fs.statSync(zipPath).size / 1024).toFixed(0)} KB`);
  }
}
