#!/usr/bin/env python3
"""把一个已构建的镜像打成可上传服务器的发布包。

用法（仓库根目录）：

    python scripts/package_release.py v0.3.2

前提：镜像已存在，且 tag 与版本号一致：

    docker build -f Dockerfile.relay -t civic-relay:v0.3.2 \\
      --build-arg VERSION=v0.3.2 --build-arg COMMIT=<hash> --build-arg BUILD_DATE=<iso> .

产物（全部落在 dist/ 下）：

    civic-relay-v0.3.2/                        源码快照（含 .env、DEPLOY.md、镜像包）
    civic-relay-v0.3.2-image.tar.gz            镜像，供 docker load
    civic-relay-v0.3.2-src.tar.gz              仅源码，供服务器现场构建（方式 B）
    civic-relay-v0.3.2-deploy.tar.gz           ★ 上传这个：解压即得上面那个目录

设计取舍：镜像包同时存在于快照目录内与 dist/ 顶层。前者让「上传一个文件就够」
（DEPLOY.md 里写的是 `docker load -i civic-relay-<版本>-image.tar.gz`，路径与
当前目录一致），后者与历史发布的命名保持一致。代价是约多占一份镜像体积。
"""
import gzip
import hashlib
import os
import shutil
import subprocess
import sys
import tarfile

DOCKER = os.environ.get(
    "DOCKER_BIN",
    r"C:\Users\15610\AppData\Local\Programs\DockerDesktop\resources\bin\docker.exe",
)

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(REPO, "dist")

# 快照里包含的顶层文件与目录（与历史发布保持一致：不带 dist/、不带 .workbuddy/）
TOP_FILES = [
    "Dockerfile.relay",
    "docker-compose.yml",
    "docker-compose.bind.yml",
    ".dockerignore",
    "go.mod",
    "go.sum",
    "README.md",
]
TOP_DIRS = ["cmd", "internal", "web", "scripts"]
DOCS_FILES = ["docker-deploy.md", "admin-api.md"]


def run(args, **kwargs):
    return subprocess.run(args, capture_output=True, text=True, encoding="utf-8", errors="replace", **kwargs)


def image_exists(tag):
    return run([DOCKER, "image", "inspect", tag]).returncode == 0


def build_snapshot(version):
    snapshot = os.path.join(DIST, f"civic-relay-{version}")
    if os.path.exists(snapshot):
        shutil.rmtree(snapshot)
    os.makedirs(os.path.join(snapshot, "docs"))

    for name in TOP_FILES:
        shutil.copy2(os.path.join(REPO, name), os.path.join(snapshot, name))
    for name in TOP_DIRS:
        shutil.copytree(
            os.path.join(REPO, name),
            os.path.join(snapshot, name),
            ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
        )
    for name in DOCS_FILES:
        shutil.copy2(os.path.join(REPO, "docs", name), os.path.join(snapshot, "docs", name))

    template = os.path.join(REPO, "docs", "DEPLOY.md")
    with open(template, encoding="utf-8") as handle:
        rendered = handle.read().replace("{{VERSION}}", version)
    with open(os.path.join(snapshot, "DEPLOY.md"), "w", encoding="utf-8", newline="\n") as handle:
        handle.write(rendered)

    with open(os.path.join(snapshot, ".env"), "w", encoding="ascii", newline="\n") as handle:
        handle.write(f"CIVIC_RELAY_VERSION={version}\n")
    return snapshot


def save_image(version, snapshot):
    """docker save 到未压缩 tar，再用 gzip 压。不用 shell 管道：PowerShell 的
    管道对二进制不安全，会把镜像流拆坏。"""
    tag = f"civic-relay:{version}"
    plain = os.path.join(DIST, f"civic-relay-{version}-image.tar")
    result = run([DOCKER, "save", "-o", plain, tag])
    if result.returncode != 0:
        raise SystemExit("docker save 失败：" + (result.stderr or "")[-400:])
    target_in_snapshot = os.path.join(snapshot, f"civic-relay-{version}-image.tar.gz")
    target_flat = os.path.join(DIST, f"civic-relay-{version}-image.tar.gz")
    with open(plain, "rb") as source, gzip.open(target_in_snapshot, "wb", compresslevel=6) as sink:
        shutil.copyfileobj(source, sink, 1024 * 1024)
    os.remove(plain)
    shutil.copy2(target_in_snapshot, target_flat)
    return target_flat


