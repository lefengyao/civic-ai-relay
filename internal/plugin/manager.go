// Package plugin manages signed-by-digest Civic WASM extension packages.
//
// Phase one deliberately keeps the host contract small: a package is a ZIP
// containing manifest.json and module.wasm. Installation validates the package,
// records its declared permissions and limits, and makes enable/disable
// reversible through the admin API. Execution is exposed through the manager's
// Runtime interface so a WASM engine can be added without changing the storage
// or HTTP contract; a missing engine is reported as a runtime_unavailable state
// instead of silently running untrusted code in the relay process.
package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"civic-ai-relay/internal/store"
	"github.com/tetratelabs/wazero"
)

const (
	APIVersion           = 1
	MaxPackageBytes      = 64 << 20
	MaxManifestBytes     = 256 << 10
	MaxWASMBytes         = 48 << 20
	StatusInstalled      = "installed"
	StatusEnabled        = "enabled"
	StatusDisabled       = "disabled"
	StatusRuntimeUnavail = "runtime_unavailable"
	StatusFailed         = "failed"
)

var pluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)
var pluginVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,31}$`)

type Manifest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	APIVersion  int      `json:"api_version"`
	Entrypoint  string   `json:"entrypoint,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Limits      Limits   `json:"limits,omitempty"`
}

type Limits struct {
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	Fuel        uint64 `json:"fuel,omitempty"`
	TimeoutMS   int    `json:"timeout_ms,omitempty"`
}

type Package struct {
	Manifest     Manifest
	ManifestJSON string
	WASMHash     string
	WASMPath     string
	PackagePath  string
}

type Manager struct {
	repo *store.Store
	root string
}

func NewManager(repo *store.Store, root string) (*Manager, error) {
	if repo == nil {
		return nil, errors.New("plugin store is required")
	}
	root = filepath.Clean(root)
	if root == "." || root == "" {
		root = filepath.Clean("data/plugins")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create plugin directory: %w", err)
	}
	return &Manager{repo: repo, root: root}, nil
}

func (m *Manager) List(ctx context.Context) ([]store.Plugin, error) { return m.repo.ListPlugins(ctx) }

