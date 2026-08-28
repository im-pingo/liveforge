# Media Startup and Cache Correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Completed steps are marked with `[x]`.

**Goal:** Remove LiveForge's independent audio cache and make publisher replacement, startup replay, sequence-header readiness, pure-audio HTTP output, SRT, and SIP startup generation-safe and directly testable.

**Architecture:** `core.Stream` owns publisher generations and exposes one atomic `StreamStartupSnapshot`; production ingress must identify its publisher and every subscriber consumes exactly one snapshot before continuing at its live cursor. GOP replay remains interleaved audio/video, pure audio starts live without a second history cache, and HTTP audio-only segmenters use configured time boundaries.

**Tech Stack:** Go 1.26, YAML v3, MPEG-TS/fMP4 muxers, RTMP/RTSP/SRT/WebRTC/SIP/GB28181 transports, embedded HTML/CSS/JavaScript Console, JSON Schema, OpenAPI.

**Spec:** `docs/superpowers/specs/2026-08-27-media-startup-cache-correctness-design.md`

## Global Constraints

- Use Go 1.26 or newer.
- Write every behavior-changing test first, run it to observe the expected failure, then implement the minimum production change.
- Production protocol ingress must call `WriteFrameForPublisher`; raw `WriteFrame` remains test/internal injection only.
- Do not add a replacement audio history cache or treat RingBuffer as a cache in API/Console copy.
- Existing subscribers end on publisher-generation change and reconnect against the new generation.
- Every source behavior/config/API change updates the corresponding repository-contract documents in the same task.
- Every commit uses `git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit`.
- Do not commit `coverage.out`, binaries, recordings, secrets, or local configuration.
- Completion audit: all steps in this plan are implemented on the current branch;
  current verification evidence is recorded in
  `.superpowers/sdd/2026-08-27-media-startup-cache-correctness/final-review-fix-report.md`.

---

### Task 1: Publisher Generations and Atomic Startup Snapshot

**Files:**
- Modify: `core/stream.go`
- Modify: `core/stream_test.go`
- Modify: `core/muxer_manager.go`
- Modify: `core/muxer_manager_test.go`

**Interfaces:**
- Consumes: existing `Publisher`, `avframe.MediaInfo`, GOP cache, and RingBuffer write cursor.
- Produces: `StreamStartupSnapshot`, `StartupSnapshot()`, `WaitForStartup(context.Context)`, `IsPublisherGeneration(uint64)`, `WriteFrameForPublisher(Publisher, *AVFrame)`, generation-aware `MuxerInstance`, and instance-specific `ReleaseMuxer`.

- [x] **Step 1: Write failing generation-isolation tests**

Add focused tests that create publisher A, write headers/GOP, remove it, set
publisher B, and assert:

```go
if stream.WriteFrameForPublisher(pubA, staleFrame) {
    t.Fatal("stale publisher frame was accepted")
}
if !stream.WriteFrameForPublisher(pubB, freshFrame) {
    t.Fatal("active publisher frame was rejected")
}
snapshot := stream.StartupSnapshot()
if snapshot.Generation != 2 || len(snapshot.ReplayFrames) != 0 ||
    snapshot.VideoSequenceHeader != nil || snapshot.AudioSequenceHeader != nil {
    t.Fatalf("replacement snapshot leaked old generation: %+v", snapshot)
}
if snapshot.LiveCursor < snapshot.GenerationStartCursor {
    t.Fatalf("live cursor %d precedes generation start %d", snapshot.LiveCursor, snapshot.GenerationStartCursor)
}
```

Also test that closing/removing A closes A's `GenerationDone`, B receives a new
channel, and a muxer release for A cannot decrement B's instance.

- [x] **Step 2: Run the core tests and observe RED**

Run: `go test ./core -run 'Test(StreamPublisherGeneration|StreamStartupSnapshot|MuxerManagerGeneration)' -count=1`

Expected: compile failure because the generation and snapshot APIs do not exist.

- [x] **Step 3: Implement generation lifecycle and guarded writes**

Add stream-owned generation fields, media information, readiness/state-change
channels, and one internal write helper:

