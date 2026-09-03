package store

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"civic-ai-relay/internal/secret"
)

func testBox(t *testing.T) *secret.Box {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	box, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestOpenInitializesWALAndSchema(t *testing.T) {
	box := testBox(t)
	db, err := Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.JournalMode(context.Background()); got != "wal" {
		t.Fatalf("journal_mode = %q", got)
	}
	for _, table := range []string{"providers", "models", "model_groups", "group_models", "client_keys", "key_groups", "key_reservations", "requests"} {
		var count int
		if err := db.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing table %s", table)
		}
	}
}

func TestOpenEscapesSQLiteFileURI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir # with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(dir, "relay.db"), testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.Exec(`INSERT INTO model_groups(name, created_at_utc, updated_at_utc) VALUES ('escaped', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaEnforcesForeignKeysAndChecks(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "relay.db"), testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var foreignKeys int
	if err := db.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d", foreignKeys)
	}
	if _, err := db.db.Exec(`INSERT INTO client_keys(name, token_digest, enabled, concurrency_limit, created_at_utc) VALUES ('bad', X'01', 2, 1, 'now')`); err == nil {
		t.Fatal("enabled CHECK accepted invalid value")
	}
	if _, err := db.db.Exec(`INSERT INTO key_reservations(key_id, model_id, reserved_tokens, reserved_amount_microyuan, status, created_at_utc) VALUES (999, 999, 0, 0, 'reserved', 'now')`); err == nil {
		t.Fatal("foreign key CHECK accepted missing references")
	}
	if _, err := db.db.Exec(`INSERT INTO key_reservations(key_id, model_id, reserved_tokens, reserved_amount_microyuan, status, created_at_utc) VALUES (1, 1, -1, 0, 'reserved', 'now')`); err == nil {
		t.Fatal("reserved_tokens CHECK accepted negative value")
	}
}

func TestStoreCloseIsIdempotent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "relay.db"), testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
}
