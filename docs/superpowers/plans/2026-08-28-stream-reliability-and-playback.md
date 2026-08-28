# Stream Reliability And Playback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the confirmed WHEP playback regression and harden the documented cache, lifecycle, resource, configuration, performance, and Console gaps with reproducible verification.

**Architecture:** Keep `core.StreamStartupSnapshot` as the only cross-module startup contract. WebRTC, HTTP, DVR, SIP, and GB28181 consumers use generation-aware admission and readers; diagnostics are attached to the owning session instead of inferred by the Console watchdog. Changes are split into independently testable phases so each can be reverted without changing the media model or reintroducing an audio cache.

**Tech Stack:** Go 1.26, Pion WebRTC, Chromium/chromedp, Go race detector, Prometheus, YAML/JSON Schema, local UDP protocol labs.

**Spec:** `docs/superpowers/specs/2026-08-28-stream-reliability-and-playback-design.md`

## Global Constraints

- Use `go 1.26` and keep `CGO_ENABLED=1` plus the `audiocodec` tag for the full baseline.
- Run `tools/check-agent-docs_test.sh` after every source change and `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh` before integration.
- Preserve the single interleaved GOP cache; do not add or restore `audioCache`.
- Keep the sample configuration local-only and never commit secrets, recordings, binaries, or private URLs.
- Update `agent-manifest.json`, `llms-full.txt`, `README.md`, `README.zh-CN.md`, schema/OpenAPI/recipes when the changed behavior affects them.
- Commit as `im-pingo <cczjp89@gmail.com>`; never use the `Pingos` identity for authored commits.

---

### Task 1: WHEP startup state and H.264 first-frame path

**Files:**
- Modify: `module/webrtc/whep.go`
- Modify: `module/webrtc/whep_feed.go`
- Modify: `module/webrtc/track_sender.go`
- Modify: `module/webrtc/session.go`
- Modify: `module/api/protocol_lab.go`
- Modify: `module/api/console.html`
- Test: `module/webrtc/whep_feed_test.go`
- Test: `module/webrtc/whep_e2e_test.go`
- Test: `module/webrtc/whep_browser_test.go`

**Interfaces:**
- `whepFeedLoop` produces a terminal/ongoing `WHEPFeedStatus` containing generation, startup cursor, mode, readiness, dropped frames, sent frames, first-media time, RTP counters when available, and a redacted terminal error.
- `Session` exposes a concurrency-safe diagnostic snapshot for the Console/API path without exposing mutable internals.
- The Console uses `mode=live` for its default preview and renders explicit waiting/error states from the returned session status.

- [ ] **Step 1: Write failing tests** for realtime waiting-keyframe diagnostics, live cached-keyframe startup, empty-cache behavior, source generation termination, H.264 Annex-B output, and propagated `WriteSample` failure.
- [ ] **Step 2: Run focused WebRTC tests** with `go test ./module/webrtc -run 'WHEP|whep' -count=1 -v`; confirm each new regression test fails for the expected missing state/error behavior.
- [ ] **Step 3: Implement the status model and make feed writes return structured errors**; preserve audio-only live-cursor behavior and the existing single GOP cache.
- [ ] **Step 4: Change only the Console default to `mode=live`**, keep explicit realtime semantics, and render waiting-keyframe versus terminal failure distinctly.
- [ ] **Step 5: Run focused tests and the browser H.264 test**; inspect SDP codec, dimensions, advancing `currentTime`, RTP counts, and media errors.
- [ ] **Step 6: Update WebRTC/OpenAPI/recipe/AI-facing documentation** for the status fields and default mode, then run the agent-doc checks.
- [ ] **Step 7: Commit** with `git -c user.name='im-pingo' -c user.email='cczjp89@gmail.com' commit` after verification.

### Task 2: Core cache bounds and generation-safe consumers

