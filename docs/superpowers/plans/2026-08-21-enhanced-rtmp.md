# Enhanced RTMP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add single-track E-RTMP ingest, FourCC media output, and capability-based classic/enhanced RTMP negotiation for LiveForge.

**Architecture:** Keep `AVFrame` as the codec-neutral internal representation and make `pkg/muxer/flv` the only implementation of classic and E-RTMP media payload parsing/writing. Store E-RTMP capabilities per RTMP connection, choose independent video/audio output modes per subscriber, and reuse the shared FLV parsers in cluster and testkit RTMP paths.

**Tech Stack:** Go, AMF0, RTMP Chunk Stream, FLV tag payloads, existing `AVFrame` and audio transcode manager, Go standard-library tests.

**Spec:** `docs/superpowers/specs/2026-08-21-enhanced-rtmp-design.md`

## Global Constraints

- Phase 1 is single-track and supports video H.264, H.265, AV1, VP8, VP9 and audio AAC, Opus, MP3.
- Classic H.264/AAC/MP3 output remains the default for peers that omit E-RTMP capability fields.
- Enhanced video coded frames carry CTS only for `avc1` and `hvc1`; AV1, VP8, and VP9 require `PTS == DTS`.
- Enhanced audio uses `[0x9 | packetType][FourCC][payload]`; it never adds a second packet-type byte.
- The server advertises `CanForward` only and `capsEx=0`; it does not claim codec encoding, multitrack, HDR, ModEx, nanosecond offsets, reconnect, or VVC.
- Optional malformed capability properties degrade to legacy behavior; they do not reject a valid RTMP `connect` command.
- Write tests before production code and verify each red-green cycle with `GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache`.
- Do not change `AVFrame`, downstream muxer track models, or unrelated protocol behavior.

---

### Task 1: Implement Shared E-RTMP FLV Wire Primitives

**Files:**
- Modify: `pkg/muxer/flv/types.go`
- Create: `pkg/muxer/flv/payload.go`
- Modify: `pkg/muxer/flv/muxer.go`
- Modify: `pkg/muxer/flv/demuxer.go`
- Test: `pkg/muxer/flv/muxer_test.go`
- Test: `pkg/muxer/flv/demuxer_test.go`
- Test: `pkg/muxer/flv/payload_test.go`

**Interfaces:**
- Produces `type EncodingMode uint8` with `EncodingAuto`, `EncodingClassic`, and `EncodingEnhanced`.
- Produces `func NewMuxerWithModes(videoMode, audioMode EncodingMode) *Muxer` while preserving `NewMuxer()`.
- Produces `func ParseVideoPayload(data []byte, dts int64) (*avframe.AVFrame, error)`.
- Produces `func ParseAudioPayload(data []byte, dts int64) (*avframe.AVFrame, error)`.
- Produces `func VideoFourCC(codec avframe.CodecType) string`, `func AudioFourCC(codec avframe.CodecType) string`, and inverse FourCC lookup helpers used by RTMP capability selection.

- [x] **Step 1: Write failing golden tests for enhanced video bytes**

Add table-driven tests that construct one sequence header and one coded frame
for each of `avc1`, `hvc1`, `av01`, `vp08`, and `vp09`. Assert the exact media
body bytes after stripping the FLV tag header. Use this expected shape for an
H.265 coded frame with DTS 100 and PTS 67:

```go
want := []byte{
    0x91, 'h', 'v', 'c', '1',
    0xff, 0xff, 0xdf,
    0x26, 0x01,
}
```

Add cases for positive and zero CTS, and assert that AV1/VP8/VP9 coded bodies
contain no three-byte CTS field. Add a case with nonzero AV1 CTS that expects
an encoding error.

- [x] **Step 2: Run the focused tests and verify the expected red failure**

Run:

```bash
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./pkg/muxer/flv -run 'TestEnhanced|TestE-RTMP' -count=1
```

Expected: FAIL because the current muxer emits CTS for every enhanced video
codec, lacks VP8 mapping, and has no explicit mode API.

- [x] **Step 3: Add codec and header constants plus signed SI24 helpers**

