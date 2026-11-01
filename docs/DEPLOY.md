# Civic Relay {{VERSION}} 服务器部署说明

本包用于在 Linux 服务器上用 Docker 运行 Civic Relay。**推荐走方式 A（直接用打包好的镜像）**，不需要联网构建。

由 `scripts/package_release.py` 生成（源模板 `docs/DEPLOY.md`）。

---

## 零、Ubuntu 前置检查（先做这三件事）

```bash
# 1. 确认 Docker 与 Compose v2 都在（apt 装的 docker.io 常常不带 compose 插件）
docker version --format '{{.Server.Version}}'
docker compose version          # 报 command not found 就装：
                                # sudo apt update && sudo apt install -y docker-compose-v2

# 2. 当前用户能否免 sudo 调用 docker（不能的话下面所有命令都要加 sudo）
docker info >/dev/null && echo "OK：已在 docker 组"

# 3. 放行 8000（ufw 在跑的话）
sudo ufw status | grep 8000 || sudo ufw allow 8000/tcp
# 云服务器还要在控制台「安全组」里放行 8000 入站，这一步脚本查不到
```

解压发布包后，先跑一次自带的环境体检——它会把上面这些连同「这是全新安装还是升级」
一并告诉你：

```bash
tar xzf civic-relay-{{VERSION}}-deploy.tar.gz
cd civic-relay-{{VERSION}}
bash scripts/docker-ops.sh 检查环境
```

### 建议：装到一个固定目录名里（为以后升级铺路）

compose 的项目名默认取**目录名**，数据卷名会变成 `<目录名>_civic-relay-config`。
如果直接就在 `civic-relay-{{VERSION}}`（带版本号）里运行，**下次升级解压出 `civic-relay-v0.3.3`
再 up，就会新建一套空卷**，看起来像"配置全丢了"。

所以全新安装时先落到一个不带版本号的固定目录：

```bash
sudo mkdir -p /opt && cd /opt
sudo tar xzf /tmp/civic-relay-{{VERSION}}-deploy.tar.gz
sudo mv civic-relay-{{VERSION}} civic-relay        # 去掉版本号
sudo chown -R "$USER":"$USER" /opt/civic-relay     # 之后不用 sudo 也能改 .env
cd /opt/civic-relay
```

以后升级只需：备份 → `docker load` 新镜像 → 把新包的 `docker-compose.yml`、`.env`、
`scripts/` 覆盖进 `/opt/civic-relay` → `docker compose up -d`。卷名不变，配置与账目都在。

---

## 一、方式 A：用现成镜像（推荐，最快）

包里已带好构建产物 `civic-relay-{{VERSION}}-image.tar.gz`（linux/amd64）。

```bash
# 1. 导入镜像（两种写法等价，用哪个都行）
docker load -i civic-relay-{{VERSION}}-image.tar.gz
# 或： gunzip -c civic-relay-{{VERSION}}-image.tar.gz | docker load
# 看到 Loaded image: civic-relay:{{VERSION}} 即成功

# 2. 启动（当前目录需为本包解压后的 civic-relay-{{VERSION}} 目录）
docker compose up -d          # 注意：不要加 --build

# 3. 等状态变成 healthy
docker compose ps

# 4. 确认跑的是目标版本
docker compose exec civic-relay wget -qO- http://127.0.0.1:8000/healthz
```

## 二、方式 B：在服务器上现场构建

需要服务器能访问 Docker Hub 与 Go 模块代理。

```bash
docker compose up -d --build
```

- 别担心 Go 模块：Dockerfile 默认走 `goproxy.cn` + `sum.golang.google.cn`，国内服务器可直接构建；海外服务器可覆盖：
  `docker compose build --build-arg GOPROXY=https://proxy.golang.org,direct --build-arg GOSUMDB=sum.golang.org`
- **服务器是 ARM 架构**（部分云主机是）时，镜像要重新构建并指定平台：
  ```bash
  docker buildx build --platform linux/arm64 -f Dockerfile.relay -t civic-relay:{{VERSION}} --load .
  ```
  或 `docker compose build --build-arg TARGETARCH=arm64`

---

## 三、首次启动与管理员密钥

首次启动会在配置卷里生成 `relay.env` 和一次性的管理员密钥文件：

