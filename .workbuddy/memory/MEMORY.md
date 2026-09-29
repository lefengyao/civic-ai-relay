# Civic Relay 项目约定

## 配置与路径
- relay.env 是唯一配置源（路径由 `CIVIC_RELAY_CONFIG_FILE` 指定，容器内 `/app/config/relay.env`）；其他环境变量（含 PORT/DB_PATH）对服务无效，别用 compose environment 覆盖。
- 引导写入的 `DB_PATH=data/relay.db` 是相对工作目录的路径 → 镜像必须保持 `WORKDIR /app`、数据卷挂 `/app/data`，否则库落在容器临时层，重建即丢数据（本项目踩过的真实缺陷）。
- 管理端保存配置会原子写回 relay.env 并热应用；仅 `HOST`/`PORT`/`DB_PATH` 需重启。
- 配置与数据库必须一起备份，尤其 `RELAY_ENCRYPTION_KEY`（丢了则库里的上游 Key 无法解密）。SQLite 在 WAL 模式下要连 `-wal`/`-shm` 一起拷，或先停容器。

## 容器运行
- 端口只改宿主机侧：`CIVIC_RELAY_BIND`（默认 `0.0.0.0`）、`CIVIC_RELAY_PORT`（默认 `8000`）。容器内固定 8000，映射写死了容器侧端口，改 relay.env 的 `PORT` 对外就连不上。
- Windows 上局域网可达还需放行防火墙入站 8000：`New-NetFirewallRule -DisplayName "Civic Relay 8000" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8000`。判据：本机 `127.0.0.1` 通、本机内网 IP 不通 = 防火墙问题，不是映射没生效。
- 镜像非 root（UID/GID `65532`）、根文件系统只读，仅 `/app/config`、`/app/data` 与 tmpfs `/tmp` 可写；Linux 宿主目录绑定卷必须 `chown -R 65532:65532`。
- distroless 无 shell：查卷用 `docker run --rm -v <卷>:/c alpine sh -c …`；自检用 `docker exec <容器> /civic-relay -healthcheck`。
- 探针语义：`/healthz` 存活（不触达数据库，避免瞬时故障引发重启循环）；`/readyz` 就绪（SQLite 不可用 503）。两者免认证、仅 GET/HEAD。
- 本机环境：`docker desktop start` 放后台**能起来**（2026-09-24 实测约 1 分钟引擎就绪，`docker.exe version` 返回 29.7.2），但它自身不快速返回，必须 `run_in_background`；真起不来时才请用户手动开。docker 命令一律显式调 `…\DockerDesktop\resources\bin\docker.exe` 且走 Bash；PowerShell 工具输出不回传，需落盘再 Read。Bash 工具 PATH 缺 coreutils（`ls`/`head` 报 command not found），命令开头补 `export PATH="/c/Users/15610/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:/c/Users/15610/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:$PATH"`。
- 本机 `go build/vet/test` 要 `GOPROXY=off`（依赖已在模块缓存，官方 proxy 握手超时）且**后台跑**：首次约 25 秒，放前台会撞 120 秒超时被 SIGTERM 且输出为空。

## 构建
- Go 模块代理走 `goproxy.cn` + `GOSUMDB=sum.golang.google.cn`（Dockerfile 里是 ARG；官方 proxy 在本机握手超时）。
- 不要写 `# syntax=docker/dockerfile:N`：会强制拉取外部前端镜像，本机第一步就失败。
- Dockerfile 中 `COPY a b c ./` 会把多个目录的内容拍平，必须逐个写目标：`COPY cmd/ ./cmd/`。

