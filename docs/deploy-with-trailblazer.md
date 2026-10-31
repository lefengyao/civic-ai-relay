# 用 Trailblazer Deploy 部署 / 更新 Civic Relay

部署管理器（trailblazer-deploy）通过手动添加的 **Civic Relay** ZIP 插件接入站点（`civic-relay`），新安装包不内置该站点。
站点源契约和专属功能面板由本仓库的 `deploy/` 目录拥有，解压与安装步骤见 [插件说明](../deploy/README.md)。Deploy 的「管理站点插件」支持导入预览、搜索、查看、导出和模板新增。
本文说明从打包到部署、更新的完整流程，以及**必须手动做**的 Nginx（宝塔）配置——
按该工具的约定（AGENTS.md），部署工具**不管理 Nginx**，反代设置要自己改。

## 一、它做了什么 / 不做什么

| 工具负责 | 工具不负责 |
|---|---|
| 上传 zip、在服务器上 `docker build -f Dockerfile.relay` | Nginx / TLS / 域名配置 |
| 首次安装：`docker run`（挂载配置与数据卷、发布 8000 端口） | 数据备份（虽然更新不会动卷，仍建议定期备份） |
| 更新：停旧容器 → 备份改名 → 起新容器 → 健康检查 → 失败自动回滚 | 首次接管前旧 compose 容器的下线（见下） |
| 只删除带 `civic-relay-manager=true` 标签的自己构建的镜像 | 公网防火墙 / 安全组 |

## 二、打包

```bash
cd civic-ai-relay
python scripts/package_site_zip.py
# → dist/civic-relay-site-<时间戳>.zip（约 170 KiB，只含构建所需文件）
```

## 三、首次接管（服务器上已有 compose 部署时）

数据库与配置存在**命名卷** `civic-relay_civic-relay-config` / `-data` 里，
部署工具的档案复用同样的卷名——接管后**配置与账目原样保留**。

1. 先在原 compose 目录下线容器（**不加 `-v`**，卷会保留）：

   ```bash
   cd /原来的civic部署目录
   docker compose down
   docker volume ls | grep civic-relay   # 确认两个卷还在
   ```

   > 如果你的卷前缀不是 `civic-relay_`（compose 项目目录名不同），
   > Docker 没有 `volume rename` 命令；请先备份，并明确把数据复制到契约声明的命名卷，
   > 或创建使用真实卷名的独立站点插件后再接管。不要直接改用空卷。

2. 备份一次（习惯而已，但值得）：

   ```bash
   docker run --rm -v civic-relay_civic-relay-config:/c -v "$PWD":/b alpine:3.20 \
     tar czf /b/relay-config-backup.tgz -C /c .
   ```

3. 部署管理器 → 站点 **Civic Relay** → 选择服务器 → 部署 → 选上面的 zip。
   工具会构建镜像、挂载两个卷、启动容器并等 `/healthz` 通过。

4. 拿管理员密钥（首次引导生成）：

   ```bash
   docker run --rm -v civic-relay_civic-relay-config:/c alpine:3.20 \
     cat /c/bootstrap-admin-key.txt
   ```

   > 卷是复用的旧卷时不会有新的 bootstrap 文件——管理员密钥还是原来那个。

## 四、更新

```bash
python scripts/package_site_zip.py
```

部署管理器里对同一服务器再点一次「部署」选新 zip 即可。流程是
停旧 → 备份改名 → 起新 → 健康检查，任何一步失败都会自动恢复旧容器；
两个数据卷全程不动。

## 五、Nginx（宝塔）——必须手动配置

Civic Relay 的 `/v1/chat/completions` 默认是 **SSE 流式**响应。Nginx 默认
`proxy_buffering on` 会把流攒着不发（表现为「发出去没反应、最后一次性全出来」），
默认 60 秒 `proxy_read_timeout` 会在上游持续静默时断开连接（表现为「流到一半断掉」）。
relay 的 SSE 注释心跳会在等待上游时保持代理连接；心跳间隔必须短于代理空闲超时。

在宝塔 → 站点 → 配置文件，找到反代 relay 的 `location` 块，改成：

```nginx
location /v1/ {
    proxy_pass http://127.0.0.1:8000;
    proxy_http_version 1.1;          # SSE 需要 1.1
    proxy_buffering off;             # 不关会把流攒到结束才发
    proxy_cache off;
    proxy_read_timeout 90s;          # 大于 relay 的 SSE 心跳间隔（默认 15s）
    proxy_send_timeout 3600s;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size 16m;        # 默认 1m，长上下文请求会 413
}
```

改完重载（宝塔「重载配置」或 `nginx -s reload`），并用这条命令验证流是逐条到达的：

```bash
curl -N -s https://<域名>/v1/chat/completions \
  -H "Authorization: Bearer <客户端密钥>" -H "Content-Type: application/json" \
  -d '{"model":"<模型名>","messages":[{"role":"user","content":"从1数到30"}],"stream":true}' \
  | while IFS= read -r line; do echo "$(date +%T) $line"; done
```

等待输出时应看到 `: relay-heartbeat`，之后逐条收到业务帧；最后一口气全蹦出来说明仍被缓冲。
Chat 流必须收到 `data: [DONE]` 才算正常结束；提前 EOF 是中断。Responses 流应检查
`response.completed` / `response.incomplete` / `response.failed`，不能只看最初的 HTTP 200。
若把心跳设为 `0`，需把 `proxy_read_timeout` 调到大于上游可能持续静默的时间。

> 管理台页面（`/admin`）是普通静态页面，不挑这些参数；
> 上面这段只服务 `/v1/`。管理台反代用普通配置即可。

## 六、日常验证

```bash
curl -s http://127.0.0.1:8000/healthz   # {"status":"ok","version":"..."}
curl -s http://127.0.0.1:8000/readyz    # 数据库就绪
docker logs --tail=50 civic-relay
```

客户端怎么接入（Base URL、密钥、短时令牌、应用标识）见
[client-integration.md](client-integration.md)。
