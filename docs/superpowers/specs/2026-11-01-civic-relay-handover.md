# Civic Relay 分组改造 — 执行状态交接（v0.4.0 已上线，阶段 B 待执行）

日期：2026-11-01
基线：main = `05b7472`，tag `v0.4.0`（commit `e2aed42c`）已部署 waigame 生产
配套文档：设计 spec `docs/superpowers/specs/2026-11-01-civic-relay-group-scoping-design.md`；阶段 B 计划 `docs/superpowers/plans/2026-11-01-civic-relay-group-quota-phase-b.md`

---

## 一、这份文档给谁看

给接手执行的下一次会话（或人）。读完应该能直接从「阶段 B 任务 1」开工，不需要重读对话历史。

三份文档的分工：**spec** 回答「为什么这样设计」与「运维环境是什么样的」；**Phase B 计划** 回答「每个任务每一步改什么」（带完整代码）；**本文档** 回答「现在走到哪了、哪些事已经定死、接手后按什么规矩干」。三处冲突时以本文档的最新状态为准（spec §12 的部分内容已被执行更新，见第六节）。

## 二、进度总览

| 阶段 | 内容 | 状态 |
|---|---|---|
| 阶段 A | 数据模型归位（密钥↔分组一对一，v16/v17） | ✅ **已完成并上线 v0.4.0**（13 提交 + tag，7 任务全部通过「实现→规格审查→质量审查」三轮 + 终审） |
| 阶段 B | 分组额度（v18 加列、判定链换数据源、删全局配置、播种、前端额度 UI） | 📋 **计划已获批并推送（`05b7472`），尚未执行任何一行代码** |
| 阶段 C | 认证与可见性（删匿名读、密钥登录、subject 矩阵、前端三态） | ⬜ 未规划（spec 第五~九节是设计，实施计划待写） |

任务跟踪器里已建好 B1~B4 四个条目（#26~#29），B5~B7 未建——接手者从 B1 开始按计划逐个派发即可。

## 三、代码与部署现状

- 远端 `https://github.com/lefengyao/civic-ai-relay.git`，main 与 origin/main 同步，工作区干净
- **生产：waigame**（`59.44.120.54`，NAT 9045→22），`/opt/civic-ai-relay`，容器 `civic-ai-relay-civic-relay-1`，镜像 `civic-relay:v0.4.0`（7 层 38.4MB）healthy，`/healthz` 报 `v0.4.0 (commit e2aed42c)`
- 服务器树 = git commit 树 + `.env`/`civic-bin`/`DEPLOYED_COMMIT` 三个本地文件；`DEPLOYED_COMMIT` 记录 version/commit/built/binary_sha256
- 镜像清单：`civic-relay-runtime:base`（**绝不能删**）+ `v0.4.0`（在跑）+ `v0.3.9`（保留但**已无回滚能力**——v17 删了它依赖的 `key_groups`）
- 升级备份（v0.4.0 迁移前的完整库）：服务器 `/opt/civic-ai-relay/backups/civic-{data,config}-20261101-023738.tar.gz` + 本机副本 `C:\Users\15610\civic-backups\waigame-pre-v0.4.0-20261101.tar.gz`（3.88MB；内含 relay.db 13,434,880 字节、SQLite 头已验证）。**备份里没有 `-wal`/`-shm` 是正常的**——`compose stop` 优雅停机时 SQLite 已把 WAL checkpoint 进主库
- 阿里云2c2g 上的 civic 已全清（归档在本机 `civic-backups\civic-aliyun-archive-20261031.tar.gz`，含 RELAY_ENCRYPTION_KEY，别外传）

## 四、阶段 A 执行中确立的事实（写代码前必须知道的）

1. **迁移只能向前**：没有 down-migration。v16/v17 已经把账目表落了 `group_id`、删了 `key_groups`。所有涉及额度判定的旧测试（`ledger_test.go`）在阶段 B Task 3 会整体改写——额度来源从 `args.Limits` 换成 `setGroupLimits()` 写分组行，fixture 改 4 值返回（多一个 `group`），13 个调用点机械补位。这是 B3 最大的工作量，计划里已逐个给出改写代码
2. **统计与扣减必须同口径**：`GroupOverview` 在阶段 B Task 4 从「走组内渠道的请求」改成「归属该组的请求」（账目行 `group_id`）。这是 spec 的硬要求——口径不一致会让运维看到「明明还剩额度却被拒」
3. **`QuotaLimits` 不拆字段**（B 计划偏差 #4）：保留全部字段，语义变为「本次请求生效的额度」——`ReserveInput.Limits` 只携带全局 `RPMLimit`，其余九个值在事务内从 `model_groups` 行读出后填进去。窗口判定函数零逻辑改动
4. **播种的幂等性靠两件事**：只填 `IS NULL` 的列（重跑不覆盖运维手工修改）+ 从 relay.env 删键（删键本身就是「已播种」标记）。顺序必须先写库、再写 relay.env
5. **bootstrap-admin-key 会「复活」是正常行为**：`config.Ensure`（`store.go:370-388`）在 relay.env 存在但该文件缺失时，把**现有的** AdminAPIKey 写回文件——不是生成新凭据。启动日志 `first start created…` 措辞有误导，修正已排进 B5（main.go 反正要动）
6. **`PUT keys/{id}/group` 对停用组会 400**（`SetKeyGroup` 校验），所以 B6 里 editKey 改成「只在改绑时才发 PUT」+ 停用的当前组也显示在列表里——否则「保持原样保存」就会报错，或更糟：静默解绑

## 五、阶段 B：怎么执行

计划：`docs/superpowers/plans/2026-11-01-civic-relay-group-quota-phase-b.md`（1648 行，7 任务，每步带完整代码与预期输出）。先读它的**头部**：执行约束、4 条与 spec 的偏差（不要「纠正」回去）、阶段 A 移交两项。

