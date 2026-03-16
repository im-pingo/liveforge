# StreamServer Design Spec

## Overview

A general-purpose streaming media server written in Go, supporting protocol muxing/demuxing (no transcoding), modular plugin architecture, and cluster distribution.

## Requirements

### Supported Protocols

| Protocol | Inbound (Publish) | Outbound (Subscribe) |
|----------|-------------------|---------------------|
| RTMP | Yes | Yes |
| HTTP-FLV | - | Yes |
| HTTP-TS | - | Yes |
| HTTP-FMP4 | - | Yes |
| WebSocket-FLV/TS/FMP4 | Optional | Yes |
| RTSP | Yes | Yes |
| WebRTC (WHIP/WHEP) | Yes (WHIP) | Yes (WHEP) |
| SIP | Yes | Yes |

### Supported Codecs

**Video:** H.264, H.265, AV1, VP8, VP9

**Audio:** AAC, Opus, MP3, G.711 (PCMU/PCMA), G.722, G.729, Speex

### Codec Compatibility Matrix

| Protocol | Video Codecs | Audio Codecs |
|----------|-------------|-------------|
| RTMP/FLV | H.264, H.265 (Enhanced RTMP) | AAC, MP3 |
| HTTP-TS | H.264, H.265 | AAC, MP3, Opus |
| HTTP-FMP4 | H.264, H.265, AV1, VP8, VP9 | AAC, Opus, MP3 |
| WebSocket | Same as encapsulation format | Same as encapsulation format |
| RTSP/RTP | H.264, H.265, VP8, VP9, AV1 | AAC, Opus, G.711, G.722, G.729, Speex, MP3 |
| WebRTC | H.264, H.265, VP8, VP9, AV1 | Opus, G.711 |
| SIP | H.264, H.265 | G.711, G.722, G.729, Opus, Speex |

Subscribing with an incompatible codec combination returns an error (HTTP 415 or protocol-specific error) at subscribe time.

**Origin pull codec check:** When a subscriber triggers an origin pull, the stream's codec is not yet known. The check happens in two phases:
1. If the schedule API response includes codec metadata, perform an early check and reject immediately if incompatible
2. When the first media frame arrives from the origin, perform a definitive codec check. Disconnect any waiting subscribers whose protocol does not support the actual codecs, with error code 415

### Design Principles