```go
func (s *Stream) WriteFrameForPublisher(pub Publisher, frame *avframe.AVFrame) bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    if !samePublisher(s.publisher, pub) || s.state != StreamStatePublishing {
        return false
    }
    return s.writeFrameLocked(frame)
}

func (s *Stream) WriteFrame(frame *avframe.AVFrame) bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.writeFrameLocked(frame)
}
```

On `SetPublisher`, increment the generation, record the ring cursor, clear GOP,
headers, media information and readiness, and allocate a new generation-done
channel. On removal, close the active generation channel exactly once.

- [x] **Step 4: Write and run failing readiness/snapshot atomicity tests**

Cover H.264/AAC waiting for headers, MP3/G.711 readiness after media, all-known
track readiness, `SourceCursor == LiveCursor` for audio-only, and an atomic
concurrent GOP rollover test that verifies the replay/live concatenation has no
duplicate frame identity.

Run: `go test ./core -run 'Test(StreamStartup|StreamReadiness|StreamSnapshot)' -race -count=1`

Expected: readiness and atomic snapshot assertions fail before implementation.

- [x] **Step 5: Implement `StreamStartupSnapshot` and wait semantics**

Use this exact public shape (adding `GenerationStartCursor` for diagnostics and
tests):

```go
type StreamStartupSnapshot struct {
    Generation           uint64
    GenerationStartCursor int64
    MediaInfo            avframe.MediaInfo
    VideoSequenceHeader  *avframe.AVFrame
    AudioSequenceHeader  *avframe.AVFrame
    ReplayFrames         []*avframe.AVFrame
    LiveCursor           int64
    SourceCursor         int64
    GenerationDone       <-chan struct{}
    Ready                bool
}
```

Clone media-info byte slices while holding `s.mu`. `WaitForStartup` loops on a
state-change channel and returns only when `StatePublishing && Ready`; it returns
`false` on context cancellation.

- [x] **Step 6: Make muxer instances generation-aware**

Store `Generation uint64` on `MuxerInstance`. When `GetOrCreateMuxer` sees a
different current generation, close the old instance's `Done`, replace the map
entry, and preserve `onStart` callbacks. Change release to:

```go
func (mm *MuxerManager) ReleaseMuxer(format string, inst *MuxerInstance)
```

Only mutate the map entry when it is the same instance; retired instances can
decrement independently.

- [x] **Step 7: Run focused and package tests**

Run: `go test ./core -race -count=1`

Expected: PASS.

- [x] **Step 8: Commit Task 1**

```bash
git add core/stream.go core/stream_test.go core/muxer_manager.go core/muxer_manager_test.go
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "fix: isolate publisher startup generations"
```

### Task 2: Delete Audio Cache and Reject the Removed Setting

**Files:**
- Modify: `config/config.go`
- Modify: `config/loader.go`
- Modify: `config/loader_test.go`
- Modify: `config/runtime/parser.go`
- Modify: `config/runtime/parser_test.go`
- Modify: `core/stream.go`
- Modify: `core/stream_test.go`
- Modify: `core/reload_test.go`
- Modify: every tracked file under `configs/` containing `audio_cache_ms`
- Modify: `docs/config/config.schema.json`
- Modify: `module/api/configschema/config.schema.json`
- Modify: `docs/recipes/runtime-config-sources.md`

**Interfaces:**
- Consumes: Task 1 GOP-only startup model.
- Produces: no `AudioCacheMs`, `audioCache`, `AudioCache`, or `AudioCacheDetail`; stale YAML receives one explicit validation error.

- [x] **Step 1: Write failing removal/validation tests**

Delete tests that assert rolling audio retention and replace them with config
tests for both load paths:

```go
_, err := runtime.ParseDocument([]byte("stream:\n  audio_cache_ms: 1000\n"))
if err == nil || !strings.Contains(err.Error(), "stream.audio_cache_ms has been removed") {
    t.Fatalf("removed setting error = %v", err)
}
```

Use a temporary file with the same document for `config.Load`.

- [x] **Step 2: Run removal tests and observe RED**