```bash
# 先确认卷的真实名称（前缀取自 compose 项目名，通常就是目录名）
docker volume ls | grep civic-relay

# 查看密钥（登录管理台用）
docker run --rm -v <卷名>:/c alpine cat /c/bootstrap-admin-key.txt

# 取用后建议删除
docker run --rm -v <卷名>:/c alpine rm -f /c/bootstrap-admin-key.txt
```

密钥在 `relay.env` 的 `ADMIN_API_KEY=` 里也有备份，删了不会丢。

然后打开管理台，按顺序配置：**供应商（渠道）→ 拉取/添加模型并定价 → 模型组（挂渠道）→ 客户端 Key**。

### 额度默认不限，按分组设置

4 个额度窗口（`5 小时 / 当日 / 本周 / 自定义周期` × `Token / 金额`）**出厂全部不限**，
新部署不会因为一个出厂限额就在第一次请求时被拒。

需要限流时在管理台「模型组 → 查看 / 编辑」里按组填数字（金额单位是**元**，可带小数）：

| 想限制 | 填什么 |
|---|---|
| 本周最多 500 万 token | 组编辑弹窗「本周 Token 限额」= `5000000` |
| 每天最多花 5 元 | 「当日金额限额（元）」= `5` |
| 5 小时不限制 | 该框填 `0`（或留空 = 保持原值） |

保存即热应用，不用重启；下一次请求就按新额度判定。

> v0.5.0 起额度不再写在 `relay.env` 里。旧部署升级时，`TOKEN_LIMIT_*` / `AMOUNT_LIMIT_*` /
> `PERIOD_LIMIT_*` 这些残留键会在首次启动时被**自动灌进每个现有分组**（只填还没设过的窗口），
> 然后从 `relay.env` 移除——所以老部署升级后额度照旧生效，不会变成「全部不限」。

## 四、访问地址

| 用途 | 地址 |
|---|---|
| 管理台 | `http://<服务器IP>:8000/admin` |
| 客户端 Base URL | `http://<服务器IP>:8000/v1` |
| 健康检查 | `http://<服务器IP>:8000/healthz`（不需要认证） |

管理接口用请求头 `X-Admin-Key`，公共接口用 `Authorization: Bearer crk_...`，两种密钥不通用。

### 必须放行防火墙

Linux 上按发行版选一条：

```bash
# ufw（Ubuntu/Debian）
sudo ufw allow 8000/tcp

# firewalld（CentOS/RHEL）
sudo firewall-cmd --add-port=8000/tcp --permanent && sudo firewall-cmd --reload

# 云服务器还要在控制台的「安全组」里放开 8000 入站
```

> ⚠️ 这是**明文 HTTP**：管理台与客户端 Key 都会明文传输。只在内网/测试环境这样用；要对公网提供服务，请把端口改回只绑本机（`CIVIC_RELAY_BIND=127.0.0.1`）再挂 Nginx/Caddy 做 HTTPS 反向代理。

## 五、升级

```bash
# 1. 先备份（停容器后整目录导出，配置与数据库必须成对）
bash scripts/docker-ops.sh 备份

# 2. 导入新镜像
docker load -i civic-relay-{{VERSION}}-image.tar.gz

# 3. 让本包目录接管运行：把新包的 docker-compose.yml 与 .env 覆盖到你的部署目录，
#    确认 .env 里是 CIVIC_RELAY_VERSION={{VERSION}}，然后重建容器
docker compose up -d
```

数据库迁移在启动时自动执行（`schema_migrations` 记录版本），无需手工操作。
跨版本回滚前先备份：旧版本不认识新迁移引入的列。

### ⚠️ 升级必须在原部署目录里做，否则会变成「全新安装」

compose 的项目名默认取**目录名**，数据卷名是 `<目录名>_civic-relay-config` /
`<目录名>_civic-relay-data`。解压出来的新包目录叫 `civic-relay-{{VERSION}}`，
**和原来的部署目录名往往不一样**——直接在新目录里 `docker compose up -d` 会新建一套空卷，
表现是「配置全没了、要重新取管理员密钥」，而旧数据其实还在旧卷里（没丢，但用不上）。

正确做法二选一：