- **No transcoding** — the server only does protocol muxing/demuxing. Transcoding, mixing, and similar processing is handled by a separate transcoding server that pushes results back.
- **Modular architecture** — nginx-style module system. Modules communicate via EventBus, zero direct dependencies between modules.
- **Mixed development** — core architecture is custom-built; individual protocol layers may reference mature libraries (pion/webrtc, etc.).

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                          StreamServer                                │
│                                                                      │
│  ┌──────────────────────── Core ─────────────────────────────────┐   │
│  │  StreamHub          EventBus (sync/async hooks)               │   │
│  │  Stream + GOP Cache + Muxer + FeedbackRouter                  │   │
│  └───────────────────────────┬───────────────────────────────────┘   │
│                              │                                       │
│  ┌───── Protocol Modules ────┼──── Business Modules ─────────────┐   │
│  │                           │                                   │   │
│  │  RTMP     HTTP-Stream     │   Auth      Notify    Record      │   │
│  │  RTSP     WebSocket       │   Cluster   API       CodecCheck  │   │
│  │  WebRTC   SIP             │                                   │   │
│  │                           │                                   │   │
│  └───────────────────────────┴───────────────────────────────────┘   │
│                                                                      │
│  All modules register via Module interface, communicate via EventBus │
└──────────────────────────────────────────────────────────────────────┘
```

### Core Layers

1. **Protocol Layer** — protocol servers handling network connections, parsing, and encapsulation
2. **StreamHub** — stream lifecycle management (create, find, destroy)
3. **Stream** — per-stream data core: Publisher, AVFrame cache, Protocol Muxers, FeedbackRouter
4. **Cluster Layer** — forward/origin pull, independent of core data flow
5. **API Layer** — management interface + event notifications
6. **Business Modules** — auth, notify, record, codec check

---

## Stream Core Data Model

```
Stream
│  StreamKey: "live/room1"
│  State: idle → publishing → destroying
│
├── Publisher (1 per stream)
│     Source protocol, MediaInfo, FeedbackAccept
│
├── AVFrame RingBuffer (DTS-ordered, audio/video interleaved)
│     ├── Video Track
│     │     Codec, GOP Cache (latest 1 GOP), SequenceHeader Cache
│     ├── Video TrackGroup (simulcast mode)
│     │     Layer "h" → GOP Cache
│     │     Layer "l" → GOP Cache
│     └── Audio Track (supports pause/resume)
│           Codec, SequenceHeader Cache, Latest N frames
│
├── MuxerManager
│     FLV Muxer  → SharedBuffer → [N Subscribers]
│     TS Muxer   → SharedBuffer → [N Subscribers]
│     FMP4 Muxer → SharedBuffer → [N Subscribers]
│     RTP Muxer  → per-subscriber (SSRC differs)
│     * Muxers created on demand, destroyed when no subscribers remain
│
├── FeedbackRouter
│     Mode: auto | passthrough | aggregate | drop | server_side
│
├── LayerController (simulcast)
│     Aggregates subscriber needs, signals Publisher to enable/disable layers
│
└── Stats
      bytes_in/out, bitrate, fps, subscriber_count, uptime, codec_info
```

### Stream State Machine

```
                    ┌─────────────────────────────┐
                    ▼                             │
idle ──(subscribe arrives)──▶ waiting_pull ──(origin pull success)──▶ publishing
 │                                │                                      │
 │                          (pull failed /                          (publisher
 │                           timeout)                               disconnects)
 │                                │                                      │
 │                                ▼                                      ▼
 │                           destroying ◀────(timeout expires)──── no_publisher
 │                                ▲                                      │
 │                                │                               (new publish
 └──(publish arrives)──▶ publishing                                arrives)
                                                                        │
                                                                        ▼
                                                                   publishing
                                                                  (on_republish)
```

| State | Description |
|-------|-------------|
| **idle** | Stream created, no publisher, no subscribers |
| **waiting_pull** | Subscribers waiting, origin pull in progress |
| **publishing** | Active publisher sending data |
| **no_publisher** | Publisher disconnected, `no_publisher_timeout` counting down. Existing subscribers kept, GOP cache retained. New publish triggers `on_republish` |
| **destroying** | Timeout expired or explicit close. All sessions cleaned up, events emitted |

### Republish Flow

1. Publisher disconnects → state transitions to `no_publisher`, timer starts (`no_publisher_timeout`)
2. Existing subscribers remain connected, receiving no new data
3. If new publisher arrives before timeout:
   - Codec compatibility check: if codecs differ from original, reject with error (subscribers expect the original codecs)
   - If compatible: register as new publisher, state → `publishing`, emit `on_republish`, flush old GOP cache, start delivering new frames
4. If timeout expires: state → `destroying`, disconnect all subscribers, emit `on_stream_destroy`

### RingBuffer Concurrency Model

The AVFrame RingBuffer is the hottest data path. It uses a **lock-free single-producer / multi-consumer (SPMC)** design:

```
Publisher (single writer)
    │
    ▼ atomic write cursor
┌───────────────────────────────────┐
│  [ frame ] [ frame ] [ frame ] ...│  ← fixed-size ring
└───────────────────────────────────┘
    ▲           ▲           ▲
    │           │           │
  reader      reader      reader     ← per-muxer read cursor
  cursor A    cursor B    cursor C
