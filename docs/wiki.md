**English** | [中文](wiki.zh-CN.md)

# LiveForge Wiki

> Comprehensive documentation for LiveForge — a high-performance multi-protocol live streaming server written in Go.

---

## Table of Contents

- [Overview](#overview)
- [Quick Start](#quick-start)
- [Configuration Reference](#configuration-reference)
  - [Server](#server)
  - [TLS](#tls)
  - [Limits](#limits)
  - [Stream](#stream)
- [Protocol Modules](#protocol-modules)
  - [RTMP](#rtmp)
  - [RTSP](#rtsp)
  - [HTTP Streaming (HLS/DASH/HTTP-FLV/FMP4/TS)](#http-streaming)
  - [WebSocket](#websocket)
  - [WebRTC (WHIP/WHEP)](#webrtc-whipwhep)
- [Business Modules](#business-modules)
  - [Authentication](#authentication)
  - [Recording](#recording)
  - [Webhook Notifications](#webhook-notifications)
- [Management](#management)
  - [REST API](#rest-api)
  - [Web Console](#web-console)
- [Architecture](#architecture)
  - [Module System](#module-system)
  - [Event Bus](#event-bus)
  - [Stream Lifecycle](#stream-lifecycle)
  - [GOP Cache](#gop-cache)
- [Usage Scenarios](#usage-scenarios)
- [Troubleshooting](#troubleshooting)

---

## Overview

LiveForge is a modular live streaming media server that ingests, transmuxes, and delivers audio/video in real time. It supports RTMP, RTSP, WebRTC (WHIP/WHEP), HLS, DASH, HTTP-FLV, FMP4, and WebSocket streaming — all from a single binary with zero external dependencies.

**Key characteristics:**

- Single binary, single config file
- Any ingest protocol can be played back by any output protocol (protocol bridge)
- Modular architecture — enable only the protocols you need
- Environment variable expansion in config (`${ENV_VAR}`)
- Graceful shutdown with drain timeout

---

## Quick Start

### Build

```bash
go build -o liveforge ./cmd/liveforge
```

### Run

```bash
./liveforge -c configs/liveforge.yaml
```

### Publish with FFmpeg

```bash
# RTMP
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://localhost:1935/live/stream1

# RTSP
ffmpeg -re -i input.mp4 -c copy -f rtsp rtsp://localhost:8554/live/stream1
```

### Play

```bash
# RTMP
ffplay rtmp://localhost:1935/live/stream1

# HLS
ffplay http://localhost:8080/live/stream1.m3u8

# DASH
ffplay http://localhost:8080/live/stream1.mpd

# HTTP-FLV
ffplay http://localhost:8080/live/stream1.flv

# HTTP-TS
ffplay http://localhost:8080/live/stream1.ts

# HTTP-FMP4
ffplay http://localhost:8080/live/stream1.mp4

# RTSP
ffplay rtsp://localhost:8554/live/stream1
```

### Web Console

Open `http://localhost:8090/console` in a browser (default login: admin/admin).

---

## Configuration Reference

The config file is YAML. All fields support environment variable expansion via `${VAR_NAME}` syntax. The default config file is at `configs/liveforge.yaml`.

### Server

```yaml
server:
  name: "streamserver-01"
  log_level: info
  drain_timeout: 30s
```

| Field | Default | Description |
|-------|---------|-------------|
| `name` | `liveforge` | Instance identifier, useful in cluster deployments |
| `log_level` | `info` | Log verbosity |
| `drain_timeout` | `0s` | Time to wait for active connections to close on shutdown |

### TLS

Global TLS configuration. When `cert_file` and `key_file` are both set, all enabled modules automatically use TLS (RTMPS, RTSPS, HTTPS).

```yaml
tls:
  cert_file: "/path/to/cert.pem"
  key_file: "/path/to/key.pem"
```

Each protocol module can override the global setting with a per-module `tls` field:

```yaml
rtmp:
  enabled: true
  listen: ":1935"
  tls: false          # Force plain TCP even if global TLS is configured

http_stream:
  enabled: true
  listen: ":8080"
  tls: true           # Force TLS (error if global cert/key not set)
```

**Override rules:**

| Global TLS | Module `tls` | Result |
|------------|-------------|--------|
| Configured | _(omitted)_ | TLS enabled |
| Configured | `false` | Plain TCP |
| Configured | `true` | TLS enabled |
| Not configured | _(omitted)_ | Plain TCP |
| Not configured | `true` | **Error** — cert/key required |
| Not configured | `false` | Plain TCP |

**Use case:** Terminate TLS at a load balancer for HTTP traffic, but use RTMPS directly for RTMP publishers:

```yaml
tls:
  cert_file: "/etc/ssl/server.crt"
  key_file: "/etc/ssl/server.key"

rtmp:
  enabled: true
  listen: ":1935"
  # tls: omitted -> follows global -> RTMPS

http_stream:
  enabled: true
  listen: ":8080"
  tls: false           # Behind reverse proxy, no TLS needed

api:
  enabled: true
  listen: ":8090"
  tls: false           # Internal network only
```

### Limits

```yaml
limits:
  max_streams: 0                    # 0 = unlimited
  max_subscribers_per_stream: 0     # 0 = unlimited
  max_connections: 0                # 0 = unlimited
  max_bitrate_per_stream: 0         # 0 = unlimited (reserved)
```

| Field | Description |
|-------|-------------|
| `max_streams` | Maximum concurrent streams the server will accept |
| `max_subscribers_per_stream` | Per-stream subscriber cap |
| `max_connections` | Global connection limit shared by all modules |
| `max_bitrate_per_stream` | Reserved for future use |

### Stream

Controls stream behavior at the server level.

```yaml
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
  audio_on_demand:
    enabled: false
  feedback:
    enabled: false
```

| Field | Default | Description |
|-------|---------|-------------|
| `gop_cache` | `true` | New subscribers receive the latest keyframe group immediately for fast startup |
| `gop_cache_num` | `1` | Number of GOPs to keep. Higher values increase memory but give subscribers more catch-up data |
| `audio_cache_ms` | `1000` | Milliseconds of audio to cache alongside the video GOP |
| `ring_buffer_size` | `1024` | Slots in the lock-free ring buffer. Increase for high-bitrate or high-FPS streams |
| `max_skip_count` | `3` | Maximum number of times a slow subscriber can skip ahead before being disconnected |
| `max_skip_window` | `60s` | Time window for counting subscriber skip events |
| `idle_timeout` | `30s` | Stream is destroyed if no publisher and no subscribers for this duration |
| `no_publisher_timeout` | `15s` | If a stream is created but no publisher connects within this time, the stream is closed |
| `simulcast.enabled` | `false` | Enable simulcast support for multi-quality streams |
| `audio_on_demand.enabled` | `false` | Enable audio-on-demand mode. Audio is only forwarded when a subscriber requests it. |
| `feedback.enabled` | `false` | Enable subscriber feedback (e.g., NACK, PLI requests from viewers) |

---

## Protocol Modules

### RTMP

The RTMP module handles both ingest (publish) and playback (subscribe) using the RTMP/RTMPS protocol.

```yaml
rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Enable the RTMP module |
| `listen` | `:1935` | TCP address to bind |
| `chunk_size` | `4096` | RTMP chunk size in bytes |
| `tls` | _(null)_ | Per-module TLS override (enables RTMPS) |

**Supported RTMP commands:** connect, createStream, publish, play, deleteStream, releaseStream, FCPublish, FCUnpublish, closeStream, @setDataFrame (metadata).

**Publish (ingest):**

```bash
# OBS: Set Server to rtmp://host:1935/live, Stream Key to stream1

# FFmpeg:
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://host:1935/live/stream1

# With auth token:
ffmpeg -re -i input.mp4 -c copy -f flv "rtmp://host:1935/live/stream1?token=YOUR_JWT"
```

**Play (subscribe):**

```bash
ffplay rtmp://host:1935/live/stream1
vlc rtmp://host:1935/live/stream1
```

**Use cases:**
- OBS Studio live streaming
- FFmpeg relay/transcoding pipelines
- Legacy player compatibility
- Low-latency ingest from hardware encoders

---

### RTSP

The RTSP module supports both TCP interleaved and UDP transport for ingest and playback.

```yaml
rtsp:
  enabled: true
  listen: ":8554"
  rtp_port_range: [10000, 20000]
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Enable the RTSP module |
| `listen` | `:8554` | TCP address to bind |
| `rtp_port_range` | `[10000, 20000]` | UDP port range for RTP/RTCP media transport |
| `tls` | _(null)_ | Per-module TLS override (enables RTSPS) |

**Supported RTSP methods:** OPTIONS, DESCRIBE, SETUP, PLAY, PAUSE, ANNOUNCE, RECORD, TEARDOWN.

**Transport modes:**
- **TCP interleaved** — RTP data embedded in the RTSP TCP connection (firewall-friendly)
- **UDP** — Separate UDP ports for RTP/RTCP (lower latency)

**Publish via RTSP (ANNOUNCE/RECORD):**

```bash
# TCP transport
ffmpeg -re -i input.mp4 -c copy -f rtsp -rtsp_transport tcp rtsp://host:8554/live/stream1

# UDP transport
ffmpeg -re -i input.mp4 -c copy -f rtsp rtsp://host:8554/live/stream1
```

**Play via RTSP (DESCRIBE/PLAY):**

```bash
ffplay -rtsp_transport tcp rtsp://host:8554/live/stream1
ffplay rtsp://host:8554/live/stream1
vlc rtsp://host:8554/live/stream1
```

**Use cases:**
- IP camera ingest (most cameras speak RTSP natively)
- Surveillance systems
- Cross-protocol bridge: RTSP ingest -> HLS/DASH delivery

---

### HTTP Streaming

A single HTTP module serves multiple streaming formats based on the URL extension.

```yaml
http_stream:
  enabled: true
  listen: ":8080"
  cors: true
  # tls: null
  hls:
    segment_duration: 6
    playlist_size: 5
  dash:
    segment_duration: 6
    playlist_size: 30
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | Enable/disable the HTTP streaming module |
| `listen` | `:8080` | HTTP listen address |
| `cors` | `true` | Enable CORS headers |
| `tls` | _(null)_ | Per-module TLS override |

**URL format:** `http://host:8080/{app}/{stream_key}.{format}`

| Format | URL | Content-Type | Description |
|--------|-----|-------------|-------------|
| **HLS** | `/live/stream1.m3u8` | `application/vnd.apple.mpegurl` | Apple HLS — widest device support |
| **HLS segments** | `/live/stream1/{N}.ts` | `video/mp2t` | Individual TS segments |
| **DASH** | `/live/stream1.mpd` | `application/dash+xml` | MPEG-DASH with SegmentTimeline |
| **DASH video** | `/live/stream1/v{N}.m4s` | `video/mp4` | fMP4 video segments |
| **DASH audio** | `/live/stream1/a{N}.m4s` | `video/mp4` | fMP4 audio segments |
| **DASH init** | `/live/stream1/init.mp4` | `video/mp4` | Video init segment (ftyp+moov) |
| **DASH audio init** | `/live/stream1/audio_init.mp4` | `video/mp4` | Audio init segment |
| **HTTP-FLV** | `/live/stream1.flv` | `video/x-flv` | Chunked FLV stream |
| **HTTP-TS** | `/live/stream1.ts` | `video/mp2t` | Chunked MPEG-TS stream |
| **HTTP-FMP4** | `/live/stream1.mp4` | `video/mp4` | Chunked fragmented MP4 stream |

#### HLS

| Field | Default | Description |
|-------|---------|-------------|
| `segment_duration` | `6` | Target segment length in seconds. Actual duration depends on keyframe intervals. |
| `playlist_size` | `5` | Number of segments listed in the m3u8 playlist. |

**Use cases:**
- Browser playback on iOS Safari (native HLS support)
- Any device with hls.js (Chrome, Firefox, Edge)
- CDN-friendly delivery (cacheable segments)

```bash
ffplay http://localhost:8080/live/stream1.m3u8
```

#### DASH

| Field | Default | Description |
|-------|---------|-------------|
| `segment_duration` | `6` | Target segment length in seconds. |
| `playlist_size` | `30` | Max segments kept in the sliding window. |

The MPD uses `SegmentTimeline` with explicit per-segment timing, providing accurate playback in ffplay and dash.js. Video and audio are served as separate AdaptationSets.

**Use cases:**
- Browser playback via dash.js
- Android ExoPlayer
- CDN delivery with adaptive bitrate readiness

```bash
ffplay http://localhost:8080/live/stream1.mpd
mpv http://localhost:8080/live/stream1.mpd
```

#### HTTP-FLV / HTTP-TS / HTTP-FMP4

Chunked transfer encoding streams — the server pushes data continuously until the client disconnects.

**Use cases:**
- HTTP-FLV: flv.js browser playback, low-latency web streaming
- HTTP-TS: Player compatibility, transcoder input
- HTTP-FMP4: MSE-based browser players

---

### WebSocket

WebSocket streaming multiplexes the same FLV/TS/FMP4 formats over a WebSocket connection.

**URL format:** `ws://host:8080/ws/live/stream1.flv`

The WebSocket handler shares the HTTP listener. Supported formats: `flv`, `ts`, `mp4`.

**Use cases:**
- Browser-based players that prefer WebSocket over HTTP chunked transfer
- Environments where HTTP chunked streaming is blocked by proxies

---

### WebRTC (WHIP/WHEP)

WebRTC support using the WHIP (publish) and WHEP (subscribe) signaling standards.

```yaml
webrtc:
  enabled: true
  listen: ":8443"
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [20000, 30000]
  candidates: []
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Enable the WebRTC module |
| `listen` | `:8443` | HTTP signaling endpoint address |
| `ice_servers` | Google STUN | STUN/TURN servers for ICE connectivity |
| `udp_port_range` | `[20000, 30000]` | UDP port range for RTP media |
| `candidates` | `[]` | Additional ICE candidates (public IPs for NAT traversal) |
| `tls` | _(null)_ | Per-module TLS override |

**WHIP (publish):**

```bash
# HTTP endpoint: POST /webrtc/whip/{app}/{stream_key}
# The browser console has a built-in WHIP publisher with camera/mic selection.

curl -X POST http://host:8443/webrtc/whip/live/stream1 \
  -H "Content-Type: application/sdp" \
  -d "$SDP_OFFER"
```

**WHEP (subscribe):**

```bash
# HTTP endpoint: POST /webrtc/whep/{app}/{stream_key}
# The browser console has built-in WHEP playback.

# Two modes:
# ?mode=realtime  — minimum latency, may skip frames
# ?mode=live      — slightly higher latency, smoother playback
```

**Session management:**
- `DELETE /webrtc/session/{id}` — Terminate a session
- `PATCH /webrtc/session/{id}` — ICE trickle candidates

**Use cases:**
- Sub-second browser-to-browser streaming
- Browser ingest without plugins (camera/screen capture)
- Ultra-low-latency monitoring dashboards
- Cross-protocol: WebRTC ingest -> HLS/DASH delivery to large audiences

---

## Business Modules

### Authentication

The auth module provides JWT token and HTTP callback authentication for publish and subscribe events.

```yaml
auth:
  enabled: true
  publish:
    mode: "token"
    token:
      secret: "${AUTH_JWT_SECRET}"
      algorithm: HS256
    callback:
      url: "http://auth-service/verify"
      timeout: 3s
  subscribe:
    mode: "callback"
    token:
      secret: "${AUTH_JWT_SECRET}"
      algorithm: HS256
    callback:
      url: "http://auth-service/verify"
      timeout: 3s
  api:
    bearer_token: "${API_TOKEN}"
```

> **Note:** Only `HS256` is currently implemented as a JWT signing algorithm.

**Auth modes:**

| Mode | Description |
|------|-------------|
| `none` | No authentication (default) |
| `token` | JWT token verification via `?token=` query parameter |
| `callback` | HTTP POST to external auth service |
| `token+callback` | Try JWT first, fall back to callback if JWT fails |

#### JWT Token Mode

The token is passed as a URL query parameter: `?token=YOUR_JWT`.

**JWT payload fields:**

```json
{
  "sub": "live/stream1",
  "action": "publish",
  "exp": 1711500000
}
```

The JWT is signed with HMAC-SHA256 using the configured `secret`.

**Example — generate a publish token:**

```python
import jwt, time
token = jwt.encode(
    {"sub": "live/stream1", "action": "publish", "exp": int(time.time()) + 3600},
    "my-secret", algorithm="HS256"
)
```

```bash
ffmpeg -re -i input.mp4 -c copy -f flv "rtmp://host:1935/live/stream1?token=$TOKEN"
```

#### Callback Mode

LiveForge sends an HTTP POST to the configured URL with:

```json
{
  "stream_key": "live/stream1",
  "protocol": "rtmp",
  "remote_addr": "192.168.1.100:54321",
  "token": "optional-token-from-query",
  "action": "publish"
}
```

- **HTTP 200** -> Allowed
- **Any other status** -> Rejected

**Use cases:**
- Integrate with existing user systems
- Dynamic stream key validation
- Rate limiting per-user

---

### Recording

The record module captures live streams to FLV files with duration-based segmentation.

```yaml
record:
  enabled: true
  stream_pattern: "live/*"
  format: flv
  path: "/data/record/{stream_key}/{date}_{time}.flv"
  segment:
    mode: duration
    duration: 30m
    max_size: 512MB
  on_file_complete:
    url: "http://my-service/on-record-complete"
```

| Field | Default | Description |
|-------|---------|-------------|
| `stream_pattern` | `*` | Glob pattern for stream keys. `live/*` records only streams under the `live` app. |
| `format` | `flv` | Output format (currently FLV only) |
| `path` | `./recordings/{stream_key}/{date}_{time}.flv` | File path template |
| `segment.duration` | `30m` | Segment duration. Set to `0` for a single continuous file. |
| `on_file_complete.url` | _(empty)_ | HTTP POST callback when a recording file is finalized |

**Path template variables:**

| Variable | Example | Description |
|----------|---------|-------------|
| `{stream_key}` | `live/stream1` | Full stream key |
| `{date}` | `2026-03-26` | Current date |
| `{time}` | `143052` | Current time (HHMMSS) |

**File complete callback payload:**

```json
{
  "stream_key": "live/stream1",
  "file_path": "/data/record/live/stream1/2026-03-26_143052.flv",
  "bytes": 52428800,
  "duration": 1800.5
}
```

**Use cases:**
- VOD archive of live streams
- Compliance recording
- Post-production editing from live footage
- Automated upload to S3/cloud storage via the callback webhook

---

### Webhook Notifications

The notify module sends HTTP POST webhooks for stream lifecycle events.

```yaml
notify:
  http:
    enabled: true
    endpoints:
      - url: "https://my-service/webhook"
        events: ["on_publish", "on_publish_stop"]
        secret: "webhook-signing-secret"
        retry: 3
        timeout: 5s
      - url: "https://analytics/events"
        events: []
        secret: ""
        retry: 1
        timeout: 3s
  alive_interval: 10s
  websocket:
    enabled: false
    path: "/ws/notify"
```

**Events:**

| Event | Trigger |
|-------|---------|
| `on_publish` | Publisher starts sending media |
| `on_publish_stop` | Publisher disconnects |
| `on_subscribe` | Subscriber connects |
| `on_subscribe_stop` | Subscriber disconnects |
| `on_stream_create` | New stream is created in the hub |
| `on_stream_destroy` | Stream is removed from the hub |
| `on_publish_alive` | Periodic heartbeat while publisher is active |
| `on_subscribe_alive` | Periodic heartbeat while subscribers exist |
| `on_stream_alive` | Periodic heartbeat while stream exists |

**Webhook payload:**

```json
{
  "event": "on_publish",
  "stream_key": "live/stream1",
  "protocol": "rtmp",
  "remote_addr": "192.168.1.100:54321",
  "timestamp": 1711500000,
  "extra": {
    "bytes_in": 1048576,
    "video_frames": 900,
    "audio_frames": 1500,
    "bitrate_kbps": 2500,
    "fps": 30.0,
    "uptime_sec": 60
  }
}
```

**Alive event extra fields:**

The `on_publish_alive`, `on_subscribe_alive`, and `on_stream_alive` events include additional statistics in the `extra` object:

| Field | Type | Description |
|-------|------|-------------|
| `bytes_in` | int64 | Total bytes received |
| `video_frames` | int64 | Total video frames received |
| `audio_frames` | int64 | Total audio frames received |
| `bitrate_kbps` | float64 | Current bitrate in kbps |
| `fps` | float64 | Current video frames per second |
| `uptime_sec` | float64 | Stream uptime in seconds |

**WebSocket notifications:**

When `websocket.enabled` is `true`, clients can connect to the WebSocket endpoint at the configured `path` to receive real-time event notifications. Events are delivered as JSON messages with the same format as HTTP webhook payloads.

**Signature verification (HMAC-SHA256):**

When a `secret` is configured, the webhook includes an `X-Signature` header:

```
X-Signature: hex(HMAC-SHA256(request_body, secret))
```

Verify on the receiving end:

```python
import hmac, hashlib
expected = hmac.new(b"webhook-signing-secret", request.body, hashlib.sha256).hexdigest()
assert hmac.compare_digest(request.headers["X-Signature"], expected)
```

**Retry behavior:** Exponential backoff (1s, 2s, 4s, ...) up to 30s cap, for the configured number of retries.

**Use cases:**
- Stream status dashboards
- Chat overlays triggered on stream start
- Analytics and billing based on stream duration
- Auto-scaling worker nodes based on stream count

---

## Management

### REST API

The API module runs on a separate port (default `:8090`) and provides management endpoints.

```yaml
api:
  enabled: true
  listen: ":8090"
  # tls: null
  auth:
    bearer_token: "${API_TOKEN}"
  console:
    username: "admin"
    password: "admin"
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | Enable/disable the API module |
| `listen` | `:8090` | HTTP listen address |
| `tls` | _(null)_ | Per-module TLS override |
| `auth.bearer_token` | `""` | Bearer token for API authentication. Supports `${ENV_VAR}` expansion. |
| `console.username` | `"admin"` | Web console login username |
| `console.password` | `"admin"` | Web console login password |

**Authentication:**
- **API endpoints** (`/api/*`): Protected by Bearer token OR valid console session cookie
- **Console** (`/console`): Protected by session cookie login (24h expiry, HMAC-SHA256 signed)

**Endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/streams` | List all active streams with stats |
| `GET` | `/api/v1/streams/{app}/{key}` | Get single stream detail |
| `DELETE` | `/api/v1/streams/{app}/{key}` | Delete a stream |
| `POST` | `/api/v1/streams/{app}/{key}/kick` | Kick the publisher |
| `GET` | `/api/v1/server/info` | Server info (version, uptime, modules, endpoints) |
| `GET` | `/api/v1/server/stats` | Server stats (stream count, connection count) |
| `GET` | `/api/v1/server/health` | Health check |
| `GET` | `/console` | Web console dashboard |
| `GET/POST` | `/console/login` | Login page |

**Example — list streams:**

```bash
curl -H "Authorization: Bearer $API_TOKEN" http://localhost:8090/api/v1/streams
```

**Response:**

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "streams": [
      {
        "key": "live/stream1",
        "state": "publishing",
        "publisher": "rtmp://192.168.1.100:54321",
        "video_codec": "H264",
        "audio_codec": "AAC",
        "subscribers": { "flv": 1, "rtmp": 2 },
        "stats": {
          "bytes_in": 5242880,
          "video_frames": 9000,
          "audio_frames": 15000,
          "uptime_sec": 300,
          "bitrate_kbps": 2500,
          "fps": 30.0
        }
      }
    ]
  }
}
```

**Example — kick publisher:**

```bash
curl -X POST -H "Authorization: Bearer $API_TOKEN" \
  http://localhost:8090/api/v1/streams/live/stream1/kick
```

### Web Console

The web console is a built-in single-page dashboard available at `/console`.

**Features:**
- Real-time stream list with auto-refresh
- Per-stream details: video/audio codec, bitrate, FPS, GOP cache info, subscriber counts
- Preview player supporting HLS, DASH, HTTP-FLV, WebRTC (WHEP) playback
- WebRTC WHIP publish directly from the browser (camera/microphone selection)
- Stream management: kick publisher, delete stream
- Server info and health status

**Accessing the console:**

1. Open `http://host:8090/console` in a browser
2. Log in with the configured `username` / `password`
3. Streams appear automatically when publishing starts

---

## Architecture

### Module System

LiveForge uses a modular architecture. Each protocol or feature is a `Module` that implements:

```go
type Module interface {
    Name() string
    Init(s *Server) error
    Hooks() []HookRegistration
    Close() error
}
```

Modules are registered in `main.go` and initialized in order. On shutdown, they are closed in reverse order (LIFO) to ensure dependent modules stop first.

Built-in modules (registration order): `auth`, `rtmp`, `rtsp`, `httpstream`, `webrtc`, `api`, `notify`, `record`

> **Note:** `auth` is registered first because sync hooks must be registered before protocol modules start accepting connections.

### Event Bus

The event bus provides a hook system for cross-module communication. Events are dispatched to registered handlers by priority (lower number = higher priority).

**Hook types:**
- **Sync hooks** — Execute in priority order. If any returns an error, subsequent hooks are skipped (used for auth rejection).
- **Async hooks** — Fire in goroutines after all sync hooks succeed (used for notifications, recording).

**Event types:**

```
EventStreamCreate    EventStreamDestroy
EventPublish         EventPublishStop
EventSubscribe       EventSubscribeStop
EventRepublish       EventSubscriberSkip
EventPublishAlive    EventSubscribeAlive    EventStreamAlive
EventVideoKeyframe   EventAudioHeader
EventForwardStart    EventForwardStop
EventOriginPullStart EventOriginPullStop
```

**Event lifecycle:**

```
Publisher connects
  -> EventStreamCreate (if new stream)
  -> EventPublish (auth hooks can reject here)
  -> [streaming...]
  -> EventPublishAlive (periodic)
  -> EventPublishStop
  -> EventStreamDestroy (after idle timeout)
```

### Stream Lifecycle

Streams have a state machine:

```
 [Idle] ---(publish)---> [Publishing] ---(publisher stops)---> [Idle]
   |                                                              |
   +---------(idle_timeout expires / delete API)---------> [Destroying]
```

| State | Description |
|-------|-------------|
| **Idle** | Stream exists but has no publisher. Waiting for `no_publisher_timeout`. |
| **WaitingPull** | Stream is waiting for an origin pull to complete. |
| **NoPublisher** | Stream has subscribers but no publisher. Waiting for `no_publisher_timeout`. |
| **Publishing** | Active publisher writing frames. Subscribers can connect. |
| **Destroying** | Stream is being torn down. Removed from the hub. |

### GOP Cache

When `gop_cache` is enabled, the server buffers the latest Group of Pictures (keyframe + subsequent frames). When a new subscriber joins, it receives the cached GOP immediately, providing instant video display without waiting for the next keyframe.

---

## Usage Scenarios

### Scenario 1: Live Event Streaming

**Setup:** OBS -> RTMP -> LiveForge -> HLS/DASH -> CDN -> Viewers

```yaml
rtmp:
  enabled: true
http_stream:
  enabled: true
  cors: true
  hls:
    segment_duration: 4
    playlist_size: 6
```

1. Configure OBS to push RTMP to `rtmp://server:1935/live/event1`
2. Distribute `https://cdn.example.com/live/event1.m3u8` to viewers
3. Monitor via the web console at `http://server:8090/console`

### Scenario 2: Security Camera NVR

**Setup:** IP Camera -> RTSP -> LiveForge -> Record + WebRTC preview

```yaml
rtsp:
  enabled: true
  listen: ":8554"
webrtc:
  enabled: true
record:
  enabled: true
  stream_pattern: "camera/*"
  path: "/data/nvr/{stream_key}/{date}_{time}.flv"
  segment:
    duration: 1h
```

1. Camera pushes RTSP to `rtsp://server:8554/camera/front-door`
2. Security staff preview via WebRTC WHEP in the browser console
3. Recordings stored as 1-hour FLV files

### Scenario 3: Browser-to-Browser Ultra-Low-Latency

**Setup:** Browser -> WHIP -> LiveForge -> WHEP -> Browser

```yaml
webrtc:
  enabled: true
  listen: ":8443"
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
```

1. Publisher opens the console, clicks "Publish", selects camera/mic
2. Viewers open the console, select a stream, click "WebRTC" preview
3. Sub-second end-to-end latency

### Scenario 4: Multi-Protocol Distribution

**Setup:** Single ingest, multiple output protocols.

```yaml
rtmp:
  enabled: true
http_stream:
  enabled: true
webrtc:
  enabled: true
rtsp:
  enabled: true
```

Publish once via RTMP, simultaneously serve:
- HLS for iOS/Safari viewers
- DASH for Android/Chrome viewers
- WebRTC for ultra-low-latency viewers
- RTSP for legacy media players
- HTTP-FLV for web players using flv.js

### Scenario 5: Authenticated Premium Streaming

**Setup:** JWT auth for publishers, callback auth for subscribers.

```yaml
auth:
  enabled: true
  publish:
    mode: "token"
    token:
      secret: "my-super-secret-key"
  subscribe:
    mode: "callback"
    callback:
      url: "http://billing-service/check-subscription"
      timeout: 3s
notify:
  http:
    enabled: true
    endpoints:
      - url: "http://analytics/events"
        events: ["on_publish", "on_publish_stop", "on_subscribe"]
```

1. Publisher obtains a JWT from the auth service
2. Publishes with `?token=JWT` in the URL
3. Viewer requests a stream; LiveForge calls the billing service to verify subscription
4. Analytics service receives webhooks for all events

---

## Troubleshooting

### Stream not playing

1. Check the console at `/console` — is the stream listed?
2. Verify the publisher is connected: `curl http://host:8090/api/v1/streams`
3. Check server logs for auth rejection messages
4. Ensure the format extension matches: `.m3u8` for HLS, `.mpd` for DASH, `.flv` for FLV

### High latency on HLS/DASH

- Reduce `segment_duration` (minimum = keyframe interval of the source)
- Ensure the encoder sends keyframes every 2-4 seconds
- For lowest latency, use WebRTC (WHEP) or HTTP-FLV instead

### ffplay DASH stuttering

- LiveForge uses `SegmentTimeline` in the MPD. Ensure you're using a recent ffplay/ffmpeg build.
- If playback gaps occur, the source encoder may have irregular keyframe intervals.

### WebRTC connection failure

- Check that the `udp_port_range` ports are open in the firewall
- For NAT traversal, configure `candidates` with the server's public IP:
  ```yaml
  webrtc:
    candidates: ["203.0.113.10"]
  ```
- Verify the STUN server is reachable

### TLS errors

- Verify cert and key files exist and are readable
- Check that the cert is valid for the hostname being used
- Test with: `openssl s_client -connect host:port`

### Connection limit reached

- Check `limits.max_connections` in config
- Monitor via API: `GET /api/v1/server/stats` -> `connections` field
- Increase the limit or add more server instances
