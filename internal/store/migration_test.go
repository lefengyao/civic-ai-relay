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
}
