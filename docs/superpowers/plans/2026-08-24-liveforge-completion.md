# LiveForge Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close every unfinished or unclosed LiveForge feature except Simulcast, with production behavior, management APIs, tests, documentation, and a repeatable final verification path.

**Architecture:** Keep the existing module boundaries. Runtime configuration publishes immutable snapshots; modules implement `core.Reloadable` for policy changes and explicitly report listener/module/TLS/port changes as restart-required. Management APIs use typed providers registered by modules, while a shared RBAC/audit middleware protects dangerous operations. Console pages consume those APIs without adding a second backend.

**Tech Stack:** Go 1.26+, `net/http`, Prometheus client, existing SIP/cluster/record/DVR implementations, embedded HTML console, YAML/OpenAPI/JSON Schema documentation.

**Spec:** `docs/superpowers/specs/2026-08-24-config-loader-design.md` plus the completion requirements recorded in `docs/PROGRESS.md`.

## Global Constraints

- Simulcast layer selection remains deferred and must stay explicitly documented as unavailable.
- Runtime reads must remain atomic/non-blocking; refresh work runs only in the background.
- Every behavior/API/config/security change updates the relevant AI-facing docs in the same final change.
- Every new behavior gets a failing focused test first, then minimal implementation, then race-safe verification.
- No secrets, recordings, generated binaries, or `latest` release claims may be committed.
- Go 1.26+ is required; run `go test ./...`, tagged race tests when FFmpeg is available, and both agent-doc checks.

---

### Task 1: Reloadable module policy application

**Files:**
- Modify: `core/server.go`, `core/stream.go`, and the affected modules under `module/{auth,api,notify,record,dvr,cluster,httpstream,hls,webrtc}`.
- Test: each affected package's existing test file plus focused `reload_test.go` files.

**Interfaces:** Modules implement `OnReload(*core.Server) error`; listener/module/TLS/port changes are left in `Server.PendingRestartChanges()`. Long-lived policy state is stored behind atomics or short mutexes and is read without network/file I/O.

- [ ] Add tests proving auth/API token, notify retry, record/DVR policy, cluster retry/health, stream limits/feedback, HLS/DASH/LL-HLS, and WebRTC GCC policy changes are applied to new and existing sessions where supported.
- [ ] Add tests proving listener addresses, enabled modules, TLS files/mode, RTP/RTSP/SIP port ranges, and audio codec enablement remain restart-required.
- [ ] Implement `OnReload` for the modules, preserving active connections when only policy values change and rejecting invalid snapshots without mutating active state.
- [ ] Run focused package tests and `go test -race` for all touched modules.

### Task 2: Cluster relay metrics and status

**Files:**
- Modify: `module/cluster/module.go`, `module/cluster/forward.go`, `module/cluster/origin.go`, `module/cluster/transport_*.go`, `module/cluster/relay_metrics.go`, `module/metrics/module.go`.
- Create: `module/cluster/status.go` and focused status/metrics tests.

**Interfaces:** `RelayMetrics` is injected into managers/transports; `ClusterStatusProvider` exposes bounded relay state for `GET /api/v1/cluster/status`.

- [ ] Write failing tests for active relay counts, bytes, errors, latency, packet-loss updates, registry gathering, and bounded labels.
- [ ] Inject one custom Prometheus registry collector through the metrics module; do not register unbounded stream/target labels.
- [ ] Record lifecycle, byte, error, latency, and RTP loss observations in actual forward/origin transport paths.
- [ ] Add status API data for active forward/origin relays and peer health, with tests for empty and active states.

### Task 3: SIP Gateway control plane

**Files:**
- Modify: `module/sipgateway/gateway.go`, `module/sipgateway/call_session.go`, `module/sipgateway/module.go`, `module/api/routes.go`, `module/api/handler.go`.
- Create: `module/sipgateway/api.go`, `module/sipgateway/status.go`, and focused API/error tests.

**Interfaces:** `SIPGatewayProvider` exposes list/detail/dial/hangup operations. Call details include direction, stream, codec, RTP/RTCP ports, remote address, start time, and state.

- [ ] Add failing tests for inbound/outbound call listing, detail lookup, dial, hangup, duplicate Call-ID, abnormal BYE, network loss, codec mismatch, and port exhaustion.
- [ ] Implement a concurrency-safe call snapshot and idempotent cleanup, including remote disconnect detection and port release exactly once.
- [ ] Register `GET /api/v1/sipgateway/calls`, `GET /api/v1/sipgateway/calls/{call_id}`, `POST /api/v1/sipgateway/calls`, and `DELETE /api/v1/sipgateway/calls/{call_id}`.
- [ ] Add gateway metrics for active calls, setup failures, codec failures, and RTP packet/byte counters.

