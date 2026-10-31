# Civic Relay Go Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Python relay with a single, CGO-free Go binary while retaining the existing OpenAI-compatible relay, Chinese administrator console, multi-provider authorization, quotas, encryption, memory guard, and deployment behavior.

**Architecture:** `cmd/civic-relay` wires configuration, SQLite repositories, provider clients, relay services, memory guard, and HTTP handlers. The `internal` packages own one responsibility each: configuration and bootstrap secrets, RSS checking, encrypted persistent state, upstream protocol, business rules, then HTTP translation. The existing administrator page becomes an embedded static asset and continues to call the same `/admin/api/*` paths; the data store begins empty and does not read the prior Python database.

**Tech Stack:** Go 1.27, standard `net/http`, `context`, `crypto/aes`, `crypto/cipher`, `crypto/sha256`, `embed`, `modernc.org/sqlite`, `testing`, Docker multi-stage build, Docker Compose, systemd.

---

## Fixed Compatibility Contract

- Public routes: `GET /healthz`, `GET /v1/models`, `POST /v1/chat/completions`.
- Administrator routes retain the Python contract and `X-Admin-Key`: `/admin`, `/admin/api/overview`, `/admin/api/requests`, `/admin/api/config`, `/admin/api/config/validate`, `/admin/api/models/sync`, `/admin/api/providers`, `/admin/api/models`, `/admin/api/groups`, and `/admin/api/keys` with their existing item routes.
- Public client credentials use `Authorization: Bearer <client-key>`; plaintext client keys return only from key creation. Provider keys are accepted only on administrator writes and never returned, logged, or placed in request metadata.
- The Go configuration deliberately has no `PUBLIC_API_KEY`: every public credential is a randomly generated client-key record created by an administrator. This replaces the former single configured public key and avoids an open-source default credential path.
- Public model aliases route to an enabled, priced model for a provider. Keys can belong to more than one model group. A key can have global/per-key concurrency, all-time token, and all-time RMB-micro-yuan limits. Reaching either enabled quota disables that key with `quota_exhausted`.
- New request admission happens in this order: payload-size/memory gate, auth/model authorization, atomic quota reservation, global lease, key lease, upstream call. Every exit path releases leases and finalizes or cancels both global and per-key reservations.
- `MEMORY_LIMIT_MB` defaults to `200`. A new `/v1` request above its RSS limit returns an OpenAI error body with `503`, `code: memory_limit_exceeded`, `X-Request-ID`, and `Retry-After: 5`. A stream crossing the limit emits `event: error` with that same error then closes. Administrator routes remain available.
- Do not migrate `data/*.db`, old providers, client keys, or historical credentials. Back up an old database outside deployment only for audit; the Go binary must not open it as a migration source.

## Target File Structure

| Path | Responsibility |
| --- | --- |
| `go.mod`, `go.sum` | Go module and the pure-Go SQLite dependency. |
| `cmd/civic-relay/main.go` | Process construction, startup/shutdown, signal handling, configuration path discovery. |
| `internal/config/settings.go` | Typed settings, parse/validation/redaction, restart-only field calculation. |
| `internal/config/store.go` | External `relay.env` parsing, atomic writes, first-start bootstrap material. |
| `internal/secret/box.go` | AES-256-GCM encryption/decryption and key digest helpers. |
| `internal/memory/rss_windows.go`, `rss_linux.go`, `rss_other.go`, `guard.go` | Platform RSS implementation and the admission/stream guard. |
| `internal/store/db.go`, `schema.go`, `providers.go`, `models.go`, `keys.go`, `ledger.go` | SQLite connection lifecycle, schema, encrypted provider data, authorization objects, quota transactions, request metadata retention. |
| `internal/upstream/client.go`, `sse.go` | OpenAI-compatible upstream HTTP transport and SSE parsing. |
| `internal/relay/service.go`, `limits.go`, `errors.go` | Authorization, routing, quota reservations, global/key concurrency leases, usage settlement. |
| `internal/httpapi/server.go`, `public.go`, `admin.go`, `json.go` | HTTP routing, OpenAI response shape, SSE proxying, administrator API and embedded UI serving. |
| `web/admin.html`, `web/embed.go` | Extracted Chinese administrator UI and the package-local embedded asset. |
| `internal/*/*_test.go`, `cmd/civic-relay/main_test.go` | Unit, repository, service, HTTP contract, clean-install tests. |
| `Dockerfile`, `docker-compose.yml`, `civic-relay.service`, `ai-relay.service`, `README.md`, `.gitignore` | Go runtime/deployment and open-source guidance. |

### Task 1: Establish the Go Module and Test Harness

**Files:**
- Create: `go.mod`
- Create: `cmd/civic-relay/main.go`
- Create: `cmd/civic-relay/main_test.go`
- Create: `internal/testutil/http.go`
- Modify: `.gitignore`
- Test: `cmd/civic-relay/main_test.go`

- [ ] **Step 1: Write the failing version/startup test.**

```go
package main

import "testing"

func TestBuildServerUsesProvidedAddress(t *testing.T) {
    server := newServer("127.0.0.1:8000", nil)
    if server.Addr != "127.0.0.1:8000" {
        t.Fatalf("Addr = %q", server.Addr)
    }
}
```

- [ ] **Step 2: Run the test to verify it fails because the module and constructor do not exist.**

Run: `go test ./cmd/civic-relay`

Expected: FAIL with a missing `go.mod` or `newServer` symbol.

- [ ] **Step 3: Create the minimal module and constructor.**

```go
// go.mod
module civic-ai-relay

go 1.27

require modernc.org/sqlite v1.40.1
```

```go
// cmd/civic-relay/main.go
package main

import "net/http"

func newServer(addr string, handler http.Handler) *http.Server {
    return &http.Server{Addr: addr, Handler: handler}
}

func main() {}
```

Add `go.work`, `*.exe`, `coverage.out`, and `data/` to `.gitignore` only if generated locally; retain existing rules for `.codex/`, `docs/`, `.env`, `*.ps1`, SQLite files, certificates, and private prompt files.

- [ ] **Step 4: Add a reusable in-process HTTP helper.**

```go
package testutil

import (
    "net/http"
    "net/http/httptest"
)

func Server(h http.Handler) *httptest.Server { return httptest.NewServer(h) }
```

- [ ] **Step 5: Run formatting and the module test.**

Run: `gofmt -w cmd internal && go mod tidy && go test ./cmd/civic-relay`

Expected: PASS.

- [ ] **Step 6: Commit the baseline.**

```powershell
git add go.mod go.sum cmd internal .gitignore
git commit -m "build: establish Go relay module"
```

### Task 2: Implement Typed External Configuration and First-Start Bootstrap

**Files:**
- Create: `internal/config/settings.go`
- Create: `internal/config/store.go`
- Create: `internal/config/settings_test.go`
- Create: `internal/config/store_test.go`
- Modify: `cmd/civic-relay/main.go`
- Test: `internal/config/settings_test.go`, `internal/config/store_test.go`

- [ ] **Step 1: Write failing parse, redaction, and bootstrap tests.**

