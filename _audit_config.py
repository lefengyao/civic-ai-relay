#!/usr/bin/env python3
"""找出「声明了但没人真正读」的配置项。

判定方法：把 Settings 的字段名在整个仓库里数一遍出现次数。只在
internal/config 里出现的（结构体声明 + Parse 解析 + EnvMap 回写）就是死配置——
配置界面能改、能存盘，但运行时没人读它。
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.abspath(__file__))
SKIP_DIRS = {".git", "dist", "backups", "node_modules", "__pycache__", "_e2e", ".workbuddy"}

settings = open(os.path.join(ROOT, "internal", "config", "settings.go"), encoding="utf-8").read()
block = re.search(r"type Settings struct \{(.*?)\n\}", settings, re.S)
if not block:
    sys.exit("找不到 Settings 结构体")

fields = []
for line in block.group(1).splitlines():
    line = line.strip()
    m = re.match(r"^([A-Z][A-Za-z0-9]*)\s+[\[\]\*A-Za-z0-9_.]+", line)
    if m:
        fields.append(m.group(1))

sources = []
for base, dirs, files in os.walk(ROOT):
    dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
    for name in files:
        if name.endswith((".go", ".html", ".yml", ".sh", ".py")):
            sources.append(os.path.join(base, name))

corpus = {}
for path in sources:
    rel = os.path.relpath(path, ROOT).replace("\\", "/")
    try:
        corpus[rel] = open(path, encoding="utf-8").read()
    except (UnicodeDecodeError, OSError):
        pass

print(f"{'字段':<26}{'总次数':>6}  所在文件")
print("-" * 78)
suspects = []
for field in sorted(fields):
    pattern = re.compile(r"\b" + field + r"\b")
    hits = {rel: len(pattern.findall(text)) for rel, text in corpus.items()}
    hits = {rel: n for rel, n in hits.items() if n}
    total = sum(hits.values())
    outside_config = total - hits.get("internal/config/settings.go", 0) - hits.get("internal/config/store.go", 0)
    flag = "  <-- 疑似死配置" if outside_config == 0 else ""
    if flag:
        suspects.append(field)
    print(f"{field:<26}{total:>6}  " + ", ".join(f"{r}×{n}" for r, n in sorted(hits.items()))[:90] + flag)

print()
print("疑似死配置（只在 internal/config 里出现）：" + (", ".join(suspects) if suspects else "无"))
