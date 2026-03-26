# Liveforge Project Progress

> This document tracks the overall development progress of the project.
> It must be updated after every development session to prevent context loss.
>
> **Last updated: 2026-03-26**

---

## Project Overview

**Liveforge** is a high-performance media streaming server written in Go, supporting multi-protocol ingest and playback.

- **Code volume**: ~14,700 lines (excluding tests)
- **Commits**: 90
- **Test packages**: 25, all passing, 0 failures
- **Author**: im-pingo <cczjp89@gmail.com>

---

## Completed Features ✅

### Phase 1 — Core Architecture + RTMP

| Module | Path | Description |
|--------|------|-------------|
| AVFrame type system | `pkg/avframe/` | Codec types, frame types, MediaInfo |
| H.264 SPS / AAC ASC parser | `pkg/codec/h264/`, `pkg/codec/aac/` | SPS width/height/profile, AudioSpecificConfig |
| Config system | `config/` | YAML loading, env expansion, defaults |
| EventBus | `core/event_bus.go` | Sync/async hooks, priority ordering, auth rejection support |
| Server lifecycle | `core/server.go` | Module registration, graceful shutdown, drain timeout |
| StreamHub | `core/stream_hub.go` | Stream lookup/create/delete |
| Stream state machine | `core/stream.go` | State transitions, GOP cache, ring buffer writes, no-publisher timeout |
| Ring buffer | `pkg/util/ringbuffer.go` | Lock-free SPMC, blocking read, signal notification |
| Publisher/Subscriber interfaces | `core/publisher.go`, `core/subscriber.go` | Standard interface definitions |
| MuxerManager | `core/muxer_manager.go` | Per-protocol muxer lifecycle management |
| SharedBuffer | `core/shared_buffer.go` | Multi-subscriber distribution of muxed data |
| FLV muxer/demuxer | `pkg/muxer/flv/` | FLV read/write, RTMP data path |
| RTMP module | `module/rtmp/` | Handshake, chunk stream, AMF0, publisher, subscriber, server |
| Auth module | `module/auth/` | JWT token verification, HTTP callback mode |
| API module | `module/api/` | Full REST API, console login, bearer token + session auth |
| Integration tests | `test/integration/` | RTMP push → RTMP pull E2E test |

---

### Phase 2 — HTTP Streaming

| Module | Path | Description |
|--------|------|-------------|
| TS muxer | `pkg/muxer/ts/` | PAT/PMT, PES, PCR, adaptation field |
| FMP4 muxer | `pkg/muxer/fmp4/` | Init segment, media segment, box utilities |
| H.265 codec helper | `pkg/codec/h265/` | NAL type parsing |
| MP3 codec helper | `pkg/codec/mp3/` | Frame header parsing |
| Opus codec helper | `pkg/codec/opus/` | Ogg header parsing |
| AV1 codec helper | `pkg/codec/av1/` | OBU parsing |
| HTTP streaming module | `module/httpstream/` | HLS (.m3u8/.ts), DASH (.mpd/.m4s), HTTP-FLV, WebSocket stream |
| Muxer worker | `module/httpstream/muxer_worker.go` | Per-format goroutine, SharedBuffer distribution |

---

### Phase 3 — RTP/SDP/RTSP

| Module | Path | Description |
|--------|------|-------------|
| SDP parser/builder | `pkg/sdp/` | Full SDP parsing, MediaInfo → SDP generation |
| RTP session | `pkg/rtp/session.go` | SSRC/sequence/timestamp management |
| RTCP SR/RR | `pkg/rtp/rtcp.go` | Build and parse Sender Report / Receiver Report |
| H.264 RTP packetizer | `pkg/rtp/h264.go` | FU-A fragmentation, STAP-A, Annex-B conversion |
| H.265 RTP packetizer | `pkg/rtp/h265.go` | FU, AP |
| AAC RTP packetizer | `pkg/rtp/aac.go` | RFC 3640 AAC-hbr |
| Opus RTP packetizer | `pkg/rtp/opus.go` | RFC 7587 |
| G.711 RTP packetizer | `pkg/rtp/g711.go` | PCMU/PCMA |
| VP8 RTP packetizer | `pkg/rtp/vp8.go` | RFC 7741 |
| VP9 RTP packetizer | `pkg/rtp/vp9.go` | Draft |
| AV1 RTP packetizer | `pkg/rtp/av1.go` | Draft |
| MP3 RTP packetizer | `pkg/rtp/mp3.go` | RFC 2250 |
| G.722/G.729/Speex packetizers | `pkg/rtp/g722.go` etc. | Basic framing |
| RTSP request/response parser | `module/rtsp/parser.go` | Full RTSP message parsing |
| RTSP session state machine | `module/rtsp/session.go` | State transitions, timeout, IsExpired() |
| TCP interleaved framing | `module/rtsp/transport.go` | `$` + channel + length + data |
| UDP transport | `module/rtsp/transport.go` | UDPTransport, PortManager port allocation |
| RTSP handler | `module/rtsp/handler.go` | OPTIONS/DESCRIBE/SETUP/PLAY/PAUSE/ANNOUNCE/RECORD/TEARDOWN, EventBus integration |
| RTSP publisher | `module/rtsp/publisher.go` | RTP depacketization → AVFrame, RTCP RR every 5s |
| RTSP subscriber | `module/rtsp/subscriber.go` | AVFrame → RTP packetization, TCP+UDP dual mode, RTCP SR every 5s |
| RTSP server | `module/rtsp/server.go` | Connection management, session reaper (10s interval), EventBus stop events |

**Verified scenarios:**
- TCP push (ANNOUNCE/RECORD) → TCP pull (DESCRIBE/PLAY) ✅
- UDP push → TCP pull ✅
- UDP push → UDP pull ✅ (fixed UDP subscriber hang)
- RTSP push → RTMP pull ✅

