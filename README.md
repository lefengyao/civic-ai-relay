# Civic Relay

Civic Relay 是一个单进程 Go 服务，将多个 OpenAI 兼容上游统一转发给局域网客户端。它支持供应商、模型、模型组、随机客户端 Key、并发限制、Token/金额配额、SSE 流式响应和中文管理端。

## 当前能力

### 转发与计费

- `GET /v1/models`：只返回当前客户端 Key 被授权的启用且已定价模型。
- `POST /v1/chat/completions`：支持普通 JSON 和流式 SSE。
- `POST /v1/responses`：OpenAI Responses API 兼容层——请求转换为 chat/completions 转发上游，支持 `instructions`、字符串或数组 `input`、`max_output_tokens`、流式（标准 response.created/output_text.delta/response.completed 事件序列）；计费与 chat/completions 同口径。暂不支持工具调用。
- 每个模型独立计价（输入/输出/缓存输入分别计价，单位：元/百万 token）。
- 缓存输入计费：解析上游 `usage.prompt_tokens_details.cached_tokens`，命中部分按「缓存输入单价」计费；未配置时回退输入价，填 0 表示缓存免费。非流式与流式共用同一计价实现（`relay.PriceUsage`）。
- 每个模型组独立倍率（`rate_milli` 千分比，1000 = 1.0×）；请求时取 Key 所属启用分组中包含该模型所属**渠道**的最大倍率，模型原始单价存库不改写，只在预留与结算时折算（缓存单价同样受倍率约束）。

### 安全

- 供应商 API Key 使用 AES-256-GCM 加密保存，管理接口只返回是否已配置。
- 客户端 Key 使用 `crk_` 前缀，数据库只保存 HMAC 摘要，明文只在创建/重置时返回一次。
- 密钥任何字符不会出现在列表接口：脱敏缩略（`crk_xxxx…`）与非敏感标识（`key-xxxxxx`）通过独立接口 `/admin/api/keys/hints` 提供（admin_test 强制）。
- 请求记录只保存元数据，不保存 prompt、回复内容、Authorization 或上游 Key。

### 配额与限制

- 模型组**管理渠道**：组内挂供应商（渠道），授权模型 = 组内渠道下所有已定价启用模型（去重并集）；「查看 / 编辑」弹窗内展示每个成员渠道的模型与输入/输出/缓存单价。
- 全局 RPM、滚动五小时 Token、北京时间自然日 Token、Key 并发和 Key 总 Token/金额配额均在 SQLite 事务中控制。
- 达到客户端 Key 任一总配额后自动停用，并记录 `quota_exhausted`。
- 运维加固：启动时及每 5 分钟自动回收崩溃残留的预留（防止并发槽位与 5h 配额永久泄漏）；每日按 `RETENTION_DAYS` 自动清理过期账目；HTTP 服务带读超时（Slowloris 防护）与空闲连接回收；SIGINT/SIGTERM 优雅停机，先排空在途请求（含 SSE 流）再落盘退出；RSS 超过 `MEMORY_LIMIT_MB` 时拒绝新公共请求并中止进行中的流；提供免认证的 `GET /healthz`（进程存活，不触达数据库）与 `GET /readyz`（数据库就绪）探针，可直接用于容器与反向代理健康检查；启动日志打印构建版本（`civic-relay <version> (commit <hash>, built <date>)`），便于确认线上运行的镜像版本。
- 管理端「运行配置」保存后原子写回 relay.env 并**热应用**（配额、输出上限、并发闸门、内存保护即时生效）；仅 HOST/PORT/DB_PATH/DOCS_ENABLED 需要重启。渠道密钥轮换或地址修改后上游连接缓存自动驱逐。
- 管理端 Key 列表实时展示已用 Token / 已用金额 / 请求次数（口径与限额检查一致，含未结算预留）。
- 默认进程 RSS 软限制为 200 MB；Linux、Windows 均提供读取实现。

### 管理（中文 Web 控制台，`/admin/`）

