# Civic Relay Admin Console Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an internal, privacy-preserving administration console that monitors Civic Relay and safely hot reloads runtime configuration from its managed environment file.

**Architecture:** Keep the existing FastAPI relay as the sole deployable process. Add focused modules for typed configuration-file persistence, immutable runtime generations, and administration routes; extend the existing SQLite ledger and concurrency guard for monitoring. `/admin` is a same-origin, dependency-free static page that calls protected JSON APIs every two seconds while visible.

**Tech Stack:** Python 3.10+, FastAPI, httpx, SQLite, asyncio, standard-library HTML/JavaScript, pytest, uv.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `config.py` | Parse the common settings mapping, validate `ADMIN_API_KEY`, describe secret and restart-required fields, and produce redacted configuration. |
| `config_store.py` | Read/write the managed `.env`-format file without exposing raw contents through APIs; validate and atomically replace it. |
| `runtime.py` | Hold immutable active configuration generations, lease the correct upstream client to requests, and swap runtime-safe settings atomically. |
| `limiter.py` | Track the current active count and allow its configured ceiling to change safely. |
| `db.py` | Return one consistent, redacted monitoring snapshot and recent request metadata. |
| `admin_ui.py` | Serve the static `/admin` HTML shell with sign-in, dashboard, records, quota, and settings views. |
| `admin_api.py` | Define authenticated `/admin/api/*` endpoints and coordinate config validation/reload. |
| `app.py` | Wire runtime state, existing relay requests, and the admin router together. |
| `tests/test_config.py` | Cover mapping/file configuration, redaction, and key validation. |
| `tests/test_config_store.py` | Cover file persistence and rollback on invalid candidate/write failure. |
| `tests/test_runtime.py` | Cover generation leases, upstream replacement, and reconfigurable concurrency. |
| `tests/test_db.py` | Cover monitoring aggregate and redacted recent row queries. |
| `tests/test_admin.py` | Cover admin authentication, metrics, config APIs, privacy, and hot reload. |
| `.env.example`, `docker-compose.yml`, `civic-relay.service`, `README.md`, `.gitignore` | Document/configure the managed file, internal deployment, and ignored brainstorming output. |

`ai-relay.service` is a redundant unit variant. Update both service files identically or remove neither; keeping both avoids an unrelated deployment-breaking deletion.

## Task 1: Establish the Admin Configuration Contract

**Files:**
- Modify: `config.py`
- Modify: `tests/conftest.py`
- Modify: `tests/test_config.py`

- [ ] **Step 1: Write the failing settings tests**

Add `ADMIN_API_KEY` to the baseline test environment and helper values. Add tests that exercise a mapping directly, reject equal public/admin keys, and prove redaction never returns a key:

```python
def test_settings_from_mapping_and_redacted_view(tmp_path):
    values = required_values(tmp_path)
    values["ADMIN_API_KEY"] = "admin-secret"

    settings = Settings.from_mapping(values)

    assert settings.admin_api_key == "admin-secret"
    assert settings.redacted()["UPSTREAM_BASE_URL"] == "https://provider.example"
    assert settings.redacted()["PUBLIC_API_KEY"] == {"is_configured": True}
    assert "admin-secret" not in repr(settings.redacted())


def test_settings_rejects_admin_key_equal_to_public_key(tmp_path):
    values = required_values(tmp_path)
    values["ADMIN_API_KEY"] = values["PUBLIC_API_KEY"]

    with pytest.raises(SettingsError, match="ADMIN_API_KEY"):
        Settings.from_mapping(values)
```

Define `required_values(tmp_path)` in the test module with all required string
keys, including `ADMIN_API_KEY`; do not depend on process environment for this
new parsing path.

- [ ] **Step 2: Run the focused tests and verify failure**

Run:

```powershell
uv run pytest tests/test_config.py -q
```

Expected: collection or assertions fail because `Settings` has no
`admin_api_key`, `from_mapping`, or `redacted` interface.

- [ ] **Step 3: Refactor `Settings` to parse a generic string mapping**

Change parser helpers to take a mapping and key name rather than reading
`os.environ` directly. Keep `Settings.from_env()` as the compatibility wrapper.
Add the configuration metadata near the module top:

