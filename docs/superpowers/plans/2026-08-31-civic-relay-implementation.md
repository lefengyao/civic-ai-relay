# Civic Relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a uv-managed, single-process OpenAI-compatible AI API relay with durable SQLite limits and safe streaming support.

**Architecture:** `app.py` owns the HTTP request lifecycle, delegates durable reservation and settlement to `db.py`, uses `limiter.py` only for instance-local concurrency and conservative token estimates, and delegates HTTP forwarding to `upstream.py`. Each request obtains its SQLite reservation before a non-blocking concurrency check, and every path either cancels the reservation before upstream work or settles it before releasing the slot.

**Tech Stack:** Python 3.11 (3.10+ supported), FastAPI, httpx, SQLite, uv, pytest, pytest-asyncio.

---

## File Structure

The current workspace directory moves from `中转站` to `civic-ai-relay` before any project files are added. The existing scheme and `docs/` move with it.

| File | Responsibility |
| --- | --- |
| `config.py` | Parse, validate, and expose immutable settings. |
| `db.py` | Create SQLite schema; atomically reserve, cancel, settle, and inspect request ledger entries. |
| `limiter.py` | Non-waiting global concurrency lease and character-based conservative token estimates. |
| `upstream.py` | Reusable `httpx.AsyncClient`, endpoint construction, SSE usage extraction, and safe upstream errors. |
| `app.py` | FastAPI routes, OpenAI error envelopes, authentication, lifecycle orchestration, logging. |
| `tests/conftest.py` | Isolated temporary settings/database and mocked upstream fixture. |
| `tests/test_acceptance.py` | API, quota, concurrency, stream, privacy, and error acceptance coverage. |
| `pyproject.toml`, `uv.lock` | Authoritative package metadata and locked uv environment. |
| `requirements.txt` | uv-exported compatibility dependencies for non-uv consumers. |
| deployment and documentation files | Reproducible Docker/systemd deployment and operator guidance. |

### Task 1: Rename and Bootstrap the uv Project

**Files:**
- Move: `D:/生产项目/项目/AI工作流/中转站` to `D:/生产项目/项目/AI工作流/civic-ai-relay`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/pyproject.toml`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/.gitignore`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/__init__.py`

- [ ] **Step 1: Rename the resolved workspace directory**

Run:

```powershell
Rename-Item -LiteralPath 'D:/生产项目/项目/AI工作流/中转站' -NewName 'civic-ai-relay'
```

Expected: the original path no longer exists and the renamed path contains `ai-relay-codex-build-prompt.md` and `docs/`.

- [ ] **Step 2: Create package metadata and ignore rules**

```toml
[project]
name = "civic-relay"
version = "0.1.0"
description = "A small, self-hosted OpenAI-compatible API relay"
requires-python = ">=3.10"
dependencies = [
  "fastapi>=0.115,<1",
  "httpx>=0.27,<1",
  "uvicorn[standard]>=0.30,<1",
]

[dependency-groups]
dev = [
  "pytest>=8,<9",
  "pytest-asyncio>=0.24,<1",
]

