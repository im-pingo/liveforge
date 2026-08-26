# Protocol Lab Simulator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add persistent in-process SIP and GB28181 fake-device sessions that perform real signaling/media loopback and are controllable from the Console.

**Architecture:** Keep protocol lifecycle state inside `module/sipgateway` and `module/gb28181` Lab Managers. API handlers expose snapshots and lifecycle commands, while Console pages render controls and reuse existing stream playback functions. Media remains deterministic and native-dependency-free.

**Tech Stack:** Go 1.26, `sipgo`, Pion RTP/RTCP, existing `core.StreamHub`, existing PS muxer, Go HTTP API, embedded Console HTML/JavaScript.

**Spec:** `docs/superpowers/specs/2026-08-26-protocol-lab-simulator-design.md`

## Global Constraints

- Keep SIP lab media audio-only because the current SIP gateway is audio-only.
- Keep GB28181 media H.264 in PS over RTP payload type 96.
- Bind simulator endpoints to loopback and release all sockets/ports on stop.
- Do not require FFmpeg, Docker, or an external PBX/GB28181 platform.
- Preserve existing self-test endpoints and existing protocol APIs.
- Update source tests and all required AI-facing/user-facing documentation in the same change.
- Use `im-pingo <cczjp89@gmail.com>` for commits.

### Task 1: Define Session Contracts And Red Tests

**Files:**
- Modify: `module/sipgateway/status.go`
- Modify: `module/gb28181/session.go`
- Test: `module/sipgateway/lab_test.go`
- Test: `module/gb28181/lab_test.go`

**Interfaces:**
- Produce `LabMode` values `publish` and `receive` plus immutable lab session snapshots with identity, stream key, state, direction, media counters, and timestamps.
- Produce manager contracts for start, list, and idempotent stop.

- [ ] Write failing tests for SIP and GB28181 start validation, list visibility, duplicate identity rejection, and idempotent stop.
- [ ] Run focused tests and confirm they fail because the Lab Manager contracts do not exist.
- [ ] Add the minimal shared snapshot and error contracts without implementing transport.
- [ ] Run focused tests again and confirm contract-level failures now identify missing lifecycle behavior.

### Task 2: Implement SIP Fake Device Publish And Receive

**Files:**
- Create: `module/sipgateway/lab.go`
- Test: `module/sipgateway/lab_test.go`
- Modify: `module/sipgateway/module.go`
- Modify: `module/sipgateway/gateway.go`
- Modify: `module/sipgateway/status.go`

**Interfaces:**
- Add `StartLabSession(ctx, request)`, `ListLabSessions()`, and `StopLabSession(id)` to the SIP gateway provider.
- Publish mode must create a real inbound SIP call and send RTP/RTCP into the gateway-created stream.
- Receive mode must create a fake SIP endpoint that accepts the gateway outbound INVITE and counts received RTP/RTCP.

- [ ] Add failing integration tests that wait for a published stream and non-zero RTP counters, then stop and verify stream/session cleanup.
- [ ] Implement loopback SIP UA/server signaling, deterministic PCMA/PCMU RTP generation, RTCP reports, and receive counters.
- [ ] Connect lifecycle cleanup to gateway Hangup/stream publisher removal and make repeated stop safe.
- [ ] Run `go test ./module/sipgateway -run 'Lab|SelfTest' -v` and then `go test -race ./module/sipgateway`.

### Task 3: Implement GB28181 Fake Device Publish And Receive

**Files:**
- Create: `module/gb28181/lab.go`
- Test: `module/gb28181/lab_test.go`
- Modify: `module/gb28181/module.go`
- Modify: `module/gb28181/api.go`
- Modify: `module/gb28181/session.go`

**Interfaces:**
- Add `StartLabSession(ctx, request)`, `ListLabSessions()`, and `StopLabSession(id)` to the GB28181 module provider.
- Publish mode must register a fake device/channel and send PS/RTP media into a real GB28181 receiver.
- Receive mode must answer the server's live-play INVITE and count PS/RTP/RTCP received by the fake device.

- [ ] Add failing tests for fake registration, channel visibility, live publish stream creation, receive-mode INVITE acceptance, non-zero counters, and teardown.
- [ ] Implement fake device SIP handlers for REGISTER, INVITE, ACK, BYE, and Keepalive/Catalog state.
- [ ] Implement deterministic H.264 frame creation, existing PS muxing, RTP packetization, RTCP reports, and receiver counters.
- [ ] Run `go test ./module/gb28181 -run 'Lab|SelfTest' -v` and then `go test -race ./module/gb28181`.

### Task 4: Add Authenticated Lab APIs And Cross-Protocol Metadata

**Files:**
- Modify: `module/api/routes.go`
- Modify: `module/api/sipgateway.go`
- Modify: `module/api/protocol_testlab.go`
- Create: `module/api/protocol_lab.go`
- Test: `module/api/protocol_lab_api_test.go`
- Modify: `module/api/rbac.go`
- Modify: `docs/api/openapi.yaml`

**Interfaces:**
- Add list/start/delete routes under `/api/v1/sipgateway/lab/sessions` and `/api/v1/gb28181/lab/sessions`.
- Return stable `{code,message,data}` management envelopes containing session snapshots and resolved stream keys.
- Include cross-protocol playback metadata for active published streams using the same listener discovery contract as `/api/v1/server/info`.

- [ ] Add failing route/RBAC tests for viewer list, operator start/stop, invalid body, unavailable module, and stream metadata.
- [ ] Implement request decoding, error mapping, provider lookup, and playback URL derivation.
- [ ] Update OpenAPI schemas and verify unknown fields, missing fields, and credentials are handled safely.
- [ ] Run `go test ./module/api -run 'Lab|Protocol|Management'`.

### Task 5: Add Console Device Controls And Cross-Protocol Playback

**Files:**
- Modify: `module/api/console.html`
- Modify: `module/api/console_management_test.go`
- Modify: `module/api/handler_test.go`

**Interfaces:**
- Add SIP and GB28181 Lab forms with publish/receive mode, identity, stream/channel fields, start/stop actions, and live counters.
- Add safe action handlers that call the new API routes and show active published streams in the existing player.
- Keep real devices/calls/sessions and local lab sessions distinguishable.

- [ ] Add failing HTML/source tests for controls, endpoint calls, permission attributes, and player actions.
- [ ] Implement responsive lab controls, session tables, polling, stop confirmation, and cross-protocol preview buttons.
- [ ] Run focused Console tests and any available Chromedp tests.

### Task 6: Synchronize Documentation And Run Full Verification

**Files:**
- Modify: `docs/recipes/protocol-test-lab.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `llms.txt`
- Modify: `llms-full.txt`
- Modify: `agent-manifest.json`
- Modify: `docs/PROGRESS.md`
- Modify: `AGENTS.md` only if verification/documentation contract changes

- [ ] Document exact local workflows for SIP/GB28181 publish, receive, cross-protocol playback, stop, and cleanup.
- [ ] Run `gofmt` and `git diff --check`.
- [ ] Run focused tests, `go test -race ./...`, `go vet ./...`, both builds, tagged baseline, and docs checks.
- [ ] Run targeted protocol/API race stress and verify no generated media, binaries, secrets, or coverage files are staged.
- [ ] Commit with `im-pingo` and inspect author, stat, and clean worktree.
