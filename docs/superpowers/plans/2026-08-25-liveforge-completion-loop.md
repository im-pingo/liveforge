# LiveForge Completion Loop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish and verify every open LiveForge productization item except Simulcast, including the remaining protocol lifecycle defects, management console, synchronized documentation, and release-level acceptance.

**Architecture:** Preserve the existing module boundaries and build on commit `43249e8` plus the current uncommitted Task 4 reliability changes. Lifecycle delivery is monotonic per stream and publisher/subscriber generation, session-local state prevents unmatched WebRTC/GB28181 events, and shutdown owners close admission before resources. The console remains one embedded HTML asset backed only by authenticated, documented management APIs.

**Tech Stack:** Go 1.26, `sync.Mutex`, `sync/atomic`, the existing EventBus/SIP/RTSP/GB28181/WebRTC modules, `net/http`, embedded HTML/CSS/JavaScript, OpenAPI 3, JSON Schema, shell smoke tests, Go race detector, and FFmpeg-tagged CGO verification.

**Spec:** `docs/superpowers/plans/2026-08-24-liveforge-completion.md`, `docs/superpowers/specs/2026-08-24-config-loader-design.md`, and `.superpowers/sdd/2026-08-24-liveforge-completion/rereview-task-4-round-3.md`.

## Global Constraints

- Simulcast layer selection is the only deferred feature and remains explicitly unavailable.
- Runtime configuration reads are atomic in-memory loads; source I/O and parsing remain background-only.
- Existing user changes and the current dirty Task 4 work are preserved; no reset, checkout, or unrelated rewrite is allowed.
- Listener/module/TLS/port/audio-codec topology changes remain restart-required.
- Every behavior change begins with a focused failing test and records its RED and GREEN commands.
- Every source change includes a documentation-impact decision under the `AGENTS.md` synchronization matrix.
- A task is complete only after focused race tests, package tests, `go vet`, cross-platform compile checks where applicable, and `git diff --check` pass.
- Final delivery requires the untagged suite, FFmpeg-tagged build/race suite, both Agent documentation checks, authenticated runtime smoke tests, and a line-by-line acceptance audit.
- A failed check returns execution to the owning task; it is never waived or converted into a completion claim.

## Evidence Ledger

| Scope | Existing implementation evidence | Final status at plan start |
| --- | --- | --- |
| Runtime reload and restart classification | Commits through `764abd4`; Task 1 review reports no source/test findings | Awaiting unified re-verification and docs |
| Cluster relay metrics/status | Commits `b0022f2`, `1521d24`; review reports no source/test findings | Awaiting unified re-verification and docs |
| SIP Gateway control plane | Commits `13a3886..7f69c59`; five review rounds | Awaiting terminal-metric ordering fix, unified re-verification, and docs |
| Recording/DVR management | Commits `aa2b0bd..8079092`; storage/shutdown hardening present | Awaiting Task 4 lifecycle closure, unified re-verification, and docs |
| RBAC/audit | Commits through `764abd4`; review reports no source/test findings | Awaiting unified re-verification, console integration, and docs |
| Protocol lifecycle/security | Dirty diff after `43249e8` contains Digest, EventBus, GB28181, and RTSP architecture fixes | In progress; no completion claim until Tasks 1-4 pass |
| Management console | Existing streams/player/publish/GB28181/DVR fragments only | Open |
| Release/operations docs | Runtime config docs exist; new control planes are not fully documented | Open |

---

### Task 1: Close WebRTC lifecycle ordering

**Files:**
- Modify: `module/webrtc/session.go`
- Modify: `module/webrtc/whip.go`
- Modify: `module/webrtc/whep.go`
- Test: `module/webrtc/lifecycle_order_test.go`
- Test: `module/webrtc/webrtc_test.go`

**Interfaces:**
- Consumes: `(*core.EventBus).EmitAsync(core.EventType, *core.EventContext)` and EventBus generation ordering keyed by `PublisherID` or `SubscriberID`.
- Produces: `(*Session).startLifecycle(*core.EventBus, core.EventType, *core.EventContext) bool` and `(*Session).stopLifecycle(*core.EventBus, core.EventType, *core.EventContext) bool`.
- Invariant: one session emits zero events when stopped before start, otherwise exactly one start followed by exactly one stop.

- [x] **Step 1: Verify the prepared lifecycle test is RED**

Run:

```bash
go test -count=1 ./module/webrtc -run '^TestSessionLifecycle'
```

