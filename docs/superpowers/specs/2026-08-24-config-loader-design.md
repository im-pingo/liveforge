# Independent Runtime Configuration Loader Design

**Status:** Approved design, pending implementation plan review

## Goal

Provide LiveForge with an independent configuration loading module that reads configuration in the background on a fixed interval, supports selectable file/HTTP/Consul/Redis sources, and publishes validated immutable snapshots for non-blocking runtime reads. Simulcast remains explicitly deferred.

## Context and constraints

The current process loads one YAML file at startup and handles `SIGHUP` by doing a synchronous file read in the signal loop. `core.Server` already stores a configuration pointer atomically, but no source lifecycle, polling, validation, versioning, restart classification, or source selection exists. The reference project at `/Users/pingo/Documents/e2b-infra-2026` provides useful provider abstraction, typed registration, bulk reads, periodic refresh, and stale-cache fallback; this design keeps those strengths while replacing lock-based readers and mutable maps with immutable snapshot publication.

The loader must satisfy these invariants:

1. All file/network/backend I/O happens in a background goroutine owned by the loader.
2. A configuration consumer performs an atomic pointer load and ordinary field access only. It never reads a file, performs network I/O, waits on a channel, or takes a refresh-held mutex.
3. A malformed or invalid refresh never replaces the last valid snapshot.
4. A source failure is observable through status and callbacks, but the last valid snapshot remains active.
5. The first valid snapshot is required before server module registration and startup.
6. A refresh publishes a new version only when the normalized configuration content changes.
7. Changes are classified as hot-reloadable, restart-required, or immutable. Restart-required values are reported and retained in the pending snapshot but are not partially applied to live modules.
8. Source credentials are read from configuration/environment and are never logged.

## Recommended architecture

### Public package boundary

The new implementation lives under `config/runtime` so the existing `config.Config` model and `config.Load` compatibility helper remain available. The package exposes:

```go
type ConfigSource interface {
    Load(ctx context.Context, previous Version) (Snapshot, error)
    Close() error
}

type Snapshot struct {
    Data         []byte
    Version      string
    LastModified time.Time
}

type Version struct {
    Value string
    Hash  string
}

type Manager struct {
    // internal immutable atomic state
}

func NewManager(opts Options) (*Manager, error)
func (m *Manager) Start(ctx context.Context) error
func (m *Manager) Refresh(ctx context.Context) error
func (m *Manager) Snapshot() *ConfigSnapshot
func (m *Manager) Status() Status
func (m *Manager) Close() error
```

`ConfigSnapshot` owns a deep-copied `config.Config`, source metadata, version/hash, load time, and restart classification. It is published with `atomic.Pointer[ConfigSnapshot]`; the manager never mutates a published value. `Snapshot()` is therefore safe and non-blocking for hot paths.

Optional typed access is provided for components that need a single setting without copying the root config:

```go
type Key[T any] struct { /* immutable key metadata and atomic value */ }
func RegisterKey[T any](m *Manager, name string, class ChangeClass, read func(*config.Config) T) (*Key[T], error)
func (k *Key[T]) Load() T
```

Registration is local-only and cannot perform backend I/O. Keys publish values during the same snapshot commit as the root configuration.

### Refresh lifecycle

`Start` launches the manager worker, requests one bounded initial load, and waits for that result before returning; this is the only startup wait and it is not on a runtime read path. The worker then continues with one polling loop. The poll interval has a minimum enforced value of one second. Each cycle calls `source.Load` with a context deadline, hashes the returned bytes, parses YAML or JSON into defaults plus overrides, normalizes values, validates the complete config, computes a field-level diff against the active snapshot, and atomically publishes only if the hash changed.

Callbacks receive a value object containing old/new versions, changed paths, hot changes, restart-required paths, and the current source status. Callback dispatch is asynchronous and bounded; a slow callback cannot block snapshot publication or runtime reads. Callback errors are recorded in status.