```python
SECRET_SETTING_NAMES = frozenset({
    "PUBLIC_API_KEY", "UPSTREAM_API_KEY", "ADMIN_API_KEY",
})
RESTART_REQUIRED_SETTING_NAMES = frozenset({
    "HOST", "PORT", "DB_PATH", "DOCS_ENABLED",
})


def required(values: Mapping[str, str], name: str) -> str:
    value = values.get(name)
    if value is None or not value.strip():
        raise _invalid(name)
    return value.strip()
```

Extend the frozen dataclass and constructor path:

```python
@dataclass(frozen=True)
class Settings:
    public_api_key: str
    admin_api_key: str
    upstream_base_url: str
    # retain every existing field, unchanged

    @classmethod
    def from_mapping(cls, values: Mapping[str, str]) -> "Settings":
        public_api_key = required(values, "PUBLIC_API_KEY")
        admin_api_key = required(values, "ADMIN_API_KEY")
        if hmac.compare_digest(public_api_key, admin_api_key):
            raise _invalid("ADMIN_API_KEY")
        return cls(
            public_api_key=public_api_key,
            admin_api_key=admin_api_key,
            # call the existing validation logic with `values` for every field
        )

    @classmethod
    def from_env(cls) -> "Settings":
        return cls.from_mapping({name: value for name, value in os.environ.items() if value is not None})

    def redacted(self) -> dict[str, object]:
        return {
            "PUBLIC_API_KEY": {"is_configured": bool(self.public_api_key)},
            "ADMIN_API_KEY": {"is_configured": bool(self.admin_api_key)},
            "UPSTREAM_API_KEY": {"is_configured": bool(self.upstream_api_key)},
            "UPSTREAM_BASE_URL": self.upstream_base_url,
            # include every non-secret setting using its ENV-style name
        }
```

Import `hmac` and `Mapping`. Do not include a secret in a validation exception,
`repr`, API mapping, or log message. Update `tests/conftest.py` collection-time
environment with `ADMIN_API_KEY=collection-admin` and update `build_settings()`
with `admin_api_key="admin-test"`.

- [ ] **Step 4: Run the configuration suite**

Run:

```powershell
uv run pytest tests/test_config.py -q
```

Expected: PASS, including all existing parsing tests after their fixtures supply
the new required key.

- [ ] **Step 5: Record a local checkpoint**

The current project directory has no `.git` directory, so do not invent a Git
repository. Record the passing command and changed files in the implementation
session; make the corresponding conventional commit only after the user places
this project under Git.

## Task 2: Add Safe Managed Configuration File Storage

**Files:**
- Create: `config_store.py`
- Create: `tests/test_config_store.py`
- Modify: `config.py`

- [ ] **Step 1: Write failing config-store tests**

Test three observable contracts: default path resolution, successful atomic
persistence, and invalid save preserving both file and active settings input.

```python
def test_store_writes_candidate_and_preserves_blank_secret(tmp_path):
    path = tmp_path / ".env"
    path.write_text("PUBLIC_API_KEY=public\nADMIN_API_KEY=admin\nUPSTREAM_API_KEY=upstream\n" + required_body(), encoding="utf-8")
    store = ConfigStore(path)

    saved = store.build_candidate({"RPM_LIMIT": "45", "UPSTREAM_API_KEY": ""})
    store.write(saved)

    text = path.read_text(encoding="utf-8")
    assert "RPM_LIMIT=45" in text
    assert "UPSTREAM_API_KEY=upstream" in text


def test_store_rejects_invalid_candidate_without_changing_file(tmp_path):
    path = write_valid_env(tmp_path)
    before = path.read_text(encoding="utf-8")
    store = ConfigStore(path)

    with pytest.raises(SettingsError, match="RPM_LIMIT"):
        store.build_candidate({"RPM_LIMIT": "0"})

    assert path.read_text(encoding="utf-8") == before
```

`required_body()` must contain valid non-secret settings, including all current
defaults, so the test is independent of shell environment.

- [ ] **Step 2: Run the new test module and verify failure**

Run:

```powershell
uv run pytest tests/test_config_store.py -q
```

Expected: FAIL because `config_store` does not exist.

- [ ] **Step 3: Implement `ConfigStore` with canonical `.env` serialization**

