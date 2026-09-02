# Civic Relay

Civic Relay is a small, self-hosted OpenAI-compatible API relay. It supports multiple client keys, model-group authorization, per-key concurrency limits, one-time Token and RMB limits, and multiple OpenAI-compatible upstream providers.

## Features

- `GET /v1/models` returns only models authorized for the caller's client key.
- Each client key can belong to multiple model groups and has independent concurrency, Token, and RMB limits.
- Provider API keys are encrypted at rest; client key plaintext is returned only once on creation.
- Providers can refresh their `/v1/models` catalogue; imported models stay disabled and unpriced until an administrator approves them.
- `POST /v1/chat/completions` supports JSON and streaming SSE responses.
- Atomic SQLite accounting for RPM, rolling five-hour tokens, and Beijing-calendar-day tokens.
- Conservative token reservation before upstream work, including streams without usage data.
- Single-process global concurrency guard and bounded request/stream lifetimes.
- No prompt, completion, authorization header, or API key retention.

## Quick Start With uv

```powershell
# The application creates external configuration and a one-time bootstrap
# administrator key on first start. Keep this directory outside the repository.
New-Item -ItemType Directory -Force C:\ProgramData\CivicRelay | Out-Null
$env:CIVIC_RELAY_CONFIG_FILE = "C:\ProgramData\CivicRelay\relay.env"
uv sync --dev
.\.venv\Scripts\uvicorn.exe app:app --host 127.0.0.1 --port 8000
```

On first start, read `C:\ProgramData\CivicRelay\bootstrap-admin-key.txt` once and then protect or remove it after storing the administrator credential safely. Add a provider from the administrator console; a public client key is created there. For an existing project `.env`, copy it to the external path manually, retain the existing values, and add `RELAY_ENCRYPTION_KEY` if it is missing. The service uses one Uvicorn process; do not use multiple workers until key concurrency and quotas are moved to shared storage.

## Docker Compose

```powershell
# Keep application secrets outside the repository. On Windows:
$env:CIVIC_RELAY_CONFIG_DIR = "C:\ProgramData\CivicRelay"
New-Item -ItemType Directory -Force $env:CIVIC_RELAY_CONFIG_DIR | Out-Null
# On Linux, use an external directory such as /etc/civic-relay:
# sudo install -d -o 10001 -g 10001 -m 700 /etc/civic-relay
docker compose up -d --build
curl http://127.0.0.1:8000/healthz
```

Compose requires `CIVIC_RELAY_CONFIG_DIR` and mounts that directory at `/app/config`; this avoids a host-specific Windows path and works on a VPS. The first container start creates `relay.env` and `bootstrap-admin-key.txt` there. Apply restrictive host permissions to the directory and read the bootstrap key once.

SQLite is persisted in `./data`.

## Container Panel HTTPS 反向代理

The Relay container listens on `127.0.0.1:8000` only. It is not a public API endpoint; use the container panel's reverse-proxy feature to terminate HTTPS.

1. Start the container with `docker-compose up -d --build`.
2. In the container panel, create a reverse proxy for the assigned domain.
3. Set the proxy target to `127.0.0.1:8000`.
4. Select or upload the certificate for that exact domain and enable HTTP-to-HTTPS redirect.
5. Do not publish port `8000` to the public network and do not select “不部署证书” for production access.
6. Verify `https://<bound-domain>/healthz`, then configure clients with `https://<bound-domain>/v1`.
7. Restrict `/admin` to an administrator IP allowlist or VPN subnet when the panel supports path-level policy.

The proxy-to-container hop is host-local HTTP by design. Client-to-proxy and Relay-to-upstream traffic use HTTPS. The panel owns certificate lifecycle; do not bypass a missing, expired, or invalid certificate by exposing port `8000` publicly.

## Example Calls

Non-streaming:

```bash
curl https://your-domain/v1/chat/completions \
  -H "Authorization: Bearer public_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"Hello"}],"stream":false}'
```

Streaming:

```bash
curl https://your-domain/v1/chat/completions \
  -H "Authorization: Bearer public_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

## Configuration

`ADMIN_API_KEY` is a separate internal-management credential. `UPSTREAM_BASE_URL` and `UPSTREAM_API_KEY` provide a migration-compatible default provider; they are imported into the tenant database on start. `RELAY_ENCRYPTION_KEY` encrypts provider API keys in SQLite and must be retained for the life of the database. `MODEL_AUTO_SYNC` refreshes the legacy provider's catalogue; each additional provider can also be synchronized from the management console.

`MAX_OUTPUT_TOKENS` is a hard per-request ceiling. `MEMORY_LIMIT_MB` is a process RSS soft guard (default `200` MB): new model requests return `503 memory_limit_exceeded` while over the limit, and an active stream receives an SSE error before it is terminated. The administrator console remains available to raise the limit or reduce load. This is an application-level guard, not an operating-system hard cap; use a Docker or systemd/cgroup memory limit as a second defense when a hard ceiling is required. `RPM_LIMIT` limits request starts during the last 60 seconds. `TOKEN_LIMIT_5H` is a rolling five-hour reservation/charge ceiling. `TOKEN_LIMIT_DAILY` uses `Asia/Shanghai` calendar dates. `GLOBAL_CONCURRENCY_LIMIT` rejects immediately when all active stream or non-stream slots are occupied. `MAX_BODY_MB` and `MAX_STREAM_DURATION` bound abuse. `RETENTION_DAYS` controls ledger cleanup.

Production API documentation is disabled by default. Set `DOCS_ENABLED=true` only in a protected development environment.

## Internal Administration

Open `http://<internal-host>:8000/admin` only from the internal network or a VPN. The page asks for `ADMIN_API_KEY` and keeps it only in the current browser page; it is never returned by the server, stored in the browser, database, or logs. Put the service behind an internal TLS reverse proxy when administrators cross an untrusted network segment.

Create a provider, approve and price its models, create a model group, then create a client key and assign its groups. The client uses that one-time key as `Authorization: Bearer <client-key>`, points its SDK base URL to `http://<relay-host>:8000/v1`, and chooses one of the public model aliases. A Token or amount limit is an all-time total; either limit reaching its ceiling automatically disables that key.

The dashboard refreshes every two seconds while the page is visible. It reports service/database health, active concurrency, current RPM, rolling 5-hour and Beijing-calendar-day token usage, recent outcome rates, and up to 50 request metadata records. It never stores or displays prompts, completions, authorization headers, or API keys.

The configuration page validates before saving and writes the managed configuration file atomically. Blank key inputs preserve the current key; a non-blank value rotates it without readback. The model section includes an authenticated manual sync action in addition to automatic synchronization. `PUBLIC_API_KEY`, `ADMIN_API_KEY`, upstream connection settings, model settings, limits, timeouts, retention, and log level apply to new requests immediately. `HOST`, `PORT`, `DB_PATH`, and `DOCS_ENABLED` are saved but require a service restart. Rotating `ADMIN_API_KEY` signs the current page out; sign in again with the replacement key.

On Windows, configuration is stored outside the repository at `C:\ProgramData\CivicRelay\relay.env`; grant access only to the current user, Administrators, and SYSTEM. Docker Compose mounts an operator-selected external directory at `/app/config` so first startup can create `relay.env`; apply restrictive host permissions and ensure the container's `relay` user can write it. The image uses UID/GID `10001`, so on Linux create the mounted directory with `sudo install -d -o 10001 -g 10001 -m 700 /etc/civic-relay`. Linux systemd uses `/etc/civic-relay/relay.env`; create that directory before first start, protect it with `chmod 700`, make the file `chmod 600`, and grant ownership to the service account.

## systemd

Install the project at `/opt/civic-ai-relay`, create a `civic-relay` system user, then create a private configuration directory. The application creates `/etc/civic-relay/relay.env` and its one-time bootstrap key when the service first starts:

```bash
sudo install -d -o civic-relay -g civic-relay -m 700 /etc/civic-relay
uv sync --frozen
sudo cp ai-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now civic-relay
sudo systemctl status civic-relay
```

Read `/etc/civic-relay/bootstrap-admin-key.txt` once as an administrator, then remove it after storing the credential safely. The unit intentionally starts one worker so the in-memory concurrency limit remains global for the process.

## Tests

```bash
uv run pytest -q
uv run python -m compileall app.py admin_api.py config.py config_store.py db.py limiter.py provider_registry.py runtime.py tenant_store.py upstream.py
```

Tests use temporary SQLite databases and mocked upstream responses; no real API key is needed.