```go
func TestLoadRejectsInvalidMemoryLimit(t *testing.T) {
    _, err := Parse(map[string]string{"MEMORY_LIMIT_MB": "0"})
    if err == nil { t.Fatal("expected validation error") }
}

func TestEnsureCreatesStableSecretsOutsideRepository(t *testing.T) {
    path := filepath.Join(t.TempDir(), "relay.env")
    first, bootstrap, created, err := Ensure(path)
    if err != nil || !created || first.AdminAPIKey == "" || first.EncryptionKey == "" || bootstrap == "" { t.Fatal("bootstrap failed") }
    second, _, createdAgain, err := Ensure(path)
    if err != nil || createdAgain || second.AdminAPIKey != first.AdminAPIKey { t.Fatal("secrets changed") }
}

func TestRedactedNeverReturnsCredential(t *testing.T) {
    settings := Settings{AdminAPIKey: "admin", EncryptionKey: "encryption", UpstreamAPIKey: "upstream"}
    got := settings.Redacted()
    if fmt.Sprint(got) == "" || strings.Contains(fmt.Sprint(got), "admin") || strings.Contains(fmt.Sprint(got), "upstream") { t.Fatal("secret leaked") }
}
```

- [ ] **Step 2: Run the targeted tests.**

Run: `go test ./internal/config -run 'Test(Load|Ensure|Redacted)' -v`

Expected: FAIL because `Settings`, `Parse`, and `Ensure` are undefined.

- [ ] **Step 3: Define configuration behavior and parsing.**

```go
type Settings struct {
    Host string; Port int; DBPath string; DocsEnabled bool
    AdminAPIKey string; EncryptionKey string
    UpstreamBaseURL string; UpstreamAPIKey string
    ModelAutoSync bool; ModelSyncInterval time.Duration
    MemoryLimitMB int; MaxBodyBytes int64; MaxOutputTokens int; MaxStreamDuration time.Duration
    GlobalConcurrencyLimit int; RPMLimit int; TokenLimit5H int64; TokenLimitDaily int64
    ConnectTimeout time.Duration; ReadTimeout time.Duration; WriteTimeout time.Duration; PoolTimeout time.Duration
    RetentionDays int; LogLevel string
}

func Parse(values map[string]string) (Settings, error) {
    // Read defaults, require positive limits, parse booleans/durations, validate base URLs,
    // and reject a supplied encryption key that is not base64-decoded 32 bytes.
}

func (s Settings) Redacted() map[string]any {
    values := s.EnvMap()
    for _, name := range []string{"ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY", "UPSTREAM_API_KEY"} {
        values[name] = map[string]bool{"is_configured": s.EnvMap()[name] != ""}
    }
    return values
}

func (s Settings) RestartOnlyChanges(next Settings) []string {
    // Compare only HOST, PORT, DB_PATH, DOCS_ENABLED in that order.
}
```

Use `CIVIC_RELAY_CONFIG_FILE` when set. Otherwise use `%ProgramData%\\CivicRelay\\relay.env` on Windows, `/etc/civic-relay/relay.env` on Linux, and return an explicit error on other systems unless the environment variable is set. Parse `KEY=value`, quoted values, comments, and duplicate keys deterministically; reject malformed lines. Serialize sorted keys and write `relay.env.tmp` with `0600` on Unix, then rename it over the target. Generate random URL-safe administrator credentials and a base64 32-byte encryption key with `crypto/rand`; create `bootstrap-admin-key.txt` adjacent to `relay.env` on the first successful write only.

- [ ] **Step 4: Wire startup to external configuration without reading repository `.env`.**

```go
configPath, err := config.DefaultPath(os.Getenv("CIVIC_RELAY_CONFIG_FILE"))
if err != nil { log.Fatal(err) }
settings, bootstrapKey, created, err := config.Ensure(configPath)
if err != nil { log.Fatal(err) }
if created { log.Printf("first start created bootstrap administrator key file: %s", filepath.Join(filepath.Dir(configPath), "bootstrap-admin-key.txt")) }
_ = bootstrapKey // Never log its value.
```

- [ ] **Step 5: Run package tests and race detection.**

Run: `gofmt -w internal/config cmd/civic-relay && go test -race ./internal/config`

Expected: PASS.

- [ ] **Step 6: Commit configuration handling.**

```powershell
git add internal/config cmd/civic-relay/main.go
git commit -m "feat: add external Go configuration bootstrap"
```

### Task 3: Add AES-GCM Secret Protection and SQLite Foundation

**Files:**
- Create: `internal/secret/box.go`
- Create: `internal/secret/box_test.go`
- Create: `internal/store/db.go`
- Create: `internal/store/schema.go`
- Create: `internal/store/db_test.go`
- Test: `internal/secret/box_test.go`, `internal/store/db_test.go`

- [ ] **Step 1: Write failing encryption and schema tests.**

```go
func TestBoxUsesRandomNonceAndRoundTrips(t *testing.T) {
    box, _ := secret.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
    a, _ := box.Seal("provider-secret")
    b, _ := box.Seal("provider-secret")
    if a == b { t.Fatal("nonce was reused") }
    got, _ := box.Open(a)
    if got != "provider-secret" || strings.Contains(a, "provider-secret") { t.Fatal("encryption failed") }
}

func TestOpenInitializesWALAndSchema(t *testing.T) {
    db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), testBox(t))
    if err != nil { t.Fatal(err) }
    defer db.Close()
    if got := db.JournalMode(t.Context()); got != "wal" { t.Fatalf("journal_mode = %q", got) }
}
```

- [ ] **Step 2: Run the tests to verify they fail.**

Run: `go test ./internal/secret ./internal/store -run 'Test(Box|Open)' -v`

Expected: FAIL because the packages do not exist.

- [ ] **Step 3: Implement encryption and the schema migrator.**

```go
type Box struct{ aead cipher.AEAD }

func New(encoded string) (*Box, error) {
    key, err := base64.StdEncoding.DecodeString(encoded)
    if err != nil || len(key) != 32 { return nil, errors.New("RELAY_ENCRYPTION_KEY must decode to 32 bytes") }
    block, err := aes.NewCipher(key); if err != nil { return nil, err }
    aead, err := cipher.NewGCM(block); if err != nil { return nil, err }
    return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain string) (string, error) {
    nonce := make([]byte, b.aead.NonceSize())
    if _, err := rand.Read(nonce); err != nil { return "", err }
    return base64.StdEncoding.EncodeToString(append(nonce, b.aead.Seal(nil, nonce, []byte(plain), nil)...)), nil
}
```

In `store.Open`, open `file:<escaped-path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)`, configure a bounded connection pool, and apply each numbered schema statement in one transaction. Create tables for `providers`, `models`, `model_groups`, `group_models`, `client_keys`, `key_groups`, `key_reservations`, and `requests`; every schema stores only metadata, digests, encrypted provider key blobs, micro-yuan integers, timestamps, and status values. Use foreign keys and `CREATE INDEX` statements for active key lookup, authorized model joins, and time-window ledger lookups. Store `client_keys.token_digest` as an HMAC-SHA-256 digest using the encryption key bytes, never reversible plaintext.

- [ ] **Step 4: Add schema constraints that enforce the agreed invariants.**

```sql
CREATE TABLE client_keys (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_digest BLOB NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  concurrency_limit INTEGER NOT NULL CHECK (concurrency_limit > 0),
  token_limit INTEGER,
  amount_limit_microyuan INTEGER,
  disabled_reason TEXT,
  created_at_utc TEXT NOT NULL
);
CREATE TABLE key_reservations (
  id INTEGER PRIMARY KEY,
  key_id INTEGER NOT NULL REFERENCES client_keys(id),
  model_id INTEGER NOT NULL REFERENCES models(id),
  reserved_tokens INTEGER NOT NULL CHECK (reserved_tokens >= 0),
  reserved_amount_microyuan INTEGER NOT NULL CHECK (reserved_amount_microyuan >= 0),
  charged_tokens INTEGER NOT NULL DEFAULT 0,
  charged_amount_microyuan INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL CHECK (status IN ('reserved','completed','failed','aborted','rejected')),
  created_at_utc TEXT NOT NULL, finished_at_utc TEXT
);
```