**Files:**
- Modify: `config/config.go`
- Modify: `config/validate.go`
- Modify: `docs/config/config.schema.json`
- Modify: `core/stream.go`
- Modify: `core/stream_hub.go`
- Modify: `core/transcode_manager.go`
- Modify: `module/httpstream/module.go`
- Modify: HLS/DASH/LL-HLS manager files under `module/httpstream/`
- Test: `core/stream_test.go`
- Test: `core/stream_hub_test.go`
- Test: `pkg/util/ringbuffer_test.go`
- Test: `module/httpstream/*_test.go`

**Interfaces:**
- Stream config exposes validated `gop_cache_max_frames`, `gop_cache_max_duration`, and `gop_cache_max_bytes` with bounded defaults.
- Cleanup APIs take `streamKey` and optional publisher generation/identity and never remove a newer generation.
- `NewRingBuffer` is safe for direct zero/negative-capacity callers while config validation remains fail-closed.

- [ ] **Step 1: Add failing tests** for each GOP bound, zero/negative ring capacity, stale destroy after republish, and historical HTTP stream registry cleanup.
- [ ] **Step 2: Run `go test ./core ./pkg/util ./module/httpstream -run 'GOP|Ring|Destroy|Republish' -count=1`** and record the expected failures.
- [ ] **Step 3: Implement bounded cache eviction and ring constructor protection** without changing interleaved audio ownership.
- [ ] **Step 4: Replace pointer-retaining HTTP registration and make manager cleanup generation-aware**; make publisher timeout remove idle streams only when the generation still matches.
- [ ] **Step 5: Run package tests, `go test -race ./core ./pkg/util ./module/httpstream`, and targeted allocation benchmarks.
- [ ] **Step 6: Update schema, config recipe, manifest and llms docs** with defaults and memory-bound semantics.
- [ ] **Step 7: Commit** the independently verified core reliability phase as `im-pingo`.

### Task 3: Strict connection, RTP ownership, and shutdown

**Files:**
- Modify: `core/server.go`
- Modify: `module/dvr/handler.go`
- Modify: `module/dvr/session.go`
- Modify: `module/gb28181/rtp_receiver.go`
- Modify: `module/gb28181/device_registry.go`
- Modify: `module/gb28181/module.go`
- Modify: `pkg/ratelimit/ratelimit.go`
- Modify: `module/httpstream/handler.go`
- Modify: `module/httpstream/handler_hls.go`
- Test: `core/server_test.go`
- Test: `module/dvr/*_test.go`
- Test: `module/gb28181/*_test.go`
- Test: `pkg/ratelimit/ratelimit_test.go`

**Interfaces:**
- `Server.AcquireConn` is an atomic admission operation that never returns true above the configured limit.
- Device registry readers receive immutable snapshots; registry and limiter close operations are idempotent.
- Forwarded client IP is accepted only through an explicit trusted-proxy policy.
- All stream responses observe request cancellation and bounded write/header deadlines.

- [ ] **Step 1: Add failing concurrency, buffer-aliasing, snapshot-race, double-close, trusted-proxy, and cancellation tests.**
- [ ] **Step 2: Run focused tests with `-race` and confirm the regressions reproduce.**
- [ ] **Step 3: Implement CAS connection admission, DVR release-once, owned RTP payloads, immutable registry snapshots, and idempotent close.**
- [ ] **Step 4: Replace cancellable streaming sleeps and add bounded HTTP deadlines without changing valid playlist contents.
- [ ] **Step 5: Run `go test -race ./core ./module/dvr ./module/gb28181 ./module/httpstream ./pkg/ratelimit` and inspect goroutine/resource cleanup.
- [ ] **Step 6: Update security/operations docs and manifest entries for trusted proxies, connection coverage, and shutdown guarantees.
- [ ] **Step 7: Commit** with the required `im-pingo` author.

### Task 4: Hot-path and configuration-source hardening