Use only the standard library. The parser accepts blank lines, comments,
`export KEY=value`, unquoted values, and JSON-style double-quoted values; it
rejects malformed managed keys. Preserve unknown variables in the mapping so a
save cannot silently drop unrelated deployment values.

```python
class ConfigStore:
    def __init__(self, path: Path):
        self.path = path

    def read_mapping(self) -> dict[str, str]:
        return parse_env_text(self.path.read_text(encoding="utf-8"))

    def build_candidate(self, patch: Mapping[str, str | None]) -> Settings:
        values = self.read_mapping()
        for name, raw_value in patch.items():
            if name in SECRET_SETTING_NAMES and not raw_value:
                continue
            if raw_value is not None:
                values[name] = raw_value
        return Settings.from_mapping(values)

    def write(self, settings: Settings) -> None:
        values = self.read_mapping()
        values.update(settings.to_env_mapping())
        content = serialize_env_mapping(values)
        fd, temporary_name = tempfile.mkstemp(prefix=f".{self.path.name}.", dir=self.path.parent, text=True)
        temporary = Path(temporary_name)
        try:
            with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
                handle.write(content)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, self.path)
        except Exception:
            temporary.unlink(missing_ok=True)
            raise
```

Implement `Settings.to_env_mapping()` as the complete, exact ENV-name mapping,
including secrets only for file persistence. Keep it private to server-side
code: neither router nor response object receives that mapping. Resolve a file
path with:

```python
def managed_config_path() -> Path:
    configured = os.getenv("CIVIC_RELAY_CONFIG_FILE")
    return Path(configured) if configured else Path(".env")
```

Do not call `write()` until the candidate parses successfully. Tests should
monkeypatch `os.replace` to raise once and assert the original file remains
unchanged and no temporary file is left behind.

- [ ] **Step 4: Run config and storage tests**

Run:

```powershell
uv run pytest tests/test_config.py tests/test_config_store.py -q
```

Expected: PASS.

- [ ] **Step 5: Record a local checkpoint**

Record the test result and changed files. Do not commit because `git rev-parse`
currently fails in this directory.

## Task 3: Make the Concurrency Limit Reconfigurable

**Files:**
- Modify: `limiter.py`
- Modify: `tests/test_limiter.py`

- [ ] **Step 1: Add failing limit-update tests**

```python
@pytest.mark.asyncio
async def test_lowered_limit_keeps_existing_lease_but_rejects_new_work():
    guard = ConcurrencyGuard(limit=2)
    assert await guard.try_acquire() is True
    assert await guard.try_acquire() is True

    await guard.set_limit(1)

    assert guard.active == 2
    assert guard.limit == 1
    assert await guard.try_acquire() is False
    await guard.release()
    assert await guard.try_acquire() is False
    await guard.release()
    assert await guard.try_acquire() is True
```

- [ ] **Step 2: Run the limiter tests and verify failure**

Run:

```powershell
uv run pytest tests/test_limiter.py -q
```

Expected: FAIL because `set_limit` and `limit` are absent.

- [ ] **Step 3: Implement guarded configuration and snapshot access**

Add a locked update and read-only properties:

```python
@property
def limit(self) -> int:
    return self._limit

async def set_limit(self, limit: int) -> None:
    if limit < 1:
        raise ValueError("limit must be positive")
    async with self._lock:
        self._limit = limit

async def snapshot(self) -> tuple[int, int]:
    async with self._lock:
        return self._active, self._limit
```

Leave `try_acquire()` non-blocking and preserve its rejection semantics.

- [ ] **Step 4: Run the limiter tests**

Run:

```powershell
uv run pytest tests/test_limiter.py -q
```

Expected: PASS.

## Task 4: Implement Runtime Generations and Upstream Leases

**Files:**
- Create: `runtime.py`
- Create: `tests/test_runtime.py`
- Modify: `upstream.py`

- [ ] **Step 1: Write failing generation tests**

Use a small fake client with an async `close()` that records close order.

