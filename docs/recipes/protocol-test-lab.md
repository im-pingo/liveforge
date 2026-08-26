# SIP And GB28181 Protocol Test Lab

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

The Console includes local one-shot protocol self-tests so SIP and GB28181
workflows can be validated without a PBX, cloud platform, or camera. The SIP
gateway provider also exposes a transport-backed persistent fake-device lab for
PCMA/PCMU publish and receive workflows. Enable SIP, its `gateway` block, and
GB28181, keep the API listener on loopback, and use a viewer or operator token.
The checked-in sample includes a loopback-safe gateway port range so this page is
usable immediately in local development:

```bash
export LIVEFORGE_API=http://127.0.0.1:8090
export VIEWER_TOKEN='replace-me'
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
release their sockets. A standalone `NewLabManager` is contract-only: its valid
sessions have `state=contract`, never `state=active`, and have no transport or
media. This provider workflow requires an initialized SIP transport and enabled
gateway.

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

Persistent GB28181 fake-device lab sessions are explicitly unavailable. The
supported local GB28181 workflow is the one-shot self-test above; do not treat
the GB28181 provider contract as a persistent transport implementation.

Both endpoints require the normal `sip:read` or `gb28181:read` permission. A
viewer may run them; a disabled or uninitialized module returns 503 and the page
must show the test as unavailable rather than a successful report.

## Verification

Run focused tests without external services:

```bash
go test ./module/api ./module/sipgateway ./module/gb28181
```

The self-tests bind only ephemeral localhost UDP sockets and release their port
pairs before returning. They do not write recordings or configuration.
