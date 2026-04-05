# Audio Transcoding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable on-demand, shared audio transcoding between all protocols so any subscriber receives audio in its native codec regardless of publisher codec.

**Architecture:** FFmpeg unified backend via go-astiav wrapping libavcodec + libswresample. Registry abstraction (Decoder/Encoder interfaces, PCMFrame exchange format) decouples codec backend from TranscodeManager. TranscodeManager per stream creates shared TranscodedTracks on demand, zero overhead when codecs match.

**Tech Stack:** Go 1.24+, go-astiav (FFmpeg CGo binding), pre-built FFmpeg 7.1 static libraries

**Spec:** `docs/superpowers/specs/2026-04-05-audio-transcode-design.md`

---

## File Structure

### New files

```
pkg/audiocodec/
├── codec.go              — PCMFrame, Decoder, Encoder interfaces
├── registry.go           — Registry singleton, codec factory management
├── registry_test.go      — Registry unit tests
├── ts_tracker.go         — Sample-count timestamp tracker for A/V sync
├── ts_tracker_test.go    — 24-hour drift test
├── ff_cgo.go             — CGo CFLAGS/LDFLAGS directives (no Go logic)
├── ff_decoder.go         — FFmpegDecoder: go-astiav decode wrapper
├── ff_decoder_test.go    — Per-codec decode tests
├── ff_encoder.go         — FFmpegEncoder: go-astiav encode wrapper
├── ff_encoder_test.go    — Per-codec encode tests
├── ff_resampler.go       — FFmpegResampler: swresample wrapper
├── ff_resampler_test.go  — Sample rate conversion tests
├── ff_register.go        — init() registering all codecs
├── ff_register_test.go   — Registry completeness tests
├── ff_roundtrip_test.go  — Encode→decode round-trip across codec pairs

core/
├── transcode_manager.go      — TranscodeManager, TranscodedTrack, transcodeLoop
├── transcode_manager_test.go — Lifecycle, sharing, cleanup, frame accumulation tests

test/integration/
├── transcode_avsync_test.go      — A/V sync verification
├── transcode_rtmp_webrtc_test.go — RTMP→WebRTC cross-protocol test
├── transcode_webrtc_rtmp_test.go — WebRTC→RTMP cross-protocol test

third_party/ffmpeg/
├── include/                   — FFmpeg headers
├── lib/{linux_amd64,linux_arm64,darwin_amd64,darwin_arm64}/  — static libs
├── BUILD.md                   — Provenance and extraction steps
└── VERSION                    — "7.1"

Makefile
.gitattributes
```

### Modified files

```
go.mod                    — add go-astiav dependency
config/config.go:6-25     — add AudioCodecConfig struct + field on Config
config/loader.go:33-128   — add AudioCodec default in defaults()
core/stream.go:82-121     — add transcodeManager field + init in NewStream + accessor
core/stream.go:140-166    — call TranscodeManager.Reset() in SetPublisher
```

---

## Task 1: Build system — FFmpeg libs + Makefile + go-astiav dependency

**Files:**
- Create: `third_party/ffmpeg/VERSION`
- Create: `third_party/ffmpeg/BUILD.md`
- Create: `third_party/ffmpeg/include/` (FFmpeg headers)
- Create: `third_party/ffmpeg/lib/darwin_arm64/` (or current platform)
- Create: `Makefile`
- Create: `.gitattributes`
- Create: `pkg/audiocodec/ff_cgo.go`
- Modify: `go.mod`

This task sets up the build foundation. All subsequent tasks depend on CGo linking working.

- [ ] **Step 1: Create `.gitattributes` for LFS tracking**

```
# .gitattributes
third_party/ffmpeg/lib/**/*.a filter=lfs diff=lfs merge=lfs -text
```

- [ ] **Step 2: Set up Git LFS**

Run: `git lfs install && git lfs track "third_party/ffmpeg/lib/**/*.a"`
Expected: LFS configured

- [ ] **Step 3: Download and extract FFmpeg static libraries for current platform**

For macOS arm64 (development machine):

```bash
# Extract from Homebrew
brew install ffmpeg
mkdir -p third_party/ffmpeg/lib/darwin_arm64
mkdir -p third_party/ffmpeg/include

# Copy static libs (only the ones we need)
cp $(brew --prefix)/lib/libavcodec.a third_party/ffmpeg/lib/darwin_arm64/
cp $(brew --prefix)/lib/libavutil.a third_party/ffmpeg/lib/darwin_arm64/
cp $(brew --prefix)/lib/libswresample.a third_party/ffmpeg/lib/darwin_arm64/
cp $(brew --prefix)/lib/libopus.a third_party/ffmpeg/lib/darwin_arm64/
cp $(brew --prefix)/lib/libmp3lame.a third_party/ffmpeg/lib/darwin_arm64/
cp $(brew --prefix)/lib/libspeex.a third_party/ffmpeg/lib/darwin_arm64/

# Copy headers
cp -r $(brew --prefix)/include/libavcodec third_party/ffmpeg/include/
cp -r $(brew --prefix)/include/libavutil third_party/ffmpeg/include/
cp -r $(brew --prefix)/include/libswresample third_party/ffmpeg/include/
```

Note: Other platforms (linux_amd64, linux_arm64, darwin_amd64) are added later. Start with the dev machine platform to unblock implementation.

- [ ] **Step 4: Create `third_party/ffmpeg/VERSION`**

```
7.1
```

- [ ] **Step 5: Create `third_party/ffmpeg/BUILD.md`**

Document the exact source URLs and extraction steps for each platform. This file ensures reproducibility. Content: where each `.a` came from, brew version, BtbN release URL for Linux, extraction commands.

- [ ] **Step 6: Create `pkg/audiocodec/ff_cgo.go`**

```go
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

- [ ] **Step 7: Add go-astiav dependency**

Run: `go get github.com/asticode/go-astiav@latest && go mod tidy`
Expected: `go.mod` updated with go-astiav

- [ ] **Step 8: Create `Makefile`**

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

- [ ] **Step 9: Verify CGo linking works**

Create a minimal Go file that imports the `audiocodec` package to test linking:

Run: `CGO_ENABLED=1 go build ./pkg/audiocodec/`
Expected: Build succeeds with no errors

- [ ] **Step 10: Commit**

```bash
git add .gitattributes Makefile third_party/ pkg/audiocodec/ff_cgo.go go.mod go.sum
git commit -m "chore: add FFmpeg static libs, Makefile, and go-astiav dependency"
```

---

## Task 2: Interfaces — PCMFrame, Decoder, Encoder, Registry

**Files:**
- Create: `pkg/audiocodec/codec.go`
- Create: `pkg/audiocodec/registry.go`
- Create: `pkg/audiocodec/registry_test.go`

Pure Go, no FFmpeg dependency. These are the stable abstractions the rest of the system depends on.

- [ ] **Step 1: Write Registry tests**

```go
// pkg/audiocodec/registry_test.go
package audiocodec

import (
    "testing"

    "github.com/im-pingo/liveforge/pkg/avframe"
)

// mockDecoder / mockEncoder implement the interfaces for testing.
type mockDecoder struct{ rate, ch int }

func (m *mockDecoder) SetExtradata([]byte) {}
func (m *mockDecoder) Decode([]byte) (*PCMFrame, error) {
    return &PCMFrame{Samples: []int16{0}, SampleRate: m.rate, Channels: m.ch}, nil
}
func (m *mockDecoder) SampleRate() int { return m.rate }
func (m *mockDecoder) Channels() int   { return m.ch }
func (m *mockDecoder) Close()          {}

type mockEncoder struct{ rate, ch, fs int }

func (m *mockEncoder) Encode(*PCMFrame) ([]byte, error) { return []byte{0}, nil }
func (m *mockEncoder) SampleRate() int                   { return m.rate }
func (m *mockEncoder) Channels() int                     { return m.ch }
func (m *mockEncoder) FrameSize() int                    { return m.fs }
func (m *mockEncoder) Close()                            {}

func TestRegistryNewDecoder(t *testing.T) {
    r := &Registry{
        decoders: make(map[avframe.CodecType]DecoderFactory),
        encoders: make(map[avframe.CodecType]EncoderFactory),
    }
    r.RegisterDecoder(avframe.CodecAAC, func() Decoder {
        return &mockDecoder{rate: 44100, ch: 2}
    })

    dec, err := r.NewDecoder(avframe.CodecAAC)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if dec.SampleRate() != 44100 {
        t.Fatalf("expected 44100, got %d", dec.SampleRate())
    }
    dec.Close()

    _, err = r.NewDecoder(avframe.CodecOpus)
    if err == nil {
        t.Fatal("expected error for unregistered codec")
    }
}