- [ ] **Step 5: Verify encryption, fresh schema, and SQL constraints.**

Run: `gofmt -w internal/secret internal/store && go test -race ./internal/secret ./internal/store`

Expected: PASS.

- [ ] **Step 6: Commit the secure persistence base.**

```powershell
git add internal/secret internal/store go.mod go.sum
git commit -m "feat: add encrypted SQLite persistence"
```

### Task 4: Build Provider, Model, Group, and Client-Key Repositories

**Files:**
- Create: `internal/store/providers.go`
- Create: `internal/store/models.go`
- Create: `internal/store/keys.go`
- Create: `internal/store/catalog_test.go`
- Create: `internal/store/keys_test.go`
- Test: `internal/store/catalog_test.go`, `internal/store/keys_test.go`

- [ ] **Step 1: Write failing repository tests for redaction and group-based authorization.**

```go
func TestProviderKeyIsEncryptedAndNeverListed(t *testing.T) {
    repo := newTestStore(t)
    created, err := repo.CreateProvider(t.Context(), store.NewProvider{Name: "A", BaseURL: "https://a.example", APIKey: "secret-a"})
    if err != nil { t.Fatal(err) }
    listed, _ := repo.ListProviders(t.Context())
    if strings.Contains(fmt.Sprint(listed), "secret-a") || created.APIKeyConfigured != true { t.Fatal("provider secret leaked") }
}

func TestKeyModelsAreUnionOfAssignedGroups(t *testing.T) {
    repo := newTestStore(t)
    key, basic, advanced := createKeyWithTwoGroups(t, repo)
    models, err := repo.AuthorizedModels(t.Context(), key.Token)
    if err != nil || len(models) != 2 { t.Fatalf("models = %#v, err = %v", models, err) }
    if basic.PublicName == advanced.PublicName { t.Fatal("fixture invalid") }
}

func TestNewKeyIsReturnedOnceAndStoredAsDigest(t *testing.T) {
    repo := newTestStore(t)
    key, _ := repo.CreateClientKey(t.Context(), store.NewClientKey{Name: "alice", ConcurrencyLimit: 1})
    rows, _ := repo.ListClientKeys(t.Context())
    if !strings.HasPrefix(key.Token, "crk_") || strings.Contains(fmt.Sprint(rows), key.Token) { t.Fatal("key handling failed") }
}
```

- [ ] **Step 2: Run the catalog tests.**

Run: `go test ./internal/store -run 'Test(Provider|Key)' -v`

Expected: FAIL because catalog repository methods are undefined.

- [ ] **Step 3: Implement typed data contracts and CRUD operations.**

```go
type Provider struct { ID int64; Name, BaseURL string; Enabled bool; APIKeyConfigured bool }
type Model struct { ID, ProviderID int64; PublicName, UpstreamName string; InputPriceMicroyuan, OutputPriceMicroyuan *int64; Enabled bool }
type ModelGroup struct { ID int64; Name string }
type ClientKey struct { ID int64; Name string; Token string; Enabled bool; ConcurrencyLimit int; TokenLimit, AmountLimitMicroyuan *int64; DisabledReason string }

func (s *Store) CreateClientKey(ctx context.Context, in NewClientKey) (ClientKey, error) {
    token, err := randomToken("crk_")
    if err != nil { return ClientKey{}, err }
    _, err = s.db.ExecContext(ctx, `INSERT INTO client_keys (name,token_digest,enabled,concurrency_limit,token_limit,amount_limit_microyuan,created_at_utc) VALUES (?,?,?,?,?,?,?)`, in.Name, s.Digest(token), 1, in.ConcurrencyLimit, in.TokenLimit, in.AmountLimitMicroyuan, nowUTC())
    if err != nil { return ClientKey{}, err }
    return ClientKey{Token: token}, nil
}
```

`CreateProvider` validates an HTTPS or explicitly allowed HTTP base URL and seals the supplied API key. `UpdateProvider` accepts an empty API key as no rotation, preserves its encrypted value, and evicts the caller's cached upstream client after successful rotation. `CreateModel` and `UpdateModel` store unit prices as integer micro-yuan per million tokens; a group update rejects disabled or unpriced models. Group/key assignment replaces only the join rows passed by the administrator. Authorization performs a single query joining enabled key, key groups, group models, enabled priced models, and enabled providers.

- [ ] **Step 4: Verify the all-time key quota disables the key at settlement.**

```go
func TestSettlementDisablesKeyAtTokenOrAmountLimit(t *testing.T) {
    repo := newTestStore(t)
    key, model := createAuthorizedKeyAndModel(t, repo, 10, 1_000)
    reservation, _ := repo.ReserveForKey(t.Context(), key.ID, model.ID, 10, 1_000)
    if err := repo.SettleKey(t.Context(), reservation.ID, 10, 1_000, "completed"); err != nil { t.Fatal(err) }
    current, _ := repo.GetClientKey(t.Context(), key.ID)
    if current.Enabled || current.DisabledReason != "quota_exhausted" { t.Fatal("key remains active") }
}
```

- [ ] **Step 5: Run all store tests.**

Run: `go test -race ./internal/store`

Expected: PASS.

- [ ] **Step 6: Commit the catalog repository.**

```powershell
git add internal/store
git commit -m "feat: add provider catalog and client key repositories"
```

### Task 5: Implement Atomic Global Ledger and Per-Key Reservations

**Files:**
- Create: `internal/store/ledger.go`
- Create: `internal/store/ledger_test.go`
- Modify: `internal/store/schema.go`
- Test: `internal/store/ledger_test.go`

- [ ] **Step 1: Write failing transactional quota tests.**

```go
func TestCompetingReservationsAllowExactlyOneFinalQuotaClaim(t *testing.T) {
    repo := newTestStore(t)
    args := newReservationArgs(600)
    results := make(chan error, 2)
    go func() { _, err := repo.ReserveRequest(context.Background(), args); results <- err }()
    go func() { _, err := repo.ReserveRequest(context.Background(), args); results <- err }()
    a, b := <-results, <-results
    if (a == nil) == (b == nil) { t.Fatalf("results = %v, %v", a, b) }
}

func TestCancelledReservationReleasesGlobalAndKeyQuota(t *testing.T) {
    repo := newTestStore(t)
    reservation, _ := repo.ReserveRequest(t.Context(), newReservationArgs(100))
    if err := repo.CancelRequest(t.Context(), reservation.ID, "rejected", 429); err != nil { t.Fatal(err) }
    if got := repo.OccupiedTokens(t.Context(), reservation.StartedAt); got != 0 { t.Fatalf("occupied = %d", got) }
}
```

- [ ] **Step 2: Run the ledger test.**

Run: `go test ./internal/store -run 'Test(Competing|Cancelled)' -v`

Expected: FAIL because `ReserveRequest` and `CancelRequest` do not exist.

- [ ] **Step 3: Implement one `BEGIN IMMEDIATE` reservation transaction.**

```go
type RequestReservation struct { ID, KeyReservationID int64; RequestID string; StartedAt time.Time; ReservedTokens, ReservedAmountMicroyuan int64 }
type QuotaError struct { Code string }
func (e *QuotaError) Error() string { return e.Code }

func (s *Store) ReserveRequest(ctx context.Context, in ReserveInput) (RequestReservation, error) {
    // BEGIN IMMEDIATE; count RPM in [now-60s, now], global five-hour and Beijing-day
    // charges plus holds, key all-time charges plus holds; reject before inserts;
    // insert request and key_reservations; COMMIT. Roll back on every error.
}
```

