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
SIP call and sends deterministic PCMA/PCMU RTP and RTCP into the gateway-created
stream. In `receive` mode, it creates a fake SIP endpoint that accepts the
gateway outbound INVITE and counts received RTP and RTCP. Stop is idempotent;
the start signaling context is derived from both the caller and session stop
context, so `StopLabSession` and `Gateway.Close` cancel unanswered starts and
release their sockets. This provider workflow requires an initialized SIP
transport and enabled gateway.

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
contains RTP/RTCP counters and relative HTTP-FLV, WS-FLV, TS, fMP4, HLS, DASH,
and WHEP playback paths when those listeners are enabled. Use the returned
session ID with `DELETE` to stop it.

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
`publish` mode the fake device sends deterministic H.264 in PS/RTP payload type
96 plus RTCP into the registered channel stream. In `receive` mode it accepts
LiveForge's live-play INVITE, sends the selected H.264 source as PS/RTP, and
counts received RTP/RTCP/PS frames. The simulator binds only loopback sockets,
requires no FFmpeg or external platform, and releases SIP, RTP, RTCP, and
session resources on stop or module close:

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
an H.264 publisher. The response includes PS/RTP/RTCP counters and cross-
protocol playback paths. A disabled or uninitialized module returns 503.

For `publish`, `stream_key` is authoritative and must be a printable ASCII
value without leading/trailing whitespace and no longer than 256 bytes. The
simulator carries it in a private SIP header that the handler honors only for
loopback requests. Normal network devices continue to publish to
`{stream_prefix}/{channel_id}`.

Both endpoints require the normal `sip:read` or `gb28181:read` permission. A
viewer may run them; a disabled or uninitialized module returns 503 and the page
must show the test as unavailable rather than a successful report.

## Verification

Run focused tests without external services:

```bash
go test ./module/api ./module/sipgateway ./module/gb28181
go test -race ./module/gb28181 -run 'Lab|SelfTest' -v
```

The self-tests bind only ephemeral localhost UDP sockets and release their port
pairs before returning. They do not write recordings or configuration.
