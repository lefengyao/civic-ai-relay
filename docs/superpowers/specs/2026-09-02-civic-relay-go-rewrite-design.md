# Civic Relay Go Rewrite Design

## Goal

Replace the Python runtime with a lean Go implementation while preserving all
current user-facing behavior. The rewrite intentionally starts with a fresh
database: providers, models, model groups, client keys, quotas, and encrypted
provider credentials are configured again after deployment.

## Scope

The Go service retains:

- OpenAI-compatible `GET /v1/models` and `POST /v1/chat/completions` routes.
- JSON and SSE streaming responses.
- The existing Chinese administrator console and `/admin/api/*` API contract.
- Multiple OpenAI-compatible providers, approved models, model groups, and
  client-key authorization.
- Per-key and global concurrency controls, token and RMB quotas, request
  metadata retention, and model synchronization.
- External configuration, first-start administrator bootstrap, HTTPS reverse
  proxy deployment, Docker Compose, and systemd support.
- The configurable `MEMORY_LIMIT_MB` guard, including `503
  memory_limit_exceeded` responses and stream error events.

The Go service does not import the Python SQLite database or preserve existing
provider/client credentials. The Python source, virtual environment, lockfile,
and Python deployment files are removed only after Go parity is verified.

## Runtime Architecture

The Go module has these ownership boundaries:

```text
cmd/civic-relay       process startup, signals, dependency wiring
internal/config       external relay.env and first-start secrets
internal/store        SQLite schema, transactions, encryption, repositories
internal/service      authorization, quotas, provider routing, relay behavior
internal/httpapi      OpenAI and administrator HTTP handlers, SSE forwarding
internal/memory       cross-platform RSS reading and memory limit guard
web                   embedded administrator HTML, CSS, and JavaScript
```

Handlers validate HTTP input and translate service results to the existing
response format. Service code owns request authorization, reservations,
concurrency, routing, and settlement. Store code is the only layer with SQL or
provider-key encryption access. Prompt content, output content, authorization
headers, and plaintext credentials are never persisted or logged.

The present administrator UI is retained as static embedded assets. Its API
responses remain compatible so its operational workflow does not change.

## Data and Security

The new SQLite database recreates provider, model, group, key, reservation, and
request-metadata tables. Client keys are returned only when created and are
stored as keyed digests. Provider API keys use AES-256-GCM with a random nonce
per value; `RELAY_ENCRYPTION_KEY` is a base64-encoded 32-byte key generated on
first start.

`relay.env` remains outside the source directory. A new installation creates
the stable administrator key, encryption key, and one-time bootstrap file. The
bootstrap file is removed after the administrator stores its credential.

## Request Lifecycle

1. The HTTP layer assigns a request ID and rejects oversized payloads.
2. `/v1` requests compare process RSS to `MEMORY_LIMIT_MB` before body parsing.
3. The service authenticates the client key and checks its model-group access.
4. A SQLite transaction atomically reserves global and key-level quota.
5. Global and per-key semaphores acquire before the upstream call.
6. The HTTP client forwards the request with explicit connection, read, write,
   and pool timeouts.
7. Non-stream responses are settled after upstream usage is parsed. Streams
   pass chunks through, continuously account for output, and settle in `defer`.
8. All reservations and semaphore slots are released on success, client abort,
   timeout, upstream failure, and memory termination.

While over the memory limit, new model requests receive a `503` OpenAI-style
error with code `memory_limit_exceeded` and `Retry-After: 5`. An active stream
receives an SSE error with the same code before its connection closes. The
administrator API remains available so the limit can be changed.

The application-level RSS guard is a soft limit. Docker or systemd/cgroup memory
limits remain the required second defense for a hard operating-system ceiling.

## Deployment and Dependencies

The service uses Go's standard `net/http`, `context`, `crypto`, and `embed`
packages where possible. SQLite uses a pure-Go driver to avoid CGO and system
library dependencies. Docker uses a multi-stage build and a non-root final
image containing the single relay binary plus CA certificates and embedded UI.

The Compose configuration preserves the loopback-only port binding and external
configuration directory. Systemd runs the same binary under the existing
service account. HTTPS continues to terminate at the reverse proxy.

## Verification

Go tests cover configuration creation, encrypted provider keys, SQLite quota
transactions, concurrent reservations, per-key/group authorization, provider
routing, memory limits, streaming settlement, and administrator configuration
reloads. HTTP contract tests verify the OpenAI and administrator routes against
the retained UI behavior. Docker build, Compose rendering, clean first-start,
and loopback-only binding are verified before Python files are removed.

## Rollout

Build the Go runtime in parallel with the Python implementation and run its
contract tests against a fresh database. Once all listed behavior passes,
replace the Dockerfile, Compose command, systemd unit, README commands, and
root runtime source with Go equivalents. Keep a backup of the old SQLite file
outside the deployment directory for audit only; it is not read by Go.
