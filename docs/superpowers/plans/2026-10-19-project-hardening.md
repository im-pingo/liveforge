# LiveForge project hardening and verification

**Goal:** Fix reproduced input, shutdown and test-reliability failures while preserving media/module boundaries and the existing uncommitted Loop executor.

**Method:** Source review, failing behavioral regressions, minimal fixes, independent review, focused and whole-repository verification. The user has authorized both analysis and repair; work continues in the current `codex/loop-executor` branch.

## Scope and decisions

- RTSP server and relay client share a standard-library framing reader in `internal/rtspwire`: 8 KiB per start/header line, 64 KiB total headers including terminators, 1 MiB body. Reject invalid/duplicate Content-Length and unsupported Transfer-Encoding before allocation or body reads. Preserve a following pipelined message.
- RTSP connections get a 60-second wait for the next packet and a 10-second completion budget after its first byte. Clear read deadlines before handing control to media/session handlers. Incomplete control input must release the global connection slot.
- H.264 input parsing in `pkg/rtp` validates STAP lengths and FU sequence/identity, caps raw NAL/FU data at 16 MiB and fragments at 16384, and preserves valid SPS/PPS/media output. Error recovery must not mix old fragments into a new frame.
- Notification WebSockets restore the library's matching-host/port Origin policy, still allow native clients without Origin, and permanently close admission when the sender closes. This is host validation, not a scheme comparison or a replacement for management authentication.
- RTMP/HLS correctness fixtures use existing real-time pacing; HLS waits for actual GOP/audio readiness. The audit permission fixture explicitly applies its intentionally unsafe mode independently of process umask.
- No new dependency, deployment, live-server restart, commit or release. Native/compiler prerequisites are checked honestly; package-level race success is distinct from FFmpeg/browser acceptance.

## Work items

- [x] Reproduce oversized/ambiguous RTSP request and peer-response framing with bounded probes; implement shared framing and rerun focused tests.
- [x] Reproduce a stalled real TCP RTSP body, add control read budgets, verify connection release and existing RTSP sessions.
- [x] Reproduce malformed H.264 panics and invalid FU admission; fix and pass complete RTP tests, tagged/race and two bounded fuzz targets.
- [x] Reproduce WebSocket admission after Close and cross-host handshakes; fix and verify real event delivery, close races, idempotence and origin-less clients.
- [x] Reproduce RTMP sub-millisecond timing, HLS segment eviction and restrictive-umask audit-fixture failure; fix fixture assumptions without weakening safety/frame assertions.
- [x] Synchronize manifest, AI navigation, README pair, recipes, OpenAPI and current risk record with exact source behavior.
- [x] Obtain independent review of the current hardening diff, including newly added files; address substantive findings with regressions.
- [x] Build current binaries, run changed modules and the full portable suite with LF_BINARY, attempt native baseline and record blockers, then run documentation/diff checks.

## Evidence and remaining assessment

This work does not certify every media input or deployment topology. Historical `closed` entries in the risk register are bounded regression claims; this review already found uncovered H.264 and RTSP inputs. Target-platform long-duration capacity, fanout and scrape-pressure measurements remain separate from these correctness fixes. Verification results are appended after the final run; no prior run is rewritten as passing.

## Independent review follow-up

The review reproduced two caller-level gaps beyond the NAL guard: one missing FU fragment stopped the RTSP UDP worker while leaving its session active, and 14,000 unmarked 1,400-byte NAL packets accumulated 19,656,000 bytes. WHIP also retained old media after codec errors. Shared access-unit assembly now bounds the outer receive path; actual RTSP TCP/UDP and WHIP regressions verify discard and recovery. RTSP additionally serializes codec state per publisher and isolates payload types without blocking Close during parsing. A deterministic two-track test reproduced concatenated media before the fix.

The independent-track regression also exposed a shared 90 kHz clock origin for audio and video: an Opus packet starting at timestamp 5000 after video timestamp 900000 became DTS 47,711,914 ms. RTSP now normalizes each track from its own initial timestamp using the existing codec RTP rate and emits complete audio samples without waiting for Marker. This is local clock normalization, not sender-report-based synchronization.

## Verification iterations

