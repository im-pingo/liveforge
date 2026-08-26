# Task 3 Report: GB28181 Fake Device Publish And Receive

Date: 2026-08-26
Status: implemented and verified

## Changed files

- `module/gb28181/lab.go`: persistent in-process GB28181 fake-device manager and session transport.
- `module/gb28181/lab_test.go`: real loopback publish, custom stream-key, receive, Catalog, duplicate identity, idempotent stop, port release, and media-counter tests.
- `module/gb28181/module.go`: initialized provider lifecycle and `StartLabSession`, `ListLabSessions`, `StopLabSession` wiring; module-close cleanup.
- `module/gb28181/handler.go`: one-time internal Call-ID to lab stream-key binding so lab sessions can honor a requested stream key without changing ordinary device behavior.
- `module/gb28181/session.go`: corrected standalone-manager documentation after transport wiring.
- `module/gb28181/api_test.go`: removed the internal-test import cycle with `module/api` by invoking the GB handlers registered on `core.Server` directly.
- `docs/PROGRESS.md`: marked persistent GB28181 lab support available only after focused and race verification.
- `task-3-report.md`: this report.

Existing Task 4/API, architecture, README/manifest/recipe, and other unrelated worktree changes were preserved and are not part of the focused Task 3 commit.

## TDD red/green commands

The original lab tests were added before the transport implementation and first failed because the Module provider methods were unavailable. During the final signaling extension, the Catalog assertion was run red with:

```text
go test ./module/gb28181 -run 'TestGBLabReceiveAcceptsRealLivePlayAndCountsMedia' -count=1 -v
module/gb28181/lab_test.go:146:43: lab.catalogSent undefined
FAIL
```

After adding Catalog tracking and sending Catalog in receive mode, the focused test passed. The custom stream-key regression also first failed because the GB handler always created `gb28181/<channel>`, then passed after the Call-ID binding was implemented.

Final green verification:

```text
go test ./module/gb28181 -run 'Lab|SelfTest' -v -count=1
PASS; ok github.com/im-pingo/liveforge/module/gb28181 0.533s

go test -race ./module/gb28181 -count=1
PASS; ok github.com/im-pingo/liveforge/module/gb28181 2.463s

go test ./module/api -run 'Lab|SelfTest' -count=1
PASS; ok github.com/im-pingo/liveforge/module/api 0.654s

go test ./...
PASS; all packages passed

tools/check-agent-docs_test.sh
agent-docs: all checks passed

CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
agent-docs: all checks passed
```

## Signaling and media behavior

Publish mode binds a fake device to loopback, performs REGISTER, Keepalive, Catalog, and a real server INVITE/200/ACK exchange, then sends deterministic H.264 keyframes packed by `pkg/muxer/ps` as RTP payload type 96. The existing GB28181 handler creates the real `Publisher` and `RTPReceiver`; the fake sends RTP packets and RTCP Receiver Reports. BYE closes the server-side live session.

Receive mode binds an independent loopback fake SIP peer and media sender, registers the fake device, sends Keepalive and Catalog through the real GB28181 handler, accepts LiveForge's real live-play INVITE with 200/SDP, receives ACK, and sends H.264 frames from the existing stream as PS/RTP. The fake receiver demuxes PS through the existing `Publisher` path and counts PS frames, RTP packets/bytes, and RTCP packets.

Device and channel identities are validated, active duplicate device identities are rejected, and custom requested stream keys are honored only through an internal one-time Call-ID mapping. No external platform, FFmpeg, Docker, or persistent HTTP route was added by Task 3.

## Cleanup

`StopLabSession` is idempotent. Session stop cancels the session context, waits for an in-progress start, sends dialog BYE where applicable, unregisters the fake device, closes RTP/RTCP/media and peer sockets, terminates SIP user agents/transactions, waits for media and peer goroutines, removes the created publisher/session, and frees the real GB RTP port pair. `Module.Close` stops every persistent lab before registry and normal module shutdown. Tests bind all simulator sockets to `127.0.0.1`, verify UDP ports can be rebound after stop, preserve a pre-existing receive source publisher, and verify the registry/session teardown.

## Concerns

- sipgo emits an intermittent `WARN UDP ref went negative on try close` during transaction shutdown. It does not prevent cleanup assertions or cause test failures, and the full package, focused, and race suites pass. It appears to be dependency-level close reference accounting and should be monitored separately.
- Persistent HTTP management routes are Task 4 scope. The current worktree contains separate uncommitted Task 4 API changes; Task 3 does not add or commit those routes.