Calculate a conservative input-token estimate using every string-bearing message field, reserve the explicit output ceiling or `MAX_OUTPUT_TOKENS`, and compute reservation cost with `ceil(tokens * microYuanPerMillion / 1_000_000)` for input and output separately. Use `Asia/Shanghai` calendar date for the daily global ledger. The existing global RPM, five-hour token, and daily token limits stay configuration-wide; key token/amount limits stay all-time per key. `SettleRequest` writes actual usage, clears both holds, keeps actual charges, and auto-disables a key when either configured all-time limit is met. `CancelRequest` clears all holds and records a redacted outcome row.

- [ ] **Step 4: Add monitoring and retention tests.**

```go
func TestRecentRequestMetadataContainsNoPromptOrCredential(t *testing.T) {
    repo := newTestStore(t)
    _, _ = repo.ReserveRequest(t.Context(), newReservationArgs(10))
    rows, _ := repo.RecentRequests(t.Context(), 50)
    got := fmt.Sprint(rows)
    if strings.Contains(got, "prompt text") || strings.Contains(got, "crk_") { t.Fatal("sensitive data retained") }
}
```

Implement `Overview`, `RecentRequests(limit <= 50)`, `Healthcheck`, and `Prune(cutoff)` over metadata-only request columns. Preserve the twelve five-minute trend buckets and completed/failed/aborted/rejected/reserved status counts currently used by the UI.

- [ ] **Step 5: Run race-safe repository tests.**

Run: `go test -race ./internal/store`

Expected: PASS.

- [ ] **Step 6: Commit quota accounting.**

```powershell
git add internal/store
git commit -m "feat: add atomic relay quota accounting"
```

### Task 6: Add Platform RSS Readers and Adjustable Concurrency Leases

**Files:**
- Create: `internal/memory/guard.go`
- Create: `internal/memory/rss_windows.go`
- Create: `internal/memory/rss_linux.go`
- Create: `internal/memory/rss_other.go`
- Create: `internal/memory/guard_test.go`
- Create: `internal/relay/limits.go`
- Create: `internal/relay/limits_test.go`
- Test: `internal/memory/guard_test.go`, `internal/relay/limits_test.go`

- [ ] **Step 1: Write failing memory and concurrency tests.**

```go
func TestGuardRejectsOnlyPublicWorkAboveLimit(t *testing.T) {
    guard := memory.NewGuard(200, func() (uint64, error) { return 201 << 20, nil })
    if err := guard.PublicAdmission(); !errors.Is(err, memory.ErrLimitExceeded) { t.Fatal(err) }
}

func TestLoweringLimitKeepsExistingLeaseButRejectsNewLease(t *testing.T) {
    gate := relay.NewGate(2)
    if !gate.TryAcquire() || !gate.TryAcquire() { t.Fatal("initial leases") }
    gate.SetLimit(1)
    if gate.TryAcquire() { t.Fatal("new request admitted") }
    gate.Release(); gate.Release()
    if !gate.TryAcquire() { t.Fatal("slot was not restored") }
}
```

- [ ] **Step 2: Run tests to verify they fail.**

Run: `go test ./internal/memory ./internal/relay -run 'Test(Guard|Lowering)' -v`

Expected: FAIL because `NewGuard` and `NewGate` are undefined.

- [ ] **Step 3: Implement OS-specific RSS and nonblocking gates.**

```go
type Reader func() (uint64, error)
type Guard struct { limitBytes uint64; read Reader }
var ErrLimitExceeded = errors.New("memory limit exceeded")
func (g Guard) PublicAdmission() error { rss, err := g.read(); if err != nil { return nil }; if rss > g.limitBytes { return ErrLimitExceeded }; return nil }
func (g Guard) StreamContinue() error { return g.PublicAdmission() }
```

On Windows, use `windows.GetProcessMemoryInfo` through `golang.org/x/sys/windows` only if it remains a direct, small dependency; on Linux parse `/proc/self/statm` and multiply resident pages by `os.Getpagesize`; `rss_other.go` returns `(0, ErrUnsupported)` and the guard treats an unsupported reader as no soft limit. Use build tags so every target has exactly one `RSS()` function. `Gate` uses a mutex-protected active counter and adjustable positive limit. `KeyGates` maps key ID to a gate and removes an idle entry after release to avoid unbounded growth.

- [ ] **Step 4: Verify stream memory behavior at the service seam.**

```go
func TestStreamGuardStopsAfterLimitCrossing(t *testing.T) {
    readings := []uint64{100 << 20, 201 << 20}
    guard := memory.NewGuard(200, func() (uint64, error) { value := readings[0]; readings = readings[1:]; return value, nil })
    if guard.PublicAdmission() != nil { t.Fatal("first sample") }
    if !errors.Is(guard.StreamContinue(), memory.ErrLimitExceeded) { t.Fatal("second sample") }
}
```

- [ ] **Step 5: Run all tests.**

Run: `gofmt -w internal/memory internal/relay && go test -race ./internal/memory ./internal/relay`

Expected: PASS.

- [ ] **Step 6: Commit memory and concurrency foundations.**

```powershell
git add internal/memory internal/relay
git commit -m "feat: add memory guard and request gates"
```

### Task 7: Implement OpenAI-Compatible Upstream HTTP and SSE Parsing

**Files:**
- Create: `internal/upstream/client.go`
- Create: `internal/upstream/sse.go`
- Create: `internal/upstream/client_test.go`
- Create: `internal/upstream/sse_test.go`
- Test: `internal/upstream/client_test.go`, `internal/upstream/sse_test.go`

- [ ] **Step 1: Write failing upstream contract tests.**

```go
func TestStreamAddsUsageOptionAndForwardsBearerKey(t *testing.T) {
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Header.Get("Authorization") != "Bearer upstream-secret" { t.Fatal("missing upstream authorization") }
        var body map[string]any; _ = json.NewDecoder(r.Body).Decode(&body)
        if body["stream_options"].(map[string]any)["include_usage"] != true { t.Fatal("usage option missing") }
        w.Header().Set("Content-Type", "text/event-stream"); _, _ = io.WriteString(w, "data: [DONE]\\n\\n")
    }))
    defer upstream.Close()
    _, err := New(upstream.URL, "upstream-secret", time.Second, time.Second, time.Second, time.Second).Stream(t.Context(), map[string]any{"stream": true})
    if err != nil { t.Fatal(err) }
}

func TestParseEventCountsDeltaAndReadsUsage(t *testing.T) {
    event := ParseEvent([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"total_tokens\":3}}\\n\\n"))
    if event.OutputCharacters != 5 || event.Usage.TotalTokens != 3 { t.Fatalf("%+v", event) }
}
```

- [ ] **Step 2: Run the upstream tests.**

Run: `go test ./internal/upstream -v`

Expected: FAIL because the client and parser do not exist.

- [ ] **Step 3: Implement bounded upstream transport.**

```go
type Client struct { baseURL, apiKey string; http *http.Client }
func New(baseURL, apiKey string, connect, read, write, pool time.Duration) *Client {
    transport := &http.Transport{MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: pool, ResponseHeaderTimeout: read}
    return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: &http.Client{Transport: transport, Timeout: connect + read + write}}
}
func (c *Client) request(ctx context.Context, method, path string, payload any) (*http.Response, error) {
    // Marshal payload, set Content-Type and Bearer header, perform context-bound request,
    // convert network failures and non-2xx status to redacted typed errors.
}
```

