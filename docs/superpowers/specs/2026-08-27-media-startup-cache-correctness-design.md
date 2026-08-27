# Media Startup and Cache Correctness Design

**Date:** 2026-08-27

**Status:** Approved for implementation

## Context

LiveForge currently maintains both a video-keyframe-bounded GOP cache and an
independent rolling `audioCache`. Production subscribers do not consume the
rolling audio cache; only the stream API and Console report it. The extra cache
therefore increases retained memory and creates a misleading model in which
audio appears to be cached independently even though audio and video are already
interleaved in each cached GOP.

The surrounding startup path also has correctness gaps:

- a replacement publisher inherits the previous publisher's sequence headers,
  GOP state, readiness channel, and retained ring history;
- production ingress writes identify only the stream, so a delayed old
  publisher can write into a replacement publisher's generation;
- consumers obtain GOP frames, sequence headers, media information, and ring
  cursors through separate calls, allowing rollover races and duplicates;
- readiness closes after either sequence header and consumers discard later
  live sequence-header updates;
- pure-audio HLS and DASH do not complete live segments, while LL-HLS uses a
  hard-coded six-second full-segment threshold;
- SRT filters live frames using a cross-track DTS watermark even though the
  atomic cursor already prevents duplicates;
- SIP outbound calls start at the oldest retained ring position and can burst
  stale history.

## Goals

1. Remove the independent `audioCache` implementation and every public surface
   that presents it as a supported cache.
2. Isolate every publisher generation so stale writes and stale startup state
   cannot enter a replacement publication.
3. Give subscribers one atomic startup view containing all state needed to
   replay a GOP and continue at the live cursor exactly once.
4. Make startup readiness aware of codecs that require configuration and codecs
   that can start from media frames alone.
5. Produce bounded live HLS, DASH, and LL-HLS output for pure-audio streams.
6. Remove cross-track timestamp filtering from SRT and stale-ring startup from
   SIP, recording, DVR, and protocol relay paths touched by the new contract.
7. Keep API, Console, configuration schemas, examples, AI-facing documents, and
   user documentation synchronized with the implementation.

## Non-Goals

- The ring buffer remains the live fan-out transport and is not renamed or
  presented as another cache.
- No general video or audio transcoding redesign is included.
- Existing subscribers are not migrated in place across a publisher change.
  They end cleanly; clients reconnect and negotiate against the new generation.
- Pure audio does not gain a second rolling history structure. A late subscriber
  starts at the atomic live cursor and receives the next frame.
- Ring-buffer slow-reader overwrite policy is not changed in this work.

## Core Model

### Publisher generation

`Stream` owns a monotonically increasing `publisherGeneration`. A successful
`SetPublisher` increments it and records `generationStartCursor` from the ring
buffer. It also clears sequence headers, internal media information, GOP frames,
GOP source positions, GOP numbering, and readiness state before accepting media
from the new publisher.

Each active generation has a `generationDone` channel. Removing or replacing
the publisher closes that channel. A blocking subscriber can use it to close its
ring reader and terminate without consuming the next generation. Subscribers
must check the generation after each read before processing the frame, so the
first frame that wakes an old reader after republish is discarded rather than
sent.

Production ingress calls:

```go
func (s *Stream) WriteFrameForPublisher(pub Publisher, frame *avframe.AVFrame) bool
```

The method accepts a frame only when `pub` is the active publisher. Identity is
checked while holding the same stream mutex used for generation changes. The
existing `WriteFrame` method remains available for tests and explicit internal
injection, but production protocol ingress must not call it.

### Stream-owned media information

The stream stores its own immutable-by-copy `avframe.MediaInfo` snapshot. On
publisher assignment it merges the publisher's current information. On every
accepted frame it records the observed track codec, and sequence-header frames
update the corresponding configuration bytes. This makes the startup view
atomic with cache and cursor updates instead of depending on a separately
mutable publisher object.

### Startup readiness

Readiness is evaluated for the current generation:

- no observed audio or video track: not ready;
- H.264, H.265, AV1, VP8, VP9, AAC, and Opus tracks that require container or
  decoder configuration: ready only after the corresponding sequence header is
  known;
- MP3, G.711 A-law, G.711 mu-law, G.722, and G.729: ready after the first media
  frame or an upfront `MediaInfo` declaration because no sequence header is
  required;
- every currently known track must satisfy its rule.

The readiness signal is generation-specific and reset on every publisher
change. A later sequence-header update is still delivered through the live ring;
consumers may refresh their muxer/decoder state and must not discard it merely
because an initial header was sent.

Because some protocols discover tracks incrementally, readiness cannot predict
a track that has not yet appeared. Atomic cursor capture guarantees that a track
and its header appearing after startup are delivered live. Container muxers that
cannot add tracks in place end their generation and are recreated through the
muxer manager when the client reconnects.

## Atomic Startup Snapshot

The old split methods `GOPCacheSnapshot`, `GOPCacheSourceStart`,
`VideoSeqHeader`, and `AudioSeqHeader` are replaced in production startup paths
by one value:

```go
type StreamStartupSnapshot struct {
    Generation          uint64
    GenerationStartCursor int64
    MediaInfo           avframe.MediaInfo
    VideoSequenceHeader *avframe.AVFrame
    AudioSequenceHeader *avframe.AVFrame
    ReplayFrames        []*avframe.AVFrame
    LiveCursor          int64
    SourceCursor        int64
    GenerationDone      <-chan struct{}
    Ready               bool
}

func (s *Stream) StartupSnapshot() StreamStartupSnapshot
func (s *Stream) WaitForStartup(ctx context.Context) (StreamStartupSnapshot, bool)
func (s *Stream) IsPublisherGeneration(generation uint64) bool
```

