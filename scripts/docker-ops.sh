#!/usr/bin/env bash
# Civic Relay 中文运维脚本
#
#   ./scripts/docker-ops.sh 帮助
#
# 在 Git Bash / Linux / macOS 下均可运行。Windows 上会自动定位 Docker Desktop
# 的 docker.exe：PATH 里那个无扩展名的 `docker` 是 sh 脚本，在 Git Bash 下直接
# 调用会报 "/usr/bin/env: 'sh': No such file or directory"。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINER="civic-ai-relay-civic-relay-1"
CONFIG_VOLUME="civic-ai-relay_civic-relay-config"
DATA_VOLUME="civic-ai-relay_civic-relay-data"

c_info() { printf '\033[36m%s\033[0m\n' "$*"; }
c_ok()   { printf '\033[32m%s\033[0m\n' "$*"; }
c_warn() { printf '\033[33m%s\033[0m\n' "$*"; }
c_err()  { printf '\033[31m%s\033[0m\n' "$*"; }
title()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

# ---------- 定位 docker 客户端 ----------
locate_docker() {
  if command -v docker.exe >/dev/null 2>&1; then
    command -v docker.exe
    return 0
  fi
  local candidates=(
    "${LOCALAPPDATA:-}/Programs/DockerDesktop/resources/bin/docker.exe"
    "/c/Users/${USERNAME:-$USER}/AppData/Local/Programs/DockerDesktop/resources/bin/docker.exe"
    "/Applications/Docker.app/Contents/Resources/bin/docker"
    "/usr/local/bin/docker"
    "/usr/bin/docker"
  )
  local p
  for p in "${candidates[@]}"; do
    if [ -n "$p" ] && [ -x "$p" ]; then
      printf '%s' "$p"
      return 0
    fi
  done
  return 1
}

if ! DOCKER="$(locate_docker)"; then
  c_err "找不到 docker 客户端。请先安装 Docker Desktop，或把它加入 PATH。"
  exit 1
fi

DC=("$DOCKER" compose -f "$ROOT/docker-compose.yml")

require_engine() {
  if ! "$DOCKER" info >/dev/null 2>&1; then
    c_err "Docker 引擎没有运行。"
    c_warn "Windows 上请先从开始菜单手动启动 Docker Desktop，等状态变为 Running 再执行本脚本。"
    exit 1
  fi
}

# docker cp / -v 需要原生路径，Git Bash 不会正确转换
native_path() {
  if command -v cygpath >/dev/null 2>&1; then
    cygpath -w "$1"
  else
    printf '%s' "$1"
  fi
}

wait_healthy() {
  local i status
  for i in $(seq 1 30); do
    status="$("$DOCKER" inspect "$CONTAINER" --format '{{.State.Health.Status}}' 2>/dev/null || true)"
    if [ "$status" = "healthy" ]; then
      c_ok "容器已就绪（healthy）"
      return 0
    fi
    sleep 2
  done
  c_warn "等待超时，容器仍未 healthy。请执行：$0 日志"
  return 1
}

cmd_up() {
  require_engine
  title "构建并启动（版本 ${CIVIC_RELAY_VERSION:-dev}）"
  (cd "$ROOT" && "${DC[@]}" up -d --build "$@")
  wait_healthy
  cmd_status
}

cmd_down() {
  require_engine
  title "停止并移除容器（数据卷保留）"
  (cd "$ROOT" && "${DC[@]}" down "$@")
  c_ok "已停止。数据仍在卷 ${CONFIG_VOLUME} / ${DATA_VOLUME} 中。"
}

cmd_restart() {
  require_engine
  title "重启容器"
  (cd "$ROOT" && "${DC[@]}" restart)
  wait_healthy
}

cmd_status() {
  require_engine
  title "容器状态"
  (cd "$ROOT" && "${DC[@]}" ps)
  title "本地镜像"
  "$DOCKER" images civic-relay --format '{{.Repository}}:{{.Tag}}  {{.Size}}'
}

cmd_probe() {
  require_engine
  local port="${CIVIC_RELAY_PORT:-8000}"
  title "健康探针（宿主机 127.0.0.1:${port}）"
  local path result
  for path in /healthz /readyz; do
    result="$(RELAY_PORT="$port" RELAY_PATH="$path" python -c '
import http.client, os
try:
    c = http.client.HTTPConnection("127.0.0.1", int(os.environ["RELAY_PORT"]), timeout=5)
    c.request("GET", os.environ["RELAY_PATH"])
    r = c.getresponse()
    print(r.status, r.read().decode().strip())
except Exception as exc:
    print("连接失败:", exc)
' 2>/dev/null || echo "无法探测（本机可能没有 python）")"
    printf '  %-9s -> %s\n' "$path" "$result"
  done
  title "容器运行态"
  "$DOCKER" inspect "$CONTAINER" \
    --format '健康={{.State.Health.Status}}  用户={{.Config.User}}  只读根={{.HostConfig.ReadonlyRootfs}}  时区={{range .Config.Env}}{{if eq (printf "%.3s" .) "TZ="}}{{.}}{{end}}{{end}}' 2>/dev/null \
    || c_warn "容器未运行"
}

cmd_logs() {
  require_engine
  (cd "$ROOT" && "${DC[@]}" logs --tail="${1:-100}" -f civic-relay)
}

