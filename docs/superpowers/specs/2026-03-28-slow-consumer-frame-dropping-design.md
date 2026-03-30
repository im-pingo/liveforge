# Slow Consumer Frame Dropping Design

## Problem

When a subscriber's consumption speed falls behind the publisher's write speed, the ring buffer eventually overwrites unread frames. The existing `SkipTracker` only disconnects after repeated overwrites — it does not prevent frame loss. A proactive frame dropping strategy is needed to degrade gracefully before overwrites occur.

## Approach

A protocol-agnostic `SlowConsumerFilter` in `core/` wraps `RingReader` and decides per-frame whether to deliver or drop based on two signals:

1. **Lag ratio** — how far the reader cursor trails the writer, as a fraction of ring buffer capacity
2. **EWMA send time** — exponentially weighted moving average of per-frame send duration, smoothed to filter jitter

## Configuration

All parameters are configurable under `stream.slow_consumer`:

```yaml
stream:
  slow_consumer:
    enabled: true
    lag_warn_ratio: 0.5       # monitoring threshold (no action, for logging)
    lag_drop_ratio: 0.75      # start dropping non-keyframes
    lag_critical_ratio: 0.9   # skip to next keyframe
    lag_recover_ratio: 0.5    # stop dropping when lag falls below this (hysteresis)
    ewma_alpha: 0.3           # EWMA smoothing factor (0-1, higher = more sensitive)
    send_time_ratio: 2.0      # send time > N * frame interval = slow
```

Setting `enabled: false` disables the filter entirely; subscribers read raw frames as before.

## State Machine

```
                   lag < recover_ratio
    +-------------------------------------+
    |                                     v
 +------+   lag > drop_ratio    +--------------+   lag > critical_ratio   +----------+
 |Normal| --AND ewma confirms-->|DropNonKey    | -----------------------> |SkipToKey |
 +------+                       +--------------+                          +----------+
    ^                                                                          |
    +--------------------------------------------------------------------------+
                          lag < recover_ratio
```

- **Normal**: all frames delivered
- **DropNonKey**: only keyframes, sequence headers, and audio delivered; interframes dropped
- **SkipToKey**: all frames dropped until the next keyframe arrives, then transition to DropNonKey

### Hysteresis

Entry threshold (75%) differs from recovery threshold (50%), creating a 25% dead zone that prevents rapid oscillation at the boundary.

### EWMA Anti-Jitter

```
ewma = alpha * current_send_time + (1 - alpha) * ewma
```

With alpha=0.3, a single spike (e.g., 200ms vs normal 33ms) yields:
`0.3 * 200 + 0.7 * 33 = 83.1ms` — below the 2x threshold (66ms). Sustained slowness over 3-4 frames is required to trigger dropping.

## Components

### `RingReader.Lag() float64` (pkg/util/ringbuffer.go)

Returns `(writeCursor - readCursor) / bufferSize` as a ratio [0.0, 1.0].

### `SlowConsumerConfig` (config/config.go)

New struct under `StreamConfig`:

```go
type SlowConsumerConfig struct {
    Enabled          bool    `yaml:"enabled"`
    LagWarnRatio     float64 `yaml:"lag_warn_ratio"`
    LagDropRatio     float64 `yaml:"lag_drop_ratio"`
    LagCriticalRatio float64 `yaml:"lag_critical_ratio"`
    LagRecoverRatio  float64 `yaml:"lag_recover_ratio"`
    EWMAAlpha        float64 `yaml:"ewma_alpha"`
    SendTimeRatio    float64 `yaml:"send_time_ratio"`
}
```

### `SlowConsumerFilter` (core/slow_consumer.go)

```go
type ConsumerState uint8
const (
    ConsumerStateNormal     ConsumerState = iota
    ConsumerStateDropNonKey
    ConsumerStateSkipToKey
)

type SlowConsumerFilter struct {
    reader   *util.RingReader[*avframe.AVFrame]
    rb       *util.RingBuffer[*avframe.AVFrame]
    config   config.SlowConsumerConfig
    state    ConsumerState
    ewmaSend float64  // EWMA of send duration in ms
    lastDTS  int64    // for frame interval calculation
    dropped  int64    // total dropped frame count
}
```

Public API:
- `NewSlowConsumerFilter(reader, rb, cfg) *SlowConsumerFilter`
- `NextFrame() (*avframe.AVFrame, bool)` — reads from ring, applies drop policy
- `ReportSendTime(d time.Duration)` — updates EWMA after each send
- `State() ConsumerState` — current state for monitoring
- `Dropped() int64` — cumulative dropped frames

### Subscriber Integration

Each subscriber wraps its RingReader in a SlowConsumerFilter:

```go
// Before
reader := stream.RingBuffer().NewReader()
frame, ok := reader.Read()

// After
filter := core.NewSlowConsumerFilter(reader, stream.RingBuffer(), cfg.SlowConsumer)
frame, ok := filter.NextFrame()
sendFrame(frame)
filter.ReportSendTime(elapsed)
```

### Relationship to SkipTracker

Layered defense:
1. **SlowConsumerFilter** (new) — proactive: drops frames before ring buffer overwrite
2. **SkipTracker** (existing) — reactive: disconnects if overwrite still occurs despite dropping

## Affected Files

| File | Change |
|------|--------|
| `config/config.go` | Add `SlowConsumerConfig` struct, add field to `StreamConfig` |
| `config/loader.go` | Add defaults |
| `configs/liveforge.yaml` | Add `slow_consumer` section |
| `core/slow_consumer.go` | New file: `SlowConsumerFilter` implementation |
| `core/slow_consumer_test.go` | New file: tests |
| `pkg/util/ringbuffer.go` | Add `Lag()` method to `RingReader` |
| `pkg/util/ringbuffer_test.go` | Add `Lag()` test |
| `module/rtmp/subscriber.go` | Integrate filter |
| `module/srt/subscriber.go` | Integrate filter |
| `module/rtsp/server.go` | Integrate filter |

HTTP stream muxer workers do NOT need integration — they write to SharedBuffer, not directly to end users.

## Not in Scope

- Simulcast-based quality switching
- Audio-on-demand pause/resume
- Per-subscriber bitrate adaptation