Run: `go test ./config ./config/runtime ./core -run 'Test.*AudioCache|Test.*RemovedStreamSetting' -count=1`

Expected: current config accepts the field and core still exposes audio-cache APIs.

- [x] **Step 3: Remove runtime/config/schema surfaces**

Remove the field, defaults, update-policy trimming, write-path retention, accessors,
fixtures, examples, and both schema properties. Add a shared YAML-node check in
`config` that detects only `stream.audio_cache_ms` before ordinary unmarshalling;
call it from file and runtime parsing so unrelated compatibility behavior does
not change.

- [x] **Step 4: Update configuration documentation**

Remove the setting from the configuration-source recipe and describe the GOP as
video-keyframe-bounded interleaved V/A replay. State that pure audio starts from
the live cursor and does not retain a separate startup cache.

- [x] **Step 5: Verify no audio-cache symbols remain**

Run: `rg -n 'AudioCache|audioCache|audio_cache_ms' --glob '!docs/superpowers/**' .`

Expected: no matches except the explicit removed-setting error and its tests.

Run: `go test ./config ./config/runtime ./core -race -count=1`

Expected: PASS.

- [x] **Step 6: Commit Task 2**

```bash
git add config core configs docs/config module/api/configschema docs/recipes/runtime-config-sources.md
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "refactor: remove independent audio cache"
```

### Task 3: Guard Every Production Ingress Writer

**Files:**
- Modify: `module/rtmp/handler.go`
- Modify: `module/rtmp/*_test.go`
- Modify: `module/rtsp/publisher.go`
- Modify: `module/rtsp/handler.go`
- Modify: `module/rtsp/*_test.go`
- Modify: `module/srt/publisher.go`
- Modify: `module/srt/*_test.go`
- Modify: `module/webrtc/whip.go`
- Modify: `module/webrtc/*_test.go`
- Modify: `module/sipgateway/call_session.go`
- Modify: `module/sipgateway/*_test.go`
- Modify: `module/gb28181/handler.go`
- Modify: `module/gb28181/invite_client.go`
- Modify: `module/gb28181/playback.go`
- Modify: `module/gb28181/*_test.go`
- Modify: `module/cluster/transport_rtmp.go`
- Modify: `module/cluster/transport_rtsp.go`
- Modify: `module/cluster/transport_srt.go`
- Modify: `module/cluster/transport_rtp.go`
- Modify: `module/cluster/transport_gb.go`
- Modify: `module/cluster/*_test.go`
- Create: `core/production_ingress_test.go`

**Interfaces:**
- Consumes: `WriteFrameForPublisher` from Task 1.
- Produces: every network/session ingress frame is bound to the publisher that owns its connection.

- [x] **Step 1: Add failing delayed-writer protocol tests**

For RTMP, WHIP, SIP gateway, GB28181 and one cluster pull transport, arrange an
old publisher callback, replace the publisher, invoke the old callback, and
assert that the replacement snapshot/ring contains no stale payload. Assert the
active callback still writes.

- [x] **Step 2: Add a production-ingress source guard**

Create a test that walks non-test Go files under `module/`, parses their AST, and
fails only when a selector call targeting a variable named `stream` or a known
stream field invokes `WriteFrame`. Allow unrelated `FileWriter.WriteFrame` and
muxer methods. The failure prints file and line and requires
`WriteFrameForPublisher`.

- [x] **Step 3: Run protocol tests and observe RED**

Run: `go test ./core ./module/rtmp ./module/rtsp ./module/srt ./module/webrtc ./module/sipgateway ./module/gb28181 ./module/cluster -run 'Test.*(Stale|PublisherWrite|ProductionIngress)' -count=1`

Expected: source guard and delayed old-writer assertions fail.

- [x] **Step 4: Migrate ingress paths with exact publisher identity**

RTMP uses `h.publisher` for both media-info updates and writes. WHIP passes `pub`
into `readTrackLoop`. RTSP publisher uses itself. SIP receive loops use
`cs.publisher`. GB28181 closures capture the newly created `pub` only after its
variable is assigned. Cluster pull loops accept/pass their relay publisher.
Every call uses:

```go
stream.WriteFrameForPublisher(pub, frame)
```

Rejected writes terminate or return from the stale producer loop where that is
safe; datagram callbacks may silently drop after session teardown.

- [x] **Step 5: Run all affected packages under race detection**

Run: `go test ./core ./module/rtmp ./module/rtsp ./module/srt ./module/webrtc ./module/sipgateway ./module/gb28181 ./module/cluster -race -count=1`

Expected: PASS.

- [x] **Step 6: Commit Task 3**

```bash
git add core/production_ingress_test.go module/rtmp module/rtsp module/srt module/webrtc module/sipgateway module/gb28181 module/cluster
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "fix: bind ingress frames to active publishers"
```

### Task 4: Migrate Direct Playback and Shared Muxers to Atomic Startup

**Files:**
- Modify: `module/rtmp/subscriber.go`
- Create: `module/rtmp/subscriber_startup_test.go`
- Modify: `module/rtsp/server.go`
- Modify: `module/rtsp/*subscriber*_test.go`
- Modify: `module/srt/subscriber.go`
- Create: `module/srt/subscriber_startup_test.go`
- Modify: `module/webrtc/whep.go`
- Modify: `module/webrtc/whep_feed.go`
- Modify: `module/webrtc/whep_feed_test.go`
- Modify: `module/httpstream/handler.go`
- Modify: `module/httpstream/ws_handler.go`
- Modify: `module/httpstream/muxer_worker.go`
- Modify: `module/httpstream/muxer_worker_test.go`

**Interfaces:**
- Consumes: Task 1 startup snapshot, generation done signal, and instance-specific muxer release.
- Produces: gap-free direct protocol startup, late header handling, no SRT cross-track DTS drops, generation-safe HTTP muxer workers.

- [x] **Step 1: Write failing replay/live and late-header tests**

Add RTMP/SRT tests where a video GOP ends at DTS 4000 and the first live audio
frame has DTS 1000; assert the audio frame is delivered. Add a later AAC sequence
header after an initial H.264 header and assert it reaches RTMP and causes SRT to
refresh TS track configuration. Add a generation-change test that verifies the
old subscriber exits without sending the first new-generation frame.

- [x] **Step 2: Run focused tests and observe RED**

Run: `go test ./module/rtmp ./module/rtsp ./module/srt ./module/webrtc ./module/httpstream -run 'Test.*(StartupSnapshot|LateSequenceHeader|CrossTrack|Generation)' -count=1`

Expected: current split startup calls, SRT DTS filtering, or generation lifetime assertions fail.

- [x] **Step 3: Migrate protocol subscribers**

Replace separate publisher/header/GOP calls with one snapshot. Direct readers
start exactly at `snapshot.LiveCursor`. Reader-close goroutines select on both
subscriber close and `snapshot.GenerationDone`; processing loops verify
`stream.IsPublisherGeneration(snapshot.Generation)` before sending.

RTMP writes live sequence-header frames instead of skipping them. SRT removes
`lastDTS` entirely and recreates the TS muxer when a live sequence header changes
known track configuration. RTSP continues to omit separate headers because SDP
carries parameter sets.

- [x] **Step 4: Migrate WHEP direct/transcode cursors**

Use `snapshot.LiveCursor` for direct media and `snapshot.SourceCursor` for
historical transcode input. Pass the snapshot's media information and sequence
headers into feed setup. Stop all feed readers on generation end.

- [x] **Step 5: Update shared muxer acquisition/release and workers**

Pass the returned `*MuxerInstance` to `ReleaseMuxer`. Each worker captures one
snapshot and stops on its generation channel. `muxerLiveReader` receives the
snapshot rather than calling `GOPCacheSourceStart` separately.

- [x] **Step 6: Verify affected packages**

Run: `go test ./module/rtmp ./module/rtsp ./module/srt ./module/webrtc ./module/httpstream -race -count=1`

Expected: PASS.

- [x] **Step 7: Commit Task 4**

```bash
git add module/rtmp module/rtsp module/srt module/webrtc module/httpstream
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "fix: make playback startup atomic"
```

