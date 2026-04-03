# Cluster Multi-Protocol Relay Design

> **Date**: 2026-04-03
> **Status**: Draft
> **Branch**: `feat/cluster-multi-protocol`

## Overview

Extend the cluster module to support multi-protocol relay between nodes, selectable via URL scheme. Currently, cluster forwarding (push) and origin pull only support RTMP. This design adds SRT, RTSP, and raw RTP direct transport, with a plugin-based architecture for extensibility.

**Key goals:**
- LAN (same datacenter): RTP direct relay for lowest latency (~30-50ms), preparing for future stream mixing
- WAN (cross datacenter): SRT/RTMP/RTSP for reliable long-distance transport
- Protocol selected by URL scheme in config — no auto-detection

## Motivation

| Scenario | Current | Target |
|----------|---------|--------|
| LAN cluster relay | RTMP (~300-500ms) | RTP direct (~30-50ms) |
| WAN cluster relay | RTMP only | SRT (~150-250ms), RTSP, RTMP |
| Future mixing/compositing | N/A | Requires sub-100ms inter-node latency |

## Architecture

### Plugin System

Each protocol is a self-contained plugin implementing `RelayTransport`. Plugins register themselves into a `TransportRegistry`, and `ForwardManager`/`OriginManager` resolve the transport by URL scheme at runtime.

```
┌─────────────────────────────────────────────────┐
│                 ForwardManager                   │
│                 OriginManager                    │
│          (protocol-agnostic logic)               │
└──────────────────┬──────────────────────────────┘
                   │ registry.Resolve(url)
                   ▼
┌─────────────────────────────────────────────────┐
│              TransportRegistry                   │
│         map[scheme] → RelayTransport             │
└──┬──────────┬──────────┬──────────┬─────────────┘
   │          │          │          │
   ▼          ▼          ▼          ▼
 RTMP       SRT       RTSP       RTP
Transport  Transport  Transport  Transport
```

### RelayTransport Interface

```go
// RelayTransport is the plugin interface for cluster relay protocols.
type RelayTransport interface {
    // Scheme returns the URL scheme this transport handles ("rtmp", "srt", "rtsp", "rtp").
    Scheme() string

    // Push connects to a remote node and pushes frames from a local stream.
    Push(ctx context.Context, targetURL string, stream *core.Stream) error

    // Pull connects to a remote node and pulls frames into a local stream.
    Pull(ctx context.Context, sourceURL string, stream *core.Stream) error

    // Close releases resources held by this transport.
    Close() error
}
```

**Error contract for Push/Pull:**
- Return `nil`: normal termination (source stream ended, context cancelled)
- Return `error`: abnormal disconnection (network error, protocol error) — caller decides whether to retry
- `Pull()` calls `stream.WriteFrame()` which returns `bool` — rejected frames (e.g. bitrate-limited) are silently dropped; this is not an error condition
- Codec negotiation errors (e.g. remote rejects all offered codecs) return a non-retryable error wrapping `ErrCodecMismatch`

### TransportRegistry

```go
type TransportRegistry struct {
    mu         sync.RWMutex
    transports map[string]RelayTransport
}

func (r *TransportRegistry) Register(t RelayTransport)
func (r *TransportRegistry) Resolve(rawURL string) (RelayTransport, error)
```

## Protocol Plugins

### RTMPTransport

Refactored from existing `rtmp_client.go`. No functional change — wraps the current RTMP client logic into the `RelayTransport` interface.

- **Push**: RTMP connect → publish → read RingBuffer → FLV mux → RTMP chunk write
- **Pull**: RTMP connect → play → read RTMP chunks → FLV demux → stream.WriteFrame()
- **Data path**: `AVFrame → FLV mux → RTMP chunk → TCP → dechunk → FLV demux → AVFrame`

### SRTTransport

Uses `github.com/datarhei/gosrt` (already a project dependency for `module/srt/`).

- **Push**: SRT dial → TS mux → SRT write
- **Pull**: SRT dial → SRT read → TS demux → stream.WriteFrame()
- **Data path**: `AVFrame → TS mux → SRT → TS demux → AVFrame`
- **Latency**: Configurable via `cluster.srt.latency` (default 120ms)

### RTSPTransport

Leverages existing `pkg/rtp/` packetizers and `pkg/sdp/` parser/builder.

