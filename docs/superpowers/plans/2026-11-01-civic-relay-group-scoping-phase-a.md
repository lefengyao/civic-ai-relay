# Civic Relay 阶段 A：数据模型归位（密钥与分组一对一）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 删掉 `key_groups` 多对多表，改用 `client_keys.group_id` 一对一；两张账目表落 `group_id` 使「切换分组」不搬历史；授权与计价三处 SQL 改写。不改权限、不改额度——那是阶段 B/C。

**Architecture:** 先加列回填（v16，保留 `key_groups` 作数据源），再切读取路径与写入路径（`SetKeyGroup`），最后删表建索引（v17）。每个 commit 保持 `go build`/`go vet`/`go test` 全绿。

**Tech Stack:** Go 1.27 + modernc.org/sqlite（纯 Go，无 CGO）；前端为 `web/admin.html` 单文件内嵌，无构建步骤。

**Spec:** `docs/superpowers/specs/2026-11-01-civic-relay-group-scoping-design.md`（第五、八节 + 第十三节阶段 A）

---

## 本机执行约束（每个 Task 都适用）

- Go 在 `/c/Program Files/Go/bin`；构建与测试必须 `GOPROXY=off`（依赖已在模块缓存，官方 proxy 握手超时）
- `go test ./...` **必须后台跑**（`run_in_background` 或 `&`），前台会撞 120 秒超时被 SIGTERM 且输出为空
- 工作目录：`E:\生产项目\项目\TrailblazerStation\civic-ai-relay`（下文简写 `$R`）
- `git push` 需 `-c http.proxy=http://127.0.0.1:7890 -c https.proxy=http://127.0.0.1:7890`，**不要**改 git config
- 提交信息用英文 Conventional Commits（仓库既有风格）

## 与 spec 的两处偏差（已在计划里定案，实施时不要再改回去）

1. **迁移拆成 v16 + v17 两条**，而不是 spec 说的单条 v16。原因：回填必须从 `key_groups` 读历史归属，所以加列（v16）与删表（v17）不能在同一条迁移里；拆开后每个 commit 都能保持测试绿。
2. **spec §8.5 的「三态」退化为显式设值/解绑**。三态（缺省=不动）是为「分组挂在主 PUT payload 里」设计的；实现保留独立路由 `keys/{id}/group`，前端总是显式传全量状态（绑定值或 0），不存在「缺省」场景，因此不需要 TTL 那种「留空不改」的测试。

---

### Task 1: migration v16 — 加列与回填（保留 key_groups）

**Files:**
- Modify: `internal/store/schema.go`（`migrations` 切片末尾，v15 之后）
- Test: `internal/store/migration_test.go`（新建）

- [ ] **Step 1: 写失败的测试**

新建 `internal/store/migration_test.go`：

```go
package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openLegacyV15 在临时文件上手工执行 v1..v15 的迁移语句，构造一个「升级前」的
// 库。迁移是包级变量，测试与实现在同一个包里，可以直接遍历执行。
func openLegacyV15(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "legacy.db")) + "?_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, m := range migrations {
		if m.version > 15 {
			break
		}
		for _, stmt := range m.statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("apply legacy migration v%d: %v", m.version, err)
			}
		}
	}
	return db
}

func applyMigration(db *sql.DB, t *testing.T, version int) {
	t.Helper()
	for _, m := range migrations {
		if m.version != version {
			continue
		}
		for _, stmt := range m.statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("apply migration v%d: %v", version, err)
			}
		}
		return
	}
	t.Fatalf("migration v%d not found", version)
}

// 回填规则必须是「倍率最大的组，同倍率取 id 最小」——这是旧 MAX(g.rate_milli)
// 的计价口径，保证曾经多组的密钥迁移后计费倍率不变。
func TestMigrationV16BackfillPicksHighestRate(t *testing.T) {
	db := openLegacyV15(t)
	const ts = `'2026-01-01T00:00:00Z'`
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO providers(id,name,base_url,api_key_ciphertext,enabled,created_at_utc,updated_at_utc) VALUES (1,'p','https://p.example',x'00',1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO models(id,provider_id,public_name,upstream_name,enabled,created_at_utc,updated_at_utc) VALUES (1,1,'m','m',1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO model_groups(id,name,rate_milli,enabled,created_at_utc,updated_at_utc) VALUES (1,'cheap',1000,1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO model_groups(id,name,rate_milli,enabled,created_at_utc,updated_at_utc) VALUES (2,'premium',2000,1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO client_keys(id,name,token_digest,enabled,concurrency_limit,created_at_utc,updated_at_utc) VALUES (7,'alice',x'00',1,1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO key_groups(key_id,group_id) VALUES (7,1),(7,2)`)
	mustExec(`INSERT INTO key_reservations(key_id,model_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (7,1,10,10,'completed',` + ts + `)`)
	mustExec(`INSERT INTO requests(request_id,key_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,created_at_utc) VALUES ('r1',7,1,1,'completed',1,1,1,` + ts + `)`)

	applyMigration(db, t, 16)

	var groupID sql.NullInt64
	if err := db.QueryRow(`SELECT group_id FROM client_keys WHERE id=7`).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if !groupID.Valid || groupID.Int64 != 2 {
		t.Fatalf("client_keys.group_id = %v, want 2（倍率最大者）", groupID)
	}
	var resGroup, reqGroup int
	if err := db.QueryRow(`SELECT group_id FROM key_reservations WHERE key_id=7`).Scan(&resGroup); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT group_id FROM requests WHERE request_id='r1'`).Scan(&reqGroup); err != nil {
		t.Fatal(err)
	}
	if resGroup != 2 || reqGroup != 2 {
		t.Fatalf("账目回填 = %d, %d, want 2, 2", resGroup, reqGroup)
	}
	// v16 只加列回填，key_groups 由 v17 删
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='key_groups'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Fatal("v16 不应删除 key_groups（回填要以它为数据源）")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestMigrationV16BackfillPicksHighestRate -count=1
```
预期：FAIL，`migration v16 not found`。