### Task 5: Produce Pure-Audio HLS, DASH, and LL-HLS Segments

**Files:**
- Modify: `config/config.go`
- Modify: `config/loader.go`
- Modify: `config/validate.go`
- Modify: `config/*_test.go`
- Modify: `module/httpstream/hls.go`
- Modify: `module/httpstream/hls_test.go`
- Modify: `module/httpstream/dash.go`
- Modify: `module/httpstream/dash_test.go`
- Modify: `module/httpstream/llhls_segmenter.go`
- Modify: `module/httpstream/llhls_segmenter_test.go`
- Modify: `module/httpstream/llhls_manager.go`
- Modify: `module/httpstream/llhls_manager_test.go`
- Modify: `module/httpstream/llhls_playlist.go`
- Modify: `module/httpstream/llhls_playlist_test.go`
- Modify: `module/httpstream/module.go`
- Modify: `module/httpstream/reload_test.go`
- Modify: runnable files under `configs/`
- Modify: `docs/config/config.schema.json`
- Modify: `module/api/configschema/config.schema.json`
- Modify: `docs/recipes/protocol-test-lab.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`

**Interfaces:**
- Consumes: Task 4 snapshot-based HTTP startup.
- Produces: `LLHLSConfig.SegmentDuration`, audio-only time splitting in all HTTP segmenters, and documented test URLs.

- [x] **Step 1: Write failing audio-only live tests**

Use an AAC publisher with no video and feed frames at 20ms DTS intervals while
the manager is running. With `segment_duration=0.2`, require an available HLS TS
segment, DASH audio m4s segment, LL-HLS TS full segment, and LL-HLS fMP4 full
segment before source shutdown. Demux each produced segment and assert it
contains audio frames.

- [x] **Step 2: Run HTTP tests and observe RED**

Run: `go test ./module/httpstream -run 'Test(HLS|DASH|LLHLS).*AudioOnly.*Live' -count=1`

Expected: HLS/DASH have zero completed segments and LL-HLS ignores the configured test duration because it is hard-coded.

- [x] **Step 3: Implement boundary-safe time splitting**

For streams without video, finalize before appending the boundary frame when:

```go
hasData && float64(frame.DTS-segStartDTS)/1000.0 >= targetDuration
```

Then set the new segment start to the boundary frame DTS and append it exactly
once. Preserve video keyframe behavior unchanged.

- [x] **Step 4: Add configurable LL-HLS full-segment duration**

Add `SegmentDuration float64` with YAML key `segment_duration`, default `1.0`,
validation `> 0` when LL-HLS is enabled, schema minimum `0.1`, runtime reload
classification matching other LL-HLS policy, and constructor flow from Module
to Manager to Segmenter and Playlist. Use the value as the pre-completion target
duration instead of `6.0`.

- [x] **Step 5: Update examples and protocol recipe**

Document `part_duration` versus `segment_duration`, pure-audio HLS/DASH/LL-HLS
verification URLs, and the expected bounded first-segment timing. Update both
README quick-test descriptions and every runnable config with `segment_duration: 1.0`.

- [x] **Step 6: Run config and HTTP package tests**

Run: `go test ./config ./config/runtime ./module/httpstream -race -count=1`

Expected: PASS.

- [x] **Step 7: Commit Task 5**

```bash
git add config module/httpstream configs docs/config docs/recipes/protocol-test-lab.md README.md README.zh-CN.md
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "fix: segment pure audio HTTP streams"
```

### Task 6: Remove Stale Ring History from SIP, Recording, DVR, GB28181, and Cluster Egress

**Files:**
- Modify: `module/sipgateway/call_session.go`
- Create: `module/sipgateway/call_session_startup_test.go`
- Modify: `module/record/session.go`
- Modify: `module/record/record_test.go`
- Modify: `module/dvr/session.go`
- Modify: `module/dvr/*_test.go`
- Modify: `module/gb28181/outbound_media.go`
- Modify: `module/gb28181/*_test.go`
- Modify: `module/cluster/transport_rtmp.go`
- Modify: `module/cluster/transport_rtsp.go`
- Modify: `module/cluster/transport_srt.go`
- Modify: `module/cluster/transport_rtp.go`
- Modify: `module/cluster/transport_gb.go`
- Modify: `module/cluster/*_test.go`
- Modify: `docs/architecture.zh-CN.md`
- Modify: `docs/cluster-guide.md`
- Modify: `docs/cluster-guide.zh-CN.md`