---

### Phase 4 — Core Enhancements + REST API

| Module | Path | Description |
|--------|------|-------------|
| Stream stats | `core/stream_stats.go` | Atomic byte/frame/bitrate/FPS counters |
| Connection limits | `core/server.go`, `core/stream.go` | max_streams, max_subscribers_per_stream, max_connections enforcement |
| Alive loop | `core/server.go` | Periodic EventStreamAlive/PublishAlive/SubscribeAlive emission |
| EventBus lifecycle | `core/stream_hub.go` | EventStreamCreate/EventStreamDestroy on hub operations |
| Full REST API | `module/api/handler.go` | Streams list/detail/delete/kick, server info/stats, health check |
| Console login | `module/api/module.go`, `module/api/login.go` | HMAC-SHA256 session cookies, login page, 24h expiry |
| Port separation | `module/api/module.go` | 8080 = media only, 8090 = API + console (protected) |
| API auth | `module/api/module.go` | Bearer token OR session cookie for API endpoints |

---

### Phase 5 — Business Modules (Notify + Record)

| Module | Path | Description |
|--------|------|-------------|
| Notify module | `module/notify/module.go` | Async hooks (priority 90) for all 9 lifecycle events |
| HTTP webhook sender | `module/notify/http_sender.go` | Buffered queue, HMAC-SHA256 signature, retry with backoff |
| WebSocket notification sender | `module/notify/ws_sender.go` | Real-time event stream via WebSocket, event filtering, multi-client broadcast |
| Record module | `module/record/module.go` | Async hooks for publish/publish_stop (priority 50) |
| Record session | `module/record/session.go` | RingBuffer reader with select-based cancellation |
| File writer | `module/record/file_writer.go` | FLV muxer, duration segmentation, path templates |

---

### Phase 6 — WebRTC + TLS

| Module | Path | Description |
|--------|------|-------------|
| WebRTC module | `module/webrtc/` | WHIP publish, WHEP subscribe, session management, ICE trickle |
| WHIP handler | `module/webrtc/whip.go` | SDP offer/answer, pion/webrtc PeerConnection, RTP depacketization → AVFrame |
| WHEP handler | `module/webrtc/whep.go` | AVFrame → RTP packetization, GOP cache replay, realtime/live modes |
| Track sender | `module/webrtc/track_sender.go` | Codec-aware RTP track writing |
| Web console WHIP | `module/api/console.html` | Browser-based camera/mic publish via WHIP, outbound stats |
| TLS support | `core/server.go` | Global TLS config, per-module `*bool` three-state override |

---

## Not Yet Implemented ❌

### Entirely missing (config exists, no code)

| Feature | Config key | Estimated effort |
|---------|-----------|-----------------|
| **SIP** | `sip:` | Large |
| **Cluster forwarding** | `cluster.forward:` | Medium |
| **Cluster origin pull** | `cluster.origin:` | Medium |

### Config stubs (field exists, not enforced)

| Feature | Config key | Status |
|---------|-----------|--------|
| **max_bitrate_per_stream** | `limits.max_bitrate_per_stream` | Config parsed, not enforced in code |
| **audio_cache_ms** | `stream.audio_cache_ms` | Config parsed, not used in stream logic |
| **max_skip_count / max_skip_window** | `stream.max_skip_count` | Config parsed, not enforced in ring buffer |
| **Simulcast** | `webrtc.simulcast` | Config has layer definitions, no layer selection logic |
| **Audio on demand** | `stream.audio_on_demand` | Config exists, stream pause/resume audio not implemented |
| **Stream feedback** | `stream.feedback` | Config exists, passthrough/aggregate not implemented |

---

## Directory Structure

```
liveforge/
├── cmd/liveforge/          # Entry point
├── config/                 # Config loading
├── core/                   # Server, Stream, EventBus, MuxerManager
├── module/
│   ├── api/                # REST API + web console + login auth
│   ├── auth/               # JWT/callback auth
│   ├── httpstream/         # HLS/DASH/HTTP-FLV/HTTP-TS/FMP4/WebSocket
│   ├── notify/             # HTTP webhook notifications
│   ├── record/             # FLV stream recording
│   ├── rtmp/               # RTMP ingest and playback
│   ├── rtsp/               # RTSP ingest and playback (TCP+UDP)
│   └── webrtc/             # WebRTC WHIP/WHEP (via pion/webrtc)
├── pkg/
│   ├── avframe/            # Frame type definitions
│   ├── codec/              # H264/H265/AAC/AV1/MP3/Opus parsing
│   ├── muxer/              # TS/FMP4/FLV muxers
│   ├── rtp/                # Full RTP/RTCP stack
│   ├── sdp/                # SDP parse/build
│   └── util/               # Ring buffer
├── test/integration/       # E2E integration tests
└── docs/
    ├── PROGRESS.md         # This file
    └── superpowers/
        ├── plans/          # Phase 1/2/3 implementation plans
        └── specs/          # Phase 1/2/3 design specs
```

---

## Known Issues / Tech Debt

| Issue | Description | Priority |
|-------|-------------|----------|
| RTMP pull initial stutter | GOP cache burst causes ffplay frame drops on join | Low (expected live stream behavior) |
| No Prometheus metrics | No metrics endpoint exposed | Low |

---

## Development Conventions

- **Commit format**: `<type>: <description>` — types: feat/fix/refactor/docs/test/chore/perf/ci
- **Language**: All code and documentation must be in English
- **Author**: im-pingo <cczjp89@gmail.com>
- **Testing**: Unit tests required for new features, integration tests for critical paths
- **Branch**: main