```bash
# ① 推荐：把新包的 compose 文件与 .env 覆盖到原部署目录，在原目录里操作
cp civic-relay-{{VERSION}}/docker-compose.yml civic-relay-{{VERSION}}/.env /path/to/原部署目录/
cd /path/to/原部署目录 && docker compose up -d

# ② 或者显式指定旧项目名（旧卷名去掉 _civic-relay-config 后缀）
cd civic-relay-{{VERSION}}
export COMPOSE_PROJECT_NAME=<旧项目名>
docker compose up -d
```

拿不准时先看卷名，并跑一次环境体检：

```bash
docker volume ls | grep civic-relay
bash scripts/docker-ops.sh 检查环境      # 会直接告诉你「全新安装」还是「升级」，以及卷名是否对得上
```

### 额度语义（v0.3.2 起）

- **先欠费后停服**：只检查「该窗口是否已用满」，不要求「已用 + 本次预留 ≤ 限额」。
  只要还有余量就放行（本次请求可能让它略微超出），用到限额之后才拒绝后续请求。
  调大限额或窗口轮换后**自动恢复**，不需要重启或人工启用 Key。
- 额度用满返回 **HTTP 429** 与具体错误码（`token_quota_exceeded` / `amount_quota_exceeded` /
  `key_token_quota_exceeded` / `key_amount_quota_exceeded`），不再是 500 `relay_error`。
  把 500 当临时故障重试的客户端，现在会正确地按限流退避。
- **Key 不再因额度自动停用**。迁移 v9 会把历史上因额度被自动停用的 Key
  （`disabled_reason='quota_exhausted'`）恢复成启用；手动停用的不受影响。
- **额度位置变了（v0.5.0）**：额度从 `relay.env` 搬到各模型组。升级后第一次启动时，
  旧 relay.env 里的 `TOKEN_LIMIT_*` / `AMOUNT_LIMIT_*` / `PERIOD_LIMIT_*` 会被灌进**每个现有分组**
  （只填还没设过的窗口），随后从 `relay.env` 删除。启动日志会有一行
  `全局额度配置已灌入 N 个分组并从 relay.env 移除` 作为凭据。

### 已删除的配置项（残留行无害）

`UPSTREAM_BASE_URL`、`UPSTREAM_API_KEY`、`DOCS_ENABLED` 已从程序中移除，原因分别是：
前两个是单上游时代的遗留（渠道与其密钥现在都存在数据库里并加密存储，填在这里既不生效、还会把
Key 明文写进 relay.env），后者对应的 docs 路由从未实现。

旧 relay.env 里残留的这几行**会被当作未知键原样保留、不再读取**，不影响启动；
介意的话可以从 relay.env 里删掉它们（该文件在配置卷内，改完重启容器生效）。

### 新增/生效的配置项

| 配置项 | 默认 | 说明 |
|---|---|---|
| `GROUP_MONITOR_INTERVAL` | `30m` | 分组可用性监测间隔，`0` 关闭。每轮对组内启用渠道打一次上游 `/v1/models`，只记录与展示，不改变服务行为 |
| `MODEL_AUTO_SYNC` | `false` | 自动同步上游模型，**默认关闭**。开启后会把上游新增模型导入为「停用 + 未定价」，需人工定价启用后才对外可用 |
| `MODEL_SYNC_INTERVAL` | `30m` | 自动同步间隔 |
| `LOG_LEVEL` | `INFO` | `DEBUG` 会打开逐请求访问日志（不记查询串/请求头/请求体）。**写错会在启动时直接报错退出**（以前这个值会被静默忽略） |

> ⚠️ 升级提示：`LOG_LEVEL` 与 `MODEL_SYNC_INTERVAL` 之前是「能改但没人读」的死配置。
> 升级后它们真的生效了，所以请顺手确认这两个值符合预期——尤其是 `MODEL_SYNC_INTERVAL`
> 填了很短间隔（如 `1m`）的话，开启自动同步后会按该频率访问所有启用渠道的上游。

## 六、数据、备份与恢复

| 卷 | 内容 |
|---|---|
| `<项目名>_civic-relay-config` | `relay.env`（含 `RELAY_ENCRYPTION_KEY`）、管理员密钥文件 |
| `<项目名>_civic-relay-data` | `relay.db`（WAL 模式，还有 `-wal`/`-shm`） |

**这两样必须成对备份**：`relay.env` 里的 `RELAY_ENCRYPTION_KEY` 一旦丢失，数据库里的上游密钥就再也解不开。

