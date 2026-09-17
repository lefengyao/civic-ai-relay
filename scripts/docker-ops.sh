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

c_info() { printf '\033[36m%s\033[0m\n' "$*"; }
c_ok()   { printf '\033[32m%s\033[0m\n' "$*"; }
c_warn() { printf '\033[33m%s\033[0m\n' "$*"; }
c_err()  { printf '\033[31m%s\033[0m\n' "$*"; }
title()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

# 容器名与卷名都随 compose 项目名变化，而项目名默认取**部署目录名**。
# 早期版本把这三个值写死成 civic-ai-relay_*，一旦用户把目录改名（例如装到
# /opt/civic-relay），取密钥/备份/看卷就会报「找不到卷」。全部改成按当前目录推导。
project_name() { printf '%s' "${COMPOSE_PROJECT_NAME:-$(basename "$ROOT")}"; }
config_volume() { printf '%s_civic-relay-config' "$(project_name)"; }
data_volume()   { printf '%s_civic-relay-data' "$(project_name)"; }

# 容器 ID 交给 compose 解析，避免拼容器名（不同 compose 版本规则不同）。
container_id() {
  (cd "$ROOT" && "${DC[@]}" ps -q civic-relay 2>/dev/null | head -n1) || true
}

require_container() {
  local id
  id="$(container_id)"
  if [ -z "$id" ]; then
    c_err "找不到 civic-relay 容器（当前目录：$ROOT，项目名：$(project_name)）"
    c_warn "先确认它已启动：$0 状态"
    c_warn "若容器其实在别的目录里跑，cd 过去再执行本脚本；或设 COMPOSE_PROJECT_NAME=<项目名>"
    exit 1
  fi
  printf '%s' "$id"
}

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
    c_warn "Linux 上试：sudo systemctl start docker"
    exit 1
  fi
  # Ubuntu 用 apt 装的 docker.io 常常不带 compose v2 插件，这一步专门拦它。
  if ! "$DOCKER" compose version >/dev/null 2>&1; then
    if command -v docker-compose >/dev/null 2>&1; then
      c_err "本脚本需要 Docker Compose v2（\`docker compose\`），检测到的是旧的 \`docker-compose\` v1。"
    else
      c_err "找不到 Docker Compose 插件（\`docker compose\`）。"
    fi
    c_warn "Ubuntu 安装：sudo apt update && sudo apt install -y docker-compose-v2"
    c_warn "（Debian 12 / Ubuntu 24.04 之后包名就是 docker-compose-v2；老发行版用 docker-compose）"
    exit 1
  fi
}

# 找出机器上所有已存在的 civic-relay 配置卷（可能属于别的目录名）
existing_config_volumes() {
  "$DOCKER" volume ls --format '{{.Name}}' 2>/dev/null | grep '_civic-relay-config$' || true
}

