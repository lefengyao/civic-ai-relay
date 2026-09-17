# Civic Relay 项目长期约定

## 配置与路径
- **relay.env 是唯一配置来源**：只读 `CIVIC_RELAY_CONFIG_FILE`（容器内 `/app/config/relay.env`）。除该变量外的环境变量（含 `PORT`/`DB_PATH`）对服务**无效**，不要试图用 compose environment 覆盖设置。
- 引导时写入的 `DB_PATH=data/relay.db` 是**相对工作目录**的路径。容器镜像必须保持 `WORKDIR /app` 且数据卷挂在 `/app/data`，否则数据库会落在容器临时层，重建即丢数据（这是本项目踩过的真实缺陷）。
- 管理端保存配置会原子写回 relay.env 并热应用；仅 `HOST`/`PORT`/`DB_PATH`/`DOCS_ENABLED` 需重启。
- 配置文件与数据库**必须一起备份**，尤其是 `RELAY_ENCRYPTION_KEY`（丢了则库里的上游 Key 无法解密）。备份 SQLite 不能只拷 `relay.db`：WAL 模式下要连 `-wal`/`-shm` 一起，或先停容器。

## 容器运行约定
- 端口默认映射 `0.0.0.0:8000->8000`（compose 变量 `CIVIC_RELAY_BIND`，等价 `8000:8000`），局域网可用 `http://<本机IP>:8000`；要仅本机可访问设 `CIVIC_RELAY_BIND=127.0.0.1`。**改宿主机端口用 `CIVIC_RELAY_PORT`，不要改 relay.env 的 `PORT`**——容器内固定 8000，映射写死了容器侧 8000，改了内网端口对外就连不上。
- **Windows 上局域网可达还需放行防火墙入站 8000**：本机三个防火墙配置（Domain/Private/Public）默认全部启用且没有任何 docker 相关规则，此时宿主机端口代理的入站连接被丢弃（其他设备表现为连接超时），而本机 `127.0.0.1:8000` 仍正常。需管理员执行 `New-NetFirewallRule -DisplayName "Civic Relay 8000" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8000`。排查时注意：`127.0.0.1` 通、`<本机IP>` 不通 = 防火墙问题，不是映射没生效。
- 镜像非 root（UID/GID `65532`）、只读根文件系统，仅 `/app/config`、`/app/data` 与 tmpfs `/tmp` 可写。宿主目录绑定卷在 Linux 上必须 `chown -R 65532:65532`。
- 无 shell（distroless）：排障用 `docker run --rm -v <卷>:/c alpine sh -c ...` 看卷内容；自检用 `docker exec <容器> /civic-relay -healthcheck`。
- 探针语义：`/healthz` = 存活（不触达数据库，避免数据库瞬时故障引发重启循环）；`/readyz` = 依赖就绪（SQLite 不可用返回 503）。两者免认证、仅 GET/HEAD。
- 代码里加健康检查依赖时，改 `store.Ping` / `relay.Service.Ready` / `httpapi.registerHealth` 三处即可。

## 构建约定（本机网络受限）
- Go 模块代理默认走 `goproxy.cn` + `GOSUMDB=sum.golang.google.cn`（官方 proxy 在本机握手超时）。Dockerfile 里是 ARG，可覆盖。
- 不要写 `# syntax=docker/dockerfile:N`：会强制拉取外部前端镜像，本机网络下第一步就失败。`RUN --mount=type=cache` 内置前端已支持。
- 构建缓存挂载 + `.dockerignore`（上下文约 331kB）已配好，二次构建通常数秒。
- **Dockerfile 中 `COPY a b c ./` 会把各目录内容拍平**，多目录必须逐个写目标：`COPY cmd/ ./cmd/`。

