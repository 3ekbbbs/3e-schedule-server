#!/usr/bin/env node
/**
 * 打发布包。编译产物在 build/，zip 在 dist/。
 *
 * 这个脚本存在的理由只有一个：**不许碰数据**。
 *
 * 以前打包直接删 dist 再来一遍，而 dist 有时正好是用户跑起来的那一份
 * （里面有 data/、certs/、config.json），一次打包就把业务数据清了。
 * 现在：
 *   · 编译只写 build/
 *   · 只覆盖 dist 下几个固定的 zip 名字
 *   · 发现 build/ 或 dist/ 里有运行时的东西，直接停下，不删任何东西
 *
 * 用法：
 *   node scripts/package.mjs                 用 PATH 里的 go
 *   GO=/path/to/go node scripts/package.mjs  指定 go
 *   node scripts/package.mjs --skip-plugins  不改插件（插件源码没动时）
 */

import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const BUILD = path.join(ROOT, 'build');
const DIST = path.join(ROOT, 'dist');
const args = process.argv.slice(2);
const SKIP_PLUGINS = args.includes('--skip-plugins');

// 运行时才会有的东西。打包目录里出现它们，说明有人把服务直接跑在这里了。
const RUNTIME = ['data', 'certs', 'config.json'];

function findRuntime(dir) {
  const hits = [];
  for (const name of RUNTIME) {
    const p = path.join(dir, name);
    if (fs.existsSync(p)) hits.push(p);
  }
  return hits;
}

function guard() {
  for (const dir of [DIST, BUILD]) {
    const hits = findRuntime(dir);
    if (hits.length) {
      console.error('停止打包：' + dir + ' 里有运行时的数据：');
      for (const h of hits) console.error('  ' + h);
      console.error('');
      console.error('这里不能当构建目录用 —— 打包会覆盖它。先做其中一件：');
      console.error('  1. 把 data/ 和 certs/ 挪到别处，再把这个目录清空；');
      console.error('  2. 或者干脆别动，用面板首页的「搬到 …」把数据搬到程序目录外。');
      process.exit(1);
    }
  }
}

function run(cmd, argv, opts = {}) {
  console.log('> ' + cmd + ' ' + argv.join(' '));
  execFileSync(cmd, argv, { stdio: 'inherit', cwd: ROOT, ...opts });
}

function build() {
  fs.mkdirSync(BUILD, { recursive: true });
  const go = process.env.GO || 'go';
  const out = path.join(BUILD, process.platform === 'win32' ? 'schedule-server.exe' : 'schedule-server');
  const flags = process.platform === 'win32' ? '-s -w -H windowsgui' : '-s -w';
  run(go, ['build', '-trimpath', '-ldflags', flags, '-o', out, './cmd/schedule-server'], {
    env: { ...process.env, CGO_ENABLED: '0' },
  });
  const size = fs.statSync(out).size;
  console.log('  编译完成：' + out + '  ' + (size / 1048576).toFixed(1) + ' MB');
}

function plugins() {
  const node = process.execPath;
  run(node, [path.join(ROOT, 'scripts', 'build-panel-plugins.mjs')]);
}

function pack() {
  const py = process.env.PYTHON || (process.platform === 'win32' ? 'python' : 'python3');
  run(py, [path.join(ROOT, 'scripts', 'pack.py')]);
}

guard();
build();
if (!SKIP_PLUGINS) plugins();
pack();

console.log('');
console.log('完成。build/ 是编译产物，dist/ 是要发出去的包。');
console.log('运行时的 data 与 certs 不在其中，也没有被改动。');
