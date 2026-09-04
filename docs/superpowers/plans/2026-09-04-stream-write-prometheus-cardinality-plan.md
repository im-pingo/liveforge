# Stream Write And Prometheus Cardinality Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Close `PERF-001` and `PERF-007` with lower shared-lock contention, bounded cardinality, realistic concurrent regression tests, and synchronized documentation.

**Architecture:** `Stream` gets a write-sequence mutex that serializes frame/generation-owned state and RingBuffer publication. Lifecycle mutations acquire the write mutex before the existing lifecycle mutex; read-only startup/GOP snapshots use the write mutex so they observe complete frame sequences. Subscriber totals are maintained atomically. `metrics.Collector` publishes an immutable admitted-key snapshot through `atomic.Value`, keeping the admission mutex only around first-time selection and never around per-stream metric traversal.

**Tech Stack:** Go 1.26, `sync.Mutex`, `sync/atomic`, Prometheus client, existing RingBuffer and StreamHub, Go tests/benchmarks/race detector.

**Spec:** `docs/superpowers/specs/2026-09-04-stream-write-prometheus-cardinality-design.md`

## Global Constraints

- WebRTC Simulcast remains the only deferred runtime feature.
- Preserve RingBuffer single-producer ordering, generation boundaries, overwrite handling, GOP bounds, publisher identity checks, subscriber lifecycle leases, and metric label limits.
- Do not add publisher, generation, subscriber-ID, address, or other unbounded labels.
- Use preallocated immutable benchmark payloads; benchmark numbers are regression evidence, not deployment capacity guarantees.
- Every source change updates the relevant risk/status and AI-facing documentation in the same change.
- Run `gofmt` and focused tests before broader suites; do not commit coverage files, binaries, recordings, or untracked user artifacts.

---

### Task 1: Split Stream write sequencing from lifecycle reads

**Files:**
- Modify: `core/stream.go`
- Test: `core/stream_test.go`, `core/stream_capacity_test.go`

**Interfaces:**
- Produces `Stream.writeMu sync.Mutex` as the single sequence lock for frame writes, generation transitions, GOP state, sequence headers, media info, and ring publication.
- Produces `Stream.TotalSubscribers() int` backed by an `atomic.Int64` field named `subscriberTotal`.
- Existing exported Stream methods keep their signatures.

- [ ] **Step 1: Write the failing subscriber-total test**

Add to `core/stream_capacity_test.go`:

```go
func TestStreamTotalSubscribersTracksProtocolChurn(t *testing.T) {
    s := NewStream("live/subscriber-total", newTestStreamConfig(), config.LimitsConfig{MaxSubscribersPerStream: 4}, NewEventBus())
    if got := s.TotalSubscribers(); got != 0 {
        t.Fatalf("initial total subscribers = %d, want 0", got)
    }
    if err := s.AddSubscriber("rtmp"); err != nil { t.Fatal(err) }
    if err := s.AddSubscriber("hls"); err != nil { t.Fatal(err) }
    if err := s.AddSubscriber("rtmp"); err != nil { t.Fatal(err) }
    if got := s.TotalSubscribers(); got != 3 { t.Fatalf("total subscribers = %d, want 3", got) }
    s.RemoveSubscriber("rtmp")
    s.RemoveSubscriber("hls")
    s.RemoveSubscriber("rtmp")
    if got := s.TotalSubscribers(); got != 0 { t.Fatalf("final total subscribers = %d, want 0", got) }
}
```

- [ ] **Step 2: Run the new test and verify the expected RED failure**

Run:

```bash
go test ./core -run '^TestStreamTotalSubscribersTracksProtocolChurn$' -count=1
```

Expected: compilation fails because `TotalSubscribers` and the atomic total do not exist yet.

- [ ] **Step 3: Add the write mutex and atomic subscriber total**

In `Stream`, add:

```go
writeMu         sync.Mutex
subscriberTotal atomic.Int64
```

Update `addSubscriberLocked`, `removeSubscriberForGeneration`, and `RemoveSubscriber` to increment/decrement `subscriberTotal` exactly when the protocol map changes. Use `subscriberTotal.Load()` for max-subscriber admission, feedback-router counts, and idle checks. Add:

```go
func (s *Stream) TotalSubscribers() int { return int(s.subscriberTotal.Load()) }
```