- The first complete portable run executed against the in-progress receive fixes: 54 package passes, two package failures and one package without tests. Four regressions failed: retained old codec fragments, and three WHIP/Simulcast fixtures that advanced timestamps per packet. The original failure log is retained locally at `.lf-loop/hardening-full-portable.jsonl`. Fixes release caller-held codec fragment storage and keep one RTP timestamp per access unit; assertions are unchanged.
- The second full portable run passed every package except `pkg/rtp`: `TestAccessUnitBoundsPendingAV1AndVP9CodecBytes` detected AV1 capacity 20,086,784 bytes despite an input length bound. Its log is retained at `.lf-loop/hardening-full-portable-final.jsonl`; the fix preflights byte input and replaces excess append capacity with one capped reserve, avoiding repeated grow/copy churn for small continuations. Retained capacity is bounded; transient legacy append allocation is documented separately. Final validation follows.
- The portable server and Loop executor build, whole-repository portable `go vet`, and both documentation checks passed. RTSP race passed after the initial integration changes.
- The tagged server build and `tools/check-review-regressions.sh` could not complete: this host rejects `runtime/cgo` compilation because the Xcode license has not been accepted. The gate passed its documentation step and focused core race tests, then failed to build the cluster package. No host license or security settings were changed. Earlier package-level RTP/RTSP/notify race success is not a substitute for native FFmpeg/browser baseline acceptance.

## Final verified state

Production code was frozen before the final run. Fresh portable server and Loop executor builds passed. `CGO_ENABLED=0 LF_BINARY=<absolute path to the fresh server> go test -json -count=1 ./...` passed with 56 tested packages, 2,990 passing tests/subtests, no failed or skipped tests, and one command package with no tests. Log: `.lf-loop/hardening-verified.jsonl`. The explicit binary enabled origin-edge subprocess coverage; portable Console browser checks ran without environment skips.

Final `CGO_ENABLED=1 go test -race ./module/rtsp ./module/webrtc ./module/notify ./pkg/rtp -count=1` passed for all four packages (12.122s, 15.284s, 11.751s and 3.058s). Final portable `go vet ./...`, `tools/check-agent-docs_test.sh`, `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh` and whitespace checks passed. H.264 payload/fragment fuzzing and shared-assembler fuzzing also exercised bounded development runs; final explicit regressions cover the capacity-reservation adjustment.

The final tagged server build still fails at `runtime/cgo` because of the host Xcode license. Full tagged FFmpeg/race/coverage and the complete native review/browser-soak gate remain unverified in this environment. This does not invalidate the passing portable or four-package race results, and does not claim native audio-transcoding acceptance. No commit, push, deployment or running-instance restart was performed.

## PR validation follow-up (2026-10-20)

The earlier verification above remains a historical record. PR #24 adds the browser camera workflow and English-only documentation alongside this hardening work. Its Linux lint gate identified cleanup, error-handling, integer-conversion and test-fixture findings. The follow-up expresses RTP wraparound ordering with equivalent unsigned half-range comparisons, retains bounded H.264 lengths without narrowing intermediate sizes, preserves the nested GB28181 header error, and makes test response cleanup and helper return contracts explicit. WebSocket Dial owns its transport response body; tests additionally close any non-nil returned body and deliberately ignore final connection-close errors after shutdown assertions.

Documentation impact: protocol acceptance, media limits, public API responses, configuration, build requirements and trusted local executor boundaries are unchanged. Narrow security annotations explain the existing validated H.264 length and parent-owned test fixtures or fixed internal Git commands; no lint rule is disabled globally. The executor prerequisite diagnostic now reads `requires Go 1.26 or newer`.

Focused portable executor tests and tagged race regressions for RTP, RTSP, WHIP, notification shutdown and SIP/GB28181 source forwarding passed after these edits. The required-browser camera publish/stop matrix and local golangci-lint v2.13.2 check against `origin/main` also passed. The complete Linux tagged race/coverage gate remains a separate PR check, rather than being inferred from those local results.

The next Linux run passed lint and security but exposed a concurrent GB28181 cleanup race: `TestGBLabPublishReceiverFailureTransitionsToFailedAndCleansUp` reached `failed`, then could not observe BYE at the Lab peer. The receiver monitor and Lab watcher both entered handler cleanup; the loser saw the early closed state and closed the peer before the winner sent BYE. A blocked-dialog regression reproduced premature peer closure before the fix. A separate handler-only per-session cleanup mutex now joins the winning teardown without changing the media-only Close contract or manager lock order. Both the new blocked-dialog regression and the original receiver-failure regression passed 30 tagged race iterations after the fix. The protocol-lab recipe and full agent context document this ordering and distinguish transmission from remote acknowledgement.
