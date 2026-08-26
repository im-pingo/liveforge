# P0/P1 Hardening and Browser Playback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the P0/P1 lifecycle and authorization behavior onto the current `main` while making console HTTP playback use and validate the active media listener.

**Architecture:** Keep the current `main` module implementations as the base. Add small shared contracts in `core` for endpoint discovery, authorization, immutable runtime configuration, canonical stream keys, and cancellation-safe readers; then adapt each protocol boundary and its existing tests. The console continues to use the existing muxers and browser libraries, but consumes bound endpoint addresses and validates media responses before player startup.

**Tech Stack:** Go 1.26, `net/http`, existing event bus and stream hub, Go `sync`/`context`, vanilla console JavaScript, mpegts.js/hls.js/dash.js, chromedp tests.

**Spec:** `docs/superpowers/specs/2026-08-26-p0-p1-main-playback-design.md`

## Global Constraints

- Preserve current `main` DVR, cluster, forwarding, playback, RTSP, SIP, and recording behavior.
- Use Go 1.26+ and keep the default no-CGO build valid.
- Run `tools/check-agent-docs_test.sh` after every source change.
- Synchronize `agent-manifest.json`, `llms-full.txt`, `llms.txt`, README files, and affected recipes/API/schema when behavior or contracts change.
- Never add secrets or recommend the sample `admin/admin` configuration for public exposure.

---

### Task 1: Bound endpoint discovery and console media diagnostics

**Files:**
- Modify: `core/module.go`, `module/api/handler.go`, `module/httpstream/module.go`, `module/api/module_test.go`, `module/api/handler_test.go`
- Modify: `module/api/console.html`, `module/api/console_publish_test.go`
- Test: existing API and console tests plus a new endpoint response regression in `module/api/handler_test.go`

**Interfaces:**
- Add `core.EndpointProvider` with `Addr() net.Addr`.
- `handleServerInfo` returns the active module address when the module implements the interface and has a listener; otherwise it returns the configured address.
- Console `mediaURL()` builds URLs from the normalized endpoint and `checkMediaResponse()` rejects non-2xx or non-media responses before attaching mpegts/MSE/HLS/DASH.

- [ ] Write a failing API test that initializes API and HTTP modules on `127.0.0.1:0`, calls `/api/v1/server/info`, and asserts the HTTP endpoint contains the HTTP module's actual port.
- [ ] Run `go test ./module/api -run 'Test(ServerInfo|Endpoint)' -count=1` and observe the configured `:0` value fail the assertion.
- [ ] Implement the endpoint provider and response fallback without changing the JSON shape.
- [ ] Add console tests that assert a wrong-port HTML response yields an explicit HTTP/status error and that wildcard/IPv6 endpoint normalization preserves a valid URL.
- [ ] Run focused API tests and `tools/check-agent-docs_test.sh`.
- [ ] Commit as `fix: report active media listener to console`.

### Task 2: Add shared authorization admission contract

**Files:**
- Create: `core/authorizer.go`, `core/authorizer_test.go`
- Modify: `core/server.go`, `core/event_bus.go`, `core/stream_hub.go`, `core/stream_hub_test.go`
- Modify: `module/auth/auth.go`, `module/auth/module.go`, `module/auth/auth_test.go`

**Interfaces:**
- `core.Authorizer` exposes synchronous `AuthorizePublish(ctx *EventContext) error` and `AuthorizeSubscribe(ctx *EventContext) error` adapters over the configured hooks.
- Stream creation and subscriber admission invoke authorization before mutating hub state.

- [ ] Write deny-before-state tests for publish and subscribe.
- [ ] Run focused core/auth tests and verify the tests fail because admission currently occurs after state allocation in affected paths.
- [ ] Implement the common authorizer and preserve existing event bus hooks as the compatibility source.
- [ ] Run `go test ./core ./module/auth -run 'Authorization|Authorize|Admission' -count=1` and the docs check.
- [ ] Commit as `feat: add shared publish and subscribe authorization`.

### Task 3: Port immutable runtime configuration boundaries

**Files:**
- Create or modify: `config/change.go`, `config/clone.go`, `config/file_source.go`, `config/manager.go`, `config/source.go`
- Modify: `config/config.go`, `config/validate.go`, `core/runtime_config.go`, `core/server.go`, `core/server_test.go`
- Modify: `cmd/liveforge/main.go`, `cmd/liveforge/main_test.go`, `module/api/config_handler.go`, `module/api/config_view.go`

**Interfaces:**
- Runtime consumers read a cloned immutable `configruntime.ConfigSnapshot`/runtime view.
- Listener/module/TLS/port and enablement changes remain restart-required; hot reload applies only reloadable policy.

- [ ] Add tests proving source documents are cloned and rejected snapshots do not mutate the active configuration.
- [ ] Run config/core tests to capture current aliasing or stale-pointer failures.
- [ ] Implement snapshot publication, validation, and reload ownership while retaining current configuration fields.
- [ ] Run `go test ./config ./core ./cmd/liveforge ./module/api -run 'Config|Reload|Runtime' -count=1` and docs checks.
- [ ] Commit as `feat: publish immutable runtime configuration snapshots`.

