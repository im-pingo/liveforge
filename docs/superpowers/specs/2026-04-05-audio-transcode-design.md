# Audio Transcoding Design Spec

## Overview

LiveForge currently cannot play audio across protocol boundaries when codecs differ. For example, an RTMP stream published with AAC produces no audio when played via WebRTC (which requires Opus), and vice versa. This spec updates and replaces the original `2026-04-02-audiocodec-design.md` with a unified FFmpeg-based approach that implements all audio codecs in a single step.

## Changes From Original Design

| Aspect | Original (2026-04-02) | This Spec |
|--------|----------------------|-----------|
| Backend | Pure Go G.711 + CGo Opus + CGo AAC | FFmpeg unified via go-astiav |
| Phases | 3 phases (G.711 → Opus → AAC) | Single phase, all codecs at once |
| Resample | Hand-written `resample()` function | FFmpeg swresample |
| Build tags | `//go:build audio_opus`, `//go:build audio_aac` | None — all codecs always available |
| Dependencies | zaf/g711, hraban/opus, go-fdkaac | go-astiav (single FFmpeg binding) |
| Build system | Standard `go build` | Makefile + pre-built FFmpeg static libs in repo |

**Unchanged**: Decoder/Encoder interfaces, PCMFrame, Registry, TranscodeManager, TranscodedTrack, shared RingBuffer, zero-overhead fast path, release closure pattern, configuration model.

## Problem Statement

Current behavior when codecs mismatch:

| Publisher | Subscriber | Audio Result |
|-----------|-----------|--------------|
| RTMP (AAC) | RTMP | Works (same codec) |
| RTMP (AAC) | WebRTC WHEP | **No audio** |
| WebRTC (Opus) | RTMP | **No audio** (FLV muxer expects AAC) |
| RTMP (AAC) | SIP (G.711) | **No audio** |
| Any | HLS/DASH (needs AAC) | **No audio** if publisher codec differs |

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
│  Created on-demand, destroyed when idle        │
├──────────────────────────────────────────────┤
│  pkg/audiocodec                               │
│  Decoder/Encoder interfaces + Registry        │
│  PCMFrame as universal exchange format        │
├──────────────────────────────────────────────┤
│  FFmpeg Backend (go-astiav)                   │
│  FFmpegDecoder / FFmpegEncoder / FFmpegResampler │
│  All codecs via libavcodec + libswresample    │
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
          │     goroutine: read AAC → FFmpegDecoder → PCM → FFmpegResampler
          │                → FFmpegEncoder(Opus) → write RingBuffer
          │     ├── WebRTC subscriber 1 (shared reader)
          │     └── WebRTC subscriber 2 (shared reader)
          │
          └── "pcmu" TranscodedTrack (created on first G.711 subscriber)
                goroutine: read AAC → FFmpegDecoder → PCM → FFmpegResampler
                           → FFmpegEncoder(G.711U) → write RingBuffer
                └── SIP subscriber
```

### Key Properties

- **Zero overhead when codecs match**: subscribers read directly from the original RingBuffer with no transcoding, no copies, no extra goroutines.
- **On-demand shared transcoding**: the first subscriber that needs a different codec triggers a transcode goroutine. Subsequent subscribers share the same transcoded output.
- **Video passthrough**: TranscodedTrack goroutines forward video frames as-is, only transcode audio. Subscribers use a single reader for both.

## Interfaces

### pkg/audiocodec/codec.go

```go
// PCMFrame is the universal exchange format between all audio codecs.
type PCMFrame struct {
    Samples    []int16 // interleaved samples (L,R,L,R... or mono)
    SampleRate int     // 8000, 16000, 44100, 48000
    Channels   int     // 1 or 2
}

// Decoder decodes compressed audio into PCM.
// Instances are NOT safe for concurrent use.
type Decoder interface {
    Decode(payload []byte) (*PCMFrame, error)
    SampleRate() int
    Channels() int
    Close()
}

// Encoder encodes PCM into compressed audio.
// Instances are NOT safe for concurrent use.
type Encoder interface {
    Encode(pcm *PCMFrame) ([]byte, error)
    SampleRate() int
    Channels() int
    FrameSize() int // samples per frame required by the encoder
    Close()
}

