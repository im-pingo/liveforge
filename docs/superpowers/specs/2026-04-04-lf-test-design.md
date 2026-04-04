# lf-test: Integrated Testing Toolkit for LiveForge

## Overview

`lf-test` is a unified CLI tool and Go library (`testkit/`) for end-to-end testing of the LiveForge streaming server. It covers multi-protocol push/play verification, audio/video quality analysis, authentication testing, and multi-cluster orchestration.

### Problem Statement

Current testing is limited to unit tests and in-memory integration tests. There are no real-network E2E tests, no AV quality validation, no cross-protocol verification, and no multi-node cluster testing. Coverage on critical modules (cluster 45.6%, gb28181 28.3%, sip 20.8%) is insufficient.

### Target Consumers

| Consumer | Requirements |
|----------|-------------|
| Developer | Local verification after code changes, human-readable output |
| CI/CD | Structured JSON output, exit codes, assertion-based pass/fail |
| AI Agent | JSON output with predictable schema, composable commands for troubleshooting |

### Design Principles

- **Deterministic**: pre-encoded test media, no runtime encoding, reproducible results
- **Zero external dependencies**: single Go binary, embedded test assets, no ffmpeg at runtime
- **Protocol-agnostic analysis**: `analyzer/` operates on `AVFrame` streams, decoupled from transport
- **Reusable library**: `testkit/` is a pure Go library importable by `go test`, not coupled to CLI

### Prerequisites

- **fmp4 demuxer**: `pkg/muxer/fmp4/` currently has only a muxer (box.go, muxer.go, init_segment.go, media_segment.go). An fmp4 demuxer must be written to support LL-HLS and DASH play. It needs to parse `moof`/`mdat` boxes, extract samples, and reconstruct DTS/PTS from `tfdt`/`trun` boxes. Estimated ~400-600 lines. This is a prerequisite before implementing LL-HLS and DASH players.
- **GB28181 push migration**: The existing `tools/gb28181-sim` uses `exec.Command("ffmpeg", ...)` to generate live streams at runtime. The consolidated `testkit/push/gb28181.go` must replace this with embedded `source.flv` demux → PS muxer (`pkg/muxer/ps/muxer.go`) → RTP packetization, eliminating the ffmpeg dependency.

### Scope Exclusions

- **Concurrent push/play stress testing** (multiple simultaneous pushers or players) is out of scope for v1. This toolkit focuses on functional correctness, not load testing.
- **Non-localhost cluster testing** is out of scope. All cluster nodes run on 127.0.0.1 with auto-allocated ports.

## Directory Structure

