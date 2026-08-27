# Protocol Lab Simulator Design

## Goal

Turn the SIP and GB28181 Console pages from one-shot self-test surfaces into
usable local protocol labs. An operator can start and stop an in-process fake
device, send media through the real LiveForge SIP/RTP/RTCP or GB28181 SIP/PS
transport, observe the resulting stream and session counters, and open that
stream through the existing cross-protocol players.

## Current Gap

The existing `RunSelfTest` methods validate parsers, codec selection, and
temporary localhost UDP sockets. They do not register a device with the
running SIP/GB28181 service, create a persistent `core.Stream`, or maintain a
device/session lifecycle. The Console can display real devices and calls, but
cannot create one locally.

## Scope

- SIP lab publish mode: a fake SIP endpoint registers, sends INVITE/SDP, and
  sends deterministic RTP audio plus RTCP to the SIP gateway. The gateway
  creates the normal inbound SIP stream.
- SIP lab receive mode: a fake SIP endpoint accepts the gateway's outbound
  INVITE and receives RTP/RTCP from an existing LiveForge audio stream.
- GB28181 lab publish mode: a fake device registers, announces one channel,
  accepts the server's live-play INVITE, and sends deterministic H.264 in PS
  over RTP to the server.
- GB28181 lab receive mode: a fake device registers and accepts a server play
  or playback INVITE, then receives the server's PS/RTP media and control
  traffic.
- Console controls for start, stop, refresh, direction, stream key, device or
  channel identity, and deterministic media profile.
- Session snapshots expose state, stream key, device/call identity, media
  ports, and packet/byte counters. Published stream keys are usable by the
  existing HTTP-FLV, TS, fMP4, HLS, DASH, and WHEP players.
- Existing fast self-tests remain available as health checks.

## Non-Goals

- Emulating a complete telephone user experience or camera firmware.
- Full SIP softphone/camera emulation beyond the deterministic H.264 plus
  PCMA/PCMU dual-track test profile.
- Requiring FFmpeg, Docker, an external PBX, or an external GB28181 platform.
- Persisting lab sessions across process restarts.

## Architecture

Each protocol module owns a bounded `LabManager` with a mutex-protected map of
active sessions. A session owns its fake endpoint, SIP dialog, UDP media
connections, cancellation context, and atomic counters. Start validates the
request, creates the fake endpoint, performs the real signaling handshake, and
only publishes the session after the media path is ready. Stop cancels the
session, sends the protocol teardown where applicable, removes temporary
publishers, closes sockets, and releases ports. Repeated stop calls are
idempotent.

The API layer exposes protocol-specific lab routes and maps module errors to
stable HTTP responses. It does not own protocol state. Console code refreshes
lab snapshots along with existing SIP calls and GB28181 devices/sessions, and
uses the same stream preview URL construction as the Streams page.

Deterministic media is generated without native dependencies from one shared
moving 160x90 constrained-baseline H.264 profile at 25 fps with one IDR per
second and audible 20 ms G.711 frames. SIP sends H.264 and configured PCMA or
PCMU on separate RTP tracks. GB28181 muxes H.264 and G.711A through the existing
PS muxer and packetizes the result as RTP payload type 96.

## API Contract

SIP:

- `GET /api/v1/sipgateway/lab/sessions`
- `POST /api/v1/sipgateway/lab/sessions` with `{mode, device_id, stream_key, codec}`
- `DELETE /api/v1/sipgateway/lab/sessions/{id}`

GB28181:

- `GET /api/v1/gb28181/lab/sessions`
- `POST /api/v1/gb28181/lab/sessions` with `{mode, device_id, channel_id, stream_key}`
- `DELETE /api/v1/gb28181/lab/sessions/{id}`

`mode` is `publish` for device-to-server media and `receive` for
server-to-device media. List responses include active session snapshots and
the resolved stream key. Start/stop require operator permission; list requires
the existing protocol read permission.

## Error Handling And Security

Invalid identities, unsupported codecs, duplicate active identities, closed
modules, exhausted RTP ports, signaling timeouts, and missing source streams
return explicit errors. The API never returns SIP passwords or bearer tokens.
Lab sessions are local-only and bind fake media endpoints to loopback. The
Console uses the existing permission and session-authentication middleware.

## Verification

- Unit tests cover request validation, session lifecycle idempotence, media
  counters, teardown, and deterministic media generation.
- Integration tests start a real module/server path and verify SIP/GB28181
  signaling plus RTP/RTCP or PS media reaches the intended LiveForge stream or
  fake endpoint.
- API tests verify RBAC, routes, cross-protocol stream metadata, and stop
  cleanup.
- Console source tests verify controls, status rendering, and player actions.
- Repository tests, race tests, tagged audio tests, builds, OpenAPI/doc
  checks, and the existing stress matrix remain required.
