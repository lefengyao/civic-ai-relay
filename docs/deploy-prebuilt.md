# 预编译部署（服务器无 Go 工具链、无 GitHub 出口）

适用场景：部署服务器能跑 Docker，但**拉不到 Docker Hub / gcr.io**、**没有 Go
工具链**、**访问不到 GitHub**，所以既不能现场多阶段构建，也不能 `git clone`。

做法是本机当构建机，服务器只接收「从某个 commit 派生的包」并组装运行层。

当前生产：waigame，部署目录 `/opt/civic-ai-relay`，compose 项目名 = 目录名，
命名卷 `civic-ai-relay_civic-relay-config` / `-data`。

## 前提

- 本机 `go`（go.mod 要求的版本）与仓库依赖缓存；`GOPROXY=off` 可离线构建
- 本机 Git Bash（`go` / `git` / `tar` / `sha256sum` 都在）
- 服务器上存在基座镜像 `civic-relay-runtime:base`
- 传输通道：ServerOpsHelper MCP 的 `upload_file` / `download_file`（从磁盘直流，
  二进制安全，无 8 MiB 限制），或任何能用的 scp/sftp

> ⚠️ `civic-relay-runtime:base` 是唯一的运行时基座，**删了就再也建不出镜像**
> （服务器拉不到 distroless）。清理旧镜像时必须保留它。

> ⚠️ 仓库里 `scripts/*.py` 那套（`package_release.py`、`package_site_zip.py`、
> `export_deploy_plugin.py`）在当前构建机上**跑不了**：本机没装 Python，只有
> `WindowsApps\python.exe` 这个空壳别名——调用它无输出、退出码 0，看起来像成功。
> 要用那条路子得先装 Python；本文档的流程只依赖 bash + go + git + docker。

## 1. 本机构建包

```bash
bash scripts/build_deploy_bundle.sh v0.3.9
```

产物在 `dist/deploy-v0.3.9/`：`civic-relay`（linux/amd64 静态二进制，版本号已
烧进 ldflags）、`src.tar.gz`（`git archive HEAD`）、`DEPLOYED_COMMIT`、`SHA256SUMS`。

脚本在工作区不干净时**直接拒绝构建**：产物必须能回溯到一个 commit，否则服务器
上的树又会漂成「只存在于服务器的改动」——这套流程就是为了消除那个毛病。

## 2. 上传

把三样东西传到部署目录：

| 本机 | 服务器 |
|---|---|
| `dist/deploy-<版本>/civic-relay` | `/opt/civic-ai-relay/civic-bin` |
| `dist/deploy-<版本>/src.tar.gz` | `/opt/civic-ai-relay/src.tar.gz` |
| `dist/deploy-<版本>/DEPLOYED_COMMIT` | `/opt/civic-ai-relay/DEPLOYED_COMMIT` |

核对校验和，别跳过：

```bash
cd /opt/civic-ai-relay && sha256sum -c SHA256SUMS   # 或逐个比对 DEPLOYED_COMMIT 里的值
```

## 3. 让服务器树等于该 commit

```bash
cd /opt/civic-ai-relay
tar -xzf src.tar.gz          # 覆盖所有 git 跟踪的文件
rm -f src.tar.gz
```

解压后目录里只有三类**不属于** commit 的本地文件：`.env`（版本号）、`civic-bin`
（二进制）、`DEPLOYED_COMMIT`（标记）。其余一切以 git 为准——需要改代码就回本机
改、提交、重新走第 1 步，**不要直接在服务器上编辑**。

## 4. 构建镜像并切换

