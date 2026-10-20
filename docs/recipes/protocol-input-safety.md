# Protocol input guards

LiveForge applies bounds before retaining untrusted RTSP control messages or H.264 fragments. These guards complement authentication, connection limits and deployment capacity planning.

## RTSP server and cluster peers

The shared control framing reader permits at most 8 KiB per start/header line, 64 KiB for the whole header section, and 1 MiB for the body. Header limits count wire bytes, including line terminators. A single unsigned decimal Content-Length is required when a length is present; empty, signed, invalid, duplicated and oversized lengths are rejected. Transfer-Encoding is unsupported and rejected. The reader rejects a declared oversized body before allocating or reading it and consumes exactly an accepted body, preserving a following message on the connection.

The RTSP server closes a connection on malformed control input. A cluster RTSP client rejects an invalid peer response and fails the relay setup instead of trusting the advertised allocation size. RTSP interleaved RTP remains separately framed by its existing 16-bit packet length.

The server waits up to 60 seconds for the first/next byte, then allows at most 10 seconds to complete that control or interleaved packet. Sending a byte periodically does not renew the completion budget. Expired reads close the connection and release its global connection slot. Read deadlines are cleared before calling session/media handlers; these limits do not impose a 10-second stream lifetime. The cluster client has framing bounds but retains its existing caller-context cancellation policy.

## H.264 RTP input

Single NAL, STAP-A aggregation and FU-A fragmentation are validated before output:

- Raw NAL and FU assembly length/capacity: 16 MiB.
- FU assembly: at most 16,384 fragments, including empty fragments.
- AVCC output including length prefixes: at most 16 MiB + 4 bytes.
- Each SPS/PPS: at most 65,535 bytes, matching the avcC length fields.
- FU continuations require consecutive RTP sequence numbers, including wraparound, plus matching timestamp, SSRC, payload type and reconstructed NAL header.

Truncated STAP lengths, invalid NAL headers, orphan/mixed fragments and exceeded bounds return an error without partial frames. Errors discard partial assembly; a new FU start can recover. Complete NAL/STAP packets also discard an abandoned FU. RFC 6184 empty FU payloads and the ignored reserved R bit remain supported. Valid SPS/PPS caching and sequence-header-before-media behavior are preserved. These checks validate framing and resources, not every H.264 bitstream syntax rule.

## RTSP and WHIP video access units

RTSP and WHIP use the same RTP video assembler. Each track retains at most 16 MiB + 4 bytes of accumulated media (length and capacity, including AVCC prefixes) and at most 16,384 RTP packets per access unit. The assembler additionally bounds AV1/VP9 codec input before decoding; the existing H.264/H.265/VP8 fragment limits still apply inside their depacketizers. These are retained per-buffer/per-track bounds, not a total process memory ceiling. The legacy AV1/VP9 depacketizers can transiently over-allocate through Go append; the assembler replaces that storage with a capped reserve so later small continuations do not repeatedly grow and copy the whole frame.

A missing Marker, timestamp/source/payload-type discontinuity, sequence gap, malformed codec input or overflow must never concatenate an abandoned frame with later media. A damaged unit is dropped through its remaining packets; a fresh unit can recover. Sequence headers are emitted separately before media, never used to flush an unfinished frame. RTSP TCP and UDP keep receiving after recoverable media loss, while closed/replaced publishers still terminate ingress. Codec state is serialized without holding the publisher lifecycle lock during parsing, so shutdown/replacement can proceed independently. Each RTSP payload type has separate assembly and clock-origin state; video uses 90 kHz and audio uses the existing codec-specific RTP rate. Audio samples do not wait for Marker. This does not add RTCP sender-report-based audio/video synchronization.

WHIP keeps its shared media clock, publisher ownership checks, Simulcast processing epochs and audio path. Resuming a paused video layer starts with fresh assembly state and retains the existing header/keyframe gate.

## Notification WebSockets

When enabled, the default `/api/v1/events` route uses management authentication and `server:read`, including when a custom path is configured. Browser Origin must match the request host and port; the library does not compare the scheme. Native clients may omit Origin. Cross-host/port or opaque Origin handshakes receive 403 before client registration.

Sender shutdown permanently closes admission. A later handshake receives 503; an upgrade already in progress is closed before registration. Normal client event filters remain unchanged. Use a same-host reverse-proxy route for browser consumers, preserve the intended public Host, and retain the API's authentication/TLS settings. The local sample's anonymous compatibility mode is not safe for public exposure.

## Verification

```sh
CGO_ENABLED=0 go test ./module/rtsp ./module/cluster ./module/notify ./module/webrtc ./pkg/rtp -count=1
CGO_ENABLED=1 go test -tags audiocodec -race ./pkg/rtp -count=1
tools/check-agent-docs_test.sh
```

The real TCP incomplete-body regression takes approximately 10 seconds and checks that the server releases its connection slot. WebSocket tests exercise real upgrade/close races and accepted event delivery. RTP tests cover malformed aggregation, fragment/access-unit limits, continuity, parameter caching, recovery and two bounded fuzz targets. RTSP regressions exercise real TCP/UDP recovery, concurrent tracks and independent audio/video clocks; WHIP regressions exercise the actual receive loop. Do not run a second cluster test binary concurrently. Native FFmpeg and full browser acceptance require their own usable toolchain and browser; a passing pure RTP package does not prove those dependencies are available.
