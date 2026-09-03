# LiveForge Project Progress

> Source-aligned project status. Update this file only after implementation and a passing verification path exist.
>
> Last updated: 2026-09-03

## Current Status

LiveForge is a Go 1.26+ modular streaming server with multi-protocol ingest/playback, protocol bridging, management operations, optional FFmpeg audio transcoding, runtime configuration refresh, and multi-node relay.

Previously identified incomplete or unclosed runtime features are implemented and documented except Simulcast layer selection. `stream.simulcast` remains configuration-only, restart-required, explicitly deferred, and unsupported by the WebRTC runtime. SIP and GB28181 persistent fake-device publish/receive signaling and RTP/RTCP loopback are implemented and verified at their providers. Record now matches ordinary GB28181 prefixed stream keys, resolves user-home recording paths, waits for zero-codec generations to expose startup media headers, and preserves the legacy declared-codec startup path. The Console WHEP H.264 regression is fixed and browser-verified; performance and codec risks now have bounded regression evidence and explicit deployment limits, leaving Simulcast as the only deferred runtime feature.

## Review Items

- **WEBRTC-001 (P0)**: Closed. The default Console and protocol-lab WHEP path uses atomic `mode=live` GOP startup; explicit realtime mode retains its waiting-keyframe semantics, while feed status and bounded diagnostics distinguish waiting, codec mismatch, write failure, generation end, and media stall.
- **WEBRTC-002 (P1)**: Closed for the supported matrix. Chromium coverage verifies real H.264/VP8 playback when the browser advertises H.264, SIP/GB28181/WHIP cross-protocol paths, decoded dimensions, advancing media time, RTP/RTCP counters, and non-stalled server status; browsers without H.264 receive an explicit environment skip, while Pion negotiation coverage remains required. The 60-second soak and bounded egress/fanout benchmarks are repeatable regression gates; their host/network limits are documented and are not deployment capacity promises.
- Performance, lifecycle, resource, security, and functional-boundary findings are recorded with source locations in [docs/TECHNICAL-RISKS.md](TECHNICAL-RISKS.md). They are not silently treated as completed work.

Release artifacts remain conditional: source builds are available from the repository; versioned binaries and GHCR images exist only after a `v*` tag completes the Release workflow. Portable release binaries use `CGO_ENABLED=0` and do not provide audio transcoding. Tagged source builds and the Dockerfile use `audiocodec` plus FFmpeg.

## Implemented Capabilities

### Core And Protocols

- Shared stream hub, lifecycle events, GOP/ring buffers, statistics, resource limits, graceful drain, rollback-capable module reload, startup rollback limited to attempted modules, and slow-consumer handling.
- RTMP ingest/playback and FLV bridging.
- RTSP ingest/playback over TCP interleaving and UDP, including separate audio/video track SETUP compatibility with unique, in-range, session-eligible track validation before transport allocation.
- Pure-Go SRT ingest/playback with MPEG-TS and optional encryption.
- WebRTC WHIP/WHEP publish/play with a 1 MiB SDP offer limit and HTTP 413 rejection, ICE trickle, session DELETE/PATCH, CORS preflight, ICE Lite, GCC, and browser console integration.
- Browser-verified WHIP H.265 + Opus bridging to HTTP-FLV, WS-FLV, HTTP-TS, FMP4, HLS, DASH, WHEP realtime, and WHEP Live; WHEP uses codec-specific HEVC parameter-set conversion, an atomic GOP/source-ring cursor transition, and an independent target-audio transcode reader.
- HLS, LL-HLS, DASH, HTTP-FLV, HTTP-TS, FMP4, and WebSocket playback; HLS/LL-HLS/DASH segmenters wait for required sequence headers and bind headers, GOP replay, and live cursor from one publisher-generation snapshot. LL-HLS initial manifests avoid duplicate completed PART delivery, while blocking reloads retain the latest completed PART identity across segment transitions. HLS/DASH manifest and media paths escape every stream-key segment, DASH URL attributes are XML-safe, and routing preserves arbitrarily deep valid keys.
- GB28181 SIP registration/keepalive/catalog, live view start/stop, playback start/stop, PTZ, alarm handling, session/device management, fast in-process self-test coverage, and persistent loopback fake-device publish/receive labs with real signaling/media and cleanup verification.
- SIP TCP/UDP listener cancellation is treated as normal shutdown without ERROR noise; unexpected listener failures while the service context is active remain ERROR with transport metadata.
- Optional audio transcoding for AAC, Opus, G.711, and MP3 when built with `CGO_ENABLED=1 -tags audiocodec` and FFmpeg libraries.