```

- **Single writer** (Publisher goroutine) advances the write cursor atomically after frame data is fully written
- **Multiple readers** (Muxer goroutines) each maintain an independent read cursor, advancing at their own pace
- Frames use reference counting for memory management — freed when all readers have advanced past them
- No locks on the hot path; atomic operations only

**Slow reader policy:** When a reader's cursor falls behind the write cursor by more than the buffer capacity:

| Subscribe Mode | Behavior |
|----------------|----------|
| GOP mode | Skip forward to the latest keyframe position. Emit `on_subscriber_skip` event for monitoring |
| Realtime mode | Skip forward to the current write position. Frames in between are lost |
| Configurable fallback | If skip count exceeds `max_skip_count` within a window, disconnect the subscriber |

Configuration:
```yaml
stream:
  ring_buffer_size: 1024            # frame slots
  max_skip_count: 3                 # max skips before disconnect
  max_skip_window: 60s              # time window for skip counting
```

### SharedBuffer (Muxed Output Buffer)

Each protocol Muxer outputs to a SharedBuffer that serves multiple subscribers:

```
Muxer (single writer)
    │
    ▼
SharedBuffer (ring of muxed packets)
    │
    ├──▶ Subscriber A (read cursor)
    ├──▶ Subscriber B (read cursor)
    └──▶ Subscriber C (read cursor)
```

- **Same SPMC model** as the AVFrame RingBuffer — one Muxer writes, N subscribers read
- **Data unit:** muxed packet (FLV tag, TS packet, FMP4 fragment) rather than raw AVFrame
- **Lifecycle:** created when the first subscriber of a protocol type joins, destroyed when the last one leaves
- **Slow subscriber policy:** same as RingBuffer — skip to keyframe boundary or disconnect
- **Memory:** packets are reference-counted; subscriber read cursors control when memory is released

### RTP Muxer Detail

RTP is the exception to the shared muxer model due to per-session state:

```
AVFrame RingBuffer
    │
    ▼
RTP Payload Packager (shared, one per codec)
    │  Output: payload chunks (e.g., H.264 FU-A fragments)
    │
    ├──▶ RTP Session A: SSRC_A, seq_A, SRTP context A → Subscriber A
    ├──▶ RTP Session B: SSRC_B, seq_B, SRTP context B → Subscriber B
    └──▶ RTP Session C: SSRC_C, seq_C, SRTP context C → Subscriber C
```

- **Shared:** RTP payload packetization (NAL unit fragmentation for H.264/H.265, Opus packet framing, etc.) is performed once
- **Per-subscriber:** SSRC, sequence number counter, timestamp offset, and SRTP encryption context

### Key Design Decisions

**DTS-ordered interleaving:** The RingBuffer stores audio and video frames interleaved by DTS. For TCP-based protocols this prevents A/V desync caused by batched single-media delivery, especially during GOP cache flush on subscriber join.

**Muxer sharing:** Same-protocol subscribers share one Muxer instance. 100 HTTP-FLV viewers = 1 FLV mux operation.

**GOP Cache:** Retains the latest N complete GOPs (configurable via `gop_cache_num`, default 1). Replaced atomically when a new keyframe arrives.

**Audio Cache:** Retains latest N frames covering approximately 1 GOP duration of audio to ensure A/V sync on playback start.

### Subscribe Modes

| Mode | Behavior | Use Case |
|------|----------|----------|
| **GOP mode** | Start from nearest keyframe, instant playback | HTTP-FLV/TS/FMP4, RTMP, RTSP, latency-insensitive WebRTC |
| **Realtime mode** | Start from current latest frame, skip history | Low-latency WebRTC, SIP calls |

In realtime mode, the subscriber receives frames from the current moment. Complete picture arrives at the next natural keyframe, or the server can request an IDR from the publisher via PLI/FIR.

### Subscriber Options

```go
type SubscribeOptions struct {
    StartMode      StartMode     // gop | realtime
    FeedbackMode   FeedbackMode  // auto | passthrough | aggregate | drop | server_side
    VideoLayer     LayerPrefer   // high | low | auto
    AudioEnabled   bool
}
```

---

## Feedback System

### Feedback Modes

| Mode | Behavior | Use Case |
|------|----------|----------|
| **passthrough** | Forward subscriber FB directly to publisher | 1:1 calls |
| **aggregate** | Aggregate multiple FBs, forward summary | Small groups |
| **drop** | Discard FB, don't affect publisher | Large broadcasts |
| **server_side** | Don't forward, server drops frames per-subscriber | Large broadcasts with weak-network adaptation |
| **auto** (default) | Dynamically switch based on subscriber count | All scenarios |

### Auto Mode Thresholds

| Condition | Auto Selection |
|-----------|---------------|
| 1 subscriber | passthrough |
| 2-5 subscribers | aggregate |
| >5 subscribers | server_side |
| Publisher rejects FB | forced server_side |

Thresholds are configurable. Auto mode transitions dynamically as subscribers join/leave.

---

## Simulcast (Large/Small Streams)

For video conferencing (tens to hundreds of participants):

### LayerController

```
Inputs:
  - Each subscriber's requested layer (high/low/audio)
  - Config switches (simulcast_enabled, audio_on_demand)