## 联调与验证配方
- **`test-data/` 已于 2026-09-11 按用户要求删除**（含假上游脚本、31 张 UI 截图、验证脚本）。需要联调时**重新写一个假上游**（绑 `0.0.0.0`，因为原脚本绑 127.0.0.1 容器访问不到）。删除前的完整留档：`backups/pre-cleanup-20260911.tar.gz`（2.2MB，内含 `data/relay.db`、`test-data/`、`.superpowers/`、`.codex/`、构建提示词），需要时可解包取回假上游脚本。
- 渠道 `base_url` 校验只允许 HTTPS，HTTP 仅放行 localhost/`*.localhost`/`*.test`/回环/私有 IP（10./172.16-31./192.168.）。容器访问宿主机请用**私有 IP**（Docker Desktop 下 `host.docker.internal` = `192.168.65.254`），不要直接用该域名。
- **`validateProviderURL` 的三个易踩点**（2026-09-16 复刻实测）：① **必须带 scheme**，`127.0.0.1:6000` 这种写法 Go 的 `url.Parse` 直接失败（`first path segment in URL cannot contain colon`），报 `provider_invalid`；② `host.docker.internal` 与**同 compose 的网络服务名/容器名都不在放行名单**，容器要连宿主机服务只能用网桥网关 IP（默认 bridge `172.17.0.1`，compose 自建网络一般 `172.18.0.1`，`docker network inspect` 确认）或宿主机内网 IP，且上游必须绑 `0.0.0.0`；③ 存库会剥掉尾部 `/` 和 `/v1`，地址框不用带 `/v1`。
- **`provider_invalid` 现在带原因**（2026-09-16 修复）：`admin.go` 的 provider 建/改失败改走 `writeProviderError` → `writeAdminErrorDetail`，响应为 `{"error":{"message":"provider_invalid: <原因>","code":"provider_invalid"}}`，管理台 toast 直接显示原因。相关联动：`internal/store/providers.go` 里与校验相关的**错误字符串已从英文改为中文**（前端 `base_url` input 也加了 `type="url"`，`openEdit` 保存前跑 `checkValidity()`）。所以排查这类问题不必再读代码，看 toast 即可；回归测试在 `internal/store/provider_url_test.go` 与 `admin_test.go::TestAdminProviderErrorCarriesReason`。
- **v0.3.2 起额度语义是「先欠费后停服」**（2026-09-16 重做，旧的「预留必须整体塞得下」口径已废）：
  只检查「窗口是否已用满」（`used >= limit`），不再要求 `已用 + 本次预留 <= 限额`。只要还有余量就放行，
  用到限额之后才拒后续请求；调大限额或窗口轮换后**自动恢复**。维度为 `5h / 当日 / 本周` × `Token / 金额`
  共 6 项（`TOKEN_LIMIT_*` / `AMOUNT_LIMIT_*`，金额配置单位是**元**），**任一项为 0 或留空 = 不限**。
  日=北京时间自然日，周=北京时间自然周（周一起，`billingWeekStart` 用区间查询，未加列、无迁移）。
- 额度拒绝一律 **429**，`error.code` ∈ `token_quota_exceeded` / `amount_quota_exceeded` /
  `key_token_quota_exceeded` / `key_amount_quota_exceeded`（消息里带窗口与数字）。旧实现落到
  `writeServiceError` 的 default 分支返回 **500 relay_error**，客户端会当服务端故障重试——这是老 bug。
- **Key 不再因额度自动停用**：`finishRequest` 里那段 `enabled=0 + disabled_reason='quota_exhausted'` 已删，
  迁移 v9 会把历史遗留的这类停用恢复成启用。Key 停用现在只能手动。
- **`ReserveForKey` / `SettleKey` / `KeyReservation` 已删除**（只有测试在用的第二套配额实现，且还带着
  刚被移除的自动停用逻辑，留着是地雷）。配额实现只剩 `ReserveRequest` / `SettleRequest` 一套。
- **输入估算**：中日韩全角 1.25 token/字、拉丁/符号 3.5 字符/token，**另加每条请求固定开销 4 token**
  （`perRequestOverheadTokens`），并且 `relay.Service.Begin` 会把 `tools`/`functions`/`response_format`
  也序列化进 `StringFields`（旧实现漏算，带工具定义的客户端会被低估）。校准测试在 `internal/store/estimate_test.go`。
- **改「解析层默认值」不影响「出厂默认值」**：额度的出厂值写在引导模板 `internal/config/store.go::GenerateInitialSettings`
  （显式写入 relay.env，压过解析 fallback）。曾经模板里写死 `100000`/`20000`，而预留按「输入估算+输出上限」算，
  新部署第一次编码请求就被拒。已在模板里全部改成 `"0"`；回归测试 `config/store_test.go::TestGeneratedSettingsLeaveQuotaWindowsUnlimited`。