- [ ] **Step 3: 实现 v16**

在 `internal/store/schema.go` 的 `migrations` 切片末尾（v15 条目之后）追加：

```go
	// v16: 密钥与分组改为一对一（设计见 docs/superpowers/specs/2026-11-01-…
	// -group-scoping-design.md 第五节）。client_keys.group_id 取代 key_groups
	// 多对多表；两张账目表冗余一份 group_id，记录请求发生时的归属 —— 否则
	// 「切换分组」会把历史用量整体搬到新组：旧组已用凭空减少、新组凭空增加，
	// 额度判定被篡改。
	//
	// 回填规则取「倍率最大的组，同倍率取 id 最小」：这正是旧 MAX(g.rate_milli)
	// 的计价口径，保证曾经多组的密钥迁移后计费倍率不变。
	//
	// key_groups 此时**保留**，由 v17 删除 —— 本条回填要以它为数据源。
	// 对曾经属于多个组的密钥，账目表的历史行只能按迁移后的单一归属回填，是近似值。
	{version: 16, statements: []string{
		`ALTER TABLE client_keys ADD COLUMN group_id INTEGER REFERENCES model_groups(id) ON DELETE SET NULL`,
		`UPDATE client_keys SET group_id = (
			SELECT kg.group_id FROM key_groups kg JOIN model_groups g ON g.id = kg.group_id
			WHERE kg.key_id = client_keys.id
			ORDER BY g.rate_milli DESC, g.id ASC LIMIT 1
		)`,
		`ALTER TABLE key_reservations ADD COLUMN group_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN group_id INTEGER NOT NULL DEFAULT 0`,
		`UPDATE key_reservations SET group_id = COALESCE((SELECT group_id FROM client_keys WHERE id = key_reservations.key_id), 0)`,
		`UPDATE requests SET group_id = COALESCE((SELECT group_id FROM client_keys WHERE id = requests.key_id), 0)`,
	}},
```

