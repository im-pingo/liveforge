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

- [x] **Step 1: Re-run focused race suites for every completed control plane**

```bash
go test -race -count=1 ./config ./config/runtime ./core ./internal/localfs ./module/auth ./module/api ./module/cluster ./module/dvr ./module/gb28181 ./module/httpstream ./module/metrics ./module/notify ./module/record ./module/rtsp ./module/sip ./module/sipgateway ./module/webrtc
```

- [x] **Step 2: Stress high-risk security/lifecycle tests**

```bash
go test -race -count=20 ./module/sip ./module/sipgateway ./module/rtsp ./module/gb28181 ./module/webrtc ./module/record ./module/dvr -run 'Test.*(Digest|Replay|Lifecycle|Rollback|Close|Shutdown|TerminalMetrics|Symlink|Partial|Finalize)'
```

- [x] **Step 3: Run static and portability checks**

```bash
go vet ./config ./config/runtime ./core ./internal/localfs ./module/auth ./module/api ./module/cluster ./module/dvr ./module/gb28181 ./module/httpstream ./module/metrics ./module/notify ./module/record ./module/rtsp ./module/sip ./module/sipgateway ./module/webrtc
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go test -exec=true -run '^$' ./core ./internal/localfs ./module/api ./module/cluster ./module/dvr ./module/gb28181 ./module/notify ./module/record ./module/rtsp ./module/sipgateway ./module/webrtc
done
```

- [x] **Step 4: Record exact pass/fail output and loop failures**

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

- [x] **Step 1: Add RED console contract tests**

Parse the embedded HTML and assert each documented endpoint appears exactly in the intended request helper; assert destructive calls use `apiFetch`, explicit HTTP methods, confirmation, and surfaced 401/403/409 errors; assert token/password/Authorization values are never assigned into HTML.

```bash
go test -count=1 ./module/api -run '^TestConsoleManagement'
```

Expected: FAIL because the new views/actions are absent.

- [x] **Step 2: Add navigation and stable unframed views**

Add compact sections for Runtime Config, Cluster Relays, SIP Calls, Recordings & DVR, and Security & Audit. Reuse the existing navigation/layout, keep controls under 8px radius, and provide fixed table/toolbar dimensions so updates do not shift the page.

- [x] **Step 3: Add one authenticated request helper and typed error rendering**

Use the existing session cookie; do not store or display bearer tokens. The helper handles JSON envelopes, 204 responses, session expiry, forbidden actions, conflict, and unavailable modules without exposing response headers or secrets.

- [x] **Step 4: Implement read views and bounded refresh**

Load only the active view, cancel stale requests with `AbortController`, render pending-restart paths and source failures, relay/call/record/DVR tables, and bounded audit entries. Poll visible operational views at a fixed interval and stop polling when hidden.

- [x] **Step 5: Implement operator actions**

Add config refresh, SIP dial/hangup, and recording delete. Use icon-plus-text commands where clarity requires a label, native confirmation for destructive actions, disabled in-flight state, and server-authoritative error messages.

- [x] **Step 6: Verify console contracts and API routes**

```bash
gofmt -w module/api/console_management_test.go module/api/console_publish_test.go
go test -race -count=1 ./module/api
go test -count=20 ./module/api -run 'TestConsole(Management|Publish)|TestManagementRouteContracts'
go vet ./module/api
```

Expected: all commands exit 0.

### Task 6B: Close source gaps discovered by the documentation inventory

**Files:**
- Modify: `module/gb28181/api.go`
- Modify: `module/api/rbac.go`
- Modify: `module/cluster/module.go`
- Modify: `module/cluster/transport_rtp.go`
- Modify: `module/cluster/transport_gb.go`
- Modify: `config/runtime/manager.go`
- Test: the owning package tests under `module/gb28181`, `module/api`, `module/cluster`, and `config/runtime`
- Create: focused helper tests for `tools/gb28181-sim` and `tools/lf-test`