Aggregation:
  need_high  = any subscriber wants high?
  need_low   = any subscriber wants low?
  need_audio = any subscriber wants audio?

→ Signal Publisher to enable/disable layers accordingly
```

### Publisher Signaling

| Protocol | Method |
|----------|--------|
| WebRTC | RTCP layer control (pause/resume RID), or Data Channel signaling |
| SIP | re-INVITE with updated SDP |
| RTMP/RTSP | Not applicable (no dynamic adjustment support) |

### VideoLayer = auto

Server automatically switches subscriber between high/low based on network feedback:
- Sufficient bandwidth → deliver high
- Bandwidth drops → switch to low
- Bandwidth recovers → switch back to high (wait for keyframe)

### Configuration

```yaml
stream:
  simulcast:
    enabled: false
    auto_pause_layer: true
    layers:
      - rid: h
        max_bitrate: 2500000
      - rid: l
        max_bitrate: 500000
  audio_on_demand:
    enabled: false
    pause_delay: 3s
```

---

## Event System + Module Architecture

### Event Types

**Stream lifecycle:**
- on_stream_create → on_stream_alive (periodic) → on_stream_destroy

**Publisher lifecycle:**
- on_publish → on_publish_alive (periodic) → on_publish_stop
- on_republish (publisher reconnects)

**Subscriber lifecycle:**
- on_subscribe → on_subscribe_alive (periodic) → on_subscribe_stop

**Data events:**
- on_video_keyframe, on_audio_header, on_metadata_update

**Cluster events:**
- on_forward_start/stop, on_origin_pull_start/stop

All lifecycle events include periodic alive heartbeats carrying real-time stats (uptime, bytes, bitrate, fps, subscriber count, codec info). Alive interval is configurable (default 10s).

### Event Bus

Supports two handler modes:

| Mode | Behavior | Use Case |
|------|----------|----------|
| **sync** | Handler can return error to abort the operation | Auth: reject unauthorized publish |
| **async** | Handler runs asynchronously, does not block | WebHook notification, recording |

### Module Interface

```go
type Module interface {
    Name() string
    Init(server *Server) error
    Hooks() []HookRegistration
    Close() error
}

type HookRegistration struct {
    Event    EventType
    Mode     HookMode        // Sync | Async
    Priority int             // Lower number = higher priority
    Handler  EventHandler
}

