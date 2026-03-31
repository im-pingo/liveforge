# Cluster Schedule URL — Dynamic Target Resolution

## Context

LiveForge's cluster module currently uses static target lists for forward push and origin pull (`cluster.forward.targets` and `cluster.origin.servers`). The `schedule_url` config fields exist but are unused. This design adds dynamic target resolution via HTTP callback, enabling per-stream scheduling decisions by an external service.

**Use case**: A scheduling service decides which CDN edges receive a given stream, or which origin server to pull from, based on load, geography, or business rules that change at runtime.

## Architecture

### New Component: `Scheduler`

A shared component in `module/cluster/scheduler.go` used by both `ForwardManager` and `OriginManager`.

```
publish event → ForwardManager.onPublish(streamKey)
  → scheduler.Resolve("forward", streamKey)
    → try primary source (schedule_url or static, per priority)
    → if failed/empty, try fallback source
  → targets list → create ForwardTarget per target

subscribe event → OriginManager.onSubscribe(streamKey)
  → scheduler.Resolve("origin", streamKey)
    → try primary source → fallback
  → servers list → create OriginPull
```

### Struct

```go
type Scheduler struct {
    url      string        // schedule_url endpoint
    fallback []string      // static targets/servers from config
    priority string        // "schedule_first" | "static_first"
    timeout  time.Duration // HTTP request timeout
    client   *http.Client
}
```

### Public API

```go
// NewScheduler creates a scheduler. If url is empty, only static fallback is used.
// If fallback is empty, only schedule_url is used.
func NewScheduler(url string, fallback []string, priority string, timeout time.Duration) *Scheduler

// Resolve returns the target list for a given stream.
// action is "forward" or "origin".
// Tries primary source first, falls back to secondary on failure or empty result.
func (s *Scheduler) Resolve(action, streamKey string) ([]string, error)
```

### Resolution Logic

```
if url == "" → return fallback (static-only mode)
if fallback == nil → return httpResolve() (dynamic-only mode)

if priority == "schedule_first":
    result = httpResolve(action, streamKey)
    if err or empty → result = fallback
else: // "static_first"
    result = fallback
    if empty → result = httpResolve(action, streamKey)

if result is empty → return error
```

## HTTP Protocol

### Request

**Method**: POST
**Content-Type**: `application/json`
**Timeout**: configurable (default 3s)

```json
{
  "action": "forward",
  "stream_key": "live/stream1"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `action` | string | `"forward"` or `"origin"` |
| `stream_key` | string | The stream key being published/subscribed |

### Response

**Status**: 200 OK
**Content-Type**: `application/json`

```json
{
  "targets": [
    "rtmp://cdn1:1935/live/stream1",
    "rtmp://cdn2:1935/live/stream1"
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `targets` | []string | RTMP URLs to forward to or pull from |

**Error cases**:
- Non-200 status code → treated as failure, triggers fallback
- Empty `targets` array → treated as "do not forward/pull this stream"
- Network timeout → treated as failure, triggers fallback

## Config Changes

### ForwardConfig

```yaml
cluster:
  forward:
    enabled: true
    targets: ["rtmp://backup/live"]        # static fallback
    schedule_url: "http://sched:8080/api/schedule"
    schedule_priority: schedule_first       # "schedule_first" | "static_first"
    schedule_timeout: 3s                    # HTTP request timeout
    retry_max: 5
    retry_interval: 3s
```

### OriginConfig

```yaml
  origin:
    enabled: true
    servers: ["rtmp://origin1/live"]       # static fallback
    schedule_url: "http://sched:8080/api/schedule"
    schedule_priority: schedule_first
    schedule_timeout: 3s
    idle_timeout: 30s
    retry_max: 3
    retry_delay: 2s
```

New fields in `config.go`:
- `SchedulePriority string` — `yaml:"schedule_priority"` (default: `"schedule_first"`)
- `ScheduleTimeout time.Duration` — `yaml:"schedule_timeout"` (default: `3s`)

## Manager Integration

### ForwardManager

`onPublish` changes:
```
Before: iterate fm.targets (static list)
After:  targets = fm.scheduler.Resolve("forward", streamKey)
        if error → log warning, skip forwarding for this stream
        iterate targets
```

The `active` map already prevents duplicate forwards for the same stream key.

### OriginManager

`onSubscribe` changes:
```
Before: pass om.servers to NewOriginPull
After:  servers = om.scheduler.Resolve("origin", streamKey)
        if error → log warning, skip origin pull
        pass servers to NewOriginPull
```

The `active` map already prevents duplicate origin pulls — multiple subscribers for the same stream only trigger one pull.

## Deduplication

Origin pull dedup is already handled by `OriginManager.active` map. The first `onSubscribe` for a stream key creates the pull; subsequent subscribers find it in the map and skip.

Forward dedup is already handled by `ForwardManager.active` map. The first `onPublish` creates targets; duplicate publish events are ignored.

No additional dedup logic is needed.

## Files to Modify

| File | Change |
|------|--------|
| `module/cluster/scheduler.go` | **New** — Scheduler struct, Resolve method, HTTP client |
| `module/cluster/scheduler_test.go` | **New** — httptest-based tests for all resolution paths |
| `config/config.go` | Add `SchedulePriority` and `ScheduleTimeout` to ForwardConfig and OriginConfig |
| `configs/liveforge.yaml` | Add `schedule_priority` and `schedule_timeout` entries |
| `module/cluster/module.go` | Create Scheduler in Init, pass to managers |
| `module/cluster/forward.go` | Replace static `targets` with `scheduler.Resolve()` call |
| `module/cluster/origin.go` | Replace static `servers` with `scheduler.Resolve()` call |
| `module/cluster/module_test.go` | Update tests for new Scheduler integration |

## Testing

1. **scheduler_test.go**: httptest server simulating:
   - Normal response with targets → verify correct list returned
   - Empty targets array → verify empty result
   - HTTP timeout → verify fallback to static list
   - Non-200 response → verify fallback
   - Priority modes: `schedule_first` and `static_first`
   - Edge cases: no URL (static-only), no fallback (dynamic-only)
2. **module_test.go updates**: verify Module.Init creates Scheduler correctly
3. **Integration**: existing cluster integration tests continue to pass (they use static targets, Scheduler falls through to fallback)
