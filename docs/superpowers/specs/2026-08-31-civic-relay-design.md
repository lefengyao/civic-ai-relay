# Civic Relay Design

## Purpose

Civic Relay is a small, self-hosted, public-benefit relay for an OpenAI-compatible upstream API. Clients receive one public API key and an OpenAI-compatible surface. The service keeps the upstream key private, forwards requests without retaining prompts or completions, and enforces persistent request, token, and concurrency limits.

The deployed project directory will be named `civic-ai-relay`. Dependency management is uv-native: `pyproject.toml` and `uv.lock` are authoritative, while `requirements.txt` is an exported compatibility artifact for conventional deployment.

## Scope

The first release exposes:

- `GET /healthz` without authentication.
- `GET /v1/models` with Bearer authentication and only configured public models.
- `POST /v1/chat/completions` with OpenAI-compatible JSON and SSE responses.

It deliberately excludes an admin interface, user accounts, multiple public keys, an ORM, external queues, Redis, and multi-worker operation.

## Structure

```text
civic-ai-relay/
├── app.py                 # FastAPI application, routes, request lifecycle
├── config.py              # Validated environment configuration
├── db.py                  # SQLite schema, atomic reservations, settlements
├── limiter.py             # In-memory global concurrency guard and token estimates
├── upstream.py            # Reusable httpx client and OpenAI-compatible forwarding
├── tests/
│   └── test_acceptance.py # Mocked upstream acceptance coverage
├── data/                  # Persistent SQLite volume, ignored by Git
├── pyproject.toml
├── uv.lock
├── requirements.txt
├── .env.example
├── Dockerfile
├── docker-compose.yml
├── ai-relay.service       # required deployment name
├── civic-relay.service    # branded alias
└── README.md
```

There are no artificial service or repository layers. Each module has one dependency direction: `app.py` coordinates `config`, `db`, `limiter`, and `upstream`; those modules do not import the FastAPI application.

## Configuration and Secrets

`config.py` loads all environment variables once at startup and fails closed when required keys, URLs, model lists, or positive limits are invalid. `UPSTREAM_BASE_URL` is normalized to remove a trailing `/v1`, preventing doubled path segments. `MODEL_MAP_JSON` is parsed as a string-to-string mapping.

The public and upstream keys are never logged, persisted, or returned. Public-key authentication uses `hmac.compare_digest`. `/docs`, `/redoc`, and `/openapi.json` are disabled by default and can be enabled only with `DOCS_ENABLED=true`.

## Data Model and Atomic Accounting

SQLite stores only aggregate request metadata in one `requests` table:

```sql
CREATE TABLE requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL,
    started_at_utc TEXT NOT NULL,
    finished_at_utc TEXT,
    billing_date_bj TEXT NOT NULL,
    model TEXT NOT NULL,
    stream INTEGER NOT NULL,
    status TEXT NOT NULL,
    reserved_tokens INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER,
    output_tokens INTEGER,
    charged_tokens INTEGER NOT NULL DEFAULT 0,
    http_status INTEGER,
    duration_ms INTEGER
);
```

Indexes cover `started_at_utc`, `billing_date_bj`, and `request_id`. The database uses WAL mode, `synchronous=NORMAL`, and a five-second busy timeout.

Before calling the upstream, a `BEGIN IMMEDIATE` transaction atomically:

1. Counts requests begun in the prior 60 seconds for RPM.
2. Totals `charged_tokens + reserved_tokens` over the rolling five-hour window.
3. Performs the equivalent total for the current `Asia/Shanghai` billing date.
4. Rejects any exceeded constraint or inserts one `reserved` record.

This serializes competing reservations so two requests cannot spend the same remaining token capacity. The reservation is conservative: estimated input tokens plus the validated effective output maximum. Settlement clears `reserved_tokens` and records the final `charged_tokens`, so every query can use the same `charged_tokens + reserved_tokens` expression. An unfinished reservation retains its full reserve until final settlement.

## Request Lifecycle

Every chat request follows this order:

1. Generate or validate a bounded `X-Request-ID`; authenticate the public key.
2. Enforce body size, parse JSON, validate the public model and output-token fields, and add the configured default when no output maximum is supplied.
3. Estimate input tokens from serialised message content using `max(1, ceil(characters / 2))`.
4. Atomically check RPM and reserve both token quotas in SQLite.
5. Attempt a non-waiting in-memory concurrency acquisition. A full service cancels its fresh reservation with zero charge and returns `429` instead of queuing, because no upstream work has begun.
6. Map the client model to the upstream model, inject only the upstream authorization header, and forward the request.
7. Settle the request and release the concurrency slot in all completion, error, timeout, and disconnect paths.

The response always includes `X-Request-ID`. Errors use OpenAI-shaped `error` objects. Expected invalid client input maps to 400/401/413/429. Upstream authentication failures map to a non-sensitive 502 response; upstream timeouts map to 504; upstream 5xx maps to 502.

## Streaming and Settlement

For `stream=true`, Civic Relay requests `stream_options.include_usage=true` while preserving the client payload otherwise. It forwards upstream SSE bytes as they arrive, without buffering the full response. While forwarding it extracts only `choices[].delta.content` character counts and final `usage` data.

The concurrency lease is held from before opening the upstream stream until the generator finishes or closes. On every exit it closes the upstream response, settles the SQLite reservation, and releases the lease. It checks `request.is_disconnected()` before forwarding chunks; a disconnect stops upstream reading and settles the observed output conservatively.

Settlement prefers upstream `total_tokens`, then `prompt_tokens + completion_tokens`. If usage is absent, the service charges a conservative input estimate plus `max(1, ceil(observed_output_characters / 2))`. If no output can be observed after an abnormal stream failure, it charges the entire reservation. Successful settlement may release unused reservation capacity; an upstream value greater than the reservation is recorded as the actual charge.

Non-streaming responses use the same hierarchy, falling back to a conservative estimate from the response's content when upstream usage is missing.

## Observability, Retention, and Deployment

Structured logs contain only request ID, public model name, stream flag, final status, charged tokens, duration, and HTTP status. They exclude credentials and all chat content. Startup and a low-frequency completion path prune request records older than `RETENTION_DAYS`; data within the last five hours and the current billing day remains available for quota checks.

The Docker image uses Python slim, an unprivileged user, a persistent `/app/data` volume, and one Uvicorn worker. Docker Compose reads `.env` and publishes port 8000. The systemd unit runs one worker from an application directory and reads `/etc/civic-relay.env`; keys never appear in its body.

## Test Strategy

Acceptance tests use a mocked `httpx` upstream. They cover model discovery, authentication, validation of both output-token field names, RPM persistence, rolling five-hour quota, Beijing day rollover, non-blocking concurrency rejection, full-lifetime streaming concurrency, usage-free stream estimates, upstream error mapping, sensitive-key non-disclosure, and restart-compatible SQLite accounting.

The tests use a temporary database and controllable clock so the five-hour and local-date boundaries are deterministic. They do not invoke a real upstream or require credentials.

## Explicit Decisions

- Python 3.11 is the default runtime, while metadata requires Python 3.10 or newer.
- SQLite is the durable source for RPM and token accounting; the in-memory lock owns only instance-local active concurrency.
- A concurrency rejection cancels its already-created reservation with zero charge, so a rejected request cannot leave capacity falsely unavailable or a permanent `reserved` record.
- The project remains single-process by design; cross-process global concurrency is expressly out of scope for this release.