type EventHandler func(ctx *EventContext) error
```

### Hook Priority Chain (example: on_publish)

```
Priority 10: AuthModule.OnPublish()       ← sync, can reject
Priority 15: CodecCheckModule             ← sync, codec compatibility
Priority 20: ClusterModule.OnPublish()    ← sync, forward decision
Priority 50: RecordModule.OnPublish()     ← async, start recording
Priority 90: NotifyModule.OnPublish()     ← async, webhook callback
```

### Module Categories

| Category | Modules | Notes |
|----------|---------|-------|
| Core (not unloadable) | StreamHub, EventBus | Infrastructure |
| Built-in | Auth, Notify, Cluster, Record, CodecCheck, API | Default loaded, configurable on/off |
| Protocol | RTMP, RTSP, HTTP-Stream, WebSocket, WebRTC, SIP | Load on demand |
| Extension | Custom business logic | Third-party development |

---

## Notify Module

### Delivery Modes

| Mode | Behavior | Use Case |
|------|----------|----------|
| **HTTP Callback** | POST events to configured URLs | Traditional backend integration |
| **WebSocket Push** | Long-lived connection, server pushes events | Real-time monitoring dashboards |
| **gRPC Stream** (reserved) | Bidirectional streaming | High-performance internal services |

### WebSocket Subscription Filtering

```json
{
  "subscribe": ["on_publish", "on_publish_alive", "on_publish_stop"],
  "streams": ["live/*"],
  "alive_interval_override": 5
}
```

### Alive Event Payload Example

```json
{
  "event": "on_publish_alive",
  "stream": "live/room1",
  "uptime_sec": 3600,
  "bytes_in": 524288000,
  "bitrate_kbps": 2500,
  "fps": 30,
  "subscriber_count": 85,
  "video_codec": "h264",
  "audio_codec": "aac"
}
```

### Configuration

```yaml
notify:
  http:
    enabled: true
    endpoints:
      - url: "http://business:8000/webhook"
        events: ["on_publish", "on_publish_stop", "on_subscribe", "on_subscribe_stop"]
        secret: "${WEBHOOK_SECRET}"
        retry: 3
        timeout: 5s
      - url: "http://monitor:8000/events"
        events: ["on_*_alive"]
        retry: 1
        timeout: 2s
  websocket:
    enabled: true
    path: /api/v1/ws/notify           # served on API port (:8090), not media port
  alive_interval: 10s
```

---

## Protocol Modules

Each protocol module: **Server** (connection management) + **Handler** (protocol parse/encapsulate).

| Module | Listen | Publish Inbound | Subscribe Outbound | Library |
|--------|--------|----------------|-------------------|---------|
| RTMP | TCP :1935 | RTMP Publish → AVFrame | AVFrame → FLV Muxer → RTMP Chunk | Custom |
| HTTP-Stream | TCP :8080 | — | AVFrame → FLV/TS/FMP4 Muxer → HTTP chunked | net/http |
| WebSocket | TCP :8080 (shared) | WS → AVFrame (optional) | AVFrame → FLV/TS/FMP4 Muxer → WS frame | nhooyr/websocket |
| RTSP | TCP :554 | ANNOUNCE/RTP → AVFrame | AVFrame → RTP Muxer → RTP/RTCP | Custom + ref gortsplib |
| WebRTC | UDP + HTTP :8443 | WHIP → SDP → RTP → AVFrame | AVFrame → RTP → DTLS/SRTP | pion/webrtc |
| SIP | UDP/TCP :5060 | INVITE → SDP → RTP → AVFrame | AVFrame → RTP → SIP session | Custom SIP signaling + shared RTP |

### Shared Libraries Between Protocols

- `pkg/muxer/flv` — RTMP + HTTP-FLV + WS-FLV
- `pkg/muxer/ts` — HTTP-TS + WS-TS
- `pkg/muxer/fmp4` — HTTP-FMP4 + WS-FMP4
- `pkg/rtp` — RTSP + WebRTC + SIP
- `pkg/sdp` — RTSP + WebRTC + SIP

---

## Cluster Layer

### Forward (Push to Remote)

```
on_publish event
  → ForwardManager queries targets:
    1. Static config → forward_targets[]
    2. HTTP schedule → GET /api/forward?stream=live/room1
  → Per target: create ForwardSession
    - Subscribe locally as Subscriber
    - Push to remote via corresponding protocol
    - Auto-reconnect on disconnect
