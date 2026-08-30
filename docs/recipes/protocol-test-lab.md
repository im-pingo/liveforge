# SIP And GB28181 Protocol Test Lab

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

The Console includes local one-shot protocol self-tests and persistent fake-device
sessions so SIP and GB28181 workflows can be validated without a PBX, cloud
platform, or camera. Enable SIP, its `gateway` block, and GB28181, keep the API
listener on loopback, and use a viewer or operator token.
Each provider admits at most `max_lab_sessions` active `starting`, `active`, or
SIP `contract` sessions at once. The default is 16; terminal history is retained
for diagnosis but does not consume the ceiling. Set a positive value in
`sip.gateway.max_lab_sessions` or `gb28181.max_lab_sessions`; non-positive values
are normalized to the default. When the ceiling is full, the start API returns
HTTP 429 with the `ProtocolLabCapacity` error instead of allocating sockets or
publishing a partially active session.
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
The port check binds both configured UDP sockets, fails when every pair is
occupied by another process, and closes/frees a successful reservation before
returning. The Console SIP page renders every phase and its failure detail.

For a persistent provider session, call the `SIPGatewayProvider` methods
`StartLabSession(ctx, LabSessionRequest)`, `ListLabSessions()`, and
`StopLabSession(id)`. In `publish` mode, the fake device creates a real inbound
SIP call and sends deterministic H.264 video plus PCMA/PCMU audio on separate
RTP tracks, with RTCP, into the gateway-created stream. The gateway binds and
parses both real RTCP receiver sockets, and the Lab counter reflects packets
accepted there rather than successful UDP writes. Gateway RTP/RTCP allocation
skips pairs already occupied by another local process and keeps both sockets
bound before SDP is accepted or offered, closing the allocation only during
setup rollback or session cleanup. Fake Lab endpoint pairs avoid the configured
gateway RTP range. In `receive` mode, the fake
SIP endpoint accepts the gateway outbound INVITE, receives the existing source
without writing generated frames into that stream, counts audio/video RTP and
RTCP, and sends periodic receiver reports for each track. Gateway sender reports
use each RTP track's SSRC, RFC NTP time, and per-track payload packet/octet
counts. Stop is idempotent; the start signaling context is derived from both
the caller and session stop context, so `StopLabSession` and `Gateway.Close`
cancel unanswered starts and release their sockets. Lab shutdown stops the
transport reader before closing its fake SIP UA, so normal cleanup does not
underflow sipgo UDP references or report an already-closed socket. This provider
workflow requires an initialized SIP transport and enabled gateway. Receive mode
waits for the selected publisher generation to become startup-ready before
sending its INVITE. The requested PCMA or PCMU value is the actual outbound
target codec, not just a display hint. A matching source is passed through;
when the source differs, the generation-bound shared audio transcoder supplies
an independent target-codec reader while H.264 continues from the original
live cursor. That conversion requires `audio_codec.enabled=true`, the
`audiocodec` build tag, and FFmpeg development libraries. A source without a
direct or available transformed path is rejected before signaling, while a
source with late sequence headers is waited on or canceled with the request
context.

Gateway RTP/RTCP pairs are socket-bound before SDP and remain owned by the call
until teardown. For a transcoded outbound call, every ready target-audio frame
rechecks its captured publisher generation immediately before RTP send. Source
retirement closes and releases the target reader, generation subscriber, and
media sockets, returns the exact port pair to the allocator, and sends one BYE
even if another local teardown races with retirement.

The publish stream contains a dependency-free moving 160x90 constrained-baseline
H.264 pattern at 25 fps, with one IDR per 25-frame loop, plus audible 20 ms
PCMA/PCMU frames. The Console uses the video player and prefers WebRTC/WHEP for
direct G.711 passthrough, so this path does not require FFmpeg. WHEP starts
asynchronously received media muted when browser autoplay policy requires it
and exposes an Unmute/Mute control after audio arrives. HTTP-FLV, HTTP-TS, fMP4,
HLS, and DASH remain available as protocol outputs, but browser audio support
for G.711 still depends on the selected output/player. fMP4 consumers parse
every concatenated `moof`/`mdat` fragment in a complete media segment instead
of retaining only the last fragment.

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
local PS mux/demux of an H.264 keyframe. The configured pair is accepted only
after both UDP sockets bind; external exhaustion fails this check, while a
successful check closes both sockets and frees the pair. It does not contact a
platform or camera. The Console GB28181 page renders every phase and its detail.

