# Fix Ineffective Stream Config Fields

## Overview

Five `StreamConfig` fields are parsed from YAML but never used at runtime. This plan wires each one into the correct code path, grouped into five logical commits.

## Priority Order and Commit Grouping

| Commit | Config Field(s) | Risk | Files Modified |
|--------|-----------------|------|----------------|
| 1 | `gop_cache` (bool) | Low | `core/stream.go`, `core/stream_test.go` |
| 2 | `gop_cache_num` (int) | Medium | `core/stream.go`, `core/stream_test.go` |
| 3 | `audio_cache_ms` (int) | Medium | `core/stream.go`, `core/stream_test.go` |
| 4 | `idle_timeout` (duration) | Medium | `core/stream.go`, `core/stream_test.go` |
| 5 | `max_skip_count` + `max_skip_window` | Medium | `config/loader.go`, `pkg/util/ringbuffer.go`, `core/stream.go`, `core/stream_test.go`, `module/rtmp/subscriber.go` |

### NOT in scope (large features):
- `simulcast.*` - Requires full simulcast layer management
- `audio_on_demand.*` - Requires audio track pause/resume
- `feedback.*` - Requires RTCP feedback routing

---

## Commit 1: Wire `gop_cache` boolean

**Problem**: `s.config.GOPCache` is never checked. GOP caching always happens.

**Changes in `core/stream.go`**: Wrap GOP cache block (lines 179-189) in `if s.config.GOPCache { ... }`.

**Test**: `TestStreamGOPCacheDisabled` - set `GOPCache=false`, write keyframe+inter, verify `GOPCache()` returns empty.

---

## Commit 2: Wire `gop_cache_num` for multi-GOP support

**Problem**: On each keyframe, `gopCache` resets to single-element slice.

**Changes in `core/stream.go`**:
1. Change `gopCache []*avframe.AVFrame` to `gopCache [][]*avframe.AVFrame`
2. On keyframe: append new GOP, trim to `GOPCacheNum`
3. On inter/audio: append to last GOP
4. Update `GOPCache()`, `GOPCacheLen()`, `GOPCacheDetail()` to flatten nested slices

**Test**: `TestStreamMultiGOPCache` - GOPCacheNum=3, write 4 GOPs, verify 3 GOPs retained (9 frames), first frame DTS=120.

---

## Commit 3: Wire `audio_cache_ms` for independent audio caching

**Problem**: No independent audio cache. Audio only cached in GOP interleave.

**Changes in `core/stream.go`**:
1. Add `audioCache []*avframe.AVFrame` field
2. In `WriteFrame()`: cache audio frames, trim by DTS window
3. Add `AudioCache()` public accessor

**Test**: `TestStreamAudioCacheMs` - 500ms window, 10 frames at 100ms intervals, verify 6 frames retained (DTS >= 400).

---

## Commit 4: Wire `idle_timeout` for stream cleanup

**Problem**: No idle stream cleanup when no publishers AND no subscribers.

**Changes in `core/stream.go`**:
1. Add `idleTimer *time.Timer` field
2. Add `checkIdleTimeout()` helper: starts timer when no pub + no sub
3. Call from `RemoveSubscriber()`, `RemovePublisher()`
4. Cancel timer in `AddSubscriber()`, `SetPublisher()`, `Close()`

**Test**: `TestStreamIdleTimeout` - remove pub+sub, verify destroying after timeout.
**Test**: `TestStreamIdleTimeoutCancelledBySubscriber` - add subscriber before timeout fires.

---

## Commit 5: Wire `max_skip_count` + `max_skip_window`

**Problem**: No skip protection for slow subscribers.

**Changes**:
1. `config/loader.go`: Add `MaxSkipWindow: 60 * time.Second` default
2. `pkg/util/ringbuffer.go`: Add `lastSkipped` field + `Skipped()` method to `RingReader`
3. `core/stream.go`: Add `SkipTracker` type + `Config()` accessor
4. `module/rtmp/subscriber.go`: Wire skip tracking in WriteLoop

**Tests**: `TestSkipTracker`, `TestSkipTrackerWindowExpiry`, `TestMaxSkipWindowDefault`.

---

## Execution Order

Commits 1-5 in order. After all: `go test -race ./config/... ./core/... ./pkg/util/... ./module/rtmp/...`