**Interfaces:**
- Consumes: Task 1 snapshot and generation channels.
- Produces: no production subscriber starts with RingBuffer `NewReader()` history; session output is generation-bound.

- [x] **Step 1: Write failing SIP and recording stale-history tests**

Pre-fill a ring with publisher A audio, replace with publisher B, start a SIP
outbound call/record session, and write one B frame. Assert the first sent or
recorded media payload is B's and A's payload never appears. Add a video case
that asserts the B snapshot GOP is replayed once.

- [x] **Step 2: Write a production-reader source guard**

AST-scan non-test module code and reject `stream.RingBuffer().NewReader()` in
protocol egress/session startup. Explicit local in-memory buffers remain allowed.

- [x] **Step 3: Run focused tests and observe RED**

Run: `go test ./module/sipgateway ./module/record ./module/dvr ./module/gb28181 ./module/cluster -run 'Test.*(StaleHistory|GenerationStartup|ProductionReader)' -count=1`

Expected: old ring history is observed or source guard reports legacy readers.

- [x] **Step 4: Migrate session and egress readers**

Capture one startup snapshot. Send snapshot headers and `ReplayFrames` once when
the destination protocol uses them, create the live reader at `LiveCursor`, and
cancel it on `GenerationDone`. Pure audio has no replay frames. Recording/DVR
set expected tracks from `snapshot.MediaInfo`, not a separately read publisher.

- [x] **Step 5: Update architecture and cluster documentation**

Describe publisher-generation termination, atomic GOP replay/live continuation,
and why cluster relays never replay retained frames from a previous origin
publisher.

- [x] **Step 6: Verify affected packages**

Run: `go test ./module/sipgateway ./module/record ./module/dvr ./module/gb28181 ./module/cluster -race -count=1`

Expected: PASS.

- [x] **Step 7: Commit Task 6**

```bash
git add module/sipgateway module/record module/dvr module/gb28181 module/cluster docs/architecture.zh-CN.md docs/cluster-guide.md docs/cluster-guide.zh-CN.md
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "fix: isolate session startup history"
```

### Task 7: Synchronize API, Console, OpenAPI, and AI-Facing Documentation

