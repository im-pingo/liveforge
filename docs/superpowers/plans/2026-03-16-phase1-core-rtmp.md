# Phase 1: Core + RTMP Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a working RTMP relay server — push with ffmpeg, pull with ffplay — on top of a modular core architecture that all future protocol modules will reuse.

**Architecture:** Hub-Stream centralized model. Publisher demuxes RTMP into AVFrames stored in a lock-free SPMC RingBuffer. FLV Muxer reads frames and writes to a SharedBuffer. RTMP subscribers read muxed FLV tags and send via RTMP chunked transport. EventBus connects modules via sync/async hooks.

**Tech Stack:** Go 1.24+, minimal dependencies: `gopkg.in/yaml.v3` (config parsing). RTMP/FLV are custom implementations.

**Note:** The project name is **LiveForge** (`github.com/im-pingo/liveforge`). The spec uses "streamserver" as a placeholder — all implementation uses `liveforge`.

**Spec Reference:** `docs/superpowers/specs/2026-03-16-streamserver-design.md`

---

## Chunk 1: Foundation Types + Config

### File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `pkg/avframe/frame.go` | AVFrame, CodecType, FrameType, MediaType enums and frame struct |
| Create | `pkg/avframe/codec_info.go` | MediaInfo, track metadata |
| Create | `pkg/codec/h264/parser.go` | H.264 SPS/PPS parsing (extract resolution, profile) |
| Create | `pkg/codec/aac/parser.go` | AAC AudioSpecificConfig parsing (extract sample rate, channels) |
| Create | `config/config.go` | Full config struct hierarchy |
| Create | `config/loader.go` | YAML loading + env var expansion + defaults |
| Create | `configs/streamserver.yaml` | Default config file |
| Create | `cmd/liveforge/main.go` | Entry point (config load only, no server yet) |

---

### Task 1: AVFrame Types

**Files:**
- Create: `pkg/avframe/frame.go`
- Test: `pkg/avframe/frame_test.go`

- [ ] **Step 1: Write failing test for AVFrame construction**

```go
// pkg/avframe/frame_test.go
package avframe

import (
    "testing"
)

func TestNewAVFrame(t *testing.T) {
    payload := []byte{0x00, 0x01, 0x02}
    f := NewAVFrame(MediaTypeVideo, CodecH264, FrameTypeKeyframe, 1000, 1000, payload)

    if f.MediaType != MediaTypeVideo {
        t.Errorf("expected MediaTypeVideo, got %v", f.MediaType)
    }
    if f.Codec != CodecH264 {
        t.Errorf("expected CodecH264, got %v", f.Codec)
    }
    if f.FrameType != FrameTypeKeyframe {
        t.Errorf("expected FrameTypeKeyframe, got %v", f.FrameType)
    }
    if f.DTS != 1000 {
        t.Errorf("expected DTS 1000, got %d", f.DTS)
    }
    if f.PTS != 1000 {
        t.Errorf("expected PTS 1000, got %d", f.PTS)
    }
    if len(f.Payload) != 3 {
        t.Errorf("expected payload len 3, got %d", len(f.Payload))
    }
}

func TestCodecTypeString(t *testing.T) {
    tests := []struct {
        codec CodecType
        want  string
    }{
        {CodecH264, "H264"},
        {CodecH265, "H265"},
        {CodecAV1, "AV1"},
        {CodecVP8, "VP8"},
        {CodecVP9, "VP9"},
        {CodecAAC, "AAC"},
        {CodecOpus, "Opus"},
        {CodecMP3, "MP3"},
        {CodecG711A, "G711A"},
        {CodecG711U, "G711U"},
        {CodecG722, "G722"},
        {CodecG729, "G729"},
        {CodecSpeex, "Speex"},
    }
    for _, tt := range tests {
        if got := tt.codec.String(); got != tt.want {
            t.Errorf("CodecType(%d).String() = %q, want %q", tt.codec, got, tt.want)
        }
    }
}

func TestMediaTypeIsVideo(t *testing.T) {
    if !MediaTypeVideo.IsVideo() {
        t.Error("MediaTypeVideo.IsVideo() should be true")
    }
    if MediaTypeAudio.IsVideo() {
        t.Error("MediaTypeAudio.IsVideo() should be false")
    }
}

func TestFrameTypeIsKeyframe(t *testing.T) {
    if !FrameTypeKeyframe.IsKeyframe() {
        t.Error("FrameTypeKeyframe.IsKeyframe() should be true")
    }
    if FrameTypeInterframe.IsKeyframe() {
        t.Error("FrameTypeInterframe.IsKeyframe() should be false")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/avframe/ -v -run TestNewAVFrame`
Expected: FAIL — package/types not defined

- [ ] **Step 3: Write AVFrame implementation**

```go
// pkg/avframe/frame.go
package avframe

// MediaType distinguishes audio and video frames.
type MediaType uint8

const (
    MediaTypeVideo MediaType = iota + 1
    MediaTypeAudio
)

func (m MediaType) IsVideo() bool { return m == MediaTypeVideo }
func (m MediaType) IsAudio() bool { return m == MediaTypeAudio }

// CodecType identifies the codec of a frame.
type CodecType uint8

const (
    // Video codecs
    CodecH264 CodecType = iota + 1
    CodecH265
    CodecAV1
    CodecVP8
    CodecVP9

    // Audio codecs
    CodecAAC CodecType = iota + 50
    CodecOpus
    CodecMP3
    CodecG711A // PCMA
    CodecG711U // PCMU
    CodecG722
    CodecG729
    CodecSpeex
)

var codecNames = map[CodecType]string{
    CodecH264: "H264", CodecH265: "H265", CodecAV1: "AV1",
    CodecVP8: "VP8", CodecVP9: "VP9",
    CodecAAC: "AAC", CodecOpus: "Opus", CodecMP3: "MP3",
    CodecG711A: "G711A", CodecG711U: "G711U",
    CodecG722: "G722", CodecG729: "G729", CodecSpeex: "Speex",
}

func (c CodecType) String() string {
    if s, ok := codecNames[c]; ok {
        return s
    }
    return "Unknown"
}

func (c CodecType) IsVideo() bool {
    return c >= CodecH264 && c <= CodecVP9
}

func (c CodecType) IsAudio() bool {
    return c >= CodecAAC
}

// FrameType distinguishes keyframes from inter-frames.
type FrameType uint8

const (
    FrameTypeKeyframe   FrameType = iota + 1
    FrameTypeInterframe
    FrameTypeSequenceHeader // SPS/PPS, AudioSpecificConfig, etc.
)

func (f FrameType) IsKeyframe() bool { return f == FrameTypeKeyframe }

// AVFrame is the universal internal media frame.
type AVFrame struct {
    MediaType MediaType
    Codec     CodecType
    FrameType FrameType
    DTS       int64  // Decode timestamp in milliseconds
    PTS       int64  // Presentation timestamp in milliseconds
    Payload   []byte // Raw codec data (no container framing)
}

// NewAVFrame creates a new AVFrame.
func NewAVFrame(mediaType MediaType, codec CodecType, frameType FrameType, dts, pts int64, payload []byte) *AVFrame {
    return &AVFrame{
        MediaType: mediaType,
        Codec:     codec,
        FrameType: frameType,
        DTS:       dts,
        PTS:       pts,
        Payload:   payload,
    }
}
```

