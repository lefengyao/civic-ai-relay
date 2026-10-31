# Civic Relay 分组权限与额度分片改造 — 设计与交接

日期：2026-11-01
状态：设计已获批，未开始实施
基线：`v0.3.9`（commit `e476f2e2`），生产运行于 waigame

---

## 一、这份文档给谁看

给接手实施的人（或下一次 agent 会话）。读完应该能直接开工，不需要再问背景。

前半部分（二~四节）是**现状与决策**，回答「为什么是这样定的」；中间（五~十节）是**设计**，回答「具体改什么」；十一节是测试计划，十二节是交接所需的运维上下文——本机与服务器的环境约束踩过一次坑，不写下来下次还会踩；十三节是实施分期，**开工前先看它**。

---

## 二、需求原话与解读

老板的原话：

> 首先，登录管理员可以操控，编辑一切。然后如果某个用户想要查看某一次分发的总用量，使用分发出去的密钥登录，登录后可以看到这个密钥所属分组的相关信息，也就是目前直接公开的信息。目前我们的项目缺乏分片功能，所以的额度编辑什么的都属于一起。后面要改成供应商共通，但是通过密钥登录只能看到其分组添加的供应商，每个分组单独分配额度信息，用量等。

拆成四条：

1. 管理员登录后可以操控、编辑一切（现状已满足，不改）
2. 用分发出去的**客户端密钥**登录控制台，登录后看到该密钥所属分组的信息——也就是现在匿名就能看到的那些内容，但按分组切开
3. 供应商（渠道）全局共通，但密钥登录只能看到其分组内挂的供应商
4. 额度从「全局一套」改成「每个分组单独分配」，用量同样按分组统计

关键背景：老板在澄清时明确「**一个密钥只对应一个分组，但可以切换分组**」。而当前实现允许一个密钥挂多个分组（证据见第三节），所以这条是**改造目标**，不是现状描述。

---

## 三、现状（已逐项核实，带行号）

### 3.1 数据模型（`internal/store/schema.go`，15 个迁移）

- `model_groups`：只有 `name`、`enabled`、`rate_milli`（v3 加倍率）。**没有任何额度字段**
- `key_groups(key_id, group_id)`：联合主键，**多对多**
- `group_providers(group_id, provider_id)`：联合主键，多对多。v8 把「组管理模型」改成「组管理渠道」
- `client_keys`：`token_limit`、`amount_limit_microyuan`（**总量，无窗口**）、`concurrency_limit`、`expires_at_utc`（v11 短时令牌）、`public_id`（v12 应用型凭据）
- `key_reservations` / `requests`：都有 `key_id`、`model_id`、`provider_id`，**都没有 `group_id`**

### 3.2 额度判定（`internal/store/ledger.go`）

判定链在预留事务内，顺序是：

| 步骤 | 位置 | 作用域 |
|---|---|---|
| 读 `client_keys`（enabled/并发/两个总量限额） | :320 | Key |
| 并发闸门（活跃预留数 vs `concurrency_limit`） | :327-332 | Key |
| 计价查询（JOIN `key_groups`，取 `MAX(g.rate_milli)`） | :334 | 多组取最大 |
| RPM | :358 | **全局** |
| 5 小时滚动窗口 | :386 | **全局**（无 `key_id` 过滤） |
| 当日窗口（`billing_date_bj`） | :395 | **全局** |
| 自定义周期 | :404-418 调 `periodQuotaTransition`（:238） | **全局** |
| 本周窗口 | :419-430 | **全局** |
| Key 总量 | :434-446（`WHERE key_id=?`，无窗口） | Key |
| 写 `requests` + `key_reservations` | :447/:455 | — |

判定口径是「**先欠费后停服**」：只看窗口是否已用满（`used >= limit`），不要求本次预留塞得下。`limit <= 0` 表示不限。拒绝一律 429。

`periodQuotaTransition`（:238-271）值得单独记：它用窗口函数找出「周期额度在哪一刻被用满」，返回那个时刻，调用方据此把周限额的起算点移到该时刻（实现「周期结束后周限额从周期结束时刻开始统计」）。函数内有**三条**聚合查询，全都没有分组过滤。

`QuotaLimits` 结构体（:17）同时装 `RPMLimit` 和 8 个额度值，由 `internal/relay/service.go:379` 从 `config.Settings` 填。

`GroupOverview`（:568）已存在，但注释写明它统计的是「**走组内渠道的请求**」——共享渠道会被重复计入多个组。它只做展示，不参与判定。

### 3.3 认证与授权（`internal/httpapi/`）

- 管理员：`adminAuthorized`（`admin.go:198`）对 `X-Admin-Key` 做定长常数时间比较；`requireAdmin`（:205）失败返回 401 `authentication_required`
- `GET /admin/api/session`（:79-82）**免认证**，只返回 `{admin: bool}`
- `isPublicRead`（:185-196）白名单放行**匿名 GET**：`overview`、`analytics`、`providers`、`models`、`groups`、`groups/health`、`providers/*`（`remote-models` 除外，因为它会带上游密钥请求上游）、`models/*`、`groups/*`
- 门禁在 :85-89：`if !(isReadMethod && isPublicRead(path)) { requireAdmin }`
- `server.go:113` 的 `readOnlyRequest` **不是认证**，只是健康探针的 GET/HEAD 方法闸门，别看名字误会
- 上游地址隐藏：`admin.go:361` 起，只读主体拿不到 `base_url`
- CORS：`cors.go:22` 注释写明「认证走 `Authorization` / `X-Admin-Key` / `X-App-Id` 请求头而非 Cookie」，:54 的 `Access-Control-Allow-Headers` 已包含 `Authorization`、`X-Admin-Key`、`X-App-Id`。**未开 `Allow-Credentials`**
- 客户端密钥认证在公共接口侧：`store.AuthenticateClientKey` / `AuthenticateClientApp`，过期返回 `store.ErrClientKeyExpired` → 401 `token_expired`（与 `invalid_api_key` 分开）

### 3.4 授权与计价 SQL（`internal/store/keys.go`）