```python
@pytest.mark.asyncio
async def test_reload_defers_old_upstream_close_until_lease_finishes(tmp_path):
    first = RecordingUpstream("first")
    second = RecordingUpstream("second")
    runtime = RuntimeConfig(build_settings(tmp_path), first, factory=lambda settings: second)

    async with runtime.lease() as leased:
        result = await runtime.reload(build_settings(tmp_path, upstream_api_key="rotated-secret"))
        assert leased.upstream is first
        assert result.pending_restart_fields == ()
        assert first.closed is False

    assert first.closed is True
    async with runtime.lease() as current:
        assert current.upstream is second
```

Also test a `host` change reports `("HOST",)` while `runtime.settings.host`
remains old, and that changing `global_concurrency_limit` updates the shared
guard immediately.

- [ ] **Step 2: Run runtime tests and verify failure**

Run:

```powershell
uv run pytest tests/test_runtime.py -q
```

Expected: FAIL because `runtime.py` does not exist.

- [ ] **Step 3: Implement immutable runtime generations**

Define explicit types rather than storing mutable dictionaries:

```python
@dataclass(frozen=True)
class RuntimeLease:
    generation: int
    settings: Settings
    upstream: Any

@dataclass(frozen=True)
class ReloadResult:
    applied_at_utc: str
    pending_restart_fields: tuple[str, ...]

class RuntimeConfig:
    def __init__(self, settings: Settings, upstream: Any, guard: ConcurrencyGuard, factory: Callable[[Settings], Any]):
        self._settings = settings
        self._managed_settings = settings
        self._upstreams = {0: upstream}
        self._leases = {0: 0}
        self._generation = 0
        self._guard = guard
        self._factory = factory
        self._lock = asyncio.Lock()
```

`lease()` increments the current generation count under `_lock`, yields the
snapshot, then decrements and closes a retired upstream outside the lock only
when no lease remains. `reload(candidate)` must prepare a replacement
`UpstreamClient` before acquiring the swap lock when the upstream fingerprint
changes. It keeps active values for
`RESTART_REQUIRED_SETTING_NAMES` using `dataclasses.replace`, records the full
candidate as `_managed_settings`, updates the guard with `set_limit`, and sets
the `civic-relay` logger level from the new effective setting. If preparing a
new upstream fails, leave every current value untouched.

Add a small factory in `upstream.py` so the runtime never duplicates timeout
construction:

```python
def build_upstream(settings: Settings) -> UpstreamClient:
    return UpstreamClient(
        settings.upstream_base_url,
        settings.upstream_api_key,
        settings.upstream_connect_timeout,
        settings.upstream_read_timeout,
        settings.upstream_write_timeout,
        settings.upstream_pool_timeout,
    )
```

Use a `TYPE_CHECKING` import for `Settings` in `upstream.py` to avoid an import
cycle at runtime.

- [ ] **Step 4: Run runtime and limiter tests**

Run:

```powershell
uv run pytest tests/test_runtime.py tests/test_limiter.py -q
```

Expected: PASS.

- [ ] **Step 5: Record a local checkpoint**

Record the completed test commands and changed files; Git commits remain
unavailable until the directory is initialized as a repository.

## Task 5: Add Ledger Monitoring Queries

**Files:**
- Modify: `db.py`
- Modify: `tests/test_db.py`

- [ ] **Step 1: Write failing deterministic monitoring tests**

Seed completed, failed, and rejected rows at known timestamps, then assert all
aggregate values and redaction:

```python
@pytest.mark.asyncio
async def test_monitoring_snapshot_and_recent_rows_are_redacted(db):
    now = datetime(2026, 9, 1, 4, 0, tzinfo=UTC)
    completed = await db.reserve("completed-id", stamp(now - timedelta(minutes=2)), "2026-09-01", "m", False, 3, 8, 30, 1000, 1000)
    await db.settle(completed.id, stamp(now - timedelta(minutes=1)), "completed", 3, 2, 5, 200, 100)
    rejected = await db.reserve("rejected-id", stamp(now), "2026-09-01", "m", True, 3, 8, 30, 1000, 1000)
    await db.cancel(rejected.id, stamp(now), 429, 1)

    snapshot = await db.monitoring_snapshot(stamp(now), "2026-09-01", recent_limit=50)

    assert snapshot["rpm_count"] == 2
    assert snapshot["five_hour_tokens"] == 5
    assert snapshot["daily_tokens"] == 5
    assert snapshot["last_hour"]["completed"] == 1
    assert snapshot["last_hour"]["rejected"] == 1
    assert snapshot["recent"][0] == {
        "request_id": "rejected-id", "started_at_utc": stamp(now), "model": "m",
        "stream": True, "status": "rejected", "http_status": 429,
        "duration_ms": 1, "charged_tokens": 0,
    }
```

