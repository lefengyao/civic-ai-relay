package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"

	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

func TestInstallValidateAndLifecycle(t *testing.T) {
	box, err := secret.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	mgr, err := NewManager(repo, filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{ID: "audit-plugin", Name: "Audit", Version: "1.0.0", APIVersion: APIVersion, Permissions: []string{"requests.read"}}
	data := pluginZip(t, manifest, noopWASM())
	p, err := mgr.Install(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusInstalled || p.Enabled {
		t.Fatalf("installed plugin = %+v", p)
	}
	p, err = mgr.Enable(context.Background(), p.ID)
	if err != nil || !p.Enabled || p.Status != StatusEnabled {
		t.Fatalf("enable: %+v %v", p, err)
	}
	p, err = mgr.Start(context.Background(), p.ID)
	if err != nil || p.Status != StatusEnabled {
		t.Fatalf("start: %+v %v", p, err)
	}
	if err = mgr.Delete(context.Background(), p.ID); err == nil {
		t.Fatal("enabled plugin should not delete")
	}
	if _, err = mgr.Disable(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Delete(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRejectsBadManifest(t *testing.T) {
	box, _ := secret.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	repo, _ := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	defer repo.Close()
	mgr, _ := NewManager(repo, filepath.Join(t.TempDir(), "plugins"))
	data := pluginZip(t, Manifest{ID: "bad", Name: "Bad", Version: "1", APIVersion: 99}, noopWASM())
	if _, err := mgr.Install(context.Background(), bytes.NewReader(data)); err == nil {
		t.Fatal("expected manifest version error")
	}
}

func noopWASM() []byte {
	// A minimal WASM 1.0 module exporting civic_plugin_start() -> void.
	return []byte{0, 'a', 's', 'm', 1, 0, 0, 0,
		1, 4, 1, 0x60, 0, 0,
		3, 2, 1, 0,
		7, 0x16, 1, 0x12, 'c', 'i', 'v', 'i', 'c', '_', 'p', 'l', 'u', 'g', 'i', 'n', '_', 's', 't', 'a', 'r', 't', 0, 0,
		10, 4, 1, 2, 0, 11}
}

func pluginZip(t *testing.T, manifest Manifest, wasm []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	manifestBytes, _ := json.Marshal(manifest)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write(manifestBytes)
	w, _ = zw.Create("module.wasm")
	_, _ = w.Write(wasm)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