- `authorizedModelsSelect`（:533）是「客户端能用哪些模型」的**唯一口径**，公共 `/v1/models` 与管理端「接入信息」共用。链路：`client_keys` → `key_groups` → `model_groups(enabled=1)` → `group_providers` → `models(enabled=1 且已定价)` → `providers(enabled=1)`
- `KeyModelRateMilli`（:495-508）：`SELECT MAX(g.rate_milli)`，注释写明「the highest rate among the key's enabled groups whose providers include the model's provider」
- `KeyGroupIDs`（:426）返回 `[]int64`；`ReplaceKeyGroups`（:447）先 `DELETE` 再循环 `INSERT`，校验组必须存在且启用（否则 `group %d is disabled`），传空切片即解绑
- `ClientKey` 结构体（:20-39）无分组字段

### 3.5 前端（`web/admin.html`，1835 行，`web/embed.go` 单文件内嵌）

- 主体状态：`adminKey`（:459，localStorage 键 `cr_admin_key`）+ `isAdmin` 布尔（:460）
- `api()`（:477-483）**无条件** `headers.set("X-Admin-Key", adminKey)`（空串也设）；401 时若 `isAdmin` 则 `logout()`
- `PUBLIC_VIEWS = {overview, analytics, providers, groups}`（:492）
- `applyAuth()`（:494-500）切 `body.readonly`（CSS :33 `body.readonly .admin-only{display:none!important}`）与登录按钮
- `login()`（:533-549）有个缺陷：`if (!r.ok) throw new Error("请求失败 ("+r.status+")")` **丢掉了错误体里的 code**
- 导航（:273-280）：`overview`/`analytics`/`providers`/`groups` 无标记，`keys`/`requests`/`plugins`/`settings` 带 `.admin-only`
- `quotaCard(label, hint, w, scope)`（:603-627）：payload 形状 `{used_tokens, limit, used_microyuan, amount_limit}`；`limit=0` 渲染「不限」；`p>=80` 标黄、`p>=100` 标红并提示已欠费；刷新按钮条件是 `scope && isAdmin`（:621）
- `loadOverview()`（:628-645）渲染 RPM、5 小时、当日、自定义周期、本周、并发、近一小时成功率，末尾 `renderRequests(d.recent, "overview-recent")`（:644）
- `checkListHTML`（:907-911）生成 `type="checkbox"`，**只有两个调用点**：:1291（组内渠道，真多选，`group_providers` 靠它维护）与 :1545（密钥的授权模型组）
- `checkedIds`（:912）两个调用点：:1297（`provider_ids`）与 :1556（`group_ids`）
- `renderKeyGroupChecks`（:1342-1346）直接生成 checkbox，不经 `checkListHTML`
- 创建密钥 body（:1567）：`group_ids: [...querySelectorAll("input:checked")]`
- 密钥编辑弹窗读 `keys/{id}/groups`（:1531-1532）
- 上游地址列按 `isAdmin` 隐藏（:881-882）；约 15 处 `isAdmin ?` 控制操作列

### 3.6 现状里与需求冲突的两点

1. **匿名可读全局**：任何人不用登录就能看到所有供应商、所有分组、全局概览与用量分析。要做分组隔离，这层必须收掉
2. **一个密钥可挂多个分组**：前端两处都是 checkbox（:1344 与经 :909 的 :1545），API 收 `group_ids []int64`（`admin.go:800`），存储层 DELETE + 循环 INSERT。老板期望的一对一**不是现状**

---

## 四、决策记录

十条，都是老板明确选定的，不要自行改动：

| # | 议题 | 决定 |
|---|---|---|
| 1 | 匿名只读视图 | **收紧**：取消匿名读，看任何信息都要登录 |
| 2 | 密钥↔分组基数 | **一对一**（一个密钥只属一个分组，可切换） |
| 3 | 多组命中的选组规则 | **不需要**——一对一之后归属天然唯一 |
| 4 | 密钥登录可见范围 | **分组汇总 + 自己明细**；看不到同组其他密钥的逐个数据 |
| 5 | 额度层级 | **分组额度取代全局额度**；Key 级总量额度保留 → 变成「分组 + Key」两层 |
| 6 | 分组额度维度 | **完整沿用**现有全局模型：4 窗口 × Token/金额 + 自定义周期 |
| 7 | 登录凭证 | **只允许密钥型 `crk_`**；应用型 `app_` 拒绝（其明文创建时即丢弃，无可验证物） |
| 8 | 会话机制 | **无会话**，每个请求带凭证请求头（与现有 `X-Admin-Key` 同构） |
| 9 | 升级时全局额度值 | **搬进每个现有分组**，然后从 `Settings` 删除这些配置项 |
| 10 | 实现方案 | **方案二「模型归位」**：删 `key_groups` 表，改用 `client_keys.group_id` |

关于决策 10 的取舍记录：我原本推荐方案一（保留 `key_groups` 加 `UNIQUE(key_id)`，不动那三处核心 SQL），理由是风险最低。老板选了方案二。方案二要重写授权、计价、预留三处最敏感的 SQL，出问题的表现是「客户端拉不到模型」或「账单金额算错」，都不容易在测试里穷尽——**实施时这三处必须有对应的回归测试先写好**。

另有一条我提出、老板未反对的默认处理：`RPM_LIMIT`、全局并发闸门、`MEMORY_LIMIT_MB`、输出上限**不是额度**，是保护性闸门，保留在全局。

### 已知但未实现（本轮不做）

- **控制台登录失败限流**：取消匿名读之后 `/admin/api/session` 成为可爆破的登录端点，而 `/admin/` 不经过 relay 的 RPM 与并发闸门。本轮不做，理由与项目对应用型凭据的既有立场一致——真正的门是「谁能访问到网关」。若将来要做，注意别把管理员自己锁在外面
- **分组级并发限额**：只有 Key 级 `concurrency_limit` 与全局闸门，分组不单独限并发
- **客户端自助换令牌**：既有遗留项，本次不涉及

---

## 五、数据模型与迁移

### 5.1 表结构变更

**`client_keys.group_id`**：`INTEGER REFERENCES model_groups(id) ON DELETE SET NULL`，可空。

可空是必须的——现有「接入信息」接口已经把「未绑分组」列为一种 issue，删组时密钥要保留并变成未绑，而不是级联删掉。

外键确实会生效：`db.go:46` 的 DSN 带 `_pragma=foreign_keys(ON)`，:62 又显式 `PRAGMA foreign_keys=ON` 设了一次。