```

### Origin Pull

```
on_subscribe event, but no local Publisher
  → OriginManager queries origin:
    1. Static config → origin_servers[]
    2. HTTP schedule → GET /api/origin?stream=live/room1
  → Create OriginPullSession
    - Pull from remote via corresponding protocol
    - Register as Publisher on local Stream
    - Close after idle_timeout when last subscriber leaves
```

### HTTP Schedule Interface

```
GET /api/forward?stream=live/room1&node=node-a&event=on_publish
Response:
{
  "targets": [
    {"url": "rtmp://node-b:1935/live/room1", "protocol": "rtmp"}
  ]
}

GET /api/origin?stream=live/room1&node=node-b&event=on_subscribe
Response:
{
  "url": "rtmp://node-a:1935/live/room1", "protocol": "rtmp"
}
```

### Configuration

```yaml
cluster:
  forward:
    enabled: false
    targets:
      - url: "rtmp://backup:1935/{stream}"
        protocol: rtmp
        stream_pattern: "live/*"
    schedule_url: "http://scheduler:8000/api/forward"
    retry_max: 5
    retry_interval: 3s
  origin:
    enabled: false
    servers:
      - url: "rtmp://origin:1935/{stream}"
        protocol: rtmp
    schedule_url: "http://scheduler:8000/api/origin"
    idle_timeout: 30s
    retry_max: 3
```

---

## API Layer + Auth

### RESTful API (:8090)

**Stream management:**

| Method | Path | Description |
|--------|------|-------------|
| GET | /api/v1/streams | List online streams |
| GET | /api/v1/streams/{streamKey} | Stream details |
| DELETE | /api/v1/streams/{streamKey} | Close stream |
| POST | /api/v1/streams/{streamKey}/kick | Kick publisher or subscriber |
| POST | /api/v1/streams/{streamKey}/forward | Manual forward trigger |
| POST | /api/v1/streams/{streamKey}/record/start | Start recording |
| POST | /api/v1/streams/{streamKey}/record/stop | Stop recording |

**Client management:**

| Method | Path | Description |
|--------|------|-------------|
| GET | /api/v1/clients | List all connections |
| GET | /api/v1/clients/{id} | Connection details |
| DELETE | /api/v1/clients/{id} | Force disconnect |

**Server:**

| Method | Path | Description |
|--------|------|-------------|
| GET | /api/v1/server/info | Version, uptime, resources |
| GET | /api/v1/server/stats | Global stats |
| POST | /api/v1/config/reload | Hot reload config |

### Auth System

```
Publish/Subscribe request
  → AuthModule (sync hook, priority 10)
    ├── Token mode: extract token from URL params → JWT verify
    ├── Callback mode: POST to external auth service
    └── Combined: Token first, fallback to Callback
```

```yaml
auth:
  enabled: true
  publish:
    mode: "token+callback"
    token:
      secret: "${AUTH_JWT_SECRET}"
      algorithm: HS256
    callback:
      url: "http://auth-server:8000/auth/publish"
      timeout: 3s
  subscribe:
    mode: "token"
    token:
      secret: "${AUTH_JWT_SECRET}"
  api:
    bearer_token: "${API_TOKEN}"
```

---

## Record Module

```
on_publish event
  → RecordModule checks config (stream_pattern match or API trigger)
  → Creates RecordSession
    - Subscribes to Stream (GOP mode)
    - Writes via FLV/MP4 Muxer
    - Supports segment by duration or size
    - on_file_complete callback
