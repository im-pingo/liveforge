# Review Closure Implementation Plan

> For agentic workers: execute independent bounded modules with tests first, review their integration, and update verification evidence after each completed phase.

Goal: close the seven reproduced defects and implement the operational and usability improvements identified by the September 7 review with source, tests, and synchronized documentation.

Architecture: retain the existing Go module boundaries and embedded browser console. Reject failed admission before acknowledging sessions, bound untrusted input before allocation, preserve user edits independently of background reads, and expose explicit versioned management operations.

Tech stack: Go 1.26, net/http, Pion RTP/WebRTC, existing embedded JavaScript console, optional FFmpeg with the audiocodec build tag.

## Constraints

- Preserve existing untracked user files; do not commit binaries, local configuration, recordings, or secrets.
- Keep anonymous development compatibility and the public health endpoint; custom registered management handlers require the same authentication as default paths.
- Each behavioral fix has a reproducing test before production edits and focused regression tests after edits.
- Root owns cross-cutting documentation and integration. Parallel workers own disjoint source modules and report documentation facts.
- Keep portable builds functional and explicitly distinguish optional audio transcoding.
- Run tools/check-agent-docs_test.sh, focused tests, portable build, tagged build, and the tagged race/coverage baseline.

## Phase 1: Reproduced Defects

- [x] Cluster ingress: reject GB max-stream failures without panic, synchronously bind sockets and admit publishers, create first-time RTP destinations, roll back failed setup, and close owned session resources.
- [x] Management authorization: protect custom registered API routes irrespective of path prefix; verify default/custom paths with anonymous, viewer, operator, and admin requests.
- [x] H.264 parsing: bound SPS reference-cycle counts and stop immediately on malformed/truncated input in both parsing paths; retain valid high-profile behavior and fuzz coverage.
- [x] H.265 reassembly: bound bytes and fragments, reject discontinuous FU input, and release pending media on error; test recovery with a valid subsequent frame.
- [x] HTTP signaling: limit SDP and management request-body duration independently of streaming response lifetimes; test actual TCP reads and connection-slot recovery.
- [x] Console drafts: preserve dirty text across blur, polling, view changes, delayed Apply responses, and reauthentication; add explicit discard and navigation guards.

## Phase 2: Operational Improvements

- [x] Cache static schema and fetch desired documents only when their revision changes; retain pending desired overlays.
- [x] Add bounded management list pagination and console navigation without breaking legacy response contracts.
- [x] Add an invalidated recording index so frequent reads avoid a full recursive scan and per-file metadata reads.
- [x] Add config revision conflict detection, redacted history/diff, and rollback through the existing serialized source writer.
- [x] Add optional bounded persistent audit storage, query filters, and export without writing secret metadata.
- [x] Add login-expiry recovery and an explicit logout operation.
- [x] Split the embedded console script along meaningful ownership boundaries and add English/Chinese interface selection.
- [x] Add common configuration controls and an application diff while preserving full-document editing.
- [x] Implement and verify the currently deferred Simulcast layer-selection and layer-pause contract, including cross-protocol selection behavior.

## Phase 3: Verification And Review

- [x] Add deterministic malformed-input and resource-admission regression gates plus bounded fuzz targets.
- [x] Add an explicit required-browser CI mode and a reproducible bounded concurrent/backpressure/long-run gate.
- [x] Review each changed module for resource ownership, concurrency, response compatibility, and documentation impact; fix findings and rerun affected checks.
- [x] Synchronize agent-manifest.json, llms.txt, llms-full.txt, both READMEs, API/schema contracts, recipes, risk records, and build/test contract where applicable.
- [x] Run final uncached package/race suites, builds, documentation checks, browser verification, and leave a working local URL for the console.

## Evidence

Baseline: the review ran 54 package groups with go test ./... -count=1 successfully, go vet ./..., documentation checks, tagged FFmpeg build, and tagged race coverage (mostly cached). Temporary probes reproduced custom-path authentication bypass, GB max-stream panic, first RTP push rejection, SPS CPU amplification, unbounded H.265 FU buffering, slow SDP connection-slot retention, and draft overwrite.

