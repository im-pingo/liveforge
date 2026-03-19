# Phase 3: RTP/SDP/RTSP Design Spec

## Overview

Add RTSP publish (ANNOUNCE) and subscribe (DESCRIBE/PLAY) support to LiveForge, with both TCP interleaved and UDP transport modes. Builds shared RTP packetization and SDP parsing libraries that will be reused by future WebRTC and SIP modules.

## Architecture

```
pkg/sdp/          — SDP parse/marshal (pure data, no protocol dependency)
pkg/rtp/          — RTP packetize/depacketize (based on pion/rtp)
module/rtsp/      — RTSP signaling + session management
```

`pkg/sdp` and `pkg/rtp` are standalone libraries. The RTSP module uses them for signaling and media transport. Future WebRTC and SIP modules will reuse both packages directly.

## Codec Support

Full codec support per the main design spec:

| Codec | RTP Packetization | RFC |
|-------|-------------------|-----|
| H.264 | FU-A fragmentation | RFC 6184 |
| H.265 | FU/AP fragmentation | RFC 7798 |
| VP8 | VP8 RTP payload | RFC 7741 |
| VP9 | VP9 RTP payload | draft-ietf-payload-vp9 |
| AV1 | AV1 RTP payload | draft-ietf-avt-rtp-av1 |
| AAC | AAC-hbr with AU-header | RFC 3640 |
| Opus | Single frame per packet | RFC 7587 |
| MP3 | MPEG Audio RTP | RFC 2250 |
| G.711 PCMU | Direct payload (8kHz) | RFC 3551 |
| G.711 PCMA | Direct payload (8kHz) | RFC 3551 |
| G.722 | Direct payload (16kHz) | RFC 3551 |
| G.729 | Direct payload (8kHz) | RFC 3551 |
| Speex | Speex RTP payload | RFC 5574 |

### Default Payload Type Mapping

| Codec | PT | Clock Rate |
|-------|----|-----------|
| PCMU | 0 | 8000 |
| PCMA | 8 | 8000 |
| G722 | 9 | 8000 (per RFC 3551, actual 16kHz) |
| G729 | 18 | 8000 |
| H.264 | 96 (dynamic) | 90000 |
| H.265 | 97 (dynamic) | 90000 |
| VP8 | 98 (dynamic) | 90000 |
| VP9 | 99 (dynamic) | 90000 |
| AV1 | 100 (dynamic) | 90000 |
| AAC | 101 (dynamic) | sample_rate |
| Opus | 111 (dynamic) | 48000 |
| MP3 | 14 | 90000 |
| Speex | 102 (dynamic) | sample_rate |

## SDP Package (`pkg/sdp/`)

### Data Structures

```go
type SessionDescription struct {
    Version    int
    Origin     Origin
    Name       string
    Info       string
    Connection *Connection
    Timing     Timing
    Attributes []Attribute
    Media      []MediaDescription
}

type Origin struct {
    Username       string
    SessionID      string
    SessionVersion string
    NetType        string // "IN"
    AddrType       string // "IP4" | "IP6"
    Address        string
}

type Connection struct {
    NetType  string
    AddrType string
    Address  string
}

type Timing struct {
    Start int64
    Stop  int64
}

type Attribute struct {
    Key   string
    Value string
}

type MediaDescription struct {
    Type       string     // "video" | "audio"
    Port       int
    Proto      string     // "RTP/AVP" | "RTP/SAVP"
    Formats    []int      // payload types (numeric only, sufficient for RTP use cases)
    Connection *Connection
    Bandwidth  int
    Attributes []Attribute
}

type RTPMap struct {
    PayloadType  int
    EncodingName string
    ClockRate    int
    Channels     int    // audio only, 0 if not specified
}
```

### Functions

