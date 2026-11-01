# Civic Relay 分组改造 — 执行状态交接（v0.6.0 已上线，阶段 A/B/C 全部完成）

日期：2026-11-01
基线：main = `9db9b2a`，tag `v0.6.0`（commit `9db9b2ac`）已部署 waigame 生产；回滚目标 tag `v0.5.0`（`6dbd6d48`，镜像仍在）。阶段 C（认证与可见性）已发版并上生产
配套文档：设计 spec `docs/superpowers/specs/2026-11-01-civic-relay-group-scoping-design.md`；阶段 B 计划 `docs/superpowers/plans/2026-11-01-civic-relay-group-quota-phase-b.md`

---

## 一、这份文档给谁看

给接手执行的下一次会话（或人）。读完应该能直接从「阶段 B 任务 1」开工，不需要重读对话历史。

三份文档的分工：**spec** 回答「为什么这样设计」与「运维环境是什么样的」；**Phase B 计划** 回答「每个任务每一步改什么」（带完整代码）；**本文档** 回答「现在走到哪了、哪些事已经定死、接手后按什么规矩干」。三处冲突时以本文档的最新状态为准（spec §12 的部分内容已被执行更新，见第六节）。

## 二、进度总览

| 阶段 | 内容 | 状态 |
|---|---|---|
| 阶段 A | 数据模型归位（密钥↔分组一对一，v16/v17） | ✅ **已完成并上线 v0.4.0**（13 提交 + tag，7 任务全部通过「实现→规格审查→质量审查」三轮 + 终审） |
| 阶段 B | 分组额度（v18 加列、判定链换数据源、删全局配置、播种、前端额度 UI） | ✅ **已完成并上线 v0.5.0**（B1~B7 七个提交 + tag，2026-11-01 部署 waigame） |
| 阶段 C | 认证与可见性（删匿名读、密钥登录、subject 矩阵、前端三态） | ✅ **已完成并上线 v0.6.0**（1 个功能提交 + tag，2026-11-01 13:11 部署 waigame，见第三节与「二之二」） |

## 二之二、阶段 C 落地记录（2026-11-01）

老板的原话是「纯用户登录网页应该先让他输密钥，输入了密钥才能看到对应分组信息，而目前是谁都能看到；运行概览和用量分析应该全量保留，但可以通过切换分组查看对应分组的信息，而不是必须到模型组查看概览」。据此实施的内容：

**后端**
- 删掉 `isPublicRead` 白名单与「匿名只读视图」，`/admin/api/` 下没有任何匿名路径。新增 `adminSubject{Admin, KeyID, GroupID, GroupName}` + `resolveSubject` + 单一可见性表 `authorizeSubject`（处理函数不再各自判 `isAdmin`）
- 凭证：管理员 `X-Admin-Key`；客户端密钥 `Authorization: Bearer crk_…`（`app_` 应用型标识显式拒绝，返回 `console_login_not_supported`）。四种拒绝码：`authentication_required` / `invalid_api_key` / `token_expired` / `console_login_not_supported`
- `GET /admin/api/session` 语义变更：匿名不再 200，改为「回答你是谁」；新增 `GET /admin/api/me`
- 行级裁剪：`overview`（密钥主体带 `group`/`me`/`scope`，`recent` 只含自己）、`analytics`（`group_id` 过滤）、`providers`（组内渠道且不含 `base_url`）、`models`（与 `/v1/models` 同源的授权口径）、`groups`/`groups/{id}`/`groups/{id}/overview`/`groups/health`（只有自己那组）、`requests`（强制 `key_id`，单条按 `RequestOwner` 判 404）。跨组一律 404
- 新增 store 能力：`RequestFilter.GroupID/KeyID`、`AnalyticsFilter.GroupID`、`RequestOwner`、`GetModelGroup`、`KeyUsageByID`、`groups/{id}` 的 GET

**前端（`web/admin.html`）**
- 打开即是登录页（没有任何匿名可见数据）；一个输入框自动判别：`crk_` 前缀走客户端密钥，其余走管理员密钥；两个 localStorage 键互斥，换主体时清空缓存
- 主体三态用 `body.readonly` + `body.mode-key`：密钥主体看不到「客户端 Key / WASM 插件 / 运行配置」，但保留「请求日志」（后端按 key 裁剪）
- **概览页保留全站数据，另加「统计范围」下拉**（全部分组 / 某个组）。选中分组后展示该组四个窗口的额度卡片、本组 RPM、进行中与成功率，最近请求也换成该组的；密钥主体看到的是固定显示的本组 + 「我的用量（这把密钥）」区块
- 用量分析筛选条件里加「分组」下拉（管理员），密钥主体由服务端锁定

