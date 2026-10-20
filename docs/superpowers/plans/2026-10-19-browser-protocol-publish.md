# Browser Protocol Publishing Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans for each owned implementation task. Independent protocol adapters run in parallel under superpowers:dispatching-parallel-agents; the primary agent owns UI, documentation and integration.

**Goal:** Publish real browser camera/microphone media through the SIP and GB28181 modes of `/console/publish`.

**Architecture:** WHIP transports captured H.264/G.711 to a temporary source stream. Optional `source_stream_key` on the existing protocol publish request selects that generation's media instead of sample generation, using the normal loopback protocol signaling and receivers.

**Tech Stack:** Go 1.26, existing Pion WebRTC/RTP, SIP/GB28181 adapters, embedded Console JavaScript, Chrome/chromedp regression tests.

**Spec:** `docs/superpowers/specs/2026-10-19-browser-protocol-publish-design.md`.

## Global Constraints

Preserve prior uncommitted changes. No new dependencies or authentication bypass. Keep generated protocol-lab publishing when the source field is omitted. Never modify a source publisher. No commits or external deployment. Native FFmpeg compilation was blocked by the host Xcode license during the preceding task; fresh builds in this task now succeed. The new path must also work with a portable build. Do not run cluster binaries concurrently.

## Review Focus

- Stop or protocol switch during pending WHIP/lab POST: clean late resources and protect the next run.
- Browser lacks H.264 or G.711: explicit failure without fallback to unsupported media.
- Source publisher is replaced/ends or reader is overwritten: stop the bridge and release subscriber/socket ownership.
- Incorrect source key/codec or same source and destination: reject before signaling.
- Actual camera and microphone content: verify both target media tracks, not only successful signaling.

### Task 1: SIP source-backed publishing

Files: `module/sipgateway/status.go`, `module/sipgateway/lab.go`, focused source bridge helper/tests in that module.

Interface: optional `SourceStreamKey string` with JSON `source_stream_key` on request and snapshot; only publish mode. Read H.264 plus matching G.711 from the captured source generation and send actual RTP/RTCP via the existing fake-device sockets. Reuse core startup/subscriber conventions.

- [x] Add failing source-content, validation and cleanup regressions using the existing local gateway test harness.
- [x] Implement bounded source admission and transport forwarding, with cancellation and generation ownership.
- [x] Run focused and complete portable module tests; report native blockers accurately.

### Task 2: GB28181 source-backed publishing

Files: `module/gb28181/session.go`, `module/gb28181/lab.go`, focused source bridge helper/tests in that module.

Interface: the same optional source field; publish mode accepts H.264 plus G.711A. Send the source generation as PS/RTP through normal registered-device/INVITE reception. Source-to-target key mapping remains authoritative for loopback publish.

- [x] Add failing real input-content, invalid-source and source/stop cleanup regressions.
- [x] Implement generation-bound source forwarding with subscription admission, PS initialization and existing transport cleanup.
- [x] Run focused and complete portable module tests.

### Task 3: Shared capture UI and integration

Files: `module/api/console.html`, `module/api/static/console-media.js`, extracted `module/api/static/console-publish.js`, Console browser tests, API request contract tests as needed.

- [x] Add browser regressions requiring visible device selectors/preview in SIP and GB28181 mode and actual target audio/video progression.
- [x] Move camera/microphone fields to shared UI; use strict H.264/G.711 for protocol modes; create a temporary WHIP source and then a publish request containing `source_stream_key`.
- [x] Own resources per run, clean late responses, monitor protocol state, and preserve preview/stats/retry behavior.
- [x] Verify both protocol modes and unchanged WHIP behavior with fake browser camera/microphone.

### Task 4: Document and validate

- [x] Update manifest, llms summaries, README pair, protocol lab recipe and OpenAPI source field semantics.
- [x] Run final changed-package tests/race where usable, portable build/full suite, and mandatory documentation checks; record the missing-covdata coverage blocker.
- [x] Inspect the running 8090 instance and validate a current build without disrupting unrelated live publishers; record the activation constraint.
- [ ] Activate the built update on 8090 after permission to interrupt the active sip/live publisher, then reload the user-facing page.

## Verification evidence (2026-10-19)

- Browser capture uses the same getUserMedia path with fake devices: SIP PCMA,
  SIP PCMU and GB28181 PCMA each passed two complete cycles on the same page,
  including unique temporary sources, decoded 640x480 target video, advancing
  audio/video, normal HTTP 200 protocol Stop, ended tracks, released publishers
  and reusable RTP/RTCP ports. No physical camera/microphone was recorded.
- Regressions first exposed early protocol startup before WHIP OnTrack, normal
  Stop racing source retirement, stale preview permission errors, and WHIP
  non-ASCII key rejection. They pass after the corresponding fixes. Stop now
  ends tracks immediately, bounds protocol deletion to five seconds and gates
  a restart on that cleanup before closing the source connection.
- SIP/GB source tests verify original payload bytes/timestamps, direct codec
  admission, the five-second startup bound, cancellation, replacement, EOF,
  ring overwrite and retained terminal media-reference cleanup.
- The final changed-module tagged race suites passed, and the final UI-only
  tagged race regression passed again after preserving WHIP non-ASCII keys.
- Portable and audiocodec server builds both succeeded. The full tagged race
  coverage attempt reported 3169 passing tests/subtests and 57 passing packages,
  with no failing or skipped test cases, but exited 1: the installed Go 1.26.0
  toolchain lacks `covdata` when processing the no-test internal/rtspwire
  package. A focused reproduction confirms normal tagged race compilation of
  that package succeeds and adding coverage produces the same missing-tool
  error. This is not a passed coverage gate. No host toolchain files were changed.
- Independent review identified retained GB source media in terminal history;
  the worker-joined reference-release fix was reviewed and race-tested. No
  unresolved findings remain in the reviewed feature scope.
- Documentation impact: Console behavior, optional publish request/snapshot
  fields and prerequisites are synchronized in the manifest, llms summaries,
  both READMEs, OpenAPI and the camera/protocol-lab recipes. No configuration,
  dependency, public port or authentication-policy change is introduced.
- Port 8090 still runs the earlier go-run process with an active `sip/live`
  publisher. Restart permission was requested because activation interrupts
  that stream; the running service has not been stopped without approval.

- Final portable full suite, with required Chrome checks and LF_BINARY pointing
  at the freshly built portable executable: exit 0, 56 passing packages and
  3042 passing tests/subtests; zero failing or skipped test cases. The one
  skipped package is internal/rtspwire, which has no test files. The new camera
  browser matrix and the origin-edge subprocess integration both ran.
- Final documentation tests, diff-aware documentation validation and whitespace
  checks all passed. Current build artifacts and raw test evidence remain in
  ignored `.lf-loop/`; no commits were made.
