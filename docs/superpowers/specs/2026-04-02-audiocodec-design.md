# AudioCodec Design Spec

## Overview

LiveForge currently cannot play audio across protocol boundaries when codecs differ. For example, an RTMP stream published with AAC produces no audio when played via WebRTC (which requires Opus). The AudioCodec subsystem introduces on-demand, shared audio transcoding at the Stream level so that any subscriber can receive audio in its native codec regardless of what the publisher sends.

## Problem Statement

Current behavior when codecs mismatch:

| Publisher | Subscriber | Audio result |
|-----------|-----------|--------------|
| RTMP (AAC) | RTMP | Works (same codec) |
| RTMP (AAC) | WebRTC WHEP | **No audio** (`codecToMime` returns "" for AAC) |
| WebRTC (Opus) | RTMP | **No audio** (FLV muxer expects AAC) |
| RTMP (AAC) | Future SIP (G.711) | **No audio** |

This is documented in `module/webrtc/whep_feed.go:134-136`:

```go
// AAC from RTMP is not WebRTC compatible (no codecToMime mapping),
// so audioSender will be nil in that case and this is a no-op.
```

## Design Goals

1. **Zero overhead when codecs match** -- subscribers read directly from the original RingBuffer with no transcoding, no copies, no extra goroutines.
2. **On-demand shared transcoding** -- the first subscriber that needs a different codec triggers a transcode goroutine. Subsequent subscribers share the same transcoded output.
3. **Pluggable codec registry** -- new codecs are added by implementing `Decoder`/`Encoder` interfaces and registering them. Build tags control which codecs are compiled in.
4. **No core API breakage** -- existing subscribers that don't need transcoding continue to work unchanged.

## Architecture

### Layer Diagram

```
┌──────────────────────────────────────────────┐
│  Protocol Modules (RTMP, WebRTC, RTSP, SRT)  │
│  Each subscriber calls TranscodeManager       │
│  to get a reader for its target codec         │
├──────────────────────────────────────────────┤
│  core/TranscodeManager                        │
│  Per-stream, manages shared TranscodedTracks  │
│  Created on-demand, destroyed when idle       │
├──────────────────────────────────────────────┤
│  pkg/audiocodec                               │
│  Decoder/Encoder interfaces + Registry        │
│  PCMFrame as universal exchange format        │
├──────────────────────────────────────────────┤
│  Codec implementations                        │
│  G.711 (builtin) | G.722 (builtin)           │
│  Opus (cgo, opt) | AAC (cgo, opt)            │
└──────────────────────────────────────────────┘
```

### Data Flow

```
Publisher (RTMP, AAC audio)
    │
    ▼
Stream.RingBuffer [AVFrame with AAC payload]
    │
    ├── RTMP subscriber: reads directly (codec match → zero overhead)
    ├── MuxerManager: reads for HLS/DASH/FLV muxing (unchanged)
    │
    └── TranscodeManager
          │
          ├── "opus" TranscodedTrack (created on first WebRTC subscriber)
          │     goroutine: read AAC → decode PCM → encode Opus → write RingBuffer
          │     ├── WebRTC subscriber 1 (shared reader)
          │     └── WebRTC subscriber 2 (shared reader)
          │
          └── "pcmu" TranscodedTrack (created on first G.711 subscriber)
                goroutine: read AAC → decode PCM → encode G.711U → write RingBuffer
                └── future conference system subscriber
```

### Key Property: Video Passthrough

TranscodedTrack goroutines read ALL frames from the source RingBuffer but only transcode audio. Video frames are forwarded as-is to the transcoded RingBuffer. This means a subscriber reading from a TranscodedTrack gets both:
- Transcoded audio in the target codec
- Original video unchanged

This allows subscribers to use a single reader for both audio and video.

## Interfaces

### pkg/audiocodec/codec.go