### Runtime Configuration

- Bootstrap YAML defaults, normalization, validation, and environment expansion.
- Selectable `file`, HTTP/HTTPS, Consul, and Redis sources.
- Immediate load plus periodic background polling with bounded load timeout.
- Atomic immutable snapshot and typed-key reads without file/network I/O, refresh waits, or status-lock contention.
- Coalesced asynchronous manual refresh from `SIGHUP` and `POST /api/v1/server/config/refresh`.
- Last-valid snapshot retention on source, parse, validation, immutable-change, or module-application failure.
- Exact hot/restart/immutable path classification; restart-required desired state is retained in `pending_restart` while effective values stay unchanged.
- Prepare/apply/publication ordering with reloader rollback on later application rejection.
- Status and Prometheus counters for accepted, rejected, application-failed, callback-failed, superseded callback, consecutive failure, and pending restart state.
- Callback coalescing retains the latest transition and increments `DroppedCallbacks` for superseded pending notifications.
- Source loads, Config Apply writes, and close are serialized by a cancellable source-I/O gate; Apply returns 202 only after the source write succeeds, while parse/application/publication remain asynchronous.
- A successful ConfigWriter write publishes a pending desired overlay immediately; stale asynchronous source reads cannot overwrite it, and the overlay is cleared only after matching content is successfully applied.

### Management, Security, And Console

- Public `GET /api/v1/server/health`; protected stream, server, config, cluster, SIP Gateway, recording/DVR, security, audit, GB28181, and debug operations.
- Named bearer tokens with viewer/operator/admin RBAC, legacy admin bearer compatibility, and role-bearing console sessions.
- The deprecated `auth.api.bearer_token` migrates only when `api.auth.bearer_token` is empty; the current path wins when both exist.
- Bounded in-memory audit plus structured logs for authentication failures, authorization denials, console login failures, rate-limited mutations, mutation outcomes, and accepted config application.
- Audit metadata removes keys containing token, secret, password, or authorization.
- Permission-aware console tabs, in order: Streams, GB28181, Config, Cluster, SIP Calls, Storage, and Security. Recent Audit is a surface inside Security, not a separate tab. Visual groups are Workspace (Streams, GB28181, SIP Calls, Storage), Operations (Cluster), and System (Config, Security); Config/Security are not peer video-stream tabs. Actions are enabled only for the active role.
- Config exposes the complete redacted effective/desired YAML document, retains raw source comments/unmapped fields, embeds and displays the complete versioned JSON Schema, and supports source details, writable state, Validate, and Apply & Refresh. `config:read` is available to viewers; `config:reload` is limited to operators/admins. File, HTTP/HTTPS, Consul, and Redis source writers are covered by source-specific tests; read-only sources return 409.
- SIP and GB28181 console pages expose fast local self-tests at `GET /api/v1/sipgateway/test` and `GET /api/v1/gb28181/test`, with no remote platform dependency. SIP fake-peer checks REGISTER/401/digest, dual-track H.264 plus PCMA/PCMU INVITE/200/ACK/BYE, rejection/timeout, RTP, and RTCP; GB28181 fake-device checks REGISTER, Keepalive, Catalog, PS INVITE/SDP/ACK/BYE, rejection/timeout, H.264 plus 8 kHz mono G.711A PS/RTP, and RTCP. Both providers also expose persistent publish/receive fake-device sessions with loopback signaling/media, aggregate and per-track counters, duplicate-identity rejection, idempotent stop, and socket/dialog/goroutine cleanup. The shared dependency-free sample is a moving 160x90 constrained-baseline pattern at 25 fps with one IDR per second and audible 20 ms audio frames. Persistent failure snapshots expose bounded redacted `last_error`; Streams reports keyframe-driven GOP generation with interleaved video/audio counts and duration, while audio-only streams show startup GOP as not applicable. Availability is claimed only after focused lab, RTP sequence-header, browser Console, and race verification pass.
- TLS API listeners set `Secure` on the HttpOnly, SameSite=Strict `lf_session` cookie; plain HTTP development listeners leave it unset.
- Redacted runtime config, security, cluster relay/peer, call, recording, storage, DVR, and audit status.

