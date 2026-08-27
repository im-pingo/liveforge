# SIP And GB28181 Protocol Test Lab

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

The Console includes local one-shot protocol self-tests and persistent fake-device
sessions so SIP and GB28181 workflows can be validated without a PBX, cloud
platform, or camera. Enable SIP, its `gateway` block, and GB28181, keep the API
listener on loopback, and use a viewer or operator token.
The checked-in sample includes a loopback-safe gateway port range so this page is
usable immediately in local development:

```bash
export LIVEFORGE_API=http://127.0.0.1:8090
export VIEWER_TOKEN='replace-me'
export OPERATOR_TOKEN='replace-me'
```

## SIP

```bash
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/sipgateway/test"
```

The report is returned by `GET /api/v1/sipgateway/test` and runs an in-process
fake SIP peer through REGISTER and 401 digest challenge, authenticated
registration, INVITE/200/ACK/BYE, incompatible-codec rejection, timeout
handling, RTP media, and RTCP control. It also checks SDP parsing and codec
negotiation against the configured gateway codecs and an RTP/RTCP port pair.
The Console SIP page renders every phase and its failure detail.

For a persistent provider session, call the `SIPGatewayProvider` methods
`StartLabSession(ctx, LabSessionRequest)`, `ListLabSessions()`, and
`StopLabSession(id)`. In `publish` mode, the fake device creates a real inbound
SIP call and sends deterministic H.264 video plus PCMA/PCMU audio on separate
RTP tracks, with RTCP, into the gateway-created stream. The gateway binds and
parses both real RTCP receiver sockets, and the Lab counter reflects packets
accepted there rather than successful UDP writes. In `receive` mode, the fake
SIP endpoint accepts the gateway outbound INVITE, receives the existing source
without writing generated frames into that stream, counts audio/video RTP and
RTCP, and sends periodic receiver reports for each track. Gateway sender reports
use each RTP track's SSRC, RFC NTP time, and per-track payload packet/octet
counts. Stop is idempotent;
the start signaling context is derived from both the caller and session stop
context, so `StopLabSession` and `Gateway.Close` cancel unanswered starts and
release their sockets. This provider workflow requires an initialized SIP
transport and enabled gateway.
The publish stream contains a dependency-free moving 160x90 constrained-baseline
H.264 pattern at 25 fps, with one IDR per 25-frame loop, plus audible 20 ms
PCMA/PCMU frames. The Console uses the video player and prefers WebRTC/WHEP for
direct G.711 passthrough, so this path does not require FFmpeg. HTTP-FLV,
HTTP-TS, fMP4, HLS, and DASH remain available as protocol outputs, but browser
audio support for G.711 still depends on the selected output/player.

The HTTP control surface is:

```bash
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/sipgateway/lab/sessions"
curl -fsS -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"mode":"publish","device_id":"sip-lab-device","stream_key":"sip/lab","codec":"PCMA"}' \
  "$LIVEFORGE_API/api/v1/sipgateway/lab/sessions"
```

`GET` requires `sip:read`; `POST` and `DELETE` require `sip:calls`. The response
contains aggregate counters, separate audio/video RTP counters, and relative
HTTP-FLV, WS-FLV, TS, fMP4, HLS, DASH, and WHEP playback paths when those
listeners are enabled. Stream-key path segments are URL-escaped, and the
Console Preview action consumes these returned paths directly. HLS, LL-HLS,
and DASH manifests retain the same segment-wise escaping, including media
segment requests for valid stream keys with more than two path components;
DASH additionally XML-escapes the generated URL attributes. Absolute
RTMP/RTSP URLs use the actual bound listeners and replace wildcard bind hosts
with the management request host. Use the returned
session ID with `DELETE` to stop it. A failed session remains visible with a
bounded, redacted `last_error` so a rejected INVITE, unavailable stream, codec
mismatch, or timeout can be diagnosed from the API and Console.

## GB28181

```bash
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/gb28181/test"
```

The report is returned by `GET /api/v1/gb28181/test` and runs an in-process fake
device through SIP registration, Keepalive, Catalog query/response, playback
INVITE/200 SDP/ACK/BYE, missing-SDP rejection, timeout handling, PS/90000 media
over localhost UDP, and RTCP control. It also checks an RTP/RTCP port pair and
local PS mux/demux of an H.264 keyframe. It does not contact a platform or
camera. The Console GB28181 page renders every phase and its detail.