## 业务语义（易踩）
- 额度 6 维：`5h / 当日 / 本周` × `Token / 金额`，任一为 0 或留空 = 不限，出厂默认全 0。判定口径是「先欠费后停服」：只看窗口是否已用满（`used >= limit`），不要求本次预留塞得下；拒绝一律 429，code ∈ `token_quota_exceeded` / `amount_quota_exceeded` / `key_token_quota_exceeded` / `key_amount_quota_exceeded`（消息带窗口与数字）。额度用满**不会**自动停用 Key，停用只能手动。
- 分组**管理渠道**而非模型：授权模型 = Key 启用分组内各渠道下「已启用且已定价」模型的去重并集；倍率取包含该模型所属渠道的最大 `rate_milli`（1000 = 1.0×）。
- `ReplaceGroupProviders` 拒绝停用渠道 → 「组内存在停用渠道」只能先入组、再停用渠道来构造。
- 路由 `groups/health` 与 `groups/monitor` 必须排在 `HasPrefix("groups/")` 之前，否则被 `groupItem` 的 ParseInt 判成 `invalid_id`。
- 管理端建资源统一返回 201 且为 `{"data": <store 结构体>}`，字段是 Go 字段名 PascalCase（结构体无 json tag）；Key 创建的明文密钥在顶层 `token`。
- 渠道 `base_url` 校验：必须带 scheme（`127.0.0.1:6000` 这种裸写法会被拒）；只允许 https，或 localhost/`*.localhost`/`*.test`/回环/私有 IP 的 http。存库会剥掉尾部 `/` 和 `/v1`。容器连宿主机只能用网桥网关 IP（bridge `172.17.0.1`，compose 自建网络一般 `172.18.0.1`）或宿主机内网 IP——`host.docker.internal` 与同 compose 的服务名/容器名都不在白名单，且上游必须绑 `0.0.0.0`。
- 已删除的配置项：`UPSTREAM_BASE_URL` / `UPSTREAM_API_KEY` / `DOCS_ENABLED`（旧 relay.env 里的残留行按未知键保留、不再读取回写）。
- `LOG_LEVEL` 到 `DEBUG` 才输出逐请求访问日志；其 `statusRecorder` 必须实现 `http.Flusher`，否则破坏 SSE 流式。
- `MODEL_AUTO_SYNC` 默认 false；同步导入的模型一律**停用 + 未定价**（自动同步不绕过人工定价）。
- 查「某个配置改了没反应」先跑 `_audit_config.py`（统计 Settings 各字段在仓库里的出现次数）。
- **「单价填 0 就不会被额度拦住」是误区**：额度是 `5h/当日/本周` × **Token** 与 **金额** 两组，**Token 那三组与单价完全无关**（按实际 token 数计）。单价 0 只让金额恒为 0、`AMOUNT_LIMIT_*` 与 Key 金额限额永不触发；`TOKEN_LIMIT_*`、`RPM_LIMIT`、全局/Key 并发、`MEMORY_LIMIT_MB` 全都照常生效。旧版本引导模板写过 `TOKEN_LIMIT_5H=20000000` / `TOKEN_LIMIT_DAILY=5000000`，从旧部署升上来的实例会带着它们，容易误以为是"不限"。
- 排查「对话中断」先分清两类：**新请求被拒**（429/503，看 `/admin/api/overview` 与错误码）与**进行中的流被掐断**（前半段有、后半段没了 —— 依次怀疑反向代理超时、`MEMORY_LIMIT_MB` 中止流、`MAX_STREAM_DURATION`/`UPSTREAM_READ_TIMEOUT`、上游中断）。
- 管理端额度过载先看 `/admin/api/overview`；判定口径验证法：把窗口限额压到已用之下 → 应得 429 + 对应 code → 改回 0 → 应立即恢复 200（此阶段被拒的请求不打上游、不产生费用）。
- **客户端接入信息**（v0.3.4 新增，面向「让其他软件接进来」）：`GET /admin/api/keys/{id}/connection` 一次给出 Base URL（按访问管理台的方式推断，尊重 `X-Forwarded-Proto`，回环地址会被前端提示换成局域网 IP）、派生地址、脱敏 token 标识、可用模型清单、分组与 `issues`/`warnings`/`ready`。**模型清单与公共 `/v1/models` 共用 `store.authorizedModelsSelect`**，两边不会漂移；明文 token 无法还原（库里只有 HMAC 摘要），只能重置。管理台 Key 行有「接入信息」按钮。路由在 `keyItem` 内分发（与 `groups`/`rotate` 同级）。
- **客户端接入套流程**：渠道（上游）→ 定价并启用模型 → 分组（挂渠道）→ 客户端 Key（勾分组）。未定价或未启用的模型不授权给任何客户端；Key 的模型范围完全由分组决定，调分组即调权限。客户端只需要 Base URL + `crk_` 密钥 + 对外模型名三样。
- **应用型凭据**（v0.3.6，迁移 v12 `client_keys.public_id`）：客户端**不持有任何密钥**的接入方式，用请求头 `X-App-Id: app_…` 认证。`keyless: true` 创建时服务端生成公开应用标识并**丢弃密钥明文**（不返回、不落盘）；`AuthenticateClientApp` 与 `AuthenticateClientKey` **互不越界**（密钥型 `public_id` 为空、应用型无法用 Bearer）——这是防权限提升的关键，别为了"兼容"把两条路打通。两路同给时**密钥优先、失败回落到应用标识**（兼容强制发 `Authorization` 的客户端），但密钥**过期**直接回 `token_expired` 不回落。⚠️ 边界必须讲明：应用标识是公开的，**能访问到网关的人都能调用**，真正的门是「谁能访问到网关」（内网/防火墙/反代），故必须配并发与额度上限。**建 Key 时先算 `token_digest` 再丢弃明文** —— 反过来（先清空再算摘要）会让所有应用型凭据撞 UNIQUE 约束（本项目真实踩过）。
- **短时令牌**（v0.3.5，迁移 v11 `client_keys.expires_at_utc`，NULL = 永久）：给第三方程序/外包用的正确姿势是发会过期的令牌。`ttl_seconds` **三态**——缺省 = 不动、`0`/负数 = 改为永久、`>0` = 从现在起重新计时（续期就是再传一次）；**留空不改有效期**这点必须守住，否则「重命名」会变成静默的永久续期。过期时 `AuthenticateClientKey` 返回 `store.ErrClientKeyExpired` → `relay.ErrTokenExpired` → **401 `token_expired`**（与 `invalid_api_key` 分开，客户端才知道该换令牌而不是当服务故障重试）。过期 ≠ 停用：Key 仍是启用状态，续期立即恢复，到期也不删 Key（历史账目保留）。`keysList`/`connection` 返回 `Expired`/`ExpiresAtUTC`/`ExpiresInSecs`。

