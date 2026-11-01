# Civic Relay 阶段 B：分组额度实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 额度从「全局配置」迁到「每个分组独立分配」（4 窗口 × Token/金额 + 自定义周期），判定在预留事务内从分组行读取；全局额度配置项删除并由启动时播种迁移；统计口径与扣减口径统一为「归属该组的请求」。

**Architecture:** 复用既有窗口判定机制、只换数据来源——`ReserveInput.Limits` 只携带全局的 `RPMLimit`，其余 9 个额度值在事务内从 `model_groups` 行读出后填进同一个 `QuotaLimits`，窗口查询各加 `AND group_id=?`（用阶段 A 落下的账目 `group_id` 列）。全局配置走「读原值 → 灌进分组 NULL 列 → 从 relay.env 删键」的幂等播种。

**Tech Stack:** Go 1.27 + modernc.org/sqlite；前端 `web/admin.html` 单文件内嵌。

**Spec:** `docs/superpowers/specs/2026-11-01-civic-relay-group-scoping-design.md`（第 5.1/5.3、7、10.3/10.6 节 + 第十三节阶段 B）

---

## 本机执行约束（每个 Task 都适用）

- Go 在 `/c/Program Files/Go/bin`；一切 go 命令 `GOPROXY=off`
- 单包测试可前台跑（超时 240s）；**全量 `go test ./...` 必须后台跑**（run_in_background），前台撞 120 秒超时
- 工作目录 `E:\生产项目\项目\TrailblazerStation\civic-ai-relay`（下文 `$R`）；分支 main 直接提交，**不要 push**
- 已知基线抖动：`TestReapStaleReservationsReleasesQuotaAndConcurrency` 有 10ms 时序窗，偶发首跑失败——复跑通过即算绿，**不要去改那个测试**
- 提交信息英文 Conventional Commits；`gofmt -l internal/ cmd/` 应无输出

## 阶段 A 移交的两件小事（Task 6 顺带）

1. `editKey` 编辑弹窗：当前所属组**已停用**时 radio 列表里没有它，直接保存会静默解绑 → 改为「当前组即使停用也显示（标『已停用』）+ 只在选中值变化时才 PUT /group」
2. `cmd/civic-relay/main.go:443` 的日志措辞「first start created…」对「恢复路径」（relay.env 在、bootstrap 文件缺 → 写回现有 AdminAPIKey，见 `config.Ensure` store.go:370-388）有误导 → 区分两种文案

## 与 spec 的偏差记录（实施时不要改回去）

1. **紧张度标记只算三个主窗口**（5h/当日/本周），不含自定义周期——周期按组独立启停，批量算紧张度要逐组跑周期切换点，成本配不上收益；周期是否咬住在组概览弹窗里看
2. **分组创建不带额度字段**，创建后用编辑弹窗设——`NewModelGroup` 保持三字段，避免创建表单 10 个框
3. `_e2e/run_e2e.py` 的 39 项断言里「额度对照」依赖全局额度配置 API，本阶段后**必然失效**——本机没装 Python 跑不了，也不做盲改；标记为已知欠账（Task 7）
4. **`QuotaLimits` 不做字段拆分，改数据来源**。spec 第七节说「QuotaLimits 只剩 RPMLimit」；实现改为保留全部字段、把语义从「全局配置快照」变成「本次请求生效的额度（事务内从分组行填入，RPMLimit 仍来自全局）」。这样五个窗口判定函数（overdrawn/periodQuotaTransition/weeklyWindowStart…）**一行不用改逻辑**，ledger_test 的改写也只动数据来源。spec 的意图（额度值不再来自配置、事务内从分组行读）完全满足，只是载体形状不同

---

### Task 1: migration v18 — model_groups 加 10 列

**Files:**
- Modify: `internal/store/schema.go`（`migrations` 末尾，v17 之后）
- Test: `internal/store/migration_test.go`

- [ ] **Step 1: 写失败的测试**

`TestMigrationV16BackfillPicksHighestRate` 末尾（v17 索引断言之后）追加：

```go
	// v18: model_groups 的 10 个额度列。全部可空，NULL/0 = 不限；
	// 全局额度值在 relay.env（配置）而非数据库，播种由 Go 侧在启动时做。
	applyMigration(db, t, 18)
	for _, col := range []string{
		"token_limit_5h", "amount_limit_5h",
		"token_limit_daily", "amount_limit_daily",
		"token_limit_weekly", "amount_limit_weekly",
		"period_limit_days", "token_limit_period", "amount_limit_period",
		"period_limit_start",
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('model_groups') WHERE name=?`, col).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing column model_groups.%s", col)
		}
	}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestMigrationV16BackfillPicksHighestRate -count=1
```
预期：FAIL，`missing column model_groups.token_limit_5h`。

- [ ] **Step 3: 实现 v18**

`schema.go` 的 `migrations` 末尾追加：

```go
	// v18: 分组额度（取代全局额度，设计见 docs/superpowers/specs/2026-11-01-…
	// -group-scoping-design.md 第 5.1/5.3 节）。10 列全部可空：NULL 或 0 = 不限，
	// 沿用「限额是运维显式选择」的既有口径——升级后所有分组默认不限，不会突然
	// 出现意外拒绝。
	//
	// 全局额度值在 relay.env（配置）而不是数据库，所以「搬进每个现有分组」
	// 不能用 SQL 迁移完成：由启动时的播种逻辑读原值 → 只填 IS NULL 的列 →
	// 从 relay.env 删键（删键本身就是幂等标记，见 cmd/civic-relay/main.go）。
	{version: 18, statements: []string{
		`ALTER TABLE model_groups ADD COLUMN token_limit_5h INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_5h INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN token_limit_daily INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_daily INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN token_limit_weekly INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_weekly INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN period_limit_days INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN token_limit_period INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_period INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN period_limit_start TEXT`,
	}},
```

- [ ] **Step 4: 跑测试确认通过 + 全包回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestMigrationV16BackfillPicksHighestRate -count=1
cd "$R" && GOPROXY=off go test ./internal/store/ -count=1
```
预期：全 PASS。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add internal/store/schema.go internal/store/migration_test.go
git commit -m "feat(store): add per-group quota columns (migration v18)"
```

---

### Task 2: ModelGroup 结构体与 CRUD 带额度列

**Files:**
- Modify: `internal/store/models.go`（`ModelGroup` :42-47、`UpdateModelGroup` :55-59 与 :275-299、`UpdateModelGroup` 实现、`ListModelGroups` :301-319）
- Test: `internal/store/catalog_test.go`（追加）

- [ ] **Step 1: 写失败的测试**

`catalog_test.go` 末尾追加（`intPtr` 已在同文件 :257，接收 int64 返回 *int64）：

```go
func strPtr(v string) *string       { return &v }
func daysPtr(v int) *int            { return &v }