func TestRegistryNewEncoder(t *testing.T) {
    r := &Registry{
        decoders: make(map[avframe.CodecType]DecoderFactory),
        encoders: make(map[avframe.CodecType]EncoderFactory),
    }
    r.RegisterEncoder(avframe.CodecOpus, func() Encoder {
        return &mockEncoder{rate: 48000, ch: 2, fs: 960}
    })

    enc, err := r.NewEncoder(avframe.CodecOpus)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if enc.FrameSize() != 960 {
        t.Fatalf("expected 960, got %d", enc.FrameSize())
    }
    enc.Close()
}

func TestRegistryCanTranscode(t *testing.T) {
    r := &Registry{
        decoders: make(map[avframe.CodecType]DecoderFactory),
        encoders: make(map[avframe.CodecType]EncoderFactory),
    }
    r.RegisterDecoder(avframe.CodecAAC, func() Decoder { return &mockDecoder{} })
    r.RegisterEncoder(avframe.CodecOpus, func() Encoder { return &mockEncoder{} })

    if !r.CanTranscode(avframe.CodecAAC, avframe.CodecOpus) {
        t.Fatal("expected CanTranscode(AAC, Opus) = true")
    }
    if r.CanTranscode(avframe.CodecOpus, avframe.CodecAAC) {
        t.Fatal("expected CanTranscode(Opus, AAC) = false (no Opus decoder)")
    }
}

func TestRegistrySequenceHeader(t *testing.T) {
    r := &Registry{
        decoders: make(map[avframe.CodecType]DecoderFactory),
        encoders: make(map[avframe.CodecType]EncoderFactory),
        seqHdrs:  make(map[avframe.CodecType]SeqHeaderFactory),
    }
    r.RegisterSequenceHeader(avframe.CodecAAC, func() SequenceHeaderFunc {
        return func() []byte { return []byte{0x12, 0x10} }
    })

    hdr := r.SequenceHeader(avframe.CodecAAC)
    if len(hdr) != 2 {
        t.Fatalf("expected 2 bytes, got %d", len(hdr))
    }
    if r.SequenceHeader(avframe.CodecOpus) != nil {
        t.Fatal("expected nil for Opus sequence header")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestRegistry`
Expected: FAIL — package does not exist yet

- [ ] **Step 3: Implement `pkg/audiocodec/codec.go`**

```go
package audiocodec

// PCMFrame is the universal exchange format between all audio codecs.
type PCMFrame struct {
    Samples    []int16 // interleaved samples (L,R,L,R... or mono)
    SampleRate int     // 8000, 16000, 44100, 48000
    Channels   int     // 1 or 2
}

// Decoder decodes compressed audio into PCM.
// Instances are NOT safe for concurrent use.
type Decoder interface {
    SetExtradata(data []byte)
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
    FrameSize() int
    Close()
}

// SequenceHeaderFunc returns an initial sequence header frame for the
// target codec, or nil if the codec does not use sequence headers.
type SequenceHeaderFunc func() []byte
```

- [ ] **Step 4: Implement `pkg/audiocodec/registry.go`**

```go
package audiocodec

import (
    "fmt"
    "sync"

    "github.com/im-pingo/liveforge/pkg/avframe"
)

// DecoderFactory creates a new Decoder instance.
type DecoderFactory func() Decoder

// EncoderFactory creates a new Encoder instance.
type EncoderFactory func() Encoder

// SeqHeaderFactory creates a SequenceHeaderFunc for a codec.
type SeqHeaderFactory func() SequenceHeaderFunc

// Registry manages available audio codecs.
type Registry struct {
    mu       sync.RWMutex
    decoders map[avframe.CodecType]DecoderFactory
    encoders map[avframe.CodecType]EncoderFactory
    seqHdrs  map[avframe.CodecType]SeqHeaderFactory
}

var (
    globalRegistry     *Registry
    globalRegistryOnce sync.Once
)

// Global returns the process-wide codec registry.
func Global() *Registry {
    globalRegistryOnce.Do(func() {
        globalRegistry = &Registry{
            decoders: make(map[avframe.CodecType]DecoderFactory),
            encoders: make(map[avframe.CodecType]EncoderFactory),
            seqHdrs:  make(map[avframe.CodecType]SeqHeaderFactory),
        }
    })
    return globalRegistry
}

func (r *Registry) RegisterDecoder(codec avframe.CodecType, f DecoderFactory) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.decoders[codec] = f
}

func (r *Registry) RegisterEncoder(codec avframe.CodecType, f EncoderFactory) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.encoders[codec] = f
}

func (r *Registry) RegisterSequenceHeader(codec avframe.CodecType, fn SeqHeaderFactory) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.seqHdrs[codec] = fn
}

func (r *Registry) NewDecoder(codec avframe.CodecType) (Decoder, error) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    f, ok := r.decoders[codec]
    if !ok {
        return nil, fmt.Errorf("audiocodec: no decoder registered for %s", codec)
    }
    return f(), nil
}

func (r *Registry) NewEncoder(codec avframe.CodecType) (Encoder, error) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    f, ok := r.encoders[codec]
    if !ok {
        return nil, fmt.Errorf("audiocodec: no encoder registered for %s", codec)
    }
    return f(), nil
}

func (r *Registry) CanTranscode(from, to avframe.CodecType) bool {
    r.mu.RLock()
    defer r.mu.RUnlock()
    _, hasDec := r.decoders[from]
    _, hasEnc := r.encoders[to]
    return hasDec && hasEnc
}

func (r *Registry) SequenceHeader(codec avframe.CodecType) []byte {
    r.mu.RLock()
    defer r.mu.RUnlock()
    fn, ok := r.seqHdrs[codec]
    if !ok {
        return nil
    }
    shf := fn()
    if shf == nil {
        return nil
    }
    return shf()
}
```

- [ ] **Step 5: Run tests**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestRegistry -race`
Expected: All 4 tests PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/audiocodec/codec.go pkg/audiocodec/registry.go pkg/audiocodec/registry_test.go
git commit -m "feat: add audiocodec interfaces and registry"
```

---

## Task 3: Timestamp tracker

**Files:**
- Create: `pkg/audiocodec/ts_tracker.go`
- Create: `pkg/audiocodec/ts_tracker_test.go`

Pure Go, no FFmpeg dependency. Small, self-contained unit.

- [ ] **Step 1: Write tests**

```go
// pkg/audiocodec/ts_tracker_test.go
package audiocodec

import "testing"

func TestTsTrackerBasic(t *testing.T) {
    var ts tsTracker
    ts.init(1000, 48000) // base=1000ms, 48kHz

    // First frame: 960 samples (20ms)
    dts := ts.next(960)
    if dts != 1000 {
        t.Fatalf("expected 1000, got %d", dts)
    }

    // Second frame: should be 1000 + 960*1000/48000 = 1020
    dts = ts.next(960)
    if dts != 1020 {
        t.Fatalf("expected 1020, got %d", dts)
    }

    // Third frame: 1000 + 1920*1000/48000 = 1040
    dts = ts.next(960)
    if dts != 1040 {
        t.Fatalf("expected 1040, got %d", dts)
    }
}

func TestTsTracker24HourDrift(t *testing.T) {
    var ts tsTracker
    ts.init(0, 48000)

    // Simulate 24 hours at 20ms per frame = 4,320,000 frames
    frames := 24 * 60 * 60 * 50 // 50 frames/sec
    for i := 0; i < frames; i++ {
        ts.next(960)
    }
    // After 24h: expected = 86,400,000 ms
    finalDts := ts.next(960)
    expected := int64(86_400_000)
    drift := finalDts - expected
    if drift < -1 || drift > 1 {
        t.Fatalf("drift after 24h: %dms (expected ≤1ms)", drift)
    }
}

