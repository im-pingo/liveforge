# LL-HLS (Low-Latency HLS) Design Spec

## Overview

Add Low-Latency HLS support to liveforge as a config-gated feature alongside existing regular HLS. LL-HLS delivers ~2s end-to-end latency using partial segments, blocking playlist reload, and preload hints per RFC 8216bis (HLS protocol version 9).

## Requirements

- **Coexistence**: LL-HLS and regular HLS coexist, controlled by `llhls.enabled` config flag
- **Code isolation**: LL-HLS lives in its own files; existing `hls.go` is untouched
- **Container support**: Both fMP4 and TS, configurable
- **Target latency**: ~2s (PART-TARGET=0.2s)
- **Self-contained**: No external library dependencies; uses existing `pkg/muxer/fmp4` and `pkg/muxer/ts`
- **Backward compatible**: Legacy HLS players ignore LL-HLS tags and play normally from full segments

## Config

```yaml
http_stream:
  hls:
    segment_duration: 6
    playlist_size: 5
  llhls:
    enabled: true
    part_duration: 0.2       # partial segment target duration (seconds)
    segment_count: 4         # full segments in playlist sliding window
    container: "fmp4"        # "fmp4" or "ts"
```

- When `llhls.enabled=true`: `.m3u8` requests serve LL-HLS playlists; partial/init segment endpoints are active
- When `llhls.enabled=false` (default): `.m3u8` requests serve regular HLS as before
- Both configs can exist simultaneously; the `enabled` flag is the switch

## Architecture

### New Files (all in `module/httpstream/`)

| File | Responsibility | ~Lines |
|------|---------------|--------|
| `llhls_segmenter.go` | Read AVFrames, produce partial + full segments | ~250 |
| `llhls_playlist.go` | Generate m3u8 with LL-HLS tags, delta updates | ~250 |
| `llhls_manager.go` | Orchestrate segmenter + playlist, blocking reload | ~300 |

### Modified Files

| File | Change |
|------|--------|
| `config/config.go` | Add `LLHLSConfig` struct |
| `config/loader.go` | Add LLHLS defaults |
| `module/httpstream/module.go` | Add `llhlsManagers` map, `getOrCreateLLHLS()`, cleanup hooks |
| `module/httpstream/handler.go` | Add LLHLS routing branch in `handleStream()`, new partial segment handler |

### Untouched Files

- `module/httpstream/hls.go` — existing HLS stays as-is
- All `pkg/muxer/*` — used as-is, no modifications
- `core/*` — no changes needed

## Component Design

### LLHLSSegmenter

Reads AVFrames from `stream.RingBuffer()`, produces segments:

```go
type LLHLSSegmenter struct {
    partDuration float64           // target partial segment duration (0.2s)
    container    string            // "fmp4" or "ts"
    onPart       func(part *LLHLSPart)
    onSegment    func(seg *LLHLSSegment)
    onInit       func(data []byte) // fMP4 init segment (fMP4 only)
}
```

- Accumulates frames until `partDuration` elapsed, then emits a partial segment
- On video keyframe: finalizes current partial, finalizes current full segment (collecting all its partials), starts new segment
- For fMP4: each partial is a moof+mdat fragment; init segment generated once from sequence headers
- For TS: each partial is a self-contained TS chunk with PAT/PMT

### LLHLSPart (Partial Segment)

```go
type LLHLSPart struct {
    Index       int     // part index within its parent segment
    Duration    float64 // actual duration in seconds
    Independent bool    // starts with keyframe (IDR)
    Data        []byte  // muxed bytes
}
```

### LLHLSSegment (Full Segment)

```go
type LLHLSSegment struct {
    MSN      int            // media sequence number
    Duration float64        // total duration in seconds
    Parts    []*LLHLSPart   // partial segments belonging to this segment
    Data     []byte         // concatenated full segment (all parts) — for legacy players
}
```

### LLHLSPlaylist

Generates m3u8 playlist text:

```go
type LLHLSPlaylist struct {
    partTarget   float64 // EXT-X-PART-INF PART-TARGET
    segmentCount int     // sliding window size
}

func (p *LLHLSPlaylist) Generate(segments []*LLHLSSegment, currentParts []*LLHLSPart, skip bool) string
```