Make all methods that mutate or read GOP/media/startup/ring-sequence state follow the sequence lock: `WriteFrame`, `WriteFrameForPublisher`, `SetPublisher`, `RemovePublisher`, `RemovePublisherIf`, `Close`, `UpdatePolicy`, timer callbacks, `WithActivePublisher`, `StartupSnapshot`, `WaitForStartup`, `GOPCacheLen`, `GOPCacheDetail`, `GOPCache`, `GOPCacheSnapshot`, `GOPCacheSourceStart`, `VideoSeqHeader`, `AudioSeqHeader`, `audioCodecState`, and `SeqHeaderReady`. Use the order `writeMu.Lock()` then `mu.Lock()` for lifecycle mutations. Frame writes acquire `writeMu`, briefly acquire `mu` to validate publisher/state, release `mu`, and then run `writeFrameLocked` while retaining `writeMu`; this prevents lifecycle mutation during the frame sequence without holding `mu` for the whole write. Keep `State`, `Config`, `Publisher`, and last-publisher access under `mu`; their writers already hold both locks.

Replace `totalSubscribers()` map scans with `subscriberTotal.Load()` and ensure no code decrements below zero when a removal is unmatched.

- [ ] **Step 4: Run focused Stream tests and race tests**

Run:

```bash
gofmt -w core/stream.go core/stream_test.go core/stream_capacity_test.go
go test ./core -run 'TestStream(TotalSubscribers|Subscribers|MaxSubscribers|GenerationSubscriberRelease|Snapshot|Write|Close|NoPublisher)' -count=1
go test -race ./core -run 'TestStream(TotalSubscribers|Subscribers|MaxSubscribers|GenerationSubscriberRelease|Snapshot|Write|Close|NoPublisher)' -count=1
```

Expected: all focused tests pass with no race report; existing generation and snapshot tests still assert exact cursor/GOP behavior.

- [ ] **Step 5: Commit the Stream synchronization change**

```bash
git add core/stream.go core/stream_test.go core/stream_capacity_test.go
git commit -m "perf: split stream write sequencing from lifecycle reads"
```

### Task 2: Add realistic multi-stream and multi-reader ingress evidence

**Files:**
- Modify: `core/stream_capacity_test.go`
- Modify: `core/production_hotpath_bench_test.go`

**Interfaces:**
- Uses `NewStream`, `SetPublisher`, `WriteFrameForPublisher`, `RingBuffer().NewReaderAt`, and `TotalSubscribers` from Task 1.
- Produces deterministic stress tests and `BenchmarkStreamIngressMatrix`.

- [ ] **Step 1: Write the failing concurrency regression tests**

Add the following deterministic multi-stream test to `core/stream_capacity_test.go`:

```go
func TestStreamMultiStreamConcurrentIngress(t *testing.T) {
    const streamCount, framesPerStream = 8, 2000
    streams := make([]*Stream, streamCount)
    publishers := make([]*testPublisher, streamCount)
    for i := range streams {
        streams[i] = NewStream(fmt.Sprintf("live/capacity/%d", i), newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
        publishers[i] = &testPublisher{id: fmt.Sprintf("publisher-%d", i), info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
        if err := streams[i].SetPublisher(publishers[i]); err != nil { t.Fatal(err) }
        t.Cleanup(streams[i].Close)
    }
    var wait sync.WaitGroup
    for i := range streams {
        wait.Add(1)
        go func(index int) {
            defer wait.Done()
            for n := 0; n < framesPerStream; n++ {
                payload := make([]byte, 8)
                binary.BigEndian.PutUint64(payload, uint64(n))
                frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, int64(n), int64(n), payload)
                if n == 0 { frame.FrameType = avframe.FrameTypeKeyframe }
                if !streams[index].WriteFrameForPublisher(publishers[index], frame) { t.Errorf("stream %d rejected frame %d", index, n); return }
            }
        }(i)
    }
    wait.Wait()
    for i, stream := range streams {
        if got := stream.RingBuffer().WriteCursor(); got != framesPerStream { t.Fatalf("stream %d cursor = %d, want %d", i, got, framesPerStream) }
        if snapshot := stream.StartupSnapshot(); !snapshot.Ready || len(snapshot.ReplayFrames) == 0 { t.Fatalf("stream %d has invalid startup snapshot", i) }
    }
}
```

Add a four-reader overwrite-accounting test using `RingReadResult` and a replacement-generation rejection test with a `sync.WaitGroup`; each reader must observe a non-decreasing `ReadCursor`, and each accepted old-publisher write after replacement must be zero.

- [ ] **Step 2: Run the tests to verify their RED/unstable baseline**

Run:

```bash
go test ./core -run 'TestStream(MultiStreamConcurrentIngress|MultiReaderIngress|PublisherReplacementDuringIngress)$' -count=1
```

Expected: the command reports `no tests to run` until the new test file is added; after adding the tests, the old implementation may pass correctness but cannot satisfy the new `TotalSubscribers`/write-sequence instrumentation added in Task 1. Any test setup error must be fixed before production edits.