func TestTsTracker8kHz(t *testing.T) {
    var ts tsTracker
    ts.init(500, 8000) // G.711: 8kHz

    // 160 samples = 20ms at 8kHz
    dts := ts.next(160)
    if dts != 500 {
        t.Fatalf("expected 500, got %d", dts)
    }
    dts = ts.next(160)
    if dts != 520 {
        t.Fatalf("expected 520, got %d", dts)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestTsTracker`
Expected: FAIL — tsTracker not defined

- [ ] **Step 3: Implement `pkg/audiocodec/ts_tracker.go`**

```go
package audiocodec

// tsTracker maintains an independent audio timeline anchored to the first
// source frame's DTS. Timestamps are derived from cumulative encoder output
// sample counts, ensuring precise A/V sync without floating-point drift.
type tsTracker struct {
    baseTime       int64
    samplesEncoded int64
    sampleRate     int
}

func (t *tsTracker) init(firstFrameDTS int64, sampleRate int) {
    t.baseTime = firstFrameDTS
    t.sampleRate = sampleRate
    t.samplesEncoded = 0
}

func (t *tsTracker) next(frameSamples int) int64 {
    dts := t.baseTime + (t.samplesEncoded*1000)/int64(t.sampleRate)
    t.samplesEncoded += int64(frameSamples)
    return dts
}
```

- [ ] **Step 4: Run tests**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestTsTracker -race`
Expected: All 3 tests PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/audiocodec/ts_tracker.go pkg/audiocodec/ts_tracker_test.go
git commit -m "feat: add sample-count timestamp tracker for A/V sync"
```

---

## Task 4: FFmpegDecoder

**Files:**
- Create: `pkg/audiocodec/ff_decoder.go`
- Create: `pkg/audiocodec/ff_decoder_test.go`

First file that uses go-astiav CGo. Tests verify decode produces valid PCM for known payloads.

- [ ] **Step 1: Write tests**

Test strategy: use FFmpegEncoder (which we haven't built yet) to create known payloads is a chicken-and-egg problem. Instead, test the simplest codec first — G.711 PCMU, where the payload is trivially constructible (raw mu-law bytes, one byte per sample).

```go
// pkg/audiocodec/ff_decoder_test.go
package audiocodec

import "testing"

func TestFFmpegDecoderPCMU(t *testing.T) {
    dec := NewFFmpegDecoder("pcm_mulaw")
    defer dec.Close()

    if dec.SampleRate() != 8000 {
        t.Fatalf("expected 8000, got %d", dec.SampleRate())
    }

    // G.711 PCMU: 160 bytes = 160 samples = 20ms at 8kHz
    // 0xFF = mu-law silence
    payload := make([]byte, 160)
    for i := range payload {
        payload[i] = 0xFF
    }

    pcm, err := dec.Decode(payload)
    if err != nil {
        t.Fatalf("decode error: %v", err)
    }
    if len(pcm.Samples) != 160 {
        t.Fatalf("expected 160 samples, got %d", len(pcm.Samples))
    }
    if pcm.SampleRate != 8000 {
        t.Fatalf("expected sample rate 8000, got %d", pcm.SampleRate)
    }
    if pcm.Channels != 1 {
        t.Fatalf("expected 1 channel, got %d", pcm.Channels)
    }
}

func TestFFmpegDecoderPCMA(t *testing.T) {
    dec := NewFFmpegDecoder("pcm_alaw")
    defer dec.Close()

    // 0xD5 = A-law silence
    payload := make([]byte, 160)
    for i := range payload {
        payload[i] = 0xD5
    }

    pcm, err := dec.Decode(payload)
    if err != nil {
        t.Fatalf("decode error: %v", err)
    }
    if len(pcm.Samples) != 160 {
        t.Fatalf("expected 160 samples, got %d", len(pcm.Samples))
    }
}

func TestFFmpegDecoderSetExtradataNoOp(t *testing.T) {
    dec := NewFFmpegDecoder("pcm_mulaw")
    defer dec.Close()
    // SetExtradata should be a no-op for non-AAC codecs — just ensure no panic
    dec.SetExtradata([]byte{0x12, 0x10})
}

// TestFFmpegDecoderAAC verifies AAC decode works when extradata (AudioSpecificConfig) is set.
// This is the most critical codec — it's what RTMP publishes.
func TestFFmpegDecoderAAC(t *testing.T) {
    // First, encode some PCM to AAC to get a valid AAC frame
    enc := NewFFmpegEncoder("aac", 44100, 2)
    defer enc.Close()

    // Generate 1024 samples of silence (AAC frame size)
    pcm := &PCMFrame{
        Samples:    make([]int16, 1024*2), // 1024 samples * 2 channels
        SampleRate: 44100,
        Channels:   2,
    }
    aacPayload, err := enc.Encode(pcm)
    if err != nil {
        t.Fatalf("encode AAC: %v", err)
    }

    // Now decode it back
    dec := NewFFmpegDecoder("aac")
    defer dec.Close()

    // AAC requires AudioSpecificConfig extradata: 0x12 0x10 = AAC-LC 44100Hz stereo
    dec.SetExtradata([]byte{0x12, 0x10})

    decoded, err := dec.Decode(aacPayload)
    if err != nil {
        t.Fatalf("decode AAC: %v", err)
    }
    if decoded.SampleRate != 44100 {
        t.Fatalf("expected 44100, got %d", decoded.SampleRate)
    }
    if decoded.Channels != 2 {
        t.Fatalf("expected 2 channels, got %d", decoded.Channels)
    }
    if len(decoded.Samples) < 1024 {
        t.Fatalf("expected at least 1024 samples, got %d", len(decoded.Samples))
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestFFmpegDecoder`
Expected: FAIL — NewFFmpegDecoder not defined

- [ ] **Step 3: Implement `pkg/audiocodec/ff_decoder.go`**

Implement `FFmpegDecoder` using go-astiav:
- `NewFFmpegDecoder(codecName)`: find decoder by name, alloc codec context, open it, alloc packet + frame
- `SetExtradata(data)`: set `codecCtx.SetExtradata(data)` — needed for AAC
- `Decode(payload)`: set packet data, send packet, receive frame, extract int16 samples into PCMFrame
- `SampleRate()`, `Channels()`: return values from codec context after open
- `Close()`: free codec context, packet, frame

Key go-astiav API calls:
- `astiav.FindDecoderByName(name)` → `*astiav.Codec`
- `astiav.AllocCodecContext(codec)` → `*astiav.CodecContext`
- `codecCtx.Open(codec, nil)` → error
- `codecCtx.SendPacket(pkt)` / `codecCtx.ReceiveFrame(frame)` → decode loop
- `frame.Data()` → raw sample data (must interpret based on sample format)

Important: FFmpeg decodes to its native sample format (e.g., `AV_SAMPLE_FMT_S16` for PCM, `AV_SAMPLE_FMT_FLTP` for AAC). For non-S16 formats, convert samples to int16 before returning PCMFrame. Use `frame.SampleFormat()` to detect and handle.

- [ ] **Step 4: Run tests**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestFFmpegDecoder -race`
Expected: All 3 tests PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/audiocodec/ff_decoder.go pkg/audiocodec/ff_decoder_test.go
git commit -m "feat: add FFmpegDecoder with go-astiav"
```

---

## Task 5: FFmpegEncoder

**Files:**
- Create: `pkg/audiocodec/ff_encoder.go`
- Create: `pkg/audiocodec/ff_encoder_test.go`

- [ ] **Step 1: Write tests**

```go
// pkg/audiocodec/ff_encoder_test.go
package audiocodec

import "testing"

func TestFFmpegEncoderPCMU(t *testing.T) {
    enc := NewFFmpegEncoder("pcm_mulaw", 8000, 1)
    defer enc.Close()

    if enc.SampleRate() != 8000 {
        t.Fatalf("expected 8000, got %d", enc.SampleRate())
    }
    if enc.Channels() != 1 {
        t.Fatalf("expected 1, got %d", enc.Channels())
    }

    // Encode 160 samples of silence
    pcm := &PCMFrame{
        Samples:    make([]int16, 160),
        SampleRate: 8000,
        Channels:   1,
    }
    payload, err := enc.Encode(pcm)
    if err != nil {
        t.Fatalf("encode error: %v", err)
    }
    if len(payload) != 160 {
        t.Fatalf("expected 160 bytes, got %d", len(payload))
    }
}

func TestFFmpegEncoderDecodeRoundTrip(t *testing.T) {
    enc := NewFFmpegEncoder("pcm_mulaw", 8000, 1)
    defer enc.Close()
    dec := NewFFmpegDecoder("pcm_mulaw")
    defer dec.Close()

    // Create a known signal: 160 samples of a ramp
    original := &PCMFrame{
        Samples:    make([]int16, 160),
        SampleRate: 8000,
        Channels:   1,
    }
    for i := range original.Samples {
        original.Samples[i] = int16(i * 100)
    }

    encoded, err := enc.Encode(original)
    if err != nil {
        t.Fatalf("encode: %v", err)
    }
    decoded, err := dec.Decode(encoded)
    if err != nil {
        t.Fatalf("decode: %v", err)
    }
    if len(decoded.Samples) != 160 {
        t.Fatalf("expected 160 samples, got %d", len(decoded.Samples))
    }
    // G.711 is lossy — check samples are in the right ballpark (±200 tolerance for mu-law)
    for i := 0; i < 10; i++ {
        diff := int(original.Samples[i]) - int(decoded.Samples[i])
        if diff < -200 || diff > 200 {
            t.Errorf("sample %d: original=%d decoded=%d diff=%d",
                i, original.Samples[i], decoded.Samples[i], diff)
        }
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestFFmpegEncoder`
Expected: FAIL — NewFFmpegEncoder not defined

- [ ] **Step 3: Implement `pkg/audiocodec/ff_encoder.go`**

Implement `FFmpegEncoder` using go-astiav:
- `NewFFmpegEncoder(codecName, sampleRate, channels)`: find encoder, alloc context, set sample_rate/channels/sample_fmt/bit_rate, open, alloc packet + frame
- For Opus: set `frame_duration` option to 20ms (960 samples at 48kHz)
- `Encode(pcm)`: fill frame data from PCMFrame.Samples, send frame, receive packet, return packet data copy
- `FrameSize()`: return `codecCtx.FrameSize()` for variable-frame codecs (AAC=1024, Opus=960), or a reasonable default (160) for PCM codecs where FrameSize is 0
- `Close()`: free all resources

Important: The encoder expects input in its preferred sample format. If the encoder wants `AV_SAMPLE_FMT_S16`, PCMFrame.Samples maps directly. If it wants `AV_SAMPLE_FMT_FLTP` (float planar, common for AAC), convert int16 → float32 before filling the frame.

- [ ] **Step 4: Run tests**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestFFmpegEncoder -race`
Expected: All 2 tests PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/audiocodec/ff_encoder.go pkg/audiocodec/ff_encoder_test.go
git commit -m "feat: add FFmpegEncoder with go-astiav"
```

---

## Task 6: FFmpegResampler

**Files:**
- Create: `pkg/audiocodec/ff_resampler.go`
- Create: `pkg/audiocodec/ff_resampler_test.go`

- [ ] **Step 1: Write tests**

```go
// pkg/audiocodec/ff_resampler_test.go
package audiocodec

import (
    "math"
    "testing"
)

func TestFFmpegResampler8kTo48k(t *testing.T) {
    r := NewFFmpegResampler(8000, 1, 48000, 1)
    defer r.Close()

    // 160 samples at 8kHz = 20ms → expect ~960 samples at 48kHz
    pcm := &PCMFrame{
        Samples:    make([]int16, 160),
        SampleRate: 8000,
        Channels:   1,
    }
    out := r.Resample(pcm)
    if out.SampleRate != 48000 {
        t.Fatalf("expected 48000, got %d", out.SampleRate)
    }
    // Allow ±5 samples tolerance for resampler buffering
    if math.Abs(float64(len(out.Samples))-960) > 5 {
        t.Fatalf("expected ~960 samples, got %d", len(out.Samples))
    }
}

func TestFFmpegResampler48kTo44k(t *testing.T) {
    r := NewFFmpegResampler(48000, 2, 44100, 2)
    defer r.Close()

    // 960 stereo samples at 48kHz = 20ms → expect ~882 stereo samples at 44.1kHz
    pcm := &PCMFrame{
        Samples:    make([]int16, 960*2),
        SampleRate: 48000,
        Channels:   2,
    }
    out := r.Resample(pcm)
    if out.SampleRate != 44100 {
        t.Fatalf("expected 44100, got %d", out.SampleRate)
    }
    expectedSamples := 882 * 2 // stereo
    if math.Abs(float64(len(out.Samples))-float64(expectedSamples)) > 10 {
        t.Fatalf("expected ~%d samples, got %d", expectedSamples, len(out.Samples))
    }
}

func TestFFmpegResamplerMonoToStereo(t *testing.T) {
    r := NewFFmpegResampler(8000, 1, 8000, 2)
    defer r.Close()

    pcm := &PCMFrame{
        Samples:    make([]int16, 160),
        SampleRate: 8000,
        Channels:   1,
    }
    out := r.Resample(pcm)
    if out.Channels != 2 {
        t.Fatalf("expected 2 channels, got %d", out.Channels)
    }
    // 160 mono → 320 interleaved stereo
    if len(out.Samples) != 320 {
        t.Fatalf("expected 320 samples, got %d", len(out.Samples))
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestFFmpegResampler`
Expected: FAIL

- [ ] **Step 3: Implement `pkg/audiocodec/ff_resampler.go`**

Implement using go-astiav's `SoftwareResampleContext`:
- `NewFFmpegResampler(inRate, inCh, outRate, outCh)`: alloc SwrContext, set in/out channel layout + sample rate + sample format (S16), init
- `Resample(pcm)`: create input frame from PCMFrame, call `swrCtx.ConvertFrame(in, out)`, extract int16 samples from output frame into new PCMFrame
- `Close()`: free SwrContext and frames

- [ ] **Step 4: Run tests**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run TestFFmpegResampler -race`
Expected: All 3 tests PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/audiocodec/ff_resampler.go pkg/audiocodec/ff_resampler_test.go
git commit -m "feat: add FFmpegResampler with swresample"
```

---

## Task 7: Codec registration + round-trip tests

**Files:**
- Create: `pkg/audiocodec/ff_register.go`
- Create: `pkg/audiocodec/ff_roundtrip_test.go`

- [ ] **Step 1: Write round-trip test**

```go
// pkg/audiocodec/ff_roundtrip_test.go
package audiocodec

import (
    "testing"

    "github.com/im-pingo/liveforge/pkg/avframe"
)

// TestRoundTripG711 verifies encode→decode round-trip for G.711 codecs.
func TestRoundTripG711(t *testing.T) {
    codecs := []struct {
        name    string
        codec   avframe.CodecType
        encName string
    }{
        {"PCMU", avframe.CodecG711U, "pcm_mulaw"},
        {"PCMA", avframe.CodecG711A, "pcm_alaw"},
    }

    for _, c := range codecs {
        t.Run(c.name, func(t *testing.T) {
            enc := NewFFmpegEncoder(c.encName, 8000, 1)
            defer enc.Close()
            dec := NewFFmpegDecoder(c.encName)
            defer dec.Close()

            pcm := &PCMFrame{
                Samples:    make([]int16, 160),
                SampleRate: 8000,
                Channels:   1,
            }
            for i := range pcm.Samples {
                pcm.Samples[i] = int16(i * 50)
            }

            encoded, err := enc.Encode(pcm)
            if err != nil {
                t.Fatalf("encode: %v", err)
            }
            decoded, err := dec.Decode(encoded)
            if err != nil {
                t.Fatalf("decode: %v", err)
            }
            if len(decoded.Samples) != len(pcm.Samples) {
                t.Fatalf("sample count mismatch: %d vs %d",
                    len(decoded.Samples), len(pcm.Samples))
            }
        })
    }
}

// TestRegistryAllCodecsRegistered verifies init() registered everything.
func TestRegistryAllCodecsRegistered(t *testing.T) {
    r := Global()

    codecs := []avframe.CodecType{
        avframe.CodecAAC, avframe.CodecOpus, avframe.CodecMP3,
        avframe.CodecG711U, avframe.CodecG711A, avframe.CodecG722,
        avframe.CodecSpeex,
    }

    for _, c := range codecs {
        if _, err := r.NewDecoder(c); err != nil {
            t.Errorf("no decoder for %s: %v", c, err)
        }
        if _, err := r.NewEncoder(c); err != nil {
            t.Errorf("no encoder for %s: %v", c, err)
        }
    }
}

// TestCanTranscodeMatrix verifies expected transcode pairs.
func TestCanTranscodeMatrix(t *testing.T) {
    r := Global()

    pairs := []struct {
        from, to avframe.CodecType
    }{
        {avframe.CodecAAC, avframe.CodecOpus},
        {avframe.CodecOpus, avframe.CodecAAC},
        {avframe.CodecAAC, avframe.CodecG711U},
        {avframe.CodecG711U, avframe.CodecOpus},
    }
    for _, p := range pairs {
        if !r.CanTranscode(p.from, p.to) {
            t.Errorf("expected CanTranscode(%s, %s) = true", p.from, p.to)
        }
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run "TestRoundTrip|TestRegistryAllCodecs|TestCanTranscodeMatrix"`
Expected: FAIL — ff_register.go not created yet, init() not run

- [ ] **Step 3: Implement `pkg/audiocodec/ff_register.go`**

```go
package audiocodec

import "github.com/im-pingo/liveforge/pkg/avframe"

var codecTable = []struct {
    typ     avframe.CodecType
    decName string
    encName string
    rate    int
    ch      int
}{
    {avframe.CodecAAC, "aac", "aac", 44100, 2},
    {avframe.CodecOpus, "libopus", "libopus", 48000, 2},
    {avframe.CodecMP3, "mp3", "libmp3lame", 44100, 2},
    {avframe.CodecG711U, "pcm_mulaw", "pcm_mulaw", 8000, 1},
    {avframe.CodecG711A, "pcm_alaw", "pcm_alaw", 8000, 1},
    {avframe.CodecG722, "g722", "g722", 16000, 1},
    {avframe.CodecSpeex, "libspeex", "libspeex", 8000, 1},
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

    // Only AAC needs a sequence header (AudioSpecificConfig for RTMP/FLV).
    // Opus, G.711, G.722, MP3, Speex do NOT use sequence headers.
    r.RegisterSequenceHeader(avframe.CodecAAC, func() SequenceHeaderFunc {
        return aacSequenceHeader
    })
}

// aacSequenceHeader builds an AudioSpecificConfig for AAC-LC at 44100Hz stereo.
// Format: 5 bits audioObjectType (2=LC) + 4 bits freqIndex (4=44100) + 4 bits chanConfig (2=stereo) + 3 bits padding
// = 0x12 0x10
// Note: This matches the codecTable entry (44100, 2) for AAC. If the AAC encoder
// is ever configured with different params, this must be derived dynamically.
func aacSequenceHeader() []byte {
    return []byte{0x12, 0x10}
}
```

- [ ] **Step 4: Run tests**

Run: `CGO_ENABLED=1 go test ./pkg/audiocodec/ -v -run "TestRoundTrip|TestRegistryAllCodecs|TestCanTranscodeMatrix" -race`
Expected: All tests PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/audiocodec/ff_register.go pkg/audiocodec/ff_roundtrip_test.go
git commit -m "feat: register all audio codecs via FFmpeg and verify round-trips"
```

---

## Task 8: Config + Stream integration

**Files:**
- Modify: `config/config.go:6-25`
- Modify: `config/loader.go:33-128`
- Modify: `core/stream.go:82-121` (struct + NewStream)
- Modify: `core/stream.go:140-166` (SetPublisher)

- [ ] **Step 1: Add `AudioCodecConfig` to `config/config.go`**

Add after the existing `Metrics MetricsConfig` field (line 24):

```go
AudioCodec AudioCodecConfig `yaml:"audio_codec"`
```

Add the struct definition (after `MetricsConfig`):

```go
type AudioCodecConfig struct {
    Enabled bool `yaml:"enabled"`
}
```

- [ ] **Step 2: Add default in `config/loader.go` `defaults()` function**

No default needed — `AudioCodecConfig{Enabled: false}` is the zero value, which means transcoding is off by default. This is intentional: users must explicitly opt in.

- [ ] **Step 3: Verify config loads**

Run: `CGO_ENABLED=1 go test ./config/ -v -race`
Expected: PASS (existing tests still pass)

- [ ] **Step 4: Add `transcodeManager` to `Stream` struct in `core/stream.go`**

Add field after `feedbackRouter` (line 104):

```go
transcodeManager *TranscodeManager
```

Note: `TranscodeManager` type does not exist yet — it will be created in Task 9. For now, use a forward reference. If the compiler complains, create a stub in `core/transcode_manager.go`:

```go
package core

type TranscodeManager struct{}
```

- [ ] **Step 5: Add `TranscodeManager()` accessor to `Stream`**

```go
func (s *Stream) TranscodeManager() *TranscodeManager {
    return s.transcodeManager
}
```

- [ ] **Step 6: Modify `SetPublisher` to call `TranscodeManager.Reset()`**

In `core/stream.go` `SetPublisher()`, add before `s.publisher = pub` (line 161):

```go
if s.transcodeManager != nil {
    s.transcodeManager.Reset()
}
```

- [ ] **Step 7: Wire TranscodeManager creation in StreamHub**

`NewStream` does NOT change signature. `StreamHub` owns the decision to enable transcoding.

Add `audioCodecEnabled bool` field to `StreamHub` struct, set from `config.AudioCodecConfig.Enabled` in `NewStreamHub`. Then in `StreamHub.GetOrCreate`, after creating the stream:

```go
if h.audioCodecEnabled {
    s.transcodeManager = NewTranscodeManager(s, audiocodec.Global(), h.config.RingBufferSize)
}
```

This is the single, authoritative place where TranscodeManager is wired. The actual `NewTranscodeManager` function is implemented in Task 9; this step creates the call site (will compile once Task 9's stub exists).

- [ ] **Step 8: Run tests**

Run: `CGO_ENABLED=1 go test ./config/ ./core/ -v -race`
Expected: All existing tests PASS

- [ ] **Step 9: Commit**

```bash
git add config/config.go config/loader.go core/stream.go
git commit -m "feat: add AudioCodecConfig and TranscodeManager slot to Stream"
```

---

## Task 9: TranscodeManager

**Files:**
- Create: `core/transcode_manager.go`
- Create: `core/transcode_manager_test.go`
- Modify: `core/stream.go` (wire TranscodeManager creation in NewStream)

This is the largest task. The TranscodeManager orchestrates on-demand transcode goroutines.

- [ ] **Step 1: Write tests**

```go
// core/transcode_manager_test.go
package core

import (
    "testing"
    "time"

    "github.com/im-pingo/liveforge/config"
    "github.com/im-pingo/liveforge/pkg/audiocodec"
    "github.com/im-pingo/liveforge/pkg/avframe"
)

// TestTranscodeManagerZeroOverhead verifies no TranscodedTrack is created
// when the subscriber requests the same codec as the publisher.
func TestTranscodeManagerZeroOverhead(t *testing.T) {
    s := newTestStream(avframe.CodecAAC)
    tm := s.TranscodeManager()

    reader, release, err := tm.GetOrCreateReader(avframe.CodecAAC)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    defer release()

    // Should return original ring buffer reader, no tracks created
    if reader == nil {
        t.Fatal("expected non-nil reader")
    }
    if len(tm.tracks) != 0 {
        t.Fatalf("expected 0 tracks, got %d", len(tm.tracks))
    }
}

// TestTranscodeManagerCreateTrack verifies a TranscodedTrack is created
// when codecs differ.
func TestTranscodeManagerCreateTrack(t *testing.T) {
    s := newTestStream(avframe.CodecG711U)
    tm := s.TranscodeManager()

    reader, release, err := tm.GetOrCreateReader(avframe.CodecG711A)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    defer release()

    if reader == nil {
        t.Fatal("expected non-nil reader")
    }
    if len(tm.tracks) != 1 {
        t.Fatalf("expected 1 track, got %d", len(tm.tracks))
    }
}

// TestTranscodeManagerSharing verifies two subscribers share one track.
func TestTranscodeManagerSharing(t *testing.T) {
    s := newTestStream(avframe.CodecG711U)
    tm := s.TranscodeManager()

    _, release1, _ := tm.GetOrCreateReader(avframe.CodecG711A)
    _, release2, _ := tm.GetOrCreateReader(avframe.CodecG711A)

    if len(tm.tracks) != 1 {
        t.Fatalf("expected 1 shared track, got %d", len(tm.tracks))
    }
    if tm.tracks[avframe.CodecG711A].subCount != 2 {
        t.Fatalf("expected subCount=2, got %d", tm.tracks[avframe.CodecG711A].subCount)
    }

    release1()
    if tm.tracks[avframe.CodecG711A].subCount != 1 {
        t.Fatalf("expected subCount=1 after release1")
    }

    release2()
    // Allow goroutine cleanup
    time.Sleep(50 * time.Millisecond)
    if len(tm.tracks) != 0 {
        t.Fatalf("expected 0 tracks after all releases, got %d", len(tm.tracks))
    }
}

// TestTranscodeManagerReset verifies all tracks are cleaned up on Reset.
func TestTranscodeManagerReset(t *testing.T) {
    s := newTestStream(avframe.CodecG711U)
    tm := s.TranscodeManager()

    _, _, _ = tm.GetOrCreateReader(avframe.CodecG711A)
    _, _, _ = tm.GetOrCreateReader(avframe.CodecG722)

    tm.Reset()
    time.Sleep(50 * time.Millisecond)

    if len(tm.tracks) != 0 {
        t.Fatalf("expected 0 tracks after Reset, got %d", len(tm.tracks))
    }
}

// TestTranscodeManagerNoPublisher verifies error when no publisher.
func TestTranscodeManagerNoPublisher(t *testing.T) {
    cfg := config.StreamConfig{RingBufferSize: 64}
    limits := config.LimitsConfig{}
    bus := NewEventBus()
    s := NewStream("test", cfg, limits, bus)
    // Do NOT set publisher

    tm := NewTranscodeManager(s, audiocodec.Global(), 64)
    _, _, err := tm.GetOrCreateReader(avframe.CodecOpus)
    if err == nil {
        t.Fatal("expected error when no publisher")
    }
}

// helper: create a stream with a mock publisher that has the given audio codec.
func newTestStream(audioCodec avframe.CodecType) *Stream {
    cfg := config.StreamConfig{RingBufferSize: 64}
    limits := config.LimitsConfig{}
    bus := NewEventBus()
    s := NewStream("test", cfg, limits, bus)
    s.transcodeManager = NewTranscodeManager(s, audiocodec.Global(), 64)

    pub := &testPublisher{
        mediaInfo: &avframe.MediaInfo{AudioCodec: audioCodec},
    }
    s.SetPublisher(pub)
    return s
}

type testPublisher struct {
    mediaInfo *avframe.MediaInfo
}

func (p *testPublisher) ID() string                    { return "test-pub" }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.mediaInfo }
func (p *testPublisher) Close() error                  { return nil }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./core/ -v -run TestTranscodeManager`
Expected: FAIL — TranscodeManager not defined

- [ ] **Step 3: Implement `core/transcode_manager.go`**

Implement exactly as specified in the design spec. Key components:

```go
package core

import (
    "context"
    "log/slog"
    "sync"

    "github.com/im-pingo/liveforge/pkg/audiocodec"
    "github.com/im-pingo/liveforge/pkg/avframe"
    "github.com/im-pingo/liveforge/pkg/util"
)

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

func NewTranscodeManager(stream *Stream, registry *audiocodec.Registry, bufSize int) *TranscodeManager { ... }
func (tm *TranscodeManager) GetOrCreateReader(targetCodec avframe.CodecType) (*util.RingReader[*avframe.AVFrame], func(), error) { ... }
func (tm *TranscodeManager) releaseTrack(targetCodec avframe.CodecType) { ... }
func (tm *TranscodeManager) Reset() { ... }
func (tm *TranscodeManager) transcodeLoop(ctx context.Context, track *TranscodedTrack) { ... }
```

The `transcodeLoop` implementation follows the spec exactly (spec lines 460-596):

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
        decoder.Close()
        track.ringBuffer.Close()
        return
    }
    defer decoder.Close()
    defer encoder.Close()

    // Set extradata for codecs that need it (e.g. AAC AudioSpecificConfig)
    if seqHeader := tm.stream.AudioSeqHeader(); seqHeader != nil {
        decoder.SetExtradata(seqHeader.Payload)
    }

    // Resampler is created lazily after the first successful decode,
    // using the actual decoded sample rate/channels.
    var resampler *audiocodec.FFmpegResampler
    resamplerInited := false

    // Emit sequence header for target codec
    if seqHeader := tm.registry.SequenceHeader(track.targetCodec); seqHeader != nil {
        track.ringBuffer.Write(avframe.NewAVFrame(
            avframe.MediaTypeAudio, track.targetCodec,
            avframe.FrameTypeSequenceHeader, 0, 0, seqHeader,
        ))
    }

    var ts audiocodec.TsTracker
    tsInited := false
    var pcmBuf []int16
    frameSize := encoder.FrameSize() * encoder.Channels()
    const maxPCMBufSamples = 48000 * 2 // cap at ~1s of 48kHz stereo

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
            ts.Init(frame.DTS, encoder.SampleRate())
            tsInited = true
        }

        // Decode
        pcm, err := decoder.Decode(frame.Payload)
        if err != nil {
            continue
        }

        // Lazy resampler init using actual decoded params
        if !resamplerInited {
            if pcm.SampleRate != encoder.SampleRate() ||
               pcm.Channels != encoder.Channels() {
                resampler = audiocodec.NewFFmpegResampler(
                    pcm.SampleRate, pcm.Channels,
                    encoder.SampleRate(), encoder.Channels(),
                )
                defer resampler.Close()
            }
            resamplerInited = true
        }

        // Resample
        if resampler != nil {
            pcm = resampler.Resample(pcm)
        }

        // Accumulate and encode in encoder-sized chunks
        pcmBuf = append(pcmBuf, pcm.Samples...)
        if len(pcmBuf) > maxPCMBufSamples {
            pcmBuf = pcmBuf[len(pcmBuf)-maxPCMBufSamples:]
        }
        for len(pcmBuf) >= frameSize {
            chunk := &audiocodec.PCMFrame{
                Samples:    pcmBuf[:frameSize],
                SampleRate: encoder.SampleRate(),
                Channels:   encoder.Channels(),
            }
            encoded, err := encoder.Encode(chunk)
            if err != nil {
                pcmBuf = pcmBuf[frameSize:]
                continue
            }

            dts := ts.Next(encoder.FrameSize())

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

- [ ] **Step 4: Verify StreamHub wiring**

The StreamHub wiring was set up in Task 8, Step 7. Verify that `StreamHub.GetOrCreate` now correctly calls `NewTranscodeManager` (which we just implemented) when `audioCodecEnabled` is true. No code changes needed here — just confirm Task 8's stub compiles with the real implementation.

- [ ] **Step 5: Run tests**

Run: `CGO_ENABLED=1 go test ./core/ -v -run TestTranscodeManager -race`
Expected: All 5 tests PASS

- [ ] **Step 6: Commit**

```bash
git add core/transcode_manager.go core/transcode_manager_test.go core/stream.go core/stream_hub.go
git commit -m "feat: add TranscodeManager with on-demand shared transcoding"
```

---

## Task 10: Integration test — A/V sync verification

**Files:**
- Create: `test/integration/transcode_avsync_test.go`

- [ ] **Step 1: Write the A/V sync test**

```go
// test/integration/transcode_avsync_test.go
package integration

import (
    "testing"
    "time"

    "github.com/im-pingo/liveforge/config"
    "github.com/im-pingo/liveforge/core"
    "github.com/im-pingo/liveforge/pkg/audiocodec"
    "github.com/im-pingo/liveforge/pkg/avframe"
)

func TestTranscodeAVSync(t *testing.T) {
    // Create stream with G.711U publisher
    cfg := config.StreamConfig{RingBufferSize: 256}
    bus := core.NewEventBus()
    s := core.NewStream("avsync-test", cfg, config.LimitsConfig{}, bus)
    // TranscodeManager is wired via StreamHub in production; here we set the
    // unexported field directly via a test helper exported from core package.
    core.SetTranscodeManagerForTest(s, core.NewTranscodeManager(s, audiocodec.Global(), 256))

    pub := &mockPublisher{info: &avframe.MediaInfo{
        AudioCodec: avframe.CodecG711U,
        VideoCodec: avframe.CodecH264,
    }}
    s.SetPublisher(pub)

    // Get transcoded reader for G.711A (different codec triggers transcode)
    reader, release, err := s.TranscodeManager().GetOrCreateReader(avframe.CodecG711A)
    if err != nil {
        t.Fatalf("GetOrCreateReader: %v", err)
    }
    defer release()

    // Write interleaved A/V frames with known timestamps
    // Video at 30fps (33ms), audio at 50fps (20ms)
    rb := s.RingBuffer()
    go func() {
        for i := 0; i < 100; i++ {
            vDTS := int64(i * 33)
            rb.Write(avframe.NewAVFrame(
                avframe.MediaTypeVideo, avframe.CodecH264,
                avframe.FrameTypeInterframe, vDTS, vDTS,
                []byte{0x00, 0x01, 0x02},
            ))

            aDTS := int64(i * 20)
            // G.711U: 160 bytes = 20ms at 8kHz
            rb.Write(avframe.NewAVFrame(
                avframe.MediaTypeAudio, avframe.CodecG711U,
                avframe.FrameTypeInterframe, aDTS, aDTS,
                make([]byte, 160),
            ))
            time.Sleep(time.Millisecond) // simulate real-time pacing
        }
    }()

    // Read from transcoded track and verify A/V timestamp alignment
    var lastVideoDTS, lastAudioDTS int64
    videoCount, audioCount := 0, 0
    timeout := time.After(3 * time.Second)

    for videoCount < 10 && audioCount < 10 {
        select {
        case <-timeout:
            t.Fatalf("timeout: got %d video, %d audio frames", videoCount, audioCount)
        default:
        }

        frame, ok := reader.TryRead()
        if !ok {
            time.Sleep(time.Millisecond)
            continue
        }

        if frame.MediaType.IsVideo() {
            lastVideoDTS = frame.DTS
            videoCount++
        } else if frame.MediaType.IsAudio() {
            lastAudioDTS = frame.DTS
            audioCount++
        }
    }

    // After receiving frames from both tracks, the DTS values should be
    // in the same ballpark (within one frame duration = 33ms)
    drift := lastVideoDTS - lastAudioDTS
    if drift < 0 {
        drift = -drift
    }
    if drift > 50 {
        t.Errorf("A/V drift too large: video DTS=%d, audio DTS=%d, drift=%dms",
            lastVideoDTS, lastAudioDTS, drift)
    }
}

type mockPublisher struct {
    info *avframe.MediaInfo
}

func (p *mockPublisher) ID() string                    { return "mock-pub" }
func (p *mockPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *mockPublisher) Close() error                  { return nil }
```

Note: `core.SetTranscodeManagerForTest` is a test helper that must be added to `core/stream.go` (or a `core/export_test.go` file):

```go
// SetTranscodeManagerForTest sets the TranscodeManager on a Stream.
// Only used by integration tests.
func SetTranscodeManagerForTest(s *Stream, tm *TranscodeManager) {
    s.transcodeManager = tm
}
```

- [ ] **Step 2: Run test**

Run: `CGO_ENABLED=1 go test ./test/integration/ -v -run TestTranscodeAVSync -race -timeout 30s`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add test/integration/transcode_avsync_test.go
git commit -m "test: add A/V sync integration test for audio transcoding"
```

---

## Task 11: Cross-protocol integration tests — RTMP↔WebRTC

**Files:**
- Create: `test/integration/transcode_rtmp_webrtc_test.go`
- Create: `test/integration/transcode_webrtc_rtmp_test.go`

These tests verify the primary use case: RTMP publisher with AAC → WebRTC subscriber gets Opus, and vice versa. They are written BEFORE protocol integration code (Tasks 12-14), following TDD.

- [ ] **Step 1: Write RTMP→WebRTC test**

```go
// test/integration/transcode_rtmp_webrtc_test.go
package integration

import (
    "testing"
    "time"

    "github.com/im-pingo/liveforge/config"
    "github.com/im-pingo/liveforge/core"
    "github.com/im-pingo/liveforge/pkg/audiocodec"
    "github.com/im-pingo/liveforge/pkg/avframe"
)

// TestRTMPtoWebRTC verifies that an RTMP publisher (AAC) produces Opus
// frames for a WebRTC subscriber via TranscodeManager.
func TestRTMPtoWebRTC(t *testing.T) {
    cfg := config.StreamConfig{RingBufferSize: 256}
    bus := core.NewEventBus()
    s := core.NewStream("rtmp-webrtc", cfg, config.LimitsConfig{}, bus)
    core.SetTranscodeManagerForTest(s, core.NewTranscodeManager(s, audiocodec.Global(), 256))

    pub := &mockPublisher{info: &avframe.MediaInfo{
        AudioCodec: avframe.CodecAAC,
    }}
    s.SetPublisher(pub)

    // WebRTC subscriber requests Opus
    reader, release, err := s.TranscodeManager().GetOrCreateReader(avframe.CodecOpus)
    if err != nil {
        t.Fatalf("GetOrCreateReader(Opus): %v", err)
    }
    defer release()

    // Write AAC sequence header + audio frames into the stream's ring buffer.
    // AAC AudioSpecificConfig: 0x12 0x10 = AAC-LC 44100Hz stereo
    rb := s.RingBuffer()
    rb.Write(avframe.NewAVFrame(
        avframe.MediaTypeAudio, avframe.CodecAAC,
        avframe.FrameTypeSequenceHeader, 0, 0,
        []byte{0x12, 0x10},
    ))

    // Encode a silent AAC frame to use as test payload
    enc := audiocodec.NewFFmpegEncoder("aac", 44100, 2)
    defer enc.Close()
    silence := &audiocodec.PCMFrame{
        Samples:    make([]int16, 1024*2),
        SampleRate: 44100,
        Channels:   2,
    }
    aacPayload, err := enc.Encode(silence)
    if err != nil {
        t.Fatalf("encode AAC: %v", err)
    }

    // Write 10 AAC frames
    for i := 0; i < 10; i++ {
        dts := int64(i) * 23 // ~23ms per AAC frame at 44100Hz
        rb.Write(avframe.NewAVFrame(
            avframe.MediaTypeAudio, avframe.CodecAAC,
            avframe.FrameTypeInterframe, dts, dts,
            aacPayload,
        ))
    }

    // Read from transcoded track and verify we get Opus frames
    opusCount := 0
    timeout := time.After(3 * time.Second)
    for opusCount < 5 {
        select {
        case <-timeout:
            t.Fatalf("timeout: only got %d Opus frames", opusCount)
        default:
        }
        frame, ok := reader.TryRead()
        if !ok {
            time.Sleep(time.Millisecond)
            continue
        }
        if frame.FrameType == avframe.FrameTypeSequenceHeader {
            continue
        }
        if frame.MediaType.IsAudio() {
            if frame.Codec != avframe.CodecOpus {
                t.Fatalf("expected Opus, got codec %d", frame.Codec)
            }
            opusCount++
        }
    }
    t.Logf("received %d Opus frames from AAC source", opusCount)
}
```

- [ ] **Step 2: Write WebRTC→RTMP test**

```go
// test/integration/transcode_webrtc_rtmp_test.go
package integration

import (
    "testing"
    "time"

    "github.com/im-pingo/liveforge/config"
    "github.com/im-pingo/liveforge/core"
    "github.com/im-pingo/liveforge/pkg/audiocodec"
    "github.com/im-pingo/liveforge/pkg/avframe"
)

// TestWebRTCtoRTMP verifies that a WebRTC publisher (Opus) produces AAC
// frames for an RTMP subscriber via TranscodeManager.
func TestWebRTCtoRTMP(t *testing.T) {
    cfg := config.StreamConfig{RingBufferSize: 256}
    bus := core.NewEventBus()
    s := core.NewStream("webrtc-rtmp", cfg, config.LimitsConfig{}, bus)
    core.SetTranscodeManagerForTest(s, core.NewTranscodeManager(s, audiocodec.Global(), 256))

    pub := &mockPublisher{info: &avframe.MediaInfo{
        AudioCodec: avframe.CodecOpus,
    }}
    s.SetPublisher(pub)

    // RTMP subscriber requests AAC
    reader, release, err := s.TranscodeManager().GetOrCreateReader(avframe.CodecAAC)
    if err != nil {
        t.Fatalf("GetOrCreateReader(AAC): %v", err)
    }
    defer release()

    // Encode silent Opus frames as test payload
    enc := audiocodec.NewFFmpegEncoder("libopus", 48000, 2)
    defer enc.Close()
    silence := &audiocodec.PCMFrame{
        Samples:    make([]int16, 960*2), // 20ms at 48kHz stereo
        SampleRate: 48000,
        Channels:   2,
    }
    opusPayload, err := enc.Encode(silence)
    if err != nil {
        t.Fatalf("encode Opus: %v", err)
    }

    // Write 20 Opus frames (no sequence header needed for Opus)
    rb := s.RingBuffer()
    for i := 0; i < 20; i++ {
        dts := int64(i) * 20 // 20ms per frame
        rb.Write(avframe.NewAVFrame(
            avframe.MediaTypeAudio, avframe.CodecOpus,
            avframe.FrameTypeInterframe, dts, dts,
            opusPayload,
        ))
    }

    // Read from transcoded track and verify we get AAC frames
    aacCount := 0
    gotSeqHeader := false
    timeout := time.After(3 * time.Second)
    for aacCount < 5 {
        select {
        case <-timeout:
            t.Fatalf("timeout: got %d AAC frames, seqHeader=%v", aacCount, gotSeqHeader)
        default:
        }
        frame, ok := reader.TryRead()
        if !ok {
            time.Sleep(time.Millisecond)
            continue
        }
        if frame.MediaType.IsAudio() {
            if frame.Codec != avframe.CodecAAC {
                t.Fatalf("expected AAC, got codec %d", frame.Codec)
            }
            if frame.FrameType == avframe.FrameTypeSequenceHeader {
                gotSeqHeader = true
                continue
            }
            aacCount++
        }
    }
    if !gotSeqHeader {
        t.Error("expected AAC sequence header before audio frames")
    }
    t.Logf("received %d AAC frames from Opus source", aacCount)
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./test/integration/ -v -run "TestRTMPtoWebRTC|TestWebRTCtoRTMP" -race -timeout 30s`
Expected: FAIL (tests rely on TranscodeManager + FFmpeg backend wired up end-to-end; will pass after Tasks 1-9 are complete)

- [ ] **Step 4: Commit**

```bash
git add test/integration/transcode_rtmp_webrtc_test.go test/integration/transcode_webrtc_rtmp_test.go
git commit -m "test: add cross-protocol integration tests for RTMP↔WebRTC audio transcoding"
```

---

## Task 12: Protocol integration — WebRTC WHEP (highest priority)

**Files:**
- Modify: `module/webrtc/whep_feed.go`

This is the primary consumer that motivated the entire feature. Change from "skip audio when codec mismatches" to "transcode via TranscodeManager."

- [ ] **Step 1: Read the current `whep_feed.go` implementation**

Identify the section that creates the audio sender and skips AAC. Look for the comment: `// AAC from RTMP is not WebRTC compatible`.

- [ ] **Step 2: Modify to use TranscodeManager**

Replace the codec-check-and-skip pattern with:

```go
// Before: skip audio if codec not WebRTC-compatible
// After:
var reader *util.RingReader[*avframe.AVFrame]
var release func()
if tm := stream.TranscodeManager(); tm != nil {
    var err error
    reader, release, err = tm.GetOrCreateReader(avframe.CodecOpus)
    if err != nil {
        // Transcode not available — fall back to original reader (video-only)
        slog.Warn("whep: audio transcode unavailable", "error", err)
        reader = stream.RingBuffer().NewReader()
        release = func() {}
    }
} else {
    reader = stream.RingBuffer().NewReader()
    release = func() {}
}
defer release()
```

- [ ] **Step 3: Run existing WebRTC tests + cross-protocol integration test**

Run: `CGO_ENABLED=1 go test ./module/webrtc/ -v -race`
Expected: PASS (existing tests should still work)

Run: `CGO_ENABLED=1 go test ./test/integration/ -v -run TestRTMPtoWebRTC -race -timeout 30s`
Expected: PASS (the RTMP→WebRTC integration test from Task 11 should now pass end-to-end)

- [ ] **Step 4: Commit**

```bash
git add module/webrtc/whep_feed.go
git commit -m "feat: integrate audio transcoding into WebRTC WHEP subscriber"
```

---

## Task 13: Protocol integration — RTMP subscriber

**Files:**
- Modify: `module/rtmp/subscriber.go` (or the muxer feed loop)

When publisher is WebRTC (Opus), RTMP subscribers need AAC.

- [ ] **Step 1: Read the current RTMP subscriber implementation**

Identify where it reads from the stream's RingBuffer or MuxerManager.

- [ ] **Step 2: Add TranscodeManager integration**

Same pattern as WHEP — request `avframe.CodecAAC` from TranscodeManager when the publisher's audio codec is not FLV-compatible (not AAC and not MP3).

- [ ] **Step 3: Run existing RTMP tests + cross-protocol integration test**

Run: `CGO_ENABLED=1 go test ./module/rtmp/ -v -race`
Expected: PASS

Run: `CGO_ENABLED=1 go test ./test/integration/ -v -run TestWebRTCtoRTMP -race -timeout 30s`
Expected: PASS (the WebRTC→RTMP integration test from Task 11 should now pass end-to-end)

- [ ] **Step 4: Commit**

```bash
git add module/rtmp/subscriber.go
git commit -m "feat: integrate audio transcoding into RTMP subscriber"
```

---

## Task 14: Protocol integration — MuxerManager (HLS/DASH/HTTP-FLV/SRT)

**Files:**
- Modify: `core/muxer_manager.go`

MuxerManager produces muxed output for container-based protocols. When the publisher codec is not compatible with the target container, it should read from a TranscodedTrack instead.

- [ ] **Step 1: Read the current MuxerManager implementation**

Identify the muxer worker goroutines and where they read AVFrames from the stream's RingBuffer.

- [ ] **Step 2: Add TranscodeManager integration**

For each muxer format, determine the required audio codec:
- FLV muxer → needs AAC or MP3
- TS muxer → needs AAC or MP3
- fMP4 muxer → needs AAC, Opus, or MP3

If the publisher's audio codec does not match, use `TranscodeManager.GetOrCreateReader(targetCodec)`.

- [ ] **Step 3: Run muxer/httpstream tests + all integration tests**

Run: `CGO_ENABLED=1 go test ./core/ ./module/httpstream/ -v -race`
Expected: PASS

Run: `CGO_ENABLED=1 go test ./test/integration/ -v -race -timeout 60s`
Expected: ALL integration tests PASS (A/V sync, RTMP→WebRTC, WebRTC→RTMP)

- [ ] **Step 4: Commit**

```bash
git add core/muxer_manager.go
git commit -m "feat: integrate audio transcoding into MuxerManager for HLS/DASH/HTTP-FLV"
```

---

## Task 15: Remaining platforms — FFmpeg libs for linux_amd64, linux_arm64, darwin_amd64

**Files:**
- Create: `third_party/ffmpeg/lib/linux_amd64/`
- Create: `third_party/ffmpeg/lib/linux_arm64/`
- Create: `third_party/ffmpeg/lib/darwin_amd64/`
- Modify: `third_party/ffmpeg/BUILD.md`

- [ ] **Step 1: Download Linux amd64 static libs**

```bash
# BtbN/FFmpeg-Builds: https://github.com/BtbN/FFmpeg-Builds/releases
# Download: ffmpeg-n7.1-latest-linux64-gpl-shared-7.1.tar.xz (or static variant)
# Extract the required .a files:
#   libavcodec.a, libavutil.a, libswresample.a
# Also need codec-specific libs (bundled in the static build):
#   libopus.a, libmp3lame.a, libspeex.a
# Place into: third_party/ffmpeg/lib/linux_amd64/
```

- [ ] **Step 2: Download Linux arm64 static libs**

```bash
# BtbN also provides aarch64 builds:
# Download: ffmpeg-n7.1-latest-linuxarm64-gpl-shared-7.1.tar.xz
# Same extraction process as Step 1
# Place into: third_party/ffmpeg/lib/linux_arm64/
```

- [ ] **Step 3: Extract macOS amd64 libs**

```bash
# Option A: Homebrew bottle for x86_64
#   brew fetch --bottle-tag=monterey ffmpeg
#   Extract .a files from the downloaded bottle
# Option B: Cross-compile from source on Apple Silicon
#   arch -x86_64 brew install ffmpeg
# Place into: third_party/ffmpeg/lib/darwin_amd64/
```

- [ ] **Step 4: Update BUILD.md with exact URLs and steps for all platforms**

- [ ] **Step 5: Verify cross-platform test (if CI available)**

Run: `make test` on each platform (or in CI)
Expected: All tests PASS on all platforms

- [ ] **Step 6: Commit**

```bash
git add third_party/ffmpeg/
git commit -m "chore: add FFmpeg static libs for all four target platforms"
```