- **Push**: TCP connect → ANNOUNCE → SETUP → RECORD → RTP pack → interleaved write
- **Pull**: TCP connect → DESCRIBE → SETUP → PLAY → interleaved read → RTP depack → stream.WriteFrame()
- **Data path**: `AVFrame → RTP pack → RTSP interleaved (TCP) → RTP depack → AVFrame`
- **Transport mode**: Configurable via `cluster.rtsp.transport` (tcp/udp, default tcp)

### RTPTransport (Direct Relay)

Lowest latency path. No container format (FLV/TS), no protocol framing (RTMP chunks/RTSP).

- **Signaling**: SDP over HTTP (like WHIP/WHEP, without ICE/DTLS)
- **Data path**: `AVFrame → RTP pack → UDP → RTP depack → AVFrame`

#### SDP Signaling Endpoints

Registered on the existing API HTTP server at `Init` time by `NewRTPTransport`. This is the only transport that registers HTTP routes — all others are pure outbound connections.

Signaling endpoints respect the existing API authentication configuration (`api.auth.bearer_token` or session cookie). Cluster nodes must share the same bearer token to relay via RTP.

```
POST /api/relay/push    — request to push RTP stream to this node
POST /api/relay/pull    — request to pull RTP stream from this node

Request body:  SDP Offer (codec info, SSRC, sending port)
Response body: SDP Answer (confirmed port, codec acknowledgment)
              — or 406 Not Acceptable if no offered codecs are supported
```

Reuses existing `pkg/sdp/` SDP build/parse capabilities.

#### RTP URL Semantics

`rtp://` URLs use the format `rtp://host:port/app/stream` where `host:port` refers to the **HTTP signaling server** (same as the API listen address on the target node). The actual RTP UDP data port is negotiated dynamically via SDP offer/answer. Example:

```
rtp://192.168.1.10:8090/live/stream1
                  ^^^^
                  API/signaling port, NOT the UDP data port
```

#### RTPTransport.Push() Flow

1. Build SDP offer from `stream.Publisher().MediaInfo()` (nil-guarded; return error if no publisher)
2. HTTP POST offer to target node's `/api/relay/push`
3. Parse SDP answer, extract target IP:port
4. Read AVFrame from stream RingBuffer → RTP pack → UDP send
5. Send RTCP SR every 5 seconds
6. Stop on stream close, context cancel, or 3 missed RTCP RRs

#### RTPTransport.Pull() Flow

1. HTTP POST SDP offer to source node's `/api/relay/pull`
2. Parse SDP answer, extract source SSRC/codec info
3. Listen on local UDP port, receive RTP packets
4. RTP depacketize → AVFrame → stream.WriteFrame()
5. Send RTCP RR every 5 seconds
6. Stop on context cancel or 3 missed RTCP SRs/RTP packets

#### RTCP Heartbeat & Graceful Shutdown

- Sender: SR every 5 seconds
- Receiver: RR every 5 seconds
- Timeout: 3 consecutive missed intervals → consider peer disconnected
- **Graceful shutdown**: `Close()` sends RTCP BYE packet before releasing the UDP socket, allowing the remote peer to detect shutdown immediately instead of waiting for the full 15s timeout

## ForwardManager / OriginManager Refactoring

Both managers become protocol-agnostic. They receive a `TransportRegistry` instead of direct protocol references.

### ForwardManager Changes

```go
func (fm *ForwardManager) onPublish(ctx *core.EventContext) error {
    stream, ok := fm.hub.Find(ctx.StreamKey)
    targets, _ := fm.scheduler.Resolve("forward", ctx.StreamKey)

    for _, targetURL := range targets {
        transport, err := fm.registry.Resolve(targetURL)
        if err != nil {
            slog.Warn("unsupported relay protocol", "url", targetURL, "error", err)
            continue
        }
        ft := NewForwardTarget(ctx.StreamKey, targetURL, stream, transport, ...)
        go ft.Run()
    }
}
```

`ForwardTarget.Run()` calls `transport.Push()` instead of `forwardOnce()`. Retry uses exponential backoff (matching OriginPull behavior), capped at 30 seconds.

**Stream path handling for forward targets**: All forward target URLs in config must include the full path (app + stream key). The current RTMP-specific `containsStreamPath` auto-append logic is removed — each protocol has different URL semantics, so explicit full URLs are required. The Scheduler callback can return protocol-appropriate full URLs per stream key.

