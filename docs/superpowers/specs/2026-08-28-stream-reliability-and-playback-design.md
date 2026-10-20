# LiveForge Stream Reliability and Playback Design

**Date:** 2026-08-28\
**Status:** Approved for execution\
**Scope:** WebRTC first-frame delivery and real H.264, core caches and lifecycle, connection capacity and protocol egress, runtime configuration and metrics, Console and cross-protocol acceptance

## Objectives

1. WHEP playback created through the Console and API reliably receives a decodable first frame within a normal GOP interval, without misreporting "waiting for a keyframe" as "no media".
2. Inputs such as SIP/GB28181/WHIP/RTMP can undergo repeatable cross-protocol verification through WebRTC and other enabled outputs after passing through the server.
3. Caches, connection limits, publisher generations, RTP reception/transmission, and module shutdown remain bounded and free of data races during errors, concurrency, and restarts.
4. Hot paths reduce unnecessary locks, reflection, allocations, and system calls while preserving media timing and protocol semantics, with repeatable benchmarks.
5. The Config page, protocol labs, and Storage page show only implemented and verifiable capabilities; configuration sources, permissions, sensitive information, and restart semantics remain consistent.

## Design Decisions

### 1. WebRTC Startup Model

WHEP uses a one-time `StreamStartupSnapshot` containing publisher identity, generation, MediaInfo, sequence headers, interleaved GOP replay frames, LiveCursor, and GenerationDone. `mode=live` sends the complete available GOP from that snapshot, then reads new frames from LiveCursor; `mode=realtime` sends no replay frames but waits for a subsequent video keyframe. For audio-only streams, both modes start directly from LiveCursor without waiting for a video keyframe or introducing a separate audio cache.

The Console's default WHEP link uses `mode=live`; explicit `mode=realtime` remains available and displays a "waiting for a keyframe" status. First-frame gating states are defined as `waiting_keyframe`, `playing`, `no_media_input`, `codec_mismatch`, `sample_write_failed`, `generation_ended`, and `closed`. The feed loop writes a structured log for every state change and associates first-frame time, wait duration, generation, cursor, dropped frames, audio/video frames sent, RTP counts, and the last error with session diagnostics; `WriteSample` errors are no longer silently discarded.

H.264/H.265 input uses a unified access-unit conversion from AVCC/HVCC to Annex-B. Keyframes carry cached SPS/PPS/VPS; empty access units, missing parameter sets, negotiated codec mismatches, and send errors each fail closed. Browser regressions are judged by actual Chromium H.264 decoding: `readyState`, video dimensions, advancing `currentTime`, audio frame counts, and media error checks must all meet their criteria. SDP success or `ontrack` alone cannot count as a pass.

### 2. Core Caches and Generations

The GOP cache remains a single replay cache that starts with a video keyframe and contains interleaved audio within that GOP; audio-only streams use only the ring live cursor. In addition to `GOPCacheNum`, the GOP cache adds per-GOP frame-count, duration, and byte limits. When any of the three limits is reached, it retains playable content beginning at the current keyframe and stops growing. Configuration values undergo schema and runtime validation, with defaults providing a bounded version of the current behavior.

`Stream.WriteFrame` preserves single-writer ordering between generation/publisher validation and ring writes, while moving statistics updates that do not need protection out of the broad lock. Publisher identity uses stable interface identity or an explicit token, without per-frame reflection. All subscriber admission, replay readers, and asynchronous manager cleanup must carry a generation; stop events from an old generation must not clean up resources belonging to a new generation.

### 3. Resource and Connection Boundaries

`AcquireConn` uses a CAS loop to enforce a strict maximum connection limit, and all HTTP, WebRTC, DVR, and protocol session paths use the same release-once convention. DVR requests acquire connection quota before creating a session and release it on cancellation, errors, and normal completion. RingBuffer guards against invalid capacity at construction time to prevent division by zero or out-of-bounds access when the utility package is called directly.

