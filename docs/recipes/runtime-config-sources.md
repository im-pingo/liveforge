# Runtime Configuration Sources

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

LiveForge reads the bootstrap YAML at startup. A single background worker then loads the selected source immediately, polls it periodically, and publishes immutable snapshots. A runtime configuration read is an atomic in-memory load: it never performs file/network I/O, waits for refresh, or takes the manager status lock. Source loads, Config Apply writes, and source close are serialized; Apply waits for its source write to complete before returning 202 with `written_and_refresh_scheduled`, then schedules background parse, module application, and publication.

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
one. Any non-positive `runtime.file.max_bytes`, `runtime.http.max_bytes`,
`runtime.consul.max_bytes`, or `runtime.redis.max_bytes` selects a 4 MiB source
limit. Any non-positive `sip.gateway.max_calls`,
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

Each cached GOP also has independent optional bounds: `stream.gop_cache_max_frames`
(default 300), `stream.gop_cache_max_duration` (default 10s), and
`stream.gop_cache_max_bytes` (default 32 MiB). A value of zero disables that
specific bound without disabling the others. Combined bounds keep the shortest
playable prefix allowed by all enabled bounds. When a bound is reached, the
cache keeps the keyframe and that interleaved prefix and stops growing until
the next video keyframe starts a new GOP. `stream.ring_buffer_size` must be
positive; direct low-level RingBuffer callers are protected with a one-slot
fallback, while configuration loading rejects zero or negative values.

When GOP caching is enabled with a positive `gop_cache_num`, at least one of
the frame or byte bounds must be positive; a duration-only policy is rejected
because equal-DTS frames would otherwise have no hard memory limit. Duration
admission uses the full unordered DTS span between the minimum and maximum
observed timestamps, without reordering the retained media. A directly
constructed, unvalidated stream with no hard bound receives the defensive
300-frame limit.

Hot reload trims every retained GOP under the new policy and recomputes the
active GOP seal from the frames still retained. Tightening a frame, duration,
or byte bound may shorten those playable prefixes and seal the active GOP
immediately. Relaxing a bound permits only the active retained GOP to accept
later interleaved audio/video frames while it remains within every enabled
bound; reaching any enabled bound seals that GOP.
Older retained GOPs stay trimmed, and frames already omitted or trimmed are not
restored. The next video keyframe starts a new complete GOP under the current
policy.

The removed `stream.audio_cache_ms` setting is rejected before typed parsing,
including when YAML mapping aliases or `<<` merge mappings and sequences
introduce it. Validation follows repeated merge aliases with bounded work and
terminates safely on recursive alias graphs.

## Prometheus Stream Detail Cardinality

Server-level aggregate metrics remain available whenever the metrics module is
enabled. Per-stream `stream_key` labels require `metrics.stream_detail: true`.
Without `metrics.stream_detail_allowlist`, `stream_detail_limit` is the maximum
number of distinct keys one Collector can admit over its entire lifetime.
Active keys are admitted in creation order; removing an admitted stream does
not free its slot for a later key. The Collector retains only scalar keys and
resolves active streams during each gather.

This deliberately favors bounded Prometheus cardinality over recency. Use the
management streams API for the current active set, or configure an exact
allowlist for selected Prometheus labels. The allowlist is deduplicated and
sorted once at Collector creation, only exact configured keys are eligible, and
`stream_detail_limit` still caps each gather. Set `stream_detail_limit: 0` to
export no per-stream series; negative configured values are invalid and
rejected. A directly constructed Collector still treats any non-positive limit
as disabled defensively.

## Local File

```yaml
runtime:
  source: file
  poll_interval: 30s
  load_timeout: 10s
  file:
    path: ""
    max_bytes: 4194304
```

An empty `file.path` uses the path passed with `-c`. A changed file is parsed, normalized, validated, and considered only when normalized content changes. Atomic replacement gives a new target private mode `0600`; when replacing an existing file, its permission bits are preserved exactly.

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

One KV prefix is loaded per attempt. Dotted or slash-separated keys map to configuration paths; a complete `config`, `config.yaml`, `config.yml`, or `config.json` value is also accepted. Dotted and slash-separated spellings are canonicalized before materialization. Duplicate paths and scalar/container prefix collisions fail closed with deterministic errors rather than relying on map iteration order. The Consul index is used as source version when available. Consul KV GET and PUT reject every redirect without dispatching a request to the target, so `X-Consul-Token` is sent only to the exact configured endpoint and is never forwarded. A caller-supplied HTTP client's other behavior is preserved without mutating its redirect policy.

## Redis

Hash mode prefers a value-free `HSCAN ... NOVALUES` field scan, then reads field
lengths and values in bounded batches. On Redis versions that reject
`HSCAN NOVALUES` with an unsupported-command or syntax error, it falls back to
`HKEYS` for field names and keeps the same bounded `HSTRLEN`/`HGET` materialization
checks. It never uses `HGETALL`, which could materialize an oversized hash before
the configured limit is known:

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
    max_bytes: 4194304
```

Prefix mode uses `SCAN` and pipelined `GET` operations:

```yaml
runtime:
  source: redis
  redis:
    addr: "127.0.0.1:6379"
    prefix: "liveforge:config:"
    max_bytes: 4194304