```bash
cd /opt/civic-ai-relay
docker build -f Dockerfile.prebuilt -t civic-relay:v0.3.9 .

cp -a .env ".env.bak-$(date +%Y%m%d-%H%M%S)"
sed -i 's/^CIVIC_RELAY_VERSION=.*/CIVIC_RELAY_VERSION=v0.3.9/' .env
sed -i 's/^CIVIC_RELAY_COMMIT=.*/CIVIC_RELAY_COMMIT=<短 hash>/' .env
sed -i 's|^CIVIC_RELAY_BUILD_DATE=.*|CIVIC_RELAY_BUILD_DATE=<iso>|' .env

docker compose up -d         # 不要加 --build：那会走 Dockerfile.relay 的 golang 阶段
```

`docker compose up -d` 会重建容器，有几秒停机。命名卷不受影响，配置与账目都在。
**永远不要用 `docker compose down -v`**——那会删掉配置卷与数据库。

**含 schema 迁移的版本升级前必须先停机做一致备份。** 项目没有 down-migration 机制，
迁移是单向的（例如 v0.4.0 的 v16/v17 会给账目表加列并 `DROP TABLE key_groups`，
旧版二进制的授权查询依赖该表，删了就起不来）——回滚只能整库恢复备份，不是切回
旧镜像。停容器后把数据卷里的 `relay.db` 连 `-wal`/`-shm` 一起拷走（WAL 模式下
只拷主文件会丢最近的账目），配置卷的 `relay.env` 一并备份：

```bash
cd /opt/civic-ai-relay && docker compose stop
TS=$(date +%Y%m%d-%H%M%S)
docker run --rm -v civic-ai-relay_civic-relay-data:/d -v /tmp:/out alpine:3.20 \
  tar -czf /out/civic-data-$TS.tar.gz -C /d .
docker run --rm -v civic-ai-relay_civic-relay-config:/c -v /tmp:/out alpine:3.20 \
  tar -czf /out/civic-config-$TS.tar.gz -C /c .
# 确认两个 tar.gz 落在 /tmp 后再 docker compose up -d
```

## 5. 验证

```bash
docker ps --filter name=civic --format '{{.Names}} | {{.Image}} | {{.Status}}'
curl -s http://127.0.0.1:8000/healthz     # version 应等于新版本号
curl -s http://127.0.0.1:8000/readyz
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8000/admin/   # 200
docker logs --tail 20 civic-ai-relay-civic-relay-1
```

日志出现 `group monitor: 已检测 N 个分组` 说明配置卷与数据库都正常加载。
数据卷可直接查验（镜像是 distroless，容器里没有 shell）：

```bash
docker run --rm -v civic-ai-relay_civic-relay-data:/d alpine:3.20 ls -la /d
```

## 6. 回滚

```bash
cd /opt/civic-ai-relay
sed -i 's/^CIVIC_RELAY_VERSION=.*/CIVIC_RELAY_VERSION=<旧版本>/' .env
docker compose up -d
```

前提是旧镜像 tag 还在。确认新版本稳定运行后再删旧镜像，否则回滚点就没了。

## 7. 清理

稳定运行一段时间后：

```bash
docker images | grep civic          # 保留 runtime:base 与当前在跑的版本
docker rmi civic-relay:<旧 tag>
docker builder prune -f
```

`Dockerfile.prebuilt` 每次只加一层二进制，镜像体积恒定在 ~38MB，不会像
「`FROM` 上一个迭代 tag」那样随次数叠加（那样做过：10 层 100MB 装 14MB 二进制）。

## 与 Dockerfile.relay 的分工

| | Dockerfile.relay | Dockerfile.prebuilt |
|---|---|---|
| 构建阶段 | `golang:1.27-bookworm` 多阶段 | 无，直接用本机产物 |
| 运行基座 | distroless（可传 `RUNTIME_IMAGE=scratch`） | `civic-relay-runtime:base` |
| 适用 | 本机能拉基础镜像时、正式发版打包 | 服务器无外网、无 Go 工具链 |

两者的运行层布局必须保持一致（`/app/config`、`/app/data` 属主 65532、CA 证书、
`Asia/Shanghai` 与 `UTC` 时区、`WORKDIR /app`、`USER 65532`、healthcheck）。
改了其中一个，另一个也要跟着改。
