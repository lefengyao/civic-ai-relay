# Civic Relay Admin Console Design

**Date:** 2026-09-01

## Goal

Add an internal-network administration console to Civic Relay. The console lets
an operator observe the relay in near real time, inspect privacy-preserving
request metadata, and update its managed environment configuration without
restarting the process for runtime-safe settings.

The console is an operational surface, not a customer-facing application. It
must preserve the relay's existing privacy guarantees: it never stores or
returns prompts, completions, authorization headers, public keys, upstream
keys, or administrator keys.

## Scope

The feature adds:

- A built-in management page at `/admin` for an internal network.
- A distinct `ADMIN_API_KEY` credential for admin APIs.
- Typed configuration read, validation, atomic persistence, and runtime reload.
- Dashboard, quota, and request-history monitoring APIs.
- Tests and deployment documentation for the console.

The feature does not add public internet exposure, multi-user accounts, audit
logs containing operator identities, prompt/completion storage, alert delivery,
or direct upstream health probes.

## User Interface

`/admin` serves a static, responsive single-page console. It has no embedded
configuration values or metrics. On first use, the operator enters the
administrator key; the browser keeps it only in the currently open page memory
and sends it as `X-Admin-Key` to admin APIs. It must not use local storage,
session storage, cookies, query parameters, or URL fragments for the key.

The main navigation contains four views:

1. **Overview** is the default dashboard. It shows database-backed service
   health, refresh age, active connections, current RPM, 5-hour quota, Beijing
   calendar-day quota, the previous hour's request success/failure rate, a
   request trend chart, and a small latest-request list.
2. **Request history** shows up to 50 recent request metadata records. Each
   record contains the request ID, UTC timestamp, model, stream flag, outcome,
   HTTP status, duration, and charged tokens. It has no request or response
   text.
3. **Quota and limits** presents all rolling quota and concurrency measurements
   alongside their configured limits and explanatory time windows.
4. **Runtime configuration** groups editable settings into access/upstream,
   models, limits, transport timeouts, retention/logging, and deployment
   sections. It has a non-mutating Validate action, a Save and hot reload
   action, clear validation messages, a change summary, and a last-applied
   timestamp.

The browser polls the overview-related APIs every two seconds while the page is
visible. It pauses polling when the page is hidden and triggers an immediate
refresh when it becomes visible again. A failed poll retains the last valid
snapshot and visibly marks its timestamp stale.

The design intentionally uses a compact, monitor-first dashboard rather than a
separate JavaScript application. It keeps deployment to the current Python
service and avoids a separate frontend build chain.

## Authentication and Network Boundary

The administrator creates a separate `ADMIN_API_KEY`; it must be non-empty and
must not equal `PUBLIC_API_KEY`. All `/admin/api/*` routes require a matching
`X-Admin-Key` and use constant-time comparison. Unauthorized API calls return a
generic 401 response without revealing whether an admin key exists.

`/admin` itself can serve its static sign-in shell without a key; no protected
data is rendered until the subsequent authenticated API requests succeed. The
deployment documentation instructs operators to make this path reachable only
from an internal network or VPN and to terminate TLS at the internal reverse
proxy where applicable. The same process continues to expose the public relay
API only according to the operator's network rules.

An `ADMIN_API_KEY` rotation is a runtime configuration change. After a
successful save, all later admin API requests require the new key. The console
clears its in-memory key and returns to its sign-in screen. It never displays
the replacement key after submission.

## Configuration Source and Persistence

`CIVIC_RELAY_CONFIG_FILE` is a process-level deployment setting and is not
editable through the console. It resolves the managed configuration file:

- Local `uv run` defaults to `.env` in the project root.
- systemd sets it to `/etc/civic-relay.env`.
- Docker Compose bind-mounts the host `.env` at `/app/.env` and sets it to that
  path.

The service reads managed relay settings from this file at startup. The existing
environment-backed path remains available for test and compatibility scenarios
where no managed file is configured. The configuration implementation is
refactored so both sources use the same typed mapping parser and validation
rules. The managed file is the source of truth once configured; inherited
process environment values do not override later console edits.

`GET /admin/api/config` returns every non-secret setting and a boolean
`is_configured` flag for each secret. It returns neither a secret value nor the
configuration file's raw text. Secret fields in save requests are optional:

- Omitted or blank `PUBLIC_API_KEY`, `UPSTREAM_API_KEY`, and `ADMIN_API_KEY`
  retain their present values.
- A non-blank submitted value replaces the corresponding secret.

Validation creates a candidate complete configuration before writing anything.
It uses the same URL, integer, model map, boolean, and timeout validation as
startup. The console writes a valid candidate with a temporary file in the
target directory, flushes it, then replaces the target atomically. A failed
parse, validation, file write, or runtime preparation leaves both the old file
and active runtime configuration intact. Validation errors identify setting
names but never echo submitted values.

## Runtime Reload Semantics

The application owns a `RuntimeConfig` manager that supplies an immutable
configuration snapshot to each request. A reload creates and validates a new
snapshot before one atomic pointer swap. Requests beginning after the swap use
the new snapshot. Requests already admitted, including streams, retain the
snapshot and upstream client that they started with until they settle.

If the upstream URL, key, or timeout settings change, the manager constructs a
new upstream client before the swap. It reference-counts old clients and closes
one only after all requests holding that generation have completed. This avoids
interrupting active SSE streams.