// 分组额度持久化：nil = 保持原值，0 = 不限，负数拒绝；周期起点自动填充。
func TestGroupQuotaPersistenceAndValidation(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	g, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{TokenLimit5H: intPtr(-1)}); err == nil {
		t.Fatal("负数额度被接受")
	}
	lim := int64(5000)
	updated, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{
		TokenLimit5H:    intPtr(5000),
		AmountLimit5H:   intPtr(1_000_000),
		PeriodLimitDays: daysPtr(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.TokenLimit5H == nil || *updated.TokenLimit5H != 5000 {
		t.Fatalf("token_limit_5h = %v", updated.TokenLimit5H)
	}
	if updated.PeriodLimitStart == nil || *updated.PeriodLimitStart == "" {
		t.Fatalf("days>0 且起点为空时应自动填 now，实际 %v", updated.PeriodLimitStart)
	}
	// 未提及的字段保持原值（nil = 不动）
	touched, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{TokenLimit5H: intPtr(6000)})
	if err != nil {
		t.Fatal(err)
	}
	if *touched.TokenLimit5H != 6000 || touched.AmountLimit5H == nil || *touched.AmountLimit5H != 1_000_000 {
		t.Fatalf("未提及的额度被误改：5h=%v 金额5h=%v", touched.TokenLimit5H, touched.AmountLimit5H)
	}
	// 0 = 显式设为不限；周期天数归零时起点一并清除
	cleared, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{
		TokenLimit5H:    intPtr(0),
		PeriodLimitDays: daysPtr(0),
		PeriodStart:     strPtr(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.TokenLimit5H == nil || *cleared.TokenLimit5H != 0 {
		t.Fatalf("0 应表示不限，实际 %v", cleared.TokenLimit5H)
	}
	if cleared.PeriodLimitStart != nil && *cleared.PeriodLimitStart != "" {
		t.Fatalf("days=0 应清除起点，实际 %v", cleared.PeriodLimitStart)
	}
	// 列表能读回
	listed, err := repo.ListModelGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *ModelGroup
	for i := range listed {
		if listed[i].ID == g.ID {
			found = &listed[i]
		}
	}
	if found == nil || found.TokenLimit5H == nil || *found.TokenLimit5H != 0 {
		t.Fatalf("列表未读回额度：%#v", found)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestGroupQuotaPersistenceAndValidation -count=1
```
预期：FAIL（`UpdateModelGroup` 无额度字段，编译错误）。

- [ ] **Step 3: 实现**

3a. `ModelGroup`（models.go:42-47）改为：

```go
type ModelGroup struct {
	ID        int64
	Name      string
	Enabled   bool
	RateMilli int64 // 计费倍率，千分比表示：1000 = 1.0x
	// 分组额度（取代旧全局额度，v18）：nil = 未设 = 不限。判定在预留事务内
	// 从本行读取（ledger.go ReserveRequest），统计与扣减同口径。
	TokenLimit5H      *int64
	AmountLimit5H     *int64 // 微元
	TokenLimitDaily   *int64
	AmountLimitDaily  *int64 // 微元
	TokenLimitWeekly  *int64
	AmountLimitWeekly *int64 // 微元
	PeriodLimitDays   *int
	TokenLimitPeriod  *int64
	AmountLimitPeriod *int64 // 微元
	// PeriodLimitStart 为 RFC3339 文本；PeriodLimitDays>0 且为空时由
	// UpdateModelGroup 自动填 now（沿用 settings.go:245 的既有行为）。
	PeriodLimitStart *string
}
```

3b. `UpdateModelGroup`（models.go:55-59）改为：

```go
type UpdateModelGroup struct {
	Name      string
	Enabled   *bool
	RateMilli *int64
	// 分组额度：nil = 保持原值；非 nil = 设为该值（0 = 不限），负数拒绝。
	// 前端编辑弹窗总是显式传全量，所以「改名」不会误清额度、
	// 「清空输入框」就是显式设为不限。
	TokenLimit5H      *int64
	AmountLimit5H     *int64
	TokenLimitDaily   *int64
	AmountLimitDaily  *int64
	TokenLimitWeekly  *int64
	AmountLimitWeekly *int64
	PeriodLimitDays   *int
	TokenLimitPeriod  *int64
	AmountLimitPeriod *int64
	PeriodLimitStart  *string // RFC3339；"" = 清除起点
}
```

3c. `UpdateModelGroup` 实现（models.go:275-299）整函数替换为：

```go
func (s *Store) UpdateModelGroup(ctx context.Context, id int64, in UpdateModelGroup) (ModelGroup, error) {
	var g ModelGroup
	var enabled int
	var t5h, a5h, td, ad, tw, aw, pd, tp, ap sql.NullInt64
	var ps sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT name,enabled,rate_milli,token_limit_5h,amount_limit_5h,token_limit_daily,amount_limit_daily,token_limit_weekly,amount_limit_weekly,period_limit_days,token_limit_period,amount_limit_period,period_limit_start FROM model_groups WHERE id=?`, id).Scan(&g.Name, &enabled, &g.RateMilli, &t5h, &a5h, &td, &ad, &tw, &aw, &pd, &tp, &ap, &ps); err != nil {
		return ModelGroup{}, err
	}
	g.ID, g.Enabled = id, enabled == 1
	g.TokenLimit5H, g.AmountLimit5H = nullableInt(t5h), nullableInt(a5h)
	g.TokenLimitDaily, g.AmountLimitDaily = nullableInt(td), nullableInt(ad)
	g.TokenLimitWeekly, g.AmountLimitWeekly = nullableInt(tw), nullableInt(aw)
	g.TokenLimitPeriod, g.AmountLimitPeriod = nullableInt(tp), nullableInt(ap)
	if pd.Valid {
		days := int(pd.Int64)
		g.PeriodLimitDays = &days
	}
	if ps.Valid && ps.String != "" {
		g.PeriodLimitStart = &ps.String
	}
	if strings.TrimSpace(in.Name) != "" {
		g.Name = strings.TrimSpace(in.Name)
	}
	if in.Enabled != nil {
		g.Enabled = *in.Enabled
	}
	if in.RateMilli != nil {
		rate, err := normalizeRateMilli(*in.RateMilli)
		if err != nil {
			return ModelGroup{}, err
		}
		g.RateMilli = rate
	}
	for _, pair := range []struct{ dst **int64; src *int64 }{
		{&g.TokenLimit5H, in.TokenLimit5H}, {&g.AmountLimit5H, in.AmountLimit5H},
		{&g.TokenLimitDaily, in.TokenLimitDaily}, {&g.AmountLimitDaily, in.AmountLimitDaily},
		{&g.TokenLimitWeekly, in.TokenLimitWeekly}, {&g.AmountLimitWeekly, in.AmountLimitWeekly},
		{&g.TokenLimitPeriod, in.TokenLimitPeriod}, {&g.AmountLimitPeriod, in.AmountLimitPeriod},
	} {
		if pair.src != nil {
			if err := validateLimit(pair.src, "group quota limit"); err != nil {
				return ModelGroup{}, err
			}
			*pair.dst = pair.src
		}
	}
	if in.PeriodLimitDays != nil {
		if *in.PeriodLimitDays < 0 {
			return ModelGroup{}, errors.New("group quota limit must be non-negative")
		}
		g.PeriodLimitDays = in.PeriodLimitDays
	}
	// 周期起点：显式传入就照设（"" = 清除）；没传且当前为空、而天数 > 0 时
	// 自动填 now——否则「只填了天数」的周期会因起点为零而静默不生效
	//（periodQuotaTransition 对 PeriodStart.IsZero() 直接返回零值）。
	if in.PeriodLimitStart != nil {
		g.PeriodLimitStart = in.PeriodLimitStart
	}
	if g.PeriodLimitDays != nil && *g.PeriodLimitDays > 0 && (g.PeriodLimitStart == nil || *g.PeriodLimitStart == "") && (in.PeriodLimitStart == nil) {
		started := nowUTC()
		text := started.Format(time.RFC3339)
		g.PeriodLimitStart = &text
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE model_groups SET name=?,enabled=?,rate_milli=?,token_limit_5h=?,amount_limit_5h=?,token_limit_daily=?,amount_limit_daily=?,token_limit_weekly=?,amount_limit_weekly=?,period_limit_days=?,token_limit_period=?,amount_limit_period=?,period_limit_start=?,updated_at_utc=? WHERE id=?`, g.Name, boolInt(g.Enabled), g.RateMilli, g.TokenLimit5H, g.AmountLimit5H, g.TokenLimitDaily, g.AmountLimitDaily, g.TokenLimitWeekly, g.AmountLimitWeekly, g.PeriodLimitDays, g.TokenLimitPeriod, g.AmountLimitPeriod, g.PeriodLimitStart, nowUTC(), id); err != nil {
		return ModelGroup{}, err
	}
	return g, nil
}
```

> 注意：`validateLimit(v *int64, label string)` 是 keys.go:231 的既有 helper（`*v < 0` 报错），直接用。`in.PeriodLimitDays` 没有现成 helper，就地判负。models.go 需要 `time` import（检查是否已有，没有就补）。

3d. `ListModelGroups`（models.go:301-319）的 SELECT 与扫描扩列：

```go
func (s *Store) ListModelGroups(ctx context.Context) ([]ModelGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,g.enabled,g.rate_milli,g.token_limit_5h,g.amount_limit_5h,g.token_limit_daily,g.amount_limit_daily,g.token_limit_weekly,g.amount_limit_weekly,g.period_limit_days,g.token_limit_period,g.amount_limit_period,g.period_limit_start,(SELECT count(*) FROM client_keys k WHERE k.group_id=g.id) FROM model_groups g ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]ModelGroup, 0)
	for rows.Next() {
		var g ModelGroup
		var enabled int
		var t5h, a5h, td, ad, tw, aw, pd, tp, ap sql.NullInt64
		var ps sql.NullString
		var bound int64
		if err := rows.Scan(&g.ID, &g.Name, &enabled, &g.RateMilli, &t5h, &a5h, &td, &ad, &tw, &aw, &pd, &tp, &ap, &ps, &bound); err != nil {
			return nil, err
		}
		g.Enabled = enabled == 1
		g.TokenLimit5H, g.AmountLimit5H = nullableInt(t5h), nullableInt(a5h)
		g.TokenLimitDaily, g.AmountLimitDaily = nullableInt(td), nullableInt(ad)
		g.TokenLimitWeekly, g.AmountLimitWeekly = nullableInt(tw), nullableInt(aw)
		g.TokenLimitPeriod, g.AmountLimitPeriod = nullableInt(tp), nullableInt(ap)
		if pd.Valid {
			days := int(pd.Int64)
			g.PeriodLimitDays = &days
		}
		if ps.Valid && ps.String != "" {
			g.PeriodLimitStart = &ps.String
		}
		g.BoundKeys = bound
		out = append(out, g)
	}
	return out, rows.Err()
}
```

> `BoundKeys`（绑定密钥数）提前在本任务落，Task 4 的删组确认要用。`ModelGroup` 结构体此时就带上 `BoundKeys int64` 字段（加在 PeriodLimitStart 之后）。

- [ ] **Step 4: 跑测试确认通过 + 全包回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestGroupQuotaPersistenceAndValidation -count=1
cd "$R" && GOPROXY=off go test ./internal/store/ -count=1
```
预期：全 PASS（既有调用只传 Name/Enabled/RateMilli，额度字段 nil=不动，不受影响）。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add internal/store/models.go internal/store/catalog_test.go
git commit -m "feat(store): persist and expose per-group quota fields on model groups"
```

---

### Task 3: 预留事务从分组行取额度；窗口查询按组过滤

**Files:**
- Modify: `internal/store/ledger.go`（`ReserveRequest` :320-460、`periodQuotaTransition` :242-275、`QuotaError` :27-42）
- Modify: `internal/relay/service.go`（:373-389 Limits 填充收缩）
- Modify: `internal/httpapi/admin.go`（:251-261 overview 的 Limits 填充收缩）
- Modify: `internal/store/ledger_test.go`（fixture 与 5 个窗口测试改写）
- Test: `internal/store/group_scoping_test.go`（追加）

- [ ] **Step 1: 写失败的测试**

1a. `group_scoping_test.go` 末尾追加（沿用该文件已有的 `setupGroupScope`）：

```go
// 分组额度取代全局额度后，窗口判定的数据来源是分组行：A 组用满不影响 B 组。
func TestGroupQuotasAreIndependentAcrossGroups(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	keyA, err := repo.CreateClientKey(ctx, NewClientKey{Name: "ka", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := repo.CreateClientKey(ctx, NewClientKey{Name: "kb", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyA.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyB.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	// A 组当日 Token 限额 50：一次预留（约 608）就把窗口灌满
	if _, err := repo.UpdateModelGroup(ctx, groupA.ID, UpdateModelGroup{TokenLimitDaily: intPtr(50)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "a-first", 600)); err != nil {
		t.Fatalf("A 组首笔应按先欠费后停服放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "a-second", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "daily" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want daily token_quota_exceeded", err)
	}
	// B 组没有限额：同样的负载完全不受 A 组影响
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyB.ID, modelID, "b-first", 600)); err != nil {
		t.Fatalf("B 组不应被 A 组的额度咬住：%v", err)
	}
	// 错误 detail 带分组信息（spec 第七节：运维要知道是哪个组咬住的）
	if quota.Group == "" || quota.GroupID != groupA.ID {
		t.Fatalf("quota detail 缺分组信息：group=%q id=%d", quota.Group, quota.GroupID)
	}
}

// 每组各自独立周期：A 组周期用满切周限额，B 组的周期不受影响。
func TestGroupPeriodsAreIndependent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	keyA, err := repo.CreateClientKey(ctx, NewClientKey{Name: "ka", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyA.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, groupA.ID, UpdateModelGroup{
		TokenLimitPeriod: intPtr(50),
		PeriodLimitDays:  daysPtr(3),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "period-a-1", 600)); err != nil {
		t.Fatalf("A 组周期首笔应放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "period-a-2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "period" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want period token_quota_exceeded", err)
	}
	// 周期是 A 组的状态：没有配周期也没有额度的 B 组完全不受影响
	keyB, err := repo.CreateClientKey(ctx, NewClientKey{Name: "kb", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyB.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyB.ID, modelID, "period-b-1", 600)); err != nil {
		t.Fatalf("B 组不应感知 A 组的周期：%v", err)
	}
}
```

（追加到同一文件，与上面两个测试同块或独立成块均可）：

```go
// 8 个维度里既有测试没覆盖的三条：5h Token、5h 金额、本周金额。
func TestFiveHourTokenWindowBlocksAfterItIsSpent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{TokenLimit5H: intPtr(50)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-token-1", 600)); err != nil {
		t.Fatalf("首笔应按先欠费后停服放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-token-2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "5h" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want 5h token_quota_exceeded", err)
	}
}

func TestFiveHourAmountWindowBlocksAfterItIsSpent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{AmountLimit5H: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-amount-1", 600)); err != nil {
		t.Fatalf("首笔应放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-amount-2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "5h" || quota.Code != "amount_quota_exceeded" || quota.Metric != "amount" {
		t.Fatalf("err = %v, want 5h amount_quota_exceeded", err)
	}
}

func TestWeeklyAmountWindowBlocksAfterItIsSpent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{AmountLimitWeekly: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "week-amount-1", 600)); err != nil {
		t.Fatalf("首笔应放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "week-amount-2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "weekly" || quota.Code != "amount_quota_exceeded" {
		t.Fatalf("err = %v, want weekly amount_quota_exceeded", err)
	}
}
```

1b. `group_scoping_test.go` 文件头 import 补 `"errors"`。

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run 'TestGroupQuotasAreIndependent|TestGroupPeriodsAreIndependent' -count=1
```
预期：FAIL——`UpdateModelGroup` 已有额度字段（Task 2），但 `ReserveRequest` 还没读分组行，`quota.Group` 字段不存在（编译错误）。

- [ ] **Step 3: 实现**

3a. `QuotaError`（ledger.go:27-33）加两个字段：

```go
type QuotaError struct {
	Code   string // token_quota_exceeded / amount_quota_exceeded / key_* / rpm_exceeded / concurrency_exceeded
	Scope  string // 5h / daily / weekly / period / key / ""
	Metric string // tokens / amount / ""
	Used   int64  // tokens 或微元，取决于 Metric
	Limit  int64
	// 额度归属从全局变成分组后，运维必须能看出是哪个组的额度咬住了请求。
	Group   string
	GroupID int64
}
```

`Detail()`（:44-69）整函数替换为（头尾文案保持原文，组信息插在已用/限额之后）：

```go
// Detail 生成给客户端看的中文说明。判定口径是"先欠费后停服"——用到限额之后
// 才拒，所以文案要说清"已欠费"，否则客户端会以为是临时故障而反复重试。
// 额度归属变成分组之后，运维必须能看出是哪个组的额度咬住的（spec 第七节）。
func (e *QuotaError) Detail() string {
	if e == nil {
		return ""
	}
	window := map[string]string{
		"5h":     "5 小时窗口",
		"daily":  "当日额度（北京时间）",
		"weekly": "本周额度（北京时间，自周一起）",
		"period": "自定义周期额度",
		"key":    "该 Key 的总额度",
	}[e.Scope]
	if window == "" {
		window = e.Scope
	}
	metric := "Token"
	if e.Metric == "amount" {
		metric = "金额"
	}
	format := func(v int64) string {
		if e.Metric == "amount" {
			return "¥" + strconv.FormatFloat(float64(v)/1e6, 'f', 2, 64)
		}
		return strconv.FormatInt(v, 10) + " tokens"
	}
	detail := fmt.Sprintf("%s%s已用满（已用 %s，限额 %s）", window, metric, format(e.Used), format(e.Limit))
	if e.Group != "" {
		detail += fmt.Sprintf("，分组「%s」#%d", e.Group, e.GroupID)
	}
	return detail + "。当前处于欠费状态，请等待窗口轮换或在管理台调大限额"
}
```

3b. `periodQuotaTransition`（:242）签名加 groupID，三条查询各加 `AND group_id=?`：

```go
func periodQuotaTransition(ctx context.Context, conn *sql.Conn, limits QuotaLimits, groupID int64, started string) (time.Time, int64, int64, error) {
```

- :245 的聚合查询：`FROM key_reservations WHERE request_id IS NOT NULL AND status IN (...) AND group_id=? AND created_at_utc>=? AND created_at_utc<? AND created_at_utc<=?`，参数列表头插 `groupID`
- :251 的窗口函数查询同样加 `AND group_id=?`（参数头插 `groupID`）

3c. `ReserveRequest` 的读 Key 查询（:320-321，现为 5 列）替换为：

```go
	var enabled int
	var limits QuotaLimits
	limits.RPMLimit = in.Limits.RPMLimit
	var groupID int64
	var groupName string
	var periodDays int64
	var periodStartRaw sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT k.enabled,k.concurrency_limit,k.token_limit,k.amount_limit_microyuan,COALESCE(k.group_id,0),COALESCE(g.name,''),COALESCE(g.token_limit_5h,0),COALESCE(g.amount_limit_5h,0),COALESCE(g.token_limit_daily,0),COALESCE(g.amount_limit_daily,0),COALESCE(g.token_limit_weekly,0),COALESCE(g.amount_limit_weekly,0),COALESCE(g.period_limit_days,0),COALESCE(g.token_limit_period,0),COALESCE(g.amount_limit_period,0),COALESCE(g.period_limit_start,'') FROM client_keys k LEFT JOIN model_groups g ON g.id=k.group_id WHERE k.id=?`, in.KeyID).Scan(&enabled, &concurrency, &tokenLimit, &amountLimit, &groupID, &groupName, &limits.Token5H, &limits.Amount5H, &limits.TokenDaily, &limits.AmountDaily, &limits.TokenWeekly, &limits.AmountWeekly, &periodDays, &limits.TokenPeriod, &limits.AmountPeriod, &periodStartRaw); err != nil {
		return rollback(err)
	}
	limits.PeriodDays = int(periodDays)
	if periodStartRaw.Valid && periodStartRaw.String != "" {
		parsed, parseErr := time.Parse(time.RFC3339, periodStartRaw.String)
		if parseErr == nil {
			limits.PeriodStart = parsed
		}
	}
```

> `PeriodDays` 在 `QuotaLimits` 里是 `int`，SQLite 扫进独立的 `var periodDays int64` 再转。原来就地声明的 `var groupID int64` 语义不变（现在从第 5 列扫出）。

3d. `ReserveRequest` 额度段里所有 `in.Limits.` 引用改为 `limits.`（:361 RPM、:389/:392/:398/:401/:413/:415/:424/:427 的窗口限额）；`:405` 的 `periodQuotaTransition(ctx, conn, in.Limits, started)` 改为 `periodQuotaTransition(ctx, conn, limits, groupID, started)`。

3e. 四个窗口查询加 `AND group_id=?`（参数头插 groupID）：

- :386（5h 滚动）：`... AND julianday(created_at_utc)>=julianday(?,'-5 hours') AND julianday(created_at_utc)<=? AND group_id=?` —— 等等，**参数顺序跟占位符顺序一致**：把 `AND group_id=?` 放 WHERE 末尾时参数放最后。写成：查询串尾加 ` AND group_id=?`，参数列表尾插 `groupID`
- :395（当日 `billing_date_bj`）：同样尾加 ` AND group_id=?`
- :419（每周 `weeklyQuery`）：同上
- periodQuotaTransition 内部两条已由 3b 处理

3f. `internal/relay/service.go` :373-389：`periodStart, _ := time.Parse(...)` 行删除；`Limits: store.QuotaLimits{...}` 收缩为：

```go
		Limits: store.QuotaLimits{RPMLimit: int64(settings.RPMLimit)},
```

3g. `internal/httpapi/admin.go` :251-261：`store.QuotaLimits{...}` 收缩为：

```go
	data, err := h.repo.Overview(r.Context(), time.Now().UTC(), store.QuotaLimits{RPMLimit: int64(settings.RPMLimit)})
```

（`parsePeriodStart`（:273-279）若无其他调用方则删除——`grep -n parsePeriodStart internal/` 确认。）

- [ ] **Step 4: 改写 ledger_test.go（额度来源迁移的测试面）**

4a. `newReservationFixture`（:13-42）改为返回 4 值（多返回组），调用点 13 处机械补第 4 个接收值（`grep -n "newReservationFixture" internal/store/*_test.go` 逐处）：

```go
func newReservationFixture(t *testing.T, tokenLimit, amountLimit int64) (repo *Store, key ClientKey, model Model, group ModelGroup) {
	t.Helper()
	repo = newTestStore(t)
	p, err := repo.CreateProvider(context.Background(), NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000)
	model, err = repo.CreateModel(context.Background(), NewModel{ProviderID: p.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	group, err = repo.CreateModelGroup(context.Background(), NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(context.Background(), group.ID, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	// Leave room for the conservative prompt estimate used by reservations.
	tokenLimit += 100
	key, err = repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 2, TokenLimit: &tokenLimit, AmountLimitMicroyuan: &amountLimit})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(context.Background(), key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	return repo, key, model, group
}
```

4b. `newReservationArgs`（:44-60）的 Limits 收缩：

```go
		// 窗口额度不再来自入参：判定在事务内从密钥所属分组的行读取。
		// 测试要用额度约束时，用 setGroupLimits 把它写到分组上。
		Limits: QuotaLimits{RPMLimit: 30},
```

并加 helper（`intPtr` 在 catalog_test.go）：

```go
// setGroupLimits 把窗口额度写到分组行上——取代旧测试直接改 args.Limits 的做法。
func setGroupLimits(t *testing.T, repo *Store, group ModelGroup, limits QuotaLimits) {
	t.Helper()
	start := ""
	if !limits.PeriodStart.IsZero() {
		start = limits.PeriodStart.Format(time.RFC3339)
	}
	if _, err := repo.UpdateModelGroup(context.Background(), group.ID, UpdateModelGroup{
		TokenLimit5H:      intPtr(limits.Token5H),
		AmountLimit5H:     intPtr(limits.Amount5H),
		TokenLimitDaily:   intPtr(limits.TokenDaily),
		AmountLimitDaily:  intPtr(limits.AmountDaily),
		TokenLimitWeekly:  intPtr(limits.TokenWeekly),
		AmountLimitWeekly: intPtr(limits.AmountWeekly),
		TokenLimitPeriod:  intPtr(limits.TokenPeriod),
		AmountLimitPeriod: intPtr(limits.AmountPeriod),
		PeriodLimitDays:   daysPtr(limits.PeriodDays),
		PeriodLimitStart:  strPtr(start),
	}); err != nil {
		t.Fatal(err)
	}
}
```

> 注意语义差：`setGroupLimits` 显式把**所有窗口**写成给定值（0=不限）。旧测试改写时把原来只覆盖的窗口照抄、其余补 0。

4c. 五个窗口测试逐个改写（`args.Limits.X = ...` → `setGroupLimits(t, repo, group, QuotaLimits{...})`）：

- `TestWeeklyTokenWindowBlocksAfterItIsSpent`（:136）：`repo, key, model, group := newReservationFixture(...)`；删 `args.Limits.TokenWeekly = 50` 行，改 `setGroupLimits(t, repo, group, QuotaLimits{RPMLimit: 30, TokenWeekly: 50})`（在首笔请求前）
- `TestPeriodQuotaCompletionHandsOffToWeeklyWindow`（:153）：改 `setGroupLimits(t, repo, group, QuotaLimits{RPMLimit: 30, TokenPeriod: 50, TokenWeekly: 1000, PeriodStart: time.Now().UTC().Add(-time.Hour), PeriodDays: 3})`，删除原 4 行 args.Limits 赋值；**保留** `args.StartedAt` 递增的行
- `TestPeriodAmountQuotaCompletionHandsOffToWeeklyWindow`（:183）：`setGroupLimits(t, repo, group, QuotaLimits{RPMLimit: 30, TokenPeriod: 1_000_000, AmountPeriod: 1, TokenWeekly: 1_000_000, AmountWeekly: 1, PeriodStart: time.Now().UTC().Add(-time.Hour), PeriodDays: 3})`
- `TestExpiredPeriodHandsOffToWeeklyWindow`（:210）：`setGroupLimits(t, repo, group, QuotaLimits{RPMLimit: 30, TokenPeriod: 1_000_000, TokenWeekly: 50, PeriodStart: now.Add(-25 * time.Hour), PeriodDays: 1})`；`args.StartedAt` 逻辑保留（`now.Add(-24 * time.Hour)` 即原 `PeriodStart.Add(time.Hour)`）
- `TestDailyAmountWindowBlocksAfterItIsSpent`（:237）：`setGroupLimits(t, repo, group, QuotaLimits{RPMLimit: 30, AmountDaily: 1})`
- `TestZeroLimitsMeanUnlimited`（:255）：`args.Limits = QuotaLimits{RPMLimit: 30}` 行删除（args 默认已只有 RPM）；语义不变——分组没设额度 = 全不限

4d. `TestGroupOverviewScopesToGroupProviders`（:355-401）改名为 `TestGroupOverviewScopesToGroupOwnership`，fixture 行改 4 值接收，末尾「未包含任何渠道的组」注释改为「没有任何请求归属的组」（断言不变：group 999 无行）。其余断言数字不变（同口径迁移）。

- [ ] **Step 5: 跑测试确认通过 + 全量回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -count=1
GOPROXY=off go test -count=1 ./...    # 后台
```
预期：全 PASS。

- [ ] **Step 6: 提交**

```bash
cd "$R" && git add internal/store/ledger.go internal/store/ledger_test.go internal/store/group_scoping_test.go internal/relay/service.go internal/httpapi/admin.go
git commit -m "feat(store)!: source window quotas from the key's group inside the reserve transaction

判定链复用既有窗口机制，只换数据来源：ReserveInput.Limits 只带全局
RPMLimit，其余九个额度值在事务内从 model_groups 行读出；四个窗口查询与
periodQuotaTransition 三条查询各加 AND group_id=?（账目行的 group_id 由
阶段 A 落下，切换分组不搬历史）。QuotaError 带分组名与 ID 供运维定位。
旧全局额度入参停止生效。"
```

---

### Task 4: GroupOverview 按归属口径带分组限额；Overview 收缩；删组影响面

**Files:**
- Modify: `internal/store/ledger.go`（`GroupOverview` :582-649 整函数重写；`Overview` :670-830 窗口块删除）
- Modify: `internal/store/models.go`（`CountKeysInGroup` 新增）
- Modify: `internal/httpapi/admin.go`（groupItem DELETE :706-709、groupsList :630-640 紧张度）
- Test: `internal/store/group_scoping_test.go`（追加）

- [ ] **Step 1: 写失败的测试**

`group_scoping_test.go` 末尾追加：

```go
// GroupOverview 的限额来自分组行，且统计口径是「归属该组的请求」（group_id 列），
// 不再是「走组内渠道的请求」——共享渠道不再被重复计入多个组。
func TestGroupOverviewCarriesGroupLimits(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.UpdateModelGroup(ctx, groupA.ID, UpdateModelGroup{TokenLimitDaily: intPtr(5000)}); err != nil {
		t.Fatal(err)
	}
	keyA, err := repo.CreateClientKey(ctx, NewClientKey{Name: "ka", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyA.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "ov-a", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(ctx, res.ID, 10, 20, 0, 30, "completed"); err != nil {
		t.Fatal(err)
	}
	ov, err := repo.GroupOverview(ctx, groupA.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got := ov["daily"].(map[string]any)["limit"].(int64); got != 5000 {
		t.Fatalf("daily limit = %v, want 5000（来自分组行）", got)
	}
	// keyB 的请求不属于 A 组，即使渠道共享：B 组的组概览看得到，A 组看不到
	keyB, err := repo.CreateClientKey(ctx, NewClientKey{Name: "kb", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyB.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	resB, err := repo.ReserveRequest(ctx, newReservationArgs(keyB.ID, modelID, "ov-b", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(ctx, resB.ID, 7, 8, 0, 15, "completed"); err != nil {
		t.Fatal(err)
	}
	ovA, err := repo.GroupOverview(ctx, groupA.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got := ovA["daily"].(map[string]any)["used_tokens"].(int64); got != 30 {
		t.Fatalf("A 组 daily tokens = %d, want 30（B 组的 15 不计入）", got)
	}
	ovB, err := repo.GroupOverview(ctx, groupB.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got := ovB["daily"].(map[string]any)["used_tokens"].(int64); got != 15 {
		t.Fatalf("B 组 daily tokens = %d, want 15", got)
	}
}

// 删组影响面：删除前能数出将解绑多少把密钥（spec 5.1 末尾）。
func TestCountKeysInGroup(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := repo.CountKeysInGroup(ctx, groupA.ID)
	if err != nil || n != 0 {
		t.Fatalf("n = %d, %v", n, err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if n, err = repo.CountKeysInGroup(ctx, groupA.ID); err != nil || n != 1 {
		t.Fatalf("n = %d, %v", n, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run 'TestGroupOverviewCarriesGroupLimits|TestCountKeysInGroup' -count=1
```
预期：FAIL（`daily.limit` 不存在；`CountKeysInGroup` 未定义）。

- [ ] **Step 3: 实现**

3a. `models.go` 加：

```go
// CountKeysInGroup 数出绑定到某分组的密钥数，供删除确认框告知影响面：
// 一对一之后删组会把该组所有密钥变成未绑、立即无法调用（spec 5.1）。
func (s *Store) CountKeysInGroup(ctx context.Context, groupID int64) (int64, error) {
	if groupID <= 0 {
		return 0, errors.New("group ID is required")
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE group_id=?`, groupID).Scan(&n)
	return n, err
}
```

3b. `GroupOverview`（ledger.go:582-649）整函数替换为：

```go
// GroupOverview 返回单个分组的用量与额度。统计口径是「归属该组的请求」
//（账目行的 group_id，由阶段 A 落下）——与额度扣减同口径，否则管理台显示的
//「已用/限额」和实际判定用的数字对不上。共享渠道不再被重复计入多个组。
// 限额来自分组行（v18），与 ReserveRequest 的判定来源一致。
func (s *Store) GroupOverview(ctx context.Context, groupID int64, now time.Time) (map[string]any, error) {
	if groupID <= 0 {
		return nil, errors.New("group ID is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	minuteStart := now.Add(-time.Minute).Format(time.RFC3339Nano)
	fiveHourStart := now.Add(-5 * time.Hour).Format(time.RFC3339Nano)
	hourStart := now.Add(-time.Hour).Format(time.RFC3339Nano)
	date := billingDate(now)
	var limits QuotaLimits
	var groupName string
	var periodDays int64
	var periodStartRaw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(name,''),COALESCE(token_limit_5h,0),COALESCE(amount_limit_5h,0),COALESCE(token_limit_daily,0),COALESCE(amount_limit_daily,0),COALESCE(token_limit_weekly,0),COALESCE(amount_limit_weekly,0),COALESCE(period_limit_days,0),COALESCE(token_limit_period,0),COALESCE(amount_limit_period,0),COALESCE(period_limit_start,'') FROM model_groups WHERE id=?`, groupID).Scan(&groupName, &limits.Token5H, &limits.Amount5H, &limits.TokenDaily, &limits.AmountDaily, &limits.TokenWeekly, &limits.AmountWeekly, &periodDays, &limits.TokenPeriod, &limits.AmountPeriod, &periodStartRaw); err != nil {
		return nil, err
	}
	limits.PeriodDays = int(periodDays)
	if periodStartRaw.Valid && periodStartRaw.String != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, periodStartRaw.String); parseErr == nil {
			limits.PeriodStart = parsed
		}
	}
	var rpm int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE group_id=? AND created_at_utc>=? AND created_at_utc<=?`, groupID, minuteStart, nowText).Scan(&rpm); err != nil {
		return nil, err
	}
	fiveHour, fiveHourAmount, err := s.windowUsage(ctx, `group_id=? AND created_at_utc>=? AND created_at_utc<=?`, groupID, fiveHourStart, nowText)
	if err != nil {
		return nil, err
	}
	daily, dailyAmount, err := s.windowUsage(ctx, `group_id=? AND billing_date_bj=? AND created_at_utc<=?`, groupID, date, nowText)
	if err != nil {
		return nil, err
	}
	weekStart, periodActiveNow := weeklyWindowStart(now, limits)
	var periodTransition time.Time
	if periodActiveNow {
		conn, connErr := s.db.Conn(ctx)
		if connErr != nil {
			return nil, connErr
		}
		periodTransition, _, _, connErr = periodQuotaTransition(ctx, conn, limits, groupID, nowText)
		conn.Close()
		if connErr != nil {
			return nil, connErr
		}
		if !periodTransition.IsZero() {
			periodActiveNow = false
			weekStart = periodTransition
		}
	}
	weekFilter := `group_id=? AND created_at_utc>=? AND created_at_utc<=?`
	weekArgs := []any{groupID, weekStart.Format(time.RFC3339Nano), nowText}
	if !periodTransition.IsZero() {
		weekFilter = `group_id=? AND created_at_utc>? AND created_at_utc<=?`
	}
	weekly, weeklyAmount, err := s.windowUsage(ctx, weekFilter, weekArgs...)
	if err != nil {
		return nil, err
	}
	periodStartText, periodEndText := "", ""
	var periodTokens, periodAmount int64
	if limits.PeriodDays > 0 && !limits.PeriodStart.IsZero() {
		periodStartText = limits.PeriodStart.Format(time.RFC3339Nano)
		periodEndText = limits.PeriodStart.AddDate(0, 0, limits.PeriodDays).Format(time.RFC3339Nano)
		periodTokens, periodAmount, err = s.windowUsage(ctx, `group_id=? AND created_at_utc>=? AND created_at_utc<?`, groupID, periodStartText, periodEndText)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM requests WHERE group_id=? AND created_at_utc>=? AND created_at_utc<=? GROUP BY status`, groupID, hourStart, nowText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	outcomes := map[string]int{"completed": 0, "failed": 0, "aborted": 0, "rejected": 0, "reserved": 0}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		if _, ok := outcomes[status]; ok {
			outcomes[status] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE group_id=? AND status='reserved'`, groupID).Scan(&active); err != nil {
		return nil, err
	}
	denominator := outcomes["completed"] + outcomes["failed"]
	errorRate := 0.0
	if denominator > 0 {
		errorRate = float64(outcomes["failed"]) / float64(denominator)
	}
	return map[string]any{
		"group_id":         groupID,
		"group_name":       groupName,
		"generated_at_utc": nowText,
		"rpm":              map[string]any{"used": rpm},
		"five_hour":        map[string]any{"used_tokens": fiveHour, "limit": limits.Token5H, "used_microyuan": fiveHourAmount, "amount_limit": limits.Amount5H},
		"daily":            map[string]any{"used_tokens": daily, "limit": limits.TokenDaily, "used_microyuan": dailyAmount, "amount_limit": limits.AmountDaily},
		"weekly":           map[string]any{"used_tokens": weekly, "limit": limits.TokenWeekly, "used_microyuan": weeklyAmount, "amount_limit": limits.AmountWeekly, "active": !periodActiveNow, "from_utc": weekStart.Format(time.RFC3339Nano)},
		"period":           map[string]any{"used_tokens": periodTokens, "limit": limits.TokenPeriod, "used_microyuan": periodAmount, "amount_limit": limits.AmountPeriod, "active": periodActiveNow, "start_utc": periodStartText, "end_utc": periodEndText, "transition_utc": func() string {
			if periodTransition.IsZero() {
				return ""
			}
			return periodTransition.Format(time.RFC3339Nano)
		}()},
		"last_hour": map[string]any{"completed": outcomes["completed"], "failed": outcomes["failed"], "aborted": outcomes["aborted"], "rejected": outcomes["rejected"], "reserved": outcomes["reserved"], "error_rate": errorRate},
		"active":    active,
	}, nil
}
```

> `weekly` 的参数拼装用上面 `weekArgs` 的形状（periodTransition 非零时 `>` 边界）；period 块的 `transition_utc` 闭包与 Overview :813 同构，照抄即可。`group_name` 塞进响应供前端弹窗标题核对。

3c. `Overview`（ledger.go:670-830）：删除 :684-726 的五个窗口块（fiveHour/daily/weekly/period 的 windowUsage 与 periodTransition 逻辑），响应 map（:807-813 起）删除 `"five_hour"`/`"daily"`/`"weekly"`/`"period"` 四个键，保留 rpm/last_hour/trend/recent/model_calls/concurrency 等。函数签名不变（`limits QuotaLimits` 只剩 RPMLimit 被用到）。响应注释同步。

3d. `admin.go` groupItem 的 DELETE 分支（:706-709）替换为：

```go
	if len(parts) == 1 && r.Method == http.MethodDelete {
		bound, err := h.repo.CountKeysInGroup(r.Context(), id)
		if err != nil {
			writeAdminError(w, 500, "store_error")
			return
		}
		if err := h.repo.DeleteModelGroup(r.Context(), id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeAdminError(w, 404, "not_found")
				return
			}
			writeAdminError(w, 500, "store_error")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "unbound_keys": bound})
		return
	}
```

> 现场先读 `writeDeleteResult` 的实现，404/错误码语义保持与其一致；`errors` 与 `database/sql` 若 admin.go 未 import 则补。

3e. `admin.go` groupsList（:630-640）附紧张度。先在 `ledger.go` 加批量用量：

```go
// GroupUsageBuckets 返回每个分组在三个主窗口的 (token, 微元) 用量，供分组
// 列表渲染额度紧张标记。周期窗口不参与（按组独立启停，批量算切换点成本高；
// 是否咬住看各组概览弹窗）。三个 map 分别是 5h / 当日 / 本周，值为 [token, 微元]。
func (s *Store) GroupUsageBuckets(ctx context.Context, now time.Time) (five, daily, weekly map[int64][2]int64, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	fiveHourStart := now.Add(-5 * time.Hour).Format(time.RFC3339Nano)
	date := billingDate(now)
	weekStart := billingWeekStart(now).Format(time.RFC3339Nano)
	base := `SELECT group_id,COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND group_id>0 AND `
	five, daily, weekly = make(map[int64][2]int64), make(map[int64][2]int64), make(map[int64][2]int64)
	load := func(into map[int64][2]int64, extra string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, base+extra, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var bucket [2]int64
			if err := rows.Scan(&id, &bucket[0], &bucket[1]); err != nil {
				return err
			}
			into[id] = bucket
		}
		return rows.Err()
	}
	if err = load(five, `created_at_utc>=? AND created_at_utc<=? GROUP BY group_id`, fiveHourStart, nowText); err != nil {
		return nil, nil, nil, err
	}
	if err = load(daily, `billing_date_bj=? AND created_at_utc<=? GROUP BY group_id`, date, nowText); err != nil {
		return nil, nil, nil, err
	}
	if err = load(weekly, `created_at_utc>=? AND created_at_utc<=? GROUP BY group_id`, weekStart, nowText); err != nil {
		return nil, nil, nil, err
	}
	return five, daily, weekly, nil
}
```

> `load` 三次只写 [0]/[1] 两位再从 prev 回填其余四位的写法太绕——**实施时改成三个独立切片返回**更直白：`func (s *Store) GroupUsageBuckets(...) (five, daily, weekly map[int64][2]int64, err error)`，三段同构查询各填一个 map。按这个改。

`groupsList`（:630-640）整函数替换为：

```go
func (h *AdminHandler) groupsList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.ListModelGroups(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	five, daily, weekly, err := h.repo.GroupUsageBuckets(r.Context(), time.Now().UTC())
	if err != nil {
		writeAdminError(w, 500, "usage_error")
		return
	}
	// 紧张度：三个主窗口里「有限额的」取已用/限额的最大百分比；全部不限则 0。
	// ≥100 的组已欠费，下一个请求就会被拒——与 quotaCard 的着色阈值一致。
	// 金额窗口不参与标记：Token 更直观，且金额含预留波动大（取舍，非遗漏）。
	tension := func(limit *int64, used int64) (float64, bool) {
		if limit == nil || *limit <= 0 {
			return 0, false
		}
		p := float64(used) / float64(*limit) * 100
		return p, used >= *limit
	}
	items := make([]map[string]any, 0, len(data))
	for _, g := range data {
		item := map[string]any{"ID": g.ID, "Name": g.Name, "Enabled": g.Enabled, "RateMilli": g.RateMilli, "BoundKeys": g.BoundKeys}
		pct, over := 0.0, false
		for _, c := range []struct {
			limit *int64
			used  [2]int64
		}{
			{g.TokenLimit5H, five[g.ID]},
			{g.TokenLimitDaily, daily[g.ID]},
			{g.TokenLimitWeekly, weekly[g.ID]},
		} {
			p, o := tension(c.limit, c.used[0])
			if o {
				over = true
			}
			if p > pct {
				pct = p
			}
		}
		item["TensionPct"] = math.Round(pct*10) / 10
		item["Overdrawn"] = over
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"data": items})
}
```

（`math` import 补充；响应从 struct 切片变为同键名的 map 切片——前端读的 `ID/Name/Enabled/RateMilli` 键名不变，新增 `BoundKeys/TensionPct/Overdrawn`。）

- [ ] **Step 4: 跑测试确认通过 + 包回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -count=1 && GOPROXY=off go test ./internal/httpapi/ -count=1
```
预期：全 PASS。若 httpapi 有测试断言 groupsList 旧形状（直接解构 struct），按新 map 形状适配。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add internal/store/ledger.go internal/store/models.go internal/httpapi/admin.go internal/store/group_scoping_test.go
git commit -m "feat(store): group overview carries group limits and ownership scope

GroupOverview 统计口径改为「归属该组的请求」并带分组限额（与扣减同口径）；
Overview 删掉全局窗口块（额度显示移到组维度）；分组列表附三窗口紧张度与
绑定密钥数，DELETE 响应带 unbound_keys 供删组确认。"
```

---

### Task 5: 删除全局额度配置 + 启动播种

**Files:**
- Modify: `internal/config/settings.go`（字段 :68-80、解析 :198-247、EnvMap :579-588、`TokenLimits()` :99-102、`AmountLimits()` 约 :103-106、helper `tokenLimit` :365-379 / `amountLimit` / `nonNegativeInt` :345）
- Modify: `internal/config/store.go`（引导模板 :322-334 的 10 个键与注释）
- Modify: `cmd/civic-relay/main.go`（播种 + :443 日志措辞）
- Modify: `internal/httpapi/admin.go`（configSave :322-329 周期保留块删除）
- Modify: `.env.example`（:25-34）
- Modify: `internal/config/settings_test.go`（:65、:100-103、:122 附近断言）、`internal/config/store_test.go`（:128）、`internal/httpapi/admin_test.go`（:408 附近）
- Test: `internal/store/group_scoping_test.go`（播种测试）

- [ ] **Step 1: 写失败的测试（播种幂等）**

`group_scoping_test.go` 末尾追加：

```go
// 播种把全局额度值灌进分组「仍为 NULL」的列：跑两次结果一致，
// 已有值的列不被覆盖（写库成功但 relay.env 删键失败而重跑时，不覆盖运维的手工修改）。
func TestSeedGroupQuotasFillsOnlyNullColumns(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	if _, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g1"}); err != nil {
		t.Fatal(err)
	}
	g2, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g2"})
	if err != nil {
		t.Fatal(err)
	}
	// g2 预先手工设过 5h=999：播种不得覆盖
	if _, err := repo.UpdateModelGroup(ctx, g2.ID, UpdateModelGroup{TokenLimit5H: intPtr(999)}); err != nil {
		t.Fatal(err)
	}
	seed := GlobalQuotaSeed{Token5H: 100000, TokenDaily: 20000}
	n, err := repo.SeedGroupQuotas(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("seeded = %d, want 2", n)
	}
	n2, err := repo.SeedGroupQuotas(ctx, seed)
	if err != nil || n2 != 2 {
		t.Fatalf("重跑播种 = %d, %v；应仍为 2 且无副作用", n2, err)
	}
	listed, err := repo.ListModelGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range listed {
		if g.TokenLimit5H == nil {
			t.Fatalf("%s 未被播种", g.Name)
		}
		want := int64(100000)
		if g.ID == g2.ID {
			want = 999
		}
		if *g.TokenLimit5H != want {
			t.Fatalf("%s token_limit_5h = %d, want %d", g.Name, *g.TokenLimit5H, want)
		}
		if g.TokenLimitDaily == nil || *g.TokenLimitDaily != 20000 {
			t.Fatalf("%s token_limit_daily = %v", g.Name, g.TokenLimitDaily)
		}
		if g.TokenLimitWeekly != nil {
			t.Fatalf("%s 未涉及的窗口被误设：%v", g.Name, g.TokenLimitWeekly)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestSeedGroupQuotasFillsOnlyNullColumns -count=1
```
预期：FAIL，`GlobalQuotaSeed undefined`。

- [ ] **Step 3: 实现**

3a. `ledger.go`（或 models.go，二选一，放 ledger.go 与额度逻辑同处）加：

```go
// GlobalQuotaSeed 携带 relay.env 里遗留的全局额度原值（Task 5 播种用）。
type GlobalQuotaSeed struct {
	Token5H, TokenDaily, TokenWeekly    int64
	Amount5H, AmountDaily, AmountWeekly int64 // 微元
	PeriodDays                          int
	TokenPeriod, AmountPeriod           int64
	PeriodStart                         string // RFC3339，空 = 不设
}

// SeedGroupQuotas 把全局额度值灌进每个分组「仍为 NULL」的额度列。
// 只填 NULL：写库成功但 relay.env 删键失败而重跑时，不会覆盖运维期间的手工修改
//——这是幂等性的全部来由。返回被写入的分组数。
func (s *Store) SeedGroupQuotas(ctx context.Context, seed GlobalQuotaSeed) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE model_groups SET
		token_limit_5h=COALESCE(token_limit_5h,?),
		amount_limit_5h=COALESCE(amount_limit_5h,?),
		token_limit_daily=COALESCE(token_limit_daily,?),
		amount_limit_daily=COALESCE(amount_limit_daily,?),
		token_limit_weekly=COALESCE(token_limit_weekly,?),
		amount_limit_weekly=COALESCE(amount_limit_weekly,?),
		period_limit_days=COALESCE(period_limit_days,?),
		token_limit_period=COALESCE(token_limit_period,?),
		amount_limit_period=COALESCE(amount_limit_period,?),
		period_limit_start=COALESCE(period_limit_start,NULLIF(?,''))`,
		seed.Token5H, seed.Amount5H, seed.TokenDaily, seed.AmountDaily,
		seed.TokenWeekly, seed.AmountWeekly, seed.PeriodDays,
		seed.TokenPeriod, seed.AmountPeriod, seed.PeriodStart)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

3b. `cmd/civic-relay/main.go`：在 `buildApplication` 成功之后、`serveApplication` 之前插入：

```go
	if err := seedGroupQuotas(context.Background(), app.Store, path); err != nil {
		log.Fatal(err)
	}
```

并新增函数（放 main.go 底部）：

```go
// seedGroupQuotas 把 relay.env 里遗留的全局额度原值搬进每个现有分组，然后
// 从 relay.env 删掉这些键。删键本身就是幂等标记：下次启动读不到原值，直接跳过；
// 「只填 NULL 列」保证写库成功但删键失败而重跑时，不覆盖运维期间的手工修改。
// 顺序必须先写库、再写 relay.env——任一步失败都不动 relay.env，下次重试。
func seedGroupQuotas(ctx context.Context, repo *store.Store, path string) error {
	values, err := config.NewStore(path).ReadMapping()
	if err != nil {
		return err
	}
	grab := func(key string) string { return strings.TrimSpace(values[key]) }
	toI64 := func(key string) int64 {
		v, _ := strconv.ParseInt(grab(key), 10, 64)
		return v
	}
	seed := store.GlobalQuotaSeed{
		Token5H:      toI64("TOKEN_LIMIT_5H"),
		TokenDaily:   toI64("TOKEN_LIMIT_DAILY"),
		TokenWeekly:  toI64("TOKEN_LIMIT_WEEKLY"),
		Amount5H:     toI64("AMOUNT_LIMIT_5H"),
		AmountDaily:  toI64("AMOUNT_LIMIT_DAILY"),
		AmountWeekly: toI64("AMOUNT_LIMIT_WEEKLY"),
		TokenPeriod:  toI64("TOKEN_LIMIT_PERIOD"),
		AmountPeriod: toI64("AMOUNT_LIMIT_PERIOD"),
		PeriodStart:  grab("PERIOD_LIMIT_START"),
	}
	days, _ := strconv.Atoi(grab("PERIOD_LIMIT_DAYS"))
	seed.PeriodDays = days
	if seed.Token5H == 0 && seed.TokenDaily == 0 && seed.TokenWeekly == 0 &&
		seed.Amount5H == 0 && seed.AmountDaily == 0 && seed.AmountWeekly == 0 &&
		seed.TokenPeriod == 0 && seed.AmountPeriod == 0 && seed.PeriodDays == 0 && seed.PeriodStart == "" {
		return nil
	}
	n, err := repo.SeedGroupQuotas(ctx, seed)
	if err != nil {
		return fmt.Errorf("seed group quotas: %w", err)
	}
	next, err := config.Parse(values)
	if err != nil {
		return err
	}
	if err := config.NewStore(path).Write(next); err != nil {
		return fmt.Errorf("rewrite relay.env without legacy quota keys: %w", err)
	}
	logging.Infof("全局额度配置已灌入 %d 个分组并从 relay.env 移除（TOKEN_LIMIT_*/AMOUNT_LIMIT_*/PERIOD_LIMIT_*，分组额度在管理台「模型组」维护）", n)
	return nil
}
```

> `config.Parse` 对未知的遗留键宽容（UPSTREAM_BASE_URL 先例）；Settings 删掉额度字段后这 10 个键就是未知键，Parse 不读、Write（走 EnvMap）不写——键就这么消失。main.go 需要 `context`/`strings` import，缺则补。

3c. **settings.go 删除**（逐处）：
- :67-80 的 10 个字段与那三行窗口注释（注释改写为一句「窗口额度已迁到 model_groups（v18），判定在预留事务内读取；RPMLimit 是唯一仍在此处的全局限制」放在 RPMLimit 之后）
- :99-106 的 `TokenLimits()` 与 `AmountLimits()` 方法（grep 确认无其他调用方后删）
- :198-247 的解析块（10 个字段 + PeriodLimitStart 校验 + 自动填 now——自动填 now 已由 UpdateModelGroup 分组版接管）
- :579-588 的 EnvMap 十行
- helper：`tokenLimit`（:365）、`amountLimit`（grep 定位）、`nonNegativeInt`（:345，grep 确认仅剩 PERIOD_LIMIT_DAYS 一个调用方后删）
- `RestartOnlyChanges` 若 grep 到额度键则删（初步查看没有）

3d. `config/store.go` :322-334：删掉 10 个键与那段注释（「六个额度窗口出厂全部不限……」整段），RPM_LIMIT 保留。

3e. `admin.go` :322-329 configSave 的 PERIOD_LIMIT_START 保留块删除（字段已不存在，编译会报错提醒）。

3f. `.env.example` :25-34 删除（注意其中 TOKEN_LIMIT_5H=100000 / TOKEN_LIMIT_DAILY=20000 的非零默认值随之消失）。

3g. 测试适配：`settings_test.go` 删 :65、:100-103、:122 附近的额度断言；`store_test.go:128` 删 AmountLimits 断言；`admin_test.go:408` 附近的 configSave 周期断言删掉（该用例其余部分保留）。逐个跑包测试按编译错误清。

- [ ] **Step 4: 跑测试确认通过 + 全量回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ ./internal/config/ ./internal/httpapi/ -count=1
GOPROXY=off go test -count=1 ./...    # 后台
GOPROXY=off go vet ./...
```
预期：全 PASS。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add -A internal/ cmd/ .env.example
git commit -m "feat(config)!: drop global window quotas in favor of per-group limits

全局额度 10 个配置项（TOKEN_LIMIT_*/AMOUNT_LIMIT_*/PERIOD_LIMIT_*）删除，
运行配置页该区块随 EnvMap 自动消失。升级路径：启动时读 relay.env 原值 →
只灌进各分组仍为 NULL 的额度列 → 原子写回 relay.env 删键（删键即幂等标记，
只填 NULL 保证重跑不覆盖手工修改）。旧 relay.env 的残留键按未知键保留。
"
```

---

### Task 6: 前端——概览收缩、模型组额度 UI、编辑弹窗细节

**Files:**
- Modify: `web/admin.html`

- [ ] **Step 1: 管理员概览去掉四张额度卡片**

`loadOverview()`（约 :628-645）：删除四行 `quotaCard(...)` 调用（5 小时/当日/自定义周期/本周），保留 RPM、并发、近一小时成功率三张 `statCard` 与 `renderRequests`。`quotaCard` 函数本身**保留**（组概览弹窗要用）。

- [ ] **Step 2: 分组列表加紧张标记**

`loadGroups()` 的表格行（:1227-1233）：`<td>状态</td>` 之后加一列「额度」，表头同步；单元格渲染：

```js
      const tension = g.TensionPct != null ? Number(g.TensionPct) : 0;
      const over = !!g.Overdrawn;
      const quotaCell = over
        ? `<span class="tag off">已欠费 ${tension.toFixed(0)}%</span>`
        : tension >= 80
          ? `<span class="tag warn">${tension.toFixed(0)}%</span>`
          : tension > 0 ? `<span class="tag">${tension.toFixed(0)}%</span>` : `<span style="color:var(--muted)">不限</span>`;
```

（tag 的 warn 类若不存在，看 :615 `p >= 80 ? "warn"` —— quotaCard 用了 `qpct ${cls}`，检查 CSS 里 `.warn`/`.full` 是否通用类；不是就内联 style。）

- [ ] **Step 3: 组概览弹窗换额度卡片**

`groupOverview`（:1236-1268）的 `render()`：statCards 改为 RPM + `quotaCard("5 小时额度", "滚动 5 小时，含进行中请求的预留", d.five_hour, "five-hour")` + `quotaCard("当日额度", "北京时间自然日", d.daily, "daily")` + `quotaCard((d.period && d.period.active) ? "自定义周期额度（生效中）" : "自定义周期额度", ..., d.period)` + `quotaCard(...本周..., d.weekly, "weekly")` + 进行中/成功率 statCard——payload 形状与 Overview 旧形状一致，`quotaCard` 零改动。底部说明文字改为：

```js
      <p style="color:var(--muted);font-size:12.5px;margin:4px 0 0">统计口径：仅计入归属本组的请求（按密钥所属分组），与额度扣减同口径；令牌与金额包含未结算的预留部分。限额来自本组的额度设置（在「查看 / 编辑」里维护，0 或留空 = 不限）。</p>
```

- [ ] **Step 4: 组编辑弹窗加 10 个额度字段**

`editGroup`（:1281 起，弹窗 HTML 在 form-grid 里）：倍率字段之后追加（数值输入，留空 = 不限）：

```js
      <div class="field"><label>5 小时 Token 限额（0 或留空 = 不限）</label><input id="eg-t5h" type="number" min="0" value="${g.TokenLimit5H ?? ""}"></div>
      <div class="field"><label>5 小时金额限额，元（0 或留空 = 不限）</label><input id="eg-a5h" type="number" step="0.01" min="0" value="${g.AmountLimit5H != null ? g.AmountLimit5H / 1e6 : ""}"></div>
      <div class="field"><label>当日 Token 限额（0 或留空 = 不限）</label><input id="eg-td" type="number" min="0" value="${g.TokenLimitDaily ?? ""}"></div>
      <div class="field"><label>当日金额限额，元（0 或留空 = 不限）</label><input id="eg-ad" type="number" step="0.01" min="0" value="${g.AmountLimitDaily != null ? g.AmountLimitDaily / 1e6 : ""}"></div>
      <div class="field"><label>本周 Token 限额（0 或留空 = 不限）</label><input id="eg-tw" type="number" min="0" value="${g.TokenLimitWeekly ?? ""}"></div>
      <div class="field"><label>本周金额限额，元（0 或留空 = 不限）</label><input id="eg-aw" type="number" step="0.01" min="0" value="${g.AmountLimitWeekly != null ? g.AmountLimitWeekly / 1e6 : ""}"></div>
      <div class="field"><label>自定义周期长度（天；0 或留空 = 关闭）</label><input id="eg-pd" type="number" min="0" value="${g.PeriodLimitDays ?? ""}"></div>
      <div class="field"><label>自定义周期 Token 限额（0 或留空 = 不限）</label><input id="eg-tp" type="number" min="0" value="${g.TokenLimitPeriod ?? ""}"></div>
      <div class="field"><label>自定义周期金额限额，元（0 或留空 = 不限）</label><input id="eg-ap" type="number" step="0.01" min="0" value="${g.AmountLimitPeriod != null ? g.AmountLimitPeriod / 1e6 : ""}"></div>
```

保存回调里 PUT body 追加（空串 → 不传，让后端 nil=不动；填 0 → 显式不限）：

```js
    const numOrNull = (id, micro) => {
      const raw = $(id).value.trim();
      if (raw === "") return null;
      const v = Number(raw);
      if (!Number.isFinite(v) || v < 0) throw new Error("额度不能为负数");
      return micro ? Math.round(v * 1e6) : Math.round(v);
    };
    body 的构造处：await apiJSON("/admin/api/groups/" + id, { method: "PUT", ..., body: JSON.stringify({ name, rate, enabled,
      token_limit_5h: numOrNull("eg-t5h", false), amount_limit_5h: numOrNull("eg-a5h", true),
      token_limit_daily: numOrNull("eg-td", false), amount_limit_daily: numOrNull("eg-ad", true),
      token_limit_weekly: numOrNull("eg-tw", false), amount_limit_weekly: numOrNull("eg-aw", true),
      period_limit_days: numOrNull("eg-pd", false), token_limit_period: numOrNull("eg-tp", false),
      amount_limit_period: numOrNull("eg-ap", true) }) });
```

> **语义注意**：不传（null）= 后端「保持原值」。这意味着「想把某个已设额度清成不限」必须**填 0** 而不是清空——把这条写进每个 label（已是「0 或留空 = 不限」的表述与此矛盾！）改为「**0 = 不限；留空 = 保持原值**」，上面 9 个 label 统一用这个口径。

- [ ] **Step 5: 删组确认带影响面**

`delGroup`（:1404-1409）：

```js
window.delGroup = id => {
  const g = cache.groups.find(x => x.ID === id);
  if (!g) return;
  const n = g.BoundKeys || 0;
  removeResource(`/admin/api/groups/${id}`,
    n > 0
      ? `删除模型组「${g.Name}」？将解绑 ${n} 个客户端密钥，它们会立即无法调用任何模型（可改绑到其他分组恢复）。`
      : `删除模型组「${g.Name}」？当前没有密钥绑定到该组。`,
    loadGroups);
};
```

- [ ] **Step 6: 编辑密钥弹窗——停用组可见 + 条件 PUT（阶段 A 移交项 1）**

`editKey`（:1528 起）：
6a. items 构造改为「启用组 + 当前组（即使停用）」：

```js
  const items = cache.groups.filter(g => g.Enabled || g.ID === groupId).map(g => ({ id: g.ID, label: g.Enabled ? g.Name : `${g.Name}（已停用）` }));
```

6b. 保存回调：`PUT /keys/{id}/group` 只在选中值变化时发：

```js
    const pickedEl = document.querySelector("#edit-body input[name='ek-group']:checked");
    const picked = pickedEl ? Number(pickedEl.value) : 0;
    if (picked !== (groupId ?? 0)) {
      await apiJSON("/admin/api/keys/" + id + "/group", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ group_id: picked }) });
    }