- 供应商（渠道）：增删改查；「模型管理」弹窗统一完成四件事——① 拉取上游 `/v1/models` 勾选导入（可逐个自定义对外名称作为映射，幂等）② 手动添加模型（自由命名 + 映射 + 可选定价）③ 已接入模型列表（含映射与计价）④ 行内编辑（名称/映射/计价/启用）与删除。导入为停用未定价。模型无独立页面，全部通过渠道查看与维护。
- 模型组：增删改查、倍率设置、组内渠道增删（拒绝停用渠道）；每组独立「概览」弹窗——RPM、5 小时 Token、当日 Token/金额、进行中请求、近一小时成功率，仅统计走组内渠道的请求，口径与全局概览一致（`GET /admin/api/groups/{id}/overview`）。
- 客户端 Key：创建（明文只返回一次）、重置密钥（旧密钥立即失效）、分组调整、删除（立即吊销）。
- 四类资源均支持删除：删除供应商级联删除其模型；历史账单金额一律保留；重复删除返回 404。
- 根路径 `/` 302 跳转到 `/admin/`。

## 本地启动

需要 Go 1.27 或更新版本。配置文件不放在仓库内：

```powershell
$env:CIVIC_RELAY_CONFIG_FILE = "C:\ProgramData\CivicRelay\relay.env"
go run ./cmd/civic-relay
```

首次启动会创建 `relay.env` 和一次性 `bootstrap-admin-key.txt`。读取管理员 Key 后，请妥善保存并删除 bootstrap 文件。管理端地址：

```text
http://127.0.0.1:8000/admin
```

局域网测试时将 `HOST` 设置为 `0.0.0.0`，然后用服务器局域网 IP 访问。先在管理端添加供应商、模型和模型组，再创建客户端 Key。

客户端使用：

```bash
curl http://<relay-ip>:8000/v1/chat/completions \
  -H "Authorization: Bearer crk_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"provider/model","messages":[{"role":"user","content":"Hello"}]}'
```

客户端 Base URL 填 `http://<relay-ip>:8000/v1`（末尾不要多带或漏掉 `/v1`）。服务端对常见路径变体做了容错：`/v1/v1/...`（多一层前缀）、`/chat/completions`（少前缀）、尾斜杠均自动归一处理；其余未知端点返回 JSON 404 并提示支持的路径。若对话 404 但能拉取模型，多半是客户端把模型名填错成带空格的值或使用了不支持的端点（如 `/v1/responses`、`/v1/embeddings`，本服务未实现）。

## Docker 运行

镜像多阶段构建，基于 distroless，以非 root（UID `65532`）运行，根文件系统只读，内置健康检查。配置与数据库放在卷里，仓库目录不参与运行。

### 方式一：命名卷（默认，跨平台零准备）

```bash
docker compose up -d --build
docker compose ps          # 等待 STATUS 变为 healthy
docker compose logs -f
```

首次启动会在 `civic-relay-config` 卷内生成 `relay.env` 与一次性的 `bootstrap-admin-key.txt`：

```bash
docker compose exec civic-relay cat /app/config/bootstrap-admin-key.txt
docker compose exec civic-relay rm /app/config/bootstrap-admin-key.txt
```

管理端地址 `http://127.0.0.1:8000/admin`，局域网内其他设备用 `http://<本机IP>:8000/admin`。端口默认绑定所有网卡（等价 `8000:8000`）；只允许本机访问时设 `CIVIC_RELAY_BIND=127.0.0.1`。注意这是明文 HTTP，管理台与客户端 Key 都会明文传输，只在可信内网使用，对外提供访问必须经 HTTPS 反向代理。

### 中文运维脚本（推荐日常使用）

不用记 docker 命令，脚本会自动定位 Docker Desktop 的 `docker.exe`：

```bash
./scripts/docker-ops.sh 帮助        # 全部命令
./scripts/docker-ops.sh 启动        # 构建并启动，等它变 healthy
./scripts/docker-ops.sh 探针        # /healthz、/readyz、容器健康一眼看完
./scripts/docker-ops.sh 备份        # 停容器后完整备份配置与数据库
./scripts/docker-ops.sh 看卷        # distroless 无 shell，用辅助容器查看卷内容
```

### 方式二：宿主机目录（需要直接备份数据库或审计文件时）