- [ ] **Step 4: 跑测试确认通过**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestMigrationV16BackfillPicksHighestRate -count=1
```
预期：PASS。

- [ ] **Step 5: 全包回归（此时 key_groups 还在，旧代码路径不受影响）**

```bash
cd "$R" && GOPROXY=off go build ./... && GOPROXY=off go test ./internal/store/ -count=1
```
预期：全 PASS。

- [ ] **Step 6: 提交**

```bash
cd "$R" && git add internal/store/schema.go internal/store/migration_test.go
git commit -m "feat(store): add one-to-one group columns and backfill (migration v16)"
```

---

### Task 2: ClientKey.GroupID 读路径 + SetKeyGroup 写路径

**Files:**
- Modify: `internal/store/keys.go`（struct :20-39、4 处 SELECT :247/:265/:312/:332、`scanClientKey` :342-355、新 `SetKeyGroup`）
- Test: `internal/store/keys_test.go`（追加）

- [ ] **Step 1: 写失败的测试**

在 `internal/store/keys_test.go` 末尾追加（`boolPtr` 定义在 catalog_test.go:258，同包可直接用）：

```go
func TestSetKeyGroupBindsUnbindsAndValidates(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	g, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 新建的 Key 未绑分组
	if fresh, _ := repo.GetClientKey(ctx, key.ID); fresh.GroupID != nil {
		t.Fatalf("新建 Key 不应有分组：%v", fresh.GroupID)
	}

	if err := repo.SetKeyGroup(ctx, key.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetClientKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupID == nil || *got.GroupID != g.ID {
		t.Fatalf("group = %v, want %d", got.GroupID, g.ID)
	}

	// 解绑：0 = 解绑，授权模型随即为空
	if err := repo.SetKeyGroup(ctx, key.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.GetClientKey(ctx, key.ID); got.GroupID != nil {
		t.Fatalf("解绑后 group = %v, want nil", got.GroupID)
	}

	// 不存在的组
	if err := repo.SetKeyGroup(ctx, key.ID, 99999); err == nil {
		t.Fatal("不存在的组被接受")
	}
	// 停用的组（与旧 ReplaceKeyGroups 的校验一致）
	if _, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, g.ID); err == nil {
		t.Fatal("停用的组被接受")
	}
	// 不存在的 Key
	if err := repo.SetKeyGroup(ctx, 99999, g.ID); err == nil {
		t.Fatal("不存在的 Key 被接受")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestSetKeyGroupBindsUnbindsAndValidates -count=1
```
预期：FAIL，`repo.SetKeyGroup undefined`。

- [ ] **Step 3: 实现**

3a. `ClientKey` 结构体（`keys.go:38` 的 `PublicID string` 之后）加：

```go
	// GroupID 为 nil 表示未绑分组（授权模型为空，客户端拉不到模型）。
	// 一对一：一个 Key 只属于一个分组，切换即改绑；历史账目按请求发生时的
	// 归属记录在 key_reservations.group_id / requests.group_id 上，不随切换搬移。
	GroupID *int64
```

3b. 4 处 SELECT 的列清单末尾（`COALESCE(public_id,'')` 之后）统一加 `,group_id`：

- `ListClientKeys`（:247）
- `GetClientKey`（:265）
- `AuthenticateClientKey`（:312）
- `AuthenticateClientApp`（:332）

3c. `scanClientKey`（:342-355）改为：

```go
func scanClientKey(src interface{ Scan(...any) error }) (ClientKey, error) {
	var k ClientKey
	var enabled int
	var tokens, amount, group sql.NullInt64
	var expires sql.NullString
	if err := src.Scan(&k.ID, &k.Name, &enabled, &k.ConcurrencyLimit, &tokens, &amount, &k.DisabledReason, &k.TokenLabel, &expires, &k.PublicID, &group); err != nil {
		return ClientKey{}, err
	}
	k.Enabled = enabled == 1
	k.TokenLimit, k.AmountLimitMicroyuan = nullableInt(tokens), nullableInt(amount)
	k.ExpiresAt = nullableTime(expires)
	if group.Valid {
		k.GroupID = &group.Int64
	}
	k.Token = ""
	return k, nil
}
```

3d. 在文件里（`ReplaceKeyGroups` 之前）新增：

```go
// SetKeyGroup 把密钥绑到唯一分组，groupID <= 0 表示解绑（授权模型随即为空）。
// 取代旧的 ReplaceKeyGroups 多对多写入；组必须存在且启用，与旧校验一致。
func (s *Store) SetKeyGroup(ctx context.Context, keyID, groupID int64) error {
	if keyID <= 0 {
		return errors.New("key ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=?`, keyID).Scan(&exists); err != nil || exists == 0 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if groupID > 0 {
		var enabled int
		if err := tx.QueryRowContext(ctx, `SELECT enabled FROM model_groups WHERE id=?`, groupID).Scan(&enabled); err != nil {
			return err
		}
		if enabled != 1 {
			return fmt.Errorf("group %d is disabled", groupID)
		}
		_, err = tx.ExecContext(ctx, `UPDATE client_keys SET group_id=?, updated_at_utc=? WHERE id=?`, groupID, nowUTC(), keyID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE client_keys SET group_id=NULL, updated_at_utc=? WHERE id=?`, nowUTC(), keyID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: 跑测试确认通过 + 全包回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run TestSetKeyGroupBindsUnbindsAndValidates -count=1
cd "$R" && GOPROXY=off go test ./internal/store/ -count=1
```
预期：全 PASS（旧路径 `ReplaceKeyGroups`/`KeyGroupIDs` 此刻仍在，不受影响）。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add internal/store/keys.go internal/store/keys_test.go
git commit -m "feat(store): add SetKeyGroup and expose GroupID on ClientKey"
```

---

### Task 3: 授权与计价切到 group_id；删除多对多（v17）

**Files:**
- Modify: `internal/store/keys.go`（`KeyModelRateMilli` :491-508、`authorizedModelsSelect` :529-533；删 `KeyGroupIDs` :424-445 与 `ReplaceKeyGroups` :447-489）
- Modify: `internal/store/schema.go`（追加 v17）
- Modify: `internal/httpapi/admin.go`（:800-823 keyCreate、:925-950 keyItem 分组路由、:1017-1046 keyConnection）
- Modify: `internal/store/migration_test.go`（v17 断言）
- Modify: `internal/store/db_test.go:34`（表清单去掉 key_groups）
- Modify: `internal/store/catalog_test.go`（:134 与 :213-238 两处多组用例、:166）
- Modify: `internal/store/keys_test.go:240`、`internal/store/delete_test.go:36/:66`、`internal/store/public_test.go:56/:100/:231/:233/:296`、`internal/relay/service_test.go:113`（调用点替换）
- Test: 同上

- [ ] **Step 1: 先写失败的行为测试（替换两个多组用例）**

3-1a. `catalog_test.go` 里 `TestAuthorizedModelsUnionAcrossGroups`（约 :100-142，用例体在 :134 挂两组）整个函数替换为：

```go
func TestAuthorizedModelsFollowSingleGroup(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "main", BaseURL: "https://main.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	off, err := repo.CreateProvider(ctx, NewProvider{Name: "off", BaseURL: "https://off.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateProvider(ctx, off.ID, UpdateProvider{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	price := int64(100)
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "basic", UpstreamName: "basic", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "advanced", UpstreamName: "advanced", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "unpriced", UpstreamName: "unpriced", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	groupA, _ := repo.CreateModelGroup(ctx, NewModelGroup{Name: "main"})
	groupB, _ := repo.CreateModelGroup(ctx, NewModelGroup{Name: "spare"})
	if err := repo.ReplaceGroupProviders(ctx, groupA.ID, []int64{off.ID}); err == nil {
		t.Fatal("disabled provider accepted")
	}
	if err := repo.ReplaceGroupProviders(ctx, groupA.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, groupB.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "alice", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	// 未绑分组：授权模型为空
	if models, err := repo.AuthorizedModels(ctx, key.Token); err != nil || len(models) != 0 {
		t.Fatalf("unbound models = %#v, err = %v", models, err)
	}
	// 授权模型 = 唯一所属组内渠道下的已定价启用模型；未定价模型不授权
	if err := repo.SetKeyGroup(ctx, key.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	models, err := repo.AuthorizedModels(ctx, key.Token)
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %#v, err = %v", models, err)
	}
	// 切换分组：授权模型随之变化（组 B 挂了同一个渠道，这里再挂一个独有渠道来区分）
	providerB, err := repo.CreateProvider(ctx, NewProvider{Name: "only-b", BaseURL: "https://b.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: providerB.ID, PublicName: "b-only", UpstreamName: "b-only", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, groupB.ID, []int64{providerB.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	models, err = repo.AuthorizedModels(ctx, key.Token)
	if err != nil || len(models) != 1 || models[0].PublicName != "b-only" {
		t.Fatalf("after switch models = %#v, err = %v", models, err)
	}
}
```

> 注意：原用例名如果是别的（以 `grep -n "func Test" internal/store/catalog_test.go` 为准），保持替换的是**挂两组断言并集的那个**；函数名可以沿用原名，只换函数体。

3-1b. `catalog_test.go` 里 `TestGroupRateMultiplierPersistenceAndKeyModelRate` 的 :213-238（「单个 key 同时属于两个分组」到「禁用分组不参与倍率」断言 :236-238）替换为：

```go
	// 倍率取自唯一所属组
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, premium.ID); err != nil {
		t.Fatal(err)
	}
	rate, err := repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1500 {
		t.Fatalf("rate = %d, want 1500", rate)
	}
	// 所属组停用：没有启用的组可授权，回落默认倍率
	if _, err := repo.UpdateModelGroup(ctx, premium.ID, UpdateModelGroup{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	rate, err = repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != DefaultGroupRateMilli {
		t.Fatalf("rate after disable = %d, want %d", rate, DefaultGroupRateMilli)
	}
	// 改绑到 cheap：倍率跟随唯一所属组
	if err := repo.SetKeyGroup(ctx, key.ID, cheap.ID); err != nil {
		t.Fatal(err)
	}
	rate, err = repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 800 {
		t.Fatalf("rate after rebind = %d, want 800", rate)
	}
```

> :239 起的「不属于任何组的渠道回落默认倍率」段保留不动——改绑到 cheap 后它仍然成立。

3-1c. `catalog_test.go:166` 的 `ReplaceKeyGroups(ctx, key.ID, []int64{})` 改为：

```go
	if err := repo.SetKeyGroup(ctx, key.ID, 0); err != nil {
		t.Fatal(err)
	}
```

3-1d. 机械替换清单（都是单元素切片调用，语义不变，直接换签名）：

| 文件:行 | 原文 | 替换为 |
|---|---|---|
| `catalog_test.go:159` | `repo.ReplaceKeyGroups(ctx, key.ID, []int64{g.ID})` | `repo.SetKeyGroup(ctx, key.ID, g.ID)` |
| `keys_test.go:240` | `repo.ReplaceKeyGroups(ctx, key.ID, []int64{group.ID})` | `repo.SetKeyGroup(ctx, key.ID, group.ID)` |
| `delete_test.go:36` | `repo.ReplaceKeyGroups(ctx, key.ID, []int64{group.ID})` | `repo.SetKeyGroup(ctx, key.ID, group.ID)` |
| `public_test.go:56` | `repo.ReplaceKeyGroups(ctx, key.ID, []int64{g.ID})` | `repo.SetKeyGroup(ctx, key.ID, g.ID)` |
| `public_test.go:100` | `repo.ReplaceKeyGroups(ctx, key.ID, []int64{group.ID})` | `repo.SetKeyGroup(ctx, key.ID, group.ID)` |
| `public_test.go:231` | `repo.ReplaceKeyGroups(ctx, app.ID, []int64{group.ID})` | `repo.SetKeyGroup(ctx, app.ID, group.ID)` |
| `public_test.go:233` | `repo.ReplaceKeyGroups(ctx, normal.ID, []int64{group.ID})` | `repo.SetKeyGroup(ctx, normal.ID, group.ID)` |
| `public_test.go:296` | `repo.ReplaceKeyGroups(ctx, key.ID, []int64{g.ID})` | `repo.SetKeyGroup(ctx, key.ID, g.ID)` |
| `service_test.go:113` | `repo.ReplaceKeyGroups(ctx, k.ID, []int64{g.ID})` | `repo.SetKeyGroup(ctx, k.ID, g.ID)` |

> 行号以改动前为准；`grep -rn "ReplaceKeyGroups" internal/` 核对一遍，漏一处就是编译错误。

3-1e. `delete_test.go:66-70` **不是**机械替换——它调用的是 `KeyGroupIDs`，改用 `GetClientKey` 的 `GroupID`，顺带把「删组 → 解绑 → Key 保留」这条 spec 语义钉住：

```go
	// Deleting the group unbinds the key (group_id → NULL) but keeps the key.
	if err := repo.DeleteModelGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	if bound, err := repo.GetClientKey(ctx, key.ID); err != nil {
		t.Fatalf("key should survive group delete: %v", err)
	} else if bound.GroupID != nil {
		t.Fatalf("key group after group delete = %v, want nil", bound.GroupID)
	}
```

3-1f. `models.go:202-203` 的 `DeleteModelGroup` 注释已过时（「key grants cascade」），改为：

```go
// DeleteModelGroup removes a group. Its provider memberships cascade; bound
// client keys survive but become unbound (group_id → NULL via ON DELETE SET
// NULL), so they can no longer authorize any model until rebound.
// models themselves are untouched.
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run 'TestAuthorizedModelsFollowSingleGroup|TestAtomicJoinReplacementOnlyChangesPassedRows|TestGroupRateMultiplierPersistenceAndKeyModelRate' -count=1
```

预期三个全 FAIL，原因各不相同（这一步是在钉「测试确实测到了新语义」）：

- `TestAuthorizedModelsFollowSingleGroup`：旧实现读 `key_groups`，`SetKeyGroup` 只写 `client_keys.group_id`，所以「未绑 → 空」会过、但绑组后 `AuthorizedModels` 仍为空 → FAIL
- `TestAtomicJoinReplacementOnlyChangesPassedRows`：`:166` 已改成 `SetKeyGroup(ctx, key.ID, 0)`，但旧授权查询走 `key_groups`，那行还在 → 解绑后仍有 2 个模型 → FAIL
- `TestGroupRateMultiplierPersistenceAndKeyModelRate`：倍率仍按 `MAX`，但 Key 只绑了 premium（1500），断言 1500 会过；真正 FAIL 的是「premium 停用后回落 Default」和「改绑 cheap 后 = 800」——旧查询跟着 `key_groups` 走，那两步根本没发生

- [ ] **Step 3: 改实现**

3-3a. `authorizedModelsSelect`（`keys.go:529-533`）整块替换为：

```go
// authorizedModelsSelect 是「客户端实际能用哪些模型」的唯一口径：
// Key 的唯一所属分组（client_keys.group_id，NULL = 未绑）→ 组内渠道 →
// 渠道下已启用且已定价的模型 → 渠道本身启用。
// 公共 /v1/models 与管理端「接入信息」共用这一份 SQL，避免两处判断漂移出
// 「客户端报 404、管理台却显示已授权」这类最难查的问题。
const authorizedModelsSelect = `SELECT DISTINCT m.id,m.provider_id,m.public_name,m.upstream_name,m.input_price_microyuan,m.output_price_microyuan,m.cached_input_price_microyuan,m.enabled FROM client_keys k JOIN model_groups g ON g.id=k.group_id AND g.enabled=1 JOIN group_providers gp ON gp.group_id=g.id JOIN models m ON m.provider_id=gp.provider_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE `
```

3-3b. `KeyModelRateMilli`（:491-508）整块替换为：

```go
// KeyModelRateMilli returns the billing rate multiplier (in thousandths, 1000 = 1.0x)
// for a model under the key's single group. Falls back to the default rate when the
// key has no group, the group is disabled, or the model's provider is not in it.
func (s *Store) KeyModelRateMilli(ctx context.Context, keyID, modelID int64) (int64, error) {
	if keyID <= 0 || modelID <= 0 {
		return DefaultGroupRateMilli, errors.New("key and model IDs are required")
	}
	var rate sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT g.rate_milli FROM client_keys k JOIN model_groups g ON g.id=k.group_id AND g.enabled=1 JOIN group_providers gp ON gp.group_id=g.id JOIN models m ON m.id=? AND m.provider_id=gp.provider_id WHERE k.id=?`, modelID, keyID).Scan(&rate)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultGroupRateMilli, nil
		}
		return DefaultGroupRateMilli, err
	}
	if !rate.Valid || rate.Int64 <= 0 {
		return DefaultGroupRateMilli, nil
	}
	return rate.Int64, nil
}
```

3-3c. 删除 `KeyGroupIDs`（:424-445）与 `ReplaceKeyGroups`（:447-489）整个函数。

3-3d. `schema.go` 追加 v17：

```go
	// v17: key_groups 的历史使命结束（v16 已按它回填 client_keys.group_id），
	// 删表并建分组维度索引。额度窗口查询在每次请求的事务里按 group_id + 时间
	// 过滤，没这两个索引就是每次请求全表扫。
	{version: 17, statements: []string{
		`DROP INDEX IF EXISTS idx_key_groups_group`,
		`DROP TABLE key_groups`,
		`CREATE INDEX idx_key_reservations_group_time ON key_reservations(group_id, created_at_utc)`,
		`CREATE INDEX idx_requests_group_time ON requests(group_id, created_at_utc)`,
	}},
```

3-3e. `migration_test.go` 的 `TestMigrationV16BackfillPicksHighestRate` 末尾（key_groups 存在断言之后）追加：

```go
	applyMigration(db, t, 17)
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='key_groups'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("v17 应删除 key_groups")
	}
```

3-3f. `db_test.go:34` 的表清单：删掉 `"key_groups"`，其余不动。

3-3g. `admin.go` 机械替换（保持编译与既有测试绿；API 语义打磨在 Task 5）：

- :800 的 `GroupIDs []int64  json:"group_ids"` 改为 `GroupID *int64  json:"group_id"`
- :821-823 改为：

```go
	if err == nil && p.GroupID != nil && *p.GroupID > 0 {
		err = h.repo.SetKeyGroup(r.Context(), k.ID, *p.GroupID)
	}
```

- :925-950 的 `groups` 路由块整块替换为：

```go
	if len(parts) == 2 && parts[1] == "group" {
		if r.Method == http.MethodGet {
			key, err := h.repo.GetClientKey(r.Context(), id)
			if err != nil {
				writeAdminError(w, 404, "not_found")
				return
			}
			var groupID any
			if key.GroupID != nil {
				groupID = *key.GroupID
			}
			writeJSON(w, 200, map[string]any{"data": groupID})
			return
		}
		if r.Method == http.MethodPut {
			var p struct {
				GroupID int64 `json:"group_id"`
			}
			if json.NewDecoder(r.Body).Decode(&p) != nil {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			if err := h.repo.SetKeyGroup(r.Context(), id, p.GroupID); err != nil {
				writeAdminError(w, 400, "key_invalid")
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
```

- :1017-1046（keyConnection 的 groupIDs/groups 段）替换为：

```go
	// 一对一：接入信息里的分组是单个对象。组详情拿不到时降级为「只有 ID」，
	// 不影响接入信息整体可用。
	var group any
	enabledGroup := false
	if key.GroupID != nil {
		entry := map[string]any{"id": *key.GroupID, "name": "", "enabled": false, "rate_milli": store.DefaultGroupRateMilli}
		if catalog, err := h.repo.ListModelGroups(r.Context()); err == nil {
			for _, g := range catalog {
				if g.ID == *key.GroupID {
					entry["name"] = g.Name
					entry["enabled"] = g.Enabled
					entry["rate_milli"] = g.RateMilli
					enabledGroup = g.Enabled
					break
				}
			}
		} else {
			entry["enabled"] = true
			enabledGroup = true
		}
		group = entry
	}
```

- :1074-1080 的 issues 判定替换为：

```go
	if key.GroupID == nil {
		issues = append(issues, "该 Key 未绑定任何分组：客户端拉不到模型，对话返回 400 model_not_allowed")
	} else if !enabledGroup {
		issues = append(issues, "所属分组已停用：授权模型为空")
	}
```

- :1112 的 `"groups":          groups,` 改为 `"group":           group,`

- [ ] **Step 4: 全量回归**

```bash
cd "$R" && GOPROXY=off go build ./... && GOPROXY=off go vet ./... && GOPROXY=off go test -count=1 ./...
```
（`go test ./...` 后台跑，约 2 分钟。）预期：11 个包全 PASS。若 `public_test.go`/`service_test.go` 有编译错误，按 Step 1-3d 的机械替换补齐。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add -A internal/ && git status --short
git commit -m "feat(store)!: switch key-group relation to one-to-one (migration v17)

client_keys.group_id 取代 key_groups 多对多表；授权口径
authorizedModelsSelect 与计价 KeyModelRateMilli 改走单一分组；
ReplaceKeyGroups/KeyGroupIDs 删除，由 SetKeyGroup 取代。
管理 API 与接入信息同步改为单分组形状。"
```

---

### Task 4: 预留事务落 group_id + 未绑短路 + 计价查询重写

**Files:**
- Modify: `internal/store/ledger.go`（:320 读 Key、:334 计价查询、:447/:455 两条 INSERT）
- Test: `internal/store/group_scoping_test.go`（新建）

- [ ] **Step 1: 写失败的测试**

新建 `internal/store/group_scoping_test.go`：

```go
package store

import (
	"context"
	"strings"
	"testing"
)

func setupGroupScope(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	ctx := context.Background()
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "p", BaseURL: "http://localhost:1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(100)
	model, err := repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return repo, provider.ID, model.ID
}

// 未绑分组的密钥没有可授权的模型，预留必须被拒——与旧版「无 key_groups 行 →
// 计价查询空」走同一个错误出口。
func TestReserveRejectsUnboundKey(t *testing.T) {
	repo, _, modelID := setupGroupScope(t)
	key, err := repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, modelID, "unbound", 600))
	if err == nil || !strings.Contains(err.Error(), "model is not authorized for key") {
		t.Fatalf("err = %v", err)
	}
}

// 切换分组不能搬历史：账目行的 group_id 记录请求发生时的归属。
func TestSwitchingGroupDoesNotMoveHistory(t *testing.T) {
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
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "in-a", 600)); err != nil {
		t.Fatal(err)
	}
	var inA int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupA.ID).Scan(&inA); err != nil {
		t.Fatal(err)
	}
	if inA != 1 {
		t.Fatalf("A 组预留数 = %d", inA)
	}

	if err := repo.SetKeyGroup(ctx, key.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	// 历史行不动
	var stillA int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupA.ID).Scan(&stillA); err != nil {
		t.Fatal(err)
	}
	if stillA != 1 {
		t.Fatalf("切换分组搬动了历史：A 组行数变成 %d", stillA)
	}
	// 新请求记到新组
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "in-b", 600)); err != nil {
		t.Fatal(err)
	}
	var inB int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupB.ID).Scan(&inB); err != nil {
		t.Fatal(err)
	}
	if inB != 1 {
		t.Fatalf("B 组预留数 = %d", inB)
	}
}

// 计价倍率取自唯一所属组（走预留事务里的报价查询，不只是 KeyModelRateMilli）。
func TestReserveQuotesRateFromSingleGroup(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	premium, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "premium", RateMilli: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, premium.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, premium.ID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "quoted", 600))
	if err != nil {
		t.Fatal(err)
	}
	if res.QuoteRateMilli != 1500 {
		t.Fatalf("QuoteRateMilli = %d, want 1500", res.QuoteRateMilli)
	}
}
```

> 三个测试共用开头的 `setupGroupScope`（provider 建在全新库上，ID 恒为 1，与 `newReservationArgs` 里写死的 `ProviderID: 1` 一致）。

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run 'TestReserve|TestSwitchingGroup' -count=1
```
预期：**三个全 FAIL，且错误都是 `no such table: key_groups`**。原因：Task 3 的 v17 已删表，而 `ledger.go:334` 的计价查询还在 JOIN `key_groups`——每次 ReserveRequest 在计价一步就炸。这正好证明三个测试都打在这条要改的路径上；Step 3 改完后它们必须转绿。

- [ ] **Step 3: 改 ledger.go**

3a. :320 的读 Key 查询改为（新增第 5 列与扫描目标）：

```go
	var groupID int64
	if err := conn.QueryRowContext(ctx, `SELECT enabled,concurrency_limit,token_limit,amount_limit_microyuan,COALESCE(group_id,0) FROM client_keys WHERE id=?`, in.KeyID).Scan(&enabled, &concurrency, &tokenLimit, &amountLimit, &groupID); err != nil {
		return rollback(err)
	}
```

> 注意：原代码的变量声明形状以现场为准（`var enabled, concurrency int` 之类），只要求**新增 `var groupID int64` 并在第 5 位扫描**；不要动原有声明风格。

3b. 在并发检查（:330-332）之后、计价查询（:334）之前插入：

```go
	// 未绑分组的密钥没有可授权的模型：与「模型不在组内」同一个错误出口。
	// 顺序不能动——停用/并发的错误优先级保持与旧版一致。
	if groupID == 0 {
		return rollback(errors.New("model is not authorized for key"))
	}
```

3c. :334 计价查询整条替换为：

```go
	var inputPrice, outputPrice, cachedPrice, rate sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT m.input_price_microyuan,m.output_price_microyuan,COALESCE(m.cached_input_price_microyuan,m.input_price_microyuan),g.rate_milli FROM model_groups g JOIN group_providers gp ON gp.group_id=g.id JOIN models m ON m.id=? AND m.provider_id=gp.provider_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE g.id=? AND g.enabled=1`, in.ModelID, groupID).Scan(&inputPrice, &outputPrice, &cachedPrice, &rate); err != nil {
		return rollback(err)
	}
```

3d. :447 的 INSERT requests：列清单 `provider_id` 之后加 `group_id`，值列表 `in.ProviderID` 之后加 `groupID`：

```go
	result, err := conn.ExecContext(ctx, `INSERT INTO requests(request_id,key_id,model_id,provider_id,group_id,status,input_tokens,reserved_tokens,amount_microyuan,streamed,billing_date_bj,created_at_utc,quote_input_microyuan,quote_output_microyuan,quote_cached_microyuan,quote_rate_milli) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.RequestID, in.KeyID, in.ModelID, in.ProviderID, groupID, "reserved", inputTokens, reservedTokens, amount, boolInt(in.Stream), date, started, inputPrice.Int64, outputPrice.Int64, cachedPrice.Int64, rate.Int64)
```

3e. :455 的 INSERT key_reservations 同样处理：

```go
	result, err = conn.ExecContext(ctx, `INSERT INTO key_reservations(request_id,billing_date_bj,key_id,model_id,group_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (?,?,?,?,?,?,?,?,?)`, in.RequestID, date, in.KeyID, in.ModelID, groupID, reservedTokens, amount, "reserved", started)
```

- [ ] **Step 4: 跑测试确认通过 + 全量回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/store/ -run 'TestReserve|TestSwitchingGroup' -count=1
cd "$R" && GOPROXY=off go test -count=1 ./...
```
预期：全 PASS。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add internal/store/ledger.go internal/store/group_scoping_test.go
git commit -m "feat(store): record group ownership on reservations and requests

预留事务在同一个事务里读出 group_id：未绑分组显式短路（沿用
model_not_allowed 出口），计价查询改按 g.id 直取，两条 INSERT 把请求
发生时的归属落到账目行——切换分组不会搬动历史用量。"
```

---

### Task 5: 管理 API 钉死 + keysList 暴露 GroupID

**Files:**
- Modify: `internal/httpapi/admin.go`（:844-871 keysList 的 item map）
- Test: `internal/httpapi/admin_test.go`（追加）

- [ ] **Step 1: 写失败的测试**

在 `internal/httpapi/admin_test.go` 末尾追加（`adminFixture`/`adminRequest` 是既有 helper，形状见 :605 的 `TestAdminKeyTTLThreeStates`）：

```go
func TestAdminKeyGroupSingleBinding(t *testing.T) {
	handler, adminKey := adminFixture(t)

	created := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey,
		map[string]any{"name": "主组"})
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	var groupBody struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &groupBody); err != nil {
		t.Fatal(err)
	}
	gid := groupBody.Data.ID

	createdKey := adminRequest(t, handler, http.MethodPost, "/admin/api/keys", adminKey,
		map[string]any{"name": "绑定组", "max_concurrency": 1, "group_id": gid})
	if createdKey.Code != http.StatusCreated {
		t.Fatal(createdKey.Body.String())
	}
	var keyBody struct {
		Data struct {
			ID      int64  `json:"ID"`
			GroupID *int64 `json:"GroupID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createdKey.Body.Bytes(), &keyBody); err != nil {
		t.Fatal(err)
	}
	if keyBody.Data.GroupID == nil || *keyBody.Data.GroupID != gid {
		t.Fatalf("创建响应 GroupID = %v, want %d", keyBody.Data.GroupID, gid)
	}
	keyPath := "/admin/api/keys/" + strconv.FormatInt(keyBody.Data.ID, 10)

	// 列表里也要能看到
	listed := adminRequest(t, handler, http.MethodGet, "/admin/api/keys", adminKey, nil)
	if !strings.Contains(listed.Body.String(), `"GroupID":`+strconv.FormatInt(gid, 10)) {
		t.Fatalf("列表缺 GroupID：%s", listed.Body.String())
	}

	// 显式解绑
	if resp := adminRequest(t, handler, http.MethodPut, keyPath+"/group", adminKey,
		map[string]any{"group_id": 0}); resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	current := adminRequest(t, handler, http.MethodGet, keyPath+"/group", adminKey, nil)
	if !strings.Contains(current.Body.String(), `"data":null`) {
		t.Fatalf("解绑后应返回 null：%s", current.Body.String())
	}

	// 不存在的组 → 400 key_invalid
	if resp := adminRequest(t, handler, http.MethodPut, keyPath+"/group", adminKey,
		map[string]any{"group_id": 999999}); resp.Code != http.StatusBadRequest {
		t.Fatalf("不存在的组应 400，实际 %d", resp.Code)
	}
}
```

（import 需要补 `strings`——若文件已有则不用。）

- [ ] **Step 2: 跑测试确认失败**

```bash
cd "$R" && GOPROXY=off go test ./internal/httpapi/ -run TestAdminKeyGroupSingleBinding -count=1
```
预期：FAIL（列表缺 `GroupID` 字段）。

- [ ] **Step 3: 实现**

`admin.go` keysList 的 item map（:852-865）加一个键（`"PublicID": k.PublicID,` 之后）：

```go
			"GroupID": k.GroupID,
```

- [ ] **Step 4: 跑测试确认通过 + 全量回归**

```bash
cd "$R" && GOPROXY=off go test ./internal/httpapi/ -count=1
cd "$R" && GOPROXY=off go test -count=1 ./...
```
预期：全 PASS。

- [ ] **Step 5: 提交**

```bash
cd "$R" && git add internal/httpapi/admin.go internal/httpapi/admin_test.go
git commit -m "feat(admin): expose single group binding on key list and pin API semantics"
```

---

### Task 6: 前端单选

**Files:**
- Modify: `web/admin.html`（:907-912 之后加 `radioListHTML`、:1342-1346、:1528-1560 editKey、:1565-1575 key-form）

前端无构建步骤、无 JS 测试框架，验证方式是起服务后 DOM 手验（仓库既有约定）。

- [ ] **Step 1: 加 `radioListHTML`（`checkListHTML` :907-911 之后，`checkedIds` :912 之前）**

```js
/* 单选列表：与 checkListHTML 共用 .check-list/.check-item 样式，radio 原生单选。
   密钥与分组是一对一——一个密钥只属于一个分组，可切换。组内渠道的多选
   （checkListHTML）不能动，group_providers 靠它维护。 */
function radioListHTML(items, selected, name) {
  return items.length ? `<div class="check-list">` + items.map(x =>
    `<label class="check-item ${selected === x.id ? "on" : ""}"><input type="radio" name="${esc(name)}" value="${x.id}" ${selected === x.id ? "checked" : ""} onchange="this.closest('.check-list').querySelectorAll('.check-item').forEach(i=>i.classList.remove('on'));this.closest('.check-item').classList.toggle('on',this.checked)"><span class="mono">${esc(x.label)}</span></label>`).join("") + `</div>`
    : `<span style="color:var(--muted);font-size:13px">${esc(name)}</span>`;
}
```

- [ ] **Step 2: 改 `renderKeyGroupChecks`（:1342-1346）**

```js
function renderKeyGroupChecks() {
  $("key-group-checks").innerHTML = cache.groups.filter(g => g.Enabled).map(g =>
    `<label class="check-item"><input type="radio" name="key-group" value="${g.ID}" onchange="this.closest('.check-list').querySelectorAll('.check-item').forEach(i=>i.classList.remove('on'));this.closest('.check-item').classList.toggle('on',this.checked)"><span>${esc(g.Name)}</span></label>`).join("")
    || `<span style="color:var(--muted);font-size:13px">暂无模型组，请先创建</span>`;
}
```

- [ ] **Step 3: 改创建表单提交（:1565-1567 的 `$("key-form").onsubmit` 第一行 body 构造）**

```js
  const body = { name: f.get("name"), max_concurrency: Number(f.get("max_concurrency")) };
  const picked = document.querySelector("#key-group-checks input:checked");
  if (picked) body.group_id = Number(picked.value);
```

（原来那行 `group_ids: [...$("key-group-checks").querySelectorAll("input:checked")].map(...)` 整个删掉。）

- [ ] **Step 4: 改 `editKey`（:1528-1560）**

4a. :1531-1532 的读取改为：

```js
  let groupId = null;
  try { groupId = (await apiJSON("/admin/api/keys/" + id + "/group")).data ?? null; } catch {}
```

4b. :1545 的 `checkListHTML(items, groupIds, "暂无模型组，请先创建")` 改为：

```js
      <div class="field" style="margin-top:12px"><label>所属模型组（一个密钥只属于一个分组，可切换）</label>${radioListHTML(items, groupId, "ek-group")}</div>
```

4c. :1556 的 PUT 改为（放在主 PUT 之后，显式传全量状态）：

```js
    const picked = document.querySelector("#edit-body input[name='ek-group']:checked");
    await apiJSON("/admin/api/keys/" + id + "/group", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ group_id: picked ? Number(picked.value) : 0 }) });
