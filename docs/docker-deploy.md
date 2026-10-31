# Civic Relay 容器运行手册

面向运维的容器部署、备份、升级与排障说明。快速上手见 [README](../README.md) 的「Docker 运行」一节。

## 1. 镜像构成

| 项目 | 值 |
|---|---|
| 构建阶段 | `golang:1.27-bookworm`，`CGO_ENABLED=0` 静态编译 |
| 运行阶段 | `gcr.io/distroless/static-debian12:nonroot`（无 shell、无包管理器） |
| 运行用户 | UID/GID `65532`（非 root） |
| 工作目录 | `/app`（**关键**：引导配置里的 `DB_PATH=data/relay.db` 相对此目录解析） |
| 监听端口 | `8000`（容器内） |
| 可写路径 | 仅 `/app/config`、`/app/data` 两个卷，以及 tmpfs `/tmp` |
| 时区 | `TZ=Asia/Shanghai`（镜像内置该时区数据）。只影响日志时间戳；配额窗口与账目全部显式使用 UTC，换时区不改变计费口径 |
| 健康检查 | `["/civic-relay", "-healthcheck"]` |
| 内置证书 | 复制构建阶段的 CA bundle，供上游 HTTPS 调用使用 |

构建参数（`docker build --build-arg`）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `VERSION` | `dev` | 写入 `/healthz` 与启动日志 |
| `COMMIT` | `unknown` | OCI 标签 |
| `BUILD_DATE` | `unknown` | OCI 标签 |
| `TARGETOS` / `TARGETARCH` | `linux` / `amd64` | 用 `buildx --platform` 时自动覆盖 |
| `RUNTIME_IMAGE` | `gcr.io/distroless/static-debian12:nonroot` | 无法拉取 gcr.io 时可用 `--build-arg RUNTIME_IMAGE=scratch`（二进制纯静态，已显式复制 CA 证书） |

构建缓存：Dockerfile 使用 BuildKit 的 `--mount=type=cache` 缓存 `go mod` 与编译产物，第二次构建通常只需数秒。

## 2. 首次部署

```bash
docker compose up -d --build
docker compose ps                       # 等待 STATUS 变为 healthy（start_period 5s）
docker compose exec civic-relay cat /app/config/bootstrap-admin-key.txt
docker compose exec civic-relay rm /app/config/bootstrap-admin-key.txt
```

拿到管理员密钥后访问 `http://127.0.0.1:8000/admin`，依次添加供应商（渠道）→ 拉取/添加模型并定价 → 建模型组挂渠道 → 创建客户端 Key。

端口默认绑定所有网卡（`0.0.0.0:8000`，等价 `8000:8000`），局域网内其他设备用 `http://<本机IP>:8000/admin` 访问；只允许本机访问时设 `CIVIC_RELAY_BIND=127.0.0.1`。

**Windows 上还要放行防火墙**：本机三个防火墙配置文件默认启用、且没有任何 docker 相关规则时，宿主机端口代理的入站连接会被丢弃（表现为其他设备连接超时）。需要以管理员身份加一条入站规则：

```powershell
New-NetFirewallRule -DisplayName "Civic Relay 8000" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8000
```

（等价的 netsh：`netsh advfirewall firewall add rule name="Civic Relay 8000" dir=in action=allow protocol=TCP localport=8000`）

## 3. 数据、密钥与备份

卷内容：

| 卷 | 挂载点 | 内容 |
|---|---|---|
| `civic-relay-config` | `/app/config` | `relay.env`（含 `RELAY_ENCRYPTION_KEY`）、`bootstrap-admin-key.txt` |
| `civic-relay-data` | `/app/data` | `relay.db`、`relay.db-wal`、`relay.db-shm` |

**两个东西必须成对备份**：

1. `relay.db` —— 账目、Key 摘要、加密后的上游密钥；
2. `relay.env` 里的 `RELAY_ENCRYPTION_KEY` —— 数据库里的上游 Key 用它加密。换了密钥，历史渠道密钥全部无法解密。

**不要用 `cp` 直接拷 SQLite 文件**：WAL 模式下未 checkpoint 的事务还在 `relay.db-wal` 里，单独复制 `relay.db` 会丢最新数据。正确做法是先停容器再拷，或复制全部三个文件：

```bash
# 推荐：停容器后整目录导出（WAL 已随关闭合并）
docker compose stop
docker compose cp civic-relay:/app/data ./backup-data        # 容器已停，改用卷导出见下
docker compose start

# 或者直接从卷导出（不停容器也可以，但三个文件必须一起拷）
docker run --rm -v civic-relay-data:/data -v "$PWD":/backup alpine \
  tar czf /backup/relay-data-$(date +%F).tar.gz -C /data .
```

恢复时把 `relay.db*` 放回 `/app/data`、`relay.env` 放回 `/app/config`，注意属主仍要是 `65532`。

## 4. 版本升级