```go
func Parse(data []byte) (*SessionDescription, error)
func (sd *SessionDescription) Marshal() []byte

// MediaDescription helpers
func (md *MediaDescription) RTPMap(pt int) *RTPMap
func (md *MediaDescription) FMTP(pt int) string
func (md *MediaDescription) Control() string         // a=control value
func (md *MediaDescription) Direction() string        // sendrecv|sendonly|recvonly|inactive

// Builder helpers for generating SDP from stream info.
// Uses MediaInfo.SampleRate and MediaInfo.Channels for audio a=rtpmap lines.
// Maps CodecType to RTP encoding name and clock rate via the payload type table.
func BuildFromMediaInfo(info *avframe.MediaInfo, baseURL string, serverAddr string) *SessionDescription
```

### Files

- `sdp.go` — Core types and Parse/Marshal
- `sdp_test.go` — Parse/Marshal round-trip tests
- `builder.go` — SDP generation from MediaInfo for DESCRIBE responses
- `builder_test.go` — Builder tests

## RTP Package (`pkg/rtp/`)

Based on `pion/rtp` for the base `rtp.Packet` type. Custom packetizers/depacketizers per codec.

### Core Interfaces

```go
// Packetizer splits an AVFrame into RTP packets.
// Does not track SSRC/seq/timestamp — those are managed by Session.
type Packetizer interface {
    Packetize(frame *avframe.AVFrame, mtu int) ([]*rtp.Packet, error)
}

// Depacketizer reassembles RTP packets into AVFrames.
// Stateful — buffers fragments until a complete frame is received.
// Returns nil AVFrame when the packet is a fragment that doesn't complete a frame yet.
type Depacketizer interface {
    Depacketize(pkt *rtp.Packet) (*avframe.AVFrame, error)
}

// NewPacketizer creates a codec-specific packetizer.
func NewPacketizer(codec avframe.CodecType) (Packetizer, error)

// NewDepacketizer creates a codec-specific depacketizer.
func NewDepacketizer(codec avframe.CodecType) (Depacketizer, error)
```

### Session

```go
// Session manages per-subscriber RTP state (SSRC, sequence number, timestamp).
type Session struct {
    SSRC        uint32
    PayloadType uint8
    ClockRate   uint32
    seqNum      uint16
    timestamp   uint32
}

// NewSession creates a new RTP session with random SSRC.
func NewSession(pt uint8, clockRate uint32) *Session

// WrapPackets stamps packets with session's SSRC, incrementing seq/timestamp.
func (s *Session) WrapPackets(pkts []*rtp.Packet, dtsMsec int64) []*rtp.Packet
```

### RTCP Helpers

RTCP helpers are placed in `pkg/rtp/` for reuse by future WebRTC/SIP modules.

```go
// pkg/rtp/rtcp.go — minimal RTCP support
func BuildSR(ssrc uint32, ntpTime uint64, rtpTime uint32, packetCount, octetCount uint32) []byte
func BuildRR(ssrc uint32, reports []ReceptionReport) []byte
func ParseSR(data []byte) (*SenderReport, error)
func ParseRR(data []byte) (*ReceiverReport, error)

type SenderReport struct {
    SSRC        uint32
    NTPTime     uint64
    RTPTime     uint32
    PacketCount uint32
    OctetCount  uint32
}

type ReceiverReport struct {
    SSRC    uint32
    Reports []ReceptionReport
}

type ReceptionReport struct {
    SSRC             uint32
    FractionLost     uint8
    TotalLost        uint32
    HighestSeq       uint32
    Jitter           uint32
    LastSR           uint32
    DelaySinceLastSR uint32
}
```

### Packetizer Details

**H.264 (RFC 6184):**
- Single NAL: frame ≤ MTU → one RTP packet
- FU-A: frame > MTU → fragment with FU indicator + FU header
- STAP-A: SPS+PPS combined into one packet (for DESCRIBE response setup)
- Depacketizer: reassemble FU-A fragments, detect frame boundary via RTP marker bit

**H.265 (RFC 7798):**
- Single NAL: frame ≤ MTU → one packet
- FU: frame > MTU → fragment with FU header (2 bytes)
- AP: aggregation packet for small NALUs (VPS+SPS+PPS)
- Depacketizer: reassemble FU fragments, detect boundary via marker bit