### Recording And DVR

- FLV, fragmented MP4, MP4, MPEG-TS, and HLS recording. New recordings default to fMP4 with `.mp4` extension, and Storage reports `state=disabled` with HTTP 200 when the record module is absent.
- fMP4 Record and DVR MPEG-TS normalize G.711 and other target-incompatible audio to AAC through the shared optional FFmpeg path; portable builds filter that audio and retain playable video-only output.
- Stream pattern selection, duration/size segmentation, path templates, completion callbacks, retry/failure preservation, and storage health.
- Authenticated recording list/status/detail, HTTP range download, inline browser playback, and admin delete operations.
- Storage Console actions preview completed recordings and DVR sessions with available segments; recording media uses the management session while DVR media remains on its separate listener without browser bearer-token persistence.
- DVR playlist/segment serving with synchronous-only subscribe authorization and no asynchronous subscribe lifecycle emission, retention cleanup, storage/session status, and Prometheus metrics.
- DVR media registration is valid on Go 1.26 and dispatches `GET /dvr/{app}/{key}.m3u8` and `GET /dvr/{app}/{key}/{filename}`, including nested stream keys. Encoded path separators, dot segments, and extra resource levels return 404 before playback authorization or storage lookup.

### SIP Gateway

- Inbound and outbound calls, codec negotiation, RTP/RTCP port management, bounded concurrency, call status, dial/detail/hangup API, console operations, local self-test, and Prometheus metrics. Inbound INVITEs run synchronous publish authorization before RTP allocation, then emit generation-matched publish start/stop lifecycle events so Record and DVR sessions follow SIP calls and finalize on hangup.
- Stable HTTP mappings for invalid input, missing streams/calls, codec mismatch, capacity/port exhaustion, setup failure, and unavailable module states.

### Cluster Relay

- Forward push and on-demand origin pull over RTMP, SRT, RTSP, RTP, and GB28181.
- Static or HTTP-scheduled targets, retry policy, idle cleanup, bounded relay pool, health checks, peer eviction/recovery, status API, and finite-cardinality metrics.
- RTP/GB signaling paths are configurable and protected by management RBAC.
- Node signaling resolves credentials from the current atomic config on every request: `api.auth.bearer_token` first, otherwise the first named admin token.
- Credential rotation is hot. Configured auth without a usable admin token fails locally before contacting a peer.
- Peer response bodies and returned/logged errors are bounded and redacted.

### Tooling And Release

- `lf-test`, GB28181 simulator, and shared testkit packages have meaningful package tests and CI-compatible output/exit behavior.
- No-native-dependency quick check: `go test ./...`.
- Tagged baseline requires Go 1.26 and FFmpeg development libraries:

```bash
CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge
CGO_ENABLED=1 go test -tags audiocodec -race \
  -coverprofile=coverage.out -covermode=atomic ./...
```

- GitHub Actions uses Node 24-compatible action majors: checkout/setup-go/upload-artifact v7, Docker setup-buildx/login v4, build-push v7, golangci-lint v9, and action-gh-release v3.
- Release binaries cover linux/darwin on amd64/arm64 with the portable no-CGO profile. The tagged Docker build contains the FFmpeg audio profile.

## Completion Evidence