**Note**: Origin server URLs do NOT include the stream key — it is appended at runtime from the subscriber's requested stream key, same as current behavior.

### OriginManager Changes

```go
func (op *OriginPull) pullOnce(sourceURL string) error {
    transport, err := op.registry.Resolve(sourceURL)
    if err != nil {
        return err
    }
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go func() { <-op.closed; cancel() }()
    return transport.Pull(ctx, sourceURL, op.stream)
}
```

Origin server list can now mix protocols: `["srt://origin1:9000", "rtmp://origin2", "rtp://192.168.1.5:5000"]`. Tried in order with exponential backoff, same as current behavior.

## Module Wiring

```go
func (m *Module) Init(s *core.Server) error {
    cfg := s.Config().Cluster

    m.registry = NewTransportRegistry()
    m.registry.Register(NewRTMPTransport())
    m.registry.Register(NewSRTTransport(cfg.SRT))
    m.registry.Register(NewRTSPTransport(cfg.RTSP))
    // RTPTransport needs *core.Server to register HTTP signaling handlers
    // on the API router at init time. This is the only transport that
    // registers HTTP routes.
    m.registry.Register(NewRTPTransport(cfg.RTP, s))

    // ForwardManager/OriginManager receive registry
    if cfg.Forward.Enabled {
        m.forward = NewForwardManager(hub, bus, scheduler, m.registry, ...)
    }
    if cfg.Origin.Enabled {
        m.origin = NewOriginManager(hub, bus, scheduler, m.registry, ...)
    }
}
```

## Configuration

```yaml
cluster:
  forward:
    enabled: true
    targets:
      - "rtmp://relay1.example.com/live/stream1"
      - "srt://relay2.example.com:9000/live/stream1"
      - "rtsp://relay3.example.com/live/stream1"
      - "rtp://192.168.1.10:5000/live/stream1"
    schedule_url: ""
    retry_max: 3
    retry_interval: 5s    # base delay for exponential backoff (existing field name preserved)

  origin:
    enabled: true
    servers:
      - "srt://origin1.example.com:9000"
      - "rtmp://origin2.example.com"
      - "rtp://192.168.1.5:5000"
    schedule_url: ""
    retry_max: 3
    retry_delay: 2s
    idle_timeout: 30s

  srt:
    latency: 120ms
    passphrase: ""
    pbkeylen: 16

  rtsp:
    transport: tcp

  rtp:
    port_range: "20000-20100"     # UDP port range for dynamically allocated RTP data ports
    signaling_path: "/api/relay"  # HTTP signaling base path (on API server)
    rtcp_interval: 5s
    timeout: 15s
```

### Config Structs

```go
type ClusterConfig struct {
    Forward ForwardConfig      `yaml:"forward"`
    Origin  OriginConfig       `yaml:"origin"`
    SRT     ClusterSRTConfig   `yaml:"srt"`
    RTSP    ClusterRTSPConfig  `yaml:"rtsp"`
    RTP     ClusterRTPConfig   `yaml:"rtp"`
}

type ClusterSRTConfig struct {
    Latency    time.Duration `yaml:"latency"`
    Passphrase string        `yaml:"passphrase"`
    PBKeyLen   int           `yaml:"pbkeylen"`
}

type ClusterRTSPConfig struct {
    Transport string `yaml:"transport"`
}

type ClusterRTPConfig struct {
    PortRange     string        `yaml:"port_range"`      // UDP port range for RTP data (e.g. "20000-20100")
    SignalingPath string        `yaml:"signaling_path"`   // HTTP signaling base path
    RTCPInterval  time.Duration `yaml:"rtcp_interval"`
    Timeout       time.Duration `yaml:"timeout"`
}
```

## Error Handling & Failover

### Forward (push)

Each target runs in its own goroutine. Failure on one target does not affect others.

```
target URL → registry.Resolve() → transport.Push()
  ↓ error returned
  → retry same URL (up to retry_max, exponential backoff capped at 30s)
  → exceed retry_max → log warning, stop this target
  → other targets unaffected
```

### Origin (pull)

Servers tried in list order. Mixed protocols are supported.

```
servers: [srt://origin1, rtmp://origin2, rtp://192.168.1.5]
  ↓ try in order
  srt://origin1 fails → rtmp://origin2 fails → rtp://192.168.1.5 succeeds
  ↓ connection drops
  → restart from first server (attempt++, exponential backoff, cap 30s)
  → exceed retry_max → give up
```

