# Final Review Media Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Completed steps are marked with `[x]`.

**Goal:** Correct fMP4 AAC timing, generation-managed fMP4 recording fallback, and GB28181 response-only cleanup errors.

**Architecture:** Keep the existing fMP4 and RecordSession ownership boundaries. Derive AAC metadata once in `Muxer.Init` and reuse the resulting audio timescale for every media fragment. Have generation-bound recording select direct AAC or an optional AAC transform before configuring the writer; unsupported audio is excluded from a video-only fMP4. Make GB28181 cleanup preserve both transport and SIP response failures.

**Tech Stack:** Go 1.26+, existing `pkg/muxer/fmp4`, `module/record`, `module/gb28181`, `audiocodec` build tag, `go test`, and repository agent-document checks.

**Spec:** `docs/superpowers/specs/2026-08-27-media-startup-cache-correctness-design.md` plus the current final-review requirements.

## Global Constraints

- Work directly on the current `codex/liveforge-completion` branch and preserve unrelated dirty changes.
- No subagents were used. Focused regression tests and the final default and
  tagged repository-wide suites are part of this fix wave's verification.
- Every behavior change gets a regression test written and observed failing before its production fix.
- The default `go test ./...` path remains dependency-free; tagged audio behavior is verified only in focused tagged packages.
- Run `gofmt` and `tools/check-agent-docs_test.sh`; run the diff-aware documentation gate without starting a broad test suite.
- Do not commit secrets, generated binaries, coverage files, or unrelated existing changes.

### Task 1: Derive fMP4 AAC metadata and media timing

**Files:**
- Modify: `pkg/muxer/fmp4/fmp4_test.go`
- Modify: `pkg/muxer/fmp4/muxer.go`
- Modify: `pkg/muxer/fmp4/init_segment.go` if the shared init path needs the resolved values

**Interfaces:**
- Consumes: `aac.ParseAudioSpecificConfig`, `Muxer.Init`, `Muxer.WriteSegment`, and `Demuxer.Parse`.
- Produces: an AAC fMP4 init whose audio `mdhd` timescale and audio sample-entry metadata match the ASC or explicit arguments, and fragments whose decode timestamps use that same timescale.

- [x] **Step 1: Write the failing test**

```go
func TestMuxerDerivesAACConfigAndTimescaleWhenInitArgsOmitted(t *testing.T) {

	// AAC-LC, 48000 Hz, stereo: audioObjectType=2, frequencyIndex=3, channels=2.
	audioHeader := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC,
		avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x11, 0x90})
	muxer := NewMuxer(0, avframe.CodecAAC)
	initSegment := muxer.Init(nil, audioHeader, 0, 0, 0, 0)
	first := muxer.WriteSegment([]*avframe.AVFrame{avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{1, 2, 3},
	)})
	second := muxer.WriteSegment([]*avframe.AVFrame{avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{4, 5, 6},
	)})
	demuxer, err := NewDemuxer(initSegment)
	if err != nil { t.Fatal(err) }
	firstFrames, err := demuxer.Parse(first)
	if err != nil { t.Fatal(err) }
	secondFrames, err := demuxer.Parse(second)
	if err != nil { t.Fatal(err) }
	firstDTS := onlyAudioDTS(t, firstFrames)
	secondDTS := onlyAudioDTS(t, secondFrames)
	if got := secondDTS - firstDTS; got != 20 {
		t.Fatalf("demuxed AAC DTS interval = %d ms, want 20 ms", got)
	}
	if got := audioTrackTimescale(t, initSegment); got != 48000 {
		t.Fatalf("AAC media timescale = %d, want 48000", got)
	}
}
```

- [x] **Step 2: Run the focused test to verify RED**

Run: `go test ./pkg/muxer/fmp4 -run TestMuxerDerivesAACConfigAndTimescaleWhenInitArgsOmitted -count=1`

Expected: FAIL because omitted AAC parameters leave the media fragment on a zero/raw-millisecond timescale while init construction falls back to 44100.

- [x] **Step 3: Implement the minimal fix**

In `Muxer.Init`, resolve omitted AAC sample rate and channels from `audioSeqHeader.Payload`. Resolve supported headerless defaults before building init: Opus `48000/2`, MP3 `44100/2`, and the existing AAC fallback only when no ASC is available. Preserve positive explicit `sampleRate` and `channels`. Store the resolved sample rate in `m.audioSampleRate` and pass resolved rate/channels to `BuildInitSegment`.

- [x] **Step 4: Run focused fMP4 verification**

Run: `go test ./pkg/muxer/fmp4 -run 'Test(MuxerDerivesAACConfigAndTimescaleWhenInitArgsOmitted|MuxerOpusSingletonFragmentsHaveContinuousTimeline|MuxerFlow)' -count=1`

Expected: PASS.

### Task 2: Make generation-managed fMP4 recording playable without AAC headers

**Files:**
- Modify: `module/record/record_test.go`
- Modify: `module/record/session.go`
- Modify: `module/record/file_writer.go` only if output metadata must be passed explicitly
- Modify: `module/record/record_audiocodec_test.go` or `module/record/record_no_audiocodec_test.go` only for focused tag-specific coverage

