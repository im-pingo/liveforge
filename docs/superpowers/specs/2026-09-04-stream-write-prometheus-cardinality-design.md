# Stream Write And Prometheus Cardinality Design

**Date:** 2026-09-04  
**Scope:** Close `PERF-001` (Stream write contention/capacity evidence) and `PERF-007` (Prometheus per-stream cardinality and scrape cost).

## Goal

Make the two remaining performance findings measurable and bounded under realistic local concurrency. The implementation must preserve stream-generation correctness, RingBuffer single-producer ordering, subscriber lifecycle behavior, and the existing opt-in Prometheus cardinality contract.

## Non-goals

- WebRTC Simulcast layer selection or automatic layer pausing.
- An unbounded per-stream metrics mode.
- A claim that a microbenchmark is a production deployment capacity guarantee.
- A redesign of protocol adapters or the RingBuffer consumer API.

## PERF-001: Stream write path

### Current constraints

`util.RingBuffer` is a single-producer, multi-consumer buffer. A write stores the slot and advances the cursor under `dataMu`; readers use the cursor and slot lock to detect overwrite. `Stream.writeFrameLocked` also updates publisher-generation media state, sequence headers, GOP metadata, statistics, and bitrate admission. Publisher attach/detach closes the generation boundary at the exact RingBuffer cursor and therefore must be ordered with frame writes.

### Design

1. Keep one logical frame writer per `Stream`, represented by a dedicated write mutex. The writer lock protects the complete ordered mutation of media info, sequence headers, startup state, GOP cache, statistics admission, and RingBuffer write.
2. Keep the lifecycle mutex for publisher/state/timer/generation ownership. `WriteFrameForPublisher` validates publisher identity and publishing state under the lifecycle mutex, then acquires the write mutex before performing the ordered frame mutation. Publisher attach, detach, close, and generation-boundary capture acquire the lifecycle mutex and then the write mutex before changing the generation or closing the ring. This fixed order prevents a boundary cursor from splitting a frame write.
3. Do not call external publisher, feedback, event, or transcode code while the write mutex is held. Existing `writeFrameLocked` behavior remains synchronous and allocation-free for the normal frame fixture.
4. Maintain an atomic total subscriber count alongside the protocol map. Max-subscriber admission, feedback-router updates, and idle-timeout decisions use the atomic total; protocol maps remain under the lifecycle mutex and are copied for API/metrics snapshots.
5. Preserve all existing rejection rules: nil frames, stale publisher IDs, destroyed streams, bitrate limits, GOP bounds, and RingBuffer closure.

### Correctness invariants

- A successful frame write advances the RingBuffer cursor exactly once and publishes the frame before that cursor is observable.
- A generation end cursor is an exclusive upper bound that never includes a frame from a later generation and never truncates a successful frame write.
- GOP replay order remains media/insertion order, begins at a keyframe, and respects configured frame, byte, duration, and GOP-count limits.
- Concurrent publisher replacement rejects frames from the old publisher after the replacement becomes active.
- Subscriber counts never become negative, exceed the configured per-stream limit, or diverge from the protocol map snapshot.

### Verification

Add focused tests and benchmarks in `core/`:

- Parallel publishers writing to independent streams with deterministic frame counts and final cursor assertions.
- Multiple readers consuming one stream while a publisher writes, checking monotonic reader cursors and exact overwrite accounting.
- Publisher replacement and stream close racing with writes, checking generation boundaries and stale-writer rejection under `-race`.
- Subscriber add/remove churn with max-limit and idle-timeout assertions.
- A benchmark matrix for 1, 8, and 32 streams, with 0, 4, and 16 readers per stream. Report allocations and frame throughput; use preallocated immutable payload fixtures.

`PERF-001` may be marked closed only when these tests pass in the default and `-race` suites and the benchmark is repeatable on the development machine. Results must be recorded as regression evidence with fixture and host details, not as deployment capacity.

## PERF-007: Prometheus cardinality and scrape path

### Current constraints

Per-stream metrics are disabled by default. When enabled, the collector supports an exact allowlist or a bounded lifecycle admission limit. Admitted stream keys are retained for the collector lifetime so churn cannot create more than the configured number of unique label values. Existing metric families also include protocol labels for subscriber counts.

### Design

1. Keep the existing external configuration and cardinality semantics: `stream_detail=false` disables detail metrics; allowlists are exact, de-duplicated, and sorted; `stream_detail_limit` is a hard upper bound; admitted keys are never silently replaced by newer streams.
2. Split admission mutation from scrape reads. Store the admitted key list in an immutable snapshot published through `atomic.Value` (or an equivalent atomic pointer). A collector gather loads one snapshot and does not hold the admission mutex while resolving streams or reading stats.
3. First-time admission takes a short mutex only while selecting stable streams and publishing a copied snapshot. Existing gathers never wait for the full stream-detail traversal. A failed or destroying lookup is skipped without mutating the snapshot.
4. Keep instance/state validation on every gather. A key is emitted only when `hub.Find` returns the same live stream instance and the stream is not destroying. This prevents stale metrics after stream replacement.
5. Preserve deterministic output order for admitted keys and allowlists. Do not add dynamic labels such as publisher ID, generation, subscriber ID, or remote address.

### Correctness and capacity invariants

- A single gather emits at most `stream_detail_limit` distinct `stream_key` values.
- Across the collector lifetime, the admitted-key set never exceeds `stream_detail_limit` in non-allowlist mode.
- Allowlist mode emits no key absent from the configured exact allowlist and emits no duplicate label set.
- Concurrent gathers, stream churn, and admission cannot race or expose a partially published key list.
- Subscriber protocol series remain bounded by the protocol map for each admitted stream.

### Verification

Add focused tests and benchmarks in `module/metrics/`:

- A large exact allowlist with duplicates and destroying/missing streams, asserting sorted unique output and the hard limit.
- Hundreds or thousands of active streams with a small and a large admission limit, asserting per-gather and lifetime cardinality bounds.
- Concurrent gather workers plus stream create/remove/replace churn under `-race`.
- Benchmarks for first admission, stable gather, allowlist gather, and concurrent gather throughput. Use deterministic in-process streams and avoid network scrape timing.

`PERF-007` may be marked closed only when the cardinality assertions and race tests pass and the benchmark records show the admission mutex is not held for the full read traversal. Results remain local regression evidence; deployment sizing still requires operator-specific scrape and stream load tests.

## Documentation impact

The implementation change must update, in the same change, `docs/TECHNICAL-RISKS.md`, `docs/PROGRESS.md`, `agent-manifest.json`, `llms-full.txt`, `llms.txt` when the top-level summary changes, and `README.md`/`README.zh-CN.md` if quick-start or operator-facing performance behavior changes. The risk entries must include the exact focused test/benchmark commands and clearly distinguish bounded regression evidence from production capacity guarantees.

## Acceptance checklist

- [ ] Focused RED tests exist for both risks and fail for the missing behavior.
- [ ] Minimal implementation passes focused tests and `-race`.
- [ ] Default package suite, tagged build/race suite, `go vet`, lint, and agent-doc gates pass.
- [ ] Benchmarks cover multi-stream/multi-reader ingress and large/concurrent metric gathers.
- [ ] Risk/status ledgers and AI-facing documentation agree with the implementation and verification output.
- [ ] Only `PERF-001` and `PERF-007` change status; Simulcast remains explicitly deferred.