// SequenceHeaderFunc returns an initial sequence header frame for the
// target codec, or nil if the codec does not use sequence headers.
type SequenceHeaderFunc func() []byte
```

### pkg/audiocodec/registry.go

```go
type Registry struct {
    mu       sync.RWMutex
    decoders map[avframe.CodecType]DecoderFactory
    encoders map[avframe.CodecType]EncoderFactory
}

type DecoderFactory func() Decoder
type EncoderFactory func() Encoder
type SeqHeaderFactory func() SequenceHeaderFunc

func Global() *Registry
func (r *Registry) NewDecoder(codec avframe.CodecType) (Decoder, error)
func (r *Registry) NewEncoder(codec avframe.CodecType) (Encoder, error)
func (r *Registry) CanTranscode(from, to avframe.CodecType) bool
func (r *Registry) RegisterSequenceHeader(codec avframe.CodecType, fn SeqHeaderFactory)
func (r *Registry) SequenceHeader(codec avframe.CodecType) []byte
```

## FFmpeg Backend

### go-astiav Integration

go-astiav is a thin Go wrapper around FFmpeg's C API. Core object mapping:

| FFmpeg C | go-astiav Go |
|----------|-------------|
| AVCodecContext | astiav.CodecContext |
| AVFrame | astiav.Frame |
| AVPacket | astiav.Packet |
| SwrContext | astiav.SoftwareResampleContext |

Each Decoder/Encoder instance holds its own CodecContext — no sharing, no concurrency. This matches the design where each transcode goroutine creates its own instances.

### FFmpegDecoder

```go
// pkg/audiocodec/ff_decoder.go

type FFmpegDecoder struct {
    codecCtx   *astiav.CodecContext
    packet     *astiav.Packet
    frame      *astiav.Frame
    codecName  string
    sampleRate int
    channels   int
}

func NewFFmpegDecoder(codecName string) *FFmpegDecoder
func (d *FFmpegDecoder) Decode(payload []byte) (*PCMFrame, error)
func (d *FFmpegDecoder) SampleRate() int
func (d *FFmpegDecoder) Channels() int
func (d *FFmpegDecoder) Close()
```

### FFmpegEncoder

```go
// pkg/audiocodec/ff_encoder.go

type FFmpegEncoder struct {
    codecCtx   *astiav.CodecContext
    packet     *astiav.Packet
    frame      *astiav.Frame
    codecName  string
    sampleRate int
    channels   int
    frameSize  int
}

func NewFFmpegEncoder(codecName string, sampleRate, channels int) *FFmpegEncoder
func (e *FFmpegEncoder) Encode(pcm *PCMFrame) ([]byte, error)
func (e *FFmpegEncoder) SampleRate() int
func (e *FFmpegEncoder) Channels() int
func (e *FFmpegEncoder) FrameSize() int
func (e *FFmpegEncoder) Close()
```

### FFmpegResampler

```go
// pkg/audiocodec/ff_resampler.go

type FFmpegResampler struct {
    swrCtx   *astiav.SoftwareResampleContext
    outFrame *astiav.Frame
}

func NewFFmpegResampler(inRate, inCh, outRate, outCh int) *FFmpegResampler
func (r *FFmpegResampler) Resample(pcm *PCMFrame) *PCMFrame
func (r *FFmpegResampler) Close()
```

### Codec Registration

```go
// pkg/audiocodec/ff_register.go

var codecTable = []struct {
    typ     avframe.CodecType
    decName string
    encName string
    rate    int
    ch      int
}{
    {avframe.CodecAAC,   "aac",       "aac",       44100, 2},
    {avframe.CodecOpus,  "libopus",   "libopus",   48000, 2},
    {avframe.CodecMP3,   "mp3",       "libmp3lame", 44100, 2},
    {avframe.CodecG711U, "pcm_mulaw", "pcm_mulaw", 8000,  1},
    {avframe.CodecG711A, "pcm_alaw",  "pcm_alaw",  8000,  1},
    {avframe.CodecG722,  "g722",      "g722",      16000, 1},
    {avframe.CodecSpeex, "libspeex",  "libspeex",  8000,  1},
}

