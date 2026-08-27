# WHIP H.265 + Opus Playback Verification

This recipe verifies a browser WHIP publisher carrying H.265/HEVC video and Opus audio through every Console Preview protocol. The checked-in sample configuration disables authentication and TLS and uses `admin/admin`; use it only on loopback and never expose it publicly unchanged.

## Prerequisites

- Go 1.26 or newer.
- FFmpeg development libraries for the complete `audiocodec` build profile.
- A browser and operating system combination that exposes an H.265 WebRTC encoder and decoder.
- Reachable API, HTTP stream, and WebRTC listeners. Production camera and microphone access requires HTTPS or another browser-compatible secure context.

Build and start the complete audio profile:

```bash
CGO_ENABLED=1 go build -tags audiocodec -o bin/liveforge ./cmd/liveforge
./bin/liveforge -c configs/liveforge.yaml
```

The `audiocodec` profile is important for FLV- and TS-based outputs: Opus is passed through by compatible fMP4/WHEP paths, while incompatible output containers request a shared AAC transcode. Shared HTTP muxers keep source video on a direct reader starting at the atomic live cursor and start an independent audio-only transcode reader at the cached GOP source position, so converted history cannot duplicate cached or live video. If the same source later switches to AAC, the muxer transfers its single audio owner to the direct reader at the AAC sequence header and stops the old source-codec decoder before it consumes AAC bytes. Without the tag, those outputs may be video-only instead of carrying transcoded audio.

## Publish

1. Open `http://127.0.0.1:8090/console` for the local sample configuration.
2. Click **+ WebRTC Publish**.
3. Enter a stream key such as `live/h265-opus`.
4. Select the camera, microphone, and **H.265 (HEVC)** video codec.
5. Click **Start Publishing**.

The Streams row must report `H265` and `Opus`. If H.265 is unavailable or negotiation fails, verify browser/platform codec support before debugging the server.

## Preview

Click **Preview** and verify each mode:

| Preview mode | Expected path |
| --- | --- |
| HTTP-FLV | HTTP FLV with compatible audio selection |
| WS-FLV | WebSocket FLV with compatible audio selection |
| HTTP-TS | HTTP MPEG-TS with compatible audio selection |
| FMP4 | Fragmented MP4; Opus can pass through |
| HLS | HLS or LL-HLS according to `http_stream.llhls.enabled`; the Console enables Hls.js low-latency part consumption |
| DASH | Separate fMP4 representations; Opus can pass through; the Console keeps one fragment of live delay |
| WebRTC | WHEP realtime mode; waits for the next live keyframe |
| WebRTC-Live | WHEP Live mode; replays an atomic GOP snapshot, then continues from the matching ring-buffer cursor |

The plain HTTP FMP4 path establishes a near-zero timeline when the shared muxer starts, with the first cached GOP rebased once and later fragments preserving their relative DTS and PTS. Subscribers share already-muxed bytes, so a late subscriber is not independently rebased to zero. The Console appends fragments to an MSE `segments` SourceBuffer so explicit `tfdt` values and signed HEVC B-frame composition offsets remain authoritative, then starts playback at that subscriber's first buffered timestamp. An MSE failure aborts and releases the live response; a finite response reaches `endOfStream()` only after queued appends drain.

For every mode, require all of the following:

- A decoded first frame and non-zero `videoWidth`/`videoHeight`.
- `currentTime` increases over at least two seconds.
- The media element has no error.
- WHEP statistics show increasing decoded frames and packets, non-zero FPS, and decoded keyframes. Confirm audio RTP statistics are present for the Opus track.

Do not accept the status label `Playing` by itself. Negotiation or MSE attachment can succeed while the decoder is stalled.

## Diagnostics

Confirm the source codecs and frame flow through the management API:

```bash
curl -H "Authorization: Bearer $API_TOKEN" \
  http://127.0.0.1:8090/api/v1/streams/live/h265-opus
```

Check these failure signatures:

- WHEP receives packets but decodes zero frames: inspect HEVC VPS/SPS/PPS conversion and confirm the keyframe carries Annex-B parameter sets.
- WHEP Live renders only the cached GOP: verify the GOP cache and source-ring cursor are captured atomically before cache replay, source video always uses that cursor, and a separate transcode reader contributes only negotiated target audio.
- FMP4 fails on the second fragment or reports a large starting timestamp: verify the shared muxer rebases both DTS and PTS by one baseline, the Console SourceBuffer remains in `segments` mode, and playback seeks to the first buffered range.
- HLS fails at a segment boundary: verify each advertised independent segment starts at a video keyframe. The initial manifest must omit completed PART tags, while blocking reloads retain the latest completed PART identities so Hls.js neither appends cold-start media twice nor reloads a full segment after consuming its parts.
- WHIP H.265 + Opus has a fixed audio offset across HLS, DASH, FLV, or TS: inspect the WHIP session timeline. Audio and video must be mapped onto one session clock from their packet arrival offsets; each track must not independently reset DTS to zero. For transcoded output, confirm the audio reader starts at the cached GOP source position.
- WHEP video arrives in `80ms + 0ms` bursts after source audio pauses: the WebRTC audio transcode worker must wait on its ring reader condition and must not consume the shared source playback wakeup. Run `CGO_ENABLED=1 go test -tags audiocodec ./module/webrtc -run TestWHEPJitterDiagnostic/video_audio_transcode -count=1 -timeout=180s -v` and confirm zero bursts, no sequence gaps, and a stable jitter trend.
- HLS or LL-HLS pauses after cached playback: verify the segmenter-specific compatibility path uses its combined historical transcode reader and filters only duplicated cached video by video DTS. Shared HTTP FLV/TS/fMP4 workers instead use independent direct-video and transformed-audio readers. The bundled Hls.js requires one completed segment in its initial manifest, then consumes low-latency parts; the server must not wait for the old three-segment buffer. The live reader begins at the atomic snapshot cursor, so no cross-track DTS watermark should discard a valid frame.
- DASH startup takes multiple GOPs or stalls after a segment: the initial MPD should return after one complete keyframe-bounded segment, advertise a `minimumUpdatePeriod` of at most two seconds, preserve measured GOP durations in `SegmentTimeline`, and use one fragment of player live delay. A cold DASH manager still needs the next keyframe to close its first segment; for a 30 fps publisher with `keyint=250`, this bounded wait can approach 8.3 seconds but must not multiply across three segments.
- FLV/TS playback has no audio: confirm the binary was built with `CGO_ENABLED=1 -tags audiocodec` and that FFmpeg libraries were found.