```bash
export CIVIC_RELAY_CONFIG_DIR=/srv/civic-relay/config
export CIVIC_RELAY_DATA_DIR=/srv/civic-relay/data
sudo mkdir -p "$CIVIC_RELAY_CONFIG_DIR" "$CIVIC_RELAY_DATA_DIR"
sudo chown -R 65532:65532 "$CIVIC_RELAY_CONFIG_DIR" "$CIVIC_RELAY_DATA_DIR"
docker compose -f docker-compose.yml -f docker-compose.bind.yml up -d --build
```

容器以 UID `65532` 运行，宿主机目录未 `chown` 会因无写权限启动失败（Windows + Docker Desktop 绑定 Windows 目录不需要，Desktop 会处理属主）。

### 可调参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `CIVIC_RELAY_BIND` | `0.0.0.0` | 宿主机绑定地址；设 `127.0.0.1` 可退回到仅本机可访问 |
| `CIVIC_RELAY_PORT` | `8000` | 宿主机端口 |
| `CIVIC_RELAY_VERSION` | `dev` | 镜像 tag，同时注入 `/healthz` 与启动日志 |
| `TZ` | `Asia/Shanghai` | 容器日志时区（镜像已内置该时区数据）；只影响日志时间戳，配额窗口与账目一律用 UTC |
| `CIVIC_RELAY_CONFIG_DIR` / `CIVIC_RELAY_DATA_DIR` | 无 | 仅绑定卷覆盖文件需要 |

### 健康检查与资源上限

- `GET /healthz`：进程存活探针，刻意不触达数据库（返回 `{"status":"ok","version":"..."}`），避免瞬时数据库锁竞争触发容器重启。
- `GET /readyz`：依赖探针，确认加密数据库仍能应答；不可用时返回 503 `database_unavailable`，编排层可据此摘流量。
- 容器 `HEALTHCHECK` 复用同一个二进制（`civic-relay -healthcheck`），它按 `relay.env` 解析端口，因此改端口后探针会跟着变，不会让容器永久处于 unhealthy。
- `mem_limit` 默认 `256m`，略高于 `MEMORY_LIMIT_MB`（默认 200MB）。这样应用自身的 RSS 软保护会先返回 503，而不是被 cgroup OOM 直接杀进程。调整 `MEMORY_LIMIT_MB` 时请同步调整 `mem_limit`，建议留 25% 余量。
- 停止容器时先发 SIGTERM，进程会排空在途请求（含 SSE 流，最长 15 秒）后落盘退出，`stop_grace_period` 已设 30 秒。

完整的容器运行手册（备份恢复、版本升级、排障）见 [docs/docker-deploy.md](docs/docker-deploy.md)。

## Linux systemd

编译并安装二进制到 `/opt/civic-ai-relay/civic-relay`，创建 `civic-relay` 用户和 `/etc/civic-relay`、`/var/lib/civic-relay` 目录，然后安装：

```bash
sudo cp civic-relay-go.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now civic-relay-go
```

服务单元启用 `MemoryMax=200M`、`ProtectSystem=strict`、`ProtectHome=true` 和 `NoNewPrivileges=true`。

## 安全说明

- 不要把 `relay.env`、`bootstrap-admin-key.txt`、数据库文件或真实上游 Key 提交到 Git。
- 管理员 Key 与客户端 Key 不同；管理员接口使用 `X-Admin-Key`，公共接口使用 `Authorization: Bearer`。
- HTTPS 终止应放在反向代理或 VPS 面板；代理到本机 Go 服务的 HTTP 只适用于受信任的本机链路。
- 修改 `RELAY_ENCRYPTION_KEY` 会导致已有供应商 Key 无法解密；数据库与该密钥必须一起备份。

## 测试

```powershell
go test -count=1 ./...
go vet ./...
```

当前 Windows 环境没有 CGO/GCC，`go test -race ./...` 需要在具备 GCC 的环境执行。

## 更多文档

- [docs/admin-api.md](docs/admin-api.md) — 完整的管理端与公共 API 参考（路由、请求/响应字段、语义约定）。
- [docs/breakpoint-2026-09-10.md](docs/breakpoint-2026-09-10.md) — 渠道多模型 + 分组倍率功能说明与端到端验证记录。
- [docs/superpowers/](docs/superpowers/) — 历史设计规格与实施计划存档。