2026-09-07 iteration: permanent custom-route authorization/logout and actual TCP body-timeout regressions passed. Config revision tests first reproduced stale Apply being accepted and missing ETag/history routes; manager/API focused tests now pass with race enabled, including simultaneous Apply, comment-only revisions, rollback preconditions, redacted history, revision non-reuse, defensive copies, and history count/byte bounds. Final integrated verification and documentation synchronization remain open.

Cluster/media: focused no-tag and tagged race passed; do not run cluster test binaries concurrently (legacy fixed ports31000/31020). Bounded fuzz passed with2workers/5seconds each: SPS211404, avcC277791, H265FU5451 executions. Further peer tests fixed RTP pull codec advertisement/mapping, unspecified peer addresses, failed-response rollback, and copied GB audio ownership.

Recording index: no-tag/tagged race, vet and docs passed;1000-recording benchmark on Apple M1 Pro was20.86us/147457B/1alloc warm versus116.72ms/2714040B/25050alloc rescanning. This is a fixed fixture, not capacity proof. Console required-Chromium suite passed after script separation, multilingual controls, paginated lists, drafts and history; final refinements and integration still in progress.

Simulcast design accepted for implementation: max3 private isolated RID layers per parent, deterministic canonical layer for all existing protocols, explicit session-time WHEP selection and family-wide subscriber admission. Automatic pause means server-local processing suspension of unused noncanonical layers while still reading RTP; arbitrary upstream sender pause/congestion-adaptive switching is not claimed. Pion's3RID peer E2E test passed before implementation. Full feature tests and documentation remain pending.

Final audit/Console focused verification: required Chromium tagged race TestConsole passed66 tests with0 skips in78.409s; tagged race audit tests passed, along with tagged API vet. Eight screenshots cover Config/Security at1440/390px in both languages, with open diff, no horizontal overflow or offscreen controls. The config Apply fixture now waits for initial source loading before testing an uncontended write; five repetitions of Apply/revision/history/rollback race regressions pass. This fixture correction does not change runtime/API behavior; documentation impact is verification evidence only. Further custom-route permission and rate-limit audit integration is under independent review.

Integrated follow-through: custom mux/RBAC, granular GB28181 permissions and rate-limit mutation audit pass tagged/untagged race. Simulcast real3RID H264/VP8, selected WHEP, canonical fMP4+FFmpeg decode, shared Opus, pause/resume, hot policy, generation replacement and joined shutdown pass. Shared VP8 extended-descriptor and bounded-frame fix includes ordinary WHIP transport and501877 five-second fuzz executions. Real-instance mobile inspection exposed long file-version/hash overflow absent from short fixtures; expanded browser regression was RED, responsive wrap fix is GREEN. No API/config behavior changes arise from the remaining lint variable/test annotation cleanup.

First full untagged run passed54 packages/2779 tests with one missing-binary test skip. Enabling LF_BINARY in the tagged race/coverage run exposed testkit origin-edge startup/config/API-envelope defects (now fixed and the real two-node topology passes), plus a converted-Opus pacing contention regression under investigation. Remaining tagged packages passed, including required Console/media browsers; final all-green baseline and gate are still pending.

Pacing follow-through: deterministic tests first reproduced immediate consecutive sends after timer oversleep and video-ahead audio buffering. Per-packet converted Opus now preserves packet-duration spacing and rebases at wakeup. Tagged race/coverage push package passes4.396s; the real RTP test deliberately queues receiver reads while verifying timestamps, avoiding the previous scheduler-dependent arrival assertion. Current-source Playwright smoke passes Config/Security at1440/390px in both locales, preserves drafts across views, has no page overflow/errors, and stores only the locale preference. Independent module reviews and new-diff lint pass; final baseline/gate execution is ongoing.