| Closure | Evidence |
| --- | --- |
| Permission-aware seven-view console and accessible operations | `5929f99` |
| RTSP separate audio/video multi-track SETUP | `08911ea` |
| Meaningful CLI/tool package tests | `7df71a0` |
| Cluster peer error bounding/redaction and final security hardening | `ef20a1a` |
| Go 1.26 DVR media route registration, strict pre-auth dispatch, and bounded lifecycle | Real-listener tests in `module/dvr/route_test.go` |
| SIP listener shutdown logging and active-failure classification | Real-listener log tests in `module/sip/listener_shutdown_test.go` |
| GB28181 live/playback stop route and `gb28181:control` permission | Registered and covered in `module/gb28181` and `module/api` tests |
| Runtime callback coalescing counter | `DroppedCallbacks` status/metrics path and manager tests |
| Cluster credential hot rotation/no-admin failure | RTP/GB transport credential tests and cluster operations recipe |
| WHIP H.265 + Opus eight-protocol browser playback | Codec-specific Annex-B tests, atomic WHEP Live snapshot test, and `docs/recipes/whip-h265-opus-playback.md` |
| Storage recording availability and unified fMP4 playback | `module/record/record_test.go`, `module/api/recording_test.go`, and `RecordingStatusResponse` contract |
| Config document/schema/validate/apply and five runtime sources | `module/api/config_api_test.go`, `config/runtime/source_test.go`, and `docs/recipes/runtime-config-sources.md` |
| CONFIG-004 through CONFIG-009 runtime/API hardening | `config/runtime/source_test.go`, `config/runtime/manager_test.go`, `module/api/console_management_test.go`, `module/api/openapi_contract_test.go`, and the synchronized schema/recipe/OpenAPI docs |
| SIP/GB28181 fast self-tests and persistent provider labs | `module/api/config_api_test.go`, `module/api/protocol_testlab_api_test.go`, `module/sipgateway/lab_test.go`, `module/gb28181/lab_test.go`, and `docs/recipes/protocol-test-lab.md` |
| ARCH-033 unified HTTP header and idle timeouts | `module/api`, `module/webrtc`, and `module/metrics` `TestHTTPServerTimeouts`; `docs/recipes/auth-and-tls.md` |

## Operations Documentation

- API: `docs/api/openapi.yaml`
- Configuration and reload annotations: `docs/config/config.schema.json`
- Runtime sources: `docs/recipes/runtime-config-sources.md`
- WHIP H.265 + Opus playback: `docs/recipes/whip-h265-opus-playback.md`
- Authentication/TLS and bearer migration: `docs/recipes/auth-and-tls.md`
- RBAC/audit: `docs/recipes/rbac-audit.md`
- Recording/DVR: `docs/recipes/recording-dvr-management.md`
- SIP Gateway: `docs/recipes/sipgateway-management.md`
- Cluster relay: `docs/recipes/cluster-relay-operations.md`
- Release artifact verification: `docs/recipes/release-verification.md`

Every operations recipe uses loopback-safe examples, authenticated requests, expected success/failure codes, diagnostics/metrics, rollback, and recovery. Each warns that `configs/liveforge.yaml` disables TLS/auth and uses `admin/admin`, so it must never be publicly exposed unchanged.

The final review synchronization records accepted-only HTTP validators, strict HTTP/HTTPS scheme matching with redirects disabled, serialized writable/read-only Config source semantics, raw complete redacted Config documents plus embedded schema, pre-allocation RTSP SETUP validation, synchronous-only DVR authorization, TLS-bound secure console cookies, the 1 MiB WHIP/WHEP offer limit, local SIP/GB28181 signaling/media labs, unified fMP4 recording defaults, and the canonical seven-tab console contract across the manifest, Agent summaries, user READMEs, schema, OpenAPI, and operations recipes.

The 2026-09-03 closure loop passed `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`, portable and `audiocodec` builds, embedded schema/doc checks, and the tagged race/coverage suite. The 60-second Chromium protocol matrix passed all three SIP/GB28181/WHIP scenarios; bounded production, RTP, metrics, and transcode benchmarks also passed. Only WebRTC Simulcast remains deferred.

## Deferred Work

| Feature | Configuration | Status |
| --- | --- | --- |
| Simulcast layer selection and automatic layer pausing | `stream.simulcast.*` | Deferred and unsupported; all fields are restart-required until a runtime implementation exists |

AI media analysis, semantic search, and other AI adaptations are roadmap opportunities, not incomplete claims against the current streaming runtime.

## Verification Contract

Run focused tests for changed modules, then:

```bash
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
go test ./...
CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge
CGO_ENABLED=1 go test -tags audiocodec -race \
  -coverprofile=coverage.out -covermode=atomic ./...
```

Do not claim Docker images, release assets, optional audio transcoding, or deferred Simulcast behavior without the corresponding artifact/build/runtime verification.
