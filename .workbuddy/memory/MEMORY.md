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
- 渠道 `base_url` 校验只允许 HTTPS，HTTP 仅放行 localhost/`*.localhost`/`*.test`/回环/私有 IP。容器访问宿主机请用**私有 IP**（Docker Desktop 下 `host.docker.internal` = `192.168.65.254`），不要直接用该域名。
- 假上游用量 4000/2000/1600，全局 `TOKEN_LIMIT_5H=100000`、`TOKEN_LIMIT_DAILY=20000`：单次预留 4000+4096，**连发超过 2 次会被配额拦截**。
- 管理端接口建资源统一返回 **201** 且结构为 `{"data": <store 结构体>}`，字段是 **Go 字段名（PascalCase）**（store 结构体无 json tag）；Key 创建的明文密钥在顶层 `token`。列表接口空集合返回 `[]`。
- 排查「models 200 但 chat 500 relay_error: token_quota_exceeded」：**先看 /admin/api/overview 的两个窗口已用量**，若都是 0 就说明不是历史占用，而是**单次预留就塞不下**——预留额度 = `输入估算 + 输出上限(MAX_OUTPUT_TOKENS)`（ledger.go:136），必须满足 `窗口已用 + 预留 <= 限额`（198/204 行）。**输入估算已于 2026-09-11 校准重写（v0.3.1）**：中日韩全角字符 1.25 token/字、拉丁/符号 3.5 字符/token，且只统计一份文本（旧实现把整段 JSON 与每条消息 JSON 双份相加、并按"1 字符=1 token"计，等效真实消耗的 4~8 倍，导致出厂默认 20,000 的日限额只够单次约 7,950 字符；校准后可容纳约 5.5 万字符）。校准测试固定在 `internal/store/estimate_test.go`。验证手法：写临时 Go 测试直接调 `estimateInputTokens`（零成本、不上游）。
- **当前全局限额**（2026-09-11 上调，热应用+已写回 relay.env）：`TOKEN_LIMIT_DAILY=5000000`、`TOKEN_LIMIT_5H=20000000`；`MAX_OUTPUT_TOKENS=4096`（客户端请求更大的输出会被静默收敛到 4096，编码类客户端可能觉得回复被截断，需要时再调）。`RPM_LIMIT=30`、`GLOBAL_CONCURRENCY_LIMIT=8`。
- 判定口径不要靠猜：建一个临时客户端 Key 做对照实验（小请求应 200、大请求应 500 token_quota_exceeded），**预留阶段被拒的请求不会打到上游、不产生费用**，适合免费验证；用完即删该 Key。
- 查库时注意：删掉客户端 Key 会级联删除它的 `key_reservations`，于是窗口用量会「归零」，但 `requests` 表的历史行仍在（金额保留、provider 置空）——别把「窗口已用 0」误读成「从来没请求过」。

## 本机环境
- Docker Desktop 需**用户手动启动**：沙箱安全策略拉黑 `wsl.exe`，会话内起不来（`docker desktop start` 会挂住）。
- Docker 命令一律显式调用 `C:\Users\15610\AppData\Local\Programs\DockerDesktop\resources\bin\docker.exe`（无扩展名的 `docker` 是 sh 脚本，git bash 下会失败），且走 Bash 工具；**PowerShell 工具输出不回传**，需要落盘再 Read。
- Docker Hub 偶发 `Bad Gateway`，重试即通；gcr.io 正常。
- 沙箱禁止在工作区外建目录、禁止 `go build -o` 输出到工作区外。