Expected: compile failure because `Session.startLifecycle` and `Session.stopLifecycle` do not exist.

- [x] **Step 2: Add the minimal session-local state machine**

Add mutex-protected lifecycle state to `Session` and implement these exact signatures:

```go
func (s *Session) startLifecycle(bus *core.EventBus, event core.EventType, ctx *core.EventContext) bool
func (s *Session) stopLifecycle(bus *core.EventBus, event core.EventType, ctx *core.EventContext) bool
```

`startLifecycle` accepts only the initial state and enqueues the start before releasing lifecycle ownership. `stopLifecycle` marks an initial session terminal without emitting, or enqueues one stop after an accepted start. Repeated calls return false.

- [x] **Step 3: Verify the state-machine tests are GREEN under race**

Run:

```bash
go test -race -count=20 ./module/webrtc -run '^TestSessionLifecycle'
```

Expected: PASS with blocked start preceding stop in every repetition.

- [x] **Step 4: Wire WHIP and WHEP to stable generation identities**

WHIP lifecycle contexts set `PublisherID: sessionID`; WHEP lifecycle contexts set `SubscriberID: sessionID`. Replace direct async start and sync stop emissions with the two session methods. Authorization `EmitSync` remains separate and does not become a lifecycle start.

- [x] **Step 5: Add handler-level close-before-start and duplicate-close tests**

The tests use a real EventBus and a test PeerConnection/session boundary. They assert one start/stop pair for established WHIP/WHEP, no unmatched stop on negotiation failure, and no event duplication when ICE close races with `DELETE /webrtc/session/{id}`.

- [x] **Step 6: Run WebRTC package gates**

```bash
gofmt -w module/webrtc/session.go module/webrtc/whip.go module/webrtc/whep.go module/webrtc/lifecycle_order_test.go
go test -race -count=1 ./module/webrtc
go vet ./module/webrtc
git diff --check
```

Expected: all commands exit 0.

### Task 2: Make GB28181 lifecycle start atomic with cleanup

**Files:**
- Modify: `module/gb28181/session.go`
- Modify: `module/gb28181/handler.go`
- Modify: `module/gb28181/invite_client.go`
- Modify: `module/gb28181/playback.go`
- Test: `module/gb28181/lifecycle_test.go`

**Interfaces:**
- Produces: `(*MediaSession).startPublishLifecycle(func()) bool`.
- Invariant: cleanup cannot close a session between marking publish-started and enqueuing `EventPublish`; the matching publisher generation is the only generation removed.

- [x] **Step 1: Add a deterministic RED test for cleanup between mark and enqueue**

The test races `startPublishLifecycle` with `handler.closeSession`, blocks the EventBus start handler, and asserts either no lifecycle events or one ordered start/stop pair. It must fail against separate `MarkPublished` and `EmitAsync` calls.

Run:

```bash
go test -count=1 ./module/gb28181 -run '^TestMediaSessionPublishLifecycleIsAtomicWithClose$'
```

Expected: FAIL because the current two-call sequence permits unmatched cleanup.

- [x] **Step 2: Implement atomic start ownership**

Add:

```go
func (s *MediaSession) startPublishLifecycle(emit func()) bool
```

The method holds `s.mu`, rejects closed/already-published sessions, sets `published`, invokes the non-blocking enqueue callback, and releases the mutex. It must not run an EventBus handler inline.

- [x] **Step 3: Replace every GB28181 mark-then-emit call site**

Update inbound INVITE, outbound live INVITE, and outbound playback INVITE paths. Keep `closeSession` as the sole stop owner and retain `RemoveIf`/publisher-generation checks.

- [x] **Step 4: Verify setup rollback and lifecycle ordering together**

```bash
go test -race -count=20 ./module/gb28181 -run 'Test(MediaSessionPublishLifecycleIsAtomicWithClose|InboundInviteResponseFailureRollsBack|OutboundLiveACKFailureRollsBack|OutboundPlaybackACKFailureRollsBack|CloseSession)'
go test -race -count=1 ./module/gb28181
go vet ./module/gb28181
```

Expected: all commands exit 0; ports/sockets are reusable and no unmatched lifecycle event is observed.

### Task 3: Prove cross-protocol lifecycle monotonicity

**Files:**
- Modify: `module/rtsp/lifecycle_test.go`
- Modify: `module/gb28181/lifecycle_test.go`
- Modify: `module/webrtc/lifecycle_order_test.go`
- Modify only if a new RED case requires it: `core/event_bus.go`, `core/module.go`, relevant protocol session/handler file