**但这里有一个一对一之后才出现的新陷阱，必须处理**：`DeleteModelGroup`（`models.go:202-216`）现在靠 `key_groups` 的 `ON DELETE CASCADE` 清理授权，注释写的是「memberships and key grants cascade」。多对多时代删组只是让密钥少一个组；**一对一之后删组会把该组下所有密钥变成未绑分组，它们的授权模型立刻变空、无法调用任何模型**。删除操作的爆炸半径被基数变化放大了。

处理方式：保留 `SET NULL` 语义（密钥不被删、历史账目保留、接入信息里能看到「未绑分组」这条 issue），但**管理台删除分组前必须先告知影响面**——确认框里写明「将解绑 N 个客户端密钥，它们会立即无法调用」，N 从后端取。API 侧 `DELETE /admin/api/groups/{id}` 的响应里也带上这个数。

> 备选方案是「仍有密钥绑定时拒绝删除，要求先改绑」，与 `ReplaceGroupProviders` 拒绝停用渠道的先例一致。没选它是因为项目对四类资源的既有立场是「均支持删除、级联清理、历史账目保留」，改成拒绝删除会破坏这个一致性；用告知 + 确认来兜底，代价更小。

**`key_reservations.group_id` 与 `requests.group_id`**：`INTEGER NOT NULL DEFAULT 0`，预留时从密钥当时的分组写入。

这一列是本次改造的关键决定。额度窗口聚合必须按它过滤，**不能 JOIN `client_keys` 推导**：老板明确要求「可以切换分组」，若归属靠 JOIN 推导，密钥一换组，历史用量就整体搬到新组——旧组已用量凭空减少、新组凭空增加，额度判定被篡改。

**`model_groups` 加 10 列**，全部可空，NULL 或 0 = 不限（沿用项目既有口径）：

```
token_limit_5h          amount_limit_5h
token_limit_daily       amount_limit_daily
token_limit_weekly      amount_limit_weekly
period_limit_days       token_limit_period    amount_limit_period
period_limit_start      -- TEXT，RFC3339
```

> 决策 6 说的是「每组 8 个数」（4 窗口 × 2 指标），但自定义周期还需要 `period_limit_days` 与 `period_limit_start` 才能定义，所以实际是 9 个数值列 + 1 个时间列。

**删除 `key_groups`**：`DROP INDEX idx_key_groups_group` + `DROP TABLE key_groups`。

**新索引**（性能必需，不是可选优化）：

```
idx_key_reservations_group_time (group_id, created_at_utc)
idx_requests_group_time         (group_id, created_at_utc)
```

额度判定在**每次请求的事务里**跑 4~6 条聚合查询，全部变成按 `group_id` + 时间范围过滤。没有这两个索引就是每次请求全表扫。

### 5.2 migration v16

走现有 `applySchema`（`schema.go:242`）的事务机制，与既有 15 个迁移同格式：

1. `ALTER TABLE client_keys ADD COLUMN group_id ...`
2. 回填 `client_keys.group_id`：相关子查询 `ORDER BY g.rate_milli DESC, g.id ASC LIMIT 1`
3. `ALTER TABLE key_reservations / requests ADD COLUMN group_id INTEGER NOT NULL DEFAULT 0`
4. 两张账目表按 `client_keys.group_id` 回填
5. `ALTER TABLE model_groups ADD COLUMN` × 10
6. `DROP INDEX` + `DROP TABLE key_groups`
7. 建两个新索引

**第 2 步的回填规则为什么是「倍率最大、同倍率取 id 最小」**：这正是现有 `MAX(g.rate_milli)` 的计价口径。曾经属于多个组的密钥，迁移后落在倍率最大的那个组上，**计费倍率保持不变**。选「id 最小」就会悄悄改变这些密钥的账单。

**第 4 步要在迁移注释里写明**：对曾属于多个组的密钥，历史账目只能按迁移后的单一归属回填，是近似值。项目既有迁移（v8/v9/v12）都是这种长注释风格，照抄。

### 5.3 全局额度值播种（Go 侧，不是 SQL）

**坑**：全局额度值在 `relay.env`（配置）里，**不在数据库**，所以「搬进每个现有分组」没法用 SQL 迁移完成。

- 时机：`applySchema` 之后、开始接受请求之前
- 动作：读 relay.env 里的全局额度值 → 写进所有现有分组的额度列 → 成功后**原子写回 relay.env 删掉这些键**
- **幂等靠「只填 `IS NULL` 的列」**：若写库成功但写 relay.env 失败，下次启动重跑时列已非 NULL → 跳过 → 再删键。重复执行无副作用，也不会覆盖运维在此期间对分组额度的修改
- 不需要额外标记表：「relay.env 里还有没有这些键」本身就是标记
- 顺序必须是先写库、再写 relay.env；任一步失败就不动 relay.env

涉及的配置项，**共 10 个键**（`settings.go:198-243` 解析、:579-588 的 EnvMap 回写）：

```
TOKEN_LIMIT_5H     TOKEN_LIMIT_DAILY    TOKEN_LIMIT_WEEKLY
AMOUNT_LIMIT_5H    AMOUNT_LIMIT_DAILY   AMOUNT_LIMIT_WEEKLY
PERIOD_LIMIT_DAYS  PERIOD_LIMIT_START
TOKEN_LIMIT_PERIOD AMOUNT_LIMIT_PERIOD
```

三处都要清：`Settings` 结构体字段与解析、EnvMap（10 个键全在里面，删掉后**运行配置页那一整块会自动消失**，前端不用改，但 `cfgLabels` 里的中英说明变成死条目要清）、以及 **`.env.example:25-34`**——注意它带着非零默认值 `TOKEN_LIMIT_5H=100000` 与 `TOKEN_LIMIT_DAILY=20000`，不删的话新部署仍会被引导出一套已经不生效的限额。

**必须复制到分组层的一个行为**：`settings.go:245-246` 有「`PeriodLimitDays > 0` 且 `PeriodLimitStart` 为空时，自动把起点填成当前时刻」。分组版必须同样处理——否则运维只填了周期天数、没填起点，`periodQuotaTransition`（`ledger.go:239`）会因为 `limits.PeriodStart.IsZero()` 直接返回零值，**周期额度静默不生效**，而界面上看不出任何异常。