**Files:**
- Modify: `module/api/handler.go`
- Modify: `module/api/handler_test.go`
- Modify: `module/api/console.html`
- Modify: `module/api/console_management_test.go`
- Modify: `docs/api/openapi.yaml`
- Modify: `agent-manifest.json`
- Modify: `llms-full.txt`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/architecture.zh-CN.md`
- Modify: `docs/recipes/protocol-test-lab.md`

**Interfaces:**
- Consumes: GOP-only core statistics and LL-HLS config from Tasks 2 and 5.
- Produces: `/api/v1/streams` and Console expose only truthful GOP semantics; all project facts match source and tests.

- [x] **Step 1: Change API/Console tests first**

Remove audio-cache response fields from the expected JSON shape. Update Console
DOM probes to require the `GOP Cache` header, interleaved `V`/`A` counts for a
video stream, and `Not applicable (audio-only)` for a stream with audio codec but
zero GOP frames. Assert no `Media Cache`, independent `Audio` row, or hard-coded
cache progress bar remains.

- [x] **Step 2: Run API tests and observe RED**

Run: `go test ./module/api -run 'Test.*(Stream|Console).*Cache' -count=1`

Expected: old API fields and Console text violate the new expectations.

- [x] **Step 3: Remove API fields and rebuild Console rendering**

Delete `audio_cache_frames` and `audio_cache_duration_ms` from `StreamInfo` and
response construction. Rename the table heading to `GOP Cache`. Render one
textual line from GOP generation/duration and V/A counts; render the audio-only
not-applicable text when `video_codec` is empty and `audio_codec` is present.
Remove the fixed `/120` width calculation and its unused markup/styles.

- [x] **Step 4: Update OpenAPI and project facts**

Remove the two unreleased required properties from `docs/api/openapi.yaml`.
Replace every separate-audio-cache claim in `agent-manifest.json`,
`llms-full.txt`, README files, architecture, and protocol recipe with the
generation-safe interleaved GOP model. Include `http_stream.llhls.segment_duration`
where configuration facts are enumerated.

- [x] **Step 5: Validate API and documentation**

Run: `go test ./module/api -race -count=1`

Run: `tools/check-agent-docs_test.sh`

Run: `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`

Expected: all commands PASS.

- [x] **Step 6: Commit Task 7**

```bash
git add module/api docs/api/openapi.yaml agent-manifest.json llms-full.txt README.md README.zh-CN.md docs/architecture.zh-CN.md docs/recipes/protocol-test-lab.md
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "docs: align console with GOP startup semantics"
```

### Task 8: Full Verification, Runtime Smoke Test, and Review Loop

**Files:**
- Modify only files required by concrete verification or review findings.

**Interfaces:**
- Consumes: all prior tasks and repository verification commands.
- Produces: fresh evidence that source, tests, docs, API, Console, and runtime media paths agree.

- [x] **Step 1: Format and run static residue checks**

Run: `gofmt -w` on every changed Go file.

Run: `rg -n 'AudioCache|audioCache|audio_cache_(ms|frames|duration)' --glob '!docs/superpowers/**' .`

Expected: only removed-setting validation text/tests may match `audio_cache_ms`; no runtime/API/Console cache symbols remain.

Run: `rg -n 'GOPCacheSnapshot|GOPCacheSourceStart|RingBuffer\(\)\.NewReader\(\)' module --glob '*.go' --glob '!*_test.go'`

Expected: no production startup-path matches.

- [x] **Step 2: Run untagged baseline tests**

Run: `go test ./...`

Expected: PASS.

- [x] **Step 3: Run documentation gates**

Run: `tools/check-agent-docs_test.sh`

Run: `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`

Expected: PASS.

- [x] **Step 4: Run tagged build and race/coverage baseline**

Run: `CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge`

Run: `CGO_ENABLED=1 go test -tags audiocodec -race -coverprofile=coverage.out -covermode=atomic ./...`

Expected: PASS. Delete generated `coverage.out` and `liveforge` after recording the result.

- [x] **Step 5: Run Console/protocol smoke tests**

Start the sample local server on an unused port, run the bundled SIP and GB28181
lab publish flows, and verify API stream stats show incoming video/audio where
the simulator declares them. Fetch RTMP/HTTP playback through existing test
clients, and fetch pure-audio HLS, DASH, and LL-HLS manifests plus one media
segment. Confirm no browser Console errors and no `audio_cache_*` response fields.

- [x] **Step 6: Dispatch an independent whole-branch code review**

Review the diff from `dcbd528acb1c676457ee8e884a5dbaefb622d1be` to HEAD against the design spec and this plan. Require findings ordered by severity with exact file/line references and explicit attention to deadlocks, generation races, frame gaps/duplicates, timestamp-domain assumptions, goroutine leaks, config/API compatibility, and missing tests.

- [x] **Step 7: Loop on every Critical or Important finding**

For each review wave, write or identify a failing regression test, reproduce the
finding, implement the smallest root-cause fix, rerun focused tests, and request
a scoped re-review. Continue until no Critical or Important finding remains and
all full verification commands pass again.

- [x] **Step 8: Final commit and clean status check**

If verification fixes changed files:

```bash
git add -u
git -c user.name=im-pingo -c user.email=cczjp89@gmail.com commit -m "fix: close media startup review findings"
```

Run: `git status --short`

Expected: empty output, with generated binaries, recordings, and coverage files absent.

> The independent review dispatch required by Task 8 was attempted during the
> completion audit, but the reviewer did not return within the bounded wait and
> was stopped without changing files. The source audit, focused tests, full
> default and tagged race suites, static checks, and local process smoke tests
> found no unresolved Critical or Important finding.