```go
// PCMFrame is the universal exchange format between all audio codecs.
// All decoders produce PCMFrame; all encoders consume PCMFrame.
type PCMFrame struct {
    Samples    []int16 // interleaved samples (L,R,L,R... or mono)
    SampleRate int     // 8000, 16000, 44100, 48000
    Channels   int     // 1 or 2
}

// Decoder decodes compressed audio into PCM.
// Instances are NOT safe for concurrent use. Each transcode goroutine
// creates its own Decoder via the Registry.
type Decoder interface {
    // Decode decodes one audio frame into PCM samples.
    Decode(payload []byte) (*PCMFrame, error)
    // SampleRate returns the decoder's output sample rate.
    SampleRate() int
    // Channels returns the decoder's output channel count.
    Channels() int
}

// Encoder encodes PCM into compressed audio.
// Instances are NOT safe for concurrent use. Each transcode goroutine
// creates its own Encoder via the Registry.
type Encoder interface {
    // Encode encodes PCM samples into one compressed audio frame.
    Encode(pcm *PCMFrame) ([]byte, error)
    // SampleRate returns the encoder's expected input sample rate.
    // The transcode goroutine resamples PCM to this rate before encoding.
    SampleRate() int
    // Channels returns the encoder's expected input channel count.
    Channels() int
}

// SequenceHeaderFunc returns an initial sequence header frame for the
// target codec, or nil if the codec does not use sequence headers.
// For example, AAC requires an AudioSpecificConfig frame before data.
type SequenceHeaderFunc func() []byte
```

### pkg/audiocodec/registry.go

```go
// Registry manages available audio codecs.
// Codec implementations register themselves via init() functions.
type Registry struct {
    mu       sync.RWMutex
    decoders map[avframe.CodecType]DecoderFactory
    encoders map[avframe.CodecType]EncoderFactory
}

type DecoderFactory func() Decoder
type EncoderFactory func() Encoder
type SeqHeaderFactory func() SequenceHeaderFunc // optional, may return nil

// Global returns the process-wide codec registry.
// Codec packages register themselves here during init().
func Global() *Registry

// NewDecoder creates a decoder for the given codec, or returns an error
// if the codec is not available (not compiled in).
func (r *Registry) NewDecoder(codec avframe.CodecType) (Decoder, error)

// NewEncoder creates an encoder for the given codec.
func (r *Registry) NewEncoder(codec avframe.CodecType) (Encoder, error)

// CanTranscode returns true if both decoder(from) and encoder(to) are available.
func (r *Registry) CanTranscode(from, to avframe.CodecType) bool

// RegisterSequenceHeader registers an optional sequence header factory for a codec.
func (r *Registry) RegisterSequenceHeader(codec avframe.CodecType, fn SeqHeaderFactory)

// SequenceHeader returns the sequence header bytes for a codec, or nil if
// the codec does not use sequence headers.
func (r *Registry) SequenceHeader(codec avframe.CodecType) []byte
```

### Codec Registration Pattern

```go
// pkg/audiocodec/g711.go
func init() {
    r := Global()
    r.RegisterDecoder(avframe.CodecG711U, func() Decoder { return &G711UDecoder{} })
    r.RegisterEncoder(avframe.CodecG711U, func() Encoder { return &G711UEncoder{} })
    r.RegisterDecoder(avframe.CodecG711A, func() Decoder { return &G711ADecoder{} })
    r.RegisterEncoder(avframe.CodecG711A, func() Encoder { return &G711AEncoder{} })
}
```

Optional codecs use build tags:

```go
// pkg/audiocodec/opus.go
//go:build audio_opus

func init() {
    r := Global()
    r.RegisterDecoder(avframe.CodecOpus, func() Decoder { return NewOpusDecoder() })
    r.RegisterEncoder(avframe.CodecOpus, func() Encoder { return NewOpusEncoder() })
}
```

## core/TranscodeManager

### Struct

```go
type TranscodedTrack struct {
    targetCodec avframe.CodecType
    ringBuffer  *util.RingBuffer[*avframe.AVFrame]
    subCount    int
    cancel      context.CancelFunc
}

type TranscodeManager struct {
    mu       sync.Mutex
    tracks   map[avframe.CodecType]*TranscodedTrack
    stream   *Stream
    registry *audiocodec.Registry
    bufSize  int
}
```

### Core Method: GetOrCreateReader

```go
// GetOrCreateReader returns a RingReader that produces AVFrames with audio
// in the target codec, plus a release function that the caller MUST call
// when done (similar to context.WithCancel returning a cancel function).
//
// If the publisher's audio codec matches targetCodec, it returns a reader
// on the original RingBuffer (zero overhead) and a no-op release function.
// Otherwise it returns a reader on a shared TranscodedTrack, creating one
// if needed.
func (tm *TranscodeManager) GetOrCreateReader(
    targetCodec avframe.CodecType,
) (reader *util.RingReader[*avframe.AVFrame], release func(), err error)
```

Behavior:

0. If `stream.Publisher()` is nil → return error immediately. Callers must verify publisher presence, or handle this error as "no audio available yet."
1. If `stream.Publisher().MediaInfo().AudioCodec == targetCodec` → return `stream.RingBuffer().NewReader()` with a no-op release. No TranscodedTrack created.
2. If `!registry.CanTranscode(sourceCodec, targetCodec)` → return error (codec not available).
3. If TranscodedTrack for targetCodec already exists → increment subCount, return new reader with release closure.
4. Otherwise → create TranscodedTrack, start transcode goroutine, return reader with release closure.