```

- [ ] **Step 5: 构建并手验 DOM**

```bash
cd "$R" && GOPROXY=off go build ./... && GOPROXY=off go run ./cmd/civic-relay
```
另开终端（或用测试库副本），浏览器开 `http://127.0.0.1:8000/admin/`，管理员密钥登录后逐项验证：

1. 创建 Key：分组列表是**单选**（点第二个自动取消第一个）；不选也能创建
2. 编辑 Key：弹窗里 radio 正确回显当前组；换成另一组保存后，重新打开回显新组
3. 选「无」保存（radio 无选中态时提交 `group_id: 0`）后，Key 行的接入信息里出现「未绑定任何分组」issue
4. 请求带未绑 Key 打 `/v1/models` → 空；绑回有渠道的组 → 模型回来

> 无头环境下可用 `curl` 验 2/3/4（PUT `/admin/api/keys/{id}/group`、GET `/admin/api/keys/{id}/connection`），radio 交互本身只能在浏览器里确认。

- [ ] **Step 6: 提交**

```bash
cd "$R" && git add web/admin.html
git commit -m "feat(admin): switch key form to single-group radio selection"
```

---

### Task 7: e2e 脚本同步 + 全量验证 + 发布说明

**Files:**
- Modify: `_e2e/run_client_onboarding.py`（:172、:235、:277）
- 无新测试；本任务是收尾验证