```

```yaml
record:
  enabled: false
  stream_pattern: "live/*"
  format: flv
  path: "/data/record/{date}/{stream}_{time}.{ext}"
  segment:
    mode: duration
    duration: 30m
    max_size: 512MB
  on_file_complete:
    url: "http://business:8000/record/done"
```

---

## Project Structure

```
streamserver/
├── cmd/
│   └── streamserver/
│       └── main.go
├── pkg/
│   ├── avframe/          # AVFrame, CodecType, FrameType
│   ├── codec/            # Codec parameter parsing (no encoding/decoding)
│   │   ├── h264/         # SPS/PPS parsing
│   │   ├── h265/         # VPS/SPS/PPS parsing
│   │   ├── aac/          # AudioSpecificConfig
│   │   ├── opus/         # Opus header
│   │   └── ...
│   ├── muxer/
│   │   ├── flv/
│   │   ├── ts/
│   │   ├── fmp4/
│   │   └── rtp/
│   ├── sdp/
│   └── util/             # Ring buffer, pool, etc.
├── core/
│   ├── server.go
│   ├── stream_hub.go
│   ├── stream.go
│   ├── publisher.go
│   ├── subscriber.go
│   ├── muxer_manager.go
│   ├── feedback_router.go
│   ├── layer_controller.go
│   ├── event_bus.go
│   ├── module.go
│   └── codec_check.go
├── module/
│   ├── rtmp/
│   ├── rtsp/
│   ├── httpstream/
│   ├── wsstream/
│   ├── webrtc/
│   ├── sip/
│   ├── auth/
│   ├── notify/
│   ├── cluster/
│   ├── record/
│   └── api/
├── config/
│   ├── config.go
│   ├── default.go
│   └── loader.go
├── configs/
│   └── streamserver.yaml
├── go.mod
└── go.sum
```

---

## Graceful Shutdown

When the server receives SIGTERM/SIGINT:

1. **Stop accepting new connections** on all protocol listeners
2. **Stop accepting new publish/subscribe** on existing connections
3. **Wait for drain timeout** (`server.drain_timeout`, default 30s):
   - Forward sessions: stop forwarding, close remote connections
   - Origin pull sessions: close pull connections
   - Recording sessions: flush buffers, finalize files, emit `on_file_complete`
4. **Disconnect remaining sessions** — emit `on_publish_stop`, `on_subscribe_stop`, `on_stream_destroy` for each
5. **Close modules** in reverse init order via `Module.Close()`
6. **Exit**

```yaml
server:
  drain_timeout: 30s
```

---

## Resource Limits

```yaml
limits:
  max_streams: 10000                # 0 = unlimited
  max_subscribers_per_stream: 5000  # 0 = unlimited
  max_connections: 50000            # 0 = unlimited
  max_bitrate_per_stream: 20000000  # bps, 0 = unlimited
```

When a limit is reached, new publish/subscribe requests are rejected with an appropriate error code.

---

## Observability

- **Prometheus metrics** exposed at `/metrics` on the API port (:8090). Includes: stream count, connection count, bandwidth in/out, per-protocol stats, GOP cache hit ratio, ring buffer skip events
- **Structured JSON logging** with configurable level. Each log entry includes stream key, client ID, protocol, and request context where applicable
- **Health check** endpoint: `GET /api/v1/server/health` returns 200 when server is accepting connections

---

## API Response Format

All API endpoints return a consistent envelope:

```json
{
  "code": 0,
  "message": "ok",
  "data": { ... }
}
```

Error responses:
```json
{
  "code": 1001,
  "message": "stream not found",
  "data": null
}
```

List endpoints support pagination:
```
GET /api/v1/streams?page=1&limit=20

{
  "code": 0,
  "data": {
    "items": [...],
    "total": 150,
    "page": 1,
    "limit": 20
  }
}
```

---

## TLS Configuration

TLS can be configured per listener or terminated at a reverse proxy:

```yaml
tls:
  cert_file: "/etc/ssl/server.crt"
  key_file: "/etc/ssl/server.key"