### RTP-specific failure detection

- Sender: 3 consecutive RTCP intervals with no RR received → close connection
- Receiver: 3 consecutive RTCP intervals with no SR or RTP data → close connection
- UDP is connectionless — RTCP heartbeat is the only liveness signal

## Metrics & Observability

New Prometheus metrics exposed via the existing metrics module:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cluster_relay_active` | Gauge | `direction` (forward/origin), `protocol` | Active relay connections |
| `cluster_relay_errors_total` | Counter | `direction`, `protocol`, `error_type` | Relay connection errors |
| `cluster_relay_bytes_total` | Counter | `direction`, `protocol` | Bytes transferred via relay |
| `cluster_relay_latency_seconds` | Histogram | `protocol` | End-to-end relay latency (RTP only, measured via RTCP SR/RR round-trip) |
| `cluster_rtp_packet_loss_ratio` | Gauge | `stream`, `direction` | RTP packet loss ratio from RTCP RR fraction lost |

## File Structure

```
module/cluster/
├── module.go              # Module init, plugin registration, wiring
├── registry.go            # TransportRegistry
├── transport.go           # RelayTransport interface definition
├── forward.go             # ForwardManager (protocol-agnostic)
├── origin.go              # OriginManager (protocol-agnostic)
├── scheduler.go           # Dynamic target resolution (existing, unchanged)
├── transport_rtmp.go      # RTMPTransport plugin
├── transport_srt.go       # SRTTransport plugin
├── transport_rtsp.go      # RTSPTransport plugin
├── transport_rtp.go       # RTPTransport plugin + SDP signaling
├── registry_test.go
├── transport_rtmp_test.go
├── transport_srt_test.go
├── transport_rtsp_test.go
├── transport_rtp_test.go
├── forward_test.go
├── origin_test.go
├── module_test.go
└── integration_test.go
```

## Testing Strategy

### Unit Tests (per transport plugin)

| File | Coverage |
|------|----------|
| `registry_test.go` | Register, Resolve, unknown scheme error |
| `transport_rtmp_test.go` | Mock TCP conn, verify FLV mux/demux round-trip |
| `transport_srt_test.go` | Mock SRT conn, verify TS mux/demux round-trip |
| `transport_rtsp_test.go` | Mock RTSP session, verify RTP pack/depack |
| `transport_rtp_test.go` | Mock UDP conn, verify SDP exchange + RTP relay |
| `forward_test.go` | Registry routing, retry logic, multi-target independence |
| `origin_test.go` | Registry routing, server list failover, mixed protocols |

### Integration Tests

| Test | Verification |
|------|-------------|
| Forward multi-protocol | Start local RTMP/SRT/RTSP listeners, forward same stream to all 3, verify frames received |
| Origin multi-protocol | Publish to local server, origin pull via each protocol, verify AVFrame content matches |
| RTP relay | Two nodes (goroutines with separate StreamHubs), SDP signaling via httptest.Server, push RTP A→B, verify frame delivery |

### Key Verification Points

- All protocols produce identical AVFrame sequences (codec, DTS, payload) for the same source
- Sequence headers (SPS/PPS, AudioSpecificConfig) correctly transmitted
- Context cancellation cleanly stops all goroutines — no leaks
- RTP RTCP timeout detection triggers proper disconnection
- Mixed-protocol origin failover works correctly

### Coverage Target

80%+ across all new and modified files.

## Latency Comparison

| Protocol | Same DC | Cross-region | Cross-continent |
|----------|---------|-------------|----------------|
| RTP direct | ~30-50ms | N/A (no FEC/ARQ) | N/A |
| SRT | ~80-120ms | ~150-250ms | ~500-800ms |
| RTSP | ~100-200ms | ~200-400ms | ~500-800ms |
| RTMP | ~300-500ms | ~500-1000ms | ~1000-2000ms |

## Implementation Phases

1. **Phase 1**: `RelayTransport` interface + `TransportRegistry` + refactor existing RTMP into `RTMPTransport` plugin. Existing cluster functionality preserved.
2. **Phase 2**: `SRTTransport` + `RTSPTransport` plugins. Config schema extension.
3. **Phase 3**: `RTPTransport` plugin + SDP signaling endpoints. Integration tests.