Support `ListModels`, `ChatJSON`, and `Stream`. Preserve upstream JSON body unchanged for successful non-stream chats except the public alias-to-upstream model replacement performed by `relay.Service`; map upstream 401/403, 429, 4xx, 5xx, timeout, connection, and malformed JSON to typed public-safe error codes. Never include an upstream response body or credential in an error. The stream method must add `stream_options.include_usage=true`, then expose a read-closer whose closure happens for normal completion, client cancellation, timeout, and memory guard interruption.

- [ ] **Step 4: Add malformed SSE and HTTP failure tests.**

```go
func TestParseEventHandlesDoneAndMalformedData(t *testing.T) {
    if !ParseEvent([]byte("data: [DONE]\\n\\n")).Done { t.Fatal("done not recognized") }
    if got := ParseEvent([]byte("event: ping\\n\\n")); got.OutputCharacters != 0 || got.Usage.TotalTokens != 0 { t.Fatalf("%+v", got) }
}

func TestUpstreamAuthenticationFailureIsRedacted(t *testing.T) {
    // Test server returns 401 with a body containing a fake secret; assert typed error is
    // upstream_authentication_failed and Error() omits the fake secret.
}
```

- [ ] **Step 5: Run upstream tests and static checks.**

Run: `gofmt -w internal/upstream && go vet ./internal/upstream && go test -race ./internal/upstream`

Expected: PASS.

- [ ] **Step 6: Commit upstream protocol support.**

```powershell
git add internal/upstream
git commit -m "feat: add OpenAI-compatible upstream client"
```

### Task 8: Implement Relay Service Request Lifecycle

**Files:**
- Create: `internal/relay/errors.go`
- Create: `internal/relay/service.go`
- Create: `internal/relay/service_test.go`
- Modify: `internal/store/keys.go`
- Test: `internal/relay/service_test.go`

- [ ] **Step 1: Write failing service tests for authorization, routing, and cleanup.**

```go
func TestServiceRejectsUnauthorizedModelBeforeUpstream(t *testing.T) {
    service, calls := newServiceFixture(t, false)
    _, err := service.Begin(t.Context(), relay.Request{Token: "valid-key", Model: "forbidden", Messages: []any{}})
    if !errors.Is(err, relay.ErrModelNotAllowed) || *calls != 0 { t.Fatalf("err=%v calls=%d", err, *calls) }
}

func TestServiceReleasesBothGatesWhenUpstreamFails(t *testing.T) {
    service, _ := newFailingServiceFixture(t)
    _, err := service.Chat(t.Context(), relay.Request{Token: "valid-key", Model: "public-model"})
    if err == nil || service.ActiveGlobal() != 0 || service.ActiveForKey("valid-key") != 0 { t.Fatal("leases leaked") }
}
```

- [ ] **Step 2: Run service tests to verify failure.**

Run: `go test ./internal/relay -run 'TestService' -v`

Expected: FAIL because `Service` does not exist.

- [ ] **Step 3: Implement a request-scoped lease with one cleanup method.**

```go
type Service struct { store Store; clients ClientFactory; global *Gate; keys *KeyGates; settings func() config.Settings }
type Lease struct { service *Service; reservation store.RequestReservation; keyID int64; released bool }
func (l *Lease) Close(ctx context.Context, outcome Outcome) error {
    if l.released { return nil }; l.released = true
    defer l.service.keys.Release(l.keyID); defer l.service.global.Release()
    return l.service.store.FinalizeRequest(ctx, l.reservation, outcome)
}
func (s *Service) Begin(ctx context.Context, req Request) (*Lease, ResolvedModel, error) {
    // Authenticate digest, choose an authorized public alias, check memory externally,
    // atomically reserve quota, acquire global then key gate; cancel reservation if either gate rejects.
}
```

Keep all opaque upstream details behind `ClientFactory.ForProvider(ctx, providerID)`. Use `http.MaxBytesReader` later in the handler, but have `Service` validate that messages is an array, model is nonempty, `max_tokens` and `max_completion_tokens` do not conflict, and the effective output ceiling is positive and no more than the configured maximum. `Chat` settles exact upstream `usage` when present. For streams, create a lease before opening upstream, track delta characters with the existing conservative `ceil(chars/2)` estimate, prefer final usage when sent, and defer `Close` on every stream result.

- [ ] **Step 4: Add per-key/global concurrency and cost tests.**

```go
func TestSecondRequestForKeyIsRejectedWhileAnotherKeyCanProceed(t *testing.T) {
    service := newConcurrentService(t)
    first := service.HoldForTest(t.Context(), "key-a")
    defer first.Close(t.Context(), relay.Outcome{Status: "aborted"})
    if _, err := service.Begin(t.Context(), requestFor("key-a")); !errors.Is(err, relay.ErrKeyConcurrencyExceeded) { t.Fatal(err) }
    if _, _, err := service.Begin(t.Context(), requestFor("key-b")); err != nil { t.Fatal(err) }
}
```

- [ ] **Step 5: Run full service tests.**

Run: `gofmt -w internal/relay internal/store && go test -race ./internal/relay ./internal/store`

Expected: PASS.

- [ ] **Step 6: Commit relay orchestration.**

```powershell
git add internal/relay internal/store
git commit -m "feat: add authenticated relay service lifecycle"
```

### Task 9: Add Public HTTP Routes, OpenAI Errors, and Streaming Proxy

**Files:**
- Create: `internal/httpapi/json.go`
- Create: `internal/httpapi/public.go`
- Create: `internal/httpapi/server.go`
- Create: `internal/httpapi/public_test.go`
- Test: `internal/httpapi/public_test.go`

- [ ] **Step 1: Write failing public API contract tests.**

```go
func TestModelsRequiresBearerAndFiltersAuthorizedModels(t *testing.T) {
    server := newPublicServer(t)
    unauthenticated := server.Get("/v1/models", nil)
    if unauthenticated.Code != http.StatusUnauthorized { t.Fatal(unauthenticated.Code) }
    response := server.Get("/v1/models", map[string]string{"Authorization": "Bearer client-key"})
    var body struct { Data []struct { ID string `json:"id"` } `json:"data"` }
    _ = json.Unmarshal(response.Body.Bytes(), &body)
    if response.Code != http.StatusOK || len(body.Data) != 1 || body.Data[0].ID != "public-chat" { t.Fatal(response.Body.String()) }
}

func TestMemoryLimitReturnsOpenAI503AndRetryAfter(t *testing.T) {
    server := newMemoryLimitedPublicServer(t)
    response := server.Get("/v1/models", map[string]string{"Authorization": "Bearer client-key"})
    var body struct { Error struct { Code string `json:"code"` } `json:"error"` }
    _ = json.Unmarshal(response.Body.Bytes(), &body)
    if response.Code != 503 || response.Header().Get("Retry-After") != "5" || body.Error.Code != "memory_limit_exceeded" { t.Fatal(response.Body.String()) }
}
```


- [ ] **Step 2: Run public HTTP tests.**

Run: `go test ./internal/httpapi -run 'Test(Models|Memory)' -v`

Expected: FAIL because the server package does not exist.

- [ ] **Step 3: Implement public route registration and error shape.**