# Per-protocol TLS override (optional)
rtmp:
  tls:
    enabled: true                    # RTMPS on :1936
    listen: ":1936"

http_stream:
  tls:
    enabled: true                    # Shares cert from global tls config

webrtc:
  # DTLS is always enabled (WebRTC requirement), uses the global cert
```

If no TLS is configured, all listeners use plain TCP/UDP. TLS termination at a reverse proxy (nginx, HAProxy) is a valid alternative.

---

## Hot Reload Scope

| Config Section | Hot Reloadable | Notes |
|---------------|---------------|-------|
| auth | Yes | Token secrets, callback URLs |
| notify | Yes | Endpoints, events, intervals |
| cluster | Yes | Forward targets, origin servers, schedule URLs |
| record | Yes | Stream patterns, paths, segment settings |
| stream.feedback | Yes | Thresholds |
| limits | Yes | All limits |
| Protocol listen addresses | No | Requires restart |
| server.log_level | Yes | |
| tls | No | Requires restart |

---

## Full Configuration Reference

```yaml
server:
  name: "streamserver-01"
  log_level: info
  drain_timeout: 30s

tls:
  cert_file: ""
  key_file: ""

limits:
  max_streams: 0
  max_subscribers_per_stream: 0
  max_connections: 0
  max_bitrate_per_stream: 0

rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096

rtsp:
  enabled: true
  listen: ":554"
  rtp_port_range: [10000, 20000]

http_stream:
  enabled: true
  listen: ":8080"
  cors: true

websocket:
  enabled: true
  listen: ":8080"
  path: "/ws/{stream}.{format}"

webrtc:
  enabled: true
  listen: ":8443"
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [20000, 30000]
  candidates: ["192.168.1.100"]

sip:
  enabled: true
  listen: ":5060"
  transport: ["udp", "tcp"]

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 1024
  max_skip_count: 3
  max_skip_window: 60s
  idle_timeout: 30s
  no_publisher_timeout: 15s
  simulcast:
    enabled: false
    auto_pause_layer: true
    layers:
      - rid: h
        max_bitrate: 2500000
      - rid: l
        max_bitrate: 500000
  audio_on_demand:
    enabled: false
    pause_delay: 3s
  feedback:
    default_mode: auto
    auto_thresholds:
      passthrough_max: 1
      aggregate_max: 5

auth:
  enabled: true
  publish:
    mode: "token+callback"
    token:
      secret: "${AUTH_JWT_SECRET}"
      algorithm: HS256
    callback:
      url: "http://auth-server:8000/auth/publish"
      timeout: 3s
  subscribe:
    mode: "token"
    token:
      secret: "${AUTH_JWT_SECRET}"
  api:
    bearer_token: "${API_TOKEN}"

notify:
  http:
    enabled: true
    endpoints:
      - url: "http://business:8000/webhook"
        events: ["on_publish", "on_publish_stop", "on_subscribe", "on_subscribe_stop"]
        secret: "${WEBHOOK_SECRET}"
        retry: 3
        timeout: 5s
  websocket:
    enabled: true
    path: /api/v1/ws/notify           # served on API port (:8090), not media port
  alive_interval: 10s

cluster:
  forward:
    enabled: false
    targets: []
    schedule_url: ""
    retry_max: 5
    retry_interval: 3s
  origin:
    enabled: false
    servers: []
    schedule_url: ""
    idle_timeout: 30s
    retry_max: 3

record:
  enabled: false
  stream_pattern: "live/*"
  format: flv
  path: "/data/record/{date}/{stream}_{time}.{ext}"
  segment:
    mode: duration
    duration: 30m
    max_size: 512MB
  on_file_complete:
    url: "http://business:8000/record/done"

api:
  enabled: true
  listen: ":8090"
  auth:
    bearer_token: "${API_TOKEN}"
```