**Interfaces and invariants:**
- GB28181 live/playback stop handlers are reachable through the real `http.ServeMux` and require `gb28181:control`, matching the corresponding start actions.
- RTP and GB28181 relay signaling resolves a node credential from the current atomic server config for every request, preferring `api.auth.bearer_token`, otherwise a named token whose role is `admin`; no credential lookup performs I/O or logs a secret.
- A peer signaling request fails locally with a clear error when management authentication is configured but no admin-capable token exists; authenticated peers receive `Authorization: Bearer ...` and token rotation does not require a process restart.
- Runtime callback coalescing counts each superseded pending callback in `DroppedCallbacks` while preserving delivery of the newest snapshot.
- Typed runtime-key tests prove a hot value remains old before commit and becomes visible only after atomic publication.

- [x] **Step 1: Add RED real-router tests for GB28181 stop operations and RBAC**

Register the GB28181 API on a real management mux, invoke both DELETE stop paths, and prove they reach their handlers. Add permission tests proving an operator can stop live/playback through `gb28181:control` while a viewer is denied.

```bash
go test -count=1 ./module/gb28181 ./module/api -run 'Test.*(Channel.*Delete|GB28181.*Permission|ManagementRoute)'
```

Expected: FAIL because the DELETE subtree is not registered and is currently classified as `gb28181:manage`.

- [x] **Step 2: Add RED authenticated relay-signaling tests, then attach rotating credentials**

Use authenticated peer test servers that reject missing/wrong bearer credentials. Exercise both RTP and GB28181 outbound signaling, verify success with the selected admin credential, verify a hot credential change is observed by the next request, and verify a configured viewer/operator-only token set fails before network dispatch.

```bash
go test -count=1 ./module/cluster -run 'Test.*(RTP|GB28181).*(Auth|Credential|Token)'
```

Expected: FAIL because the current transports do not send node credentials.

- [x] **Step 3: Add RED callback-coalescing accounting test**

Block callback delivery, accept two newer snapshots, and assert one superseded pending transition is counted while the latest version is ultimately delivered.

```bash
go test -count=1 ./config/runtime -run '^TestManagerCoalescesNotificationsWithoutLosingLatestSnapshot$'
```

Expected: FAIL because `DroppedCallbacks` remains zero.

- [x] **Step 4: Strengthen atomic-key and CLI package behavior tests**

Replace the immutable-key runtime test with a deterministic `limits.max_streams` commit-boundary test. Add meaningful tests around existing parsing, formatting, channel-building, and error-classification helpers in `tools/gb28181-sim` and `tools/lf-test` so the required coverage suite has real test-bearing packages rather than empty packages.

- [x] **Step 5: Run focused closure gates and record documentation impact**

```bash
gofmt -w module/gb28181 module/api module/cluster config/runtime tools/gb28181-sim tools/lf-test
go test -race -count=1 ./module/gb28181 ./module/api ./module/cluster ./config/runtime ./tools/gb28181-sim ./tools/lf-test
go test -race -count=20 ./module/cluster ./config/runtime -run 'Test.*(Auth|Credential|Token|Coalesces|Atomic|Key)'
go vet ./module/gb28181 ./module/api ./module/cluster ./config/runtime ./tools/gb28181-sim ./tools/lf-test
git diff --check
```

Expected: all commands exit 0. The Task 7 inventory and documentation work must include the reachable DELETE routes, node credential behavior and rotation rule, callback-drop semantics, and the added release verification packages.

### Task 6C: Repair RTSP multi-track SETUP state handling

**Files:**
- Modify: `module/rtsp/handler.go`
- Test: `module/rtsp/helpers_test.go` or a focused multi-track handler test
- Test: `tools/testkit/push/pusher_test.go`
- Test: `tools/testkit/play/player_test.go`

**Root-cause evidence:** `.superpowers/sdd/2026-08-25-liveforge-completion-loop/rtsp-455-diagnosis.md`

**Invariant:** The first valid SETUP transitions an announced/described session to `StateReady`; additional valid track SETUP requests preserve `StateReady` and return 200. SETUP in genuinely invalid states still returns 455, and no rejected request leaves a track installed.

- [x] **Step 1: Add RED multi-track response and atomicity tests**