删除方式沿用项目既有先例（`UPSTREAM_BASE_URL` / `UPSTREAM_API_KEY` / `DOCS_ENABLED`）：从代码里删掉，旧 relay.env 的残留行按未知键保留、不再读取。但这里多一步——因为值被消费掉了，要主动删键，否则每次启动都会重新播种。

### 5.4 回滚

**v16 是破坏性的**（`DROP TABLE key_groups`），项目没有 down-migration 机制。回到 v0.3.9 只能恢复数据库备份——旧代码的授权查询依赖 `key_groups`，表没了会直接失败。

所以**升级前必须停机做一致备份**（SQLite 在 WAL 模式下要连 `-wal`/`-shm` 一起拷，或先停容器），与 2026-10-18 那次 v0.3.2→v0.3.8 的做法相同。这条要写进 `docs/deploy-prebuilt.md`。

---

## 六、认证与主体

**删掉 `isPublicRead` 整个白名单**（`admin.go:185-196`）。匿名 GET 不再有任何放行路径。

**主体模型**：

```go
type subject struct {
    admin   bool
    keyID   int64  // 仅密钥主体
    groupID int64  // 0 = 不受限（管理员）
}
```

**凭证请求头**：客户端密钥复用公共接口已有的 `Authorization: Bearer crk_...`，管理员继续用 `X-Admin-Key`。

选它的实际理由：这两个头**都已在 `cors.go:54` 的 `Access-Control-Allow-Headers` 里**，CORS 一行不用改。换成新的 `X-Client-Key` 就得同步改 CORS 和它的测试。

**`GET /admin/api/session` 语义变更**：现在免认证返回 `{admin: bool}`，改成「回答你是谁」——凭证有效则 200 返回 `{admin, scope:{key_id, group_id, group_name}}`，无效或缺失则 401。它仍不需要预先登录（它本身就是登录探针），但不再对匿名返回 200。

**四种拒绝必须分开**，沿用项目「让客户端能区分该换令牌还是凭据不对」的既有口径：

| 情况 | 响应 |
|---|---|
| 密钥已过期（v11 TTL） | 401 `token_expired` |
| 密钥停用 / 摘要不匹配 | 401 `invalid_api_key` |
| 应用型凭据 `app_` 来登录 | 401 `console_login_not_supported` |
| 管理员密钥不对 | 401 `authentication_required` |

第三条要**显式拦**：`X-App-Id` 在公共接口是合法认证方式，但控制台绝不能接受它——`app_` 标识是公开的，接受它等于把分组用量对任何人开放。

**未绑分组的密钥**：登录成功，`scope.group_id` 为 null，前端显示「该密钥未绑定分组，无可查看内容」。比直接 403 有用——持有者能自己看出是配置问题。

**新增 `GET /admin/api/me`**：返回调用者自己这把密钥的明细（用量、请求数、有效期剩余、可用模型数）。分组汇总走已有的 group 作用域端点，自己明细走这个，不混在一起。

**鉴权集中在一张表**：`authorize(path, method, subject)` 一处判定，不在 30 个处理函数里散着写 `if subject.admin`。处理函数只负责把 `groupID` 传进 repo 查询（0 = 不过滤）。

---

## 七、额度判定

**判定链从三层变两层**：删掉全局窗口层，插入分组窗口层（同样 4 窗口 × Token/金额），Key 总量层原样保留。RPM、全局并发、Key 并发、`MEMORY_LIMIT_MB`、输出上限继续留在全局。

**`QuotaLimits` 要拆**（`ledger.go:17`）。额度值改为**在事务内从 `model_groups` 行读**：第 320 行那条读 `client_keys` 的查询扩成 `LEFT JOIN model_groups`，一次拿到 `group_id` + 10 个新列（9 个额度值 + 周期起点）。`QuotaLimits` 只剩 `RPMLimit`，`service.go:379` 的填充跟着简化。

必须在事务内读、不能从配置传，两个理由：额度判定与预留写入在同一个事务里，而配置是进程级快照，热更新时会出现「判定用旧限额、写入在新事务」的错位；而且每组额度不同，本来就没法用一个全局结构体传。

**四个窗口查询各加 `AND group_id=?`**，用新的 `key_reservations.group_id` 列：

- 5h 滚动（:386）
- 当日 `billing_date_bj`（:395）
- 每周 `weekStart` 起（:419）
- `periodQuotaTransition` 里的**三条**（:245、:251，含那条窗口函数）

`periodQuotaTransition` 最容易漏改：少加一处过滤，周期切换点就会算到别的组的账上。

**错误码不变**：仍是 `token_quota_exceeded` / `amount_quota_exceeded`，Scope 仍是窗口名（`5h`/`daily`/`weekly`/`period`）。额度归属从全局变成分组，但「你超了某个窗口的额度」这个语义没变，客户端已有分支不用改——README 对外文档化了这些码，改字符串是破坏性变更。分组名与组 ID 加进错误 detail，运维才知道是哪个组咬住的。`key_token_quota_exceeded` / `key_amount_quota_exceeded` 原样不动。

**`GroupOverview` 口径必须改**（`ledger.go:568`）：从「走组内渠道的请求」改成「`group_id` 等于该组的请求」。这不是顺手清理——**统计口径必须与扣减口径一致**，否则管理台显示的「已用/限额」和实际判定用的数字对不上，运维会看到「明明还剩额度却被拒」，正是 v9 迁移当初要消灭的那类问题。

**未绑分组 / 分组停用**：用 `LEFT JOIN`（否则密钥行都读不到，`enabled` 检查失效），扫描后按现有顺序判：先 `enabled != 1` → `client key is disabled`，再并发，**然后** `groupID == 0` → 返回现有的 `model is not authorized for key`。顺序不能乱，否则「停用且未绑组」的密钥会报成授权问题。

**判定顺序保持不变**：5h → 当日 → 周期 → 每周 → Key 总量。两个额度同时超限时客户端拿到哪个码，与现在一致。

---

## 八、授权与计价

三处 SQL 改写，**这是方案二风险最集中的地方，建议先写回归测试再改**。

### 8.1 `authorizedModelsSelect`（`keys.go:533`）

去掉 `JOIN key_groups kg`，改成 `JOIN model_groups g ON g.id=k.group_id AND g.enabled=1`。少一个 JOIN。