Persistent GB28181 sessions use the same control shape and perform real
REGISTER, Keepalive, Catalog, INVITE, ACK, BYE, and unregister signaling. In
`publish` mode the fake device starts a listening SIP endpoint and advertises
that Contact during registration. LiveForge then uses its normal invite client
to initiate live play; the fake device accepts INVITE, consumes ACK without a
response, handles BYE, and sends deterministic H.264 plus G.711A in PS/RTP
payload type 96 with RTCP into LiveForge's real RTP/RTCP receiver. In `receive`
mode LiveForge validates an existing H.264/G.711A source before activation and
a module-owned outbound media session admits a source subscriber before it
sends that source as PS/RTP/RTCP. Subscriber-limit rejection is returned
synchronously and the Lab is never published as active. If the sender later
fails, the Lab transitions to `failed`, records a bounded redacted diagnostic,
and releases its dialog, module session, subscriber, sockets, and ports. The
fake device only receives and counts RTP, RTCP, PS, audio, and video frames. The
simulator binds only loopback sockets, requires no FFmpeg or external platform,
and releases both SIP UAs, dialogs, RTP/RTCP ports, and session resources on
stop or module close:

After the initial registration, each persistent fake device continues sending
Keepalive messages at roughly one-third of the configured
`gb28181.keepalive.timeout` (bounded to a practical interval), so a session
remains online for long-running preview and cross-protocol tests.

```bash
curl -fsS -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"mode":"publish","device_id":"34020000001320000011","channel_id":"34020000001320000012","stream_key":"gb28181/34020000001320000012"}' \
  "$LIVEFORGE_API/api/v1/gb28181/lab/sessions"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/gb28181/lab/sessions"
```

`GET` requires `gb28181:read`; `POST` and `DELETE` require
`gb28181:control`. Receive mode requires the requested stream to already have
an H.264 video and G.711A audio publisher. The publish sample includes a moving constrained-baseline
SPS/PPS/IDR/interframe pattern at 25 fps and audible 8 kHz mono G.711A audio, so the
resulting stream can be decoded by the Console and other H.264-capable outputs.
The lab Preview action uses the
requested stream key. The response includes
PS/RTP/RTCP counters, separate audio/video frame counts, and cross-protocol
playback paths. Lab responses and 400/404 errors use the management
`{code,message,data}` envelope. A failed session keeps a bounded `last_error`
with SIP credentials and bearer tokens removed before truncation; a disabled
or uninitialized module returns 503.

SIP and GB28181 Lab `stream_key` values must be printable ASCII no longer than
256 bytes. Every slash-separated segment must be non-empty and cannot be `.` or
`..`; the API and Console reject ambiguous keys before a session starts. For
GB28181 `publish`, the accepted key is authoritative. The simulator carries it
in a private SIP header that the handler honors only for loopback requests.
Normal network devices continue to publish to
`{stream_prefix}/{channel_id}`.

Both endpoints require the normal `sip:read` or `gb28181:read` permission. A
viewer may run them; a disabled or uninitialized module returns 503 and the page
must show the test as unavailable rather than a successful report.

When SIP Gateway and GB28181 share the same SIP listener, INVITE dispatch uses
the SDP media identity: H.264 plus PCMA/PCMU RTP offers are handled by SIP
Gateway, while video RTP/AVP payload 96 with `PS/90000` is handled by GB28181. This allows the
SIP lab to use short identities such as `device_id=d1` and `stream_key=s1`
without GB28181 creating a competing publisher. After either lab publishes, use
the returned cross-protocol paths to verify playback through the enabled HTTP,
RTSP, HLS, DASH, or WebRTC output.

Each Lab manager retains all active sessions plus the newest 16 terminal
records. Starting and stopping more sessions prunes only the oldest terminal
records; active sessions are never removed by history maintenance.

The Streams Console reports the video GOP and rolling audio cache separately.
`GOP #N` is a stream-lifetime generation that increments for every keyframe, so
cache replacement remains visible even when the polling interval repeatedly
samples the same point in a fixed GOP cycle. For a healthy one-second sample
loop, the GOP grows after the first IDR and contains video plus interleaved
audio, while Audio Cache reports independently according to
`stream.audio_cache_ms`. A zero GOP before the first keyframe is normal; a
persistent zero GOP with increasing video-frame totals is not.

## Verification

Run focused tests without external services:

```bash
go test ./module/api ./module/sipgateway ./module/gb28181
go test ./pkg/rtp -run TestH264DepacketizerEmitsSequenceHeaderForSeparateSPSAndPPSPackets -count=1
go test ./module/webrtc -run 'Test(RegisterCodecs|WHEPPCMAudioPassthroughDeliversRTP)$' -count=1
go test -race ./module/gb28181 -run 'Lab|SelfTest' -v
```

The self-tests bind only ephemeral localhost UDP sockets and release their port
pairs before returning. They do not write recordings or configuration.