```
tools/
├── lf-test/
│   ├── main.go                    # cobra root command + subcommand registration
│   └── cmd/
│       ├── push.go                # push subcommand
│       ├── play.go                # play subcommand
│       ├── auth.go                # auth subcommand
│       └── cluster.go             # cluster subcommand
│
├── testkit/                       # Pure Go library, zero CLI dependency
│   ├── source/                    # Test source: embedded media + demux + loop + timestamp regen
│   │   ├── source.go              # Source interface: NextFrame() (*avframe.AVFrame, error)
│   │   ├── flv_source.go          # FLV demux implementation with go:embed
│   │   └── testdata/
│   │       └── source.flv         # Pre-encoded: H.264 Baseline + AAC-LC, 640x360, 30fps, 2s
│   │
│   ├── push/                      # Push engine
│   │   ├── pusher.go              # Pusher interface + protocol factory
│   │   ├── rtmp.go
│   │   ├── rtsp.go
│   │   ├── srt.go
│   │   ├── whip.go
│   │   └── gb28181.go             # Consolidates existing tools/gb28181-sim
│   │
│   ├── play/                      # Play engine
│   │   ├── player.go              # Player interface + protocol factory
│   │   ├── rtmp.go
│   │   ├── rtsp.go
│   │   ├── srt.go
│   │   ├── whep.go
│   │   ├── http_flv.go
│   │   ├── ws_flv.go
│   │   ├── hls.go                 # m3u8 parsing + TS segment download + demux
│   │   ├── llhls.go               # LL-HLS partial segment support
│   │   └── dash.go                # MPD parsing + segment download + demux
│   │
│   ├── analyzer/                  # AV analyzer (protocol-agnostic)
│   │   ├── analyzer.go            # Analyzer: receives AVFrame stream, outputs Report
│   │   ├── video.go               # fps, bitrate, keyframe interval, DTS monotonicity
│   │   ├── audio.go               # bitrate, sample rate, DTS monotonicity
│   │   ├── sync.go                # AV drift measurement
│   │   ├── stall.go               # Stall detection (DTS gap spikes)
│   │   └── codec.go               # Codec parameter validation (profile/level/resolution)
│   │
│   ├── auth/                      # Auth testing
│   │   ├── tester.go              # AuthTester: runs test case matrix, outputs AuthReport
│   │   ├── cases.go               # Case definitions: valid/invalid/expired/missing/wrong_stream/wrong_action
│   │   ├── rtmp.go                # RTMP ?token= auth
│   │   ├── rtsp.go                # RTSP Basic/Digest
│   │   ├── srt.go                 # SRT streamid auth
│   │   ├── whip.go                # WHIP/WHEP Bearer token
│   │   └── http.go                # HLS/HTTP-FLV/API token query param
│   │
│   ├── cluster/                   # Multi-cluster orchestration
│   │   ├── orchestrator.go        # Node lifecycle: start/stop/health check
│   │   ├── topology.go            # Topology model: Origin-Center-Edge
│   │   ├── config_gen.go          # Auto-generate per-node YAML from topology
│   │   └── scenario.go            # Scenario execution: push → wait relay → play → collect
│   │
│   └── report/                    # Report output
│       ├── report.go              # Report struct definitions
│       ├── json.go                # JSON formatter
│       ├── human.go               # Human-readable table formatter
│       └── assert.go              # Assertion engine: "video.fps>=29,audio.bitrate>=100000"
│
└── testdata/
    └── source.flv                 # Canonical copy (also go:embed'd in testkit/source)
```

## Core Interfaces

### Source (Test Media)

```go
// testkit/source/source.go
type Source interface {
    NextFrame() (*avframe.AVFrame, error) // Returns io.EOF when duration exceeded
    MediaInfo() *avframe.MediaInfo
    Reset()                                // Restart for looping
}
```

Embedded asset: `source.flv` — H.264 Baseline + AAC-LC, 640x360, 30fps, 500kbps video + 128kbps audio, 2 seconds (1 GOP).

**Looping behavior**: On loop restart, the source re-emits sequence headers (SPS/PPS for H.264, AudioSpecificConfig for AAC) so players joining mid-loop can decode. DTS/PTS accumulates from previous loop's end, ensuring global monotonic increase.

**Golden reference values** (for self-validation tests of the source itself):
- Video: 60 frames (30fps x 2s), ~125KB total video payload
- Audio: ~86 frames (44100/1024 x 2s), ~32KB total audio payload
- Total file size budget: <200KB (suitable for `go:embed`)

### Pusher

```go
// testkit/push/pusher.go
type PushConfig struct {
    Protocol string        // "rtmp", "rtsp", "srt", "whip", "gb28181"
    Target   string        // Target URL
    Duration time.Duration
    Token    string        // Optional auth token
}

type Pusher interface {
    Push(ctx context.Context, src source.Source, cfg PushConfig) error
}

func NewPusher(protocol string) (Pusher, error) // Protocol factory
```

### Player

```go
// testkit/play/player.go
type PlayConfig struct {
    Protocol string        // "rtmp","rtsp","srt","whep","hls","llhls","dash","httpflv","wsflv"
    URL      string
    Duration time.Duration
    Token    string        // Optional auth token
}

type FrameCallback func(frame *avframe.AVFrame)

type Player interface {
    Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error
}

func NewPlayer(protocol string) (Player, error) // Protocol factory
```

### Analyzer

```go
// testkit/analyzer/analyzer.go
type Analyzer struct { /* video, audio, sync, stall, codec sub-analyzers */ }

func New() *Analyzer
func (a *Analyzer) Feed(frame *avframe.AVFrame)
func (a *Analyzer) Report() *report.PlayReport
```

Data flow:

```
Player.Play(onFrame) ──frame──> Analyzer.Feed(frame) ──done──> Analyzer.Report()
```

## Report Schema

### Play Report (AV Analysis)