The release closure captures the target codec internally, so the caller never needs to remember which codec was requested.

### Transcode Goroutine

```go
func (tm *TranscodeManager) transcodeLoop(ctx context.Context, track *TranscodedTrack) {
    sourceCodec := tm.stream.Publisher().MediaInfo().AudioCodec

    decoder, err := tm.registry.NewDecoder(sourceCodec)
    if err != nil {
        slog.Error("transcode: decoder unavailable", "from", sourceCodec, "error", err)
        track.ringBuffer.Close()
        return
    }
    encoder, err := tm.registry.NewEncoder(track.targetCodec)
    if err != nil {
        slog.Error("transcode: encoder unavailable", "to", track.targetCodec, "error", err)
        track.ringBuffer.Close()
        return
    }

    // Emit target codec's sequence header if the encoder provides one.
    // For example, AAC encoders produce an AudioSpecificConfig frame that
    // RTMP/FLV subscribers require before audio data frames.
    if seqHeader := tm.registry.SequenceHeader(track.targetCodec); seqHeader != nil {
        track.ringBuffer.Write(avframe.NewAVFrame(
            avframe.MediaTypeAudio,
            track.targetCodec,
            avframe.FrameTypeSequenceHeader,
            0, 0,
            seqHeader,
        ))
    }

    // Start from the current write position to avoid burst-reading stale frames.
    // This matches the WHEP feed loop pattern (NewReaderAt with latest position).
    reader := tm.stream.RingBuffer().NewReaderAt(tm.stream.RingBuffer().WriteCursor())
    for {
        select {
        case <-ctx.Done():
            track.ringBuffer.Close()
            return
        default:
        }

        frame, ok := reader.TryRead()
        if !ok {
            select {
            case <-ctx.Done():
                track.ringBuffer.Close()
                return
            case <-tm.stream.RingBuffer().Signal():
                continue
            }
        }

        // Video frames: passthrough unchanged
        if frame.MediaType.IsVideo() {
            track.ringBuffer.Write(frame)
            continue
        }

        // Audio sequence headers: skip (source codec-specific, not relevant for target)
        if frame.FrameType == avframe.FrameTypeSequenceHeader {
            continue
        }

        // Transcode: decode → PCM → (resample if needed) → encode
        pcm, err := decoder.Decode(frame.Payload)
        if err != nil {
            slog.Debug("transcode: decode error", "error", err)
            continue
        }
        if decoder.SampleRate() != encoder.SampleRate() ||
           decoder.Channels() != encoder.Channels() {
            pcm = resample(pcm, decoder.SampleRate(), encoder.SampleRate(),
                           decoder.Channels(), encoder.Channels())
        }
        encoded, err := encoder.Encode(pcm)
        if err != nil {
            slog.Debug("transcode: encode error", "error", err)
            continue
        }

        track.ringBuffer.Write(avframe.NewAVFrame(
            avframe.MediaTypeAudio,
            track.targetCodec,
            avframe.FrameTypeInterframe,
            frame.DTS, frame.PTS,
            encoded,
        ))
    }
}
```

### Cleanup via Release Closure

The release function returned by `GetOrCreateReader` is a closure that captures the target codec. Callers use `defer release()`:

```go
reader, release, err := stream.TranscodeManager().GetOrCreateReader(avframe.CodecOpus)
if err != nil { ... }
defer release()
```

Internally the release closure calls:

```go
func (tm *TranscodeManager) releaseTrack(targetCodec avframe.CodecType) {
    tm.mu.Lock()
    defer tm.mu.Unlock()

    track, ok := tm.tracks[targetCodec]
    if !ok {
        return // zero-overhead path (codec matched), no track to release
    }
    track.subCount--
    if track.subCount <= 0 {
        track.cancel()           // stop transcode goroutine
        delete(tm.tracks, targetCodec)
    }
}
```

### Publisher Change (Republish)

When a publisher disconnects and a new publisher starts on the same stream (possibly with a different audio codec), all active TranscodedTracks become stale. `Stream.SetPublisher()` must be modified to call `TranscodeManager.Reset()` before replacing the publisher. This is critical because the old publisher's codec-specific decoder would silently corrupt frames from the new publisher:

```go
func (tm *TranscodeManager) Reset() {
    tm.mu.Lock()
    defer tm.mu.Unlock()
    for codec, track := range tm.tracks {
        track.cancel()
        delete(tm.tracks, codec)
    }
}
```