`ConcurrencyGuard` becomes reconfigurable while retaining its active count.
When a limit is lowered below the current active count, no active request is
cancelled; new work is rejected until the count drops below the new limit.

The following settings apply immediately to newly admitted requests:

- `PUBLIC_API_KEY`, `ADMIN_API_KEY`, `UPSTREAM_BASE_URL`, and
  `UPSTREAM_API_KEY`
- `MODEL_WHITELIST`, `MODEL_MAP_JSON`
- `TOKEN_LIMIT_5H`, `TOKEN_LIMIT_DAILY`, `RPM_LIMIT`,
  `GLOBAL_CONCURRENCY_LIMIT`
- `MAX_OUTPUT_TOKENS`, `MAX_BODY_MB`, `MAX_STREAM_DURATION`,
  `RETENTION_DAYS`, `LOG_LEVEL`
- `UPSTREAM_CONNECT_TIMEOUT`, `UPSTREAM_READ_TIMEOUT`,
  `UPSTREAM_WRITE_TIMEOUT`, `UPSTREAM_POOL_TIMEOUT`

`HOST`, `PORT`, `DB_PATH`, and `DOCS_ENABLED` can be saved in the deployment
section but cannot be safely changed by an already-bound Uvicorn process.
The save response lists these values in `pending_restart_fields`; the console
labels them **Saved; restart required** and does not claim they were hot
applied. `CIVIC_RELAY_CONFIG_FILE` is intentionally absent from the editable
schema. A single Uvicorn worker remains required for process-local concurrency
to be global within the service.

## Administration API

All APIs below require `X-Admin-Key`, except the static `/admin` page.

| Endpoint | Purpose |
| --- | --- |
| `GET /admin/api/overview` | One current monitoring snapshot for dashboard and quota views. |
| `GET /admin/api/requests?limit=50` | Recent redacted request metadata; `limit` is constrained to 1-50. |
| `GET /admin/api/config` | Typed, redacted managed configuration and reload status. |
| `POST /admin/api/config/validate` | Validate a proposed partial edit without persistence or reload. |
| `PUT /admin/api/config` | Validate, atomically persist, then hot apply runtime-safe edits. |

The overview response includes server UTC time, process uptime, ledger health,
active and configured concurrency, current 60-second request count and limit,
5-hour consumed/reserved tokens and limit, daily consumed/reserved tokens and
limit, recent request totals, completed/failed/rejected counts, error rate, and
time-bucketed request counts for the last hour. It also returns a small redacted
recent list suitable for the dashboard. It does not make an extra upstream
request: existing ledger outcomes are the relay's upstream health signal.

The ledger receives focused aggregate and recent-row query methods. Statistics
take their snapshot from a consistent database read; guard activity is read from
the process-local guard. Existing `requests` rows remain the only stored
operational record.

## Error Handling and Privacy

Admin failures use JSON error envelopes with a request ID but never exception
traces, raw configuration text, headers, prompts, completions, or secret values.
The UI surfaces these errors as field-specific validation messages or a general
operation message. It retains the old dashboard snapshot on polling errors.

The service logs only operation type, request ID, success/failure, and changed
non-secret setting names. It must never log request payloads, response payloads,
any `X-Admin-Key`, or submitted secret values. Configuration file permissions
remain the responsibility of the service account and deployment environment;
documentation requires owner-only readability for the systemd file.

## Deployment Changes

- Add `ADMIN_API_KEY` to `.env.example` with a non-production placeholder.
- Add `CIVIC_RELAY_CONFIG_FILE` deployment guidance without exposing it in the
  editable configuration page.
- Update the systemd unit to declare the `/etc/civic-relay.env` managed path.
- Update Docker Compose to mount `.env` read/write at the path used by the
  process so console changes persist on the host.
- Document internal-network restrictions, key rotation behavior, the
  restart-required settings, and an operator startup command using `uv`.
- Add `.superpowers/` to `.gitignore` when this project is placed in a Git
  repository, so local brainstorming artifacts are not committed.

## Verification Plan

The implementation adds focused tests for:

- Every admin API returning 401 without a valid independent admin key.
- Admin key equality with the public key being rejected at startup and reload.
- Config reads redacting all secrets and request history omitting bodies.
- Validation errors and write failures preserving the old file and active
  snapshot.
- Valid runtime changes affecting new authentication, models, quotas, upstream
  routing/timeouts, and logging without disrupting an existing simulated stream.
- Admin-key rotation invalidating the old key immediately.
- Lowered concurrency not cancelling active work while rejecting new work.
- Accurate rolling RPM, quota, error-rate, recent request, and active-connection
  statistics.
- Static-setting updates being persisted and reported as restart-required.
- Existing API relay tests, compilation, `uv lock --check`, and the new admin
  route smoke tests continuing to pass.

## Acceptance Criteria

An internal operator can sign in with an independent administrator key, see a
dashboard that refreshes no slower than every two seconds, inspect the current
limits and up to 50 privacy-preserving request records, validate and save all
operator-visible configuration, rotate all supported keys without key readback,
and observe runtime-safe edits take effect on the next request. Any deferred
deployment setting is plainly marked as requiring a process restart. No public
relay behavior or privacy guarantee regresses.