- [ ] **Step 4: Run all tests to verify they pass**

Run: `go test ./pkg/avframe/ -v`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/avframe/
git commit -m "feat: add AVFrame types — codec, media, frame type enums and frame struct"
```

---

### Task 2: Codec Parsers (H.264 + AAC)

**Files:**
- Create: `pkg/codec/h264/parser.go`
- Create: `pkg/codec/aac/parser.go`
- Test: `pkg/codec/h264/parser_test.go`
- Test: `pkg/codec/aac/parser_test.go`

- [ ] **Step 1: Write failing test for H.264 SPS parsing**

```go
// pkg/codec/h264/parser_test.go
package h264

import "testing"

func TestParseSPS(t *testing.T) {
    // Minimal SPS for 1920x1080, profile 100 (High), level 4.0
    // This is a real-world SPS NAL unit (without the start code)
    sps := []byte{
        0x67, 0x64, 0x00, 0x28, 0xAC, 0xD9, 0x40, 0x78,
        0x02, 0x27, 0xE5, 0xC0, 0x44, 0x00, 0x00, 0x03,
        0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xF0, 0x3C,
        0x60, 0xC6, 0x58,
    }
    info, err := ParseSPS(sps)
    if err != nil {
        t.Fatalf("ParseSPS error: %v", err)
    }
    if info.Width != 1920 {
        t.Errorf("expected width 1920, got %d", info.Width)
    }
    if info.Height != 1080 {
        t.Errorf("expected height 1080, got %d", info.Height)
    }
    if info.Profile != 100 {
        t.Errorf("expected profile 100, got %d", info.Profile)
    }
}

func TestExtractNALUs(t *testing.T) {
    // Annex-B format with start codes
    data := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x01, 0x02,
        0x00, 0x00, 0x00, 0x01, 0x68, 0x03, 0x04}
    nalus := ExtractNALUs(data)
    if len(nalus) != 2 {
        t.Fatalf("expected 2 NALUs, got %d", len(nalus))
    }
    if nalus[0][0] != 0x67 {
        t.Errorf("first NALU type byte: expected 0x67, got 0x%02x", nalus[0][0])
    }
    if nalus[1][0] != 0x68 {
        t.Errorf("second NALU type byte: expected 0x68, got 0x%02x", nalus[1][0])
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/codec/h264/ -v`
Expected: FAIL

- [ ] **Step 3: Implement H.264 SPS parser**

Key implementation details:
- `ParseSPS(data []byte) (*SPSInfo, error)` — parse SPS NAL to extract width, height, profile, level
- `ExtractNALUs(data []byte) [][]byte` — split Annex-B byte stream into individual NAL units
- Uses Exp-Golomb decoding for SPS fields
- `SPSInfo` struct: `Width, Height, Profile, Level int`

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/codec/h264/ -v`
Expected: ALL PASS

- [ ] **Step 5: Write failing test for AAC AudioSpecificConfig parsing**

```go
// pkg/codec/aac/parser_test.go
package aac

import "testing"

func TestParseAudioSpecificConfig(t *testing.T) {
    // AAC-LC, 44100Hz, stereo: object_type=2, freq_index=4, channel=2
    // Binary: 00010 0100 0010 000 = 0x12 0x10
    config := []byte{0x12, 0x10}
    info, err := ParseAudioSpecificConfig(config)
    if err != nil {
        t.Fatalf("ParseAudioSpecificConfig error: %v", err)
    }
    if info.ObjectType != 2 {
        t.Errorf("expected object type 2 (AAC-LC), got %d", info.ObjectType)
    }
    if info.SampleRate != 44100 {
        t.Errorf("expected sample rate 44100, got %d", info.SampleRate)
    }
    if info.Channels != 2 {
        t.Errorf("expected channels 2, got %d", info.Channels)
    }
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./pkg/codec/aac/ -v`
Expected: FAIL

- [ ] **Step 7: Implement AAC AudioSpecificConfig parser**

Key implementation details:
- `ParseAudioSpecificConfig(data []byte) (*AACInfo, error)` — parse 2+ byte config
- `AACInfo` struct: `ObjectType, SampleRate, Channels int`
- Frequency index table: `{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}`

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./pkg/codec/aac/ -v`
Expected: ALL PASS

- [ ] **Step 9: Commit**

```bash
git add pkg/codec/
git commit -m "feat: add H.264 SPS parser and AAC AudioSpecificConfig parser"
```

---

### Task 3: Config System

**Files:**
- Create: `config/config.go`
- Create: `config/loader.go`
- Create: `configs/streamserver.yaml`
- Test: `config/config_test.go`

- [ ] **Step 1: Write failing test for config loading**

```go
// config/config_test.go
package config

import (
    "os"
    "path/filepath"
    "testing"
)

func TestLoadConfig(t *testing.T) {
    yaml := `
server:
  name: "test-server"
  log_level: debug
  drain_timeout: 10s

rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 512
  idle_timeout: 30s
  no_publisher_timeout: 15s
`
    dir := t.TempDir()
    path := filepath.Join(dir, "config.yaml")
    if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
        t.Fatal(err)
    }

    cfg, err := Load(path)
    if err != nil {
        t.Fatalf("Load error: %v", err)
    }
    if cfg.Server.Name != "test-server" {
        t.Errorf("expected name test-server, got %s", cfg.Server.Name)
    }
    if cfg.Server.LogLevel != "debug" {
        t.Errorf("expected log_level debug, got %s", cfg.Server.LogLevel)
    }
    if !cfg.RTMP.Enabled {
        t.Error("expected RTMP enabled")
    }
    if cfg.RTMP.Listen != ":1935" {
        t.Errorf("expected RTMP listen :1935, got %s", cfg.RTMP.Listen)
    }
    if cfg.Stream.RingBufferSize != 512 {
        t.Errorf("expected ring_buffer_size 512, got %d", cfg.Stream.RingBufferSize)
    }
}

func TestLoadConfigDefaults(t *testing.T) {
    yaml := `{}`
    dir := t.TempDir()
    path := filepath.Join(dir, "config.yaml")
    if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
        t.Fatal(err)
    }

    cfg, err := Load(path)
    if err != nil {
        t.Fatalf("Load error: %v", err)
    }
    if cfg.RTMP.Listen != ":1935" {
        t.Errorf("expected default RTMP listen :1935, got %s", cfg.RTMP.Listen)
    }
    if cfg.Stream.RingBufferSize != 1024 {
        t.Errorf("expected default ring_buffer_size 1024, got %d", cfg.Stream.RingBufferSize)
    }
}