```go
type openAIError struct { Message, Type, Code string; Param any `json:"param"` }
func writeOpenAIError(w http.ResponseWriter, status int, requestID, message, typ, code string) {
    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("X-Request-ID", requestID)
    json.NewEncoder(w).Encode(map[string]any{"error": openAIError{Message: message, Type: typ, Code: code, Param: nil}})
}
func (s *Server) routes() http.Handler {
    mux := http.NewServeMux()
    mux.HandleFunc("GET /healthz", s.healthz)
    mux.HandleFunc("GET /v1/models", s.models)
    mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
    return s.requestID(mux)
}
```

Generate an opaque request ID with `crypto/rand` and return it for every public result. Apply memory admission before reading `/v1` request bodies. Use `http.MaxBytesReader` with `MAX_BODY_MB`; malformed/oversize payloads, absent Bearer token, invalid model, max token conflicts, quota errors, concurrency errors, and upstream errors must have explicit public OpenAI-style status/code mappings and no detail from secrets. Return only enabled models authorized for the client key in OpenAI model list shape.

- [ ] **Step 4: Implement correct SSE output and lifecycle tests.**

```go
func TestStreamingPassesDataAndEmitsMemoryErrorBeforeClose(t *testing.T) {
    server := newStreamThatCrossesMemoryLimit(t)
    response := server.PostJSON("/v1/chat/completions", bearer("client-key"), chatPayload(true))
    if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "event: error") || !strings.Contains(response.Body.String(), "memory_limit_exceeded") { t.Fatal(response.Body.String()) }
    if server.ActiveGlobal() != 0 { t.Fatal("stream lease leaked") }
}
```

Set `Content-Type: text/event-stream`, `Cache-Control: no-cache`, and `X-Accel-Buffering: no`; flush each upstream chunk through `http.Flusher`. Before forwarding each chunk call `memoryGuard.StreamContinue`; on a limit error write `event: error\ndata: {"error":{"code":"memory_limit_exceeded",...}}\n\n`, flush, close the upstream reader, settle/cancel exactly once, and return. Propagate downstream cancellation through `r.Context()`.

- [ ] **Step 5: Run HTTP tests and race checks.**

Run: `gofmt -w internal/httpapi && go test -race ./internal/httpapi ./internal/relay`

Expected: PASS.

- [ ] **Step 6: Commit the public API.**

```powershell
git add internal/httpapi
git commit -m "feat: serve OpenAI-compatible Go relay API"
```

### Task 10: Port the Chinese Administrator UI and Administration API

**Files:**
- Create: `web/admin.html`
- Create: `web/embed.go`
- Create: `internal/httpapi/admin.go`
- Create: `internal/httpapi/admin_test.go`
- Modify: `internal/httpapi/server.go`
- Test: `internal/httpapi/admin_test.go`

- [ ] **Step 1: Extract the existing browser experience before replacing Python.**

Copy the HTML, CSS, and JavaScript literal currently defined by `ADMIN_HTML` in `admin_ui.py` into `web/admin.html` without changing its endpoint names, Chinese labels, form fields, or API payload field names. Remove the Python string delimiter and no other browser behavior. Embed the page from its own package so the Go compiler never needs a parent-directory embed pattern.

```go
// web/embed.go
package web

import _ "embed"

//go:embed admin.html
var AdminHTML []byte
```

Import `civic-ai-relay/web` from `internal/httpapi/admin.go` and serve a copy of `web.AdminHTML`; the handler must not read a disk asset at runtime.

- [ ] **Step 2: Write failing administrator contract tests.**

```go
func TestAdminRequiresIndependentKeyAndRedactsConfig(t *testing.T) {
    server := newAdminServer(t)
    if response := server.Get("/admin/api/config", nil); response.Code != 401 { t.Fatal(response.Code) }
    response := server.Get("/admin/api/config", map[string]string{"X-Admin-Key": "admin-key"})
    if response.Code != 200 || strings.Contains(response.Body.String(), "upstream-secret") || strings.Contains(response.Body.String(), "admin-key") { t.Fatal(response.Body.String()) }
}

func TestAdminCanCreateProviderGroupAndOneTimeKey(t *testing.T) {
    server := newAdminServer(t)
    provider := server.PostJSON("/admin/api/providers", adminHeaders(), providerPayload())
    if provider.Code != 201 { t.Fatal(provider.Body.String()) }
    key := server.PostJSON("/admin/api/keys", adminHeaders(), keyPayload())
    if key.Code != 201 || !strings.Contains(key.Body.String(), "crk_") { t.Fatal(key.Body.String()) }
    listed := server.Get("/admin/api/keys", adminHeaders())
    if strings.Contains(listed.Body.String(), "crk_") { t.Fatal("key leaked from list") }
}
```

- [ ] **Step 3: Run the administrator tests.**

Run: `go test ./internal/httpapi -run 'TestAdmin' -v`

Expected: FAIL because administrator handlers do not exist.

- [ ] **Step 4: Implement constant-time admin auth and all existing endpoint shapes.**

```go
func (s *Server) adminAuthorized(r *http.Request) bool {
    supplied := []byte(r.Header.Get("X-Admin-Key"))
    expected := []byte(s.runtime.Active().AdminAPIKey)
    return len(supplied) == len(expected) && subtle.ConstantTimeCompare(supplied, expected) == 1
}
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
    if s.adminAuthorized(r) { return true }
    writeAdminError(w, http.StatusUnauthorized, "authentication required", requestID(r)); return false
}
```

Serve `GET /admin` as the embedded page with `Cache-Control: no-store`. Implement every existing route and method with its current payload names and response wrappers (`{"data": ...}` for catalog lists/mutations, `{"settings": ..., "pending_restart_fields": ...}` for config, `{"models": ..., "imported": ...}` for sync). Enforce list limit `1..50`. Remove the single-public-key input from the embedded configuration view and direct all public-key creation through the existing key-management view. Parse model prices from `input_price_yuan_per_million`/`output_price_yuan_per_million` and legacy `input_price`/`output_price` aliases; convert with round-to-nearest micro-yuan. A blank provider/admin/upstream credential in a config patch preserves the current secret; nonblank rotates it. Admin key rotation must apply after the successful response and invalidate the old key on the next request.

- [ ] **Step 5: Add configuration validation, atomic save, and hot-reload tests.**

```go
func TestConfigSaveReloadsPublicSettingsAndMarksRestartFields(t *testing.T) {
    server := newAdminServer(t)
    response := server.PutJSON("/admin/api/config", adminHeaders(), map[string]any{"settings": map[string]string{"MEMORY_LIMIT_MB": "256", "PORT": "9001"}})
    var body struct { Pending []string `json:"pending_restart_fields"` }; _ = json.Unmarshal(response.Body.Bytes(), &body)
    if response.Code != 200 || !slices.Equal(body.Pending, []string{"PORT"}) || server.MemoryLimit() != 256 { t.Fatal(response.Body.String()) }
}
```

Use a runtime object with read/write locking: active settings affect new requests immediately; managed settings retain saved restart-only values; replacement upstream clients are not closed until leases using them finish. Serialize configuration validation/write/reload under one lock so a failed atomic file write never changes live settings.

- [ ] **Step 6: Run admin and UI asset tests.**

Run: `gofmt -w internal/httpapi && go test -race ./internal/httpapi`

Expected: PASS, including a test that `GET /admin` contains representative Chinese text and referenced API paths.

- [ ] **Step 7: Commit administrator functionality.**

```powershell
git add internal/httpapi web
git commit -m "feat: port Chinese administrator console"
```

### Task 11: Add Provider Registry, Auto-Synchronization, and Cleanup Jobs