func (m *Manager) Install(ctx context.Context, src io.Reader) (store.Plugin, error) {
	data, err := io.ReadAll(io.LimitReader(src, MaxPackageBytes+1))
	if err != nil {
		return store.Plugin{}, fmt.Errorf("read plugin package: %w", err)
	}
	if len(data) > MaxPackageBytes {
		return store.Plugin{}, errors.New("plugin package exceeds 64 MiB")
	}
	pkg, err := unpack(data, m.root)
	if err != nil {
		return store.Plugin{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	permissions, _ := json.Marshal(pkg.Manifest.Permissions)
	limits, _ := json.Marshal(pkg.Manifest.Limits)
	plugin := store.Plugin{ID: pkg.Manifest.ID, Name: pkg.Manifest.Name, Version: pkg.Manifest.Version,
		Description: pkg.Manifest.Description, PackagePath: pkg.PackagePath, WASMHash: pkg.WASMHash,
		ManifestJSON: pkg.ManifestJSON, PermissionsJSON: string(permissions), LimitsJSON: string(limits),
		Status: StatusInstalled, InstalledAtUTC: now, UpdatedAtUTC: now}
	if err := m.repo.UpsertPlugin(ctx, plugin); err != nil {
		return store.Plugin{}, err
	}
	return m.repo.GetPlugin(ctx, plugin.ID)
}

func (m *Manager) Enable(ctx context.Context, id string) (store.Plugin, error) {
	p, err := m.repo.GetPlugin(ctx, id)
	if err != nil {
		return store.Plugin{}, err
	}
	if _, err := ValidateWASMFile(filepath.Join(p.PackagePath, "module.wasm")); err != nil {
		_ = m.repo.RecordPluginFailure(ctx, id, StatusFailed, err.Error())
		return store.Plugin{}, err
	}
	if err := m.repo.SetPluginEnabled(ctx, id, true, StatusEnabled, ""); err != nil {
		return store.Plugin{}, err
	}
	return m.repo.GetPlugin(ctx, id)
}

func (m *Manager) Disable(ctx context.Context, id string) (store.Plugin, error) {
	if err := m.repo.SetPluginEnabled(ctx, id, false, StatusDisabled, ""); err != nil {
		return store.Plugin{}, err
	}
	return m.repo.GetPlugin(ctx, id)
}

// Start compiles and invokes the package in a fresh wazero instance. The host
// provides no imports, so plugins cannot access files, sockets, environment
// variables, clocks, or the relay process. The exported entrypoint is called
// with no arguments and must return normally.
func (m *Manager) Start(ctx context.Context, id string) (store.Plugin, error) {
	p, err := m.repo.GetPlugin(ctx, id)
	if err != nil {
		return store.Plugin{}, err
	}
	if !p.Enabled {
		return store.Plugin{}, errors.New("plugin is disabled")
	}
	if _, err := ValidateWASMFile(filepath.Join(p.PackagePath, "module.wasm")); err != nil {
		_ = m.repo.RecordPluginFailure(ctx, id, StatusFailed, err.Error())
		return store.Plugin{}, err
	}
	manifest, err := parseManifest([]byte(p.ManifestJSON))
	if err != nil {
		_ = m.repo.RecordPluginFailure(ctx, id, StatusFailed, err.Error())
		return store.Plugin{}, err
	}
	if err := executeWASM(ctx, filepath.Join(p.PackagePath, "module.wasm"), manifest); err != nil {
		_ = m.repo.RecordPluginFailure(ctx, id, StatusFailed, err.Error())
		return store.Plugin{}, err
	}
	if err := m.repo.RecordPluginStart(ctx, id, StatusEnabled, ""); err != nil {
		return store.Plugin{}, err
	}
	return m.repo.GetPlugin(ctx, id)
}

func executeWASM(parent context.Context, path string, manifest Manifest) error {
	wasm, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	timeout := time.Duration(manifest.Limits.TimeoutMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	pages := uint32((manifest.Limits.MemoryBytes + 65535) / 65536)
	if pages == 0 {
		pages = 1
	}
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(pages))
	defer rt.Close(ctx)
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return fmt.Errorf("compile wasm: %w", err)
	}
	defer compiled.Close(ctx)
	module, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(manifest.ID))
	if err != nil {
		return fmt.Errorf("instantiate wasm: %w", err)
	}
	defer module.Close(ctx)
	entrypoint := manifest.Entrypoint
	if entrypoint == "" {
		entrypoint = "civic_plugin_start"
	}
	fn := module.ExportedFunction(entrypoint)
	if fn == nil {
		return fmt.Errorf("wasm entrypoint %q is not exported", entrypoint)
	}
	if _, err := fn.Call(ctx); err != nil {
		return fmt.Errorf("wasm entrypoint %q failed: %w", entrypoint, err)
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, id string) (store.Plugin, error) {
	if err := m.repo.RecordPluginStop(ctx, id, StatusEnabled, ""); err != nil {
		return store.Plugin{}, err
	}
	return m.repo.GetPlugin(ctx, id)
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	p, err := m.repo.GetPlugin(ctx, id)
	if err != nil {
		return err
	}
	if p.Enabled {
		return errors.New("disable plugin before deleting it")
	}
	if err := m.repo.DeletePlugin(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(p.PackagePath)
}

func ValidateWASMFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Size() < 8 || info.Size() > MaxWASMBytes {
		return "", errors.New("module.wasm has an invalid size")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	header := make([]byte, 8)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", errors.New("module.wasm header is truncated")
	}
	if string(header[:4]) != "\x00asm" || header[4] != 1 || header[5] != 0 || header[6] != 0 || header[7] != 0 {
		return "", errors.New("module.wasm is not a WebAssembly 1.0 module")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func unpack(data []byte, root string) (Package, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Package{}, errors.New("plugin package is not a valid ZIP")
	}
	var manifestBytes, wasmBytes []byte
	for _, file := range r.File {
		name := filepath.ToSlash(filepath.Clean(file.Name))
		if name == "." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") || filepath.IsAbs(name) {
			return Package{}, errors.New("plugin package contains an unsafe path")
		}
		switch name {
		case "manifest.json":
			if file.UncompressedSize64 > MaxManifestBytes {
				return Package{}, errors.New("manifest.json exceeds 256 KiB")
			}
			manifestBytes, err = readZipFile(file, MaxManifestBytes)
		case "module.wasm":
			if file.UncompressedSize64 > MaxWASMBytes {
				return Package{}, errors.New("module.wasm exceeds 48 MiB")
			}
			wasmBytes, err = readZipFile(file, MaxWASMBytes)
		}
		if err != nil {
			return Package{}, err
		}
	}
	if len(manifestBytes) == 0 || len(wasmBytes) < 8 {
		return Package{}, errors.New("plugin package requires manifest.json and module.wasm")
	}
	manifest, err := parseManifest(manifestBytes)
	if err != nil {
		return Package{}, err
	}
	wasmHash := sha256.Sum256(wasmBytes)
	if string(wasmBytes[:4]) != "\x00asm" || wasmBytes[4] != 1 || wasmBytes[5] != 0 || wasmBytes[6] != 0 || wasmBytes[7] != 0 {
		return Package{}, errors.New("module.wasm is not a WebAssembly 1.0 module")
	}
	packagePath := filepath.Join(root, manifest.ID, manifest.Version)
	if err := os.MkdirAll(packagePath, 0o700); err != nil {
		return Package{}, err
	}
	if err := os.WriteFile(filepath.Join(packagePath, "manifest.json"), manifestBytes, 0o600); err != nil {
		return Package{}, err
	}
	if err := os.WriteFile(filepath.Join(packagePath, "module.wasm"), wasmBytes, 0o600); err != nil {
		return Package{}, err
	}
	return Package{Manifest: manifest, ManifestJSON: string(manifestBytes), WASMHash: hex.EncodeToString(wasmHash[:]),
		WASMPath: filepath.Join(packagePath, "module.wasm"), PackagePath: packagePath}, nil
}

func parseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if len(data) == 0 || json.Unmarshal(data, &manifest) != nil {
		return Manifest{}, errors.New("manifest.json is invalid JSON")
	}
	manifest.ID = strings.TrimSpace(manifest.ID)
	manifest.Name = strings.TrimSpace(manifest.Name)
	manifest.Version = strings.TrimSpace(manifest.Version)
	if !pluginIDPattern.MatchString(manifest.ID) || manifest.Name == "" || !pluginVersionPattern.MatchString(manifest.Version) {
		return Manifest{}, errors.New("manifest requires a valid id, name and version")
	}
	if manifest.APIVersion != APIVersion {
		return Manifest{}, fmt.Errorf("unsupported plugin api_version %d", manifest.APIVersion)
	}
	if manifest.Permissions == nil {
		manifest.Permissions = []string{}
	}
	if manifest.Limits.MemoryBytes == 0 {
		manifest.Limits.MemoryBytes = 16 << 20
	}
	if manifest.Limits.MemoryBytes > 128<<20 {
		return Manifest{}, errors.New("memory_bytes exceeds 128 MiB")
	}
	if manifest.Limits.TimeoutMS == 0 {
		manifest.Limits.TimeoutMS = 1000
	}
	if manifest.Limits.TimeoutMS < 1 || manifest.Limits.TimeoutMS > 30000 {
		return Manifest{}, errors.New("timeout_ms must be between 1 and 30000")
	}
	seen := map[string]bool{}
	for i, permission := range manifest.Permissions {
		permission = strings.TrimSpace(permission)
		if permission == "" || len(permission) > 80 || seen[permission] {
			return Manifest{}, errors.New("manifest permissions must be unique and non-empty")
		}
		seen[permission] = true
		manifest.Permissions[i] = permission
	}
	sort.Strings(manifest.Permissions)
	return manifest, nil
}

func readZipFile(file *zip.File, limit uint64) ([]byte, error) {
	r, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if uint64(len(b)) > limit {
		return nil, errors.New("plugin file exceeds its size limit")
	}
	return b, nil
}