Existing subscribers holding readers on stale TranscodedTracks will see the RingBuffer close and exit their read loops. They can reconnect and get a new reader with a fresh transcode goroutine for the new publisher's codec.

### GOP Cache for Transcoded Tracks

TranscodedTracks do NOT have their own GOP cache. Late-joining subscribers on a TranscodedTrack only receive frames from the point the transcode goroutine has reached. This is acceptable because:

1. **Video keyframes**: the transcode goroutine passes video through unchanged. The subscriber will receive the next keyframe from the source stream.
2. **Audio**: audio frames are small and frequent (~20ms each). Missing a few frames at join time causes at most a brief silence, not a visual glitch.
3. **Full GOP cache support** for transcoded tracks can be added later if needed, by having the transcode goroutine maintain its own cache. This is a future optimization, not a Phase 1 requirement.

For WebRTC WHEP specifically, the existing GOP cache flow can be preserved: send GOP cache frames from the original RingBuffer (video only, skip incompatible audio), then switch to the TranscodedTrack reader for live frames.

### Video Memory Note

The transcode goroutine copies video frames into the transcoded RingBuffer. This means video data exists in two RingBuffers when transcoding is active. The memory overhead is proportional to `ringBufferSize * averageFrameSize`. For typical configurations (1024 slots, ~10KB average frame), this is ~10MB per transcoded track. This is acceptable for Phase 1. A future optimization could use a dual-reader approach (video from original, audio from transcoded) to eliminate the duplication.

## G.711 Implementation (Phase 1)

G.711 PCMU (mu-law) and PCMA (A-law) are the simplest possible audio codecs. Each sample is one byte, decoded via a 256-entry lookup table.

### G.711U (mu-law, ITU-T G.711)

```go
// Decode: payload byte → int16 PCM sample via lookup table
// Encode: int16 PCM sample → payload byte via lookup table
// Sample rate: 8000 Hz, mono
// Bitrate: 64 kbps
// Complexity: O(n) table lookup, no allocations beyond output buffer
```

### G.711A (A-law, ITU-T G.711)

Same structure as G.711U with a different lookup table.

### Why G.711 First

- Pure Go, zero external dependencies
- Validates the entire architecture (interfaces, registry, TranscodeManager, shared readers)
- Trivially correct (lookup table, easy to test with known vectors)
- Required by the future Conference System (SIP endpoints use G.711 as baseline codec)

## Sample Rate Conversion

When transcoding between codecs with different sample rates (e.g., G.711 at 8kHz and Opus at 48kHz), resampling is needed. This is handled transparently inside the transcode goroutine:

```go
// In transcodeLoop, between decode and encode:
if decoder.SampleRate() != encoder.SampleRate() ||
   decoder.Channels() != encoder.Channels() {
    pcm = resample(pcm, decoder.SampleRate(), encoder.SampleRate(),
                   decoder.Channels(), encoder.Channels())
}
```

For Phase 1 (G.711 ↔ G.711), no resampling is needed (both 8kHz). Resampling will be added in Phase 2 when Opus (48kHz) is introduced.

## Integration Points

### WebRTC WHEP (primary consumer)

Current code in `whep_feed.go` reads directly from `stream.RingBuffer()` and skips audio when codec is unsupported. With AudioCodec:

```go
// Before: audioSender is nil for AAC → no audio
// After:
reader, release, err := stream.TranscodeManager().GetOrCreateReader(avframe.CodecOpus)
if err != nil {
    // Opus codec not available (not compiled in), fall back to no audio
    reader = stream.RingBuffer().NewReader()
    release = func() {} // no-op
}
defer release()

// reader now produces AVFrames with Opus audio + original video
```

### RTMP Subscriber (future)

When publisher is WebRTC (Opus) and subscriber is RTMP (needs AAC):

```go
reader, release, err := stream.TranscodeManager().GetOrCreateReader(avframe.CodecAAC)
if err != nil { ... }
defer release()
```

### Other Modules

HLS/DASH/HTTP-FLV: these modules use MuxerManager, which reads from the original RingBuffer and muxes into container formats. If the publisher codec is compatible with the container (AAC for FLV/TS), no change needed. If not (Opus publisher → HLS), the muxer worker would use TranscodeManager similarly.

## Configuration

### Config Struct

```go
// Added to config/config.go
type AudioCodecConfig struct {
    Enabled bool `yaml:"enabled"`
}

// Added to Config struct:
// AudioCodec AudioCodecConfig `yaml:"audio_codec"`
```

