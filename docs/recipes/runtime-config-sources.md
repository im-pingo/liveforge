# Runtime Configuration Sources

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

LiveForge reads the bootstrap YAML at startup. A single background worker then loads the selected source immediately, polls it periodically, and publishes immutable snapshots. A runtime configuration read is an atomic in-memory load: it never performs file/network I/O, waits for refresh, or takes the manager status lock. Source loads, Config Apply writes, and source close are serialized; Apply waits for its source write to complete before returning 202, then schedules background parse, module application, and publication.

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

## Stream Startup Cache Semantics

The GOP cache is bounded by video keyframes and replays the interleaved video
and audio frames retained between those boundaries. A late subscriber replays
that GOP once, then continues from the atomically captured live cursor. A
pure-audio stream has no GOP startup history: it starts at the live cursor and
receives the next frame without a separate audio startup cache.

The removed `stream.audio_cache_ms` setting is rejected before typed parsing,
including when YAML mapping aliases or `<<` merge mappings and sequences
introduce it. Validation follows repeated merge aliases with bounded work and
terminates safely on recursive alias graphs.

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

The source type and URL scheme must match exactly: `runtime.source: http` requires `http://`, and `runtime.source: https` requires `https://`. Mismatches are rejected before network dispatch. Redirects are disabled for every HTTP source response, including same-scheme and same-host redirects, so bearer credentials are never forwarded through a redirect and HTTPS cannot be downgraded.

The client uses `If-None-Match` and `If-Modified-Since` only from the last parsed, validated, applied, and atomically accepted snapshot. Validators returned with a malformed, invalid, or application-failed document are not committed or sent later. `304 Not Modified` retains the accepted snapshot. `X-Config-Version` is source version metadata and remains separate from the ETag validator. Use authenticated HTTPS and a bounded `max_bytes` value.

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

## Config Console And Apply

The Config view reads the complete effective and desired configuration document from
`GET /api/v1/server/config/document`, fetches the complete versioned JSON Schema from
`GET /api/v1/server/config/schema`, and redacts values whose field names contain
`token`, `password`, `secret`, `credential`, `passphrase`, or `private_key`. The
The desired document is retained from the selected source so comments and fields
not represented by the typed runtime struct remain visible. The page can edit the
YAML, run a read-only Validate, and use Apply & Refresh. Viewer
tokens have `config:read`; Apply and Refresh require `config:reload` (operator or
admin).

Apply writes the complete document through the selected source and then schedules
the normal background refresh. The writer behavior is:

| Source | Apply behavior | Failure/constraint |
| --- | --- | --- |
| `file` | Atomic temp-file write, fsync, rename; existing mode is preserved | The process must write the configured directory |
| `http` / `https` | Authenticated `PUT` to the exact configured URL | URL scheme must match the source; redirects are disabled |
| `consul` | Authenticated `PUT` to `<prefix>/config.yaml` | Consul token and KV write permission are required |
| `redis` | Write `config.yaml` into the configured hash or prefix; increment `version_key` when set | Redis credentials and write permission are required |

The request must contain a complete valid YAML/JSON document. YAML requests carry
raw YAML text. JSON requests must use `{"document":"..."}`; a raw JSON object is
not accepted as the request envelope. `[REDACTED]`
placeholders are replaced with the currently effective sensitive values before a
write, so the editor never needs to receive secrets. A source that only implements
`ConfigSource` is read-only and Apply returns HTTP 409. The response is 202 only
after the serialized source write succeeds; parsing, module application, and
publication still run asynchronously and are visible in the status endpoint.

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
