#!/usr/bin/env python3
"""打包 Trailblazer Deploy（部署管理器）可用的站点 zip。

用法：
    python scripts/package_site_zip.py            # dist/civic-relay-site-<时间戳>.zip
    python scripts/package_site_zip.py -o 路径.zip # 指定输出

部署工具期望的包结构（站点档案 package.requiredFiles 校验）：
    Dockerfile.relay、go.mod、go.sum、cmd/、internal/、web/

只打包构建镜像真正需要的文件——测试、文档、脚本、发布产物都不进去，
zip 越小上传越快；构建上下文干净也避免把本机垃圾带进镜像层。
"""

from __future__ import annotations

import argparse
import fnmatch
import datetime as _dt
import os
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

INCLUDE_FILES = ("Dockerfile.relay", ".dockerignore", "go.mod", "go.sum")
INCLUDE_DIRS = ("cmd", "internal", "web")

# 目录内跳过的名字：隐藏目录与 Go 构建的本地产物。
SKIP_DIR_NAMES = {"testdata", "test-data", "__pycache__", "node_modules", "data", "backups"}
SKIP_FILE_PATTERNS = ("*_test.go", "*.test.js", "*.spec.js", "relay.env", ".env*",
                      "bootstrap-admin-key.txt", "*.db", "*.db-*", "*.pem", "*.key",
                      "*.exe", "*.dll", "*.log")


def _collect_files() -> list[tuple[str, str]]:
    """返回 (zip 内路径, 磁盘绝对路径) 列表，路径一律用 POSIX 分隔符。"""
    entries: list[tuple[str, str]] = []

    for name in INCLUDE_FILES:
        path = os.path.join(ROOT, name)
        if not os.path.isfile(path):
            raise SystemExit(f"缺少必需文件：{name}")
        entries.append((name, path))

    for top in INCLUDE_DIRS:
        base = os.path.join(ROOT, top)
        if not os.path.isdir(base):
            raise SystemExit(f"缺少必需目录：{top}/")
        for current, dirnames, filenames in os.walk(base):
            dirnames[:] = [
                d for d in dirnames
                if not d.startswith(".") and not d.startswith("__debug_bin") and d not in SKIP_DIR_NAMES
            ]
            for filename in filenames:
                if filename.startswith(".") or any(fnmatch.fnmatch(filename.lower(), pattern) for pattern in SKIP_FILE_PATTERNS):
                    continue
                disk = os.path.join(current, filename)
                arc = os.path.relpath(disk, ROOT).replace(os.sep, "/")
                entries.append((arc, disk))

    return entries


def main() -> None:
    parser = argparse.ArgumentParser(description="打包部署工具可用的站点 zip")
    parser.add_argument("-o", "--output", help="输出 zip 路径（默认 dist/ 下按时间戳命名）")
    args = parser.parse_args()

    entries = _collect_files()
    if args.output:
        output = os.path.abspath(args.output)
    else:
        stamp = _dt.datetime.now().strftime("%Y%m%d-%H%M%S")
        dist = os.path.join(ROOT, "dist")
        os.makedirs(dist, exist_ok=True)
        output = os.path.join(dist, f"civic-relay-site-{stamp}.zip")

    os.makedirs(os.path.dirname(output), exist_ok=True)
    with zipfile.ZipFile(output, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as bundle:
        for arc, disk in sorted(entries):
            bundle.write(disk, arc)

    with zipfile.ZipFile(output) as bundle:
        if bundle.testzip() is not None:
            raise SystemExit('源码 ZIP 校验失败')

    size_kb = os.path.getsize(output) / 1024
    print(f"已生成 {output}")
    print(f"  条目 {len(entries)} 个，大小 {size_kb:.0f} KiB")


if __name__ == "__main__":
    main()