**Interfaces:**
- Consumes: immutable `core.EventContext`, `PublisherID`, `SubscriberID`, and per-generation EventBus lanes.
- Invariant: an async stop never overtakes its matching async start; different generations remain independent; completed lane state is removed.

- [x] **Step 1: Add real RTSP publisher and subscriber inversion tests**

Use TCP RTSP requests through the module listener. Block the start hook, send `TEARDOWN` or close the connection, and assert stop waits. Repeat for ANNOUNCE/RECORD and DESCRIBE/SETUP/PLAY.

- [x] **Step 2: Add GB28181 inbound and outbound inversion tests**

Exercise inbound INVITE plus BYE, outbound live ACK plus API/session cleanup, and outbound playback ACK plus cleanup. Assert EventContext contains the exact Call-ID-derived publisher generation.

- [x] **Step 3: Add WebRTC publish and subscribe inversion tests**

Exercise the session lifecycle methods through the WHIP/WHEP handler-owned session and assert `SubscriberID` is non-empty for WHEP stop events.

- [x] **Step 4: Stress all lifecycle consumers**

```bash
go test -race -count=20 ./core ./module/record ./module/dvr ./module/rtsp ./module/gb28181 ./module/webrtc -run 'Test.*(Lifecycle|BlockedStart|StartAfterStop|Republish|CloseSession)'
```

Expected: PASS; Record and DVR retain no session after the publisher generation is gone.

- [x] **Step 5: Audit raw mutable session reads**

```bash
rg -n 'session\.(ID|StreamKey|RemoteAddr|State|Publisher|Subscriber|MediaInfo|Tracks|Stream)' module/rtsp module/gb28181 --glob '*.go'
```

Every production match must be a constructor-time immutable field, occur under the session mutex, or use a snapshot/mutation method. Add a focused RED race test before changing any unsafe match.

### Task 4: Stabilize SIP Gateway terminal metrics

**Files:**
- Modify: `module/sipgateway/gateway.go`
- Modify: `module/sipgateway/call_session.go`
- Test: `module/sipgateway/control_plane_test.go`
- Test: `module/sipgateway/control_plane_fix_test.go`

**Interfaces:**
- Consumes: `Gateway.Metrics() GatewayMetricsSnapshot` and `Gateway.ActiveCalls() int`.
- Invariant: once an observer sees `ActiveCalls == 0` for an established terminated call, the same snapshot cannot show the call-specific terminal counter without `CallsEnded`.

- [x] **Step 1: Add a RED observation-window stress test**

Start one established call, terminate it as network-lost, and sample `Metrics()` concurrently. Reject the state `ActiveCalls == 0 && NetworkFailures == 1 && CallsEnded == 0`.

```bash
go test -race -count=50 ./module/sipgateway -run '^TestTerminalMetricsPublishAtomicallyWithSessionRemoval$'
```

Expected: FAIL at least once against the current counter update after unlocking/removal.

- [x] **Step 2: Publish terminal state and metrics under one gateway ordering boundary**

Move the established-call `callsEnded` update into the same gateway critical section that removes the active session and appends terminal history. Preserve `CallSession.terminate` ownership for network-failure classification, and ensure duplicate finish calls still return false without incrementing counters.

- [x] **Step 3: Verify terminal metrics and full SIP packages**

```bash
go test -race -count=100 ./module/sipgateway -run 'Test(TerminalMetricsPublishAtomicallyWithSessionRemoval|GatewayDetectsRTCPNetworkLoss|GatewayDetectsNetworkLoss|GatewayCallControl)$'
go test -race -count=1 ./module/sip ./module/sipgateway
go vet ./module/sip ./module/sipgateway
```

Expected: all commands exit 0.

### Task 5: Unified re-verification of Tasks 1-5

**Files:**
- Modify only when verification produces a reproducible RED: owning package source/test files
- Update evidence: `.superpowers/sdd/2026-08-24-liveforge-completion/progress.md`
- Update evidence: `.superpowers/sdd/2026-08-24-liveforge-completion/task-4-report.md`

- [ ] **Step 1: Re-run focused race suites for every completed control plane**

```bash
go test -race -count=1 ./config ./config/runtime ./core ./internal/localfs ./module/auth ./module/api ./module/cluster ./module/dvr ./module/gb28181 ./module/httpstream ./module/metrics ./module/notify ./module/record ./module/rtsp ./module/sip ./module/sipgateway ./module/webrtc
```

