# Runtime Configuration Sources

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

LiveForge reads the bootstrap YAML at startup. A single background worker then loads the selected source immediately, polls it periodically, and publishes immutable snapshots. A runtime configuration read is an atomic in-memory load: it never performs file/network I/O, waits for refresh, or takes the manager status lock.

## Prerequisites

- Keep the management listener on `127.0.0.1` while validating a source.
- Put source credentials in deployment secret injection or environment variables.
- Ensure every source returns a complete configuration, not a partial patch.
- Export a token with `config:reload` (operator or admin) for manual refresh:

```bash
export LIVEFORGE_API=http://127.0.0.1:8090
export OPERATOR_TOKEN='replace-me'
```

The source, poll interval, load timeout, and source connection settings are bootstrap-controlled and require restart to change.

Configuration accepts explicit default sentinels where the owning module defines
one. Any non-positive `runtime.http.max_bytes` or `runtime.consul.max_bytes`
selects a 4 MiB limit. Any non-positive `sip.gateway.max_calls`,
`cluster.health_check.evict_threshold`, or `api.audit.max_entries` selects 100
calls, 3 failures, or 1000 entries respectively. Explicit empty RTSP, WebRTC,
and GB28181 port ranges select their documented module fallback behavior;
non-empty ranges must contain two ordered positive ports. See the field
descriptions in `docs/config/config.schema.json` for exact range behavior.

## Local File

```yaml
runtime:
  source: file
  poll_interval: 30s
  load_timeout: 10s
  file:
    path: ""
```

An empty `file.path` uses the path passed with `-c`. A changed file is parsed, normalized, validated, and considered only when normalized content changes.

## HTTP Or HTTPS

```yaml
runtime:
  source: https
  poll_interval: 15s
  load_timeout: 10s
  http:
    url: "https://config.example.internal/liveforge.yaml"
    token: "${LIVEFORGE_CONFIG_TOKEN}"
    max_bytes: 4194304
```

The client uses `If-None-Match` and `If-Modified-Since` after a successful response. `304 Not Modified` retains the snapshot. Use authenticated HTTPS and a bounded `max_bytes` value.

## Consul KV

```yaml
runtime:
  source: consul
  poll_interval: 15s
  consul:
    address: "https://consul.example.internal"
    prefix: liveforge
    token: "${CONSUL_HTTP_TOKEN}"
    max_bytes: 4194304
```

One KV prefix is loaded per attempt. Dotted or slash-separated keys map to configuration paths; a complete `config`, `config.yaml`, `config.yml`, or `config.json` value is also accepted. The Consul index is used as source version when available.

## Redis

Hash mode uses one `HGETALL`:

```yaml
runtime:
  source: redis
  redis:
    addr: "redis.example.internal:6379"
    username: liveforge
    password: "${REDIS_PASSWORD}"
    db: 0
    hash: "liveforge:config"
    version_key: "liveforge:config:version"
    tls: true
```

Prefix mode uses `SCAN` and pipelined `GET` operations:

```yaml
runtime:
  source: redis
  redis:
    addr: "127.0.0.1:6379"
    prefix: "liveforge:config:"
```

Redis fields use dotted or slash-separated paths such as `server.log_level` and `limits.max_connections`. Prefer hash mode when an atomic producer can update the hash and version key together.

## Refresh And Observe

```bash
curl -fsS -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  "$LIVEFORGE_API/api/v1/server/config/refresh"
curl -fsS -H "Authorization: Bearer $OPERATOR_TOKEN" \
  "$LIVEFORGE_API/api/v1/server/config"
```

Refresh success is 202 and means scheduled, not already loaded. Status success is 200. The refresh route returns 401 for invalid credentials, 403 for a viewer, 429 when rate limited, and 503 when the manager is unavailable, closed, or not started. `SIGHUP` has the same asynchronous enqueue semantics and performs no source I/O in the signal loop.

Status reports source/version/hash, last attempt/success, consecutive failures, redacted last error, pending restart paths, callback failures, superseded callback count, and accepted/rejected/application-failed counters. Coalescing retains the newest accepted callback and increments `dropped_callbacks` for each superseded pending notification.

Prometheus exports:

```text
liveforge_config_consecutive_failures
liveforge_config_pending_restart
liveforge_config_callback_failures
liveforge_config_callbacks_dropped
liveforge_config_changes_total{result="accepted|rejected|application_failed"}
```

## Application And Failure Semantics

- Empty, timed-out, unavailable, malformed, or invalid input retains the last valid effective snapshot.
- `server.name` is immutable; attempting to change it rejects the complete candidate.
- Hot policy changes are prepared/applied before atomic publication. A module rejection prevents publication, and already prepared reloaders are rolled back when a later reloader fails.
- Restart-required desired values remain visible in `pending_restart`; effective values retain the previously applied values until process restart.
- Exact per-field classes are in `docs/config/config.schema.json` under `x-liveforge-reload`.
- All `stream.simulcast` fields are restart-required and deferred; no runtime layer selection exists.

The deprecated `auth.api.bearer_token` is copied to `api.auth.bearer_token` only when the current path is empty. If both are present, the current path wins. Migrate with a single move:

```text
auth.api.bearer_token -> api.auth.bearer_token
```

## Rollback And Recovery

1. Retain the last known valid source document and its version.
2. Restore that complete document, then request refresh and verify a new `last_success`/active hash.
3. On an immutable rejection, restore `server.name`; a restart does not make an immutable runtime transition acceptable.
4. On application failure, inspect the redacted status/log entry and the responsible module; the prior effective snapshot remains active.
5. For pending restart paths, either revert them in the source or restart in a controlled window after validating the desired document.
6. During backend outage, do not point reads at the backend. Runtime consumers continue using the last atomic snapshot.