Exercise two SETUP requests for both announce/record and describe/play flows. Assert both responses are 200, both tracks exist, the state remains Ready, and an invalid-state SETUP returns 455 without changing the track set.

```bash
go test -count=1 ./module/rtsp -run 'Test.*MultiTrack.*Setup'
```

Expected: FAIL because the second valid SETUP performs an illegal `Ready -> Ready` transition and because mutation currently precedes validation.

- [x] **Step 2: Separate per-track setup from the one-time Ready transition**

Validate that SETUP is permitted in announced, described, or ready states before installing the track. Transition only announced/described sessions to Ready; keep Ready unchanged for additional tracks. Preserve 455 for every other state and do not mutate rejected sessions.

- [x] **Step 3: Verify handler, package, and end-to-end flows**

```bash
gofmt -w module/rtsp/handler.go module/rtsp/*_test.go
go test -race -count=20 ./module/rtsp -run 'Test.*(MultiTrack.*Setup|Setup)'
go test -count=5 ./tools/testkit/push -run '^TestRTSPPush$'
go test -count=5 ./tools/testkit/play -run '^TestRTSPPlay$'
go test -race -count=1 ./module/rtsp ./tools/testkit/push ./tools/testkit/play
go vet ./module/rtsp ./tools/testkit/push ./tools/testkit/play
git diff --check
```

Expected: all commands exit 0. Record the regression and its user-visible multi-track compatibility impact in `docs/PROGRESS.md` during Task 7.

### Task 6D: Make DVR media routes valid on Go 1.26

**Files:**
- Modify: `module/dvr/module.go`
- Modify: `module/dvr/handler.go`
- Test: focused real-mux and module-init tests under `module/dvr`
- Modify: `docs/PROGRESS.md`

**Root-cause evidence:** the authenticated Task 8 smoke start panics while registering `GET /dvr/{app}/{key}.m3u8`; Go 1.26 requires a wildcard segment to end with `}` and does not permit a suffix after `{key}`.

**Invariant:** The documented media URLs remain `GET /dvr/{app}/{key}.m3u8` and `GET /dvr/{app}/{key}/{filename}`. Registration must use valid Go 1.26 ServeMux syntax, dispatch must preserve exact app/key/filename values, malformed or nested resources return 404, and module startup/shutdown is bounded without panic.

- [x] **Step 1: Add RED real-registration and dispatch tests**

Use `127.0.0.1:0` and a real `core.Server.Init` with DVR enabled to prove module registration currently panics. Add real mux requests for playlist, segment, malformed suffix, missing key, and nested filename traversal.

- [x] **Step 2: Register one legal terminal wildcard and dispatch strictly**

Register a legal tail wildcard under `/dvr/{app}/`, parse only the two existing URL shapes, populate handler path values without string-concatenating filesystem paths, and return 404 before authorization/storage access for malformed resources.

- [x] **Step 3: Verify DVR and smoke startup gates**

```bash
gofmt -w module/dvr
go test -race -count=20 ./module/dvr -run 'Test.*(Route|Init|Playlist|Segment)'
go test -race -count=1 ./module/dvr ./module/api
go vet ./module/dvr ./module/api
tools/check-agent-docs_test.sh
git diff --check
```

Expected: all commands exit 0 and the Task 8 loopback smoke server starts with DVR enabled.

### Task 6E: Classify normal SIP listener shutdown without ERROR noise

**Files:**
- Modify: `module/sip/dispatch.go`
- Test: focused real-listener shutdown/log tests under `module/sip`
- Modify: `docs/PROGRESS.md`

**Root-cause evidence:** Task 8 completed SIGTERM in 0.210 seconds with exit 0 and all listeners/PIDs removed, but the SIP TCP `ListenAndServe` goroutine unconditionally logged its cancellation-driven `accept ... use of closed network connection` return at ERROR. The same line appeared in the Task 6D smoke and the fresh Task 8 smoke.

**Invariant:** A normal context-cancelled SIP TCP or UDP listener shutdown is bounded and emits no ERROR. A non-nil listener failure while its service context is still active remains an ERROR with transport metadata. Shutdown classification must not hide an unexpected live-listener failure.