Extend `types.go` with `vp08`, `avc1`, `hvc1`, `av01`, `vp09`, `mp4a`, `Opus`,
and `.mp3` mappings, the ExAudio packet constants, and helpers that convert a
three-byte big-endian value to a signed `int32` and back using sign extension.
Keep legacy public constants needed by existing callers, but route new output
through the E-RTMP constants.

- [x] **Step 4: Implement mode-aware video and audio writers**

Change `Muxer` to store independent video and audio modes. `NewMuxer()` uses
`EncodingAuto`; `NewMuxerWithModes` stores the explicit modes. Implement these
rules:

```go
switch mode {
case EncodingClassic:
    // H.264/AAC/MP3 only; return an unsupported-codec error otherwise.
case EncodingEnhanced:
    // Use the codec's FourCC and the E-RTMP header layout.
case EncodingAuto:
    // Classic for H.264/AAC/MP3; enhanced for the other Phase 1 codecs.
}
```

For enhanced video, write `SequenceStart` without CTS, write `CodedFrames`
with signed SI24 only for AVC/HEVC, and write the raw payload after the header.
For enhanced audio, put packet type in the low nibble of the first byte and
write the FourCC immediately after it. Do not append the old extra packet-type
byte.

- [x] **Step 5: Run the focused golden tests and verify green**

Run the command from Step 2. Expected: all new exact-byte tests and the
existing classic muxer tests pass.

- [x] **Step 6: Write failing parser and round-trip tests**

Add tests for classic H.264/AAC/MP3 and enhanced single-track media. Assert
codec, frame type, DTS, PTS, and copied payload. Include negative CTS (`-33`),
sequence-end skipping, truncated FourCC/header input, unknown FourCC input, and
unsupported multitrack/modifier packet input.

- [x] **Step 7: Run parser tests to verify red, then implement parsers**

Run:

```bash
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./pkg/muxer/flv -run 'TestParse|TestDemux|TestMuxRoundTrip' -count=1
```

Expected: new enhanced parser tests fail before implementation. Then implement
`ParseVideoPayload` and `ParseAudioPayload`, make the demuxer delegate to them,
copy payload bytes, sign-extend CTS, and skip tags with no `AVFrame` equivalent.

- [x] **Step 8: Run all FLV tests and commit the wire layer**

Run:

```bash
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./pkg/muxer/flv -count=1
```

Expected: PASS. Commit:

```bash
git add pkg/muxer/flv
git commit -m "feat(flv): add E-RTMP media payload support"
```

### Task 2: Add AMF0 Arrays and RTMP Capability Modeling

**Files:**
- Modify: `module/rtmp/amf0.go`
- Test: `module/rtmp/amf0_test.go`
- Create: `module/rtmp/capabilities.go`
- Create: `module/rtmp/capabilities_test.go`

**Interfaces:**
- Produces `type PeerCapabilities struct` containing `FourCCList`, video/audio info maps, and `CapsEx`.
- Produces `func ParsePeerCapabilities(obj map[string]any) PeerCapabilities`.
- Produces `func (c PeerCapabilities) SupportsVideo(codec avframe.CodecType) bool` and `func (c PeerCapabilities) SupportsAudio(codec avframe.CodecType) bool`.
- Produces `func ServerCapabilitiesObject() map[string]any` for AMF0 `_result` encoding.
- Produces `func ClientCapabilitiesObject() map[string]any` for E-RTMP-aware cluster and testkit connect commands.
- Produces AMF0 strict-array encoding/decoding as `[]any` and ECMA-array decoding as `map[string]any`.

- [ ] **Step 1: Write failing AMF0 strict-array and ECMA-array tests**

Add a round-trip case for:

```go
[]any{"vp08", "vp09", "av01", "hvc1", "Opus"}
```

and a manually built ECMA-array marker (`0x08`) containing
`videoFourCcInfoMap["vp09"] = 4`. Assert the decoded types and values,
including a nested strict array inside an object.

- [ ] **Step 2: Run the AMF0 tests and verify red**

Run:

```bash
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/rtmp -run 'TestAMF0' -count=1
```

Expected: FAIL with the current unsupported `0x0A`/`0x08` markers.

- [ ] **Step 3: Implement strict-array and ECMA-array support**

Add markers `0x08` and `0x0A`, encode `[]any` as a four-byte count followed by
values, decode strict arrays with bounds checks, and decode ECMA arrays through
the existing object-property loop. Propagate nested encode errors instead of
discarding them. Keep deterministic object key ordering.

- [ ] **Step 4: Write failing capability parsing tests**

Test these cases:

```go
obj := map[string]any{
    "fourCcList": []any{"vp09", "Opus"},
    "videoFourCcInfoMap": map[string]any{"vp09": float64(1)},
    "audioFourCcInfoMap": map[string]any{"Opus": float64(5)},
    "capsEx": float64(0),
}
```

Assert that `vp09` and `Opus` are playable, that `CanDecode` and
`CanForward` are accepted, that `CanEncode` alone is rejected for playback,
that a wildcard map overrides a specific entry, and that no fields means a
legacy peer. Assert the exact server object keys and FourCC order.

- [ ] **Step 5: Run capability tests to verify red, then implement the model**

Run:

```bash
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/rtmp -run 'TestPeerCapabilities|TestServerCapabilities' -count=1
```

Expected: FAIL before the model exists. Implement the three flag constants,
codec-to-FourCC lookup calls, tolerant numeric/string/array conversion, and
wildcard precedence exactly as specified. `ServerCapabilitiesObject` must
return `fourCcList` as `[]any`, maps with `CanForward`, and numeric `capsEx=0`.

- [ ] **Step 6: Run AMF0 and capability tests and commit**

Run:

```bash
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/rtmp -run 'TestAMF0|TestPeerCapabilities|TestServerCapabilities' -count=1
```

Expected: PASS. Commit:

```bash
git add module/rtmp/amf0.go module/rtmp/amf0_test.go module/rtmp/capabilities.go module/rtmp/capabilities_test.go
git commit -m "feat(rtmp): negotiate E-RTMP capabilities"
```
### Task 3: Integrate E-RTMP With RTMP Handler and Subscriber

**Files:**
- Modify: `module/rtmp/handler_protocol.go`
- Modify: `module/rtmp/handler.go`
- Modify: `module/rtmp/subscriber.go`
- Test: `module/rtmp/handler_test.go`
- Test: `module/rtmp/helpers_test.go`
- Create: `module/rtmp/subscriber_format_test.go`

**Interfaces:**
- `Handler` stores one `PeerCapabilities` snapshot from its `connect` command.
- `sendConnectResult` includes `ServerCapabilitiesObject` in the response properties object.
- `NewSubscriberWithCapabilities(..., caps PeerCapabilities, onFailure func(error)) *Subscriber` is added; `NewSubscriber` remains a compatibility wrapper.
- Produces `chooseOutputPolicy(info *avframe.MediaInfo, caps PeerCapabilities) (outputPolicy, error)` with independent video/audio modes and audio-transcode selection.

- [ ] **Step 1: Write failing handler capability-response tests**

Send a `connect` command whose command object contains a strict `fourCcList`
and `videoFourCcInfoMap`. Decode the server `_result` and assert that its
properties object includes `fourCcList`, both info maps, and `capsEx` in
addition to the existing fields. Assert that a legacy connect still receives a
valid `_result`.

- [ ] **Step 2: Run the handler tests and verify red**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/rtmp -run 'TestHandlerConnect|TestConnectCapabilities' -count=1
~~~

Expected: the capability response assertions fail because the handler does not
yet parse or return E-RTMP fields.

- [ ] **Step 3: Add handler capability parsing and response fields**

Extract the connect command object as a `map[string]any`, call
`ParsePeerCapabilities`, and pass `ServerCapabilitiesObject()` to
`sendConnectResult`. Ignore malformed optional fields while preserving app and
transaction parsing. Pass the snapshot to `NewSubscriberWithCapabilities` in
`onPlay`.

- [ ] **Step 4: Write failing RTMP ingest tests for enhanced media**