```

（停用组保持原选时不发 PUT，绕开 SetKeyGroup 对停用组的 400。）

- [ ] **Step 7: 运行配置页清理**

cfgLabels（:1738-1747）删 10 条；:1763 的 `["quota", "额度与周期", [...]]` 分组键列表删掉 10 个额度键（`RPM_LIMIT` 保留，分组名改「限流」）；:1773 的 `PERIOD_LIMIT_DAYS` 特判与 :1782-1784 的 ±天按钮逻辑删除（无该字段即成死代码）。

- [ ] **Step 8: 构建与手验**

```bash
cd "$R" && GOPROXY=off go build ./...
```

起一次性实例（$TEMP 工作目录，参照阶段 A Task 6 的做法），浏览器或 DOM 脚本验证：
1. 管理员概览只有 RPM/并发/成功率/最近请求，无额度卡片
2. 模型组列表出现「额度」列（未设 → 不限）
3. 编辑组设当日 Token=5000 保存 → 重开弹窗回显 5000；组概览弹窗出现四张额度卡且当日显示 0 / 5,000
4. 删组确认框显示「将解绑 N 个客户端密钥」
5. 运行配置页无「额度与周期」区块（只剩限流 RPM_LIMIT）

验证完杀进程。**主会话会再做一轮浏览器验证。**

- [ ] **Step 9: 提交**

```bash
cd "$R" && git add web/admin.html
git commit -m "feat(admin): per-group quota editing and tension markers