### YAML

```yaml
# configs/liveforge.yaml
audio_codec:
  enabled: true
  # Builtin codecs (always available, pure Go):
  #   pcmu, pcma, g722
  #
  # Optional codecs (require build tags):
  #   opus  (go build -tags audio_opus)
  #   aac   (go build -tags audio_aac)
```

When `audio_codec.enabled` is `false` (or the section is omitted), the TranscodeManager is not created on streams. Calls to `stream.TranscodeManager()` return nil, and protocol modules fall back to current behavior (no audio when codecs mismatch). This ensures zero runtime cost when transcoding is not wanted.

No configuration is needed for which transcodings to enable. The system automatically transcodes when a subscriber needs a different codec and both decoder and encoder are available in the registry.

## Stream Lifecycle

### Publisher Starts
- TranscodeManager is created (empty, no tracks).

### First WebRTC Subscriber Arrives (AAC→Opus needed)
- `GetOrCreateReader(CodecOpus)` called
- TranscodedTrack created, goroutine starts reading from source RingBuffer
- Returns reader on transcoded RingBuffer

### More WebRTC Subscribers Arrive
- `GetOrCreateReader(CodecOpus)` called
- Existing TranscodedTrack found, subCount incremented
- Returns new reader on same transcoded RingBuffer

### WebRTC Subscriber Leaves
- `release()` called (closure from GetOrCreateReader)
- subCount decremented

### Last WebRTC Subscriber Leaves
- subCount reaches 0
- Goroutine cancelled, TranscodedTrack removed
- No transcoding CPU cost when no one needs it

### Publisher Disconnects
- Source RingBuffer closed
- Transcode goroutines detect closed reader, exit
- All TranscodedTracks cleaned up

## Error Handling

| Condition | Behavior |
|-----------|----------|
| Target codec not in registry | `GetOrCreateReader` returns error; subscriber falls back to no-audio or rejects |
| Decode error on single frame | Frame skipped, log warning, continue |
| Encode error on single frame | Frame skipped, log warning, continue |
| Publisher disconnects mid-transcode | Source RingBuffer closes → goroutine exits cleanly |
| No publisher when subscriber arrives | `GetOrCreateReader` returns error (no MediaInfo to determine source codec) |

## Testing Strategy

### Unit Tests
- `pkg/audiocodec`: G.711 encode/decode round-trip with known ITU test vectors
- `pkg/audiocodec`: Registry registration, lookup, CanTranscode
- `core/TranscodeManager`: mock decoder/encoder, verify on-demand creation and cleanup

### Integration Tests
- Publish RTMP with AAC → subscribe via mock that requests G.711 → verify transcoded frames received
- Verify zero-overhead path: publish AAC → subscribe requesting AAC → verify same RingBuffer reader returned
- Verify cleanup: subscribe → unsubscribe → verify goroutine stopped and track removed

### Race Detection
- All tests run with `-race` flag
- TranscodeManager concurrent subscribe/unsubscribe stress test

## Implementation Phases

### Phase 1: Architecture + G.711 (Pure Go)
- `pkg/audiocodec`: interfaces, PCMFrame, Registry, G.711 PCMU/PCMA
- `core/transcode_manager.go`: TranscodeManager, TranscodedTrack, lifecycle management
- Config: `audio_codec` section in config loader
- Tests: unit + integration for G.711 round-trip and TranscodeManager lifecycle
- **Validation**: G.711U ↔ G.711A transcoding works end-to-end

### Phase 2: Opus (CGo, build tag)
- `pkg/audiocodec/opus.go` with `//go:build audio_opus`
- Depends on `gopkg.in/hraban/opus.v2` (CGo binding to libopus)
- Sample rate conversion: 8kHz/44.1kHz/48kHz ↔ 48kHz
- Integration with WebRTC WHEP subscriber
- **Validation**: RTMP(AAC) push → WebRTC(Opus) play with audio

### Phase 3: AAC (CGo, build tag)
- `pkg/audiocodec/aac.go` with `//go:build audio_aac`
- Depends on CGo binding to libfdk-aac
- **Validation**: WebRTC(Opus) push → RTMP(AAC) play with audio

## Non-Goals

- **Video transcoding**: out of scope. Video codecs (H.264, VP8, etc.) require GPU or FFmpeg. Not addressed here.
- **Audio mixing**: out of scope. Mixing N streams is the future Mixer Module's concern. AudioCodec provides the decode/encode primitives that Mixer will use.
- **SIP integration**: SIP will be in a separate Conference System. AudioCodec prepares the transcoding infrastructure it will need.