**VP8 (RFC 7741):**
- VP8 payload descriptor (1+ bytes) + VP8 payload
- Fragmentation via S/N bits in payload descriptor

**VP9 (draft-ietf-payload-vp9):**
- VP9 payload descriptor + VP9 payload
- I/P/L/F flags, flexible/non-flexible mode

**AV1 (draft-ietf-avt-rtp-av1):**
- AV1 aggregation header + OBU elements
- Each OBU can span multiple RTP packets via fragmentation

**AAC (RFC 3640 mode=AAC-hbr):**
- AU-header-section (2 bytes AU-headers-length + per-AU: 13-bit size + 3-bit index)
- Multiple AAC frames can be packed into one RTP packet

**Opus (RFC 7587):**
- Single Opus frame per RTP packet, no header modifications needed

**MP3 (RFC 2250):**
- 4-byte MPEG Audio header (MBZ + frag_offset) + MP3 frame data

**G.711/G.722/G.729 (RFC 3551):**
- Raw PCM/coded samples directly in RTP payload
- G.711: 8 bytes per ms (8kHz × 8bit), typical 20ms = 160 bytes
- G.722: same PT format as G.711 (clock rate 8000 per RFC, actual 16kHz)
- G.729: 10 bytes per 10ms frame

**Speex (RFC 5574):**
- Speex frame directly in RTP payload

### Files

```
pkg/rtp/
├── packetizer.go       — Packetizer/Depacketizer interfaces, NewPacketizer/NewDepacketizer factories
├── session.go          — RTP Session state management
├── rtcp.go             — RTCP SR/RR build/parse helpers
├── h264.go             — H264Packetizer, H264Depacketizer (FU-A, STAP-A)
├── h264_test.go
├── h265.go             — H265Packetizer, H265Depacketizer (FU, AP)
├── h265_test.go
├── vp8.go              — VP8Packetizer, VP8Depacketizer
├── vp8_test.go
├── vp9.go              — VP9Packetizer, VP9Depacketizer
├── vp9_test.go
├── av1.go              — AV1Packetizer, AV1Depacketizer
├── av1_test.go
├── aac.go              — AACPacketizer, AACDepacketizer (AAC-hbr)
├── aac_test.go
├── opus.go             — OpusPacketizer, OpusDepacketizer
├── opus_test.go
├── mp3.go              — MP3Packetizer, MP3Depacketizer
├── mp3_test.go
├── g711.go             — G711Packetizer, G711Depacketizer (PCMU + PCMA)
├── g711_test.go
├── g722.go             — G722Packetizer, G722Depacketizer
├── g722_test.go
├── g729.go             — G729Packetizer, G729Depacketizer
├── g729_test.go
├── speex.go            — SpeexPacketizer, SpeexDepacketizer
├── speex_test.go
├── rtcp_test.go
├── session_test.go
└── packetizer_test.go  — Factory tests
```

## RTSP Module (`module/rtsp/`)

### Files

```
module/rtsp/
├── server.go        — TCP listener, core.Module implementation
├── conn.go          — RTSP connection: request/response parser, read/write
├── session.go       — RTSP session state machine, session ID management, timeout
├── handler.go       — Command dispatching: OPTIONS/DESCRIBE/SETUP/PLAY/PAUSE/ANNOUNCE/RECORD/TEARDOWN/GET_PARAMETER
├── publisher.go     — RTSP Publisher: receives RTP → Depacketize → AVFrame → Stream.WriteFrame
├── subscriber.go    — RTSP Subscriber: Stream.RingBuffer → AVFrame → Packetize → RTP → send
├── transport.go     — Transport negotiation + UDP RTP sender/receiver + port manager
├── interleaved.go   — TCP interleaved frame: $ + channel(1) + length(2) + payload
├── handler_test.go
├── publisher_test.go
├── subscriber_test.go
└── transport_test.go
```

### RTSP Request/Response Parser

Text-based protocol similar to HTTP/1.1:

```
Request:   METHOD URI RTSP/1.0\r\n Headers\r\n\r\n [Body]
Response:  RTSP/1.0 StatusCode ReasonPhrase\r\n Headers\r\n\r\n [Body]
```

Key headers: `CSeq`, `Session`, `Transport`, `Content-Type`, `Content-Length`.

```go
type Request struct {
    Method  string
    URL     string
    Headers http.Header  // case-insensitive, supports multiple values per key
    Body    []byte
}

type Response struct {
    StatusCode int
    Reason     string
    Headers    http.Header
    Body       []byte
}

func ReadRequest(r *bufio.Reader) (*Request, error)
func WriteResponse(w io.Writer, resp *Response) error
```

### Session State Machine

```
States: Init → Described/Announced → Ready → Playing/Recording → Closed

Transitions:
  Init + DESCRIBE     → Described
  Init + ANNOUNCE     → Announced
  Described + SETUP   → Ready (after all tracks setup)
  Announced + SETUP   → Ready (after all tracks setup)
  Ready + PLAY        → Playing
  Ready + RECORD      → Recording
  Playing + PAUSE     → Ready
  Playing + TEARDOWN  → Closed
  Recording + TEARDOWN → Closed
  Any + TEARDOWN      → Closed
  Any + connection close → Closed
  Any + session timeout → Closed
```

```go
type SessionState uint8

const (
    StateInit SessionState = iota
    StateDescribed
    StateAnnounced
    StateReady
    StatePlaying
    StateRecording
    StateClosed
)

type RTSPSession struct {
    ID         string
    State      SessionState
    StreamKey  string
    Tracks     []*Track       // negotiated tracks
    Transport  TransportMode  // TCP or UDP
    Timeout    time.Duration  // session timeout (default 60s)
    lastActive time.Time      // last activity timestamp
    mu         sync.Mutex
}

type Track struct {
    MediaType   avframe.MediaType
    Codec       avframe.CodecType
    PayloadType uint8
    ClockRate   uint32
    Control     string          // track URL suffix (e.g., "trackID=0")
    // UDP mode:
    ClientRTPPort  int
    ClientRTCPPort int
    ServerRTPPort  int
    ServerRTCPPort int
    // TCP interleaved mode:
    InterleavedRTP  int        // channel number for RTP
    InterleavedRTCP int        // channel number for RTCP
}
```

### Session Timeout

Sessions have a configurable timeout (default 60 seconds). The SETUP response includes `Session: <id>;timeout=60`. Activity that resets the timer:
- Any RTSP request (PLAY, GET_PARAMETER, TEARDOWN, etc.)
- Incoming RTCP packets (for UDP sessions)
- Incoming RTP data (for publish sessions)

A background goroutine in the server checks for expired sessions periodically (every 10s) and cleans them up, closing transport and releasing resources.

### RTSP Command Handling

**OPTIONS:**
```
Response: Public: OPTIONS, DESCRIBE, SETUP, PLAY, PAUSE, ANNOUNCE, RECORD, TEARDOWN, GET_PARAMETER
```

**DESCRIBE** (subscribe path):
1. Parse stream key from URL
2. Look up stream in StreamHub
3. If no stream or no publisher: return `404 Not Found`
4. Build SDP from stream's MediaInfo using `sdp.BuildFromMediaInfo()`
5. Return `200 OK` with `Content-Type: application/sdp` and SDP body

**SETUP** (per track):
1. Parse Transport header
2. If TCP interleaved: allocate channel pair, record in Track
3. If UDP: allocate server RTP/RTCP port pair from port range, record in Track
4. Create session if first SETUP, add track to session
5. Return `200 OK` with negotiated Transport header and `Session: <id>;timeout=60` header

**PLAY:**
1. Verify session state is Ready, with DESCRIBE flow
2. Emit `EventSubscribe` on EventBus (triggers auth + codec check)
3. If auth or codec check hook returns error: return `401 Unauthorized` or `415 Unsupported Media Type`
4. Create RTSP Subscriber, attach to Stream
5. Start sending RTP packets via negotiated transport
6. Return `200 OK` with `RTP-Info` header (seq, rtptime per track)