**执行方式**（沿用阶段 A 的既定流程）：每任务派发全新实现子代理（prompt 里贴任务全文+上下文）→ 规格符合性审查子代理 → 代码质量审查子代理 → 有问题打回修复并复审 → 通过才进下一任务。全部完成后做一次整体验收（对照 spec §13 阶段 B 清单 + 权限不变的承诺：`server.go` diff 必须为空、`internal/relay/` 除测试调用点外零改动）。

**每个任务的红线**（计划里都写了，这里是速查）：

| 任务 | 一句话 | 最容易错的地方 |
|---|---|---|
| B1 | v18 给 model_groups 加 10 个可空额度列 | 无回填（全局值在 relay.env 不在库）；别顺手做播种 |
| B2 | ModelGroup/UpdateModelGroup 带 10 个指针字段 | nil=不动 语义必须保住，否则「只改倍率」会清空额度；周期起点自动填 now（settings.go:245 行为的分组版） |
| B3 | 预留事务从分组行读额度 + 窗口查询加 `AND group_id=?` | `PeriodDays` 是 int 要经 int64 中转扫描；`in.Limits.` 的 8 处引用全改 `limits.`；ledger_test 13 处 fixture 调用点补第 4 返回值 |
| B4 | GroupOverview 归属口径+限额；Overview 删窗口块；删组带 unbound_keys；groupsList 紧张度 | Overview 删窗口块后响应少四个键，**前端 loadOverview 还在渲染它们**——B6 会摘掉，B4→B6 之间前端短暂不匹配是已知中间态 |
| B5 | 删全局额度 10 个配置项 + main.go 播种 | configSave 的 PERIOD_LIMIT_START 保留块（admin.go:325-329）要删；settings.go 的 TokenLimits()/AmountLimits()/tokenLimit/amountLimit/nonNegativeInt 变死代码要删；**顺带修 main.go:443 日志措辞（计划遗漏项，见下）** |
| B6 | 前端：概览摘额度卡、组列表紧张标记、组概览 quotaCard、组编辑 10 字段、删组确认、editKey 停用组 | 组编辑的额度语义是「0=不限、留空=保持原值」，label 文案别写反；editKey 只在改绑时 PUT |
| B7 | README/admin-api.md/changelog 改口径 + 全量验证 | `_e2e/run_e2e.py` 额度断言失效是已知欠账，标记不修 |

**一个已发现的计划缺口**：B 计划前言说 main.go:443 日志措辞「Task 6 顺带」，但 Task 6 步骤里没有它——实际应该在 **B5** 做（B5 已经在改 main.go）：把 `first start created bootstrap administrator key file` 拆成两种文案（真正首启 vs `config.Ensure` 的恢复路径，后者可说「已恢复既有管理员凭据的引导文件」）。接手者在 B5 的实现 prompt 里补上这一条。

**发版**：B 全部完成后走 `bash scripts/build_deploy_bundle.sh v0.5.0`，按 `docs/deploy-prebuilt.md` 第 4 节部署——**v18 不是破坏性迁移**（纯加列），但停机备份的流程照走（形成习惯，且 spec 5.4 要求）。

## 六、spec §12 需要更新的条目（以本文为准）

- §12.1 「中间提交不保证单独可编译」的范围扩大到 `2245471..05671ff` 之外：阶段 A 的 13 个提交里**每个都是绿的**（子代理流程保证了），这个警告只适用于 9/29~10/31 那段历史
- §12.2 生产版本 v0.3.9 → **v0.4.0**；镜像三个（见第三节）；部署目录多了 `backups/` 与 `DEPLOYED_COMMIT` 已存在
- §12.5 SSH 仍然敞开（`permitrootlogin yes` + 密码认证 + 96 万次爆破），**整个阶段 A/B 期间都没处理**——接手者不要忘记这件事悬着
- §12.6 本机留存敏感文件新增：`civic-backups\waigame-pre-v0.4.0-20261101.tar.gz`（含完整账目库）

## 七、已知欠账（都排了去处，别新开摊子）

| 欠账 | 去处 |
|---|---|
| `_e2e/run_e2e.py` 39 项断言的额度对照依赖全局配置，v0.5.0 后失效 | B7 在 changelog「已知但尚未实现」记录；待本机装 Python 后改分组口径重跑 |
| `_e2e/run_client_onboarding.py` 的 `group_id` 适配只存在于本地工作区（`_e2e/` 被 gitignore） | 无需动作；换机器要重新改 |
| `TestReapStaleReservationsReleasesQuotaAndConcurrency` 10ms 时序窗偶发抖动 | 基线问题；想根治把 maxAge/sleep 放大，**不要在本阶段顺手改** |
| 编辑弹窗「绑定失败但 Key 已创建」的孤儿 Key 语义 | admin-api.md 已写明（`ae7de6c`）；要不要改成失败即回滚删除，留待老板定夺 |
| main.go:443 日志措辞 | **B5 补做**（见第五节计划缺口） |

## 八、接手后第一件事

1. 读 Phase B 计划头部（约束 + 偏差 + 移交项）
2. `cd /e/生产项目/项目/TrailblazerStation/civic-ai-relay && git log --oneline -3 && git status` 确认基线（应为 `05b7472`、干净）
3. 按第五节的流程从 B1 开始派发；B1 的实现 prompt = 计划 Task 1 全文 + 环境约束（计划头部已写）+ 仓库路径与「直接提交 main、不要 push」的约定
4. 全部任务完成后：整体验收 → `build_deploy_bundle.sh v0.5.0` → 部署 waigame（先问老板）→ 阶段 C 进 brainstorming