The default60-second regression gate completed successfully: ten repeated concurrency/cluster rounds, focused revision/RBAC/Simulcast/index/body-timeout/audit race checks, four bounded fuzz targets, required Console browser suite75.691s, and three-scenario cross-protocol Chromium soak191.454s. Tagged and portable final full-suite runs remain the last verification step.

Portable origin-edge follow-through: the unpaced looping test publisher sent84481 frames/82.2MB in500ms, overwrote the bounded origin ring, and retired the relay generation before playback. Enabling existing real-time pacing preserves the original media assertion and passes five repeated portable topologies (162 video/237 audio frames each), the full portable cluster package (7.055s), and tagged race cluster package (8.202s). Generated configs retain production GOP bounds and stream discovery reads the management envelope; CI now supplies its just-built LF_BINARY. Full suites are being rerun against this final source.

The portable full suite then passed54 packages/2809 tests with zero failures or skips. The next tagged full suite exposed an intermittent existing fMP4 fixture failure: externally injected terminateOverwrite canceled pumps before notifying readers, permitting clean EOF and a137-byte tail flush. Production termination originates inside a counted pump, which prevents the same early frames-channel closure; no equivalent production playback failure was reproduced. A deterministic cancellation-boundary test first failed. Terminal notification now precedes pump cancellation while retaining the original pending-fragment discard assertion. Focused repeated race checks and final integrated verification are in progress.

HTTP follow-through:500 tagged race repetitions of input/overwrite/clean-end cases and the complete HTTP package passed. The next full tagged race/coverage baseline passed54 packages/2937 tests with zero failures or skips and78.0% statement coverage. Portable follow-through then exposed an immediate Simulcast status assertion racing the final successful sample counter: loopback RTP can arrive before WriteSample returns and RecordVideo increments. The test now waits up to2seconds for the same >=2 count, fails RID/HTTP/JSON errors immediately, and still compares actual headers and media bytes for each layer. No production behavior or API contract changed in this fixture correction. Twenty portable repetitions passed; final verification continues.

## Final Verification

- Final portable full run: `LIVEFORGE_REQUIRE_BROWSER=1 LF_BINARY=/tmp/liveforge-review-portable go test -json ./... -count=1 -timeout=15m`, exit0,54 packages/2810 tests and subtests, zero failures or skips. Log: `/tmp/liveforge-review-portable-final.jsonl`.
- Tagged full baseline: `LIVEFORGE_REQUIRE_BROWSER=1 LIVEFORGE_PROTOCOL_MATRIX_SOAK=15s LF_BINARY=/tmp/liveforge-review-native CGO_ENABLED=1 go test -json -tags audiocodec -race -coverprofile=coverage.out -covermode=atomic ./... -count=1 -timeout=15m`, exit0,54 packages/2937 tests and subtests, zero failures or skips,78.0% statement coverage. Log: `/tmp/liveforge-review-tagged-complete.jsonl`. The subsequent test-only Simulcast synchronization adjustment passed20 portable and5 tagged race repetitions; production source did not change after the green tagged baseline.
- The default regression gate passed repeated concurrency/backpressure/admission checks, four bounded fuzz targets, required Console tests, and three60-second cross-protocol browser scenarios. HTTP terminal publication also passed500 repeated race checks and the full tagged HTTP package.
- Current no-CGO and audiocodec builds passed. The release matrix's Linux/macOS amd64/arm64 portable compilation passed during this iteration. Tagged/untagged vet, new-diff lint (zero issues), matching embedded/config schemas, agent-doc self-tests, diff-aware docs checks, and whitespace checks passed.
- Rebuilt local native server is healthy at `http://127.0.0.1:18090/console`, with temporary loopback-only development configuration. Current-instance Playwright checks passed eight1440/390px English/Chinese Config/Security cases, draft retention across views, no browser page errors, and no stored credentials. Wide tables scroll inside their own containers; the page itself does not overflow.

All planned review items are implemented and verified within these documented bounds. No commit, release, deployment, or production-stream operation was performed. Longer real-network, platform-runtime, and deployment-capacity testing remain environment-specific validation, not claims made by this local regression run.
