# LiveForge Project Progress

> Source-aligned project status. Update this file only after implementation and a passing verification path exist.
>
> Last updated: 2026-08-25

## Current Status

LiveForge is a Go 1.26+ modular streaming server with multi-protocol ingest/playback, protocol bridging, management operations, optional FFmpeg audio transcoding, runtime configuration refresh, and multi-node relay.

All previously identified incomplete or unclosed runtime features are implemented and documented except Simulcast layer selection. `stream.simulcast` remains configuration-only, restart-required, explicitly deferred, and unsupported by the WebRTC runtime.

Release artifacts remain conditional: source builds are available from the repository; versioned binaries and GHCR images exist only after a `v*` tag completes the Release workflow. Portable release binaries use `CGO_ENABLED=0` and do not provide audio transcoding. Tagged source builds and the Dockerfile use `audiocodec` plus FFmpeg.

## Implemented Capabilities

### Core And Protocols

- Shared stream hub, lifecycle events, GOP/ring buffers, statistics, resource limits, graceful drain, rollback-capable module reload, and slow-consumer handling.
- RTMP ingest/playback and FLV bridging.
- RTSP ingest/playback over TCP interleaving and UDP, including separate audio/video track SETUP compatibility.
- Pure-Go SRT ingest/playback with MPEG-TS and optional encryption.
- WebRTC WHIP/WHEP publish/play, ICE trickle, session DELETE/PATCH, CORS preflight, ICE Lite, GCC, and browser console integration.
- HLS, LL-HLS, DASH, HTTP-FLV, HTTP-TS, FMP4, and WebSocket playback.
- GB28181 SIP registration/keepalive/catalog, live view start/stop, playback start/stop, PTZ, alarm handling, session/device management, and simulator coverage.
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

### Management, Security, And Console

- Public `GET /api/v1/server/health`; protected stream, server, config, cluster, SIP Gateway, recording/DVR, security, audit, GB28181, and debug operations.
- Named bearer tokens with viewer/operator/admin RBAC, legacy admin bearer compatibility, and role-bearing console sessions.
- The deprecated `auth.api.bearer_token` migrates only when `api.auth.bearer_token` is empty; the current path wins when both exist.
- Bounded in-memory audit plus structured logs for authentication failures, authorization denials, console login failures, rate-limited mutations, mutation outcomes, and accepted config application.
- Audit metadata removes keys containing token, secret, password, or authorization.
- Permission-aware seven-view console: streams, config, cluster, SIP, storage, security, and audit. Actions are enabled only for the active role.
- Redacted runtime config, security, cluster relay/peer, call, recording, storage, DVR, and audit status.

### Recording And DVR

- FLV, fragmented MP4, MP4, MPEG-TS, and HLS recording.
- Stream pattern selection, duration/size segmentation, path templates, completion callbacks, retry/failure preservation, and storage health.
- Authenticated recording list/status/detail, HTTP range download, and admin delete operations.
- DVR playlist/segment serving, retention cleanup, storage/session status, and Prometheus metrics.
- DVR media registration is valid on Go 1.26 and strictly dispatches only `GET /dvr/{app}/{key}.m3u8` and `GET /dvr/{app}/{key}/{filename}`; malformed or nested resources return 404 before playback authorization or storage lookup.

### SIP Gateway

- Inbound and outbound calls, codec negotiation, RTP/RTCP port management, bounded concurrency, call status, dial/detail/hangup API, console operations, and Prometheus metrics.
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
| GB28181 live/playback stop route and `gb28181:control` permission | Registered and covered in `module/gb28181` and `module/api` tests |
| Runtime callback coalescing counter | `DroppedCallbacks` status/metrics path and manager tests |
| Cluster credential hot rotation/no-admin failure | RTP/GB transport credential tests and cluster operations recipe |

## Operations Documentation

- API: `docs/api/openapi.yaml`
- Configuration and reload annotations: `docs/config/config.schema.json`
- Runtime sources: `docs/recipes/runtime-config-sources.md`
- Authentication/TLS and bearer migration: `docs/recipes/auth-and-tls.md`
- RBAC/audit: `docs/recipes/rbac-audit.md`
- Recording/DVR: `docs/recipes/recording-dvr-management.md`
- SIP Gateway: `docs/recipes/sipgateway-management.md`
- Cluster relay: `docs/recipes/cluster-relay-operations.md`
- Release artifact verification: `docs/recipes/release-verification.md`

Every operations recipe uses loopback-safe examples, authenticated requests, expected success/failure codes, diagnostics/metrics, rollback, and recovery. Each warns that `configs/liveforge.yaml` disables TLS/auth and uses `admin/admin`, so it must never be publicly exposed unchanged.

This DVR fix restores the already documented media URL contract and does not change capabilities, prerequisites, ports, configuration, authentication, REST contracts, or operator workflows. Therefore `agent-manifest.json`, `llms.txt`, `llms-full.txt`, `README.md`, `README.zh-CN.md`, `docs/api/openapi.yaml`, `docs/config/config.schema.json`, and the recording/DVR recipe require no corresponding content change.

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