**Interfaces:**
- Consumes: `StreamStartupSnapshot`, `TranscodeManager.GetOrCreateReaderAtFromHistory`, `audiocodec.Registry`, and `FileWriter.SetExpectedTracks`.
- Produces: fMP4 recordings with direct AAC when present, AAC output for non-AAC audio when the optional path is available, and completed video-only recordings when that path is unavailable.

- [x] **Step 1: Write the failing generation-managed regression**

Construct an active stream publisher declaring H.264 plus Opus, write the H.264 sequence header and interleaved video/audio frames, create `NewRecordSession` after the generation is active, stop it, and assert the status is `RecordingCompleted`, the file contains `avc1`, and it does not contain the direct unsupported `Opus` sample entry. The assertion accepts either `mp4a` for an available AAC transform or no audio track for the dependency-free build.

- [x] **Step 2: Run the focused test to verify RED**

Run: `go test ./module/record -run TestGenerationManagedFMP4RecordHandlesHeaderlessNonAACAudio -count=1`

Expected: FAIL with `recording codec configuration is incomplete` because MP3/Opus is currently treated as a direct fMP4 audio track despite having no sequence header.

- [x] **Step 3: Implement the minimal session fix**

Treat only AAC as direct fMP4 audio. For every other declared audio codec, try the existing generation-bound AAC reader when `CanTranscode` and the generated AAC sequence header are available. Set expected tracks and write headers using the resolved output codec, preserving the existing G.711-to-AAC path. If no transform is available, set output audio codec to zero before configuring the writer so audio is filtered and video initializes normally.

- [x] **Step 4: Run focused recording verification with and without the tag**

Run: `go test ./module/record -run 'Test(GenerationManagedFMP4RecordHandlesHeaderlessNonAACAudio|FMP4RecordSessionDropsUnsupportedAudioWithoutTranscoder)' -count=1`

Run: `CGO_ENABLED=1 go test -tags audiocodec ./module/record -run 'Test(GenerationManagedFMP4RecordHandlesHeaderlessNonAACAudio|FMP4RecordSessionTranscodesG711Audio)' -count=1`

Expected: PASS in both environments.

### Task 3: Preserve SIP status in GB28181 cleanup errors

**Files:**
- Modify: `module/gb28181/lab_test.go`
- Modify: `module/gb28181/lab.go`

**Interfaces:**
- Consumes: `gbLabSession.cleanup`, `unregister`, and `joinSIPResponseError`.
- Produces: stored terminal cleanup errors containing the non-2xx unregister status even when transport returned no error.

- [x] **Step 1: Write the failing cleanup regression**

Use the existing real SIP lab fixture or a local sipgo UDP peer that returns a non-2xx final response to `REGISTER` with `Expires: 0`. Stop the lab, read its terminal snapshot, and assert `LastError` contains the exact SIP status, such as `SIP response 503 Service Unavailable`.

- [x] **Step 2: Run the focused test to verify RED**

Run: `go test ./module/gb28181 -run TestGBLabCleanupPreservesUnregisterSIPStatus -count=1`

Expected: FAIL because cleanup currently joins only the transport error and loses a response-only failure.

- [x] **Step 3: Implement the minimal cleanup fix**

Replace the cleanup join with `errors.Join(cleanupErr, joinSIPResponseError(err, response))` when unregister fails or returns a non-2xx response, guarding nil responses through the helper.

- [x] **Step 4: Run focused GB28181 verification**

Run: `go test ./module/gb28181 -run 'TestGBLabCleanupPreservesUnregisterSIPStatus|TestGBLabPublishShutdownDoesNotUnderflowSIPUDPRefs' -count=1`

Expected: PASS.

### Task 4: Documentation, formatting, and bounded verification

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `llms-full.txt`
- Modify: `agent-manifest.json`
- Modify: `docs/recipes/recording-dvr-management.md`
- Create: `.superpowers/sdd/2026-08-27-media-startup-cache-correctness/final-review-fix-report.md`

- [x] **Step 1: Synchronize behavior documentation**

State that fMP4 AAC init derives omitted rate/channels from ASC and uses one matching media timescale; fMP4 recording passes AAC directly, converts non-AAC through optional audiocodec/FFmpeg when available, and otherwise emits playable video-only output. Keep G.711-to-AAC and portable no-CGO limitations explicit.

- [x] **Step 2: Format and inspect the diff**

Run: `gofmt -w pkg/muxer/fmp4/muxer.go pkg/muxer/fmp4/fmp4_test.go module/record/session.go module/record/record_test.go module/gb28181/lab.go module/gb28181/lab_test.go`

Run: `git diff --check`

- [x] **Step 3: Run verification gates**

Run: `tools/check-agent-docs_test.sh`

Run: `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`

Run: `python3 -m json.tool agent-manifest.json >/dev/null`

The final verification ran `go test ./...` and the repository-wide tagged race
suite in addition to the focused package tests.

- [x] **Step 4: Write the full report before the final response**

Record the reviewer findings, RED commands and observed failures, GREEN commands and observed results, documentation changes, commit identity, exact bounded test output, and concerns in `.superpowers/sdd/2026-08-27-media-startup-cache-correctness/final-review-fix-report.md`.