- [ ] **Step 2: Run the database suite and verify failure**

Run:

```powershell
uv run pytest tests/test_db.py -q
```

Expected: FAIL because `monitoring_snapshot` does not exist.

- [ ] **Step 3: Implement one read-consistent monitoring snapshot**

Add `monitoring_snapshot(now_utc, billing_date_bj, recent_limit)` and
`recent_requests(limit)`. Both should call synchronous helpers through
`asyncio.to_thread`; clamp `limit` to 1-50 in the public method.

Inside a single SQLite read transaction, execute bounded parameterized queries
for the current 60-second RPM count, rolling 5-hour token sum, local-day token
sum, last-hour outcome counts, 12 five-minute request-count buckets, and recent
rows. Shape rows manually rather than returning `SELECT *`:

```python
SELECT request_id, started_at_utc, model, stream, status,
       http_status, duration_ms, charged_tokens
FROM requests
ORDER BY id DESC
LIMIT ?
```

Set error rate to `failed / (completed + failed)` and return `0.0` when no
completed or failed requests exist. Keep rejected responses in a separate
count. The snapshot may expose only the fields asserted above plus documented
aggregate fields; never add prompt/completion columns because they do not exist
and must not be introduced.

- [ ] **Step 4: Run the database suite**

Run:

```powershell
uv run pytest tests/test_db.py -q
```

Expected: PASS.

## Task 6: Build the Protected Administration API

**Files:**
- Create: `admin_api.py`
- Create: `tests/test_admin.py`
- Modify: `app.py`
- Modify: `tests/conftest.py`

- [ ] **Step 1: Add failing route/authentication tests**

Create `admin_headers()` in `tests/conftest.py` and cover both the negative and
positive paths:

```python
def admin_headers(key: str = "admin-test") -> dict[str, str]:
    return {"X-Admin-Key": key}


@pytest.mark.asyncio
async def test_admin_endpoints_require_independent_admin_key(app_client):
    client, _ = app_client
    for path in ("/admin/api/overview", "/admin/api/requests", "/admin/api/config"):
        assert (await client.get(path)).status_code == 401
        assert (await client.get(path, headers=admin_headers("public-test"))).status_code == 401

    response = await client.get("/admin/api/overview", headers=admin_headers())
    assert response.status_code == 200
    assert response.json()["concurrency"] == {"active": 0, "limit": 2}


@pytest.mark.asyncio
async def test_admin_config_is_redacted_and_recent_records_exclude_content(app_client):
    client, _ = app_client
    config = await client.get("/admin/api/config", headers=admin_headers())
    assert config.status_code == 200
    assert "upstream-secret" not in config.text
    assert config.json()["settings"]["UPSTREAM_API_KEY"] == {"is_configured": True}
```

Construct the admin-aware app fixture with a temporary valid managed env file,
`ConfigStore`, and `RuntimeConfig`; this avoids tests touching the project's
real `.env`.

- [ ] **Step 2: Run the admin tests and verify failure**

Run:

```powershell
uv run pytest tests/test_admin.py -q
```

Expected: FAIL because the routes and fixture dependencies do not exist.

- [ ] **Step 3: Implement an explicit admin service and router**

Keep dependency ownership in a small object, not module globals:

```python
@dataclass
class AdminService:
    runtime: RuntimeConfig
    store: ConfigStore
    ledger: Ledger
    guard: ConcurrencyGuard
    started_at_utc: datetime

    def authorized(self, request: Request) -> bool:
        supplied = request.headers.get("X-Admin-Key", "")
        return bool(supplied) and hmac.compare_digest(supplied, self.runtime.settings.admin_api_key)
```

`make_admin_router(service)` returns an `APIRouter` with the five endpoints from
the design specification. Each route calls a single `require_admin(request)`
dependency that returns a generic 401 `JSONResponse` on failure. The overview
route combines `ledger.monitoring_snapshot()` and `await guard.snapshot()` into
the documented response. The requests route rejects non-integer limits and
clamps valid values to 1-50.

