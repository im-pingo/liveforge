# Browser camera and microphone publishing

Open `/console/publish` as an operator or administrator over HTTPS or localhost.
The checked-in sample is local development only: authentication and TLS are
disabled and its Console credentials are `admin/admin`.

1. Select WHIP/WebRTC, SIP, or GB28181.
2. Allow camera and microphone access and select the desired devices. Every
   protocol shows the same local preview. Capture stays local until Start.
3. Enter a target stream key and, for SIP/GB28181, device/channel identifiers.
   SIP also selects PCMA or PCMU audio.
4. Start publishing. Use Streams Preview to view the target over WHEP, including
   its G.711 audio. Stop releases the camera, microphone and owned sessions.

All three modes use real browser `getUserMedia` capture. SIP requires browser
H.264 and 8 kHz mono PCMA/PCMU; GB28181 requires H.264 and PCMA. Missing codec
support is an explicit startup error. WHIP retains its video codec selector.
This workflow has no FFmpeg dependency and works with the portable build.

Enable WebRTC and the selected SIP Gateway or GB28181 module, including its
shared SIP transport. Management authentication and ordinary stream publish
and subscribe policies still apply. Browsers cannot open ordinary SIP/UDP
sockets: the page creates a temporary `_console/<uuid>` WHIP source, waits at
most ten seconds for the server to receive audio/video, and asks the existing
local protocol provider to forward that source. Actual captured media then
passes through SIP signaling and separate RTP tracks, or GB28181 signaling and
PS/RTP, into the requested target stream. These modes publish into LiveForge's
local receiver; remote PBX/platform destinations use separate gateway workflows.

The temporary source is an ordinary visible stream and may match configured
recording or other stream policies. It disappears as an active publisher on
cleanup; ordinary idle stream retention still applies. The page stops capture
on Stop, startup failure, device disconnection, connection failure or terminal
protocol state. A late startup response is deleted without affecting a newer
run. Closing the page sends best-effort session cleanup; transport timeouts
remain the fallback if the browser disappears without sending requests.

## Source-backed protocol API

Both `POST /api/v1/sipgateway/lab/sessions` and
`POST /api/v1/gb28181/lab/sessions` accept optional `source_stream_key` in
`publish` mode, and return it in their session snapshots. Omission preserves
the deterministic test-device workflow described in
[protocol-test-lab.md](protocol-test-lab.md). Receive and generated-media
controls remain on the protocol management pages.

The source must already have an active publisher. Source and target keys must
differ, be printable ASCII of at most 256 bytes, and have non-empty slash
segments other than `.` and `..`. Providers admit one publisher generation
and wait at most five seconds for H.264 headers and the required G.711 audio
before allocating signaling/media resources. Subscriber admission limits apply.
This mode does not transcode mismatched audio. Source EOF, replacement or ring
overwrite fails the session and releases its reader, subscription and sockets;
it never switches to the replacement or stops the source publisher itself.

SIP requires `sip:calls` for start/stop and `sip:read` for monitoring. GB28181
requires `gb28181:control` and `gb28181:read`. The Console also reads stream
status while waiting for capture readiness. Existing operator/admin roles
include these permissions. No new configuration fields or public ports are
introduced.

## Verification

```bash
CGO_ENABLED=0 go test ./module/sipgateway ./module/gb28181 -run 'LabPublishSource' -count=1
LIVEFORGE_REQUIRE_BROWSER=1 CGO_ENABLED=0 go test ./module/api -run 'TestConsoleCamera|TestConsoleProtocolPublishSharesCameraAndMicrophone|TestConsolePublishFlow' -count=1
LIVEFORGE_REQUIRE_BROWSER=1 CGO_ENABLED=0 go test ./test/integration -run '^TestConsoleCameraProtocolPublish$' -count=1 -v
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
```

Set `CHROME_BIN` to the H.264-capable Chrome executable when discovery is
ambiguous. Required-browser mode fails if Chrome or its required codecs are
unavailable. Browser tests use fake camera/microphone devices through the same
capture API: they verify source and target codecs, decoded target dimensions,
advancing audio/video, stop/restart and resource release. They do not constitute
verification of a particular physical camera or microphone.
