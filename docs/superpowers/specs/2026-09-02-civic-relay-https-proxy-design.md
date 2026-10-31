# Civic Relay HTTPS Reverse-Proxy Design

## Goal

Deploy Civic Relay behind the user's existing container-panel reverse proxy. The panel terminates HTTPS and owns the public domain/certificate; the Relay container has no directly reachable public application port.

## Scope

- Prepare Docker Compose, container startup settings, deployment documentation, and verification checks.
- Use the container panel shown by the user for domain binding, certificate selection, HTTPS enforcement, and forwarding to Relay's host-local port.
- Keep client-to-Relay and Relay-to-upstream traffic encrypted with HTTPS outside the local container boundary.

## Non-goals

- Do not create a second gateway application.
- Do not implement custom encryption, custom TLS, or a bundled Caddy/Nginx container.
- Do not issue or upload a certificate automatically; the panel remains responsible for certificate lifecycle.
- Do not expose the management API on a separate service in this change.

## Architecture

```text
Client
  -> HTTPS :443
Container-panel reverse proxy
  -> HTTP 127.0.0.1:8000
Civic Relay container
  -> HTTPS :443
OpenAI-compatible upstream
```

Docker publishes Relay as `127.0.0.1:8000:8000`, so it is reachable only from the host. The panel reverse proxy binds the selected domain and forwards to host port `8000`. It presents the configured certificate and redirects HTTP to HTTPS.

The container runs as the unprivileged `relay` user. Its configuration remains a read-only mount from `C:\ProgramData\CivicRelay\relay.env`; provider credentials remain encrypted in SQLite and never enter the proxy configuration.

## Client And Administration Access

- API clients use `https://<bound-domain>/v1`.
- `/healthz` remains unauthenticated for proxy health checks.
- `/admin` stays protected by `ADMIN_API_KEY`; it must only be opened over HTTPS.
- Where the container panel supports path or IP policy, restrict `/admin` to administrator IP addresses or a VPN subnet. The public API routes remain protected by per-client API keys and quotas.

## Failure Behavior

- If the certificate is absent, expired, or invalid, the panel must not provide a trusted HTTPS route. Relay remains host-local and is not opened directly as an HTTP fallback.
- If the proxy cannot reach `127.0.0.1:8000`, it returns its own upstream error; the container's port is not remapped publicly as a workaround.
- If upstream HTTPS fails, the existing Relay error handling returns a sanitized upstream error and does not expose provider credentials.

## Verification

1. Compose publishes only the loopback address and does not map `8000` to all interfaces.
2. The container starts and `/healthz` responds at the host-local port.
3. The panel proxy forwards the bound domain to port `8000` and serves a valid certificate.
4. `https://<bound-domain>/healthz` returns 200; plain HTTP redirects to HTTPS at the panel.
5. No client API key, administrator key, or upstream key appears in proxy or application logs.
6. `/admin` requires HTTPS and the administrator credential; client API endpoints continue to reject missing/invalid client keys.
