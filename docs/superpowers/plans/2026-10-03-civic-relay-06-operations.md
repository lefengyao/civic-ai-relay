# 阶段6：备份、恢复与发布验证实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. User may explicitly choose superpowers:subagent-driven-development instead. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在保留现有手动备份的基础上，增加可验证的在线备份、可选定时与异地备份，并使每次发布可回归、可恢复。

**Architecture:** SQLite 进程内生成一致快照，与有效配置和 manifest 配套；只发布验证成功的归档。应用负责本地在线备份，主机脚本负责恢复和可选加密异地传输。CI 复用既有打包检查并增加协议、迁移和恢复门禁。

**Tech Stack:** Go、SQLite VACUUM INTO、SHA-256、tar/gzip、Python unittest、Bash、Docker/systemd、主机端 age。

---

依赖见[总计划](2026-10-03-civic-relay-hardening.md)：Task1可在首次迁移发布前先实施，Task2/3依赖阶段1/2/4，Task4的全功能发布门禁依赖全部前置阶段。现有 scripts/docker-ops.sh 已有停机备份/恢复，scripts/package_release.py 已有基础 test/gofmt 检查；本阶段扩充并修正它们。自动备份默认关闭，异地地址默认空，本轮不创建实际定时任务、不上传或部署。

一致快照依据：[SQLite VACUUM INTO](https://www.sqlite.org/lang_vacuum.html)；归档加密依据：[age 官方使用说明](https://github.com/FiloSottile/age)。生产进程只持有供应商解密密钥，异地加密的解密私钥由恢复方另存。

## Task 1：修正手动备份/恢复的失败路径

**Files:** Modify `scripts/docker-ops.sh`、`README.md`、`cmd/civic-relay/main.go`；Create `scripts/test_docker_ops.py`、`scripts/backup_archive.py`、`scripts/test_backup_archive.py`、`internal/store/snapshot_check.go`、`internal/store/snapshot_check_test.go`、`cmd/civic-relay/backup_check.go`、`cmd/civic-relay/backup_check_test.go`。

- [ ] **Step 1：用假Docker写故障测试。** Python unittest 在临时 PATH 放置假的 docker/docker.exe，记录 inspect/stop/cp/start 调用，模拟第二次cp失败、启动失败、原容器停止、健康检查失败。断言失败保留非0状态，原运行容器得到恢复启动，原停止容器不会被擅自启动，失败目录不标“备份完成”。测试使用临时目录，不控制用户正在运行的Docker项目。
- [ ] **Step 2：运行现状回归。** `python -m unittest scripts.test_docker_ops -v`。当前 cp 失败会在 set -e 下退出、遗留停止状态；修复后所有故障断言 PASS。`bash -n scripts/docker-ops.sh` 预期无输出、退出0。
- [ ] **Step 3：在子shell内注册状态恢复trap。** 每个备份/恢复事务独立子shell，保存原运行状态，不覆盖整个脚本全局trap。备份状态保护核心为：

```bash
(
  was_running="$("$DOCKER" inspect "$cid" --format '{{.State.Running}}')"
  restore_runtime() {
    result=$?
    trap - EXIT INT TERM
    if [ "$was_running" = true ]; then
      if ! "${DC[@]}" start civic-relay >/dev/null; then
        c_err "恢复原运行状态失败"
        result=1
      fi
    fi
    exit "$result"
  }
  trap restore_runtime EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if [ "$was_running" = true ]; then
    "${DC[@]}" stop civic-relay >/dev/null
  fi
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$cid:/app/config/." "$(native_path "$target/config")"
  MSYS_NO_PATHCONV=1 "$DOCKER" cp "$cid:/app/data/." "$(native_path "$target/data")"
)
```

实际调用 `$DOCKER` 一律双引号包围，避免包含空格的Windows路径。require_container 增加包含停止容器的查询（compose ps -a -q），否则不能验证原停止状态。运行状态恢复成功后，原先running的容器必须等待healthy；trap保证启动尝试，健康失败不能伪成功。

- [ ] **Step 4：先校验再覆盖，并可回退。** backup_archive.py 提供 `verify` 与 `extract` 子命令：检查 manifest、成员白名单、大小上限、SHA、数据库/配置版本；拒绝绝对路径、..、反斜线逃逸、链接、重复成员、设备文件。成员只有 config/relay.env、data/relay.db、manifest.json；旧停机目录允许 data 的配套wal/shm，不能当在线快照直接套用。隔离目录运行数据库检查与密钥解密后才停服务。替换前保存当前完整配置/数据，不把新文件覆盖旧目录残留wal/shm；替换失败或新服务不healthy，恢复原目录并恢复原运行状态，两个失败原因均可见。

恢复保留现有终端确认行为；新增 `--dry-run` 只做隔离校验、`--yes` 用于后续明确授权的无人值守调用。这里仅实现和测试，不能实际恢复生产数据。旧格式无manifest时提供 legacy 校验路径，并明确无法验证原产物hash，不默认跳过解密与一致性检查。

此任务先建立只读验证入口 `store.ValidateSnapshot(ctx context.Context, dbPath string, box *secret.Box) error`，只用mode=ro连接、不调用会迁移数据库的Store.Open；校验schema版本不高于当前、quick_check、foreign_key_check和全部providers.api_key_ciphertext解密，结果只返回稳定码。二进制增加 `-verify-backup-dir <隔离解包目录>`，配置解析/secret.New/ValidateSnapshot成功后输出一行不含秘密的JSON并退出；失败非0，不启动HTTP/维护循环、不修改归档。Python脚本调用该二进制；容器校验用同版本镜像、隔离目录只读挂载，systemd用本地二进制。这样Task1可独立交付，Task2在线备份再复用，不依赖尚未实现的后续入口。

- [ ] **Step 5：验证与检查点。** `python -m unittest scripts.test_docker_ops scripts.test_backup_archive -v`，`bash -n scripts/docker-ops.sh`，`go test -count=1 ./internal/store ./cmd/civic-relay -run 'Test(ValidateSnapshot|BackupCheck)' -v`。建议 `fix: make backup and restore failures recoverable`。纠正脚本帮助和升级文案：跨迁移回滚必须用旧镜像+对应旧数据库备份。

## Task 2：在线快照、有效配置与原子归档

**Files:**

- Create: `internal/store/snapshot.go`、`internal/store/snapshot_test.go`、`internal/backup/service.go`、`internal/backup/service_test.go`、`internal/config/runtime.go`、`internal/httpapi/admin_backup.go`、`internal/httpapi/admin_backup_test.go`。
- Modify: `internal/config/settings.go`、`internal/httpapi/admin.go`、`cmd/civic-relay/main.go`、`internal/httpapi/server.go`。

- [ ] **Step 1：写并发快照和密钥一致性测试。** 临时SQLite持续写100次预留/终态，同时创建快照；每份快照 quick_check=ok、foreign_key_check无行，账本内部一致，供应商密钥可用归档配置解密。模拟取消、磁盘不足、归档中断、文件重名、配置改DB_PATH尚未重启、错误加密密钥；都不能发布“成功”的文件或泄露密钥。
- [ ] **Step 2：实现可参数化的SnapshotTo。** 目标必须为 backup 服务生成的工作目录中新文件，不能接受管理员任意路径。SQL路径通过参数绑定，禁止字符串拼SQL；最低入口为：

```go
func (s *Store) SnapshotTo(ctx context.Context, path string) error {
    if _, err := os.Lstat(path); err == nil {
        return errors.New("snapshot target already exists")
    } else if !errors.Is(err, os.ErrNotExist) {
        return err
    }
    _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path)
    return err
}
```

创建目录0700并排他命名，快照完成后chmod0600。快照独立于其他写事务执行；上游请求继续服务，但SQLite忙时明确失败，不能无限占用。固定默认备份截止120s、一次只允许一份本地备份，不占用公开请求并发Gate。磁盘预检至少 `3*(DB主文件+WAL大小)+64MiB` 可用空间，写入过程中错误仍按失败清理；使用低内存流式压缩，不把数据库读进RAM。

- [ ] **Step 3：区分有效配置和保存配置。** RuntimeSettings 保存启动时必须固定的DB路径、监听、Transport参数、EncryptionKey，加上真正已热生效的字段；配置变更与取快照用同一短锁读取。备份有效relay.env时 DB_PATH 规范为归档布局 data/relay.db，并在manifest保留源路径供恢复部署映射，禁止复制尚未重启的新DB路径/密钥。

管理配置直接修改 RELAY_ENCRYPTION_KEY 会使现有ciphertext无法解密，因此首版明确拒绝不同于运行密钥的变更，错误 `encryption_key_rotation_required`；空值仍按既有规则保留原值。密钥轮换需重加密事务，本计划不提供假“保存后重启即可”行为。此规则在configSave/API/UI测试，管理员密钥的既有热更新另行保持。

定义 `backup.Manifest` 包含 format_version、backup_id、kind(manual/automatic)、created_at_utc、application_version、commit、schema_version、source_db_path、files(path/size/sha256)。不含明文密钥或数据库内容。流程：创建唯一 `.partial/ID`→写快照和有效config→只读连接检查SQLite→用归档config初始化secretbox验证全部供应商密钥可解密→写manifest→生成同卷 `.partial/ID.tar.gz`→Sync→rename到最终目录→记录成功元数据。只对成功归档执行保留策略；取消/失败清理属于该ID的临时目录，不能递归删除用户指定路径。

在线备份复用Task1的ValidateSnapshot和只读检查合同；不使用Python标准库假装验证AES-GCM，不调用Store.Open给备份自动迁移。使用归档有效配置建立secret.Box，检查成功后才生成最终manifest。

- [ ] **Step 4：接管理员异步任务。** `POST /admin/api/backups` 只接受 kind=manual，不接受绝对路径/文件名/远端地址；返回202和backup_id。`GET /admin/api/backups` 与 `GET /admin/api/backups/{id}` 返回queued/running/succeeded/failed、时间、大小、hash、error_code，不返回config/密钥/归档字节。队列容量1，一个运行+一个待运行；满时409 backup_busy。任务使用应用生命周期context，不因管理员刷新页面取消；停机时取消并等待后台退出。启动只清自身过期partial文件；已成功归档不删除。
- [ ] **Step 5：测试与检查点。** `go test -count=1 ./internal/store ./internal/config ./internal/backup ./internal/httpapi -run 'Test(Snapshot|Backup|RuntimeSettings|EncryptionKeyChange)' -v`，再 `go test -count=1 ./...`。预期一致性、权限、异步任务和隐私断言 PASS。建议 `feat: add verified online backups with effective config`。

## Task 3：持久化路径、定时轮换与可选异地传输

**Files:**

- Create: `internal/backup/scheduler.go`、`internal/backup/scheduler_test.go`、`scripts/backup_offsite.py`、`scripts/test_backup_offsite.py`。
- Modify: `internal/config/settings.go`、`internal/config/store.go`、`web/admin.html`、`Dockerfile.relay`、`docker-compose.yml`、`docker-compose.bind.yml`、`civic-relay-go.service`、`deploy/civic-relay.json`、`deploy/README.md`。

- [ ] **Step 1：新增默认关闭的配置。** BACKUP_ENABLED=false、BACKUP_DIR=backups、BACKUP_TIME=03:00、BACKUP_TIMEZONE=Asia/Shanghai、BACKUP_KEEP=7、BACKUP_TIMEOUT=120s；parse/envmap/redacted/bootstrap均覆盖。KEEP只接受1..365，TIME为HH:mm，TIMEZONE必须LoadLocation成功，TIMEOUT正数。镜像只带上海/UTC时区，若允许其他时区则在备份包使用匿名导入time/tzdata嵌入时区数据，不能让本机可解析而scratch镜像失败。目录变更要求重启，时间/启用/保留数量在下一次调度生效。API/UI只展示配置与任务状态，不显示有效relay.env内容。
- [ ] **Step 2：写可控时钟的调度。** 定义 `NextRun(now time.Time, location *time.Location, hour, minute int) time.Time`，选该时区下一次尚未到达的日时间；scheduler通过timer等待并监听配置/停机，不能每秒忙轮询。每日本地日期最多一份成功automatic；从成功manifest重建最近日期防重启重复。失败当天按5min、15min最多两次重试，无每日之外的无限重试；磁盘/密钥错误只记录可见状态，不自动改配置。手动任务不影响当天自动完成判定。清理仅匹配自身manifest.kind=automatic且succeeded的归档，保留最新7，手动归档保留；无法验证manifest的文件不删。

测试 `TestNextRunShanghai`、`TestSchedulerRestartDeduplicates`、`TestAutomaticRetentionPreservesManual`、`TestSchedulerStopsAndCancelsBackup` 覆盖午夜、重启、暂停、两个失败重试、热修改时间；测试只推进假时钟，不真实等一天。`go test -count=1 ./internal/backup ./internal/config -v` 预期 PASS。

- [ ] **Step 3：同步所有运行方式的可写路径。** 路径交付表：

| 运行方式 | 目录与权限 | 必须更新 |
|---|---|---|
| 默认Compose | /app/backups，命名卷 civic-relay-backups，UID/GID65532 | Dockerfile构建阶段mkdir，runtime COPY --chown；Compose声明并挂载第三卷 |
| bind Compose | CIVIC_RELAY_BACKUP_DIR宿主目录→/app/backups | 覆盖文件与安装命令，Linux预建并给65532权限 |
| Deploy插件 | 自有站点档案新增backup卷target=/app/backups | deploy/civic-relay.json与导出合同测试 |
| systemd | /var/lib/civic-relay/backups，civic-relay用户可写 | ReadWritePaths覆盖、配置示例与安装目录 |
| 本地二进制 | 工作目录backups | 自动创建0700；不写用户项目的dist源码归档 |

大快照不落到Compose的16MiB /tmp tmpfs；所有临时备份在BACKUP_DIR同卷，以便原子rename。根文件系统仍只读、非root；备份默认关闭也应保证目录可用。systemd StopTimeout给阶段1取消/结算与备份退出留出30s。Deploy只修改本项目导出档案，不运行 `--sync-deploy` 去覆盖邻项目。

- [ ] **Step 4：实现主机端可选加密传输。** backup_offsite.py 的 `encrypt-send --config <主机配置文件> --archive <已验证归档>` 读取 recipient public key、remote ssh destination、remote directory和身份文件路径。subprocess使用参数数组、无shell=True；所有远端字符串严格校验，只允许配置的主机/绝对目录，不能拼接用户输入成远端shell命令。采用sftp批处理上传 `.partial` 后rename（路径正确转义并拒绝换行），上传前验证归档hash，age加密文件本地0600并计算hash；绝不上传明文。未知SSH主机密钥失败即停止，不禁用主机验证。接收方私钥不进入配置、镜像和仓库。

启动前检查已安装age及recipient有效；没有工具时返回具体失败，不自动安装。配置缺失则不执行异地动作；CLI由运维明确配置后触发，首版不在Go服务内保存远端登录凭据。成功记录原backup_id与密文hash，不打印身份文件内容。`python -m unittest scripts.test_backup_offsite -v` 使用假age/sftp测试失败、特殊路径、明文禁止和hash。单独恢复演练用本地临时age密钥加密/解密夹具，不访问真实远端。

- [ ] **Step 5：容器与浏览器验收。** Docker隔离项目构建，read_only+UID65532+256m下在线备份成功、公开请求正常、重建后备份仍存在；绑定卷和Deploy JSON合同都验证。执行 `python -m unittest scripts.test_export_deploy_plugin scripts.test_package_site_zip scripts.test_backup_offsite -v`。建议 `feat: add backup scheduling retention and deployment volumes`。

## Task 4：CI、恢复演练与发布产物校验

**Files:** Create `.github/workflows/ci.yml`、`scripts/verify_release.py`、`scripts/test_verify_release.py`、`scripts/test_restore_integration.py`；Modify `scripts/package_release.py`、`scripts/package_site_zip.py`、`scripts/export_deploy_plugin.py`、`README.md`。

- [ ] **Step 1：补 Linux CI。** 工作流push/PR都运行；使用go.mod工具链，分开CGO_ENABLED=1竞争检查与CGO_ENABLED=0生产构建，避免静态镜像设置使race不可用。最小工作流骨架：

```yaml
name: CI
on: [push, pull_request]
jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test -count=1 ./...
      - run: go vet ./...
      - run: CGO_ENABLED=1 go test -race -count=1 ./...
      - run: python3 -m unittest discover -s scripts -p 'test_*.py'
      - run: bash -n scripts/docker-ops.sh
      - run: docker build -f Dockerfile.relay --build-arg VERSION=ci -t civic-relay:ci .
```

独立进程协议用例与Docker/Nginx代理验收（阶段2）必须列入工作流，不能因unittest默认发现不到而漏跑。集成缺Docker/age时本地允许明确SKIP；发布CI提供依赖并将这些SKIP当失败，普通纯单元测试无需实际age/远端。执行时核对action官方版本并固定已验证引用。

- [ ] **Step 2：自动隔离恢复演练。** test_restore_integration 使用v12夹具升级到当前版本→生成已验证在线备份→还原到新目录/隔离容器→启动→认证旧Key/App-ID、解密供应商密钥、检查金额/预留→发一个假上游请求。旧备份+旧镜像演练回退，不能用新schema倒喂旧镜像。错误hash、密钥、越界归档和缺文件都应在替换前拒绝；模拟替换失败验证原数据与原运行状态恢复。

`python -m unittest scripts.test_restore_integration -v` 为必须通过的发布检查。旧版本夹具由固定迁移集合建立、只含虚构密钥/数据库，不从生产导出，不依赖本机旧dist产物。

- [ ] **Step 3：对所有产物建立校验合同。** verify_release.py接受 `--release-dir`，读取发布manifest，检查版本/commit/buildDate和SHA-256、镜像OCI标签/二进制版本；源ZIP检查 requiredFiles、路径布局、部署JSON第三卷；插件ZIP检查manifest版本与功能合同。扫描拒绝 relay.env、bootstrap-admin-key.txt、数据库/wal/shm、backups、.codex和私钥；Compose版本.env只允许 CIVIC_RELAY_VERSION/CIVIC_RELAY_COMMIT/CIVIC_RELAY_BUILD_DATE三项。三个打包脚本复用manifest生成与敏感文件排除，不把用户已有脚本改动覆回旧版本。hash清单从实际文件生成，不能只更新标签字符串。

- [ ] **Step 4：执行实际发布命令。** 阶段版本通过后，在隔离产物目录依次执行 `go test -count=1 ./...`、`go vet ./...`、`python -m unittest discover -s scripts -p 'test_*.py'`；按核对后的真实版本构建镜像并运行 `python scripts/package_release.py <实际新版本>`、`python scripts/package_site_zip.py`、`python scripts/export_deploy_plugin.py`。这里的版本参数必须取发布任务确定的值，不把建议版本覆盖已存在文件；不使用 --skip-checks。随后 `python scripts/verify_release.py --release-dir <本次产物绝对目录>` 应退出0并列每种产物验证通过。验证器CLI参数名在test_verify_release中固定。

先以隔离部署验证health/ready、实际构建版本、慢流、取消、工具调用、切换与恢复。生产部署仅在后续发布任务明确范围时执行；生成计划与本地产物不视为已上线。建议检查点 `ci: verify protocols restore drills and release artifacts`。

## 阶段验收

- [ ] 手动备份/恢复失败后原数据和原运行状态可恢复，错误仍返回失败。
- [ ] 在线快照经过SQLite、hash与密钥检查；不中断普通请求、不发布partial。
- [ ] 自动备份默认关闭，启用后上海时间03:00、成功自动份数保留7、手动不误删。
- [ ] 四种部署方式均有持久可写备份目录，distroless/只读根/低内存条件通过。
- [ ] 异地传输仅加密归档，配置与工具不足不会偷偷上传或伪成功。
- [ ] Linux发布门禁、隔离恢复演练、实际产物版本/hash与敏感文件检查通过。