All fields are captured while holding the stream read lock. `ReplayFrames` is
the flattened GOP cache. `LiveCursor` is the ring write cursor immediately
after every replay frame. `SourceCursor` is the oldest cached GOP source cursor
when a GOP exists; otherwise it equals `LiveCursor`, so pure audio never replays
the retained ring as implicit history.

`WaitForStartup` waits on an internal state-change signal and returns only a
ready, actively publishing generation. Context cancellation returns `false`.

Direct subscribers follow one invariant:

```text
wait for ready snapshot
send snapshot sequence headers as required by the protocol
send ReplayFrames once
read RingBuffer from LiveCursor
drop/exit if GenerationDone closes or generation no longer matches
```

Transforming subscribers use `SourceCursor` for the transform input and still
use `LiveCursor` for the direct source track. This removes separate snapshot
calls and cross-track DTS overlap filters.

## Muxer Lifetime

`MuxerInstance` records the publisher generation for which it was created.
`MuxerManager.GetOrCreateMuxer` creates a fresh instance when the current map
contains an instance from another generation. The old instance is retired by
closing its `Done` channel, but registered format callbacks remain on the
manager.

Release is instance-specific:

```go
func (mm *MuxerManager) ReleaseMuxer(format string, inst *MuxerInstance)
```

An old HTTP request can therefore release its retired instance without
decrementing the subscriber count of the replacement generation.

## Audio Cache Removal

The following are removed:

- `Stream.audioCache`, `AudioCache`, and `AudioCacheDetail`;
- `stream.audio_cache_ms` and `StreamConfig.AudioCacheMs`;
- API fields `audio_cache_frames` and `audio_cache_duration_ms`;
- Console's independent Audio cache row and the temporary `Media Cache` label.

The Console label returns to `GOP Cache`. For video streams it displays GOP
number, total duration, and interleaved video/audio frame counts. For a
pure-audio stream it displays `Not applicable (audio-only)` because no GOP
exists. A hard-coded frame-count progress bar is removed.

Configuration documents containing `stream.audio_cache_ms` fail with the
explicit error `stream.audio_cache_ms has been removed; audio is interleaved in
the GOP cache`. Both file loading and runtime-source parsing apply this check.
There is no silent ignored compatibility field because no public release
contains this setting.

## Protocol Changes

### RTMP and RTSP

RTMP and RTSP subscribers consume one startup snapshot and continue from its
live cursor. RTMP sends sequence-header updates that arrive after startup.
RTSP keeps sequence headers in SDP/parameter-set handling and exits if the
publisher generation changes.

RTMP and RTSP publishers write with their own publisher identity. The RTMP
handler updates only `h.publisher`, never whichever publisher currently happens
to be attached to the stream.

### SRT

SRT builds MPEG-TS track state from the atomic snapshot. It removes the maximum
cached DTS filter; the cursor is the sole duplicate boundary. A live sequence
header rebuilds track configuration when necessary. Generation change closes
the subscriber.

### WebRTC

WHIP track readers write using the WHIP publisher identity. WHEP uses the atomic
snapshot for direct and transcode cursors, and stops when that generation ends.

### SIP and GB28181

SIP inbound receive loops and GB28181/PS receivers write with the exact session
publisher. SIP outbound startup sends only the snapshot GOP when video is
present and otherwise starts at `LiveCursor`; it never uses `NewReader()`.
GB28181 outbound relay follows the same snapshot/cursor rule.

### Cluster, recording, and DVR

Cluster pull transports write with their relay publisher identity. Push
transports, recording, and DVR start from an atomic generation snapshot rather
than the oldest retained ring entry. Recording/DVR write snapshot headers and
GOP once, then continue from `LiveCursor`, preventing stale-generation prefix
data and duplicate headers.

## Pure-Audio HTTP Segmentation

HLS and DASH retain keyframe boundaries for streams containing video. When the
snapshot declares no video track, they finalize a segment once audio DTS reaches
the configured `segment_duration`. The frame at the boundary starts the next
segment, so no frame is duplicated or omitted.

LL-HLS adds `http_stream.llhls.segment_duration` with a default of `1.0`
seconds. It remains keyframe-bounded for video and uses that configured duration
for pure-audio full segments. Partial segments continue to use
`part_duration`. The initial request still waits for a completed segment for
the bundled Hls.js compatibility, but the wait is now bounded by the explicit
low-latency full-segment policy instead of the former hard-coded six seconds.

## API and Documentation Compatibility

The `/api/v1/streams` response removes the two unreleased audio-cache fields.
The GOP fields stay unchanged. OpenAPI, API tests, Console fixtures, both JSON
schemas, all runnable configurations, README files, architecture and protocol
recipes, `agent-manifest.json`, and `llms-full.txt` change in the same commits as
their source behavior.

## Verification

Required regression coverage includes:

- stale publisher writes are rejected after replacement;
- replacement clears headers/GOP and startup cursors never expose old frames;
- snapshot replay plus live cursor has no gap or duplicate during concurrent
  writes and GOP rollover;
- AAC/H.264 waits for configuration while MP3/G.711 becomes ready without a
  sequence header;
- later live sequence-header updates reach RTMP/SRT startup logic;
- pure-audio HLS, DASH, LL-HLS fMP4, and LL-HLS TS produce live segments within
  configured duration;
- SRT preserves audio whose DTS is below a video replay DTS;
- SIP outbound does not send pre-snapshot retained audio history;
- removed config and API fields are rejected/absent and Console shows GOP-only
  semantics;
- no production module ingress uses raw `Stream.WriteFrame`.

The final gate is the repository baseline build/test suite with `audiocodec`,
race detection, agent-document checks, and an independent whole-branch review.