- [x] **Step 1: Add a RED real-listener shutdown log test**

Start a real SIP service on a reserved loopback address, wait until the configured TCP listener accepts connections, cancel it through `service.close`, and capture structured slog output. Require bounded return and zero ERROR records for the cancellation-driven listener result. Add a focused classifier/logging case proving an unexpected error with an active context remains ERROR.

- [x] **Step 2: Apply the minimal context-aware listener result classification**

Classify the `ListenAndServe` return at the goroutine boundary. Suppress only a listener-stop error associated with the service context already being cancelled; continue to log any non-nil return while the context is active. Do not use message-string matching, do not change SIP request handling, and do not weaken listener startup failures.

- [x] **Step 3: Verify SIP dependents and the full smoke shutdown**

```bash
gofmt -w module/sip
go test -race -count=20 ./module/sip -run 'Test.*Listener.*(Shutdown|Stop)'
go test -race -count=1 ./module/sip ./module/sipgateway ./module/gb28181
go vet ./module/sip ./module/sipgateway ./module/gb28181
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
git diff --check
```

Expected: all commands exit 0. The authenticated Task 8 smoke then receives SIGHUP, exits 0 under SIGTERM within the drain bound, removes TCP/UDP listeners and PID, and contains no ERROR/panic/fatal or secret value in its complete log.

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

- [x] **Step 1: Generate a source-of-truth inventory before editing**

```bash
rg -n 'HandleFunc|Handle\(' module/api module/gb28181 module/webrtc --glob '*.go'
rg -n 'prometheus\.(New|MustNew)|prometheus.BuildFQName|Name:' module --glob '*.go'
rg -n 'yaml:"' config --glob '*.go'
```

Map every endpoint, permission, metric, config field, failure state, and restart-required path to one documentation owner.

- [x] **Step 2: Complete OpenAPI and JSON Schema**

OpenAPI documents runtime config/status refresh, cluster status, SIP call control, recording management/download, DVR status/detail, security/audit responses, authentication, permissions, and error codes. JSON Schema documents runtime sources, RBAC bindings, audit settings, recording/DVR/reload fields, and restart-required/deferred annotations.

- [x] **Step 3: Write runnable operations recipes**

Each recipe contains prerequisites, safe local examples, authenticated requests, expected status codes, metrics/diagnostics, rollback, and failure recovery. Public exposure warnings must state that the sample config disables TLS/auth and uses `admin/admin`.

- [x] **Step 4: Synchronize user and Agent entrypoints**

Update both READMEs, both `llms` files, the manifest, and `docs/PROGRESS.md`. Remove stale planned claims only where source plus tests exist. Retain conditional release/image availability, optional FFmpeg support, and explicit Simulcast deferral.

- [x] **Step 5: Verify route/config/document parity**

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

- [x] **Step 1: Format and inspect the complete branch delta**

```bash
gofmt -w $(git diff --name-only --diff-filter=ACMRTUXB | rg '\.go$')
git diff --check
git status --short
```