Send an enhanced H.265 sequence-start body and an enhanced Opus coded body as
RTMP media messages after publish. Assert the stream GOP/audio cache contains
frames with the expected codecs, frame types, payloads, DTS, and PTS. Add a
malformed enhanced message and assert the handler remains alive for a following
valid classic message.

- [ ] **Step 5: Run ingest tests to verify red, then delegate to shared parsers**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/rtmp -run 'TestHandlerMedia|TestEnhancedIngest' -count=1
~~~

Expected: enhanced frames are currently ignored or misparsed. Replace the
duplicated header logic in `module/rtmp/handler_protocol.go` with wrappers around
`flvpkg.ParseVideoPayload` and `ParseAudioPayload`; log and drop parser errors
inside `handleMediaMessage` without terminating the RTMP command loop.

- [ ] **Step 6: Write failing subscriber negotiation tests**

Test `chooseOutputPolicy` and payload construction for these peers:

1. No capability fields: H.264/AAC use classic bodies.
2. `av01` and `Opus` receive flags: AV1/Opus use enhanced bodies.
3. H.265 with no enhanced capability returns an unsupported-video error.
4. Opus with no enhanced capability requests AAC transcoding.
5. H.264 `avc1` support selects enhanced video independently of AAC mode.

Assert exact first bytes/FourCC and that no Opus extra packet-type byte is
present.

- [ ] **Step 7: Run subscriber tests to verify red, then implement policy**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/rtmp -run 'TestOutputPolicy|TestSubscriberPayload|TestEnhancedSubscriber' -count=1
~~~

Expected: the current subscriber always relies on muxer auto mode and has no
peer capability state. Implement the policy matrix, initialize the muxer with
explicit modes, retain the existing transcode reader for unsupported audio,
and call an optional `onFailure` callback before closing on unsupported video.

- [ ] **Step 8: Run all RTMP tests and commit the server integration**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/rtmp -count=1
~~~

Expected: PASS. Commit:

~~~
git add module/rtmp
git commit -m "feat(rtmp): negotiate enhanced media output"
~~~

### Task 4: Reuse the Wire Layer in Cluster and Testkit RTMP Paths

**Files:**
- Modify: `module/cluster/rtmp_client.go`
- Modify: `module/cluster/transport_rtmp.go`
- Test: `module/cluster/rtmp_client_test.go`
- Modify: `tools/testkit/play/rtmp.go`
- Modify: `tools/testkit/push/rtmp.go`

**Interfaces:**
- Cluster and testkit parsers delegate to `flvpkg.ParseVideoPayload` and `ParseAudioPayload`.
- Cluster/testkit connect objects include `rtmp.ClientCapabilitiesObject()` so they can receive recognized enhanced codecs.
- Existing `buildRTMPPayload` callers continue using `flvpkg.NewMuxer()` and remain single-track.

- [ ] **Step 1: Write failing cluster parser regression tests**

Add enhanced `hvc1` and corrected `Opus` bodies to the existing cluster parser
table and assert the same `AVFrame` values as the FLV package tests. Add a
negative CTS case to ensure the old unsigned conversion is gone.

- [ ] **Step 2: Run the cluster tests and verify red**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/cluster -run 'Test(Parse|RTMP)' -count=1
~~~

Expected: duplicate parsers fail enhanced FourCC and signed CTS cases.

- [ ] **Step 3: Replace duplicate parsers and advertise capabilities**

Delegate the cluster parser wrappers to the exported FLV functions. Add the
server-supported capability object to cluster/testkit `connect` commands while
leaving their current command transaction sequence and message IDs unchanged.
Update response parsing to tolerate the larger `_result` object.

- [ ] **Step 4: Run cluster and testkit-adjacent tests and commit**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./module/cluster ./tools/testkit/... -count=1
~~~

Expected: PASS. Commit:

~~~
git add module/cluster tools/testkit/play/rtmp.go tools/testkit/push/rtmp.go
git commit -m "refactor(rtmp): share enhanced payload parsing"
~~~

### Task 5: Cross-Layer Regression and Compatibility Verification