```bash
git pull
CIVIC_RELAY_VERSION=v0.2.0 docker compose up -d --build
docker compose exec civic-relay wget -qO- http://127.0.0.1:8000/healthz   # 确认版本号
```

数据库迁移在启动时自动执行（`schema_migrations` 表记录版本），无需手工操作。回滚：把 `CIVIC_RELAY_VERSION` 改回旧标签重建即可；注意旧版本不认识新迁移引入的列，跨版本回滚前先备份。

## 5. 在命名卷与宿主机目录之间迁移

```bash
# 命名卷 → 宿主机目录
docker compose stop
docker run --rm -v civic-relay-config:/from -v /srv/civic-relay/config:/to alpine cp -a /from/. /to/
docker run --rm -v civic-relay-data:/from   -v /srv/civic-relay/data:/to   alpine cp -a /from/. /to/
sudo chown -R 65532:65532 /srv/civic-relay/config /srv/civic-relay/data
export CIVIC_RELAY_CONFIG_DIR=/srv/civic-relay/config CIVIC_RELAY_DATA_DIR=/srv/civic-relay/data
docker compose -f docker-compose.yml -f docker-compose.bind.yml up -d --build
```

反过来把两个 `cp -a` 的源与目标对调即可。

## 6. 排障

| 现象 | 原因与处理 |
|---|---|
| 容器反复重启，日志报 `mkdir ... permission denied` 或写 `relay.env` 失败 | 绑定卷宿主机目录属主不对：`sudo chown -R 65532:65532 <目录>` |
| `STATUS` 长期 `unhealthy` | 进容器看日志；`civic-relay -healthcheck` 按 `relay.env` 里的 HOST/PORT 探测，若 HOST 被改成具体地址而非 `0.0.0.0` 且容器外不可达，探针会失败。容器内应保持 `HOST=0.0.0.0` |
| 容器被 OOM 杀掉（不是 503） | `mem_limit` 低于实际占用。保持 `mem_limit` 高于 `MEMORY_LIMIT_MB` 约 25% |
| 数据库文件重启后消失、账目清零 | 说明数据库落在了容器临时层。确认镜像内 `WORKDIR /app` 未被覆盖，且 `/app/data` 确实挂了卷（`docker compose config` 复核） |
| 端口被占用 | 改 `CIVIC_RELAY_PORT`（宿主机侧端口）即可，容器内始终是 8000 |
| 上游 HTTPS 报证书错误 | 自定义上游走了私有 CA。构建时替换 CA bundle，或改用带完整证书链的上游 |

常用命令：

```bash
docker compose ps                                  # 状态与健康
docker compose logs --tail=200 -f civic-relay      # 日志
docker compose exec civic-relay ls -la /app/data   # 卷内容
docker inspect --format '{{json .State.Health}}' civic-relay
docker stats --no-stream civic-relay               # 内存占用
```

## 8. 中文运维脚本

`scripts/docker-ops.sh` 把常用操作包成中文命令，免去记 docker 参数；Windows 上会自动定位
Docker Desktop 的 `docker.exe`（Git Bash 下直接调用无扩展名的 `docker` 会失败）。

| 命令 | 作用 |
|---|---|
| `启动` / `停止` / `重启` | 构建启动、停止（保留卷）、重启，启动后自动等 healthy |
| `状态` / `探针` | 容器与镜像状态；`/healthz`、`/readyz`、运行用户、只读根、时区 |
| `日志 [行数]` | 跟踪日志 |
| `看卷` | 用 alpine 辅助容器列出两个卷的内容（distroless 无 shell） |
| `管理员密钥` / `删密钥` | 取用并删除一次性管理员密钥文件 |
| `备份 [目录]` | 停容器 → 完整导出 `relay.env` 与 `relay.db*` → 重启 |
| `恢复 <目录>` | 用备份覆盖当前数据，需输入 `yes` 二次确认 |
| `升级 [版本]` | 重建镜像并升级，自动等 healthy 并复查探针 |

可用环境变量：`CIVIC_RELAY_VERSION`、`CIVIC_RELAY_PORT`、`CIVIC_RELAY_BIND`、`TZ`。

## 9. 安全基线

镜像与 compose 已默认启用：

- 非 root 运行（UID 65532），`cap_drop: ALL`，`no-new-privileges`；
- `read_only: true` 根文件系统，仅两个卷与 `/tmp` tmpfs 可写；
- 端口默认绑定 `0.0.0.0:8000`（等价 `8000:8000`），便于局域网访问；只在可信内网这样用，对外必须经 HTTPS 反向代理。要退回仅本机可访问：`CIVIC_RELAY_BIND=127.0.0.1`；
- 局域网可达还需要 Windows 防火墙放行入站 8000（见第 2 节）；
- 容器内存与进程数上限（`mem_limit: 256m`、`pids_limit: 256`）；
- json-file 日志轮转（`max-size=10m`、`max-file=3`）。

仍需运维侧保证：`relay.env` 与数据库卷的访问权限、`bootstrap-admin-key.txt` 读后即删、反向代理上的 TLS。