cmd_volumes() {
  require_engine
  title "配置卷内容（${CONFIG_VOLUME}）"
  "$DOCKER" run --rm -v "$CONFIG_VOLUME":/c alpine sh -c 'ls -la /c'
  title "数据卷内容（${DATA_VOLUME}）"
  "$DOCKER" run --rm -v "$DATA_VOLUME":/d alpine sh -c 'ls -la /d'
  c_warn "镜像基于 distroless（无 shell），所以用 alpine 辅助容器查看卷内容。"
}

cmd_admin_key() {
  require_engine
  title "管理员密钥（首次启动生成，仅供取用）"
  "$DOCKER" run --rm -v "$CONFIG_VOLUME":/c alpine sh -c \
    'cat /c/bootstrap-admin-key.txt 2>/dev/null || echo "（文件不存在：可能已删除）"'
  c_warn "取用后建议执行：$0 删密钥"
}

cmd_rm_key() {
  require_engine
  title "删除一次性管理员密钥文件"
  "$DOCKER" run --rm -v "$CONFIG_VOLUME":/c alpine sh -c \
    'rm -f /c/bootstrap-admin-key.txt && echo 已删除'
}

cmd_backup() {
  require_engine
  local target="${1:-$ROOT/backups/$(date +%Y%m%d-%H%M%S)}"
  mkdir -p "$target"
  title "备份到 $target"
  c_warn "先停容器：WAL 模式下未 checkpoint 的事务还在 -wal 里，热拷贝会丢最新数据。"
  (cd "$ROOT" && "${DC[@]}" stop >/dev/null)
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$CONTAINER:/app/config/." "$(native_path "$target/config")"
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$CONTAINER:/app/data/." "$(native_path "$target/data")"
  (cd "$ROOT" && "${DC[@]}" start >/dev/null)
  wait_healthy
  title "备份内容"
  ls -la "$target/config" "$target/data"
  c_ok "备份完成：$target"
  c_warn "必须与 relay.env 一起保存：里面的 RELAY_ENCRYPTION_KEY 一旦丢失，库中的上游密钥将无法解密。"
}

cmd_restore() {
  require_engine
  local source="${1:-}"
  if [ -z "$source" ] || [ ! -d "$source" ]; then
    c_err "用法：$0 恢复 <备份目录>"
    exit 1
  fi
  c_err "⚠️ 此操作非常危险，可能导致不可逆的数据丢失！"
  c_err "将用 [${source}] 覆盖当前的配置与全部账目，且无法撤销。"
  read -r -p "确认继续请输入 yes：" answer
  if [ "$answer" != "yes" ]; then
    c_warn "已取消。"
    return 0
  fi
  (cd "$ROOT" && "${DC[@]}" stop >/dev/null)
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$(native_path "$source/config/.")" "$CONTAINER:/app/config"
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$(native_path "$source/data/.")" "$CONTAINER:/app/data"
  (cd "$ROOT" && "${DC[@]}" start >/dev/null)
  wait_healthy
  c_ok "恢复完成。"
}

cmd_upgrade() {
  require_engine
  if [ -n "${1:-}" ]; then
    export CIVIC_RELAY_VERSION="$1"
  fi
  title "升级到版本 ${CIVIC_RELAY_VERSION:-dev}（数据库迁移在启动时自动执行）"
  (cd "$ROOT" && "${DC[@]}" up -d --build --force-recreate)
  wait_healthy
  cmd_probe
  c_warn "回滚：把 CIVIC_RELAY_VERSION 改回旧标签重建即可；跨版本回滚前请先备份。"
}

cmd_help() {
  cat <<'EOF'

Civic Relay 中文运维脚本

用法：./scripts/docker-ops.sh <命令> [参数]

  启动              构建镜像并启动容器，等它变 healthy
  停止              停止并移除容器（数据卷保留，数据不丢）
  重启              重启容器
  状态              查看容器状态与本地镜像
  探针              检查 /healthz、/readyz 与容器健康、时区
  日志 [行数]       跟踪日志，默认 100 行
  看卷              查看配置卷与数据卷内容
  管理员密钥        打印首次启动生成的管理员密钥
  删密钥            删除一次性管理员密钥文件
  备份 [目录]       停容器后完整备份配置与数据库
  恢复 <目录>       用备份覆盖当前数据（不可逆，需二次确认）
  升级 [版本]       重建镜像并升级

可用环境变量：
  CIVIC_RELAY_VERSION   镜像版本号，默认 dev
  CIVIC_RELAY_PORT      宿主机端口，默认 8000
  CIVIC_RELAY_BIND      绑定地址，默认 127.0.0.1（局域网访问设 0.0.0.0）
  TZ                    容器日志时区，默认 Asia/Shanghai

EOF
}

case "${1:-帮助}" in
  启动 | up)              shift; cmd_up "$@" ;;
  停止 | down)            shift; cmd_down "$@" ;;
  重启 | restart)         shift; cmd_restart "$@" ;;
  状态 | status)          cmd_status ;;
  探针 | health)          cmd_probe ;;
  日志 | logs)            shift; cmd_logs "$@" ;;
  看卷 | volumes)         cmd_volumes ;;
  管理员密钥 | admin-key) cmd_admin_key ;;
  删密钥 | rm-key)        cmd_rm_key ;;
  备份 | backup)          shift; cmd_backup "$@" ;;
  恢复 | restore)         shift; cmd_restore "$@" ;;
  升级 | upgrade)         shift; cmd_upgrade "$@" ;;
  帮助 | help | -h | --help) cmd_help ;;
  *)
    c_err "未知命令：$1"
    cmd_help
    exit 1
    ;;
esac