**Files:**
- Modify: `pkg/muxer/flv/muxer_test.go`
- Modify: `pkg/muxer/flv/demuxer_test.go`
- Modify: `module/rtmp/handler_test.go`
- Modify: `module/rtmp/subscriber_format_test.go`
- Modify: `module/cluster/integration_test.go` only if an existing fixture needs an explicit capability field

**Interfaces:**
- No new production interfaces; this task verifies the contracts produced by Tasks 1-4.

- [ ] **Step 1: Run focused cross-package tests**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./pkg/muxer/flv ./module/rtmp ./module/cluster -count=1
~~~

Expected: PASS with exact-byte, parser, negotiation, and legacy regression
coverage.

- [ ] **Step 2: Run race-sensitive RTMP tests**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test -race ./module/rtmp ./module/cluster -count=1
~~~

Expected: PASS without concurrent writer, capability snapshot, or frame-payload
race reports.

- [ ] **Step 3: Run the full repository test suite**

Run:

~~~
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go test ./...
~~~

Expected: all packages pass. Investigate only failures caused by the changed
wire/API behavior; do not alter unrelated failing tests.

- [ ] **Step 4: Run formatting, vet, and configured lint checks**

Run:

~~~
gofmt -w pkg/muxer/flv module/rtmp module/cluster tools/testkit/play/rtmp.go tools/testkit/push/rtmp.go
GOTOOLCHAIN=auto GOPATH=/tmp/liveforge-gopath GOMODCACHE=/tmp/liveforge-gomodcache go vet ./pkg/muxer/flv ./module/rtmp ./module/cluster
make lint
~~~

Expected: `gofmt` changes no files after the first pass, `go vet` exits 0, and
the repository lint target reports no new findings.

- [ ] **Step 5: Review the diff against the spec and commit verification fixes**

Run:

~~~
git diff --check
git status --short
git diff --stat origin/main...HEAD
~~~

Confirm that only the documented FLV, RTMP, cluster/testkit, tests, design, and
plan files changed. Commit any formatting or test-only corrections with:

~~~
git add .
git commit -m "test: verify E-RTMP compatibility paths"
~~~

### Task 6: Publish the Change and Update Issue #10

**Files:**
- No additional source files; use the verified commits from Tasks 1-5.

- [ ] **Step 1: Confirm branch and verification evidence**

Run:

~~~
git branch --show-current
git log --oneline --decorate origin/main..HEAD
git status --short
~~~

Expected: branch is `feat/enhanced-rtmp`, the worktree is clean, and all
required verification commands from Task 5 have fresh passing output.

- [ ] **Step 2: Push the feature branch**

Run:

~~~
HTTP_PROXY=http://10.4.80.9:1181 HTTPS_PROXY=http://10.4.80.9:1181 git push -u origin feat/enhanced-rtmp
~~~

Expected: the remote branch is created under `im-pingo/liveforge`.

- [ ] **Step 3: Create a pull request with compatibility notes**

Run:

~~~
gh pr create --repo im-pingo/liveforge --base main --head feat/enhanced-rtmp --title "feat: add single-track Enhanced RTMP support" --body-file /tmp/liveforge-enhanced-rtmp-pr.md
~~~

The PR body must state that E-RTMP preserves RTMP transport compatibility but
does not make every legacy media decoder compatible with every new codec. List
the supported FourCCs, classic fallback behavior, deferred multitrack/HDR
scope, and the exact test commands that passed.

- [ ] **Step 4: Reply to Issue #10 with the PR link**

After the PR is created, retrieve its URL and use it in the issue comment:

~~~bash
PR_URL=$(gh pr view --repo im-pingo/liveforge --json url --jq .url)
gh issue comment 10 --repo im-pingo/liveforge --body "已实现单轨 E-RTMP 支持，包含 FourCC 音视频头、AMF0 能力协商、classic/enhanced 输出协商及旧客户端回退。多轨、HDR、ModEx 暂不在本阶段。PR: ${PR_URL}"
~~~

Do not close the issue automatically unless the PR is merged or the user
explicitly requests closure.