- [ ] **Step 2: Stress high-risk security/lifecycle tests**

```bash
go test -race -count=20 ./module/sip ./module/sipgateway ./module/rtsp ./module/gb28181 ./module/webrtc ./module/record ./module/dvr -run 'Test.*(Digest|Replay|Lifecycle|Rollback|Close|Shutdown|TerminalMetrics|Symlink|Partial|Finalize)'
```

- [ ] **Step 3: Run static and portability checks**

```bash
go vet ./config ./config/runtime ./core ./internal/localfs ./module/auth ./module/api ./module/cluster ./module/dvr ./module/gb28181 ./module/httpstream ./module/metrics ./module/notify ./module/record ./module/rtsp ./module/sip ./module/sipgateway ./module/webrtc
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go test -exec=true -run '^$' ./core ./internal/localfs ./module/api ./module/cluster ./module/dvr ./module/gb28181 ./module/notify ./module/record ./module/rtsp ./module/sipgateway ./module/webrtc
done
```

- [ ] **Step 4: Record exact pass/fail output and loop failures**

Append commands, exit status, and any repair commits/diffs to the two evidence files. Any failure creates a focused RED test and returns to Tasks 1-4 or the owning original task.

### Task 6: Complete the embedded management console

**Files:**
- Modify: `module/api/console.html`
- Modify: `module/api/console_publish_test.go`
- Create: `module/api/console_management_test.go`
- Test: `module/api/recording_test.go`
- Test: `module/api/security_test.go`

**Interfaces:**
- Consumes: `GET/POST /api/v1/server/config`, `GET /api/v1/cluster/status`, SIP Gateway call CRUD, recording list/detail/download/delete, DVR status/detail, `GET /api/v1/security/status`, and `GET /api/v1/audit` as documented by the API router.
- Produces: build-free configuration, cluster, calls, storage, and security views plus permission-aware operator actions.

- [ ] **Step 1: Add RED console contract tests**

Parse the embedded HTML and assert each documented endpoint appears exactly in the intended request helper; assert destructive calls use `apiFetch`, explicit HTTP methods, confirmation, and surfaced 401/403/409 errors; assert token/password/Authorization values are never assigned into HTML.

```bash
go test -count=1 ./module/api -run '^TestConsoleManagement'
```

Expected: FAIL because the new views/actions are absent.

- [ ] **Step 2: Add navigation and stable unframed views**

Add compact sections for Runtime Config, Cluster Relays, SIP Calls, Recordings & DVR, and Security & Audit. Reuse the existing navigation/layout, keep controls under 8px radius, and provide fixed table/toolbar dimensions so updates do not shift the page.

- [ ] **Step 3: Add one authenticated request helper and typed error rendering**

Use the existing session cookie; do not store or display bearer tokens. The helper handles JSON envelopes, 204 responses, session expiry, forbidden actions, conflict, and unavailable modules without exposing response headers or secrets.

- [ ] **Step 4: Implement read views and bounded refresh**

Load only the active view, cancel stale requests with `AbortController`, render pending-restart paths and source failures, relay/call/record/DVR tables, and bounded audit entries. Poll visible operational views at a fixed interval and stop polling when hidden.

- [ ] **Step 5: Implement operator actions**

Add config refresh, SIP dial/hangup, and recording delete. Use icon-plus-text commands where clarity requires a label, native confirmation for destructive actions, disabled in-flight state, and server-authoritative error messages.

- [ ] **Step 6: Verify console contracts and API routes**

```bash
gofmt -w module/api/console_management_test.go module/api/console_publish_test.go
go test -race -count=1 ./module/api
go test -count=20 ./module/api -run 'TestConsole(Management|Publish)|TestManagementRouteContracts'
go vet ./module/api
```

Expected: all commands exit 0.

### Task 7: Synchronize release, migration, API, config, and operations documentation