### Task 4: Recording and DVR management/reliability

**Files:**
- Modify: `module/record/module.go`, `module/record/session.go`, `module/record/file_writer.go`, `module/dvr/module.go`, `module/dvr/handler.go`, `module/dvr/cleanup.go`, `module/api/routes.go`, `module/api/handler.go`.
- Create: `module/record/storage.go`, `module/record/status.go`, `module/dvr/status.go`, and focused tests.

**Interfaces:** `RecordingProvider` lists, describes, downloads, and deletes completed recordings; `DVRStatusProvider` adds per-session and storage health fields. Local storage is implemented behind an interface so object storage can be added later without changing APIs.

- [ ] Add failing tests for recording index/list/detail/download/delete, traversal protection, write failure, retry/recovery, cleanup, disk pressure, and DVR segment lifecycle.
- [ ] Implement a local storage backend, atomic file finalization, bounded retry with session status, and cleanup metrics; preserve partial files as failed records rather than silently losing them.
- [ ] Add authenticated management routes for recordings and a unified DVR status/detail route while retaining media playlist/segment routes.
- [ ] Implement `OnReload` for record/DVR policy values; active sessions finish safely and new sessions use the new snapshot.

### Task 5: RBAC and audit security

**Files:**
- Modify: `config/config.go`, `config/loader.go`, `module/api/module.go`, `module/api/login.go`, `module/api/handler.go`, `module/api/routes.go`.
- Create: `module/api/rbac.go`, `module/api/audit.go`, focused security tests, and a security recipe.

**Interfaces:** Requests resolve an authenticated principal with roles (`viewer`, `operator`, `admin`); route metadata requires permissions such as `streams:read`, `streams:delete`, `streams:kick`, `sip:calls`, `recordings:delete`, and `config:reload`. Audit entries contain request ID, principal, action, resource, result, and redacted metadata.

- [ ] Write failing tests for role matrices, bearer/session authentication, denied operations, rate limits, secret redaction, and audit records for every destructive operation.
- [ ] Add configurable role bindings while retaining backward-compatible single bearer token behavior as an admin principal.
- [ ] Protect dangerous routes (stream delete/kick, SIP hangup, recording delete, config refresh) with explicit permissions and emit structured audit events for success and failure.
- [ ] Add authentication-failure, rate-limit, audit, and config-change metrics; never log token/password/header values.

### Task 6: Management console closure

**Files:**
- Modify: `module/api/console.html`, `module/api/console.go`, and API client scripts/styles as needed.
- Test: `module/api/console_publish_test.go` plus endpoint contract tests.

- [ ] Add views for runtime config health/pending restart, cluster relay status, SIP calls, recordings/DVR, and audit/security state.
- [ ] Add operator controls for SIP dial/hangup, recording deletion, and config refresh with permission-aware disabled/error states.
- [ ] Keep the console usable without external build tooling; verify all API calls against the OpenAPI paths and ensure secrets are never rendered.

### Task 7: Release, migration, and operations closure

**Files:**
- Modify: `agent-manifest.json`, `llms.txt`, `llms-full.txt`, `README.md`, `README.zh-CN.md`, `docs/api/openapi.yaml`, `docs/config/config.schema.json`, `docs/PROGRESS.md`.
- Create/update: `docs/recipes/recording-dvr-management.md`, `docs/recipes/sipgateway-management.md`, `docs/recipes/cluster-relay-operations.md`, `docs/recipes/rbac-audit.md`, `docs/recipes/release-verification.md`.

- [ ] Document every new endpoint, permission, config field, metric, failure mode, migration rule, and restart-required field.
- [ ] Add a production deployment checklist, rollback/config migration procedure, and troubleshooting commands.
- [ ] Verify release workflow action majors remain Node 24-compatible and describe only versioned assets that the workflow can publish.
- [ ] Remove stale “planned” claims for completed features while retaining the explicit Simulcast deferral and optional FFmpeg limitations.

### Task 8: Final loop verification and acceptance

- [ ] Run `gofmt` and `git diff --check`.
- [ ] Run focused tests for every changed package, then `go test ./...` and `go test -race ./...`.
- [ ] When FFmpeg is available, run the tagged build and tagged race suite.
- [ ] Run `tools/check-agent-docs_test.sh` and `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`.
- [ ] Start the server with the sample config, exercise health/config/cluster/SIP/record/DVR endpoints, and verify auth/audit behavior with a smoke script.
- [ ] Re-read this plan line by line, mark only evidenced items complete, and do not deliver while any non-Simulcast item lacks source, test, docs, or verification evidence.