- [ ] **Step 1: 同步 e2e 脚本**

`_e2e/run_client_onboarding.py` 三处 `"group_ids": [group_id]` 改为 `"group_id": group_id`（:172 创建「写作软件」、:235 创建「外包A」、:277 创建「我的软件」keyless）。

> 本机没装 Python（只有 WindowsApps 空壳别名），**这个脚本当前跑不了**——改了留着，装了 Python 再跑。别为此装 Python。

- [ ] **Step 2: 全量验证**

```bash
cd "$R" && gofmt -l . && GOPROXY=off go vet ./... && GOPROXY=off go test -count=1 ./...
```
预期：`gofmt` 无输出；vet 干净；11 包全 PASS。

- [ ] **Step 3: 提交**

```bash
cd "$R" && git add _e2e/run_client_onboarding.py
git commit -m "test(e2e): align client onboarding script with single-group API"
```

- [ ] **Step 4: 发布前的停机备份（部署时执行，不是现在）**

按 `docs/deploy-prebuilt.md` 走，版本号用 v0.4.0（版本来自 `build_deploy_bundle.sh` 的参数，代码里无版本常量要改）。**v16/v17 是破坏性迁移且项目没有 down-migration**，升级前必须停容器做一致备份（连 `-wal`/`-shm` 一起拷）。回滚 = 恢复数据库备份 + `.env` 版本改回 v0.3.9 + `docker compose up -d`。