```go
type PlayReport struct {
    Video    VideoReport  `json:"video"`
    Audio    AudioReport  `json:"audio"`
    Sync     SyncReport   `json:"sync"`
    Stalls   []StallEvent `json:"stalls"`
    Codec      CodecReport `json:"codec"`
    DurationMs int64       `json:"duration_ms"`
    Error      string      `json:"error,omitempty"`
}

type VideoReport struct {
    Codec            string  `json:"codec"`
    Profile          string  `json:"profile"`
    Level            string  `json:"level"`
    Resolution       string  `json:"resolution"`
    FPS              float64 `json:"fps"`
    BitrateKbps      float64 `json:"bitrate_kbps"`
    KeyframeInterval float64 `json:"keyframe_interval"`
    DTSMonotonic     bool    `json:"dts_monotonic"`
    FrameCount       int64   `json:"frame_count"`
}

type AudioReport struct {
    Codec        string  `json:"codec"`
    SampleRate   int     `json:"sample_rate"`
    Channels     int     `json:"channels"`
    BitrateKbps  float64 `json:"bitrate_kbps"`
    DTSMonotonic bool    `json:"dts_monotonic"`
    FrameCount   int64   `json:"frame_count"`
}

type SyncReport struct {
    MaxDriftMs float64 `json:"max_drift_ms"`
    AvgDriftMs float64 `json:"avg_drift_ms"`
}

type StallEvent struct {
    TimestampMs int64   `json:"timestamp_ms"`
    GapMs       float64 `json:"gap_ms"`
    MediaType   string  `json:"media_type"`
}

type CodecReport struct {
    VideoMatch bool   `json:"video_match"`
    AudioMatch bool   `json:"audio_match"`
    Details    string `json:"details,omitempty"`
}
```

### Push Report

```go
type PushReport struct {
    Protocol   string `json:"protocol"`
    Target     string `json:"target"`
    DurationMs int64  `json:"duration_ms"`
    FramesSent int64  `json:"frames_sent"`
    BytesSent  int64  `json:"bytes_sent"`
}
```

### Auth Report

```go
type AuthReport struct {
    Total  int          `json:"total"`
    Passed int          `json:"passed"`
    Failed int          `json:"failed"`
    Cases  []CaseResult `json:"cases"`
}

type CaseResult struct {
    Protocol    string `json:"protocol"`
    Action      string `json:"action"`
    Credential  string `json:"credential"`
    ExpectAllow bool   `json:"expect_allow"`
    ActualAllow bool   `json:"actual_allow"`
    Pass        bool   `json:"pass"`
    Error       string `json:"error,omitempty"`
    LatencyMs   int64  `json:"latency_ms"`
}
```

Auth credential cases per protocol: `valid`, `expired`, `wrong_secret`, `missing`, `wrong_stream`, `wrong_action`.

Protocol-specific auth mechanisms:

| Protocol | Credential Transport |
|----------|---------------------|
| RTMP | `?token=xxx` in stream path |
| RTSP | `Authorization: Basic/Digest` header |
| SRT | Token embedded in `streamid` |
| WebRTC WHIP/WHEP | `Authorization: Bearer xxx` header |
| HLS/HTTP-FLV | `?token=xxx` query parameter |
| API | `Authorization: Bearer xxx` header |

### Cluster Report

```go
type ClusterReport struct {
    Topology string       `json:"topology"`
    Nodes    []NodeStatus `json:"nodes"`
    Push     *PushReport  `json:"push"`
    Play     *PlayReport  `json:"play"`
    RelayMs  int64        `json:"relay_ms"`
}

type NodeStatus struct {
    Name    string         `json:"name"`
    Role    string         `json:"role"`
    Healthy bool           `json:"healthy"`
    Ports   map[string]int `json:"ports"`
}
```

### Top-Level Report Wrapper

