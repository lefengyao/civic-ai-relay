package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openLegacyV15 在临时文件上手工执行 v1..v15 的迁移语句，构造一个「升级前」的
// 库。迁移是包级变量，测试与实现在同一个包里，可以直接遍历执行。
// 与 applySchema 的差异：不走事务、不写 schema_migrations 版本记录——
// 本测试只验证语句本身的语义；若将来 applySchema 给迁移加事务外的逻辑，这里测不到。
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
	mustExec(`INSERT INTO model_groups(id,name,rate_milli,enabled,created_at_utc,updated_at_utc) VALUES (3,'premium-alt',2000,1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO client_keys(id,name,token_digest,enabled,concurrency_limit,created_at_utc,updated_at_utc) VALUES (7,'alice',x'00',1,1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO key_groups(key_id,group_id) VALUES (7,1),(7,2)`)
	mustExec(`INSERT INTO key_groups(key_id,group_id) VALUES (7,3)`)
	mustExec(`INSERT INTO key_reservations(key_id,model_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (7,1,10,10,'completed',` + ts + `)`)
	mustExec(`INSERT INTO requests(request_id,key_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,created_at_utc) VALUES ('r1',7,1,1,'completed',1,1,1,` + ts + `)`)
	mustExec(`INSERT INTO client_keys(id,name,token_digest,enabled,concurrency_limit,created_at_utc,updated_at_utc) VALUES (8,'bob',x'01',1,1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO key_reservations(key_id,model_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (8,1,5,5,'completed',` + ts + `)`)
	mustExec(`INSERT INTO requests(request_id,key_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,created_at_utc) VALUES ('r2',8,1,1,'completed',1,1,1,` + ts + `)`)

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
	// 同倍率（2000）的组有两个（id=2 与 id=3），必须取 id 最小的那个
	var resGroup8, reqGroup8 int
	if err := db.QueryRow(`SELECT group_id FROM key_reservations WHERE key_id=8`).Scan(&resGroup8); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT group_id FROM requests WHERE request_id='r2'`).Scan(&reqGroup8); err != nil {
		t.Fatal(err)
	}
	if resGroup8 != 0 || reqGroup8 != 0 {
		t.Fatalf("无分组密钥的账目回填 = %d, %d, want 0, 0", resGroup8, reqGroup8)
	}
	var bound sql.NullInt64
	if err := db.QueryRow(`SELECT group_id FROM client_keys WHERE id=8`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound.Valid {
		t.Fatalf("无分组密钥的 group_id = %v, want NULL", bound)
	}

	applyMigration(db, t, 17)
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='key_groups'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("v17 应删除 key_groups")
	}
	for _, idx := range []string{"idx_key_reservations_group_time", "idx_requests_group_time"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing index %s", idx)
		}
	}

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
}

// v23 的语义补丁：升级时把「没有被任何聚合组包含」的 normal 组改成 aggregate。
//
// 不这么做的话，老库升级后每个既有组都会变成成员组（v23 之前 normal 就是对外
// 服务的组），所有 Key 会同时失效；这种「升级完整个服务不通」的故障代价远高于
// 「个别预留成员组被当成独立组」。被聚合组包含的 normal 组保留成员身份。
func TestMigrationV23PromotesStandaloneNormalGroups(t *testing.T) {
	db := openLegacyV15(t)
	for v := 16; v <= 22; v++ {
		applyMigration(db, t, v)
	}
	const ts = `'2026-01-01T00:00:00Z'`
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO model_groups(id,name,kind,enabled,rate_milli,created_at_utc,updated_at_utc) VALUES (1,'standalone','normal',1,1000,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO model_groups(id,name,kind,enabled,rate_milli,created_at_utc,updated_at_utc) VALUES (2,'member','normal',1,1000,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO model_groups(id,name,kind,enabled,rate_milli,created_at_utc,updated_at_utc) VALUES (3,'owner','aggregate',1,1000,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO group_compositions(parent_group_id,child_group_id) VALUES (3,2)`)

	applyMigration(db, t, 23)

	kindOf := func(id int64) string {
		t.Helper()
		var kind string
		if err := db.QueryRow(`SELECT kind FROM model_groups WHERE id=?`, id).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		return kind
	}
	if got := kindOf(1); got != "aggregate" {
		t.Fatalf("独立组 kind = %q, want aggregate", got)
	}
	if got := kindOf(2); got != "normal" {
		t.Fatalf("成员组 kind = %q, want normal", got)
	}
	if got := kindOf(3); got != "aggregate" {
		t.Fatalf("聚合组 kind = %q, want aggregate", got)
	}
}

// v24 的去重：同一渠道历史上可能挂在多个组，升级时收敛成一条——保留**最早建立**
// 的那条归属（rowid 最小），因为计费一直用的是「最小 ID 成员组」那句话；收敛之后
// 建唯一索引，把「一个渠道只属于一个组」钉在库上。
func TestMigrationV24DedupesProviderGroups(t *testing.T) {
	db := openLegacyV15(t)
	for v := 16; v <= 23; v++ {
		applyMigration(db, t, v)
	}
	const ts = `'2026-01-01T00:00:00Z'`
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO providers(id,name,base_url,api_key_ciphertext,enabled,created_at_utc,updated_at_utc) VALUES (1,'p1','http://up.example','x',1,` + ts + `,` + ts + `)`)
	mustExec(`INSERT INTO providers(id,name,base_url,api_key_ciphertext,enabled,created_at_utc,updated_at_utc) VALUES (2,'p2','http://up2.example','x',1,` + ts + `,` + ts + `)`)
	for _, g := range []struct {
		id         int
		name, kind string
	}{{1, "old", "normal"}, {2, "new", "normal"}, {3, "agg", "aggregate"}} {
		mustExec(`INSERT INTO model_groups(id,name,kind,enabled,rate_milli,created_at_utc,updated_at_utc) VALUES (?,?,?,1,1000,`+ts+`,`+ts+`)`, g.id, g.name, g.kind)
	}
	// p1 先挂 old(#1)、又挂 new(#2)：保留先建立的那条。
	mustExec(`INSERT INTO group_providers(group_id,provider_id) VALUES (1,1)`)
	mustExec(`INSERT INTO group_providers(group_id,provider_id) VALUES (2,1)`)
	// p2 只挂一处，不该被动到。
	mustExec(`INSERT INTO group_providers(group_id,provider_id) VALUES (3,2)`)

	applyMigration(db, t, 24)

	owners := func(providerID int64) []int64 {
		t.Helper()
		rows, err := db.Query(`SELECT group_id FROM group_providers WHERE provider_id=? ORDER BY group_id`, providerID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var gid int64
			if err := rows.Scan(&gid); err != nil {
				t.Fatal(err)
			}
			out = append(out, gid)
		}
		return out
	}
	if got := owners(1); len(got) != 1 || got[0] != 1 {
		t.Fatalf("p1 归属 = %v, want [1]（保留最早建立的那条）", got)
	}
	if got := owners(2); len(got) != 1 || got[0] != 3 {
		t.Fatalf("p2 归属 = %v, want [3]（单归属不应被改动）", got)
	}
	// 唯一索引生效：再插一条重复归属必须失败。
	if _, err := db.Exec(`INSERT INTO group_providers(group_id,provider_id) VALUES (3,1)`); err == nil {
		t.Fatal("v24 之后同一渠道仍能挂进第二个组")
	}
}