Persistent GB28181 sessions use the same control shape and perform real
REGISTER, Keepalive, Catalog, INVITE, ACK, BYE, and unregister signaling. In
`publish` mode the fake device starts a listening SIP endpoint and advertises
that Contact during registration. LiveForge then uses its normal invite client
to initiate live play; the fake device accepts INVITE, consumes ACK without a
response, handles BYE, and sends deterministic H.264 plus G.711A in PS/RTP
payload type 96 with RTCP into LiveForge's real RTP/RTCP receiver. In `receive`
mode LiveForge requires H.264 plus direct G.711A or source audio that the
tagged runtime can convert to G.711A. A module-owned outbound media session
admits a source subscriber before activation, keeps H.264 on the source live
cursor, and uses an independent generation-bound target-audio reader when
conversion is needed. Unsupported conversion fails before signaling, while a
supported source is sent as PS/RTP/RTCP. Subscriber-limit rejection is returned
synchronously and the Lab is never published as active. If the sender later
fails, the Lab transitions to `failed`, records a bounded redacted diagnostic,
and releases its dialog, module session, subscriber, sockets, and ports. The
fake device only receives and counts RTP, RTCP, PS, audio, and video frames. The
simulator binds only loopback sockets and requires no external platform. Direct
G.711A requires no FFmpeg; converting Opus, AAC, or another supported source
codec requires the `audiocodec` build. The Lab releases both SIP UAs, dialogs,
RTP/RTCP ports, target-audio readers, and session resources on stop or module
close. The fake-client transport reader exits before its UA and
the fake-peer listener exits before its peer UA, avoiding sipgo UDP reference
underflow and closed-socket cleanup warnings.

Inbound device INVITEs complete asynchronous publish-start admission before
LiveForge exposes `200 OK`. Backpressure returns a non-2xx response and removes
the publisher, session, receiver sockets, newly created stream, and allocated
pair without emitting an unmatched publish-stop. GB28181 receive-mode outbound
media also allocates and binds its RTP/RTCP pair atomically, so an externally
occupied first pair is skipped in favor of a later configured pair.

Successful server-initiated live and playback INVITEs transfer dialog ownership
to the media session. Local stop, receiver failure, rollback after an accepted
2xx response, and repeated cleanup converge on one managed dialog, so at most
one BYE is sent and the transaction is closed once.

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
H.264 video and either direct G.711A audio or an audio codec the running tagged
build can transform to G.711A, with the required startup sequence headers. The
receive path waits for that publisher generation to become ready
before sending its INVITE; a late header is not treated as a playable source
until it arrives. The publish sample includes a moving constrained-baseline
SPS/PPS/IDR/interframe pattern at 25 fps and audible 8 kHz mono G.711A audio, so the
resulting stream can be decoded by the Console and other H.264-capable outputs.
The lab Preview action uses the
requested stream key. The response includes
PS/RTP/RTCP counters, separate audio/video frame counts, and cross-protocol
playback paths. Lab responses and 400/404 errors use the management
`{code,message,data}` envelope. A failed session keeps a bounded `last_error`
with SIP credentials and bearer tokens removed before truncation; a disabled
or uninitialized module returns 503.

GB28181 outbound PS accepts internal Annex-B video and converts AVCC/HVCC H.264
or H.265 samples to Annex-B before muxing. This keeps Lab receive output and
normal GB28181 egress decodable when the source originated from an MP4-style
length-prefixed protocol.

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

## Pure-Audio HTTP Segments

An AAC-only publisher does not have video keyframes to close HTTP media
segments. HLS, DASH, and LL-HLS therefore close audio-only segments by elapsed
media time while the source remains live. The frame at the duration boundary
starts the next segment and is included once. Video-bearing streams remain
keyframe-bounded.