func TestLoadConfigEnvExpansion(t *testing.T) {
    t.Setenv("TEST_JWT_SECRET", "mysecret123")
    yaml := `
auth:
  enabled: true
  publish:
    mode: "token"
    token:
      secret: "${TEST_JWT_SECRET}"
`
    dir := t.TempDir()
    path := filepath.Join(dir, "config.yaml")
    if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
        t.Fatal(err)
    }

    cfg, err := Load(path)
    if err != nil {
        t.Fatalf("Load error: %v", err)
    }
    if cfg.Auth.Publish.Token.Secret != "mysecret123" {
        t.Errorf("expected expanded secret mysecret123, got %s", cfg.Auth.Publish.Token.Secret)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./config/ -v`
Expected: FAIL

- [ ] **Step 3: Implement config structs and loader**

`config/config.go` — Full config struct hierarchy matching the spec's YAML schema. All fields with `yaml` tags. Duration fields use `time.Duration` with custom YAML unmarshal.

`config/loader.go` — `Load(path string) (*Config, error)`:
1. Read file
2. Expand `${ENV_VAR}` patterns using `os.ExpandEnv`
3. Apply defaults
4. Unmarshal YAML (use `gopkg.in/yaml.v3`)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./config/ -v`
Expected: ALL PASS

- [ ] **Step 5: Create default config file**

Create `configs/streamserver.yaml` matching the full config reference from the spec.

- [ ] **Step 6: Commit**

```bash
git add config/ configs/
git commit -m "feat: add config system with YAML loading, env expansion, and defaults"
```

---

### Task 4: Entry Point (main.go)

**Files:**
- Create: `cmd/liveforge/main.go`

- [ ] **Step 1: Write main.go that loads config and prints info**

```go
// cmd/liveforge/main.go
package main

import (
    "flag"
    "fmt"
    "log"
    "os"

    "github.com/im-pingo/liveforge/config"
)

var version = "dev"

func main() {
    configPath := flag.String("c", "configs/streamserver.yaml", "config file path")
    showVersion := flag.Bool("v", false, "show version")
    flag.Parse()

    if *showVersion {
        fmt.Printf("liveforge %s\n", version)
        os.Exit(0)
    }

    cfg, err := config.Load(*configPath)
    if err != nil {
        log.Fatalf("failed to load config: %v", err)
    }

    log.Printf("liveforge %s starting, server name: %s", version, cfg.Server.Name)
}
```

- [ ] **Step 2: Build and run**

Run: `go build -o bin/liveforge ./cmd/liveforge && ./bin/liveforge -v`
Expected: `liveforge dev`

Run: `./bin/liveforge -c configs/streamserver.yaml`
Expected: `liveforge dev starting, server name: streamserver-01`

- [ ] **Step 3: Commit**

```bash
git add cmd/
git commit -m "feat: add liveforge entry point with config loading"
```

---

## Chunk 2: EventBus + Module System + Server Bootstrap

### File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `core/event_bus.go` | EventBus with sync/async hook dispatch |
| Create | `core/module.go` | Module interface, HookRegistration, EventType enums |
| Create | `core/server.go` | Server struct — module registry, lifecycle, signal handling |

---

### Task 5: EventBus

**Files:**
- Create: `core/event_bus.go`
- Create: `core/module.go`
- Test: `core/event_bus_test.go`

- [ ] **Step 1: Write failing test for EventBus sync hook**

```go
// core/event_bus_test.go
package core

import (
    "errors"
    "sync/atomic"
    "testing"
    "time"
)

func TestEventBusSyncHook(t *testing.T) {
    bus := NewEventBus()
    var called int32

    bus.Register(HookRegistration{
        Event:    EventPublish,
        Mode:     HookSync,
        Priority: 10,
        Handler: func(ctx *EventContext) error {
            atomic.AddInt32(&called, 1)
            return nil
        },
    })

    err := bus.Emit(EventPublish, &EventContext{StreamKey: "live/test"})
    if err != nil {
        t.Errorf("unexpected error: %v", err)
    }
    if atomic.LoadInt32(&called) != 1 {
        t.Errorf("expected handler called once, got %d", called)
    }
}

func TestEventBusSyncHookReject(t *testing.T) {
    bus := NewEventBus()
    errAuth := errors.New("unauthorized")

    bus.Register(HookRegistration{
        Event:    EventPublish,
        Mode:     HookSync,
        Priority: 10,
        Handler: func(ctx *EventContext) error {
            return errAuth
        },
    })

    err := bus.Emit(EventPublish, &EventContext{StreamKey: "live/test"})
    if !errors.Is(err, errAuth) {
        t.Errorf("expected errAuth, got %v", err)
    }
}

func TestEventBusPriorityOrder(t *testing.T) {
    bus := NewEventBus()
    var order []int

    bus.Register(HookRegistration{
        Event: EventPublish, Mode: HookSync, Priority: 20,
        Handler: func(ctx *EventContext) error { order = append(order, 20); return nil },
    })
    bus.Register(HookRegistration{
        Event: EventPublish, Mode: HookSync, Priority: 10,
        Handler: func(ctx *EventContext) error { order = append(order, 10); return nil },
    })
    bus.Register(HookRegistration{
        Event: EventPublish, Mode: HookSync, Priority: 15,
        Handler: func(ctx *EventContext) error { order = append(order, 15); return nil },
    })

    _ = bus.Emit(EventPublish, &EventContext{})
    if len(order) != 3 || order[0] != 10 || order[1] != 15 || order[2] != 20 {
        t.Errorf("expected priority order [10,15,20], got %v", order)
    }
}

func TestEventBusAsyncHook(t *testing.T) {
    bus := NewEventBus()
    done := make(chan struct{})

    bus.Register(HookRegistration{
        Event: EventPublish, Mode: HookAsync, Priority: 90,
        Handler: func(ctx *EventContext) error {
            close(done)
            return nil
        },
    })

    err := bus.Emit(EventPublish, &EventContext{})
    if err != nil {
        t.Errorf("unexpected error: %v", err)
    }
    select {
    case <-done:
        // OK
    case <-time.After(1 * time.Second):
        t.Fatal("async handler was not called within 1s")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core/ -v -run TestEventBus`
Expected: FAIL

- [ ] **Step 3: Implement Module interface and EventBus**

`core/module.go`:
- `EventType` enum: `EventStreamCreate`, `EventStreamDestroy`, `EventPublish`, `EventPublishStop`, `EventRepublish`, `EventSubscribe`, `EventSubscribeStop`, `EventPublishAlive`, `EventSubscribeAlive`, `EventStreamAlive`, `EventVideoKeyframe`, `EventAudioHeader`, `EventForwardStart`, `EventForwardStop`, `EventOriginPullStart`, `EventOriginPullStop`, `EventSubscriberSkip`
- `HookMode`: `HookSync`, `HookAsync`
- `EventContext` struct: `StreamKey string`, `Protocol string`, `RemoteAddr string`, `Extra map[string]any`
- `Module` interface: `Name() string`, `Init(s *Server) error`, `Hooks() []HookRegistration`, `Close() error`
- `HookRegistration` struct: `Event EventType`, `Mode HookMode`, `Priority int`, `Handler EventHandler`
- `EventHandler` type: `func(ctx *EventContext) error`

`core/event_bus.go`:
- `EventBus` struct with `map[EventType][]HookRegistration` (sorted by priority)
- `Register(h HookRegistration)` — insert sorted by priority
- `Emit(event EventType, ctx *EventContext) error` — run sync hooks in order; if any returns error, abort and return it. Then fire async hooks in goroutines.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core/ -v -run TestEventBus`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add core/module.go core/event_bus.go core/event_bus_test.go
git commit -m "feat: add EventBus with sync/async hooks and priority ordering"
```

---

### Task 6: Server Bootstrap + Graceful Shutdown

**Files:**
- Create: `core/server.go`
- Test: `core/server_test.go`

- [ ] **Step 1: Write failing test for Server module registration and lifecycle**

```go
// core/server_test.go
package core

import (
    "testing"

    "github.com/im-pingo/liveforge/config"
)

type mockModule struct {
    name    string
    inited  bool
    closed  bool
    hooks   []HookRegistration
}

func (m *mockModule) Name() string                { return m.name }
func (m *mockModule) Init(s *Server) error         { m.inited = true; return nil }
func (m *mockModule) Hooks() []HookRegistration    { return m.hooks }
func (m *mockModule) Close() error                 { m.closed = true; return nil }

func TestServerModuleLifecycle(t *testing.T) {
    cfg := &config.Config{}
    cfg.Server.Name = "test"
    s := NewServer(cfg)

    mod := &mockModule{name: "test-module"}
    s.RegisterModule(mod)

    if err := s.Init(); err != nil {
        t.Fatalf("Init error: %v", err)
    }
    if !mod.inited {
        t.Error("expected module to be inited")
    }

    s.Shutdown()
    if !mod.closed {
        t.Error("expected module to be closed")
    }
}

func TestServerModuleCloseReverseOrder(t *testing.T) {
    cfg := &config.Config{}
    s := NewServer(cfg)

    var order []string
    s.RegisterModule(&orderTrackModule{name: "first", order: &order})
    s.RegisterModule(&orderTrackModule{name: "second", order: &order})

    _ = s.Init()
    s.Shutdown()

    if len(order) != 2 || order[0] != "second" || order[1] != "first" {
        t.Errorf("expected close order [second, first], got %v", order)
    }
}

type orderTrackModule struct {
    name  string
    order *[]string
}

func (m *orderTrackModule) Name() string              { return m.name }
func (m *orderTrackModule) Init(s *Server) error       { return nil }
func (m *orderTrackModule) Hooks() []HookRegistration  { return nil }
func (m *orderTrackModule) Close() error               { *m.order = append(*m.order, m.name); return nil }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core/ -v -run TestServer`
Expected: FAIL

- [ ] **Step 3: Implement Server**

`core/server.go`:
- `Server` struct: `Config`, `EventBus`, `modules []Module`, `StreamHub` (nil for now)
- `NewServer(cfg *config.Config) *Server`
- `RegisterModule(m Module)` — append to modules list
- `Init() error` — init each module in order, register their hooks on the EventBus
- `Shutdown()` — close modules in reverse order
- `Run() error` — call Init, then block on signal (SIGINT/SIGTERM), then Shutdown
- `EventBus() *EventBus` — getter for modules to use

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core/ -v -run TestServer`
Expected: ALL PASS

- [ ] **Step 5: Update main.go to use Server**

```go
// cmd/liveforge/main.go — update main() to:
// 1. Create Server with config
// 2. server.Run() (blocks until signal)
```

- [ ] **Step 6: Build and test signal handling**

Run: `go build -o bin/liveforge ./cmd/liveforge && ./bin/liveforge -c configs/streamserver.yaml &`
Then: `kill -SIGINT $!`
Expected: clean shutdown log message

- [ ] **Step 7: Commit**

```bash
git add core/server.go core/server_test.go cmd/liveforge/main.go
git commit -m "feat: add Server bootstrap with module lifecycle and graceful shutdown"
```

---

## Chunk 3: StreamHub + Stream + RingBuffer

### File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `pkg/util/ringbuffer.go` | Generic lock-free SPMC ring buffer |
| Create | `core/stream_hub.go` | StreamHub — stream lifecycle management |
| Create | `core/stream.go` | Stream — state machine, publish/subscribe coordination |
| Create | `core/publisher.go` | Publisher interface |
| Create | `core/subscriber.go` | Subscriber interface, SubscribeOptions |

---

### Task 7: SPMC Ring Buffer

**Files:**
- Create: `pkg/util/ringbuffer.go`
- Test: `pkg/util/ringbuffer_test.go`

- [ ] **Step 1: Write failing test for RingBuffer basic write/read**

```go
// pkg/util/ringbuffer_test.go
package util

import (
    "testing"
)

func TestRingBufferWriteRead(t *testing.T) {
    rb := NewRingBuffer[int](4)
    rb.Write(10)
    rb.Write(20)
    rb.Write(30)

    reader := rb.NewReader()
    val, ok := reader.Read()
    if !ok || val != 10 {
        t.Errorf("expected (10, true), got (%v, %v)", val, ok)
    }
    val, ok = reader.Read()
    if !ok || val != 20 {
        t.Errorf("expected (20, true), got (%v, %v)", val, ok)
    }
    val, ok = reader.Read()
    if !ok || val != 30 {
        t.Errorf("expected (30, true), got (%v, %v)", val, ok)
    }
    // No more data
    _, ok = reader.TryRead()
    if ok {
        t.Error("expected no more data")
    }
}

func TestRingBufferOverflow(t *testing.T) {
    rb := NewRingBuffer[int](4)
    // Write 6 items into size-4 buffer — oldest 2 should be overwritten
    for i := 0; i < 6; i++ {
        rb.Write(i)
    }
    reader := rb.NewReader()
    // Reader should start from oldest available (2)
    val, ok := reader.Read()
    if !ok || val != 2 {
        t.Errorf("expected (2, true), got (%v, %v)", val, ok)
    }
}

func TestRingBufferMultipleReaders(t *testing.T) {
    rb := NewRingBuffer[int](8)
    rb.Write(1)
    rb.Write(2)
    rb.Write(3)

    r1 := rb.NewReader()
    r2 := rb.NewReader()

    v1, _ := r1.Read()
    v2, _ := r2.Read()
    if v1 != v2 {
        t.Errorf("readers should get same first value: r1=%d, r2=%d", v1, v2)
    }

    // r1 advances, r2 stays
    r1.Read()
    v1, _ = r1.Read()
    v2, _ = r2.Read()
    if v1 != 3 || v2 != 2 {
        t.Errorf("expected r1=3, r2=2, got r1=%d, r2=%d", v1, v2)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/util/ -v -run TestRingBuffer`
Expected: FAIL

- [ ] **Step 3: Implement generic SPMC RingBuffer**

`pkg/util/ringbuffer.go`:
- `RingBuffer[T any]` struct: `buf []T`, `size int`, `writeCursor atomic.Int64`
- `NewRingBuffer[T](size int) *RingBuffer[T]`
- `Write(val T)` — write at `writeCursor % size`, advance cursor atomically
- `NewReader() *RingReader[T]` — creates reader starting at oldest available position
- `RingReader[T]` struct: `rb *RingBuffer[T]`, `readCursor int64`
- `Read() (T, bool)` — blocking read: waits on an internal `chan struct{}` signal when caught up with writer. Writer calls `notify()` after each write to wake all waiting readers.
- `TryRead() (T, bool)` — non-blocking, returns false if caught up
- Internal `signal chan struct{}` (capacity 1) — writer does non-blocking send after each write; readers select on this channel with timeout in `Read()`

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/util/ -v -run TestRingBuffer`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/util/ringbuffer.go pkg/util/ringbuffer_test.go
git commit -m "feat: add generic lock-free SPMC ring buffer"
```

---

### Task 8: Publisher and Subscriber Interfaces

**Files:**
- Create: `core/publisher.go`
- Create: `core/subscriber.go`

- [ ] **Step 1: Write Publisher and Subscriber interfaces**

```go
// core/publisher.go
package core

import "github.com/im-pingo/liveforge/pkg/avframe"

// Publisher represents a stream source that feeds AVFrames into a Stream.
type Publisher interface {
    // ID returns a unique identifier for this publisher.
    ID() string
    // MediaInfo returns the codec information for this publisher.
    MediaInfo() *avframe.MediaInfo
    // Close disconnects the publisher.
    Close() error
}

// core/subscriber.go
package core

// StartMode determines how a subscriber receives initial frames.
type StartMode uint8

const (
    StartModeGOP      StartMode = iota + 1 // Start from nearest keyframe
    StartModeRealtime                       // Start from current frame
)

// FeedbackMode determines how subscriber feedback is handled.
type FeedbackMode uint8

const (
    FeedbackAuto        FeedbackMode = iota // Default: auto-select based on subscriber count
    FeedbackPassthrough                      // Forward FB directly to publisher
    FeedbackAggregate                        // Aggregate FBs before forwarding
    FeedbackDrop                             // Discard FB
    FeedbackServerSide                       // Server-side adaptation, don't forward
)

// LayerPrefer determines which simulcast layer to subscribe.
type LayerPrefer uint8

const (
    LayerAuto LayerPrefer = iota
    LayerHigh
    LayerLow
)

// SubscribeOptions configures how a subscriber receives data.
type SubscribeOptions struct {
    StartMode    StartMode
    FeedbackMode FeedbackMode
    VideoLayer   LayerPrefer
    AudioEnabled bool
}

// DefaultSubscribeOptions returns sensible defaults.
func DefaultSubscribeOptions() SubscribeOptions {
    return SubscribeOptions{
        StartMode:    StartModeGOP,
        FeedbackMode: FeedbackAuto,
        VideoLayer:   LayerAuto,
        AudioEnabled: true,
    }
}

// Subscriber represents a stream consumer that receives muxed data.
type Subscriber interface {
    ID() string
    Options() SubscribeOptions
    // OnData is called when a new muxed packet is available (FLV tag, TS packet, etc).
    // The implementation sends it over the network.
    OnData(data []byte) error
    Close() error
}
```

- [ ] **Step 2: Add MediaInfo to avframe package**

```go
// pkg/avframe/codec_info.go
package avframe

// MediaInfo describes the codec configuration of a stream.
type MediaInfo struct {
    VideoCodec        CodecType
    AudioCodec        CodecType
    Width             int
    Height            int
    VideoSequenceHeader []byte // SPS/PPS for H.264, VPS/SPS/PPS for H.265
    AudioSequenceHeader []byte // AudioSpecificConfig for AAC, etc.
    SampleRate        int
    Channels          int
}

func (m *MediaInfo) HasVideo() bool { return m.VideoCodec != 0 }
func (m *MediaInfo) HasAudio() bool { return m.AudioCodec != 0 }
```

- [ ] **Step 3: Commit**

```bash
git add core/publisher.go core/subscriber.go pkg/avframe/codec_info.go
git commit -m "feat: add Publisher/Subscriber interfaces and MediaInfo"
```

---

### Task 9: Stream + StreamHub

**Files:**
- Create: `core/stream.go`
- Create: `core/stream_hub.go`
- Test: `core/stream_test.go`
- Test: `core/stream_hub_test.go`

- [ ] **Step 1: Write failing test for Stream state machine**

```go
// core/stream_test.go
package core

import (
    "testing"
    "time"

    "github.com/im-pingo/liveforge/config"
    "github.com/im-pingo/liveforge/pkg/avframe"
)

func newTestStreamConfig() config.StreamConfig {
    return config.StreamConfig{
        GOPCache:           true,
        GOPCacheNum:        1,
        AudioCacheMs:       1000,
        RingBufferSize:     256,
        IdleTimeout:        5 * time.Second,
        NoPublisherTimeout: 3 * time.Second,
    }
}

type testPublisher struct {
    id    string
    info  *avframe.MediaInfo
}

func (p *testPublisher) ID() string                  { return p.id }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *testPublisher) Close() error                 { return nil }

func TestStreamStateTransitions(t *testing.T) {
    bus := NewEventBus()
    s := NewStream("live/test", newTestStreamConfig(), bus)

    if s.State() != StreamStateIdle {
        t.Fatalf("expected idle, got %v", s.State())
    }

    pub := &testPublisher{
        id:   "pub1",
        info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
    }
    err := s.SetPublisher(pub)
    if err != nil {
        t.Fatalf("SetPublisher error: %v", err)
    }
    if s.State() != StreamStatePublishing {
        t.Fatalf("expected publishing, got %v", s.State())
    }

    s.RemovePublisher()
    if s.State() != StreamStateNoPublisher {
        t.Fatalf("expected no_publisher, got %v", s.State())
    }
}

func TestStreamRejectDuplicatePublisher(t *testing.T) {
    bus := NewEventBus()
    s := NewStream("live/test", newTestStreamConfig(), bus)

    pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
    pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}

    _ = s.SetPublisher(pub1)
    err := s.SetPublisher(pub2)
    if err == nil {
        t.Error("expected error for duplicate publisher")
    }
}

func TestStreamWriteAndReadFrames(t *testing.T) {
    bus := NewEventBus()
    s := NewStream("live/test", newTestStreamConfig(), bus)

    pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
    _ = s.SetPublisher(pub)

    // Write frames
    keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 0x01})
    s.WriteFrame(keyframe)

    audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{0xFF, 0x01})
    s.WriteFrame(audio)

    inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41, 0x01})
    s.WriteFrame(inter)

    // Verify GOP cache
    gop := s.GOPCache()
    if len(gop) < 1 {
        t.Fatal("expected at least 1 frame in GOP cache")
    }
    if gop[0].FrameType != avframe.FrameTypeKeyframe {
        t.Error("first frame in GOP should be keyframe")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core/ -v -run TestStream`
Expected: FAIL

- [ ] **Step 3: Implement Stream**

`core/stream.go`:
- `StreamState` enum: `StreamStateIdle`, `StreamStateWaitingPull`, `StreamStatePublishing`, `StreamStateNoPublisher`, `StreamStateDestroying`
- `Stream` struct: `key string`, `state StreamState`, `mu sync.RWMutex`, `publisher Publisher`, `config StreamConfig`, `ringBuffer *util.RingBuffer[*avframe.AVFrame]`, `gopCache []*avframe.AVFrame`, `audioCache []*avframe.AVFrame`, `sequenceHeaders map[avframe.MediaType]*avframe.AVFrame`, `eventBus *EventBus`, `noPublisherTimer *time.Timer`
- `NewStream(key string, cfg StreamConfig, bus *EventBus) *Stream`
- `State() StreamState`
- `SetPublisher(pub Publisher) error` — reject if already has publisher
- `RemovePublisher()` — transition to NoPublisher, start timer
- `WriteFrame(frame *AVFrame)` — write to ring buffer, update GOP/audio cache
- `GOPCache() []*AVFrame` — return copy of current GOP cache
- GOP cache logic: on keyframe, replace cache; on interframe, append to current GOP

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core/ -v -run TestStream`
Expected: ALL PASS

- [ ] **Step 4b: Write test for noPublisherTimeout**

```go
func TestStreamNoPublisherTimeout(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    cfg.NoPublisherTimeout = 100 * time.Millisecond // short timeout for test
    s := NewStream("live/timeout", cfg, bus)

    pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
    _ = s.SetPublisher(pub)
    s.RemovePublisher()

    if s.State() != StreamStateNoPublisher {
        t.Fatalf("expected no_publisher, got %v", s.State())
    }

    // Wait for timeout
    time.Sleep(200 * time.Millisecond)
    if s.State() != StreamStateDestroying {
        t.Errorf("expected destroying after timeout, got %v", s.State())
    }
}

func TestStreamRepublishBeforeTimeout(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    cfg.NoPublisherTimeout = 500 * time.Millisecond
    s := NewStream("live/republish", cfg, bus)

    pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
    _ = s.SetPublisher(pub1)
    s.RemovePublisher()

    // Republish before timeout with same codec
    pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
    err := s.SetPublisher(pub2)
    if err != nil {
        t.Fatalf("republish should succeed: %v", err)
    }
    if s.State() != StreamStatePublishing {
        t.Errorf("expected publishing after republish, got %v", s.State())
    }
}
```

- [ ] **Step 4c: Run timeout tests**

Run: `go test ./core/ -v -run TestStreamNoPublisher -run TestStreamRepublish`
Expected: ALL PASS

- [ ] **Step 5: Write failing test for StreamHub**

```go
// core/stream_hub_test.go
package core

import (
    "testing"

    "github.com/im-pingo/liveforge/config"
)

func TestStreamHubCreateAndFind(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    hub := NewStreamHub(cfg, bus)

    s1 := hub.GetOrCreate("live/room1")
    s2 := hub.GetOrCreate("live/room1")
    if s1 != s2 {
        t.Error("expected same stream instance for same key")
    }

    s3 := hub.GetOrCreate("live/room2")
    if s1 == s3 {
        t.Error("expected different stream for different key")
    }

    if hub.Count() != 2 {
        t.Errorf("expected 2 streams, got %d", hub.Count())
    }
}

func TestStreamHubRemove(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    hub := NewStreamHub(cfg, bus)

    hub.GetOrCreate("live/room1")
    hub.Remove("live/room1")

    if hub.Count() != 0 {
        t.Errorf("expected 0 streams after remove, got %d", hub.Count())
    }
}

func TestStreamHubList(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    hub := NewStreamHub(cfg, bus)

    hub.GetOrCreate("live/a")
    hub.GetOrCreate("live/b")

    keys := hub.Keys()
    if len(keys) != 2 {
        t.Errorf("expected 2 keys, got %d", len(keys))
    }
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./core/ -v -run TestStreamHub`
Expected: FAIL

- [ ] **Step 7: Implement StreamHub**

`core/stream_hub.go`:
- `StreamHub` struct: `streams sync.Map` (key → *Stream), `config StreamConfig`, `eventBus *EventBus`
- `NewStreamHub(cfg StreamConfig, bus *EventBus) *StreamHub`
- `GetOrCreate(key string) *Stream` — atomic get-or-create
- `Find(key string) (*Stream, bool)`
- `Remove(key string)`
- `Count() int`
- `Keys() []string`

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./core/ -v -run TestStreamHub`
Expected: ALL PASS

- [ ] **Step 9: Commit**

```bash
git add core/stream.go core/stream_test.go core/stream_hub.go core/stream_hub_test.go core/publisher.go core/subscriber.go
git commit -m "feat: add Stream with state machine, GOP cache, and StreamHub"
```

---

## Chunk 4: FLV Muxer/Demuxer

### File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `pkg/muxer/flv/types.go` | FLV tag types, constants |
| Create | `pkg/muxer/flv/demuxer.go` | FLV demuxer — parse FLV tags into AVFrames |
| Create | `pkg/muxer/flv/muxer.go` | FLV muxer — pack AVFrames into FLV tags |

---

### Task 10: FLV Types and Constants

**Files:**
- Create: `pkg/muxer/flv/types.go`

- [ ] **Step 1: Write FLV types**

FLV tag types (8=audio, 9=video, 18=script), codec IDs, frame type mappings, FLV header bytes. Constants only, no logic.

- [ ] **Step 2: Commit**

```bash
git add pkg/muxer/flv/types.go
git commit -m "feat: add FLV format constants and type definitions"
```

---

### Task 11: FLV Demuxer

**Files:**
- Create: `pkg/muxer/flv/demuxer.go`
- Test: `pkg/muxer/flv/demuxer_test.go`

- [ ] **Step 1: Write failing test for FLV tag parsing**

Test with known FLV tag bytes (video keyframe H.264, audio AAC) → verify correct AVFrame output with correct codec, frame type, DTS, payload.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/muxer/flv/ -v -run TestDemux`
Expected: FAIL

- [ ] **Step 3: Implement FLV demuxer**

`pkg/muxer/flv/demuxer.go`:
- `Demuxer` struct: stateful parser for FLV byte stream
- `ReadTag(r io.Reader) (*avframe.AVFrame, error)` — read one FLV tag, convert to AVFrame
- Handle: FLV header (9-byte + PreviousTagSize), video tags (H.264 NALU extraction), audio tags (AAC raw frame extraction), script data (metadata)
- For H.264: detect sequence header (AVC decoder config record) vs NALU
- For AAC: detect sequence header (AudioSpecificConfig) vs raw frame

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/muxer/flv/ -v -run TestDemux`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/muxer/flv/demuxer.go pkg/muxer/flv/demuxer_test.go
git commit -m "feat: add FLV demuxer — parse FLV tags into AVFrames"
```

---

### Task 12: FLV Muxer

**Files:**
- Create: `pkg/muxer/flv/muxer.go`
- Test: `pkg/muxer/flv/muxer_test.go`

- [ ] **Step 1: Write failing test for FLV muxing**

Create AVFrames (H.264 keyframe, AAC audio) → mux to FLV bytes → verify FLV header + correct tag structure. Round-trip test: mux → demux → compare.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/muxer/flv/ -v -run TestMux`
Expected: FAIL

- [ ] **Step 3: Implement FLV muxer**

`pkg/muxer/flv/muxer.go`:
- `Muxer` struct: tracks whether header/sequence headers have been written
- `WriteHeader(w io.Writer, hasVideo, hasAudio bool) error` — 9-byte FLV header + PreviousTagSize0
- `WriteFrame(w io.Writer, frame *avframe.AVFrame) error` — convert AVFrame to FLV tag bytes
- For H.264: wrap in AVC NALU format (length-prefix), handle sequence header vs data
- For AAC: wrap in AAC raw format, handle sequence header vs data
- All tags include: tag type, data size, timestamp (with extension), stream ID, previous tag size

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/muxer/flv/ -v -run TestMux`
Expected: ALL PASS

- [ ] **Step 5: Run round-trip test**

Run: `go test ./pkg/muxer/flv/ -v`
Expected: ALL PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/muxer/flv/muxer.go pkg/muxer/flv/muxer_test.go
git commit -m "feat: add FLV muxer — pack AVFrames into FLV tags"
```

---

## Chunk 5: RTMP Protocol Module

### File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `module/rtmp/handshake.go` | RTMP C0/C1/C2/S0/S1/S2 handshake |
| Create | `module/rtmp/chunk.go` | RTMP chunk stream read/write |
| Create | `module/rtmp/message.go` | RTMP message types, AMF0 encoding |
| Create | `module/rtmp/handler.go` | RTMP connection handler — publish/play command processing |
| Create | `module/rtmp/server.go` | TCP listener, accept loop, register as Module |
| Create | `module/rtmp/publisher.go` | RTMP Publisher — demux incoming RTMP to AVFrames |
| Create | `module/rtmp/subscriber.go` | RTMP Subscriber — mux AVFrames to RTMP chunks |

---

### Task 13: RTMP Handshake

**Files:**
- Create: `module/rtmp/handshake.go`
- Test: `module/rtmp/handshake_test.go`

- [ ] **Step 1: Write failing test for RTMP handshake**

Test simple handshake (no encryption): C0C1 → S0S1S2 → C2. Use `net.Pipe()` to simulate connection. Verify handshake completes without error and correct byte lengths.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./module/rtmp/ -v -run TestHandshake`
Expected: FAIL

- [ ] **Step 3: Implement RTMP handshake**

Simple handshake (version 3, no digest/encryption):
- Read C0 (1 byte, version) + C1 (1536 bytes, time + random)
- Write S0 (1 byte) + S1 (1536 bytes) + S2 (echo of C1)
- Read C2 (1536 bytes)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./module/rtmp/ -v -run TestHandshake`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add module/rtmp/handshake.go module/rtmp/handshake_test.go
git commit -m "feat: add RTMP simple handshake"
```

---

### Task 14: RTMP Chunk Stream

**Files:**
- Create: `module/rtmp/chunk.go`
- Test: `module/rtmp/chunk_test.go`

- [ ] **Step 1: Write failing test for chunk read/write**

Test: write an RTMP message as chunks with default chunk size (128), read them back, verify reassembled message matches original. Test with message larger than chunk size to verify multi-chunk splitting.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./module/rtmp/ -v -run TestChunk`
Expected: FAIL

- [ ] **Step 3: Implement RTMP chunk stream**

`module/rtmp/chunk.go`:
- `ChunkReader` struct: `r io.Reader`, `chunkSize int`, `prevHeaders map[uint32]*ChunkHeader` (for fmt 1/2/3 delta decoding)
- `ReadChunk() (*ChunkHeader, []byte, error)` — read basic header (fmt + csid), message header (varies by fmt), and chunk data
- `ChunkWriter` struct: `w io.Writer`, `chunkSize int`, `prevHeaders map[uint32]*ChunkHeader`
- `WriteMessage(csid uint32, msg *Message) error` — split message into chunks, write with appropriate fmt headers
- Handle all 4 fmt types (0, 1, 2, 3)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./module/rtmp/ -v -run TestChunk`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add module/rtmp/chunk.go module/rtmp/chunk_test.go
git commit -m "feat: add RTMP chunk stream reader and writer"
```

---

### Task 15: RTMP Messages + AMF0

**Files:**
- Create: `module/rtmp/message.go`
- Create: `module/rtmp/amf0.go`
- Test: `module/rtmp/amf0_test.go`

- [ ] **Step 1: Write failing test for AMF0 encoding/decoding**

Test: encode/decode AMF0 string, number, boolean, object, null. Round-trip test.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./module/rtmp/ -v -run TestAMF0`
Expected: FAIL

- [ ] **Step 3: Implement AMF0 and message types**

`module/rtmp/amf0.go`:
- `AMF0Encode(vals ...any) ([]byte, error)` — encode Go values to AMF0 bytes
- `AMF0Decode(data []byte) ([]any, error)` — decode AMF0 bytes to Go values
- Support types: number (float64), boolean, string, object (map[string]any), null

`module/rtmp/message.go`:
- Message type constants: `MsgSetChunkSize=1`, `MsgAbort=2`, `MsgAck=3`, `MsgUserControl=4`, `MsgWindowAckSize=5`, `MsgSetPeerBandwidth=6`, `MsgAudio=8`, `MsgVideo=9`, `MsgAMF0Command=20`, `MsgAMF0Data=18`
- `Message` struct: `TypeID uint8`, `Length uint32`, `Timestamp uint32`, `StreamID uint32`, `Payload []byte`

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./module/rtmp/ -v -run TestAMF0`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add module/rtmp/message.go module/rtmp/amf0.go module/rtmp/amf0_test.go
git commit -m "feat: add RTMP message types and AMF0 encoder/decoder"
```

---

### Task 16: RTMP Connection Handler

**Files:**
- Create: `module/rtmp/handler.go`
- Test: `module/rtmp/handler_test.go`

- [ ] **Step 1: Write failing test for RTMP command handling**

Test with mock connection: simulate connect → createStream → publish command sequence. Verify handler creates a publisher on the StreamHub. Then simulate connect → createStream → play → verify subscriber created.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./module/rtmp/ -v -run TestHandler`
Expected: FAIL

- [ ] **Step 3: Implement RTMP handler**

`module/rtmp/handler.go`:
- `Handler` struct: `conn net.Conn`, `chunkReader`, `chunkWriter`, `hub *core.StreamHub`, `eventBus *core.EventBus`
- `Handle()` — main loop: read messages, dispatch by type
- Command handlers:
  - `connect` → respond with `_result`, set window ack size, set peer bandwidth
  - `createStream` → respond with `_result` (stream ID)
  - `publish` → extract stream key, create Publisher on StreamHub, begin reading media messages
  - `play` → extract stream key, create Subscriber on StreamHub, begin writing media messages
  - `deleteStream`, `FCPublish`, `releaseStream` → handle gracefully

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./module/rtmp/ -v -run TestHandler`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add module/rtmp/handler.go module/rtmp/handler_test.go
git commit -m "feat: add RTMP connection handler with publish/play commands"
```

---

### Task 17: RTMP Publisher + Subscriber

**Files:**
- Create: `module/rtmp/publisher.go`
- Create: `module/rtmp/subscriber.go`
- Test: `module/rtmp/publisher_test.go`
- Test: `module/rtmp/subscriber_test.go`

- [ ] **Step 1: Write failing test for RTMP publisher frame extraction**

Test: feed raw RTMP video/audio message payloads to publisher → verify it produces correct AVFrames with proper codec detection, frame type, timestamps.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./module/rtmp/ -v -run TestPublisher`
Expected: FAIL

- [ ] **Step 3: Implement RTMP Publisher**

`module/rtmp/publisher.go`:
- Implements `core.Publisher` interface
- `ReadLoop()` — reads RTMP messages from the connection, converts video/audio messages to AVFrames using FLV demuxer (RTMP uses FLV tag format internally), writes to Stream

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./module/rtmp/ -v -run TestPublisher`
Expected: ALL PASS

- [ ] **Step 5: Write failing test for RTMP subscriber**

Test: create subscriber with a `net.Pipe()` connection, feed muxed FLV data via SharedBuffer, verify subscriber writes correct RTMP chunks to the pipe. Verify GOP cache is delivered first, then live frames. Verify backpressure handling (slow write → subscriber detects and handles).

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./module/rtmp/ -v -run TestSubscriber`
Expected: FAIL

- [ ] **Step 7: Implement RTMP Subscriber**

`module/rtmp/subscriber.go`:
- Implements `core.Subscriber` interface
- `OnData(data []byte) error` — receives muxed FLV tags from SharedBuffer
- `WriteLoop()` — reads muxed data, wraps in RTMP message, writes via chunk writer to connection

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./module/rtmp/ -v -run TestSubscriber`
Expected: ALL PASS

- [ ] **Step 9: Commit**

```bash
git add module/rtmp/publisher.go module/rtmp/subscriber.go module/rtmp/publisher_test.go module/rtmp/subscriber_test.go
git commit -m "feat: add RTMP publisher and subscriber with FLV frame conversion"
```

---

### Task 18: RTMP Server Module

**Files:**
- Create: `module/rtmp/server.go`
- Modify: `cmd/liveforge/main.go` — register RTMP module

- [ ] **Step 1: Implement RTMP server as Module**

`module/rtmp/server.go`:
- `RTMPModule` struct implementing `core.Module`
- `Name()` → "rtmp"
- `Init(s *Server)` → start TCP listener on configured port
- `Hooks()` → no hooks needed (RTMP creates publishers/subscribers directly)
- `Close()` → stop listener, close all connections
- Accept loop: for each connection → goroutine → handshake → Handler.Handle()

- [ ] **Step 2: Register RTMP module in main.go**

```go
// In main(), after creating server:
if cfg.RTMP.Enabled {
    s.RegisterModule(rtmp.NewModule())
}
```

- [ ] **Step 3: Build**

Run: `go build -o bin/liveforge ./cmd/liveforge`
Expected: build succeeds

- [ ] **Step 4: Commit**

```bash
git add module/rtmp/server.go cmd/liveforge/main.go
git commit -m "feat: add RTMP server module with TCP listener"
```

---

## Chunk 6: MuxerManager + SharedBuffer + Integration

### File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `core/muxer_manager.go` | Per-stream muxer lifecycle management |
| Create | `core/shared_buffer.go` | SharedBuffer for muxed output sharing |
| Modify | `core/stream.go` | Integrate MuxerManager, subscribe flow |

---

### Task 19: SharedBuffer

**Files:**
- Create: `core/shared_buffer.go`
- Test: `core/shared_buffer_test.go`

- [ ] **Step 1: Write failing test for SharedBuffer**

Test: create SharedBuffer, add readers, write muxed packets, verify all readers receive same data. Test slow reader skip behavior.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core/ -v -run TestSharedBuffer`
Expected: FAIL

- [ ] **Step 3: Implement SharedBuffer**

`core/shared_buffer.go`:
- Wraps `RingBuffer[[]byte]` for muxed packet distribution
- `NewSharedBuffer(size int) *SharedBuffer`
- `Write(packet []byte)`
- `NewReader() *SharedBufferReader`
- Reader tracks cursor, supports skip-to-keyframe on overflow

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core/ -v -run TestSharedBuffer`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add core/shared_buffer.go core/shared_buffer_test.go
git commit -m "feat: add SharedBuffer for muxed output distribution"
```

---

### Task 20: MuxerManager

**Files:**
- Create: `core/muxer_manager.go`
- Test: `core/muxer_manager_test.go`

- [ ] **Step 1: Write failing test for MuxerManager**

Test: request FLV muxer → creates on first request. Request again → returns same instance. Remove last subscriber → muxer destroyed.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core/ -v -run TestMuxerManager`
Expected: FAIL

- [ ] **Step 3: Implement MuxerManager**

`core/muxer_manager.go`:
- `MuxerManager` struct: `muxers map[string]*MuxerInstance` (keyed by format: "flv", "ts", "fmp4")
- `MuxerInstance`: muxer + SharedBuffer + subscriber count
- `GetOrCreateMuxer(format string, stream *Stream) *SharedBufferReader` — create muxer if needed, start mux goroutine, return a reader
- `ReleaseMuxer(format string)` — decrement subscriber count, destroy if zero
- Mux goroutine: reads from Stream's RingBuffer → muxes → writes to SharedBuffer

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core/ -v -run TestMuxerManager`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add core/muxer_manager.go core/muxer_manager_test.go
git commit -m "feat: add MuxerManager for per-protocol muxer lifecycle"
```

---

### Task 21: Stream Subscribe Integration

**Files:**
- Modify: `core/stream.go` — add AddSubscriber/RemoveSubscriber methods
- Test: `core/stream_test.go` — add subscribe integration tests

- [ ] **Step 1: Write failing test for full publish → subscribe flow**

Test: create stream, set publisher, write frames, add subscriber, verify subscriber receives frames from GOP cache onward.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core/ -v -run TestStreamSubscribe`
Expected: FAIL

- [ ] **Step 3: Implement subscribe flow in Stream**

- `AddSubscriber(sub Subscriber) error` — add subscriber, send GOP cache, start delivery goroutine
- `RemoveSubscriber(id string)` — remove subscriber, release muxer if last
- Delivery goroutine: read from SharedBuffer reader → call sub.OnFrame()

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core/ -v -run TestStreamSubscribe`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add core/stream.go core/stream_test.go
git commit -m "feat: add subscribe flow with GOP cache delivery and SharedBuffer"
```

---

### Task 22: End-to-End Integration Test

**Files:**
- Create: `test/integration/rtmp_relay_test.go`

- [ ] **Step 1: Write integration test — RTMP publish + subscribe**

Start RTMP server on random port, use RTMP client (or raw TCP) to simulate publish with known H.264+AAC frames, then simulate play and verify received frames match.

- [ ] **Step 2: Run test**

Run: `go test ./test/integration/ -v -run TestRTMPRelay -timeout 30s`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add test/
git commit -m "test: add RTMP relay end-to-end integration test"
```

---

### Task 23: Manual E2E Verification

- [ ] **Step 1: Build final binary**

Run: `go build -o bin/liveforge ./cmd/liveforge`

- [ ] **Step 2: Start server**

Run: `./bin/liveforge -c configs/streamserver.yaml`

- [ ] **Step 3: Push with ffmpeg**

Run: `ffmpeg -re -f lavfi -i testsrc=size=640x480:rate=30 -f lavfi -i sine=frequency=1000:sample_rate=44100 -c:v libx264 -c:a aac -f flv rtmp://localhost:1935/live/test`

- [ ] **Step 4: Pull with ffplay**

Run: `ffplay rtmp://localhost:1935/live/test`
Expected: see test pattern video with tone audio, smooth playback

- [ ] **Step 5: Verify logs**

Check server output for: stream create event, publish event, subscribe event, stats.

- [ ] **Step 6: Final commit and push**

```bash
git push origin main
```

---

## Summary

| Chunk | Tasks | Description |
|-------|-------|-------------|
| 1 | 1-4 | AVFrame types, codec parsers, config, main.go |
| 2 | 5-6 | EventBus, Module system, Server bootstrap |
| 3 | 7-9 | RingBuffer, Publisher/Subscriber interfaces, Stream, StreamHub |
| 4 | 10-12 | FLV types, demuxer, muxer |
| 5 | 13-18 | RTMP handshake, chunks, messages, AMF0, handler, publisher/subscriber, server |
| 6 | 19-23 | SharedBuffer, MuxerManager, subscribe flow, integration test, E2E verification |

**Total: 23 tasks, ~6 chunks**

Each chunk produces a working, testable increment. After all chunks, the server can relay RTMP streams end-to-end.