cmd_doctor() {
  # 刻意不调用 require_engine：体检的意义就是把「compose 插件缺失」这类问题
  # 报出来，而不是自己先死在门槛上。
  local env_version="" volumes="" current_project="" port
  if ! "$DOCKER" info >/dev/null 2>&1; then
    c_err "Docker 引擎没有运行（Linux: sudo systemctl start docker）"
    return 1
  fi
  title "运行环境"
  printf '  docker        : %s\n' "$("$DOCKER" version --format '{{.Server.Version}}' 2>/dev/null || echo 未知)"
  if "$DOCKER" compose version >/dev/null 2>&1; then
    printf '  compose       : %s\n' "$("$DOCKER" compose version --short 2>/dev/null)"
  else
    c_err "  compose       : 缺失！本项目需要 Compose v2 的 \`docker compose\`"
    c_warn "                  安装：sudo apt update && sudo apt install -y docker-compose-v2"
  fi
  printf '  当前用户       : %s（能直接跑 docker 说明已在 docker 组，否则每条命令都要 sudo）\n' "$(id -un 2>/dev/null || echo 未知)"
  printf '  部署目录       : %s\n' "$ROOT"
  printf '  compose 项目名 : %s\n' "$(project_name)"
  printf '  磁盘可用       : %s\n' "$(df -h "$ROOT" | awk 'NR==2 {print $4}')"

  title "镜像"
  if "$DOCKER" images civic-relay --format '{{.Repository}}:{{.Tag}}  {{.Size}}' | grep -q .; then
    "$DOCKER" images civic-relay --format '  {{.Repository}}:{{.Tag}}  {{.Size}}'
    env_version="$(grep -E '^CIVIC_RELAY_VERSION=' "$ROOT/.env" 2>/dev/null | cut -d= -f2- | tr -d '\r' || true)"
    if [ -n "$env_version" ] && ! "$DOCKER" image inspect "civic-relay:${env_version}" >/dev/null 2>&1; then
      c_warn "  .env 指定的是 ${env_version}，但本地没有这个 tag ——直接 up 会去构建（服务器多半没网）。"
      c_warn "  请先执行：$0 载入镜像 <包内镜像文件>"
    fi
  else
    c_warn "  本地没有任何 civic-relay 镜像。离线安装请先执行：$0 载入镜像 <包内镜像文件>"
  fi

  title "数据卷（决定这是全新安装还是升级）"
  local volumes current_project
  volumes="$(existing_config_volumes)"
  current_project="$(project_name)"
  if [ -z "$volumes" ]; then
    c_warn "  没有找到 civic-relay 配置卷 → 这会是【全新安装】，首次启动会生成新的管理员密钥。"
  else
    printf '  已存在的配置卷：\n'
    printf '%s\n' "$volumes" | sed 's/^/    /'
    if ! printf '%s\n' "$volumes" | grep -qx "${current_project}_civic-relay-config"; then
      c_err "  ⚠️ 当前目录名是「${current_project}」，与上面已有的卷不匹配！"
      c_err "     在错误的目录里 up 会新建一套空卷 → 等于全新安装，旧配置与账目不会丢但会用不上。"
      c_warn "     对策（二选一）：① cd 到原来的部署目录再操作；② 显式指定旧项目名："
      c_warn "       export COMPOSE_PROJECT_NAME=<旧卷名去掉 _civic-relay-config 后缀>"
    else
      c_ok "  与当前目录匹配 → 会在既有配置与账目上做升级。"
      c_warn "  升级前请先备份：$0 备份"
    fi
  fi

  title "端口与防火墙"
  local port="${CIVIC_RELAY_PORT:-8000}"
  if ! command -v ss >/dev/null 2>&1; then
    c_warn "  无 ss 命令，跳过端口占用检测（Ubuntu 上应有 iproute2）"
  elif ss -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]${port}\$"; then
    c_warn "  端口 ${port} 已被占用："
    ss -ltnp 2>/dev/null | grep -E "[:.]${port}\$" | sed 's/^/    /' || true
  else
    c_ok "  端口 ${port} 空闲"
  fi
  if command -v ufw >/dev/null 2>&1; then
    local ufw_state
    ufw_state="$(sudo -n ufw status 2>/dev/null || true)"
    if [ -z "$ufw_state" ]; then
      c_warn "  ufw 状态需要 root，自行确认：sudo ufw status | grep ${port}"
    elif printf '%s' "$ufw_state" | grep -q "Status: active"; then
      if printf '%s' "$ufw_state" | grep -qE "^${port}(/tcp)?\b"; then
        c_ok "  ufw 已放行 ${port}"
      else
        c_err "  ufw 处于 active 但没有放行 ${port}"
        c_warn "  执行：sudo ufw allow ${port}/tcp"
      fi
    else
      c_ok "  ufw 未启用"
    fi
  fi
  c_warn "  云服务器还要在控制台「安全组」里放行 ${port} 入站，脚本查不到。"
  c_warn "  默认绑定 0.0.0.0 是明文 HTTP，仅适合可信内网；对外请设 CIVIC_RELAY_BIND=127.0.0.1 并挂 HTTPS 反代。"
}