For LL-HLS, `part_duration` controls the target duration of low-latency parts,
while `segment_duration` controls completed full segments. The default full
segment target is `1.0` second and values below `0.1` are rejected by the
configuration schema. Both settings are hot-reloadable; existing segment
managers stop and later requests recreate them with the new policy.
An initial LL-HLS request waits for one complete segment using a
millisecond-rounded `segment_duration + part_duration` budget, with a 10-second
floor and 30-second cap. If that budget expires or the manager stops first, the
request returns HTTP 503 instead of exposing a part-only playlist.

```yaml
http_stream:
  llhls:
    enabled: true
    part_duration: 0.2
    segment_duration: 1.0
    segment_count: 4
    container: fmp4
```

For an AAC-only stream key `live/audio`, inspect the live outputs before
stopping the publisher:

```bash
curl -fsS http://127.0.0.1:8080/live/audio.m3u8
curl -fsS -o /tmp/liveforge-audio-0.ts http://127.0.0.1:8080/live/audio/0.ts
curl -fsS -o /tmp/liveforge-audio-0.m4s http://127.0.0.1:8080/live/audio/0.m4s
curl -fsS http://127.0.0.1:8080/live/audio.mpd
curl -fsS -o /tmp/liveforge-audio-init.mp4 http://127.0.0.1:8080/live/audio/audio_init.mp4
curl -fsS -o /tmp/liveforge-audio-a1.m4s http://127.0.0.1:8080/live/audio/a1.m4s
```

The `.m3u8` route serves HLS or LL-HLS according to the active module policy;
LL-HLS full segments use `.ts` or `.m4s` according to `container`, while HLS
uses `.ts`. DASH uses the audio init and `a$Number$.m4s` media routes. The first
full audio-only segment should become available near its configured target
duration, without waiting for source shutdown or a video keyframe.
DASH rebases the first source timestamp once and keeps all later fMP4 fragments
on that same relative decode timeline, including when the source starts at DTS
zero.

The startup cache is the video-keyframe-bounded GOP. `GOP #N` is a
stream-lifetime generation that increments for every keyframe, so replacement
remains visible even when the polling interval repeatedly samples the same point
in a fixed GOP cycle. For a healthy one-second sample loop, the GOP grows after
the first IDR and contains video plus interleaved audio. A pure-audio stream
starts from the live cursor without retained startup history. A zero GOP before
the first keyframe is normal; a persistent zero GOP with increasing video-frame
totals is not.

The Streams Console labels this surface `GOP Cache` and reports one
keyframe-driven generation with interleaved video/audio frame counts and
duration. Audio-only streams show `Not applicable (audio-only)` because they
have no video-keyframe-bounded startup GOP.

## Verification

Run focused tests without external services:

```bash
go test ./module/api ./module/sipgateway ./module/gb28181
go test ./pkg/rtp -run TestH264DepacketizerEmitsSequenceHeaderForSeparateSPSAndPPSPackets -count=1
go test ./module/webrtc -run 'Test(RegisterCodecs|WHEPPCMAudioPassthroughDeliversRTP)$' -count=1
go test -race ./module/gb28181 -run 'Lab|SelfTest' -v
CGO_ENABLED=1 go test -tags audiocodec ./test/integration -run '^TestSIPGB28181WHIPBrowserBridgeMatrix$' -count=1 -v
LIVEFORGE_PROTOCOL_MATRIX_SOAK=60s CGO_ENABLED=1 go test -tags audiocodec ./test/integration -run '^TestSIPGB28181WHIPBrowserBridgeMatrix$' -count=1 -v
```

The Chromium matrix checks SIP publish to GB28181 receive plus WHEP,
GB28181 publish to SIP receive plus WHEP, and WHIP H.264/Opus publish to both
SIP and GB28181 receive plus WHEP. It requires expected decoded dimensions,
connected ICE, no browser media error, increasing video/audio RTP and decoded
frame counters, an advancing media clock, and WHEP server RTP/RTCP state that
never enters `media_stalled`. The browser checks run when Chromium advertises
H.264 receive support; otherwise the test reports an environment skip, while
Pion negotiation tests remain mandatory. The soak duration is a correctness
soak; it is not evidence of leak freedom, concurrency capacity, or deployment
capacity.

The self-tests bind their configured RTP/RTCP pair plus ephemeral localhost UDP
sockets and release every pair before returning. They do not write recordings
or configuration.
