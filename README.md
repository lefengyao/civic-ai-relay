# Civic Relay

Civic Relay 是一个单进程 Go 服务，将多个 OpenAI 兼容上游统一转发给局域网客户端。它支持供应商、模型、模型组、随机客户端 Key、并发限制、Token/金额配额、SSE 流式响应和中文管理端。

## 当前能力

- `GET /v1/models`：只返回当前客户端 Key 被授权的启用且已定价模型。
- `POST /v1/chat/completions`：支持普通 JSON 和流式 SSE。
- 供应商 API Key 使用 AES-256-GCM 加密保存，管理接口只返回是否已配置。
- 客户端 Key 使用 `crk_` 前缀，数据库只保存 HMAC 摘要，明文只在创建时返回一次。
- 模型组与客户端 Key 支持多组关联，授权模型取去重并集。
- 全局 RPM、滚动五小时 Token、北京时间自然日 Token、Key 并发和 Key 总 Token/金额配额均在 SQLite 事务中控制。
- 达到客户端 Key 任一总配额后自动停用，并记录 `quota_exhausted`。
- 默认进程 RSS 软限制为 200 MB；Linux、Windows 均提供读取实现。
- 请求记录只保存元数据，不保存 prompt、回复内容、Authorization 或上游 Key。

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

## Docker Compose

配置和数据库目录必须位于仓库外：

```powershell
$env:CIVIC_RELAY_CONFIG_DIR = "C:\ProgramData\CivicRelay"
$env:CIVIC_RELAY_DATA_DIR = "C:\ProgramData\CivicRelay\data"
New-Item -ItemType Directory -Force $env:CIVIC_RELAY_CONFIG_DIR,$env:CIVIC_RELAY_DATA_DIR | Out-Null
docker compose up -d --build
```

Compose 默认只绑定 `127.0.0.1:8000`。生产环境应通过 HTTPS 反向代理对外提供访问，不要直接暴露 HTTP 端口。

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