**Playlist tags generated:**

- `#EXT-X-VERSION:9`
- `#EXT-X-TARGETDURATION:<ceil(maxSegDur)>`
- `#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=<3*partTarget>,CAN-SKIP-UNTIL=<6*targetDur>`
- `#EXT-X-PART-INF:PART-TARGET=<partTarget>`
- `#EXT-X-MAP:URI="<basePath>/init.mp4"` (fMP4 only)
- `#EXT-X-MEDIA-SEQUENCE:<seqBase>`
- For each completed segment: `#EXTINF:<dur>,` + segment URI + `#EXT-X-PART` for each partial
- For in-progress segment: `#EXT-X-PART` entries for available partials
- `#EXT-X-PRELOAD-HINT:TYPE=PART,URI=<next partial URI>`
- `#EXT-X-SKIP:SKIPPED-SEGMENTS=<n>` (when `_HLS_skip=YES`)

### LLHLSManager

Orchestrator:

```go
type LLHLSManager struct {
    mu          sync.Mutex
    cond        *sync.Cond        // broadcast on new partial/segment
    segments    []*LLHLSSegment   // completed segments (sliding window)
    currentParts []*LLHLSPart     // partials of in-progress segment
    currentMSN  int               // MSN of in-progress segment
    initSegment []byte            // fMP4 init segment
    segmenter   *LLHLSSegmenter
    playlist    *LLHLSPlaylist
    streamKey   string
    basePath    string
    container   string
    done        chan struct{}
}
```

**Blocking playlist reload:**

```go
func (m *LLHLSManager) GeneratePlaylist(targetMSN, targetPart int, skip bool) string {
    m.mu.Lock()
    defer m.mu.Unlock()

    // Block until requested MSN/part is available
    for !m.hasContent(targetMSN, targetPart) {
        m.cond.Wait()
    }

    return m.playlist.Generate(m.segments, m.currentParts, skip)
}
```

- `cond.Broadcast()` called whenever a new partial or full segment is produced
- Timeout handled by caller (HTTP handler uses `r.Context().Done()`)

## URL Scheme

| URL Pattern | Description |
|------------|-------------|
| `GET /{app}/{key}.m3u8` | LL-HLS playlist (or regular HLS if llhls disabled) |
| `GET /{app}/{key}.m3u8?_HLS_msn=N&_HLS_part=M` | Blocking playlist reload |
| `GET /{app}/{key}.m3u8?_HLS_skip=YES` | Delta playlist update |
| `GET /{app}/{key}/init.mp4` | fMP4 init segment (shared with DASH) |
| `GET /{app}/{key}/{MSN}.m4s` | Full fMP4 segment |
| `GET /{app}/{key}/{MSN}.{partIdx}.m4s` | Partial fMP4 segment |
| `GET /{app}/{key}/{MSN}.ts` | Full TS segment (TS container mode) |
| `GET /{app}/{key}/{MSN}.{partIdx}.ts` | Partial TS segment (TS container mode) |

## Blocking Reload Implementation

1. HTTP handler parses `_HLS_msn` and `_HLS_part` query params
2. Calls `manager.GeneratePlaylist(msn, part, skip)` in a goroutine
3. Manager locks mutex, checks if content is available
4. If not available: enters `cond.Wait()` loop
5. Segmenter produces new partial → manager stores it → calls `cond.Broadcast()`
6. Waiter wakes, checks condition, generates playlist if satisfied
7. HTTP handler uses `select` with `r.Context().Done()` for timeout (client disconnect or server shutdown)

To integrate `sync.Cond` with context cancellation:
- Spawn a goroutine that waits on context, then does `cond.Broadcast()` to unblock
- Main goroutine checks both content availability and context error after waking

## Edge Cases and Design Decisions

### Audio-Only Streams

When no video track is present (no keyframes to split on), the segmenter uses time-based splitting:
- Full segments split at `segment_count`-derived intervals (default ~6s)
- Partial segments split at `partDuration` intervals (0.2s)
- All partials are marked `INDEPENDENT=YES` since audio frames are independently decodable

