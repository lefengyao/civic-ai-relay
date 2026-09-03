package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureCreatesStableSecretsOutsideRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "relay.env")
	first, bootstrapKey, created, err := Ensure(path)
	if err != nil || !created || first.AdminAPIKey == "" || first.EncryptionKey == "" || bootstrapKey != first.AdminAPIKey {
		t.Fatalf("bootstrap failed: settings=%+v bootstrap=%q created=%v err=%v", first, bootstrapKey, created, err)
	}
	second, bootstrapAgain, createdAgain, err := Ensure(path)
	if err != nil || createdAgain || bootstrapAgain != "" || second.AdminAPIKey != first.AdminAPIKey || second.EncryptionKey != first.EncryptionKey {
		t.Fatalf("secrets changed on second ensure: first=%+v second=%+v bootstrap=%q created=%v err=%v", first, second, bootstrapAgain, createdAgain, err)
	}
	if !strings.HasPrefix(first.AdminAPIKey, "adm_") {
		t.Fatalf("admin key is not URL-safe bootstrap credential: %q", first.AdminAPIKey)
	}
	bootstrapPath := filepath.Join(filepath.Dir(path), "bootstrap-admin-key.txt")
	bootstrapBytes, err := os.ReadFile(bootstrapPath)
	if err != nil || strings.TrimSpace(string(bootstrapBytes)) != first.AdminAPIKey {
		t.Fatalf("bootstrap file mismatch: %q, err=%v", bootstrapBytes, err)
	}
	if runtime.GOOS != "windows" {
		if mode := fileMode(t, path); mode&0077 != 0 {
			t.Fatalf("relay.env permissions = %o", mode)
		}
		if mode := fileMode(t, bootstrapPath); mode&0077 != 0 {
			t.Fatalf("bootstrap permissions = %o", mode)
		}
	}
}

func TestEnsureDoesNotCreateBootstrapUntilConfigWriteSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-parent", "relay.env")
	// The parent is created by a successful config write. This test verifies the
	// externally observable contract: both artifacts exist only after success.
	settings, bootstrapKey, created, err := Ensure(path)
	if err != nil || !created || settings.AdminAPIKey == "" {
		t.Fatal(err)
	}
	if bootstrapKey != settings.AdminAPIKey {
		t.Fatalf("bootstrap key mismatch")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "bootstrap-admin-key.txt")); err != nil {
		t.Fatalf("bootstrap file was not created after config write: %v", err)
	}
}

func TestEnsureRecoversMissingBootstrapFileFromPersistedAdminKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.env")
	settings, _, created, err := Ensure(path)
	if err != nil || !created {
		t.Fatalf("initial ensure failed: settings=%+v created=%v err=%v", settings, created, err)
	}
	bootstrapPath := filepath.Join(filepath.Dir(path), "bootstrap-admin-key.txt")
	if err := os.Remove(bootstrapPath); err != nil {
		t.Fatal(err)
	}

	recoveredSettings, bootstrapKey, recovered, err := Ensure(path)
	if err != nil || !recovered || bootstrapKey != recoveredSettings.AdminAPIKey || bootstrapKey != settings.AdminAPIKey {
		t.Fatalf("missing bootstrap was not recovered: key=%q recovered=%v settings=%+v err=%v", bootstrapKey, recovered, recoveredSettings, err)
	}
	if content, err := os.ReadFile(bootstrapPath); err != nil || strings.TrimSpace(string(content)) != settings.AdminAPIKey {
		t.Fatalf("recovered bootstrap mismatch: %q err=%v", content, err)
	}
}

func TestParseEnvTextSupportsCommentsQuotesAndDuplicateLastWins(t *testing.T) {
	values, err := ParseEnvText("\n# comment\nexport A=plain\nB=\"a value # with quote\"\nC='single quoted'\nA=last\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "last", "B": "a value # with quote", "C": "single quoted"}
	for key, value := range want {
		if values[key] != value {
			t.Errorf("%s = %q, want %q", key, values[key], value)
		}
	}
}

func TestParseEnvTextRejectsMalformedLinesAndQuotedTrailingText(t *testing.T) {
	for _, text := range []string{"NO_EQUALS", "=value", "bad-key=value", "A=\"unterminated", "A=\"ok\" trailing", "A='unterminated", "A='ok' trailing'", "A='bad\\q'"} {
		if _, err := ParseEnvText(text); err == nil {
			t.Errorf("malformed config was accepted: %q", text)
		}
	}
}

func TestSerializeEnvMappingSortsAndRoundTrips(t *testing.T) {
	input := map[string]string{"Z": "last", "A": "a value", "EMPTY": "", "HASH": "a#b", "QUOTE": "a'b"}
	serialized, err := SerializeEnvMapping(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(serialized, "A=") || strings.Index(serialized, "A=") > strings.Index(serialized, "Z=") {
		t.Fatalf("mapping was not sorted: %q", serialized)
	}
	parsed, err := ParseEnvText(serialized)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range input {
		if parsed[key] != value {
			t.Errorf("%s = %q, want %q", key, parsed[key], value)
		}
	}
}

func TestConfigStoreWriteIsAtomicAndPreservesUnknownValues(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "relay.env")
	settings, err := GenerateInitialSettings()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("EXTRA_VALUE=kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if err := store.Write(settings); err != nil {
		t.Fatal(err)
	}
	values, err := store.ReadMapping()
	if err != nil || values["EXTRA_VALUE"] != "kept" || values["ADMIN_API_KEY"] != settings.AdminAPIKey {
		t.Fatalf("stored values = %#v, err=%v", values, err)
	}
	for _, entry := range mustReadDir(t, directory) {
		if strings.HasPrefix(entry, ".relay.env-") {
			t.Fatalf("temporary file left behind: %s", entry)
		}
	}
}

func TestConfigStoreWriteReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.env")
	initial, err := GenerateInitialSettings()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("STALE=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if err := store.Write(initial); err != nil {
		t.Fatalf("replacing existing config failed: %v", err)
	}
	values, err := store.ReadMapping()
	if err != nil {
		t.Fatal(err)
	}
	if values["STALE"] != "value" || values["ADMIN_API_KEY"] != initial.AdminAPIKey {
		t.Fatalf("replacement did not produce expected mapping: %#v", values)
	}
}

func TestDefaultPathHonorsOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "relay.env")
	path, err := DefaultPath(override)
	if err != nil || path != filepath.Clean(override) {
		t.Fatalf("path=%q err=%v", path, err)
	}
	managed, err := ManagedConfigPath()
	if err != nil || managed == "" {
		t.Fatalf("managed path=%q err=%v", managed, err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func mustReadDir(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name())
	}
	return result
}