func init() {
    r := Global()
    for _, c := range codecTable {
        c := c
        r.RegisterDecoder(c.typ, func() Decoder {
            return NewFFmpegDecoder(c.decName)
        })
        r.RegisterEncoder(c.typ, func() Encoder {
            return NewFFmpegEncoder(c.encName, c.rate, c.ch)
        })
    }
    r.RegisterSequenceHeader(avframe.CodecAAC, func() SequenceHeaderFunc {
        return aacSequenceHeader
    })
}
```

## Build System

### Pre-built FFmpeg Static Libraries

Four platforms, committed to git (managed via Git LFS for `.a` files):

```
third_party/ffmpeg/
├── include/                     — headers (shared across platforms)
│   ├── libavcodec/
│   ├── libavutil/
│   └── libswresample/
├── lib/
│   ├── linux_amd64/             — ~4-5MB
│   │   ├── libavcodec.a
│   │   ├── libavutil.a
│   │   ├── libswresample.a
│   │   ├── libopus.a
│   │   ├── libmp3lame.a
│   │   └── libspeex.a
│   ├── linux_arm64/
│   ├── darwin_amd64/
│   └── darwin_arm64/
├── BUILD.md                     — source URLs and extraction steps (reproducible)
└── VERSION                      — FFmpeg version (e.g. "7.1")
```

Sources:
- Linux: BtbN/FFmpeg-Builds static GPL packages
- macOS: Extracted from Homebrew bottles

Only audio-related libraries included: libavcodec, libswresample, libavutil, plus external codec libs (libopus, libmp3lame, libspeex). Estimated total: ~16-20MB across four platforms.

### CGo Linking

```go
// pkg/audiocodec/ff_cgo.go
package audiocodec

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/ffmpeg/include

#cgo darwin,amd64  LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/darwin_amd64
#cgo darwin,arm64  LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/darwin_arm64
#cgo linux,amd64   LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/linux_amd64
#cgo linux,arm64   LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/linux_arm64

#cgo LDFLAGS: -lavcodec -lswresample -lavutil -lopus -lmp3lame -lspeex -lm -lpthread
#cgo linux LDFLAGS: -ldl
*/
import "C"
```

### Makefile

```makefile
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test clean

build:
	CGO_ENABLED=1 go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

test:
	CGO_ENABLED=1 go test -race -cover ./...

clean:
	rm -rf bin/
```

### Single Binary Guarantee

- Console HTML/JS/CSS: already embedded via `go:embed` in `module/api/console.go`
- FFmpeg: statically linked via `.a` files — no runtime shared library dependency
- Output: one self-contained binary with zero external dependencies

Developer experience:

```bash
git clone ...     # includes FFmpeg static libs (via LFS)
make build        # single binary output
```

## Audio-Video Sync

### Problem

Transcoding introduces two sync-breaking factors:

1. **Frame size mismatch**: AAC decodes 1024 samples, Opus encodes 960 samples. A frame accumulation buffer is needed, which shifts timing if source timestamps are reused naively.
2. **Resample ratio rounding**: 44100→48000 is an irrational ratio (48000/44100 ≈ 1.08844...). Long-running streams accumulate rounding error.

### Solution: Sample-Count Based Timestamp Tracking

The transcode goroutine does NOT reuse source frame DTS/PTS. Instead it maintains an independent timeline anchored to the first source audio frame, calculated from actual encoder output sample counts:

```go
// pkg/audiocodec/ts_tracker.go

type tsTracker struct {
    baseTime       int64 // first source audio frame DTS (ms)
    samplesEncoded int64 // cumulative samples output by encoder
    sampleRate     int   // encoder sample rate
}

func (t *tsTracker) init(firstFrameDTS int64, sampleRate int) {
    t.baseTime = firstFrameDTS
    t.sampleRate = sampleRate
}

