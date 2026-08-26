# High-Concurrency Forwarding Performance Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reduce wakeup loss, allocation, syscall, and metrics overhead in high-concurrency stream forwarding while preserving protocol output and shutdown behavior.

**Architecture:** Per-reader blocking waits use the existing RingBuffer condition variable and a context cancellation hook, so readers no longer consume a shared notification channel. Protocol writers reuse per-connection FLV state, use vectored writes for RTSP interleaving, and accumulate relay byte metrics in a pre-bound observation before flushing to Prometheus at bounded intervals and shutdown.

**Tech Stack:** Go 1.26, `sync.Cond`, `context.AfterFunc`, `net.Buffers`, Prometheus client, Go benchmarks, race detector.

**Spec:** Repository performance analysis requested for high-concurrency forwarding; this plan records the directly authorized implementation scope.

## Global Constraints

- Preserve Go 1.26+ support and existing module boundaries.
- Preserve RTMP, RTSP, relay metric names and label cardinality.
- Preserve cancellation, stream-close, and reader-close behavior.
- Do not reset or overwrite unrelated user changes.
- Run focused tests, race tests, tagged build/tests when available, and `tools/check-agent-docs_test.sh`.

---

### Task 1: Context-aware per-reader ring-buffer waits

**Files:**
- Modify: `pkg/util/ringbuffer.go`
- Test: `pkg/util/ringbuffer_test.go`
- Benchmark: `pkg/util/ringbuffer_test.go`

**Interfaces:**
- Produces `(*RingReader[T]).ReadContext(context.Context) (T, bool)`.
- Existing `Read`, `TryRead`, and `Signal` APIs remain source-compatible.

- [ ] Add a test proving two readers can independently block and both receive the next write through `ReadContext`.
- [ ] Add cancellation and reader-close tests for `ReadContext`.
- [ ] Add a benchmark comparing immediate `TryRead` and the blocking path without shared signal consumption.
- [ ] Implement `ReadContext` with `context.AfterFunc` broadcasting the existing condition variable only while the caller is waiting.
- [ ] Run `go test ./pkg/util` and `go test -race ./pkg/util`.

### Task 2: Migrate forwarding consumers off shared Signal

**Files:**
- Modify: `module/cluster/transport_rtmp.go`
- Modify: `module/cluster/transport_srt.go`
- Modify: `module/cluster/transport_rtsp.go`
- Modify: `module/cluster/transport_rtp.go`
- Modify: `module/cluster/transport_gb.go`
- Modify: `module/dvr/session.go`
- Modify: `module/record/session.go`
- Modify: `module/sipgateway/call_session.go`
- Modify: `module/webrtc/whep_feed.go`

**Interfaces:**
- Consumers use their own `RingReader` with `ReadContext`; no loop relies on draining `RingBuffer.Signal()`.

- [ ] Replace each `TryRead` plus shared signal wait with a reader-specific context-aware wait.
- [ ] Preserve protocol-specific timeout behavior with child contexts or timers.
- [ ] Add/adjust focused tests for relay readers and WHEP wakeups.
- [ ] Run cluster, DVR, record, SIP, and WebRTC focused tests with race detection.

### Task 3: Reuse RTMP encoding state

**Files:**
- Modify: `module/cluster/rtmp_client.go`
- Test: `module/cluster/rtmp_client_test.go`
- Benchmark: `module/cluster/rtmp_client_test.go`

**Interfaces:**
- `rtmpConn.sendMediaFrame` uses connection-owned `flv.Muxer` and `bytes.Buffer`; standalone `buildRTMPPayload` remains compatible for tests and callers.

- [ ] Add an allocation benchmark for repeated media frame encoding.
- [ ] Implement reusable connection-owned encoding state and keep payload lifetime valid through `WriteMessage`.
- [ ] Verify encoded payload bytes remain protocol-compatible for H.264 and AAC.

### Task 4: Reduce RTSP interleaved writes and relay metrics hot-path work

**Files:**
- Modify: `module/rtsp/interleaved.go`
- Modify: `module/cluster/transport_rtsp.go`
- Modify: `module/cluster/transport.go`
- Modify: `module/cluster/relay_metrics.go`
- Modify: `module/cluster/forward.go`
- Modify: `module/cluster/origin.go`
- Test: `module/cluster/relay_metrics_test.go`
- Test: `module/rtsp/transport_test.go`

**Interfaces:**
- RTSP writers use `net.Buffers` for writev-capable connections and preserve framing.
- Relay observations pre-bind byte counters, batch updates at a 64 KiB threshold, and flush on operation completion.

- [ ] Add framing and metric-flush regression tests.
- [ ] Implement vectored interleaved writes without per-packet combined-buffer allocation.
- [ ] Implement bounded atomic byte accumulation and final flush.
- [ ] Run focused RTSP/cluster metric tests and benchmarks.

### Task 5: Documentation and verification

**Files:**
- Modify: `agent-manifest.json`
- Modify: `llms-full.txt`
- Modify: `llms.txt`
- Modify: `README.md`
- Modify: `README.zh-CN.md`

- [ ] Document the high-concurrency forwarding behavior, bounded relay metric batching, and benchmark/verification commands.
- [ ] Run `gofmt` on changed Go files.
- [ ] Run `go test ./...` and focused race tests.
- [ ] Run `CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge`.
- [ ] Run the tagged race suite when the environment supports it.
- [ ] Run `tools/check-agent-docs_test.sh` and report any environment-only blockers.