`DISTINCT` 在单组下已冗余（`group_providers` 有联合主键，每个模型只出一行），但**保留**——删它属于无关改动。

### 8.2 `KeyModelRateMilli`（`keys.go:495`）

`MAX(g.rate_milli)` → `g.rate_milli`，经 `client_keys.group_id` 关联。无行或 `rate<=0` 回落 `DefaultGroupRateMilli`，不变。

函数上方 :491-494 那段注释写的是「the highest rate among the key's enabled groups」，**必须重写**。

### 8.3 预留计价查询（`ledger.go:334`）

不再 JOIN `client_keys`，直接用第 320 行事务内已读出的 `groupID` 当参数：

```sql
SELECT m.input_price_microyuan, m.output_price_microyuan,
       COALESCE(m.cached_input_price_microyuan, m.input_price_microyuan), g.rate_milli
FROM model_groups g
JOIN group_providers gp ON gp.group_id=g.id
JOIN models m ON m.id=? AND m.provider_id=gp.provider_id AND m.enabled=1
     AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL
JOIN providers p ON p.id=m.provider_id AND p.enabled=1
WHERE g.id=? AND g.enabled=1
```

### 8.4 存储层 API

- `KeyGroupIDs` **取消**，改成在 `ClientKey`（`keys.go:20`）上加 `GroupID *int64`——指针，区分「未绑」与「组 id 为 0」。读密钥时一次带出，管理端不用再单独查
- `ReplaceKeyGroups(ctx, keyID, []int64)` → `SetKeyGroup(ctx, keyID, groupID int64)`，`0` = 解绑。保留现有校验：组必须存在且启用

### 8.5 API 三态

`admin.go:800` 的 `GroupIDs []int64` → `GroupID *int64`，照抄项目里 `ttl_seconds` 的既有形状：

| 传值 | 含义 |
|---|---|
| 字段缺省 | 不动——改名不会顺手把分组清掉 |
| `0` / `null` | 解绑 |
| `>0` | 绑定该组 |

v0.3.5 那条「留空不改有效期」已被 `admin_test.go::TestAdminKeyTTLThreeStates` 钉住，这里补一个同形状的测试。

### 8.6 路由与接口

- `keys/{id}/groups` → `keys/{id}/group`（`admin.go:925` + 前端 :1531/:1556）。留着复数名会误导
- 接入信息接口（`admin.go:997-1044`，:1112 的 `"groups": groups`）：数组变单个 `group` 对象。现有「组查不到时补 `{name:"", enabled:false}` 占位」的分支保留；`issues` 里的「未绑分组」原样保留

---

## 九、可见性矩阵

| 路径 | 管理员 | 密钥主体 |
|---|---|---|
| `session` GET | 身份 | 身份 |
| `me` GET（新增） | 400 | 自己这把密钥的明细 |
| `overview` / `analytics` GET | 全局 | 该组 |
| `providers` GET | 全部 | 仅组内渠道，**不含 `base_url`** |
| `providers/{id}/*` GET | 全部 | 该渠道在组内才可见，否则 404 |
| `providers/{id}/remote-models` | 允许 | 404（会带上游密钥请求上游） |
| `models` GET | 全部 | 该组授权模型，与 `/v1/models` 同源 |
| `groups` GET | 全部 | 只有自己那一个 |
| `groups/{id}` / `groups/{id}/overview` GET | 全部 | id == 自己的组，否则 404 |
| `groups/health` GET | 全部 | 仅自己组 |
| `requests` / `requests/{id}` GET | 全部 | 仅 `key_id = 自己` 的行 |
| `keys`、`keys/hints`、`plugins`、`config` | 允许 | 404 |
| `groups/monitor` POST、一切写操作 | 允许 | 404 |

**跨组一律 404，不用 403**。组 ID 是自增整数，403 等于告诉调用者「这个 ID 存在」，可枚举。

**两层分开**：`authorize()` 判路径级；处理函数再把 `subject.groupID` / `keyID` 传进 repo 做行级裁剪。单条资源（`groups/{id}`、`providers/{id}`、`requests/{id}`）必须**同时过两层**——路径允许但 id 越界要 404。只做路径级判定是这类改造最常见的漏洞形状。

**给密钥主体开 `requests` 的理由**：「自己明细」包含失败排查，而请求日志按项目既有口径只存元数据、不存 prompt 与回复。但 `requests/{id}` 单条也必须按 `key_id` 过滤，否则能遍历 ID 看别人的。

**唯一需要新写查询能力的是 `analytics`**：`internal/store/analytics.go`（228 行）现在全是全局查询，要加 `group_id` 过滤。其余端点的裁剪都能落在已有查询上加条件。

---

## 十、前端

### 10.1 主体与导航