def make_tar(source, target, skip=None):
    """按名字排序写入，保证同一份源码打出的包内容稳定可复现。"""
    root_name = os.path.basename(source.rstrip("/\\"))
    entries = []
    for current, dirs, files in os.walk(source):
        dirs.sort()
        for name in sorted(files):
            full = os.path.join(current, name)
            relative = os.path.relpath(full, source)
            if skip and skip(relative):
                continue
            entries.append((full, os.path.join(root_name, relative)))
    with tarfile.open(target, "w:gz") as archive:
        for full, arcname in entries:
            info = archive.gettarinfo(full, arcname)
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            info.mtime = 0
            with open(full, "rb") as handle:
                archive.addfile(info, handle)
    return target


def preflight(skip):
    """发布前闸门：格式与测试不过就别打包。

    这两项曾真实漏过——手工改完代码直接打包，包里的源码是未 gofmt 的。
    加 --skip-checks 可临时跳过（例如目标机器没有 Go 工具链）。
    """
    if skip:
        print("[0/5] 跳过格式与测试检查（--skip-checks）")
        return
    print("[0/5] 检查 gofmt 与单元测试 …")
    unformatted = run(["gofmt", "-l", "internal", "web", "cmd", "scripts"], cwd=REPO)
    if unformatted.returncode != 0:
        raise SystemExit("gofmt 执行失败，确认 Go 工具链在 PATH 上：" + (unformatted.stderr or "")[-300:])
    if unformatted.stdout.strip():
        raise SystemExit("以下文件未格式化，先跑 gofmt -w：\n" + unformatted.stdout.strip())
    tested = run(["go", "test", "-count=1", "./..."], cwd=REPO)
    if tested.returncode != 0:
        raise SystemExit("单元测试失败，修好再打包：\n" + ((tested.stdout or "") + (tested.stderr or ""))[-2000:])


def write_checksums():
    """覆盖 dist/ 下所有发布压缩包。整体重写而不是追加：同一版本重打时不会留下
    过期的旧哈希。"""
    lines = []
    for name in sorted(os.listdir(DIST)):
        if not name.endswith(".tar.gz"):
            continue
        digest = hashlib.sha256()
        with open(os.path.join(DIST, name), "rb") as handle:
            for block in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(block)
        lines.append(f"{digest.hexdigest()} *{name}")
    target = os.path.join(DIST, "SHA256SUMS.txt")
    with open(target, "w", encoding="ascii", newline="\n") as handle:
        handle.write("\n".join(lines) + "\n")
    return target


def main():
    args = [item for item in sys.argv[1:] if not item.startswith("--")]
    skip_checks = "--skip-checks" in sys.argv
    if len(args) != 1:
        raise SystemExit(__doc__)
    version = args[0].strip()
    if not version.startswith("v"):
        raise SystemExit(f"版本号应以 v 开头，例如 v0.3.2（收到 {version!r}）")
    os.makedirs(DIST, exist_ok=True)

    tag = f"civic-relay:{version}"
    if not image_exists(tag):
        raise SystemExit(f"本地找不到镜像 {tag}，先构建再打包（见本脚本头部注释）")

    preflight(skip_checks)

    print(f"[1/5] 生成源码快照 dist/civic-relay-{version}/ …")
    snapshot = build_snapshot(version)

    print(f"[2/5] 导出镜像 {tag} …")
    image_path = save_image(version, snapshot)

    print("[3/5] 打仅源码包 …")
    src_tar = make_tar(
        snapshot,
        os.path.join(DIST, f"civic-relay-{version}-src.tar.gz"),
        skip=lambda rel: rel.endswith("-image.tar.gz"),
    )

    print("[4/5] 打一体化上传包 …")
    deploy_tar = make_tar(snapshot, os.path.join(DIST, f"civic-relay-{version}-deploy.tar.gz"))

    print("[5/5] 生成 SHA256SUMS.txt …")
    checksums = write_checksums()

    print("\n产物：")
    for path in (snapshot, image_path, src_tar, deploy_tar, checksums):
        size = "-"
        if os.path.isfile(path):
            size = f"{os.path.getsize(path) / 1024 / 1024:.2f} MB"
        elif os.path.isdir(path):
            total = sum(
                os.path.getsize(os.path.join(base, name))
                for base, _, names in os.walk(path)
                for name in names
            )
            size = f"{total / 1024 / 1024:.2f} MB"
        print(f"  {os.path.relpath(path, REPO)}  ({size})")
    print(f"\n上传 {os.path.relpath(deploy_tar, REPO)} 到服务器，解压后 docker load 再 docker compose up -d。")
    print("上传后可用 sha256sum -c SHA256SUMS.txt 校验完整性。")


if __name__ == "__main__":
    main()