**Files:**
- Create: `internal/relay/providers.go`
- Create: `internal/relay/providers_test.go`
- Create: `internal/relay/runtime.go`
- Create: `internal/relay/runtime_test.go`
- Modify: `cmd/civic-relay/main.go`
- Test: `internal/relay/providers_test.go`, `internal/relay/runtime_test.go`

- [ ] **Step 1: Write failing provider caching and synchronizer tests.**

```go
func TestSyncImportsDisabledUnpricedModels(t *testing.T) {
    registry, repo := newRegistryFixture(t, []string{"chat", "reasoner"})
    imported, err := registry.SyncProvider(t.Context(), 1)
    if err != nil || len(imported) != 2 { t.Fatal(err) }
    models, _ := repo.ListModels(t.Context())
    if models[0].Enabled || models[0].InputPriceMicroyuan != nil { t.Fatal("imported model was activated") }
}

func TestRuntimeWaitsForLeaseBeforeClosingRotatedProviderClient(t *testing.T) {
    runtime, first, second := newRuntimeFixture(t)
    lease := runtime.Acquire()
    runtime.Reload(rotatedSettings())
    if first.Closed() { t.Fatal("closed while active") }
    lease.Release()
    if !first.Closed() || !runtime.Uses(second) { t.Fatal("rotation failed") }
}
```

- [ ] **Step 2: Run the tests.**

Run: `go test ./internal/relay -run 'Test(Sync|Runtime)' -v`

Expected: FAIL because registry and runtime implementations are missing.

- [ ] **Step 3: Implement provider client caching and explicit model synchronization.**

```go
type Registry struct { store *store.Store; mu sync.Mutex; clients map[int64]*upstream.Client }
func (r *Registry) ForModel(ctx context.Context, model store.Model) (*upstream.Client, error) { /* decrypt provider key only here, construct/cache client */ }
func (r *Registry) SyncProvider(ctx context.Context, providerID int64) ([]string, error) {
    client, err := r.ForProvider(ctx, providerID); if err != nil { return nil, err }
    names, err := client.ListModels(ctx); if err != nil { return nil, err }
    return r.store.ImportModels(ctx, providerID, names)
}
```

Ensure cache entries are deleted and old clients closed after a provider update or disable. `ImportModels` must preserve existing administrator-set alias, pricing, enablement, and group membership; insert only unknown upstream names as disabled, unpriced aliases `<provider-name>/<upstream-name>`. The admin manual sync invokes this registry. If configured legacy default upstream values are present on first boot, create one encrypted provider only if no provider yet exists; otherwise never overwrite administrator catalog data.

- [ ] **Step 4: Add context-aware periodic work in main.**

```go
func every(ctx context.Context, interval time.Duration, work func(context.Context)) {
    ticker := time.NewTicker(interval); defer ticker.Stop()
    for { select { case <-ctx.Done(): return; case <-ticker.C: work(ctx) } }
}
```

Start auto-sync only when `MODEL_AUTO_SYNC=true`; each run logs provider ID and redacted outcome only. Start a daily retention job calling `Prune` using `RETENTION_DAYS`. Use a single application context canceled on `SIGINT`/`SIGTERM`; `http.Server.Shutdown` first stops new requests, then waits for or force-closes active streams by its configured shutdown deadline.

- [ ] **Step 5: Run runtime tests.**

Run: `gofmt -w internal/relay cmd/civic-relay && go test -race ./internal/relay ./cmd/civic-relay`

Expected: PASS.

- [ ] **Step 6: Commit provider orchestration.**

```powershell
git add internal/relay cmd/civic-relay
git commit -m "feat: add provider synchronization and runtime reload"
```

### Task 12: Wire the Full Application and Verify a Clean First Start

**Files:**
- Modify: `cmd/civic-relay/main.go`
- Create: `cmd/civic-relay/e2e_test.go`
- Modify: `internal/httpapi/server.go`
- Test: `cmd/civic-relay/e2e_test.go`

- [ ] **Step 1: Write a failing fresh-install end-to-end test.**

```go
func TestFreshInstallCreatesExternalConfigAndServesAdmin(t *testing.T) {
    configPath := filepath.Join(t.TempDir(), "config", "relay.env")
    app, err := buildApplication(configPath)
    if err != nil { t.Fatal(err) }
    defer app.Close(context.Background())
    response := httptest.NewRecorder()
    app.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin", nil))
    if response.Code != http.StatusOK { t.Fatal(response.Code) }
    if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "bootstrap-admin-key.txt")); err != nil { t.Fatal(err) }
    if _, err := os.Stat(configPath); err != nil { t.Fatal(err) }
}
```

- [ ] **Step 2: Run the test.**

Run: `go test ./cmd/civic-relay -run TestFreshInstall -v`

Expected: FAIL because `buildApplication` is incomplete.

- [ ] **Step 3: Complete dependency wiring and graceful shutdown.**

```go
type Application struct { Handler http.Handler; Store *store.Store; Registry *relay.Registry; Server *http.Server }
func buildApplication(configPath string) (*Application, error) {
    settings, _, _, err := config.Ensure(configPath); if err != nil { return nil, err }
    box, err := secret.New(settings.EncryptionKey); if err != nil { return nil, err }
    database, err := store.Open(settings.DBPath, box); if err != nil { return nil, err }
    runtime := relay.NewRuntime(settings)
    registry := relay.NewRegistry(database, runtime)
    service := relay.NewService(database, registry, runtime)
    handler := httpapi.NewServer(runtime, database, service, registry, memory.Default(settings.MemoryLimitMB)).Handler()
    return &Application{Handler: handler, Store: database, Registry: registry}, nil
}
```

`main` must set `CIVIC_RELAY_CONFIG_FILE` only from its environment, construct this application, listen on configured `HOST:PORT`, log the address but no secret values, and close server, registry, then SQLite deterministically. Register no public `/docs` route unless `DOCS_ENABLED=true`; if enabled, serve only a small local endpoint under administrator protection, never an unauthenticated interactive console.

- [ ] **Step 4: Add full behavior E2E tests.**

```go
func TestFreshInstallAdminCreatesProviderModelGroupAndKeyThenRelays(t *testing.T) {
    // With httptest upstream: authenticate bootstrap admin, create provider, sync/import a model,
    // set price and enabled, create group, assign model, create key, assign group, then call
    // /v1/models and /v1/chat/completions. Assert no JSON response includes the provider key.
}
```

Cover non-stream/stream success, client abort, upstream 401/429/timeout, malformed JSON, payload maximum, all quota limits, global and key concurrency rejection, auto-disable, model/group changes, memory 503/SSE error, config hot reload, restart markers, and a database path with no Python migration behavior.

- [ ] **Step 5: Run the whole Go suite and race detector.**

Run: `go test -race ./...`

Expected: PASS.

- [ ] **Step 6: Commit application assembly.**

```powershell
git add cmd internal web
git commit -m "feat: assemble Civic Relay Go application"
```

### Task 13: Replace Python Build and Deployment Artifacts

**Files:**
- Modify: `Dockerfile`
- Modify: `docker-compose.yml`
- Modify: `civic-relay.service`
- Modify: `ai-relay.service`
- Modify: `README.md`
- Delete: `pyproject.toml`
- Delete: `uv.lock`
- Delete: `requirements.txt`
- Delete: `app.py`
- Delete: `admin_api.py`
- Delete: `admin_ui.py`
- Delete: `config.py`
- Delete: `config_store.py`
- Delete: `db.py`
- Delete: `limiter.py`
- Delete: `provider_registry.py`
- Delete: `runtime.py`
- Delete: `tenant_store.py`
- Delete: `upstream.py`
- Delete: `tests/`
- Test: `cmd/civic-relay/e2e_test.go`