cmd_load() {
  require_engine
  local file="${1:-}"
  if [ -z "$file" ]; then
    file="$(ls -1 "$ROOT"/civic-relay-*-image.tar.gz 2>/dev/null | head -n1 || true)"
  fi
  if [ -z "$file" ] || [ ! -f "$file" ]; then
    c_err "用法：$0 载入镜像 <civic-relay-<版本>-image.tar.gz>"
    c_warn "解压发布包后，镜像包就在其根目录；也可以先 cd 进去再执行本脚本。"
    exit 1
  fi
  title "导入镜像 $file"
  # docker load 能直接读 gzip 包，不需要先解压。
  if "$DOCKER" load -i "$file"; then
    c_ok "导入完成，执行 $0 状态 查看本地镜像"
  else
    c_err "导入失败。若报 unexpected EOF，说明文件在传输中被截断，请重新上传并核对 SHA256SUMS.txt。"
    exit 1
  fi
}

cmd_up_no_build() {
  require_engine
  title "启动（使用本地镜像，不构建）"
  (cd "$ROOT" && "${DC[@]}" up -d "$@")
  wait_healthy
  cmd_status
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
    status="$("$DOCKER" inspect "$(container_id)" --format '{{.State.Health.Status}}' 2>/dev/null || true)"
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
  c_ok "已停止。数据仍在卷 $(config_volume) / $(data_volume) 中。"
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
  "$DOCKER" inspect "$(container_id)" \
    --format '健康={{.State.Health.Status}}  用户={{.Config.User}}  只读根={{.HostConfig.ReadonlyRootfs}}  时区={{range .Config.Env}}{{if eq (printf "%.3s" .) "TZ="}}{{.}}{{end}}{{end}}' 2>/dev/null \
    || c_warn "容器未运行"
}

cmd_logs() {
  require_engine
  (cd "$ROOT" && "${DC[@]}" logs --tail="${1:-100}" -f civic-relay)
}

cmd_volumes() {
  require_engine
  local cfg data
  cfg="$(config_volume)"; data="$(data_volume)"
  title "配置卷内容（${cfg}）"
  "$DOCKER" run --rm -v "$cfg":/c alpine sh -c 'ls -la /c'
  title "数据卷内容（${data}）"
  "$DOCKER" run --rm -v "$data":/d alpine sh -c 'ls -la /d'
  c_warn "镜像基于 distroless（无 shell），所以用 alpine 辅助容器查看卷内容。"
}

cmd_admin_key() {
  require_engine
  local cfg
  cfg="$(config_volume)"
  title "管理员密钥"
  "$DOCKER" run --rm -v "$cfg":/c alpine sh -c \
    'cat /c/bootstrap-admin-key.txt 2>/dev/null || { echo "（一次性文件已删除或不曾生成，改从配置里读：）"; grep "^ADMIN_API_KEY=" /c/relay.env; }'
  c_warn "取用后建议执行：$0 删密钥"
}

cmd_rm_key() {
  require_engine
  local cfg
  cfg="$(config_volume)"
  title "删除一次性管理员密钥文件"
  "$DOCKER" run --rm -v "$cfg":/c alpine sh -c \
    'rm -f /c/bootstrap-admin-key.txt && echo 已删除（relay.env 里的 ADMIN_API_KEY 仍是同一把）'
}