```

Redis fields use dotted or slash-separated paths such as `server.log_level` and `limits.max_connections`. Prefer hash mode when an atomic producer can update the hash and version key together. Each source has a 4 MiB default document/materialization limit, configurable with `runtime.redis.max_bytes`; the limit is checked before complete values are read when Redis exposes `STRLEN`/`HSTRLEN`. Config Apply queues the `config.yaml` write and optional `version_key` increment in one `MULTI/EXEC` transaction; transaction or command errors are returned and Apply does not report success. Redis executes transactions atomically with respect to interleaving clients, but a command error inside `EXEC` is not rolled back, so conflicting Redis key types must be corrected before retrying.

For flattened Consul and Redis snapshots, leaf values are interpreted
conservatively. Case-insensitive booleans and `null`, canonical base-10
integers without leading zeroes, and finite decimal or exponent floats become
typed YAML scalars. Leading-zero identifiers, durations, non-finite or
out-of-range numbers, and YAML-looking collection text remain strings. Supply
maps and sequences through a complete `config.yaml`/`config.json` value rather
than encoding YAML syntax in a flattened leaf.

## Config Console And Apply

The Config view reads the complete effective and desired configuration document from
`GET /api/v1/server/config/document`, fetches the complete versioned JSON Schema from
`GET /api/v1/server/config/schema`, and redacts every schema
`x-liveforge-secret` field plus sensitive names including `api_key` and
`tls.key_file`. Valid absolute hierarchical URL scalars are recognized by value
even under unmapped non-URL-shaped keys. They retain safe public scheme, host,
and port identity while removing userinfo, query parameters, and fragments;
every non-root path becomes a stable opaque digest marker in documents/source
details and an opaque marker in failure status. URL-shaped keys retain the
existing TURN/opaque, malformed/hostless fail-closed, and plain-address policy.
Ordinary strings, durations, IDs, and bare host/address values remain unchanged
outside that key policy. The
desired document is retained from the selected source so comments and fields
not represented by the typed runtime struct remain visible in the editable source
pane. The effective applied document is shown in a separate read-only pane;
pending restart paths identify desired values that have not yet changed the
effective configuration. Source details show the selected kind plus redacted
file, HTTP, Consul, and Redis settings. The page can edit the YAML, run a
read-only Validate, and use Apply & Refresh. Validate never expands the server
process environment: references remain literal, exactly one YAML/JSON document
is accepted, and unknown root or nested typed fields are rejected. This strict
boundary is viewer-specific; Apply and trusted runtime source loading remain
permissive for fields not mapped by the typed runtime struct, preserving the
editable desired source, while trusted source loading retains environment
expansion. Viewer
tokens have `config:read`; Apply and Refresh require `config:reload` (operator or
admin).

Sensitive containers are redacted recursively: collection shape and stable
identity fields (`id`, `name`, `username`, `channel_id`, and `device_id`) remain
visible, while every other scalar descendant is replaced. Structured
URL/address values remain under opaque traversal. Address keys retain bare
IPv4/IPv6 values accepted by `net.ParseIP` and validated plain `host:port`
values. Apply restores placeholders
from the current desired source document; reordered structured collections are matched by stable
public identity, and missing, ambiguous, or marked shape-mismatched originals
are rejected instead of guessing.

The editor starts fail-closed while source metadata is loading. Read-only sources
and failed refreshes keep the editor read-only and Apply disabled; the page only
enables writing after a successful response confirms that the selected source
implements `ConfigWriter`.

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
write, so the editor never needs to receive secrets. Secret maps and sequences
retain their original structure. For named tokens,
ICE servers, and notification endpoint collections, placeholder restoration
matches reordered items by a stable non-secret identity instead of by array
index. Inserting or deleting an item cannot inherit another item's old secret;
an ambiguous identity rejects Apply. Leaving a URL path digest marker unchanged
restores only the matching original path; explicitly replacing it with a new
URL location retains that new location while restoring only matching secret
components.
A source that only implements `ConfigSource` is read-only and Apply returns HTTP 409. The response is 202 with `status: written_and_refresh_scheduled` only after the serialized source write succeeds; parsing, module application, and publication still run asynchronously and are visible in the status endpoint. After Apply completes, the Console repopulates the editor with the submitted document only if its monotonic editor revision is unchanged. A newer local edit wins over the stale desired snapshot returned by the scheduled refresh.

After a successful Apply write, the manager keeps a pending desired overlay until a source refresh returns the same normalized content. `GET /api/v1/server/config/document` exposes that submitted desired document immediately, including after a page reload, while `effective_document` remains the last applied snapshot until asynchronous application completes.

## Refresh And Observe

```bash
curl -fsS -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  "$LIVEFORGE_API/api/v1/server/config/refresh"
curl -fsS -H "Authorization: Bearer $OPERATOR_TOKEN" \
  "$LIVEFORGE_API/api/v1/server/config"
```

Refresh success is 202 with `status: scheduled` and means scheduled, not already loaded. Config Apply success is 202 with `status: written_and_refresh_scheduled`. Status success is 200. The refresh route returns 401 for invalid credentials, 403 for a viewer, 429 when rate limited, and 503 when the manager is unavailable, closed, or not started. `SIGHUP` has the same asynchronous enqueue semantics and performs no source I/O in the signal loop.

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