For configuration endpoints, accept only a JSON object whose `settings` value
is a JSON object of string or null fields. Reject unknown setting names with a
field name only. `POST /validate` calls `store.build_candidate()` and returns:

```json
{"valid": true, "changed_fields": ["RPM_LIMIT"]}
```

`PUT /config` performs, in this exact order: build candidate; calculate changed
field names; prepare runtime reload; atomically write the candidate; finalize
the prepared swap; and return `applied_at_utc`, `changed_fields`, and
`pending_restart_fields`. To preserve file/runtime atomicity, add
`RuntimeConfig.prepare_reload()` and `commit_prepared_reload()` rather than
swapping before `ConfigStore.write()` succeeds. On a write failure, discard the
prepared upstream without modifying current state. If a race is possible,
protect validate/write/commit with one `asyncio.Lock` held by `AdminService`.

Update `app.py` to create the admin service, include its router, and save it as
`app.state.admin_service`. Existing API routes must obtain their settings and
upstream via `async with runtime.lease()` rather than closing over startup
settings. Keep request IDs and existing OpenAI error shapes unchanged.

- [ ] **Step 4: Run admin and existing acceptance tests**

Run:

```powershell
uv run pytest tests/test_admin.py tests/test_acceptance.py -q
```

Expected: PASS. A public bearer key must still work only for `/v1/*`, while an
admin key must still not authenticate it.

## Task 7: Verify Hot Reload Through Real Relay Requests

**Files:**
- Modify: `tests/test_admin.py`
- Modify: `app.py`
- Modify: `runtime.py`

- [ ] **Step 1: Write end-to-end reload tests**

Test key rotation, model reload, lowered concurrency, and active-stream safety
with the existing fake upstream infrastructure:

```python
@pytest.mark.asyncio
async def test_config_save_rotates_public_key_and_model_for_new_requests(admin_client):
    client, application = admin_client
    response = await client.put(
        "/admin/api/config",
        headers=admin_headers(),
        json={"settings": {
            "PUBLIC_API_KEY": "new-public", "MODEL_WHITELIST": "new-chat",
            "MODEL_MAP_JSON": '{"new-chat":"provider-new"}',
        }},
    )
    assert response.status_code == 200
    assert (await client.get("/v1/models", headers={"Authorization": "Bearer public-test"})).status_code == 401
    models = await client.get("/v1/models", headers={"Authorization": "Bearer new-public"})
    assert models.json()["data"] == [{"id": "new-chat", "object": "model", "created": 0, "owned_by": "relay"}]


@pytest.mark.asyncio
async def test_static_setting_save_reports_restart_required(admin_client):
    client, _ = admin_client
    response = await client.put("/admin/api/config", headers=admin_headers(), json={"settings": {"PORT": "9001"}})
    assert response.status_code == 200
    assert response.json()["pending_restart_fields"] == ["PORT"]
```

Add a delayed streaming fake test: admit a stream, rotate upstream settings,
assert the old client is not closed until the stream ends, and verify the next
request uses the replacement client.

- [ ] **Step 2: Run reload tests and verify failure**

Run:

```powershell
uv run pytest tests/test_admin.py -q
```

Expected: FAIL until the request lifecycle retains a `RuntimeLease` through
SSE completion and the two-phase reload path is complete.

- [ ] **Step 3: Complete request-lifecycle integration**

In `app.py`, take the lease before authorization/model/output validation and
hold it through reservation, upstream work, settlement, and guard release.
Pass `lease.settings` and `lease.upstream` explicitly into `stream_response`;
do not read `runtime.settings` again while a request is in flight. The relevant
shape is:

```python
async with runtime.lease() as lease:
    settings = lease.settings
    if not _authorized(request, settings):
        return openai_error(...)
    # reserve with `settings`
    if stream:
        return StreamingResponse(
            stream_response(request, lease, upstream_payload, reservation, input_estimate, rid),
            media_type="text/event-stream",
        )
    actual = await lease.upstream.post_json(upstream_payload)
```

Because an async context exits when the endpoint returns a `StreamingResponse`,
move the streaming lease lifetime inside the generator: acquire the lease before
creating the generator and explicitly close it in the stream generator's
`finally`, or add `RuntimeConfig.acquire()` / `release()` methods and use that
lease token. Confirm every early error path releases both a guard slot and a
runtime lease exactly once.