- 管理端接口建资源统一返回 **201** 且结构为 `{"data": <store 结构体>}`，字段是 **Go 字段名（PascalCase）**（store 结构体无 json tag）；Key 创建的明文密钥在顶层 `token`。列表接口空集合返回 `[]`。
- 排查额度类报错**先看 `/admin/api/overview`**：`five_hour`/`daily`/`weekly` 各自返回
  `{used_tokens, limit, used_microyuan, amount_limit}`，limit 为 0 即不限；管理台概览会把用满的窗口标红提示「已欠费」。
- 判定口径不要靠猜：把某个窗口限额压到「已用之下」→ 应得 429 + 对应 code → 改回 0 → 应立即恢复 200。
  **判定阶段被拒的请求不会打到上游、不产生费用**，适合免费验证。
- 查库时注意：删掉客户端 Key 会级联删除它的 `key_reservations`，于是窗口用量会「归零」，但 `requests` 表的历史行仍在（金额保留、provider 置空）——别把「窗口已用 0」误读成「从来没请求过」。

## 联调与发布
- **分组可用性监测**（2026-09-17 新增）：`GROUP_MONITOR_INTERVAL`（默认 `30m`，`0` = 关闭）控制定时轮询；
  每轮先判配置（组启用 / 有启用渠道 / 渠道下有已启用且已定价模型），再对启用渠道打上游 `GET /v1/models`。
  五态：`ok` / `degraded`（部分渠道不可达）/ `upstream_unreachable` / `config_unavailable` / `disabled`。
  **只记录与展示，不改变服务行为**。配置不成立时**不探活**（避免掩盖配置问题）。
  管理台分组列表有「立即检测全部」与每行「检测」，概览弹窗有最近 10 轮历史。
  相关文件：`internal/store/monitor.go`、`internal/relay/monitor.go`、迁移 v10 的 `group_monitor_results`。
- ⚠️ **`GET /admin/api/groups/health` 与 `POST /admin/api/groups/monitor` 的路由必须排在
  `HasPrefix("groups/")` 之前**，否则会被 `groupItem` 的 `ParseInt` 当成 ID 判成 `invalid_id`。
  已由 `admin_test.go::TestAdminGroupMonitorRoutesAreNotParsedAsIDs` 钉住。
- ⚠️ **`ReplaceGroupProviders` 拒绝停用渠道**（models.go:374），所以「组内有停用渠道」只能
  先入组、再停用渠道产生。写测试或做迁移时按这个顺序来。
- **`MODEL_AUTO_SYNC` / `MODEL_SYNC_INTERVAL` 曾长期是死配置**（2026-09-17 已实现）：
  现在由 `internal/relay/sync.go::ModelSyncer` 按间隔遍历启用渠道、调 `Registry.SyncProvider`
  导入上游新模型，导入一律**停用 + 未定价**（需人工定价启用，自动同步不绕过定价）。
  **解析默认值已改为 false**（原来那个 true 从未生效过，不该在升级后突然开始写库）。
- **`LOG_LEVEL` 曾长期是死配置**（2026-09-17 已实现）：新增 `internal/logging`
  （ERROR/WARN/INFO/DEBUG，`ParseLevel` 严格校验，`Configure` 热应用）+
  `internal/httpapi/accesslog.go`（DEBUG 下逐请求访问日志，**只记方法/路径/状态/耗时/来源地址**，
  绝不记查询串、请求头、请求体）。
  ⚠️ 访问日志的 `statusRecorder` **必须实现 `http.Flusher`**，否则会破坏 SSE 流式；
  非 DEBUG 时完全不包装 ResponseWriter。
- **已删除的配置项**：`UPSTREAM_BASE_URL` / `UPSTREAM_API_KEY`（单上游时代遗留，填这里会把
  Key 明文写进 relay.env 且完全不生效）、`DOCS_ENABLED`（从来没有 docs 路由）。
  旧 relay.env 里残留的这几行按未知键保留、不再读取、不再回写。
  `config` 里那个只服务于 `UPSTREAM_BASE_URL` 的 `optionalURL` 也一并删除。