**PAUSE:**
- For live streams: return `200 OK` but delivery continues (live streams cannot be paused meaningfully). The subscriber can be kept alive to allow PLAY resume.

**ANNOUNCE** (publish path):
1. Parse SDP from body
2. Extract codec info, create MediaInfo
3. Emit `EventPublish` on EventBus (triggers auth)
4. Create Stream in StreamHub, set publisher
5. Return `200 OK`

**RECORD:**
1. Verify session state is Ready, with ANNOUNCE flow
2. Start receiving RTP packets
3. For each complete frame from Depacketizer: `Stream.WriteFrame()`
4. Return `200 OK`

**TEARDOWN:**
1. If playing: remove subscriber, release resources
2. If recording: remove publisher
3. Close transport (UDP ports or TCP interleaved)
4. Return `200 OK`

**GET_PARAMETER:**
- Used as keepalive. Resets session timeout timer. Return `200 OK` with empty body.

### Subscriber Data Flow

The RTSP subscriber reads directly from `Stream.RingBuffer()` (not via MuxerManager — RTP is per-subscriber due to independent SSRC/seq/timestamp state).

```go
func (s *RTSPSubscriber) WriteLoop() {
    // 1. Create RTP Sessions per track from negotiated Track info
    videoSession := rtp.NewSession(track.PayloadType, track.ClockRate)
    audioSession := rtp.NewSession(track.PayloadType, track.ClockRate)

    // 2. Create Packetizers per codec from MediaInfo
    videoPacketizer, _ := rtp.NewPacketizer(stream.MediaInfo().VideoCodec)
    audioPacketizer, _ := rtp.NewPacketizer(stream.MediaInfo().AudioCodec)

    // 3. Send sequence headers as initial RTP packets
    //    For H.264: send SPS/PPS via STAP-A packet
    //    For H.265: send VPS/SPS/PPS via AP packet

    // 4. Send GOP cache with correct RTP timestamps
    //    Convert each frame's DTS (milliseconds) to RTP timestamp:
    //    rtpTimestamp = dts * clockRate / 1000
    for _, frame := range stream.GOPCache() {
        pkts, _ := packetizer.Packetize(frame, 1400)
        pkts = session.WrapPackets(pkts, frame.DTS)
        s.sendPackets(pkts) // via TCP interleaved or UDP
    }

    // 5. Read live frames from RingBuffer
    reader := stream.RingBuffer().NewReader()
    for {
        frame, ok := reader.Read()
        if !ok {
            return // stream closed
        }

        var packetizer rtp.Packetizer
        var session *rtp.Session
        if frame.MediaType.IsVideo() {
            packetizer = videoPacketizer
            session = videoSession
        } else {
            packetizer = audioPacketizer
            session = audioSession
        }

        pkts, err := packetizer.Packetize(frame, 1400)
        if err != nil {
            continue
        }
        pkts = session.WrapPackets(pkts, frame.DTS)
        if err := s.sendPackets(pkts); err != nil {
            return // client disconnected
        }
    }
}

func (s *RTSPSubscriber) sendPackets(pkts []*rtp.Packet) error {
    for _, pkt := range pkts {
        data, _ := pkt.Marshal()
        switch s.transport {
        case TransportTCP:
            // Write interleaved frame: $ + channel + length + data
            s.tcpTransport.WriteInterleaved(s.track.InterleavedRTP, data)
        case TransportUDP:
            s.udpTransport.WriteRTP(data)
        }
    }
    return nil
}
```

### RTCP in RTSP Context

**Publisher direction (server receiving RTP from client):**
- Parse incoming SR (Sender Reports) from the publishing client
- Send RR (Receiver Reports) periodically (every 5s)
- RTCP uses the same transport as RTP: UDP on RTCP port (RTP port + 1), or TCP interleaved on odd channel