**与设计 spec 的偏差（老板本轮明确要求）**：spec §10.3 原本定「管理员概览去掉额度卡片，分组额度只看模型组弹窗」；现在改为**概览页内置分组切换器**，模型组弹窗的概览入口保留不变。两处口径一致（都走 `GroupOverview`）。

**测试**：`internal/httpapi/visibility_test.go` 新增可见性矩阵 table-driven（33 条：匿名 401、密钥本组 200、跨组 404、写操作 404）、四种登录拒绝码分开断言、过期令牌返回 `token_expired`、密钥主体只看到本组渠道且 `base_url` 为空、管理员概览「全站 + group 块」并存。`go build` / `go vet` / `go test ./...` 11 个包全绿；另用 Playwright（本机 chromium）跑了两轮真实浏览器验证（登录门禁、分组切换、密钥主体只读视图、组概览弹窗、请求日志），24 项断言全过，无 4xx 噪点。

**发布（2026-11-01 已完成）**：tag `v0.6.0`（`9db9b2ac`）→ `bash scripts/build_deploy_bundle.sh v0.6.0` → 按 `2026-11-01-v0.5.0-deploy-handover.md` 同一套流程上 waigame。**本次无数据库迁移**（schema 仍 v18），回滚只需把 `.env` 的 `CIVIC_RELAY_VERSION` 改回 `v0.5.0` 再 `compose up -d`。

任务跟踪器里已建好 B1~B4 四个条目（#26~#29），B5~B7 未建——接手者从 B1 开始按计划逐个派发即可。

## 三、代码与部署现状

- 远端 `https://github.com/lefengyao/civic-ai-relay.git`，main 与 origin/main 同步，工作区干净
- **生产：waigame**（`59.44.120.54`，NAT 9045→22），`/opt/civic-ai-relay`，容器 `civic-ai-relay-civic-relay-1`，镜像 `civic-relay:v0.6.0` healthy，`/healthz` 报 `v0.6.0`（commit `9db9b2ac`）
- 对外入口：`https://civic.fengyao.xin` → 宝塔 Nginx → `127.0.0.1:8000`（容器内 8000）。Nginx 并未显式 `proxy_buffering off`，流式靠应用侧 `X-Accel-Buffering: no` 关闭缓冲；`client_max_body_size` 走全局 50m，`proxy_read_timeout 600s`
- 服务器树 = git commit 树 + `.env`/`civic-bin`/`DEPLOYED_COMMIT` 三个本地文件；`DEPLOYED_COMMIT` 记录 version/commit/built/binary_sha256
- 镜像清单：`civic-relay-runtime:base`（**绝不能删**）+ `v0.6.0`（在跑）+ `v0.5.0`（**保留作回滚**：v0.6.0 无迁移，旧二进制直接跑）+ `v0.4.0`（保留）+ `v0.3.9`（保留但**已无回滚能力**——v17 删了它依赖的 `key_groups`）
- v0.5.0 升级备份（停机后一致备份）：服务器 `/opt/civic-ai-relay/backups/civic-{data,config}-20261101-040557.tar.gz` + 本机副本 `C:\Users\15610\civic-backups\civic-{data,config}-20261101-040557.tar.gz`（3.9MB / 649B，两端 sha256 已对齐：data `e1585ef1…`、config `c4919398…`）
- **v0.5.0 部署实测（2026-11-01 12:06）**：`全局额度配置已灌入 1 个分组并从 relay.env 移除` 如期出现；`福利` 组 `TokenLimitPeriod=920000000`、周期起点 `2026-10-28T07:54:41Z` 原样保留，组概览 `used_tokens=809956034` 与升级前全局周期用量完全一致（**额度不是从 0 重新计**）；relay.env 已清掉全部 `TOKEN_LIMIT_*`/`AMOUNT_LIMIT_*`/`PERIOD_LIMIT_*`，`ADMIN_API_KEY` 与 `RELAY_ENCRYPTION_KEY` 保留；`healthz/readyz/admin` 全 200，日志无 ERROR/WARN；经 Nginx 路径（`--resolve` 到 127.0.0.1:443）核验同一版本
- **分组额度判定实测**：临时组（日额度压到 1）调用返回 `429 token_quota_exceeded`，消息含「分组「__verify_tmp_429」#2」；临时 Key/组已删除，生产只剩 `福利` 组与 1 把 Key，用量未变
- v0.6.0 升级备份（停机后一致备份）：服务器 `/opt/civic-ai-relay/backups/civic-{data,config}-20261101-051103.tar.gz` + 本机副本 `C:\Users\15610\civic-backups\civic-{data,config}-20261101-051103.tar.gz`（4.2MB / 574B，两端 sha256 已对齐：data `fa84dc6c…`、config `6a85125a…`；data 卷内为 `relay.db` 14,704,640 字节，无 `-wal`/`-shm`）
- **v0.6.0 部署实测（2026-11-01 13:11）**：`healthz` 报 `v0.6.0 (commit 9db9b2ac)`、`readyz`/`admin` 全 200、日志 0 条 ERROR；**匿名 `/admin/api/{overview,session,groups,analytics,providers}` 全部 401 `authentication_required`**（升级前是 200）；管理员 `X-Admin-Key` 全部 200；客户端密钥主体 `session` 返回 `{"admin":false,"scope":{…}}`、本组 200 / 跨组 404、`keys`/`config`/`plugins` 404、写操作 404、渠道列表 `BaseURL` 为空；该密钥实调 `/v1/chat/completions` 200，请求日志只回自己的；周期用量 809,987,971（升级前 809,956,034 + 验证调用），**额度仍从原处继续计**；经 Nginx（`--resolve` 到 127.0.0.1:443）核验同一版本、匿名 api 也是 401
- ⚠️ **v0.6.0 已知小问题（管理员侧，非安全）**：`GET /admin/api/overview?group_id=<不存在的组>` 返回 200 且 `group` 块全 0、`group_name` 为空，而不是 404。前端下拉只列真实分组，正常操作碰不到；若要修，在 `overview` 里判 `group_name == ""` 后回 404
- ⚠️ **周期额度不是硬停**：自定义周期用满后不返回 429，而是由 `periodQuotaTransition` 把「本周」窗口起点挪到用满那一刻（`transition_utc`），即自动滚动进入下一周期。想让周期额度真正封顶，必须同时配「本周」Token/金额额度（当前 `福利` 组本周为不限）
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
- §12.2 生产版本 v0.3.9 → v0.4.0 → v0.5.0 → **v0.6.0**（2026-11-01 13:11）；镜像五个（见第三节）；部署目录多了 `backups/`、`DEPLOYED_COMMIT` 与 `.env.bak-*`
- §12.5 SSH 加固**已做一半**（2026-11-01 12:20）：**fail2ban 0.11.2 已装并开机自启**（sshd jail：10 分钟 5 次失败封 1 小时，屡犯每次翻倍、上限 7 天）；头号爆破源 `195.178.110.220`（TECHOFF SRV LIMITED，每分钟 399 次字典爆破）已用 ufw 永久封禁；`PubkeyAuthentication` 已打开、密钥 `~/.ssh/civic_waigame_ed25519` 已写入服务器 `authorized_keys`（回环 `KEY_LOGIN_OK` 验证通过）。**`PasswordAuthentication` 仍为 yes**——老板选择过渡，确认密钥连得稳之后再关，关掉后爆破才彻底归零
- §12.6 本机留存敏感文件新增：`civic-backups\waigame-pre-v0.4.0-20261101.tar.gz`、`civic-backups\civic-{data,config}-20261101-040557.tar.gz`、`civic-backups\civic-{data,config}-20261101-051103.tar.gz`（含完整账目库与 `RELAY_ENCRYPTION_KEY`，别外传）