- [ ] **Step 1: Write deployment text tests before modifying artifacts.**

```go
func TestDockerfileBuildsStaticGoRuntimeAsNonRoot(t *testing.T) {
    body, _ := os.ReadFile(filepath.Join(projectRoot(t), "Dockerfile"))
    text := string(body)
    for _, required := range []string{"FROM golang:", "CGO_ENABLED=0", "USER relay", "COPY --from=build", "civic-relay"} {
        if !strings.Contains(text, required) { t.Fatalf("Dockerfile missing %q", required) }
    }
}
func TestComposeKeepsHostPortLoopbackOnly(t *testing.T) {
    body, _ := os.ReadFile(filepath.Join(projectRoot(t), "docker-compose.yml"))
    if !strings.Contains(string(body), "127.0.0.1:8000:8000") { t.Fatal("public port binding") }
}
```

- [ ] **Step 2: Run deployment text tests.**

Run: `go test ./cmd/civic-relay -run 'Test(Dockerfile|Compose)' -v`

Expected: FAIL until the artifact assertions and Go deployment exist.

- [ ] **Step 3: Replace the Dockerfile with a multi-stage, non-root Go image.**

```dockerfile
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/civic-relay ./cmd/civic-relay

FROM alpine:3.22
RUN addgroup -g 10001 relay && adduser -D -H -u 10001 -G relay relay && apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/civic-relay /app/civic-relay
RUN mkdir -p /app/data && chown -R relay:relay /app
USER relay
EXPOSE 8000
ENTRYPOINT ["/app/civic-relay"]
```

Keep Compose external config mount at `/app/config`, persistent `./data:/app/data`, `CIVIC_RELAY_CONFIG_FILE=/app/config/relay.env`, restart policy, and `127.0.0.1:8000:8000`. Add `mem_limit: 256m` as the hard cgroup defense while retaining the default 200 MB soft guard; document that operators can raise both consistently. Replace systemd `ExecStart` with `/opt/civic-ai-relay/civic-relay`, retain `CIVIC_RELAY_CONFIG_FILE=/etc/civic-relay/relay.env`, one process, non-root user, `NoNewPrivileges=true`, `PrivateTmp=true`, `ProtectSystem=strict`, explicit writable `ReadWritePaths=/opt/civic-ai-relay/data /etc/civic-relay`, and `MemoryMax=256M`.

- [ ] **Step 4: Rewrite README deployment instructions for Go and fresh database only.**

Document `go test -race ./...`, `go build ./cmd/civic-relay`, first-start external config location, one-time bootstrap administrator key, new administrator provider/model/group/key workflow, client examples, LAN-only HTTP boundary, HTTPS reverse proxy for any untrusted path, Docker/systemd commands, backup-only handling for an old database, and a clear statement that no upstream key ships in the repository or binary. Remove all `uv`, Python, and `.env` workflow references. Keep GitHub safety rules including ignored `docs/`, `.codex/`, `*.ps1`, `.env`, database, certificate, and key artifacts.

- [ ] **Step 5: Build and render deployment configuration.**

Run: `go test ./... && go build -o civic-relay.exe ./cmd/civic-relay && docker compose config && docker build -t civic-relay:go .`

Expected: all commands exit `0`; Compose output keeps a loopback-only port binding and an external config directory requirement.

- [ ] **Step 6: Delete Python runtime only after all Go checks in this plan pass.**

Run: `git rm pyproject.toml uv.lock requirements.txt app.py admin_api.py admin_ui.py config.py config_store.py db.py limiter.py provider_registry.py runtime.py tenant_store.py upstream.py tests`

Expected: staged deletions only after the Go build, race suite, Docker build, Compose render, and E2E tests are green. Do not delete `docs/`, `.codex/`, or external `C:\ProgramData\CivicRelay\relay.env`.

- [ ] **Step 7: Verify that no old runtime reference remains.**

Run: `rg -n "uvicorn|FastAPI|pytest|python|pyproject|requirements\.txt" README.md Dockerfile docker-compose.yml civic-relay.service ai-relay.service .github 2>$null; go test -race ./...; git status --short`

Expected: no production Python reference, Go tests PASS, and only intended tracked migration changes appear.

- [ ] **Step 8: Commit the runtime cutover.**

```powershell
git add Dockerfile docker-compose.yml civic-relay.service ai-relay.service README.md .gitignore cmd internal web
git add -u
git commit -m "feat!: migrate Civic Relay runtime to Go"
```

### Task 14: Final Fresh-Install and Security Acceptance

**Files:**
- Modify: `cmd/civic-relay/e2e_test.go`
- Modify: `README.md`
- Test: `cmd/civic-relay/e2e_test.go`

- [ ] **Step 1: Add final acceptance assertions.**

```go
func TestAcceptanceDoesNotPersistSecretsOrContent(t *testing.T) {
    app := newAcceptanceApp(t)
    // Relay a request with distinct prompt text and provider secret.
    // Inspect provider rows, request metadata, administrator config/list APIs, and logs captured by the test.
    // Assert none contains the prompt, completion, client token, administrator key, or provider secret.
}

func TestAcceptanceUsesEmptyFreshCatalog(t *testing.T) {
    app := newAcceptanceApp(t)
    response := app.AdminGet("/admin/api/providers")
    if response.Code != 200 || string(response.Body.Bytes()) != "{\"data\":[]}\\n" { t.Fatal(response.Body.String()) }
}
```

- [ ] **Step 2: Run acceptance checks.**

Run: `go test -race ./cmd/civic-relay -run TestAcceptance -v`

Expected: PASS.

- [ ] **Step 3: Perform clean manual startup without reading the existing configuration content.**

Run: `$testConfig = Join-Path $env:TEMP ("civic-relay-go-" + [guid]::NewGuid()); New-Item -ItemType Directory -Force $testConfig | Out-Null; $env:CIVIC_RELAY_CONFIG_FILE = Join-Path $testConfig 'relay.env'; .\civic-relay.exe`

Expected: service starts on `127.0.0.1:8000`, creates a new `relay.env` and `bootstrap-admin-key.txt` in the temporary directory, and never prints the bootstrap key value. Stop it with `Ctrl+C`, then remove only the explicitly created `$testConfig` directory through the normal temporary-file cleanup process.

- [ ] **Step 4: Run final checks and inspect the intended release diff.**

Run: `go test -race ./...; go vet ./...; docker compose config; git diff --check; git status --short`

Expected: all checks PASS and no external configuration, database, binary, `.codex/`, or `docs/` private content is staged.

- [ ] **Step 5: Commit acceptance coverage and documentation corrections.**

```powershell
git add cmd/civic-relay/e2e_test.go README.md
git commit -m "test: verify clean Go relay installation"
```

## Execution Notes

- Keep commits scoped to the listed task. Do not amend or reset user changes.
- Preserve the old Python implementation until Task 13 Step 6. New Go source should coexist during tasks 1-12 and use a distinct test database path.
- Test fixture credentials must be synthetic values such as `provider-secret` and never read `C:\ProgramData\CivicRelay\relay.env`.
- During the first Go deployment, configure fresh providers, prices, groups, and client keys in `/admin`; do not copy old production credentials or key records into repository files.
- Do not publish port `8000` to a public interface. LAN HTTP remains an explicitly trusted-network option; use HTTPS reverse proxy for any traffic crossing an untrusted segment or future Internet multi-user deployment.
