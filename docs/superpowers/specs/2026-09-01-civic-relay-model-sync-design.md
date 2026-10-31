# Upstream Model Sync Design

## Goal

Keep the public model whitelist synchronized with the configured upstream
provider without making model discovery a public, unbounded proxy operation.

## Behavior

- `MODEL_AUTO_SYNC` defaults to `true` and enables background synchronization.
- `MODEL_SYNC_INTERVAL` defaults to 30 minutes.
- The application starts normally with its saved `MODEL_WHITELIST`, then starts
  a background refresh attempt. An upstream failure is logged and leaves the
  last successful list in place.
- A successful refresh accepts only unique non-empty string `id` values from
  the upstream `GET /v1/models` response. It atomically writes the resulting
  whitelist and hot-reloads it for new requests.
- Administrators can trigger the same refresh through a protected endpoint and
  the management page.

## Safety

- Public clients continue to see only the locally persisted whitelist through
  `GET /v1/models`.
- The upstream bearer credential is used only by the server-side HTTP client.
- Invalid upstream responses and network failures expose only a generic error.

## Tests

- Verify that upstream model parsing rejects invalid response structures.
- Verify that an authenticated administrator can synchronize models, persist
  them, and expose them through the public endpoint.
- Verify that the background loop leaves the existing whitelist intact when
  a refresh fails.