## 七、已知欠账（都排了去处，别新开摊子）

| 欠账 | 去处 |
|---|---|
| `_e2e/run_e2e.py` 39 项断言的额度对照依赖全局配置，v0.5.0 后失效 | B7 在 changelog「已知但尚未实现」记录；待本机装 Python 后改分组口径重跑 |
| `_e2e/run_client_onboarding.py` 的 `group_id` 适配只存在于本地工作区（`_e2e/` 被 gitignore） | 无需动作；换机器要重新改 |
| `TestReapStaleReservationsReleasesQuotaAndConcurrency` 10ms 时序窗偶发抖动 | 基线问题；想根治把 maxAge/sleep 放大，**不要在本阶段顺手改** |
| 编辑弹窗「绑定失败但 Key 已创建」的孤儿 Key 语义 | admin-api.md 已写明（`ae7de6c`）；要不要改成失败即回滚删除，留待老板定夺 |
| main.go:443 日志措辞 | **B5 补做**（见第五节计划缺口） |

## 八、接手后第一件事

1. 读本文第三节「v0.6.0 部署实测」：生产已是 v0.6.0，控制台无匿名路径，密钥主体按分组只读；`福利` 组周期额度 920M / 已用 809.99M（88%）
2. `git log --oneline -3 && git status` 确认基线（应为 `9db9b2a` 之后的提交、干净）。**远端仍未 push**（阶段 A/B/C 的提交都在本地），推之前先确认老板意愿
3. 阶段 C 已发版，**没有下一阶段排期**。候选：spec 里剩下的「三态前端打磨」、§10.3 与实现的偏差已按老板要求改完无需回收
4. 另外三件悬留项：SSH 关密码认证（公钥已验证可登，只剩把 `PasswordAuthentication` 改 no）；`_e2e/run_e2e.py` 额度断言按分组口径重写；`overview?group_id=<不存在>` 的回 200 小问题（见第三节）