```go
type TopLevelReport struct {
    Command    string         `json:"command"`
    Timestamp  time.Time      `json:"timestamp"`
    DurationMs int64          `json:"duration_ms"`
    Pass       bool           `json:"pass"`
    Play       *PlayReport    `json:"play,omitempty"`
    Auth       *AuthReport    `json:"auth,omitempty"`
    Cluster    *ClusterReport `json:"cluster,omitempty"`
    Push       *PushReport    `json:"push,omitempty"`
    Errors     []ErrorDetail  `json:"errors,omitempty"`
}

// ErrorDetail provides structured error info for AI agent consumption.
type ErrorDetail struct {
    Code    string `json:"code"`    // CONNECT_FAILED, AUTH_REJECTED, TIMEOUT, DEMUX_ERROR, PROTOCOL_ERROR
    Message string `json:"message"`
}
```

**Duration fields**: All duration fields use `_ms` suffix with `int64` milliseconds (not `time.Duration`) for unambiguous JSON serialization. This applies to `DurationMs`, `RelayMs`, `LatencyMs`, `GapMs`, etc.

## Multi-Cluster Orchestration

### Topology Model

```go
type Topology struct {
    Nodes []NodeConfig `yaml:"nodes"`
    Links []Link       `yaml:"links"`
}

type NodeRole string // "origin", "center", "edge"

type NodeConfig struct {
    Name      string   `yaml:"name"`
    Role      NodeRole `yaml:"role"`
    Protocols []string `yaml:"protocols"`
}

type Link struct {
    From     string `yaml:"from"`
    To       string `yaml:"to"`
    Protocol string `yaml:"protocol"` // relay protocol between nodes
}
```

### Orchestrator Flow

1. Allocate ports for each node (auto-detect available ports starting from base 30000)
2. Generate per-node YAML configs with correct cluster forward/origin addresses
3. Start all node processes, wait for health check (`GET /health` on API port, timeout: 10s per node)
4. Start Pusher to origin node
5. Wait for stream propagation (poll `GET /api/v1/streams` on edge every 500ms, timeout: 15s)
6. Start Player from edge node + feed Analyzer
7. Wait for configured duration, stop Player/Pusher
8. Stop all nodes (SIGTERM, wait 5s, then SIGKILL)
9. Return ClusterReport

**Node name → URL resolution**: In cluster YAML config, `push.target` and `play.target` reference node names (e.g. `origin`, `edge`), not URLs. The orchestrator resolves these to `127.0.0.1:<allocated_port>` at runtime based on the node's allocated ports and the specified protocol.

### Built-in Topologies

| Name | Structure |
|------|-----------|
| `origin-edge` | push → origin → edge → play |
| `origin-center-edge` | push → origin → center → edge → play |
| `origin-multi-edge` | push → origin → N edges → play (parallel) |

Custom topologies via YAML config file (see CLI section).

### Config Generation

`GenerateNodeConfig()` maps topology roles to liveforge config:
- **origin**: `cluster.forward.enabled=true`, targets point to downstream node(s)
- **center**: `cluster.origin.enabled=true` (pull from upstream) + `cluster.forward.enabled=true` (push to downstream)
- **edge**: `cluster.origin.enabled=true`, pulls from upstream

## CLI Interface

### Commands

```
lf-test push    --protocol <proto> --target <url> [--duration 10s] [--token <t>] [--assert "..."] [--output json|human]
lf-test play    --protocol <proto> --url <url> [--duration 10s] [--token <t>] [--assert "..."] [--output json|human]
lf-test auth    [--standalone] [--server-rtmp <addr>] [--server-rtsp <addr>] ... [--secret <s>] [--stream <key>] [--output json|human]
lf-test cluster --topology <name|file.yaml> [--push-protocol <p>] [--play-protocol <p>] [--relay-protocol <p>] [--duration 10s] [--assert "..."] [--output json|human]
```

**`push --assert`**: Supports assertions on `PushReport` fields, e.g. `--assert "frames_sent>=300,bytes_sent>0"`.

**`auth --standalone`**: Starts a temporary liveforge server with auth enabled (JWT mode), runs the full auth test matrix against it, then shuts down. No external server setup required. Without `--standalone`, tests run against an already-running server specified by `--server-*` flags.

### Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` | auto (human if TTY, json if piped) | Output format |
| `--timeout` | 30s (60s for cluster) | Global timeout, kills everything if exceeded |
| `--verbose` | false | Debug logging to stderr |
| `--binary` | auto-detect | Path to liveforge server binary (for auth --standalone and cluster). Auto-detection: looks for `./bin/liveforge`, then `$GOPATH/bin/liveforge`, then `$(go env GOPATH)/bin/liveforge`. Can also be set via `LF_BINARY` env var. |