[tool.pytest.ini_options]
asyncio_mode = "auto"
testpaths = ["tests"]
```

`.gitignore` must include `.venv/`, `__pycache__/`, `.pytest_cache/`, `.env`, `data/*.db`, and `data/*.db-*`.

- [ ] **Step 3: Lock and export dependencies**

Run:

```powershell
uv lock
uv export --no-dev --no-emit-project --format requirements-txt -o requirements.txt
uv run python -c "import fastapi, httpx; print('dependencies-ready')"
```

Expected: `uv.lock` and `requirements.txt` exist, and the final command prints `dependencies-ready`.

- [ ] **Step 4: Verify repository state without altering it**

Run:

```powershell
git rev-parse --is-inside-work-tree
```

Expected: failure because the supplied workspace is not a Git repository. Record the result and do not initialize a repository without user authorization.

### Task 2: Implement Validated Configuration

**Files:**
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/test_config.py`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/config.py`

- [ ] **Step 1: Write failing configuration tests**

```python
import pytest

from config import Settings, SettingsError


def test_settings_normalize_v1_base_url_and_model_map(monkeypatch):
    monkeypatch.setenv("UPSTREAM_BASE_URL", "https://provider.example/v1/")
    monkeypatch.setenv("UPSTREAM_API_KEY", "upstream-secret")
    monkeypatch.setenv("PUBLIC_API_KEY", "public-secret")
    monkeypatch.setenv("MODEL_WHITELIST", "public-chat")
    monkeypatch.setenv("MODEL_MAP_JSON", '{"public-chat":"provider-chat"}')
    settings = Settings.from_env()
    assert settings.upstream_base_url == "https://provider.example"
    assert settings.model_map["public-chat"] == "provider-chat"


def test_settings_reject_missing_required_limit(monkeypatch):
    monkeypatch.delenv("RPM_LIMIT", raising=False)
    with pytest.raises(SettingsError, match="RPM_LIMIT"):
        Settings.from_env()
```

- [ ] **Step 2: Run the focused tests and confirm failure**

Run: `uv run pytest tests/test_config.py -q`

Expected: FAIL because `config` does not yet exist.

- [ ] **Step 3: Implement the immutable settings contract**

Implement `Settings` as a frozen dataclass and `Settings.from_env()` with these exact exposed attributes: `public_api_key`, `upstream_base_url`, `upstream_api_key`, `model_whitelist`, `model_map`, `token_limit_5h`, `token_limit_daily`, `rpm_limit`, `global_concurrency_limit`, `max_output_tokens`, `max_body_bytes`, `max_stream_duration`, `retention_days`, `db_path`, `host`, `port`, `log_level`, `docs_enabled`, and four upstream timeout values. Raise `SettingsError` using only the invalid setting name, never its value.

```python
@dataclass(frozen=True)
class Settings:
    public_api_key: str
    upstream_base_url: str
    upstream_api_key: str
    model_whitelist: tuple[str, ...]
    model_map: dict[str, str]
    token_limit_5h: int
    token_limit_daily: int
    rpm_limit: int
    global_concurrency_limit: int
    max_output_tokens: int
    max_body_bytes: int
    max_stream_duration: int
    retention_days: int
    db_path: str
    host: str
    port: int
    log_level: str
    docs_enabled: bool
    upstream_connect_timeout: float
    upstream_read_timeout: float
    upstream_write_timeout: float
    upstream_pool_timeout: float

    @classmethod
    def from_env(cls) -> "Settings":
        base_url = required_url("UPSTREAM_BASE_URL").rstrip("/")
        if base_url.endswith("/v1"):
            base_url = base_url[:-3]
        return cls(
            public_api_key=required("PUBLIC_API_KEY"),
            upstream_base_url=base_url,
            upstream_api_key=required("UPSTREAM_API_KEY"),
            model_whitelist=parse_models("MODEL_WHITELIST"),
            model_map=parse_model_map("MODEL_MAP_JSON"),
            token_limit_5h=positive_int("TOKEN_LIMIT_5H"),
            token_limit_daily=positive_int("TOKEN_LIMIT_DAILY"),
            rpm_limit=positive_int("RPM_LIMIT"),
            global_concurrency_limit=positive_int("GLOBAL_CONCURRENCY_LIMIT"),
            max_output_tokens=positive_int("MAX_OUTPUT_TOKENS"),
            max_body_bytes=positive_int("MAX_BODY_MB") * 1024 * 1024,
            max_stream_duration=positive_int("MAX_STREAM_DURATION", 1800),
            retention_days=positive_int("RETENTION_DAYS", 7),
            db_path=os.getenv("DB_PATH", "data/relay.db"),
            host=os.getenv("HOST", "0.0.0.0"),
            port=positive_int("PORT", 8000),
            log_level=os.getenv("LOG_LEVEL", "INFO"),
            docs_enabled=parse_bool("DOCS_ENABLED", False),
            upstream_connect_timeout=positive_float("UPSTREAM_CONNECT_TIMEOUT", 10),
            upstream_read_timeout=positive_float("UPSTREAM_READ_TIMEOUT", 300),
            upstream_write_timeout=positive_float("UPSTREAM_WRITE_TIMEOUT", 30),
            upstream_pool_timeout=positive_float("UPSTREAM_POOL_TIMEOUT", 10),
        )
```

- [ ] **Step 4: Run configuration tests**

Run: `uv run pytest tests/test_config.py -q`

Expected: PASS.

### Task 3: Implement SQLite Reservations and Settlement

**Files:**
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/test_db.py`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/db.py`

- [ ] **Step 1: Write failing ledger tests**

```python
async def test_second_reservation_is_rejected_when_prior_reservation_uses_quota(db):
    await db.reserve("a", now_utc, "2026-08-31", "m", False, 600, 2, 1000, 1000)
    with pytest.raises(QuotaExceeded):
        await db.reserve("b", now_utc, "2026-08-31", "m", False, 600, 2, 1000, 1000)


async def test_cancelled_reservation_releases_all_quota(db):
    record = await db.reserve("a", now_utc, "2026-08-31", "m", False, 600, 2, 1000, 1000)
    await db.cancel(record.id, now_utc)
    assert await db.occupied_tokens(now_utc, "2026-08-31") == (0, 0)


async def test_settlement_clears_reservation_and_keeps_actual_charge(db):
    record = await db.reserve("a", now_utc, "2026-08-31", "m", False, 500, 2, 1000, 1000)
    await db.settle(record.id, now_utc, "completed", 200, 80, 280, 200, 8)
    assert await db.get(record.id) == {"reserved_tokens": 0, "charged_tokens": 280}
```

- [ ] **Step 2: Run ledger tests and confirm failure**

Run: `uv run pytest tests/test_db.py -q`

Expected: FAIL because `db` does not yet exist.

- [ ] **Step 3: Implement a small SQLite ledger**

Create the specified `requests` schema and indexes. `Ledger.reserve()` must open one connection, execute `BEGIN IMMEDIATE`, check 60-second request count and both quota totals calculated as `SUM(charged_tokens + reserved_tokens)`, insert a `reserved` record, then commit. `cancel()` sets `reserved_tokens = 0`, `charged_tokens = 0`, `status = 'rejected'`, and terminal metadata. `settle()` sets `reserved_tokens = 0` and records actual input/output/charge/status atomically.

```python
conn.execute("BEGIN IMMEDIATE")
five_hour = conn.execute(
    "SELECT COALESCE(SUM(charged_tokens + reserved_tokens), 0) FROM requests WHERE started_at_utc >= ?",
    (window_start,),
).fetchone()[0]
if five_hour + reservation_tokens > five_hour_limit:
    raise QuotaExceeded("token_quota_exceeded")
```

Use UTC ISO timestamps and receive the Beijing billing date from the caller. Configure WAL, `synchronous=NORMAL`, and `busy_timeout=5000` at initialization. Prune only records older than `retention_days`.

- [ ] **Step 4: Run ledger tests**

Run: `uv run pytest tests/test_db.py -q`

Expected: PASS, including RPM, five-hour, and Beijing-date fixture cases.

### Task 4: Implement Local Concurrency and Conservative Estimation

**Files:**
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/test_limiter.py`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/limiter.py`

- [ ] **Step 1: Write failing limiter tests**

```python
def test_character_estimate_is_never_zero():
    assert estimate_tokens("") == 1
    assert estimate_tokens("abc") == 2


async def test_concurrency_guard_rejects_without_waiting():
    guard = ConcurrencyGuard(limit=1)
    assert await guard.try_acquire() is True
    assert await guard.try_acquire() is False
    await guard.release()
    assert await guard.try_acquire() is True
```

- [ ] **Step 2: Run limiter tests and confirm failure**

Run: `uv run pytest tests/test_limiter.py -q`

Expected: FAIL because `limiter` does not yet exist.

- [ ] **Step 3: Implement the concurrency guard and estimates**

```python
def estimate_tokens(text: str) -> int:
    return max(1, math.ceil(len(text) / 2))


class ConcurrencyGuard:
    async def try_acquire(self) -> bool:
        async with self._lock:
            if self._active >= self._limit:
                return False
            self._active += 1
            return True

    async def release(self) -> None:
        async with self._lock:
            self._active -= 1
```

Require a positive limit and guard against releasing below zero.

- [ ] **Step 4: Run limiter tests**

Run: `uv run pytest tests/test_limiter.py -q`

Expected: PASS.

### Task 5: Implement Upstream Transport and SSE Usage Parsing

**Files:**
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/test_upstream.py`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/upstream.py`

- [ ] **Step 1: Write failing upstream helper tests**

```python
def test_sse_event_parser_counts_delta_not_json_metadata():
    event = b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n'
    parsed = parse_sse_event(event)
    assert parsed.output_characters == 5
    assert parsed.usage is None


def test_sse_event_parser_reads_final_usage():
    event = b'data: {"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}\n\n'
    assert parse_sse_event(event).usage.total_tokens == 7
```

- [ ] **Step 2: Run helper tests and confirm failure**

Run: `uv run pytest tests/test_upstream.py -q`

Expected: FAIL because `upstream` does not yet exist.

- [ ] **Step 3: Implement the upstream client boundary**

Expose `UpstreamClient` with `post_json(payload)` and `stream(payload)` methods that always send `Authorization: Bearer <upstream key>` and call `{normalized_base}/v1/chat/completions`. Create explicit `UpstreamFailure(status_code, code)` exceptions, translating only upstream 401/403 to a generic authentication error and preserving safe 400/429 classes. `parse_sse_event()` must decode only `data:` JSON lines, collect `choices[].delta.content`, and accept final `usage` objects.

For streaming payloads, make a shallow payload copy and merge `{"include_usage": true}` into `stream_options`; do not remove other client fields.

- [ ] **Step 4: Run upstream tests**

Run: `uv run pytest tests/test_upstream.py -q`

Expected: PASS.

### Task 6: Build the FastAPI Surface and Lifecycle Cleanup

**Files:**
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/conftest.py`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/test_acceptance.py`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/app.py`

- [ ] **Step 1: Write failing acceptance tests**

```python
async def test_models_requires_constant_time_bearer_key(client):
    response = await client.get("/v1/models")
    assert response.status_code == 401
    response = await client.get("/v1/models", headers={"Authorization": "Bearer public-test"})
    assert response.json()["data"][0]["id"] == "public-chat"


async def test_stream_keeps_slot_until_iterator_finishes(client, slow_upstream):
    first = client.stream("POST", "/v1/chat/completions", json=stream_payload(), headers=auth())
    async with first as response:
        await response.aiter_bytes().__anext__()
        second = await client.post("/v1/chat/completions", json=plain_payload(), headers=auth())
        assert second.status_code == 429


async def test_usage_free_stream_is_charged_and_slot_is_released(client, ledger):
    response = await client.post("/v1/chat/completions", json=stream_payload(), headers=auth())
    record = await ledger.latest()
    assert response.status_code == 200
    assert record["charged_tokens"] > 0
    assert record["reserved_tokens"] == 0
```

Include acceptance cases for bad/missing key, model denial, both maximum-token fields and conflict, body-size rejection, RPM, 5-hour quota, Beijing rollover, non-stream upstream usage, safe upstream 401/5xx/timeout responses, unique/bounded request IDs, secret absence from errors, and `/healthz` database availability.

- [ ] **Step 2: Run acceptance tests and confirm failure**

Run: `uv run pytest tests/test_acceptance.py -q`

Expected: FAIL because `app` does not yet exist.

- [ ] **Step 3: Implement routes and a single cleanup path**

Implement `create_app(settings: Settings | None = None) -> FastAPI`, disable docs unless configured, add a body-size middleware, and define an `openai_error()` helper. In the chat handler, use this exact ownership pattern:

```python
reservation = None
acquired = False
try:
    reservation = await ledger.reserve(
        request_id=request_id,
        started_at_utc=started_at,
        billing_date_bj=billing_date,
        model=public_model,
        stream=stream,
        input_tokens=input_estimate,
        reservation_tokens=reservation_tokens,
        limits=settings,
    )
    acquired = await guard.try_acquire()
    if not acquired:
        await ledger.cancel(reservation.id, finished_at_utc=utc_now(), http_status=429)
        return error_response("concurrency limit exceeded", 429, "rate_limit_error", "concurrency_limit_exceeded")
    if stream:
        return StreamingResponse(
            stream_response(request, payload, reservation, request_id),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )
    upstream_payload = await upstream.post_json(payload)
    await settle_non_stream(reservation, upstream_payload, http_status=200)
    return JSONResponse(upstream_payload, headers={"X-Request-ID": request_id})
except UpstreamTimeout:
    await settle_if_needed(reservation, status="failed", http_status=504)
    return error_response("upstream timeout", 504, "upstream_error", "upstream_timeout")
finally:
    if not stream and acquired:
        await guard.release()
```

`stream_response()` owns the stream-specific `finally`: close the upstream context, choose usage or conservative estimate, settle, and release the guard. Check `await request.is_disconnected()` before forwarding each upstream chunk. Never log headers, payloads, prompts, completion text, or keys.

- [ ] **Step 4: Run acceptance tests**

Run: `uv run pytest tests/test_acceptance.py -q`

Expected: PASS. Confirm the slow-stream test observes a `429` before the first stream ends and succeeds after it closes.

### Task 7: Add Deployment Artifacts and Operator Documentation

**Files:**
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/.env.example`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/Dockerfile`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/docker-compose.yml`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/civic-relay.service`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/ai-relay.service`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/README.md`
- Create: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/test_deployment.py`

- [ ] **Step 1: Write deployment-content assertions**

```python
def test_deployment_defaults_to_one_uvicorn_worker():
    compose = Path("docker-compose.yml").read_text(encoding="utf-8")
    unit = Path("civic-relay.service").read_text(encoding="utf-8")
    assert "--workers" not in compose
    assert "--workers" not in unit
    assert "./data:/app/data" in compose


def test_env_example_contains_every_required_secret_and_limit():
    env = Path(".env.example").read_text(encoding="utf-8")
    for name in ("PUBLIC_API_KEY", "UPSTREAM_API_KEY", "TOKEN_LIMIT_5H", "TOKEN_LIMIT_DAILY"):
        assert f"{name}=" in env
```

- [ ] **Step 2: Run deployment assertions and confirm failure**

Run: `uv run pytest tests/test_deployment.py -q`

Expected: FAIL because deployment files do not yet exist.

- [ ] **Step 3: Create production artifacts**

`Dockerfile` must use `python:3.11-slim`, install only runtime dependencies from `requirements.txt`, create an unprivileged `relay` user, create `/app/data`, and start `uvicorn app:app --host 0.0.0.0 --port 8000` without `--workers`. Compose must build the image, use `.env`, bind `8000:8000`, mount `./data:/app/data`, and use `restart: unless-stopped`.

The systemd unit must use `EnvironmentFile=/etc/civic-relay.env`, an explicit `WorkingDirectory=/opt/civic-ai-relay`, `User=civic-relay`, `Restart=always`, and a single Uvicorn process. `.env.example` must document every configuration name from `Settings`, use non-secret placeholders, and choose sensible development-safe defaults. `README.md` must include uv, Docker, and systemd startup instructions, both curl styles, configuration reference, quota semantics, and a privacy statement.

- [ ] **Step 4: Run deployment assertions**

Run: `uv run pytest tests/test_deployment.py -q`

Expected: PASS.

### Task 8: Final Quality Verification

**Files:**
- Modify: `D:/生产项目/项目/AI工作流/civic-ai-relay/requirements.txt` (regenerate from lock file only if dependencies changed)
- Test: `D:/生产项目/项目/AI工作流/civic-ai-relay/tests/`

- [ ] **Step 1: Run all tests and compile checks**

Run:

```powershell
uv run pytest -q
uv run python -m compileall app.py config.py db.py limiter.py upstream.py
uv export --locked --no-dev --no-emit-project --format requirements-txt -o requirements.txt
```

Expected: all tests pass, every Python module compiles, and the requirements export completes without changing dependency resolution.

- [ ] **Step 2: Perform static privacy and scope checks**

Run:

```powershell
rg -n "prompt|completion|authorization|api_key" app.py db.py limiter.py upstream.py
(Get-Content app.py, config.py, db.py, limiter.py, upstream.py | Measure-Object -Line).Lines
```

Expected: references to sensitive fields are confined to request handling/authentication and never SQLite schema inserts or log message formats; effective application code stays close to the 500-800 line target.

- [ ] **Step 3: Smoke-test health and models locally**

Run:

```powershell
$env:PUBLIC_API_KEY='public-test'; $env:UPSTREAM_API_KEY='upstream-test'; $env:UPSTREAM_BASE_URL='https://provider.example'; $env:MODEL_WHITELIST='public-chat'; $env:TOKEN_LIMIT_5H='10000'; $env:TOKEN_LIMIT_DAILY='10000'; $env:RPM_LIMIT='30'; $env:GLOBAL_CONCURRENCY_LIMIT='1'; $env:MAX_OUTPUT_TOKENS='256'; uv run uvicorn app:app --host 127.0.0.1 --port 8000
```

In another terminal, run `curl http://127.0.0.1:8000/healthz` and an authenticated `GET /v1/models`; expect `{"status":"ok"}` and a list containing only `public-chat`.

- [ ] **Step 4: Verify that no Git state was changed implicitly**

Run: `git rev-parse --is-inside-work-tree`

Expected: same non-repository result as Task 1. Do not create commits or initialize Git without explicit user authorization.