- [ ] **Step 4: Run focused and full relay tests**

Run:

```powershell
uv run pytest tests/test_admin.py tests/test_acceptance.py tests/test_upstream.py -q
uv run pytest -q
```

Expected: both commands PASS.

- [ ] **Step 5: Record a local checkpoint**

Record the full suite result and changed files; do not run Git commands that
cannot succeed in the current directory.

## Task 8: Create the Same-Origin Administration Page

**Files:**
- Create: `admin_ui.py`
- Modify: `app.py`
- Modify: `tests/test_admin.py`

- [ ] **Step 1: Write failing static-page tests**

```python
@pytest.mark.asyncio
async def test_admin_shell_contains_no_configuration_and_calls_same_origin_api(app_client):
    client, _ = app_client
    response = await client.get("/admin")
    assert response.status_code == 200
    assert "Civic Relay" in response.text
    assert "upstream-secret" not in response.text
    assert "sessionStorage" not in response.text
    assert "localStorage" not in response.text
    assert "/admin/api/overview" in response.text
```

- [ ] **Step 2: Run the page test and verify failure**

Run:

```powershell
uv run pytest tests/test_admin.py::test_admin_shell_contains_no_configuration_and_calls_same_origin_api -q
```

Expected: FAIL because `/admin` is not registered.

- [ ] **Step 3: Implement the page as a static HTML response**

Put `ADMIN_HTML` in `admin_ui.py` and return it from an `app.get("/admin")`
route as `HTMLResponse`. Keep the page dependency-free and same-origin. The
core client state must be in memory only:

```javascript
let adminKey = "";
let refreshTimer = null;
let lastSnapshot = null;

async function adminFetch(path, options = {}) {
  const headers = new Headers(options.headers || {});
  headers.set("X-Admin-Key", adminKey);
  const response = await fetch(path, {...options, headers});
  if (response.status === 401) signOut();
  return response;
}

function startPolling() {
  stopPolling();
  refreshOverview();
  refreshTimer = window.setInterval(refreshOverview, 2000);
}

document.addEventListener("visibilitychange", () => {
  if (document.hidden) stopPolling(); else if (adminKey) startPolling();
});
```

Implement nav views exactly as the approved mockup: Overview, Request history,
Quota and limits, and Runtime configuration. Use semantic `<button>` elements
for navigation/actions, `<input type="password">` for all keys, `<input
type="number">` for numeric limits, and a `<textarea>` for model-map JSON.
Render incoming request/config data with `textContent`, never `innerHTML`, to
avoid turning upstream model names or metadata into executable markup. Secret
inputs remain blank after reads; a blank save value is omitted from the JSON
payload.

Show stale dashboard data when fetch fails, set an accessible status message,
render field errors by name, and sign out after an admin-key rotation response.

- [ ] **Step 4: Run page and admin API tests**

Run:

```powershell
uv run pytest tests/test_admin.py -q
```

Expected: PASS.

## Task 9: Update Deployment Inputs and Operator Documentation

**Files:**
- Modify: `.env.example`
- Modify: `docker-compose.yml`
- Modify: `civic-relay.service`
- Modify: `ai-relay.service`
- Modify: `README.md`
- Modify: `.gitignore`
- Modify: `tests/test_deployment.py`

- [ ] **Step 1: Write failing deployment/documentation tests**

```python
def test_admin_deployment_has_managed_config_and_internal_admin_key():
    env = (ROOT / ".env.example").read_text(encoding="utf-8")
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
    unit = (ROOT / "civic-relay.service").read_text(encoding="utf-8")

    assert "ADMIN_API_KEY=" in env
    assert "CIVIC_RELAY_CONFIG_FILE=/app/.env" in compose
    assert ".env:/app/.env" in compose
    assert "CIVIC_RELAY_CONFIG_FILE=/etc/civic-relay.env" in unit
```

- [ ] **Step 2: Run deployment tests and verify failure**

Run:

```powershell
uv run pytest tests/test_deployment.py -q
```

Expected: FAIL because deployment files do not yet define the admin key and
managed configuration path.

- [ ] **Step 3: Update deployment files and README**