- **`scripts/init_config.ps1` / `migrate_config.ps1` 已删除**：Docker 化之前的 Windows 原生部署
  遗留（`C:\ProgramData\CivicRelay\relay.env` + `PUBLIC_API_KEY`/`MODEL_WHITELIST` 等已不存在的键），
  而且会进发布包、跑一遍就产出配置错的 relay.env。`scripts/` 现在只有 `docker-ops.sh` 与 `package_release.py`。
- **审计死配置的脚本**：`_audit_config.py`（统计 Settings 每个字段在仓库里的出现次数，
  只在 `internal/config` 里出现的就是没人读的）。查「某个配置改了没反应」时先跑它。
- **端到端断言读容器日志必须合并 stdout+stderr**：Go 的 `log` 默认写 stderr，
  而 `docker logs` 把容器 stderr 送到自己的 stderr，只读 `.stdout` 会拿到空字符串
  （断言会「永远通过」或「永远失败」）。`_e2e/run_e2e.py` 的 `container_logs()` 已处理。
- **端到端联调harness在 `_e2e/`**（已 gitignore，不进发布包）：`fake_upstream.py`（Python stdlib 假上游，
  绑 `0.0.0.0`，`protocol_version` 保持 HTTP/1.0 否则 SSE 收不到 EOF；固定返回 prompt 12 / completion 7）
  + `run_e2e.py`（解析 `host.docker.internal` 的真实 IP 当渠道地址、用无代理 opener 打本机 127.0.0.1、
  跑 39 项断言含额度对照实验、finally 清容器与卷）。跑法：`python _e2e\run_e2e.py`（需先构建镜像）。
- **发布打包用 `scripts/package_release.py <版本>`**：重建源码快照 → 渲染 `docs/DEPLOY.md` 模板
  （`{{VERSION}}` 占位）→ 写 `.env` → `docker save -o` + Python gzip（**PowerShell 里绝不能
  `docker save | gzip`，管道会把二进制拆坏**）→ 出 `-image.tar.gz` / `-src.tar.gz` /
  `-deploy.tar.gz`（★ 上传服务器的就是最后一个，解压即得完整目录）。
- 版本号只出现在命令行参数与 `docs/DEPLOY.md` 模板里；改部署说明改模板，别改 `dist/` 里的产物。
- **升级必须在原部署目录里做**：compose 项目名默认取**目录名**，卷名是 `<目录名>_civic-relay-config`。
  发布包解压出来的目录叫 `civic-relay-vX.Y.Z`，与用户原部署目录名通常不同 → 在新目录里 `up -d`
  会**新建一套空卷**，表现为「配置全没了、要重新取管理员密钥」（旧卷没删、数据没丢但用不上）。
  对策：把新包的 compose 文件与 `.env` 覆盖进原目录再操作，或 `export COMPOSE_PROJECT_NAME=<旧项目名>`。
- `scripts/docker-ops.sh` 新增两条命令：`检查环境`（doctor：docker/compose 版本、镜像是否已 load、
  **卷名与当前目录是否匹配**、端口占用、ufw 是否放行，并直接判定「全新安装 / 升级」）与
  `载入镜像 [文件]`（`docker load` 离线镜像包，默认在当前目录 glob `civic-relay-*-image.tar.gz`）、
  `启动离线`（`compose up -d` 不带 `--build`）。Ubuntu 用 apt 装的 `docker.io` 常常缺 compose v2 插件，
  doctor 会直接提示 `sudo apt install -y docker-compose-v2`。

## 本机环境
- Docker Desktop 需**用户手动启动**：沙箱安全策略拉黑 `wsl.exe`，会话内起不来（`docker desktop start` 会挂住）。
- Docker 命令一律显式调用 `C:\Users\15610\AppData\Local\Programs\DockerDesktop\resources\bin\docker.exe`（无扩展名的 `docker` 是 sh 脚本，git bash 下会失败），且走 Bash 工具；**PowerShell 工具输出不回传**，需要落盘再 Read。
- Docker Hub 偶发 `Bad Gateway`，重试即通；gcr.io 正常。
- 沙箱禁止在工作区外建目录、禁止 `go build -o` 输出到工作区外。