- [ ] **Step 3: Implement deterministic test helpers and benchmark matrix**

Use a fixed eight-byte big-endian frame identity and one shared read-only payload per frame class. Keep writer goroutines independent by stream. Add:

```go
func BenchmarkStreamIngressMatrix(b *testing.B) {
    for _, streamCount := range []int{1, 8, 32} {
        for _, readersPerStream := range []int{0, 4, 16} {
            b.Run(fmt.Sprintf("streams=%d/readers=%d", streamCount, readersPerStream), func(b *testing.B) {
                // create streams/publishers/readers before ResetTimer
                // write preallocated key/inter/audio fixtures in parallel
            })
        }
    }
}
```

The benchmark must report `b.ReportAllocs()`, stop readers before cleanup, and assert each writer's successful frame count. It must not use sleeps or network sockets.

- [ ] **Step 4: Run focused stress/race tests and benchmarks**

Run:

```bash
go test ./core -run 'TestStream(MultiStreamConcurrentIngress|MultiReaderIngress|PublisherReplacementDuringIngress)$' -count=1
go test -race ./core -run 'TestStream(MultiStreamConcurrentIngress|MultiReaderIngress|PublisherReplacementDuringIngress)$' -count=1
go test -run '^$' -bench '^BenchmarkStreamIngress(Matrix|ProductionStablePublisher|WithBitrateLimit)$' -benchmem -count=3 ./core
```

Expected: tests pass deterministically; benchmark output includes all nine matrix combinations and no allocations regression is hidden by a missing `ReportAllocs` call.

- [ ] **Step 5: Commit ingress evidence**

```bash
git add core/stream_capacity_test.go core/production_hotpath_bench_test.go
git commit -m "test: add concurrent stream ingress capacity evidence"
```

### Task 3: Publish Prometheus admission snapshots atomically

**Files:**
- Modify: `module/metrics/collector.go`
- Test: `module/metrics/metrics_test.go`, `module/metrics/collector_benchmark_test.go`

**Interfaces:**
- Replaces mutable `streamDetailKeys []string` reads with an immutable `streamDetailSnapshot` stored in `atomic.Value`.
- Keeps `detailStreams(hub *core.StreamHub) []*core.Stream` and all public collector behavior unchanged.

- [ ] **Step 1: Write the failing concurrent-gather test**

Add this timeout-based regression to `module/metrics/metrics_test.go`:

```go
func TestMetricsConcurrentGatherProgressesWhileAdmissionLockHeld(t *testing.T) {
    cfg := testConfig()
    cfg.Metrics.StreamDetailLimit = 32
    server := core.NewServer(cfg)
    for i := range 64 { if _, err := server.StreamHub().GetOrCreate(fmt.Sprintf("live/gather/%03d", i)); err != nil { t.Fatal(err) } }
    collector := NewCollector(server)
    registry := prometheus.NewRegistry()
    registry.MustRegister(collector)
    if _, err := registry.Gather(); err != nil { t.Fatal(err) }
    collector.streamDetailMu.Lock()
    done := make(chan error, 1)
    go func() { _, err := registry.Gather(); done <- err }()
    select {
    case err := <-done:
        collector.streamDetailMu.Unlock()
        if err != nil { t.Fatal(err) }
    case <-time.After(100 * time.Millisecond):
        collector.streamDetailMu.Unlock()
        t.Fatal("stable gather waited on the admission mutex")
    }
}
```

Add `TestMetricsLargeAllowlistRemainsBounded` with 1,000 exact keys, duplicate entries, missing keys, and a destroying stream; gather through a registry and assert sorted unique output contains only live exact keys and no more than `stream_detail_limit` keys.

- [ ] **Step 2: Run the tests to verify RED against the intended non-blocking behavior**

Run:

```bash
go test ./module/metrics -run 'TestMetrics(ConcurrentGatherDoesNotSerializeOnDetailAdmission|LargeAllowlistRemainsBounded)$' -count=1
```

Expected: `TestMetricsConcurrentGatherProgressesWhileAdmissionLockHeld` fails by timing out against the current `detailStreams` mutex. This is the intended RED failure; fix only test synchronization mistakes before changing production code.

- [ ] **Step 3: Implement immutable admission snapshots**

Define:

```go
type streamDetailSnapshot struct { keys []string }
```