- 保留 `isAdmin`（约 15 处 `isAdmin ?` 全部继续正确——密钥主体不是管理员，编辑按钮与上游地址列自动隐藏），新增 `isKey` 与 `scope = {keyID, groupID, groupName}`
- `body.readonly` 换成 `body.mode-anon|mode-key|mode-admin`。两态布尔类名撑不住三种主体。CSS :33 与 `applyAuth()`（:495）跟着改
- 导航只需**两类**可见性：无标记 = `overview`/`analytics`/`providers`/`groups`/**`requests`**（管理员与密钥主体都可见，范围由后端裁剪）；`.admin-only` = `keys`/`plugins`/`settings`
- 不需要第三类「仅密钥可见」——「自己明细」是 overview 里的一个区块（见 10.3），不是独立视图，所以没有只给密钥主体看的导航项。别为此预留类名
- `requests` 从 `.admin-only` 移出，由后端按 `key_id` 裁剪

### 10.2 认证块

- `api()`（:477-483）按主体设**且只设一个**头：admin → `X-Admin-Key`，key → `Authorization: Bearer crk_…`，anon → 都不设。现在无条件设 `X-Admin-Key`（空串也设），必须改成分支
- 两个 localStorage 键**互斥**：`cr_admin_key`（现有）与 `cr_client_key`（新增）。登录其一时必须清掉另一个，否则 `api()` 无法判断带哪个头
- 登录框一个输入自动判别：`candidate.startsWith("crk_")` → 客户端密钥，否则 → 管理员密钥
- `login()`（:533）改成读 `body.error.code` 再映射文案：

| code | 文案 |
|---|---|
| `token_expired` | 令牌已过期，请联系管理员续期 |
| `invalid_api_key` | 密钥无效或已停用 |
| `console_login_not_supported` | 应用型凭据（app_…）没有可验证的密钥，无法登录控制台 |
| `authentication_required` | 管理员密钥无效 |

- **`PUBLIC_VIEWS` 概念删掉**：没有匿名视图了。直接后果是**打开 `/admin/` 就是登录页**，而不是先看一屏只读数据。这是本次最明显的 UX 变化，已获老板确认
- `logout()`（:510-516）清两个键、回登录页，不再 `showView("overview")`
- 启动时（:1819 的 `if (adminKey)`）改成「任一凭据存在 → 调 session → 定主体；否则显示登录页」

### 10.3 概览与额度显示

**管理员概览的额度卡片必须拿掉，不能用「不限」顶替。**

全局额度取消后，`loadOverview()`（:628）那四张 `quotaCard` 没有了限额来源。最省事的做法是继续渲染、限额传 0——但 `quotaCard` 会把 0 显示成「**不限**」，而实际上每个组都有自己的限额。那是主动误导，比不显示更糟。

也不要把各组限额相加成「虚拟总量」：各组窗口起点不同，自定义周期更是每组独立，相加没有意义。

**分组额度归到「模型组」视图**，那是它语义上所属的地方：

- 管理员 overview 只留全局闸门类指标——RPM、并发、近一小时成功率、最近请求（:637/:642/:643 三张 `statCard` 原样保留），四张 `quotaCard` 移除
- 模型组视图每行加**额度紧张标记**：任一窗口 ≥80% 标黄、≥100% 标红，阈值直接复用 `quotaCard` 里已有的 `p >= 80` / `p >= 100` 判据
- 每组已有的「概览」弹窗（`groups/{id}/overview`）里渲染那四张 `quotaCard`——**payload 同形状，`quotaCard` 一行不用改**

**密钥主体的 overview 保留四张 `quotaCard`**，数据是自己组的，同样复用；加上「我的用量」区块（数据来自 `/admin/api/me`）。**不新开视图**——分组汇总与自己明细同屏，正好对应决策 4，也省掉一个导航项和一整套视图。

**`quota-refresh` 条件放宽**：`scope && isAdmin`（:621）改成只看 `scope`。刷新是只读重算，密钥主体刷自己的视图无害。

**容易漏的泄露点：overview 上的最近请求。** :644 的 `renderRequests(d.recent, "overview-recent")` 会在概览页渲染最近请求列表。密钥主体的 `d.recent` 必须由后端裁成只剩自己 `key_id` 的行——否则分组汇总的入口顺手泄露了同组其他密钥的请求记录，与第九节矩阵冲突。`renderRequests` 本身不用改，裁剪在服务端。

### 10.4 其余视图

| 视图 | 密钥主体下的变化 |
|---|---|
| analytics | 后端按 `group_id` 过滤；**模型选择器只列该组授权模型**（否则会列出他根本用不了的模型） |
| providers | 只列组内渠道；「上游地址」列已由 :881-882 的 `isAdmin` 判断隐藏，不用改；:1261 那个「管理员编辑弹窗与访客只读弹窗共用」的成员渠道模型一览走只读分支 |
| groups | 只有自己那一个；额度字段只读 |
| requests | 只有自己的行；诊断详情里的 `quote_rate_milli` 是他自己的计价倍率，保留 |
| keys | `.admin-only`；管理员侧把原来的多组展示改成单个分组名 |
| settings | 全局额度区块随配置项删除**自动消失**（该页按 EnvMap 渲染）；但 `cfgLabels` 里那几个键的中英说明变成死条目，要清掉 |

**空状态**：密钥主体未绑分组时，overview 不渲染额度卡片，显示「该密钥未绑定分组，无可查看内容」。

### 10.5 分组选择改单选

- 加 `radioListHTML` 兄弟函数，**`checkListHTML` 本身不动**（:1291 的组内渠道是真多选，`group_providers` 靠它维护）
- `renderKeyGroupChecks`（:1342-1346）：`type="checkbox"` → `type="radio" name="key-group"`
- :1545 改用 `radioListHTML`
- `checkedIds`（:912）：:1297 的 `provider_ids` 不动，:1556 改成读单选值
- 创建密钥 body（:1567）：`group_ids: [...]` → `group_id`
- :1531-1532 读 `keys/{id}/group`

### 10.6 分组额度编辑 UI

组编辑弹窗现在有名称、启用、倍率、组内渠道（多选）。加 10 个额度字段（4 窗口 × Token/金额、周期天数、周期 Token/金额、周期起点）。复用运行配置页那套额度输入的标记与「0 或留空 = 不限」提示文案，保持一致。

### 10.7 规模

`admin.html` 1835 行，改动集中在认证块（:459-549）、`applyAuth`/导航（:273-280、:494-500）、`loadOverview`（:628-645）、组编辑弹窗与密钥表单（:1291-1346、:1531-1567），加一个新的 `radioListHTML`。估计 250~350 行改动，**不新增文件**——`web/embed.go` 是单文件内嵌，拆文件会牵动构建。

---

## 十一、测试计划

基线：当前 `go build` / `go vet` 干净，`go test -count=1 ./...` **11 个包全绿**。改造后必须仍然全绿。

### store

- v16 应用后 `key_groups` 不存在、`client_keys.group_id` 存在；多组密钥回填到**倍率最大**的组；账目表 `group_id` 回填正确
- 播种幂等：跑两次结果一致；只填 NULL 列
- `SetKeyGroup`：绑定 / 解绑 / 拒绝停用组 / 组不存在
- 授权口径：未绑组 → 空；组停用 → 空；组内渠道增删 → 授权模型随之变
- 计价：倍率取自唯一所属组；无组时回落默认
- 分组额度 8 个维度各一条「用满即拒、窗口轮换后自动恢复」
- **两个组互不影响**：A 组用满不影响 B 组
- **两个组各自独立周期**——最容易漏的一条：周期状态现在按组走，而 `weekStart` 是请求局部变量，从代码形状上看不出变化
- **切换分组不搬历史**：A 组用满 → 密钥切到 B → B 的已用量不含 A 的历史，A 的已用量不变

### httpapi

- **把第九节那张矩阵写成 table-driven 测试**，逐个 `(path, method, subject)` 断言 200/401/404。这是本次改造**最重要的一张测试**——可见性漏洞不报错，只静默泄露
- 四种拒绝码分开断言
- **匿名一律 401**：现有那些断言「匿名可读 overview/providers/groups」的用例要反过来改
- 跨组 404 而非 403
- 泄露面复查：密钥主体拿到的 providers 不含 `base_url`；任何响应不含其他密钥的 `token_label` / `token_hint`；overview 的 `recent` 只含自己的行

### relay

- 额度拒绝仍是 429 + 对应 code + 窗口与数字（README 对外承诺的口径）
- 分组额度改完下一次请求即生效（改成事务内读库后天然成立，但要钉住）

### 现有测试的处置

`internal/store/catalog_test.go:134` 与 `:218` 这两个用例给一个 Key 挂了两个组，验证 `MAX(rate_milli)` 选倍率。**不要直接删**——它们覆盖的是计价路径，删了就没覆盖了。改成验证新语义：一个改成「切换分组后授权模型随之变化」，一个改成「倍率取自唯一所属组」。

`catalog_test.go:166` 那个「传空切片清空分组」改成「传 0 解绑」。

`ReplaceKeyGroups` 约 12 个调用点（`admin.go` 4 处 + 各测试）改成 `SetKeyGroup`。

另补三条：未绑分组的密钥请求被拒、解绑后再绑回、组停用后授权模型为空。

### e2e（`_e2e/`，gitignored）

- `run_client_onboarding.py`（31 项断言）里「勾分组」改「选一个分组」
- 新增闭环：用分发出去的 `crk_` 登录控制台 → 只看得到自己组 → 看不到别的组、别的密钥、config、plugins

### UI

按项目既有约定走 DOM 验证（`admin.html` 是单文件内嵌，无构建步骤）。单选控件要断言：勾一个组后再勾另一个，前一个自动取消，且提交出去的 `group_id` 是单值。

---

## 十二、交接：运维与环境上下文

这一节与改造本身无关，但不写下来下次还会踩。

### 12.1 代码与版本

- 仓库：`E:\生产项目\项目\TrailblazerStation\civic-ai-relay`
- 远端：`https://github.com/lefengyao/civic-ai-relay.git`，分支 `main`
- 基线：HEAD `e476f2e2`，tag `v0.3.9`
- **`2245471`（9/29）到 `05671ff` 之间的中间提交不保证单独可编译**：`settings.go`、`schema.go`、`server.go`、`admin.html` 同时承载多个特性的改动，只有 `05671ff` 及之后是完整绿的。`git bisect` 跨这段不可用

### 12.2 生产部署

- 主机：**waigame**（`59.44.120.54`，NAT 映射 `9045→22`），不是阿里云
- 部署目录 `/opt/civic-ai-relay`，compose 项目名 = 目录名
- 容器 `civic-ai-relay-civic-relay-1`，镜像 `civic-relay:v0.3.9`
- 命名卷 `civic-ai-relay_civic-relay-config` / `-data`
- 服务器树 = **git commit 的完整树 + 三个本地文件**（`.env`、`civic-bin`、`DEPLOYED_COMMIT`），已逐文件核对，无多余无缺失
- 镜像只剩两个：`civic-relay-runtime:base`（17.7MB，运行时基座）与 `civic-relay:v0.3.9`（38.4MB）
- **`civic-relay-runtime:base` 绝不能删**：服务器拉不到 gcr.io 与 Docker Hub，删了就再也建不出镜像

阿里云2c2g 上的 civic 已全部清除（镜像、卷、`/opt/civic-relay`、构建缓存），nginx 无残留反代。归档备份在 `C:\Users\15610\civic-backups\civic-aliyun-archive-20261031.tar.gz`（含 relay.env 与 `RELAY_ENCRYPTION_KEY`，**别外传**）。

### 12.3 构建与部署流程

服务器**没有 Go 工具链、访问不到 GitHub**（`github.com` 与 git 智能协议全部 000），所以只能本机产出包再上传。完整步骤见 `docs/deploy-prebuilt.md`，摘要：

```bash
bash scripts/build_deploy_bundle.sh v0.4.0     # 工作区不干净会拒绝构建
# 上传 dist/deploy-v0.4.0/ 整个目录（4 个文件）到 /opt/civic-ai-relay/bundle/
#   sha256sum -c bundle/SHA256SUMS   # 先校验，别跳过
#   mv bundle/civic-relay civic-bin  # 注意改名：包里叫 civic-relay，服务器上叫 civic-bin
#   mv bundle/DEPLOYED_COMMIT .  &&  tar -xzf bundle/src.tar.gz  &&  rm -rf bundle
# 服务器：tar -xzf src.tar.gz && rm src.tar.gz
#   docker build -f Dockerfile.prebuilt -t civic-relay:v0.4.0 .
#   改 .env 的 CIVIC_RELAY_VERSION → docker compose up -d（不要加 --build）
```

- `Dockerfile.prebuilt` 是 `FROM civic-relay-runtime:base` + `COPY civic-bin`，体积恒定 7 层 38MB
- **不要用「`FROM` 上一个迭代 tag」的做法**：那样层层叠加，做过 22 个镜像叠到 10 层 100MB 装一个 14MB 二进制，且删任一旧镜像就断重建链
- 传输用 ServerOpsHelper MCP 的 `upload_file` / `download_file`（磁盘直流、二进制安全、无 8 MiB 限制）。14.5MB 约 1.6 秒，15MB 整包约 6 秒
- **绝不要 `docker compose down -v`**——会删掉配置卷与数据库

### 12.4 本机环境约束

- Go：`go1.27.0 windows/amd64`，在 `/c/Program Files/Go/bin`
- **本机没装 Python**：只有 `WindowsApps\python.exe` 空壳别名，调用无输出、退出码 0，看着像成功。仓库里 `scripts/*.py`（`package_release.py`、`package_site_zip.py`、`export_deploy_plugin.py`）**目前全都跑不了**。新脚本一律用 bash
- 构建要 `GOPROXY=off`（依赖已在模块缓存，官方 proxy 握手超时），且**后台跑**——前台会撞 120 秒超时被 SIGTERM 且输出为空
- Docker Desktop：CLI 在 `C:\Users\15610\AppData\Local\Programs\DockerDesktop\resources\bin\docker.exe`（**不是** `Program Files`），守护进程默认没启动
- `git push` 到 github.com 需要走本机代理 `http://127.0.0.1:7890`（直连超时；`api.github.com` 反而直连可通）。用一次性参数 `-c http.proxy=... -c https.proxy=...`，**不要改 git config**
- `core.autocrlf=true`，已加 `.gitattributes` 钉 `* text=auto eol=lf`。注意 `git archive` 会套用 autocrlf 把导出转成 CRLF，做逐文件比对时要用 `-c core.autocrlf=false`，否则会显示「全部不同」
- 本机 `md5sum` 输出带二进制标记 `*`（`hash *path`），Linux 端没有（`hash  path`）。跨端比对要规范化，否则键全错开

### 12.5 遗留的运维风险（未处理）

**waigame 的 SSH 是敞开的**，本次只做了诊断，未改任何配置：

```
permitrootlogin yes   passwordauthentication yes   pubkeyauthentication no
maxstartups 10:30:100   logingracetime 120   fail2ban 未安装
auth.log 195MB，失败尝试 965,973 次
头号来源 195.178.110.220 独占 561,081 次
```

`MaxStartups 10` 配 `LoginGraceTime 120`，22 口常年 10 个连接被爆破流量占满——本次工作中 MCP 的 SFTP 调用两次报 `Exceeded MaxStartups`，也就是**老板自己随时可能被挡在门外**。

建议顺序（每步 `sshd -t` 验证 + `systemctl reload ssh`，reload 不断现有连接，**不要 restart**）：

1. 先在本机生成密钥、写入 `authorized_keys`，确认能密钥登录
2. 再翻 `PubkeyAuthentication yes` / `PasswordAuthentication no` / `PermitRootLogin prohibit-password`
3. `MaxStartups 100:30:200`、`LoginGraceTime 30`
4. 装 fail2ban
5. 截断 195MB 的 auth.log

### 12.6 本机留存的两处敏感文件

- `C:\Users\15610\civic-build\waigame-bootstrap-admin-key.txt` —— 管理台初始密钥，**服务器副本已删，这是唯一一份**
- `C:\Users\15610\civic-backups\civic-aliyun-archive-20261031.tar.gz` —— 阿里云归档，内含 `relay.env` 与 `RELAY_ENCRYPTION_KEY`

`C:\Users\15610\civic-build\civic-bin` 是 v0.3.9 之前的生产二进制（md5 `61bec819…`），按老板要求保留作参照。

---

## 十三、实施分期

这份 spec 对单个实施计划来说偏大（同时动数据模型、额度判定、认证与前端）。拆成三个阶段，**每阶段可独立发布、独立验证**。

### 阶段 A：数据模型归位（一对一）

- migration v16 的第 1-4、6-7 步：加 `client_keys.group_id`、回填、两张账目表加 `group_id` 并回填、删 `key_groups`、建两个新索引
- 三处 SQL 改写（8.1 / 8.2 / 8.3）、`SetKeyGroup`、`ClientKey.GroupID`
- API 三态、`keys/{id}/group` 改名、接入信息接口
- 前端分组单选（10.5）
- 测试：`catalog_test.go:134/218` 两个多组用例改语义，授权与计价的回归测试

> **账目表的 `group_id` 必须在阶段 A 就加并回填**，即使阶段 B 才用它判定。分两次迁移会让回填逻辑写两遍，而且第二次回填时 `key_groups` 已经被删了，无从推导历史归属。

发布后外部行为除「一个密钥只能挂一个组」之外不变，权限与额度都不动。

### 阶段 B：分组额度

- migration v17：`model_groups` 加 10 列
- 判定链改造（第七节）：`QuotaLimits` 拆分、四个窗口加分组过滤、`periodQuotaTransition` 三条查询、错误 detail 带组信息
- 删除全局额度：`Settings` 字段与解析、EnvMap、`cfgLabels`、`.env.example`
- Go 侧播种（5.3），含 `PeriodLimitStart` 自动填充的分组版
- `GroupOverview` 口径改为按 `group_id`
- 删组的影响面告知（5.1 末尾那条）
- 前端：管理员概览去掉额度卡片、模型组视图加紧张标记、组概览弹窗渲染 `quotaCard`、组编辑弹窗加 10 个额度字段
- 测试：分组额度 8 个维度、两组互不影响、**两组各自独立周期**、**切换分组不搬历史**、播种幂等

发布后仍是匿名只读 + 管理员编辑，只是额度归属从全局变成分组。

### 阶段 C：认证与可见性

- 删 `isPublicRead`、引入 `subject` 与 `authorize()` 矩阵
- `session` 语义变更、新增 `me`、四种拒绝码
- `analytics` 加 `group_id` 过滤（**本阶段唯一需要新写查询能力的地方**）
- 各端点行级裁剪，特别是 overview 的 `recent`
- 前端：三态主体、`mode-*` 类名、`api()` 头分支、两个互斥 localStorage 键、登录框自动判别与 code 映射、删 `PUBLIC_VIEWS`、密钥主体 overview 的「我的用量」区块与空状态
- 测试：**第九节矩阵的 table-driven 测试**、匿名一律 401、跨组 404 而非 403、泄露面复查
- e2e 新增闭环：用 `crk_` 登录 → 只看得到自己组

### 为什么是这个顺序

B 依赖 A（分组额度按 `group_id` 归属，而这一列是 A 落下的）；C 依赖 B（密钥主体要看的正是分组额度与用量，B 没做完 C 的界面没内容可显示）。反过来都不成立。

### 三条纪律

1. **每阶段各发一个版本**，不要攒成一个大版本。三阶段都动了额度或权限，混在一起出问题时无法定位是哪一层
2. **阶段 A 和 B 升级前必须停机做一致备份**（5.4），两者都含破坏性迁移，且项目没有 down-migration 机制
3. 这与 12.1 记的教训是同一件事：9/29 之后一个多月的工作零提交、22 个 `workspace-*` 镜像叠在一起，最后花了一整轮才理清。别再攒
