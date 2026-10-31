#!/usr/bin/env bash
# 为「服务器没有 Go 工具链、也访问不到 GitHub」的部署方式产出可复现包。
#
# 用法（仓库根目录，Git Bash）：
#
#     bash scripts/build_deploy_bundle.sh v0.3.9
#
# 产物（dist/deploy-<版本>/）：
#
#     civic-relay        linux/amd64 静态二进制，版本号已烧进 ldflags
#     src.tar.gz         git archive HEAD —— 服务器解压即得该 commit 的完整树
#     DEPLOYED_COMMIT    版本 / commit / 构建时间 / 二进制 sha256
#     SHA256SUMS
#
# 设计取舍：源码用 `git archive HEAD` 而不是打包工作区，服务器上的树因此永远
# 等于某个 commit，不会再漂成「只存在于服务器上的改动」。工作区不干净时直接
# 拒绝构建——否则产物回溯不到 commit，正是要消除的毛病。
#
# 为什么是 bash 而不是 Python：本机没装 Python（只有 WindowsApps 的空壳别名，
# 跑起来无输出、退出码 0），仓库里原有的 scripts/*.py 目前全都跑不起来。
# bash + go + git + sha256sum 是这台机器上确实可用的组合。
#
# 传输与部署步骤见 docs/deploy-prebuilt.md（服务器侧只需要 docker）。
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

usage() {
  echo "用法：bash scripts/build_deploy_bundle.sh <版本号>   例如 v0.3.9" >&2
  exit 1
}

[[ $# -eq 1 ]] || usage
VERSION="$1"

command -v go >/dev/null 2>&1 || { echo "找不到 go，请确认已安装并在 PATH 里" >&2; exit 1; }

DIRTY="$(git status --porcelain)"
if [[ -n "$DIRTY" ]]; then
  echo "工作区不干净，产物无法回溯到 commit。先提交或 stash：" >&2
  echo "$DIRTY" >&2
  exit 1
fi

COMMIT="$(git rev-parse HEAD)"
SHORT="${COMMIT:0:8}"
BUILT="$(date +%Y-%m-%dT%H:%M:%S%:z)"

OUT="dist/deploy-$VERSION"
mkdir -p "$OUT"

echo "交叉编译 linux/amd64 → $OUT/civic-relay"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOPROXY="${GOPROXY:-off}" GOTOOLCHAIN=local \
  go build -trimpath -mod=readonly \
    -ldflags "-s -w -X main.version=$VERSION -X main.commit=$SHORT -X main.buildDate=$BUILT" \
    -o "$OUT/civic-relay" ./cmd/civic-relay

MAGIC="$(head -c 4 "$OUT/civic-relay" | od -An -tx1 | tr -d ' \n')"
[[ "$MAGIC" == "7f454c46" ]] || {
  echo "产物不是 ELF（magic=$MAGIC），交叉编译没生效" >&2; exit 1; }

echo "导出源码快照 git archive HEAD → $OUT/src.tar.gz"
git archive --format=tar.gz --output="$OUT/src.tar.gz" HEAD

BIN_SUM="$(sha256sum "$OUT/civic-relay" | cut -d' ' -f1)"
printf 'version=%s\ncommit=%s\nbuilt=%s\nbinary_sha256=%s\n' \
  "$VERSION" "$COMMIT" "$BUILT" "$BIN_SUM" > "$OUT/DEPLOYED_COMMIT"

( cd "$OUT" && sha256sum civic-relay src.tar.gz DEPLOYED_COMMIT > SHA256SUMS )

printf '\n完成：%s\n  commit  %s\n  built   %s\n  binary  %s\n  体积    二进制 %s MiB，源码 %s KiB\n' \
  "$OUT" "$SHORT" "$BUILT" "$BIN_SUM" \
  "$(( $(stat -c%s "$OUT/civic-relay") / 1048576 ))" \
  "$(( $(stat -c%s "$OUT/src.tar.gz") / 1024 ))"