Add `streamDetailSnapshot atomic.Value` to `Collector` and initialize it with an empty snapshot in `NewCollector`. In the non-allowlist branch, lock `streamDetailMu`, read the current snapshot, select additional stable streams until `streamDetailLimit`, copy the keys into a new slice, and publish one new `streamDetailSnapshot`. Unlock before resolving streams for metric emission. In the read path, load one snapshot and iterate its detached keys. Never mutate a published slice. Keep allowlist selection unchanged except for using a local detached slice. Continue checking `hub.Find`, pointer identity, and `StreamStateDestroying` for every gather.

- [ ] **Step 4: Add large-scale benchmarks**

In `module/metrics/collector_benchmark_test.go`, add benchmarks for 1,000 active streams with limits 32 and 512, a 1,000-entry allowlist, first admission, stable gather, and eight concurrent gather workers. Report allocations and assert the gathered stream-key count stays within the configured bound. Use an in-process Prometheus registry and no HTTP server.

- [ ] **Step 5: Run metrics focused/race tests and benchmarks**

Run:

```bash
gofmt -w module/metrics/collector.go module/metrics/metrics_test.go module/metrics/collector_benchmark_test.go
go test ./module/metrics -run 'TestMetrics(StreamDetail|ConcurrentGather|LargeAllowlist)' -count=1
go test -race ./module/metrics -run 'TestMetrics(StreamDetail|ConcurrentGather|LargeAllowlist)' -count=1
go test -run '^$' -bench '^BenchmarkMetrics' -benchmem -count=3 ./module/metrics
```

Expected: all existing allowlist, sticky lifetime, churn, and HTTP metrics tests pass; race output is clean; no gathered family contains more distinct stream keys than configured.

- [ ] **Step 6: Commit metrics cardinality change**

```bash
git add module/metrics/collector.go module/metrics/metrics_test.go module/metrics/collector_benchmark_test.go
git commit -m "perf: publish bounded metrics admission snapshots"
```

### Task 4: Synchronize risk ledgers and run the complete verification loop

**Files:**
- Modify: `docs/TECHNICAL-RISKS.md`
- Modify: `docs/PROGRESS.md`
- Modify: `agent-manifest.json`
- Modify: `llms-full.txt`
- Modify: `llms.txt` only if the top-level discovery summary changes
- Modify: `README.md`, `README.zh-CN.md` only if quick-start/operator performance guidance changes
- Test/verification: `tools/check-agent-docs_test.sh`, `tools/check-agent-docs.sh`

**Interfaces:**
- Documents the exact focused tests and benchmark commands from Tasks 2 and 3.
- Changes only `PERF-001` and `PERF-007` to closed after evidence exists; Simulcast remains deferred.

- [ ] **Step 1: Record benchmark output and update risk entries**

Run the focused benchmarks from Tasks 2 and 3 three times, record the stable range, host/Go version, fixture sizes, stream/reader counts, and allocation results in `docs/TECHNICAL-RISKS.md`. Mark both risks closed only if the concurrency/race tests and cardinality assertions pass. State that the results are local regression evidence and that deployment sizing still requires operator-specific load tests.

- [ ] **Step 2: Update project progress and AI-facing facts**

Update `docs/PROGRESS.md` and `agent-manifest.json` with the new lock/snapshot behavior and exact verification commands. Update the corresponding performance paragraphs in `llms-full.txt`; keep `llms.txt` unchanged unless its discovery links or top-level description references the old status. Do not claim a released artifact or a production capacity number.

- [ ] **Step 3: Run all required local gates**

Run:

```bash
go test ./... -count=1
go test -race ./... -count=1
CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge
CGO_ENABLED=1 go test -tags audiocodec -race -coverprofile=/tmp/liveforge-coverage.out -covermode=atomic ./...
go vet ./...
golangci-lint run --new-from-rev=HEAD^
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
git diff --check
```

If FFmpeg or a local lint binary is unavailable, record the exact blocker in the final status and do not mark the corresponding verification as passed.

- [ ] **Step 4: Inspect the final diff and commit synchronized documentation**

```bash
git status --short --untracked-files=all
git diff --stat
git diff -- docs/TECHNICAL-RISKS.md docs/PROGRESS.md agent-manifest.json llms-full.txt llms.txt README.md README.zh-CN.md
git add docs/TECHNICAL-RISKS.md docs/PROGRESS.md agent-manifest.json llms-full.txt llms.txt README.md README.zh-CN.md
git commit -m "docs: close stream write and metrics cardinality risks"
```

- [ ] **Step 5: Confirm only intended tracked files changed**

Run `git status --short --untracked-files=all` and verify the pre-existing `.playwright-mcp/`, `docs/superpowers/plans/2026-09-03-liveforge-nonsimulcast-closure.md`, and `lf-test` remain untouched and uncommitted.