`Refresh` only schedules an explicit asynchronous refresh on a coalescing request channel and returns immediately; `SIGHUP` calls it without doing I/O in the signal loop. `Close` cancels the polling context, waits for the loader goroutine, and closes the source exactly once.

### Source implementations

All sources implement the same read-only `ConfigSource` contract and return a complete configuration document (or a complete key snapshot converted to one) in one call.

- **File:** reads a configured path and returns file modification time as metadata. Environment expansion remains supported by the existing parser.
- **HTTP:** performs `GET`, sends optional bearer token and conditional `If-None-Match`/`If-Modified-Since` headers, accepts `200` and treats `304` as unchanged, enforces response size and timeout limits, and closes response bodies.
- **Consul:** reads one KV prefix through the Consul HTTP API, uses the Consul index as `Version.Value`, decodes base64 values, and maps the configured prefix snapshot to the same YAML/JSON document format. ACL tokens are sent in headers and never included in errors.
- **Redis:** reads one configured hash or key prefix in one pipeline, uses the Redis keyspace version (or configured version key) as metadata, and converts key/value pairs to a document. TLS, username, password, database, and pool limits are source options. The client is closed with the manager.

Source construction is selected by a `config.runtime` section in the existing configuration file. The bootstrap source is always loaded from the local file once so the process can discover remote source settings; after that, the selected source supplies complete snapshots. A file source remains the explicit fallback when no remote source is configured.

### Change classification

The initial classification table is conservative:

- **Hot reload:** server log level and drain policy; connection/stream limits and rate limiting; auth rules and API bearer token; notification endpoints; cluster schedules, retry policies, and health thresholds; recording/DVR policies; stream cache/timeout/slow-consumer/feedback settings; HLS/DASH/LL-HLS segment policies; WebRTC GCC settings.
- **Restart required:** module enablement, listener addresses, listener TLS mode, RTMP/RTSP/SRT/WebRTC/SIP/GB28181 port ranges, TLS certificate/key paths, audio codec enablement, and module topology. A changed enabled module is reported but the module set is not rebuilt in place.
- **Immutable:** server identity and any future process-level resource settings that cannot be switched without losing connections.

Simulcast fields are retained as configuration data but remain `restart_required`/deferred because no runtime consumer is implemented in this cycle.

### Status and observability

`Status()` returns source kind, active version/hash, last successful load, last attempt, consecutive failures, last error (redacted), pending restart-required paths, and callback failure count. The API/metrics integration exposes this state after the manager is wired into `core.Server`; no secret values are returned.

## Error handling and safety

- Source contexts have deadlines and are cancelled on shutdown.
- Empty documents, invalid YAML/JSON, schema violations, invalid durations/ranges, and inconsistent TLS/auth settings are rejected before publication.
- Hashing occurs on a private byte copy; source-owned buffers are never retained for mutation.
- On source failure, the active config and typed keys continue serving. The manager retries on the next interval and reports the failure.
- Remote source credentials are redacted from logs and status. HTTP redirects are limited to the configured host policy.
- Callback queues are bounded; overflow increments a counter and preserves the active snapshot.

## Testing strategy

Unit tests cover each source with `httptest` or in-memory fakes, parser/default/normalize/validation behavior, hash/version de-duplication, stale-cache fallback, restart classification, callback backpressure, cancellation, and close idempotence. A benchmark asserts `Snapshot()` and typed `Key.Load()` perform no allocations and do not block while refresh is in progress. Integration tests exercise bootstrap file plus remote source selection and `SIGHUP` scheduling. Existing `go test ./...`, tagged race tests, tagged build, and `tools/check-agent-docs_test.sh` remain required.

## Delivery boundaries

This design delivers the loader and its integration into startup/runtime config publication. It does not rebuild listener/module topology in place; restart-required changes are surfaced for operators. It does not implement Simulcast, AI media analysis, or unrelated control-plane/productization gaps; those remain separate roadmap items.