### Assertion Syntax

```
--assert "video.fps>=29,video.dts_monotonic==true,sync.max_drift_ms<100"
```

Supported operators: `>=`, `<=`, `>`, `<`, `==`, `!=`

Addressable fields: all JSON fields in report structs using dot notation.

**Type coercion rules**:
- Numeric fields (`float64`, `int64`): right-hand side parsed as float64. Example: `video.fps>=29`
- Boolean fields: right-hand side must be `true` or `false`. Example: `video.dts_monotonic==true`
- String fields: right-hand side is literal string, only `==` and `!=` supported. Example: `video.codec==H264`

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success, all assertions passed |
| 1 | Assertion failure |
| 2 | Runtime error |

### Cluster YAML Config Example

```yaml
topology:
  nodes:
    - name: origin
      role: origin
      protocols: [rtmp, rtsp]
    - name: center
      role: center
      protocols: [rtsp, srt]
    - name: edge
      role: edge
      protocols: [srt, http_stream]
  links:
    - { from: origin, to: center, protocol: rtsp }
    - { from: center, to: edge,   protocol: srt }

push:
  protocol: rtmp
  target: origin
  duration: 10s

play:
  protocol: hls
  target: edge
  duration: 8s

assert:
  - "video.fps >= 29"
  - "video.dts_monotonic == true"
  - "audio.dts_monotonic == true"
  - "sync.max_drift_ms < 200"
```

## Test Media Generation

One-time command (development only, not in code):

```bash
ffmpeg -f lavfi -i testsrc2=size=640x360:rate=30:duration=2 \
       -f lavfi -i sine=frequency=1000:duration=2:sample_rate=44100 \
       -c:v libx264 -profile:v baseline -preset fast -b:v 500k -g 30 \
       -c:a aac -b:a 128k \
       -f flv tools/testdata/source.flv
```

Embedded via `//go:embed testdata/source.flv` in `testkit/source/`.

## Protocol Coverage Matrix

### Push Protocols (5)

| Protocol | Transport | Reuses LiveForge Module |
|----------|-----------|------------------------|
| RTMP | TCP | `module/rtmp` (chunk codec, handshake) |
| RTSP | TCP+UDP | `module/rtsp` (SDP, session) + `pkg/rtp`. Note: existing code is server-side; client-side RTSP push/play must be extracted from `module/cluster/transport_rtsp.go` relay logic. |
| SRT | UDP (SRT) | `module/srt` |
| WebRTC WHIP | UDP (ICE) | `module/webrtc` |
| GB28181 | UDP (RTP) | `module/gb28181` + `module/sip` (consolidates `tools/gb28181-sim`, replaces ffmpeg with embedded source) |

### Play Protocols (9)

| Protocol | Transport | Reuses LiveForge Module |
|----------|-----------|------------------------|
| RTMP | TCP | `module/rtmp` |
| RTSP | TCP+UDP | `module/rtsp` + `pkg/rtp` |
| SRT | UDP (SRT) | `module/srt` |
| WebRTC WHEP | UDP (ICE) | `module/webrtc` |
| HLS | HTTP | `pkg/muxer/ts` (demux) |
| LL-HLS | HTTP | `pkg/muxer/fmp4` (demux — **must be written**, see Prerequisites) |
| DASH | HTTP | `pkg/muxer/fmp4` (demux — **must be written**, see Prerequisites) |
| HTTP-FLV | HTTP | `pkg/muxer/flv` (demux) |
| WS-FLV | WebSocket | `pkg/muxer/flv` (demux) |

## Documentation

Single comprehensive document at `docs/lf-test.md`:

1. **Quick Start** — 30-second onboarding for developers
2. **Command Reference** — Full parameter documentation per subcommand
3. **Assertion Syntax** — Available fields and operators
4. **Cluster Topologies** — Built-in topologies and custom YAML format
5. **CI/CD Integration** — GitHub Actions / shell script examples
6. **AI Agent Guide** — JSON output schema, typical invocation examples, exit code interpretation, command composition for troubleshooting
7. **Troubleshooting** — Common errors and resolution steps
