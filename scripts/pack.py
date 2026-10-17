#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把发布物打成 zip。由 scripts/package.mjs 调用，不单独使用。

两个包：

  dist/schedule-server-windows-amd64.zip   exe + README + LICENSE + 配置样例
  dist/schedule-server-source-<版本>.zip   源码（不含 data / certs / dist / tmp）

只写 dist 下这两个 zip，不删任何目录。数据目录不在这里，也不该在这里。
"""

import os
import re
import sys
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")
BUILD = os.path.join(ROOT, "build")

SKIP_DIRS = {".git", "dist", "build", "tmp", "data", "certs", "node_modules", ".idea", ".vscode"}
SKIP_FILES = {"config.json", "deploy-token.txt", "startup-error.log"}
SKIP_EXT = {".exe", ".dll", ".log", ".db", ".db-wal", ".db-shm", ".zip"}


def version():
    with open(os.path.join(ROOT, "internal", "app", "version.go"), encoding="utf-8") as f:
        text = f.read()
    m = re.search(r'const\s+version\s*=\s*"([^"]+)"', text)
    if not m:
        raise SystemExit("读不出版本号：internal/app/version.go")
    return m.group(1)


def windows_package(ver):
    exe_name = "schedule-server.exe" if os.name == "nt" else "schedule-server"
    exe = os.path.join(BUILD, exe_name)
    if not os.path.exists(exe):
        raise SystemExit("没有编译产物：" + exe)
    out = os.path.join(DIST, "schedule-server-windows-amd64.zip")
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        z.write(exe, "schedule-server/" + exe_name)
        for name in ("README.md", "LICENSE", "config.example.json"):
            z.write(os.path.join(ROOT, name), "schedule-server/" + name)
        plugin_dir = os.path.join(DIST, "plugins")
        if os.path.isdir(plugin_dir):
            for name in sorted(os.listdir(plugin_dir)):
                if name.endswith(".zip"):
                    z.write(os.path.join(plugin_dir, name), "schedule-server/plugins/" + name)
    return out


def source_package(ver):
    out = os.path.join(DIST, "schedule-server-source-%s.zip" % ver)
    count = 0
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for dirpath, dirnames, filenames in os.walk(ROOT):
            dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
            rel = os.path.relpath(dirpath, ROOT).replace("\\", "/")
            if rel != ".":
                z.writestr("schedule-server/%s/" % rel, b"")
            for name in sorted(filenames):
                if name in SKIP_FILES:
                    continue
                if os.path.splitext(name)[1].lower() in SKIP_EXT:
                    continue
                full = os.path.join(dirpath, name)
                arc = "schedule-server/" + (name if rel == "." else rel + "/" + name)
                z.write(full, arc)
                count += 1
    return out, count


def main():
    ver = version()
    os.makedirs(DIST, exist_ok=True)
    win = windows_package(ver)
    src, count = source_package(ver)
    print("version %s" % ver)
    print("  %s  %.1f MB" % (os.path.basename(win), os.path.getsize(win) / 1048576))
    print("  %s  %.0f KB  %d 个文件" % (os.path.basename(src), os.path.getsize(src) / 1024, count))


if __name__ == "__main__":
    sys.exit(main())