func (t *tsTracker) next(frameSamples int) int64 {
    dts := t.baseTime + (t.samplesEncoded * 1000) / int64(t.sampleRate)
    t.samplesEncoded += int64(frameSamples)
    return dts
}
```

### Precision

Integer arithmetic avoids floating-point accumulation:
- 48000Hz running 24 hours = 4,147,200,000 samples
- `4147200000 * 1000 / 48000 = 86,400,000 ms` — exact, int64 no overflow
- Maximum error < 1ms (integer truncation), imperceptible

### Video Sync

Video frames pass through unchanged with original DTS/PTS. Audio timestamps are anchored to the same source stream's first audio frame DTS. Both timelines share the same origin, so A/V sync is preserved.

## TranscodeManager

### Core Method: GetOrCreateReader (unchanged from original design)

```go
func (tm *TranscodeManager) GetOrCreateReader(
    targetCodec avframe.CodecType,
) (reader *util.RingReader[*avframe.AVFrame], release func(), err error)
```

Behavior:
1. Publisher nil → return error
2. Source codec == target codec → return original RingBuffer reader + no-op release (zero overhead)
3. Registry cannot transcode → return error
4. TranscodedTrack exists → increment subCount, return new reader
5. Otherwise → create TranscodedTrack, start goroutine, return reader

### Transcode Goroutine (updated)

```go
func (tm *TranscodeManager) transcodeLoop(ctx context.Context, track *TranscodedTrack) {
    sourceCodec := tm.stream.Publisher().MediaInfo().AudioCodec

    decoder, _ := tm.registry.NewDecoder(sourceCodec)
    encoder, _ := tm.registry.NewEncoder(track.targetCodec)
    defer decoder.Close()
    defer encoder.Close()

    var resampler *FFmpegResampler
    if decoder.SampleRate() != encoder.SampleRate() ||
       decoder.Channels() != encoder.Channels() {
        resampler = NewFFmpegResampler(
            decoder.SampleRate(), decoder.Channels(),
            encoder.SampleRate(), encoder.Channels(),
        )
        defer resampler.Close()
    }

    // Emit sequence header for target codec
    if seqHeader := tm.registry.SequenceHeader(track.targetCodec); seqHeader != nil {
        track.ringBuffer.Write(avframe.NewAVFrame(
            avframe.MediaTypeAudio, track.targetCodec,
            avframe.FrameTypeSequenceHeader, 0, 0, seqHeader,
        ))
    }

    var ts tsTracker
    tsInited := false
    var pcmBuf []int16
    frameSize := encoder.FrameSize() * encoder.Channels()

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

        // Video: passthrough unchanged
        if frame.MediaType.IsVideo() {
            track.ringBuffer.Write(frame)
            continue
        }

        // Skip source sequence headers
        if frame.FrameType == avframe.FrameTypeSequenceHeader {
            continue
        }

        // Anchor timestamp to first audio frame
        if !tsInited {
            ts.init(frame.DTS, encoder.SampleRate())
            tsInited = true
        }

        // Decode
        pcm, err := decoder.Decode(frame.Payload)
        if err != nil {
            continue
        }

        // Resample
        if resampler != nil {
            pcm = resampler.Resample(pcm)
        }

        // Accumulate and encode in encoder-sized chunks
        pcmBuf = append(pcmBuf, pcm.Samples...)
        for len(pcmBuf) >= frameSize {
            chunk := &PCMFrame{
                Samples:    pcmBuf[:frameSize],
                SampleRate: encoder.SampleRate(),
                Channels:   encoder.Channels(),
            }
            encoded, err := encoder.Encode(chunk)
            if err != nil {
                pcmBuf = pcmBuf[frameSize:]
                continue
            }

            dts := ts.next(encoder.FrameSize())

            track.ringBuffer.Write(avframe.NewAVFrame(
                avframe.MediaTypeAudio, track.targetCodec,
                avframe.FrameTypeInterframe,
                dts, dts,
                encoded,
            ))
            pcmBuf = pcmBuf[frameSize:]
        }
    }
}
```

### Cleanup (unchanged)

Release closure calls `releaseTrack()` which decrements subCount. When subCount reaches 0, the goroutine is cancelled and the track is deleted.

`Reset()` is called from `Stream.SetPublisher()` when a new publisher replaces the old one. All active TranscodedTracks are cancelled and removed.

## Configuration

```go
// config/config.go
type AudioCodecConfig struct {
    Enabled bool `yaml:"enabled"`
}
```

```yaml
audio_codec:
  enabled: true