### Codec Change Mid-Stream

If the publisher reconnects with different codec parameters:
- The manager detects changed sequence headers
- Regenerates the fMP4 init segment
- Inserts `#EXT-X-DISCONTINUITY` tag in the playlist before the next segment
- Resets the segmenter state

### GOP Cache Handling

LL-HLS skips the GOP cache entirely. Playing cached GOP frames would add latency that defeats the purpose of LL-HLS. The segmenter starts reading from the current RingBuffer write cursor and only serves live partials. Legacy HLS players will wait for the first full segment to complete before playback begins, which is expected behavior.

### Memory Eviction

When a full segment slides out of the playlist window, all its `LLHLSPart` entries and their `Data` slices are released. The sliding window is trimmed in `LLHLSManager` after each new full segment is finalized. With `partDuration=0.2s`, `segment_count=4`, and ~6s segments, peak memory is ~120 partials per stream (~3MB at 1Mbps).

### Mutual Exclusivity with Regular HLS

When `llhls.enabled=true`, the regular `HLSManager` is NOT created for any stream. The LL-HLS playlist already contains full segment references (`#EXTINF` + segment URI) for backward compatibility with legacy players. Running both managers simultaneously would waste memory and CPU. The `getOrCreateHLS()` path in `module.go` is gated by `!llhls.enabled`.

### fMP4 Muxer Usage for Partial Segments

The existing `fmp4.Muxer.WriteSegment()` is called once per partial segment (not once per full segment). Each call produces a moof+mdat fragment with an incrementing `sequenceNumber`. This is correct CMAF behavior — each partial is an independently addressable CMAF chunk. The full segment `Data` field is the byte concatenation of all its partials' `Data`.

### TS Muxer PAT/PMT for Partial Segments

For TS container mode, each partial segment must begin with PAT/PMT tables to be independently playable. The segmenter calls `ts.Muxer.WritePATAndPMT()` at the start of each partial (this method already exists in the TS muxer). Frames within the partial are muxed normally via `WriteFrame()`.

### Server-Side Hold Timeout

Blocking playlist requests have a server-side maximum hold duration of `3 * targetSegmentDuration` (default 18s). If the requested MSN/part is not produced within this time, the server returns the current playlist immediately. This prevents CDN intermediaries from holding connections indefinitely.

### Caching Headers

| Endpoint | Cache-Control |
|----------|--------------|
| `.m3u8` playlist | `no-cache, no-store` |
| `init.mp4` | `public, max-age=3600` |
| Full segment (`.m4s`/`.ts`) | `public, max-age=60` |
| Partial segment (`.m4s`/`.ts`) | `no-cache` (may still be in-progress) |

### Non-Blocking Plain Playlist Request

When a client requests `/{app}/{key}.m3u8` without `_HLS_msn`/`_HLS_part` query params, the server returns the full current playlist immediately (non-blocking). This is the path legacy HLS players use.

### Subscribe Event

LL-HLS playlist and segment requests do NOT emit `EventSubscribe`, consistent with the existing HLS and DASH handler behavior. Auth hooks that gate subscribe events are not triggered for HTTP segment delivery.

### Out of Scope (Future Work)

- `_HLS_skip=v2` (date-range skipping) — not needed without EXT-X-DATERANGE usage
- `EXT-X-RENDITION-REPORT` — deferred until multi-rendition/ABR support is added
- Per-stream LL-HLS toggle — current implementation is global config only

## Error Handling

- If stream not publishing: 404
- If blocking request times out (server-side hold exceeded or client disconnects): return current playlist
- If segment not found (expired from window): 404
- If init segment not ready yet: brief poll (100ms x 50) then 404

## Testing Strategy

- **Unit tests for LLHLSPlaylist**: verify m3u8 output contains correct tags, version 9, proper PART-HOLD-BACK math
- **Unit tests for LLHLSSegmenter**: feed AVFrames, verify partial segments emitted at correct intervals, keyframe splits work
- **Unit tests for LLHLSManager**: verify blocking reload (concurrent goroutines), sliding window trimming, delta updates
- **Integration test**: publish RTMP stream → request LL-HLS playlist → verify partial segments are fetchable