**不要只拷 `relay.db`**：WAL 模式下未 checkpoint 的数据在 `relay.db-wal` 里，热拷贝会丢最新账目。正确做法是先停容器再整目录导出：

```bash
bash scripts/docker-ops.sh 备份          # 自动停容器 → 导出全部文件 → 重启
```

## 七、常用命令

```bash
bash scripts/docker-ops.sh 帮助      # 全部命令
bash scripts/docker-ops.sh 启动      # 构建并启动，自动等 healthy
bash scripts/docker-ops.sh 探针      # /healthz、/readyz、容器健康一眼看完
bash scripts/docker-ops.sh 日志 200  # 跟踪最后 200 行
bash scripts/docker-ops.sh 看卷      # 用辅助容器查看两个卷（镜像无 shell）
bash scripts/docker-ops.sh 备份      # 停容器后完整备份
```

等价的原始命令：

```bash
docker compose ps                       # 状态
docker compose logs --tail=100 -f       # 日志
docker inspect --format '{{.State.Health}}' <容器名>
docker stats --no-stream <容器名>
```

## 八、可调参数

在包目录下建 `.env`（已有默认值）或用环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `CIVIC_RELAY_VERSION` | `{{VERSION}}`（本包 `.env` 已设） | 镜像 tag。**必须与 `docker load` 出来的 tag 一致**，否则 compose 会去构建 |
| `CIVIC_RELAY_BIND` | `0.0.0.0` | 监听地址；设 `127.0.0.1` 则仅本机可访问 |
| `CIVIC_RELAY_PORT` | `8000` | 宿主机端口 |
| `TZ` | `Asia/Shanghai` | 容器日志时区 |

运行期配额、输出上限等配置在**管理台「运行配置」**里改（保存即热应用，不用重启）；只有 `HOST`/`PORT`/`DB_PATH` 需要重启，换 `RELAY_ENCRYPTION_KEY` 也必须重启。

## 九、换成宿主机目录存放数据（可选）

默认用 Docker 命名卷，开箱即用。若要把配置与数据库放到宿主机目录：

```bash
export CIVIC_RELAY_CONFIG_DIR=/srv/civic-relay/config
export CIVIC_RELAY_DATA_DIR=/srv/civic-relay/data
sudo mkdir -p "$CIVIC_RELAY_CONFIG_DIR" "$CIVIC_RELAY_DATA_DIR"
# 容器以非 root 的 UID 65532 运行，Linux 上必须 chown，否则启动就写不进文件
sudo chown -R 65532:65532 "$CIVIC_RELAY_CONFIG_DIR" "$CIVIC_RELAY_DATA_DIR"
docker compose -f docker-compose.yml -f docker-compose.bind.yml up -d
```

## 十、排查

| 现象 | 处理 |
|---|---|
| 其他机器连不上，服务器本机 `curl 127.0.0.1:8000/healthz` 正常 | 防火墙/安全组没放行 8000，见第四节 |
| `docker compose up` 开始重新构建镜像 | 镜像 tag 对不上，检查 `.env` 的 `CIVIC_RELAY_VERSION` 与 `docker images` 是否一致 |
| 容器反复重启、日志报权限错误 | 用了绑定卷但没 `chown 65532`，见第九节 |
| 客户端报 `token_quota_exceeded` / `amount_quota_exceeded` | 对应窗口已用满。管理台概览会把该窗口标红并提示**已欠费**；调大限额或等窗口轮换即自动恢复。要彻底不限就填 `0` |
| 客户端报 `key_token_quota_exceeded` | 该 Key 的**总**额度用满（不随窗口轮换）。到管理台「客户端 Key」编辑，调大或清空总额度 |
| 客户端报 `relay_error` 但 `/v1/models` 正常 | 看容器日志与管理台「请求记录」；上游错误会带 `upstream_*` 码 |
| 添加渠道报 `provider_invalid` | 错误消息里已带具体原因（缺协议头、不在 HTTP 放行名单、重名等）。容器访问宿主机上的服务要用网桥网关 IP（如 `172.17.0.1`），填 `127.0.0.1` 只会指向容器自己 |

更详细的说明见 `docs/docker-deploy.md` 与 `docs/admin-api.md`。