管理员概览摘掉全局额度卡片（限额来源已不存在，显示「不限」是误导）；
分组列表加三窗口紧张标记（≥80% 黄、≥100% 红已欠费）；组概览弹窗用
quotaCard 渲染四窗口已用/限额；组编辑弹窗 10 个额度字段（0=不限、
留空=保持原值）；删组确认带将解绑的密钥数；密钥编辑弹窗保留已停用的
当前组并只在改绑时发 PUT（避免静默解绑与停用组 400）。"
```

---

### Task 7: 文档 + 全量验证

**Files:**
- Modify: `README.md`（额度章节）、`docs/admin-api.md`、`docs/changelog.md`

- [ ] **Step 1: README 额度章节改口径**

`grep -n "额度" README.md` 定位（约在「配额与限制」节，21KB 里占大段）。把「全局 5h/日/周 × Token/金额 + 自定义周期」的描述改为分组口径，要点：

- 额度按**分组**分配：4 窗口（5h 滚动 / 北京时间当日 / 本周 / 自定义周期）× Token/金额，在管理台「模型组 → 查看 / 编辑」维护
- 判定来源：密钥所属分组（事务内读取，改完下一次请求即生效）；`0` 或留空 = 不限，出厂默认全不限
- 先欠费后停服、429 错误码（`token_quota_exceeded` / `amount_quota_exceeded` / `key_*`）语义不变，detail 带分组名与 ID
- Key 级总量额度与 RPM/并发/内存闸门不变；**全局额度配置项已删除**，旧 relay.env 残留键按未知键保留，升级时自动灌进各分组

- [ ] **Step 2: docs/admin-api.md 同步**

`grep -n "TOKEN_LIMIT\|group" docs/admin-api.md` 定位：
- groups PUT：补 9 个额度字段（`token_limit_5h` 等，nil=保持原值、0=不限、负数 400）与 `PeriodLimitStart` 自动填充行为
- groupsList 响应：补 `BoundKeys` / `TensionPct` / `Overdrawn`
- `DELETE /admin/api/groups/{id}`：响应带 `unbound_keys`
- `GET /admin/api/groups/{id}/overview`：响应形状（four windows with limits）与「归属该组的请求」口径
- 若有全局额度配置的描述段落，删或改

- [ ] **Step 3: changelog 加 v0.5.0 草稿**

`docs/changelog.md` 顶部按既有格式加 v0.5.0 草稿（改了什么/为什么/踩过的坑：概览限额误导、口径统一、播种幂等）。

- [ ] **Step 4: e2e 欠账记录**

在 `docs/changelog.md` 的 v0.5.0 草稿「已知但尚未实现」节写明：`_e2e/run_e2e.py` 的额度对照断言依赖全局额度配置，本版本后失效，待本机装 Python 后改为分组口径重跑。

- [ ] **Step 5: 全量验证**

```bash
cd "$R" && gofmt -l internal/ cmd/ ; GOPROXY=off go vet ./...
GOPROXY=off go test -count=1 ./...    # 后台
```
预期：全 PASS。

- [ ] **Step 6: 提交**

```bash
cd "$R" && git add README.md docs/admin-api.md docs/changelog.md
git commit -m "docs: switch quota documentation to per-group allocation"
```