**Files:**
- Modify: `core/stream.go`
- Modify: `core/stream_stats.go`
- Modify: `module/gb28181/outbound_media.go`
- Modify: `module/sipgateway/call_session.go`
- Modify: `config/runtime/manager.go`
- Modify: `config/runtime/source_consul.go`
- Modify: `config/runtime/source_redis.go`
- Modify: `module/api/config.go`
- Modify: `module/metrics/collector.go`
- Test: corresponding package tests and new `*_bench_test.go` files beside changed packages

**Interfaces:**
- `Stream.WriteFrame` keeps media ordering while using stable publisher identity and a narrower critical section.
- Runtime source reads/writes retain serialization and immutable snapshots, but unchanged versions do not repeat full application work.
- Configuration redaction covers URL userinfo and error values; metrics expose bounded labels or a documented opt-in stream detail mode.

- [ ] **Step 1: Add failing behavior tests** for URL/userinfo redaction, unchanged refresh, publisher identity hot path, and bounded metric labels.
- [ ] **Step 2: Add baseline benchmarks** for `WriteFrame`, ring readers, RTMP/RTSP/RTP output and config refresh; capture before numbers.
- [ ] **Step 3: Implement one optimization at a time**, running its focused tests after each change; preserve packet timing and byte counts.
- [ ] **Step 4: Run race tests and benchmarks** with `go test -bench='BenchmarkRingReader|BenchmarkRTMPConn' -benchmem ./pkg/util ./module/cluster` plus focused new benchmarks.
- [ ] **Step 5: Update configuration/security/metrics docs** and record measured limits without claiming unmeasured capacity.
- [ ] **Step 6: Commit** with the required `im-pingo` author.

### Task 5: Console, protocol matrix, and release verification

**Files:**
- Modify: `module/api/console.html`
- Modify: `module/api/protocol_lab.go`
- Modify: `module/api/config.go`
- Modify: `module/api/recording.go`
- Modify: `module/api/console_management_test.go`
- Modify: `module/api/protocol_testlab_api_test.go`
- Modify: `module/api/config_api_test.go`
- Modify: `module/api/recording_test.go`
- Modify: `agent-manifest.json`
- Modify: `llms.txt`
- Modify: `llms-full.txt`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/api/openapi.yaml`
- Modify: `docs/recipes/protocol-test-lab.md`
- Modify: `docs/recipes/runtime-config-sources.md`
- Modify: `docs/recipes/recording-dvr-management.md`
- Modify: `docs/TECHNICAL-RISKS.md`

**Interfaces:**
- Console group hierarchy has System-level Config/Security and Workspace-level media/lab/storage views.
- Config exposes complete redacted effective/desired documents, schema, source details, validation, apply/refresh state and pending restart paths for file/http/https/consul/redis.
- Protocol lab responses expose separate source/target stream, codec, audio/video/RTCP counters, generation and cross-protocol playback links.
- Storage distinguishes disabled record module, empty/incomplete output, complete playable recording, and DVR availability.

- [ ] **Step 1: Add failing DOM/API contract tests** for navigation groups, all config fields/source kinds, redaction, lab counters/links, and disabled storage.
- [ ] **Step 2: Run focused API tests and browser smoke checks** to verify the failures represent missing behavior.
- [ ] **Step 3: Implement the smallest UI/data changes** and keep labels tied to actual API capability states.
- [ ] **Step 4: Run local SIP/GB28181 publish and receive labs**, then test WHEP, HTTP-FLV/TS/fMP4, HLS/DASH and recording playback where codecs allow it.
- [ ] **Step 5: Run the complete verification matrix:**

```bash
go test ./...
CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge
CGO_ENABLED=1 go test -tags audiocodec -race -coverprofile=coverage.out -covermode=atomic ./...
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
git diff --check
jq empty agent-manifest.json
```

- [ ] **Step 6: Review the final diff against the spec and risk table**, run `git status`, and verify no recordings, secrets, binaries or generated profiles are staged.
- [ ] **Step 7: Commit** only after all commands above have fresh successful output, then push/merge only when CI is green.