Add the independent placeholder to `.env.example`:

```dotenv
ADMIN_API_KEY=admin_change_me
```

Retain `.env` in `.gitignore` and append:

```gitignore
.superpowers/
```

In Docker Compose, mount the existing host file and set the immutable path:

```yaml
environment:
  CIVIC_RELAY_CONFIG_FILE: /app/.env
volumes:
  - ./.env:/app/.env
  - ./data:/app/data
```

In both systemd units, add:

```ini
Environment=CIVIC_RELAY_CONFIG_FILE=/etc/civic-relay.env
```

Update `README.md` with the `/admin` URL, `X-Admin-Key` sign-in behavior,
internal-network/TLS requirements, no-secret-readback policy, two-second
polling, hot-applied settings, restart-required settings, Docker bind-mount
behavior, and systemd permissions such as `chmod 600 /etc/civic-relay.env`.
Do not write any real credential into documentation.

- [ ] **Step 4: Run deployment tests**

Run:

```powershell
uv run pytest tests/test_deployment.py -q
```

Expected: PASS.

## Task 10: Run Regression, Privacy, and Manual Smoke Verification

**Files:**
- Modify only if a preceding test exposes a regression.

- [ ] **Step 1: Run the complete automated suite**

Run:

```powershell
uv run pytest -q
uv run python -m compileall app.py admin_api.py admin_ui.py config.py config_store.py runtime.py db.py limiter.py upstream.py
uv lock --check
```

Expected: all tests pass, compilation reports no syntax errors, and the lock
file is current.

- [ ] **Step 2: Start the local service with an isolated configuration file**

Run from a temporary copy or with explicitly set safe test credentials, never
with an operator's real `.env`:

```powershell
$env:CIVIC_RELAY_CONFIG_FILE = "$PWD\.env"
uv run uvicorn app:app --host 127.0.0.1 --port 8001
```

Expected: the process starts once, `/healthz` returns `{"status":"ok"}`, and
`/admin` returns HTML without secret values.

- [ ] **Step 3: Exercise the two credentials separately**

In a second terminal, replace placeholders only in the shell history-free test
environment; do not place real keys in the command transcript. Verify the
behaviors with a local test client or temporary values:

```powershell
Invoke-WebRequest http://127.0.0.1:8001/admin/api/overview -UseBasicParsing
Invoke-WebRequest http://127.0.0.1:8001/healthz -UseBasicParsing
```

Expected: the first request returns HTTP 401 without `X-Admin-Key`; the second
returns HTTP 200. Then authenticate the overview endpoint with the temporary
admin key and confirm it reports active/limit concurrency and contains no key
values.

- [ ] **Step 4: Verify a live runtime setting change**

Using the page with temporary credentials, change `RPM_LIMIT`, click Save and
hot reload, and confirm the success summary has `pending_restart_fields: []`.
Then change `PORT`, save, and confirm `pending_restart_fields` lists `PORT`
while the running listener remains on port 8001. Restore the temporary file
before stopping the local process.

- [ ] **Step 5: Record final implementation evidence**

Record test commands/results, changed paths, manual smoke observations, and
the known Docker limitation if Docker daemon is unavailable. Do not claim a
Git commit in this non-repository directory.

## Plan Self-Review

**Spec coverage:** Tasks 1-2 cover secret redaction, independent admin keys,
managed source-of-truth file, validation, and atomic persistence. Tasks 3-4
cover concurrency and safe generation replacement. Task 5 implements every
dashboard statistic and redacted record. Tasks 6-8 cover all five admin APIs,
the protected interface, polling, errors, and key rotation. Task 9 covers
uv/systemd/Docker operator setup. Task 10 verifies privacy, regression, and
manual hot/restart-required behavior.

**Placeholder scan:** No implementation task uses an unresolved placeholder;
all file paths, route names, expected test commands, persistence ordering, and
runtime behaviors are specified.

**Type consistency:** `Settings`, `ConfigStore`, `RuntimeConfig`,
`ConcurrencyGuard`, and `AdminService` are the single named interfaces used
throughout. `RuntimeLease` is held for both non-streaming and streaming request
lifetimes, and `ReloadResult.pending_restart_fields` is the response field used
by both API and page tasks.