### Task 4: Make ring readers and muxer shutdown cancellation-safe

**Files:**
- Modify: `pkg/util/ringbuffer.go`, `pkg/util/ringbuffer_test.go`
- Modify: `core/transcode_manager.go`, `module/httpstream/{hls.go,dash.go,llhls_segmenter.go,muxer_worker.go}`
- Modify: `module/cluster/{transport_gb.go,transport_rtmp.go,transport_rtp.go,transport_rtsp.go,transport_srt.go}`, `module/dvr/session.go`, `module/record/session.go`, `module/sipgateway/call_session.go`, `module/webrtc/whep_feed.go`

**Interfaces:**
- A ring reader close cancels its context, wakes blocked reads, and unregisters the wake callback exactly once.
- Protocol readers close their owned ring reader from terminal goroutines and do not leak condition callbacks.

- [ ] Add deterministic tests that block readers, close them, and assert prompt return plus zero remaining callbacks.
- [ ] Run `go test ./pkg/util -run 'Ring|Reader' -count=1` and capture the pre-fix hang/failure.
- [ ] Implement context-aware waits and adapt all current reader owners without changing frame ordering.
- [ ] Run focused package tests and `go test -race ./pkg/util ./module/httpstream ./module/webrtc`.
- [ ] Commit as `fix: make ring reader shutdown cancellation safe`.

### Task 5: Enforce canonical route and muxer ownership

**Files:**
- Modify: `core/secure_path.go`, `core/stream_key.go`, `core/stream.go`, `core/stream_hub.go`, `core/muxer_manager.go`
- Modify: `module/httpstream/{handler.go,handler_hls.go,handler_dash.go,ws_handler.go,module.go}`
- Modify: `module/rtmp/{handler.go,subscriber.go}`, `module/rtsp/{handler.go,session.go,server.go}`, `module/srt/module.go`

**Interfaces:**
- Stream keys and route paths are canonicalized once and rejected when they escape their app/key ownership boundary.
- Muxer generation and stream replacement are checked before attaching a subscriber.

- [ ] Add path traversal, duplicate publisher, and stale muxer generation tests.
- [ ] Run focused core/httpstream/RTMP/RTSP/SRT tests to verify the new tests fail.
- [ ] Implement canonicalization and ownership checks at the route/session boundaries.
- [ ] Run focused tests with `-race` and the docs check.
- [ ] Commit as `fix: enforce stream and muxer ownership boundaries`.

### Task 6: Harden HTTP and WebRTC lifecycle admission/cleanup

**Files:**
- Modify: `module/httpstream/handler.go`, `module/httpstream/handler_hls.go`, `module/httpstream/handler_dash.go`, `module/dvr/handler.go`, `module/dvr/session.go`
- Modify: `module/webrtc/{auth.go,connection.go,module.go,whip.go,whep.go,whep_feed.go}`
- Modify: `module/webrtc/*_test.go`, `module/httpstream/*_test.go`, `module/dvr/*_test.go`

**Interfaces:**
- HTTP/DVR/HLS/DASH/FMP4/TS subscriptions authorize synchronously before muxer/session allocation.
- WHIP/WHEP authorize both before resource creation and after connection establishment where required; terminal cleanup is serialized and idempotent.

- [ ] Add denial tests asserting no stream/session/subscriber remains.
- [ ] Add terminal WHIP/WHEP tests asserting one cleanup and no leaked connection.
- [ ] Run focused tests to establish failures.
- [ ] Implement admission ordering, post-connect authorization, and cleanup ownership while preserving current media readers.
- [ ] Run focused race tests and docs checks.
- [ ] Commit as `fix: harden protocol authorization and terminal cleanup`.

### Task 7: Synchronize project documentation and verification

**Files:**
- Modify: `agent-manifest.json`, `llms.txt`, `llms-full.txt`, `README.md`, `README.zh-CN.md`
- Modify: `docs/api/openapi.yaml`, affected `docs/recipes/*.md`, `AGENTS.md` only if the contract changes

- [ ] Document active listener endpoint discovery and browser media error behavior.
- [ ] Document unified publish/subscribe authorization and terminal cleanup guarantees.
- [ ] Run `tools/check-agent-docs_test.sh` and `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`.
- [ ] Commit as `docs: synchronize p0 p1 and preview contracts`.

### Task 8: Full verification and PR handoff

- [ ] Run focused package tests for every changed module.
- [ ] Run `GOPATH=/tmp/liveforge-go-path GOMODCACHE=/tmp/liveforge-go-modcache GOTOOLCHAIN=auto go test ./...`.
- [ ] Run the tagged race/coverage suite when FFmpeg development libraries are available; otherwise record the blocker.
- [ ] Re-run both agent-doc checks and inspect `git diff origin/main...HEAD` for accidental regressions or deleted current-main files.
- [ ] Push `codex/p0-p1-main-playback` and open a PR; leave old PR #16 unchanged.