```

When `enabled: false`, TranscodeManager is not created on streams. Protocol modules fall back to current behavior (no audio when codecs mismatch).

## Protocol Integration

### Target Codec per Protocol

| Protocol | Container | Required Audio Codec | Transcode When |
|----------|-----------|---------------------|----------------|
| RTMP | FLV | AAC, MP3 | Publisher is Opus/G.711 |
| WebRTC WHEP | RTP | Opus | Publisher is AAC/G.711 |
| HLS (TS) | MPEG-TS | AAC, MP3 | Publisher is Opus/G.711 |
| HLS (fMP4) | fMP4 | AAC, Opus, MP3 | Publisher is G.711 |
| DASH | fMP4 | AAC, Opus, MP3 | Publisher is G.711 |
| HTTP-FLV | FLV | AAC, MP3 | Publisher is Opus/G.711 |
| RTSP | RTP | All | Usually no transcode needed |
| SRT | MPEG-TS | AAC, MP3 | Publisher is Opus/G.711 |
| SIP | RTP | G.711, Opus | Publisher is AAC |

### Integration Pattern

All protocol modules follow the same pattern:

```go
reader, release, err := stream.TranscodeManager().GetOrCreateReader(targetCodec)
if err != nil {
    // TranscodeManager disabled or codec unavailable — fall back
    reader = stream.RingBuffer().NewReader()
    release = func() {}
}
defer release()
// reader produces AVFrames with transcoded audio + original video
```

## Testing Strategy

### Unit Tests — pkg/audiocodec/

| File | Tests |
|------|-------|
| ff_decoder_test.go | Each codec: decode known payload → verify PCMFrame sample rate, channels, non-empty samples |
| ff_encoder_test.go | Each codec: encode PCMFrame → verify non-empty output, decodable back to PCM |
| ff_resampler_test.go | 8kHz→48kHz, 48kHz→44100Hz, verify output sample count matches ratio |
| ff_roundtrip_test.go | All (src, dst) codec pairs: encode→decode→compare PCM SNR |
| ff_register_test.go | Registry correctness, CanTranscode matrix |
| ts_tracker_test.go | Simulate 24-hour stream, verify drift < 1ms |

### Unit Tests — core/

| File | Tests |
|------|-------|
| transcode_manager_test.go | Zero-overhead path: codec match → original RingBuffer reader |
| | Transcode path: codec mismatch → TranscodedTrack created |
| | Sharing: two subscribers same codec → shared TranscodedTrack |
| | Release: last subscriber release → goroutine stops, track deleted |
| | Reset: publisher change → all tracks cleaned up |
| | Frame accumulation: AAC(1024)→Opus(960) frame size mismatch handled |

### Integration Tests — test/integration/

| File | Tests |
|------|-------|
| transcode_rtmp_webrtc_test.go | RTMP publisher (AAC) → WebRTC subscriber → verify Opus frames received |
| transcode_webrtc_rtmp_test.go | WebRTC publisher (Opus) → RTMP subscriber → verify AAC frames received |
| transcode_avsync_test.go | Known-timestamp A/V frames → verify TranscodedTrack output A/V DTS delta ≤ 1ms |

All tests run with `-race` flag.

## File Inventory

### New Files

```
pkg/audiocodec/
├── codec.go
├── registry.go
├── ff_cgo.go
├── ff_decoder.go
├── ff_encoder.go
├── ff_resampler.go
├── ff_register.go
├── ts_tracker.go
├── ff_decoder_test.go
├── ff_encoder_test.go
├── ff_resampler_test.go
├── ff_roundtrip_test.go
├── ff_register_test.go
└── ts_tracker_test.go

core/
├── transcode_manager.go
└── transcode_manager_test.go

test/integration/
├── transcode_rtmp_webrtc_test.go
├── transcode_webrtc_rtmp_test.go
└── transcode_avsync_test.go

third_party/ffmpeg/
├── include/
├── lib/{linux_amd64,linux_arm64,darwin_amd64,darwin_arm64}/
├── BUILD.md
└── VERSION

Makefile
.gitattributes                  — *.a filter=lfs
```

### Modified Files

```
config/config.go                — add AudioCodecConfig struct
config/loader.go                — load audio_codec section
core/stream.go                  — add transcodeManager field, call Reset in SetPublisher
go.mod                          — add github.com/asticode/go-astiav

Protocol module integration (by priority):
  module/webrtc/whep_feed.go    — use TranscodeManager.GetOrCreateReader
  module/rtmp/subscriber.go     — same
  core/muxer_manager.go         — same (affects HLS/DASH/HTTP-FLV/SRT)
```

## Non-Goals

- **Video transcoding**: out of scope. Video codecs require GPU or heavy CPU. Not addressed here.
- **Audio mixing**: out of scope. Mixing N streams is a future Mixer Module concern. AudioCodec provides the decode/encode primitives it will use.
- **G.729 encoding**: G.729 is patent-encumbered. FFmpeg supports decode but not encode. Decode-only is registered; encode attempts return a registry error.