- [x] **Step 2: Run the complete untagged verification**

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./cmd/liveforge
go build ./tools/lf-test
```

- [x] **Step 3: Run the required FFmpeg-tagged baseline**

```bash
CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge
CGO_ENABLED=1 go test -tags audiocodec -race -coverprofile=coverage.out -covermode=atomic ./...
```

Expected: both commands exit 0 with Go 1.26 and the detected FFmpeg development libraries.

- [x] **Step 4: Run both documentation gates**

```bash
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
```

- [x] **Step 5: Run an authenticated local runtime smoke test**

Build an ignored smoke binary and an ignored temporary configuration derived from `configs/liveforge.yaml`. Bind every enabled listener to a dedicated loopback-only test port, disable TLS only for this local test, use non-default console credentials, enable API bearer/RBAC with viewer/operator/admin principals, and place recording/DVR data under the ignored smoke workspace. Do not print or commit credentials.

Start the real `cmd/liveforge` binary and require all of the following:

- Public `GET /api/v1/server/health` reaches 200 without credentials.
- A protected management read without credentials reaches 401.
- The viewer can read config, cluster, SIP, recordings, DVR, security, audit, and GB28181 status but cannot mutate them.
- The operator can schedule config refresh and perform documented operator actions, while admin-only deletion remains denied.
- The admin can reach every documented management action used by the console.
- Cluster, SIP Gateway, recording, DVR, security, audit, and GB28181 endpoints return their documented envelopes/status shapes; audit contains the exercised authenticated operations.
- `SIGHUP` schedules an asynchronous refresh while the process remains responsive and configuration reads do not perform source I/O.
- `SIGTERM` exits cleanly within the configured drain bound with no panic, leaked listener, or unfinalized smoke process.

Save command/status evidence under this plan's ignored SDD workspace. A failed endpoint or lifecycle check returns to its owning task and restarts Task 8 at Step 1.

- [x] **Step 6: Run embedded-console browser acceptance**

Use the in-app Browser against the authenticated smoke server. Exercise login and all seven tabs: Streams, GB28181, Config, Cluster, SIP Calls, Storage, and Security. The Security tab includes the Recent Audit surface. Require:

- Viewer/operator/admin visibility and action states agree with server RBAC; 401, 403, 409, and unavailable-module errors surface without exposing tokens, passwords, headers, or raw secret-bearing bodies.
- Hostile dynamic stream/call/record identifiers render as text and cannot create markup or executable attributes.
- ArrowLeft/ArrowRight/Home/End tab navigation, focus indication, modal focus containment, Escape dismissal, and focus restoration work from the keyboard.
- Only the visible page polls; switching tabs or hiding the document aborts/stops stale polling.
- Desktop and mobile screenshots show no incoherent overlap, clipped control text, nested-card layout, or horizontal body overflow.
- Browser console has no uncaught exception, failed asset load, or repeated unauthorized request loop.

Record the tested viewport sizes, screenshots, browser-console result, network/auth observations, and any repaired defect in the ignored SDD evidence workspace.

- [x] **Step 7: Audit the completion contract line by line**

Re-read `AGENTS.md`, `agent-manifest.json`, `llms.txt`, `llms-full.txt`, both completion plans, the configuration-loader spec, `docs/PROGRESS.md`, all task reports/reviews, the complete branch diff, and fresh verification logs. For every non-Simulcast feature require all four columns below; a missing column is a blocker:

| Feature | Source implementation | Passing behavior/race test | Synchronized user/Agent docs | Runnable verification path |
| --- | --- | --- | --- | --- |
| Runtime configuration sources and atomic reads | required | required | required | required |
| Protocol lifecycle/security and RTSP multi-track | required | required | required | required |
| Cluster relay/status/authentication | required | required | required | required |
| SIP Gateway control plane | required | required | required | required |
| Recording and DVR management/media serving | required | required | required | required |
| RBAC, audit, and management console | required | required | required | required |
| Build/release/install claims | required or explicitly unavailable | required | required | required |

Confirm Simulcast layer selection is the only explicit deferral everywhere and remains configuration-only.

- [x] **Step 8: Run an independent whole-branch review**

Generate one review package from the branch merge-base through the verified head. A fresh reviewer checks the complete non-Simulcast acceptance contract, security boundaries, concurrency/lifecycle behavior, test quality, documentation parity, and the ledger's rulings/deferred minors. Critical or Important findings get exactly one consolidated fix dispatch and one scoped re-review; residual load-bearing findings block delivery.

- [x] **Step 9: Loop until no failure or unsupported claim remains**

For every test, smoke, browser, audit, or review failure: capture the exact command/output, add or identify a focused RED test, apply the minimal repair in the owning module, rerun its focused gate and independent scoped review, then restart Task 8 from Step 1. Do not skip a failing package, narrow a final suite, waive a browser defect, or convert an unsupported claim into completion.

- [x] **Step 10: Commit only the verified completion delta**

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
- [x] Browser QA, role-matrix smoke tests, SIGHUP/SIGTERM behavior, whole-branch review, and cleanup are explicit terminal gates.