**Subscriber direction (server sending RTP to client):**
- Send SR periodically (every 5s) with NTP timestamp + RTP timestamp + packet/octet counts
- Parse incoming RR from the subscribing client for monitoring (log only, no congestion control in Phase 3)

### Transport Layer

```go
type TransportMode uint8

const (
    TransportTCP TransportMode = iota  // TCP interleaved (RTP over RTSP connection)
    TransportUDP                        // UDP (separate RTP/RTCP ports)
)

// UDPTransport manages UDP RTP/RTCP sockets for one track.
type UDPTransport struct {
    rtpConn    *net.UDPConn
    rtcpConn   *net.UDPConn
    clientAddr *net.UDPAddr
}

// TCPTransport wraps RTP data in interleaved frames over the RTSP TCP connection.
type TCPTransport struct {
    conn net.Conn
    mu   sync.Mutex  // serialize writes
}

// PortManager allocates UDP port pairs from the configured range.
// Scans from minPort upward in steps of 2, returning the first even port
// where both port (RTP) and port+1 (RTCP) are free.
type PortManager struct {
    minPort int
    maxPort int
    mu      sync.Mutex
    used    map[int]bool
}

func (pm *PortManager) Allocate() (rtpPort, rtcpPort int, err error)
func (pm *PortManager) Release(rtpPort int)
```

**TCP Interleaved Frame Format:**
```
$ (0x24) | channel (1 byte) | length (2 bytes big-endian) | RTP/RTCP data
```

Even channels = RTP, odd channels = RTCP (convention: channel 0/1 for first track, 2/3 for second).

### Integration with Core

- `server.go` implements `core.Module` interface
- Registered in `cmd/liveforge/main.go` when `cfg.RTSP.Enabled`
- Publisher implements `core.Publisher` interface
- Subscriber reads directly from `Stream.RingBuffer()` (not via MuxerManager — RTP is per-subscriber)
- Events emitted: `EventPublish`, `EventPublishStop`, `EventSubscribe`, `EventSubscribeStop`
- Auth module hooks into `EventPublish`/`EventSubscribe` events (sync, priority 10) for token/callback verification
- CodecCheck module hooks into `EventSubscribe` (sync, priority 15) to verify codec compatibility between stream codecs and RTSP-supported codecs. Returns `415 Unsupported Media Type` if incompatible.

### Config

Uses the existing `RTSPConfig` already defined in `config/config.go`:

```go
// Already exists in config/config.go
type RTSPConfig struct {
    Enabled      bool   `yaml:"enabled"`
    Listen       string `yaml:"listen"`
    RTPPortRange []int  `yaml:"rtp_port_range"` // slice, validated at startup to contain exactly 2 elements
}
```

```yaml
rtsp:
  enabled: true
  listen: ":554"
  rtp_port_range: [10000, 20000]
```

## Error Handling

- Unknown stream in DESCRIBE/PLAY: `404 Not Found`
- Stream exists but no publisher: `503 Service Unavailable`
- Auth failure (from EventBus hook): `401 Unauthorized`
- Codec incompatibility (from CodecCheck hook): `415 Unsupported Media Type`
- Unsupported transport: `461 Unsupported Transport`
- Session not found: `454 Session Not Found`
- Invalid state transition: `455 Method Not Valid In This State`
- Port range exhausted: `503 Service Unavailable`
- Session timeout: silent cleanup (no response sent — session already expired)

## Testing Strategy

1. **Unit tests** for SDP: parse/marshal round-trip, builder output
2. **Unit tests** for RTP: per-codec packetize/depacketize round-trip with known payloads
3. **Unit tests** for RTCP: SR/RR build/parse round-trip
4. **Unit tests** for RTSP: request/response parsing, session state machine, session timeout
5. **Integration test**: RTMP push → RTSP pull with simulated RTSP client
6. **Integration test**: RTSP push → RTMP pull with simulated RTSP publisher
7. **E2E test**: `ffmpeg -f rtsp` push + `ffplay rtsp://` pull