## 验证与发布
- 联调 harness 在 `_e2e/`（已 gitignore，不进发布包）：`fake_upstream.py`（Python stdlib 假上游，绑 `0.0.0.0`，`protocol_version` 保持 HTTP/1.0 否则 SSE 收不到 EOF）+ `run_e2e.py`（解析 `host.docker.internal` 真实 IP 当渠道地址、无代理 opener 打本机、39 项断言含额度对照、finally 清容器与卷）。跑法：`python _e2e\run_e2e.py`（需先构建镜像）。**读容器日志必须合并 stdout+stderr**（Go 的 log 写 stderr，只读 stdout 会得到空串）。
  另有 `_e2e/run_client_onboarding.py`（31 项断言）：专验「其他软件接进来」这条闭环 ——
  建渠道/模型/分组/Key → 读接入信息 → **照抄它给的 base_url + token 真调用** → 停用 Key/分组时
  接入信息的提示与实际错误码对齐 → 泄露面复查（接入信息与渠道列表都不含上游密钥）。
  改动 `AdminHandler`/`keyItem`/`web/admin.html` 后跑它。
- 打包：`python scripts\package_release.py <版本>` → `dist/` 下 `-image.tar.gz` / `-src.tar.gz` / `-deploy.tar.gz`（上传服务器用最后一个，解压即完整目录）。PowerShell 里绝不能 `docker save | gzip`（管道会把二进制拆坏）。
- 升级必须在原部署目录里做：compose 项目名默认取**目录名**，卷名 `<目录名>_civic-relay-config`；在新目录 `up -d` 会新建一套空卷（表现为「配置全没了」，旧卷未删、数据没丢但用不上）。
- `scripts/docker-ops.sh` 提供中文命令：帮助 / 启动 / 探针 / 备份 / 看卷 / 检查环境 / 载入镜像 / 启动离线。
- **trailblazer-deploy 已内置 civic-relay 站点档案**（2026-09-26）：打包用 `python scripts\package_site_zip.py`
  → 部署管理器选 Civic Relay 站点部署 zip；档案复用命名卷 `civic-relay_civic-relay-*`，接管前先在原 compose
  目录 `docker compose down`（不带 -v）。Nginx 配置不归部署工具管（其 AGENTS.md 禁令），宝塔 SSE 四件套见
  `docs/deploy-with-trailblazer.md`。

## 生产部署环境（老板的实际部署，2026-09-25 记录）
- **阿里云 ECS + 宝塔面板（BT-Panel）+ Docker Compose**；容器 `civic-ai-relay-civic-relay-1`，卷 `civic-relay_civic-relay-config`（compose 项目名 = 部署目录名）。
- 前面有 **Nginx 反向代理**（宝塔管理）。流式接口对反代的要求见 `docs/client-integration.md` 第八节：
  **`proxy_buffering off` + `proxy_read_timeout 3600s` + `proxy_http_version 1.1` + `client_max_body_size 16m`**。
  默认值会同时造成「发出去没反应」（响应被缓冲）与「流到一半断」（60s 读超时）两种症状 —— 一个根因。
- 宝塔下站点配置在 `/www/server/panel/vhost/nginx/*.conf`，错误日志在 `/www/wwwlogs/*.error.log`。
- ⚠️ 排障命令要**一条一条、单行、不用 `\` 续行、不用 python3**（宝塔终端整块粘贴会错位；CentOS 系可能没 python3，统计用 `grep -o | sort | uniq -c`）。
- ⚠️ `docker run -v <不存在的卷>:/c` 会**静默创建空卷**：取管理员密钥前先 `docker volume ls | grep civic-relay` 确认卷名，否则拿到空串后一路 401。