The GB28181 RTP receiver makes an owned copy of payloads passed from the UDP buffer to the reorder queue. DeviceRegistry exposes only immutable snapshots, and both Stop and Close are idempotent. HTTP streaming sets header/write deadlines and responds to request cancellation; waiting paths such as HLS/DASH/LL-HLS use context-aware conditions instead of fixed sleeps that cannot be canceled.

### 4. Performance and Observability

Protocol egress prioritizes packet/fragment buffer reuse and uses `net.Buffers` or batched writes where ownership remains unchanged. Configuration refresh keeps source I/O serialized, while parsing/hash/diff/application do not block the next scheduling cycle and repeated versions return promptly. Prometheus stream labels use a controlled, configurable collection policy; by default, arbitrary high-cardinality stream keys must not produce an unbounded number of time series. Required stream details are available through the management API.

Every optimization requires behavior tests and benchmarks. Benchmarks compare relative changes and cannot replace capacity acceptance checks. Error logs must not contain source URL credentials, bearer tokens, or SIP passwords. The rate limiter reads forwarded headers only when trusted proxies are explicitly configured; otherwise, it uses RemoteAddr.

### 5. Console and Configuration UX

The Console's top level is divided into Workspace, Operations, and System. Config and Security belong only to System; Streams, GB28181, SIP Calls, and Storage belong to Workspace; Cluster belongs to Operations. The Config page separately displays the complete redacted effective document, desired source document, schema, source details, pending restart, validation results, and apply status, without presenting a read-only source as editable.

Both publish and receive modes in the SIP/GB28181 labs display source/target streams, RTP/RTCP, audio/video frames, generation, codec, errors, and cross-protocol playback links. Labs use only loopback fake devices and do not depend on external platforms. A capability matrix clearly labels H.264/G.711/Opus/AAC output capabilities as "available, requires FFmpeg, video-only, or unsupported." Recording defaults to fMP4/MP4; Storage shows a disabled state rather than a module error when the record module is absent.

## Phases and Acceptance

### Phase A: WHEP First Frame and H.264

- Add tests for realtime/live/audio-only/sparse keyframes/no cache/sample-write failures.
- Add browser regressions for real GB28181, SIP, and WHIP H.264 inputs to WHEP.
- Correct the default Console mode and status display.
- Acceptance: default WHEP playback advances `currentTime` within 8 seconds; logs and UI distinguish four categories of root causes on failure.

### Phase B: Core Reliability

- Add the three GOP limits, guards against invalid ring capacity, and publisher identity hot-path optimization.
- Fix generation-aware cleanup, publisher timeouts, historical pointers in HTTP registries, and subscriber admission.
- Acceptance: race tests, publisher replacement stress tests, and cache memory-bound tests all pass.

### Phase C: Connections, RTP, and Shutdown

- Fix strict connection limits, DVR quotas, GB28181 packet ownership, DeviceRegistry snapshots, idempotent close, and HTTP cancellation.
- Acceptance: concurrent connection admission never exceeds the configured limit, all paths release resources, and `go test -race` reports no data races or close panics.

### Phase D: Performance and Configuration

- Reduce locking on hot paths, reuse egress buffers, improve configuration refresh scheduling, define the metrics high-cardinality policy, and add benchmarks.
- Fix config source redaction and trusted proxy semantics.
- Acceptance: benchmark reports are retained in the PR/change description, and functional tests and configuration source contract tests pass.

### Phase E: Console and Release Acceptance

- Complete page grouping, Config support for all fields and source adapters, the protocol lab capability matrix, and Storage disabled/playback states.
- Update the manifest, llms, README, schema, OpenAPI, and recipes.
- Acceptance: complete SIP/GB28181 publish/receive, cross-protocol playback, and recording playback locally without external platforms, and pass the full build, race tests, documentation checks, and CI.

## Non-Goals

- This iteration does not implement WebRTC simulcast layer selection; configuration remains marked deferred/unsupported.
- Do not describe builds without FFmpeg as supporting transcoding of non-AAC audio; dependency-free builds guarantee only their declared codecs and video-only fallback.
- Do not change the published stream-key escaping, permission, or bearer token semantics unless tests prove the current behavior violates the security contract.