**Files:**
- Modify: `agent-manifest.json`
- Modify: `llms.txt`
- Modify: `llms-full.txt`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/api/openapi.yaml`
- Modify: `docs/config/config.schema.json`
- Modify: `docs/PROGRESS.md`
- Create: `docs/recipes/recording-dvr-management.md`
- Create: `docs/recipes/sipgateway-management.md`
- Create: `docs/recipes/cluster-relay-operations.md`
- Create: `docs/recipes/rbac-audit.md`
- Create: `docs/recipes/release-verification.md`

- [ ] **Step 1: Generate a source-of-truth inventory before editing**

```bash
rg -n 'HandleFunc|Handle\(' module/api module/gb28181 module/webrtc --glob '*.go'
rg -n 'prometheus\.(New|MustNew)|prometheus.BuildFQName|Name:' module --glob '*.go'
rg -n 'yaml:"' config --glob '*.go'
```

Map every endpoint, permission, metric, config field, failure state, and restart-required path to one documentation owner.

- [ ] **Step 2: Complete OpenAPI and JSON Schema**

OpenAPI documents runtime config/status refresh, cluster status, SIP call control, recording management/download, DVR status/detail, security/audit responses, authentication, permissions, and error codes. JSON Schema documents runtime sources, RBAC bindings, audit settings, recording/DVR/reload fields, and restart-required/deferred annotations.

- [ ] **Step 3: Write runnable operations recipes**

Each recipe contains prerequisites, safe local examples, authenticated requests, expected status codes, metrics/diagnostics, rollback, and failure recovery. Public exposure warnings must state that the sample config disables TLS/auth and uses `admin/admin`.

- [ ] **Step 4: Synchronize user and Agent entrypoints**

Update both READMEs, both `llms` files, the manifest, and `docs/PROGRESS.md`. Remove stale planned claims only where source plus tests exist. Retain conditional release/image availability, optional FFmpeg support, and explicit Simulcast deferral.

- [ ] **Step 5: Verify route/config/document parity**

```bash
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
go test -count=1 ./module/api -run 'Test(OpenAPI|Route|Console)'
git diff --check
```

Expected: all commands exit 0 and no implemented public route/config key is undocumented.

### Task 8: Release-level verification loop and acceptance

**Files:**
- Modify only when a verification failure is reproduced: owning source/test/docs files
- Update evidence: `.superpowers/sdd/2026-08-24-liveforge-completion/progress.md`
- Update: this plan's checkboxes only after fresh evidence exists

- [ ] **Step 1: Format and inspect the complete branch delta**

```bash
gofmt -w $(git diff --name-only --diff-filter=ACMRTUXB | rg '\.go$')
git diff --check
git status --short
```

- [ ] **Step 2: Run the complete untagged verification**

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./cmd/liveforge
go build ./tools/lf-test
```

- [ ] **Step 3: Run the required FFmpeg-tagged baseline**

```bash
CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge
CGO_ENABLED=1 go test -tags audiocodec -race -coverprofile=coverage.out -covermode=atomic ./...
```

Expected: both commands exit 0 with Go 1.26 and the detected FFmpeg development libraries.

- [ ] **Step 4: Run both documentation gates**

```bash
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
```

- [ ] **Step 5: Run an authenticated local smoke test**

Build a temporary configuration derived from `configs/liveforge.yaml` with loopback-only ephemeral listener ports, TLS disabled for local testing, non-default console credentials, API bearer/RBAC enabled, and temporary recording/DVR directories. Start the server, wait for public health, then verify authenticated config/cluster/SIP/record/DVR/security/audit endpoints and denied unauthenticated mutations. Stop with SIGTERM and require a clean bounded exit.

- [ ] **Step 6: Audit the completion contract line by line**

Re-read `AGENTS.md`, both completion plans, `docs/PROGRESS.md`, all task reports, `git diff`, and verification logs. For each non-Simulcast feature require source, a passing behavior test, synchronized docs, and a runnable verification path.

- [ ] **Step 7: Loop until no failure or unsupported claim remains**

For every failure: capture the exact command/output, add or identify a focused RED test, apply the minimal repair, rerun the focused gate, then restart Task 8 from Step 1. Do not skip a failing package and do not narrow the final suite.

- [ ] **Step 8: Commit only the verified completion delta**

```bash
git add -A
git diff --cached --check
git commit -m "fix: close remaining LiveForge completion gaps"
```

The commit must exclude `coverage.out`, binaries, recordings, temporary configuration, secrets, and smoke-test data. Final delivery reports the commit, exact verification commands, test outcomes, documented Simulcast deferral, and any environmental blocker; an unresolved non-Simulcast blocker prevents delivery.

## Plan Self-Review

- [x] Every requirement in the original completion plan maps to the evidence ledger or Tasks 1-8 above.
- [x] No `TBD`, deferred non-Simulcast behavior, or unspecified error-handling step exists.
- [x] Method names and event identity fields match the current Go types.
- [x] Every implementation task contains a RED command, a minimal implementation boundary, and a GREEN/race gate.
- [x] Documentation and smoke-test work is part of acceptance rather than follow-up work.