cmd_backup() {
  require_engine
  local target="${1:-$ROOT/backups/$(date +%Y%m%d-%H%M%S)}" cid
  cid="$(require_container)"
  mkdir -p "$target"
  title "备份到 $target"
  c_warn "先停容器：WAL 模式下未 checkpoint 的事务还在 -wal 里，热拷贝会丢最新数据。"
  (cd "$ROOT" && "${DC[@]}" stop >/dev/null)
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$cid:/app/config/." "$(native_path "$target/config")"
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$cid:/app/data/." "$(native_path "$target/data")"
  (cd "$ROOT" && "${DC[@]}" start >/dev/null)
  wait_healthy
  title "备份内容"
  ls -la "$target/config" "$target/data"
  c_ok "备份完成：$target"
  c_warn "必须与 relay.env 一起保存：里面的 RELAY_ENCRYPTION_KEY 一旦丢失，库中的上游密钥将无法解密。"
}

cmd_restore() {
  require_engine
  local source="${1:-}" cid
  if [ -z "$source" ] || [ ! -d "$source" ]; then
    c_err "用法：$0 恢复 <备份目录>"
    exit 1
  fi
  cid="$(require_container)"
  c_err "⚠️ 此操作非常危险，可能导致不可逆的数据丢失！"
  c_err "将用 [${source}] 覆盖当前的配置与全部账目，且无法撤销。"
  read -r -p "确认继续请输入 yes：" answer
  if [ "$answer" != "yes" ]; then
    c_warn "已取消。"
    return 0
  fi
  (cd "$ROOT" && "${DC[@]}" stop >/dev/null)
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$(native_path "$source/config/.")" "$cid:/app/config"
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$(native_path "$source/data/.")" "$cid:/app/data"
  (cd "$ROOT" && "${DC[@]}" start >/dev/null)
  wait_healthy
  c_ok "恢复完成。"
}

cmd_upgrade() {
  require_engine
  if [ -n "${1:-}" ]; then
    export CIVIC_RELAY_VERSION="$1"
  fi
  local version="${CIVIC_RELAY_VERSION:-dev}"
  title "升级到版本 ${version}（数据库迁移在启动时自动执行）"
  if "$DOCKER" image inspect "civic-relay:${version}" >/dev/null 2>&1; then
    c_info "本地已有 civic-relay:${version}，直接用它重建容器（不构建）"
    (cd "$ROOT" && "${DC[@]}" up -d --force-recreate)
  else
    c_warn "本地没有 civic-relay:${version}，将现场构建（需要能访问 Docker Hub 与 Go 模块代理）"
    c_warn "离线服务器请先执行：$0 载入镜像 <镜像包>，再重跑本命令"
    (cd "$ROOT" && "${DC[@]}" up -d --build --force-recreate)
  fi
  wait_healthy
  cmd_probe
  c_warn "回滚：把 CIVIC_RELAY_VERSION 改回旧标签重建即可；跨版本回滚前请先备份。"
}

cmd_help() {
  cat <<'EOF'

Civic Relay 中文运维脚本

用法：./scripts/docker-ops.sh <命令> [参数]

  ★ 检查环境        上线前先跑：docker/compose/镜像/数据卷/端口/防火墙一次看完，
                    并告诉你这次是【全新安装】还是【升级】
  ★ 载入镜像 [文件] 导入离线镜像包（civic-relay-<版本>-image.tar.gz，不用解压），
                    默认在当前目录找
  ★ 启动离线        用本地已有镜像启动（不构建），适合离线/无网服务器
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
  CIVIC_RELAY_VERSION    镜像版本号，默认 dev
  CIVIC_RELAY_PORT       宿主机端口，默认 8000
  CIVIC_RELAY_BIND       绑定地址，默认 0.0.0.0（局域网可访问；仅本机设 127.0.0.1）
  COMPOSE_PROJECT_NAME   卷名前缀，默认取当前目录名。**升级时若目录名变了必须显式指定旧项目名**，
                         否则会新建一套空卷（等于全新安装）
  TZ                     容器日志时区，默认 Asia/Shanghai

EOF
}

case "${1:-帮助}" in
  检查环境 | doctor)      cmd_doctor ;;
  载入镜像 | load)        shift; cmd_load "$@" ;;
  启动离线 | up-offline)  shift; cmd_up_no_build "$@" ;;
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
