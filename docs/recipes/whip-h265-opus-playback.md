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

The `audiocodec` profile is important for FLV- and TS-based outputs: Opus is passed through by compatible fMP4/WHEP paths, while incompatible output containers request a shared AAC transcode. Shared HTTP muxers keep source video on a direct reader starting at the atomic live cursor and start an independent audio-only target-AAC reader at the cached GOP source position, so converted history cannot duplicate cached or live video. They retain that target reader even when startup audio is already direct AAC; a later G.711/Opus epoch can therefore resume transformed AAC without restarting the worker or losing direct video. Source audio codec changes within one publisher generation are tracked as separate epochs. Snapshot readers reject retained audio below the startup epoch floor, so late readers do not emit stale audio or move DTS backward before current output. A transcoder-generated AAC startup header only declares the transformed track and never transfers ownership; real source AAC remains the direct owner, and a later G.711/Opus epoch switches to a fresh transformed pipeline. Decoder, encoder, resampler, timestamp, and PCM state never cross incompatible epochs. An unsupported startup or intermediate epoch leaves the shared producer open; a later supported epoch emits a current target header and media to existing and new readers. Each worker retains its own target reader until shutdown, so one FLV/TS/fMP4 handoff cannot close the shared track used by peers, RTMP/WHEP, or segmenters. Without the tag, those outputs may be video-only instead of carrying transcoded audio.

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
| WebRTC | WHEP realtime mode; explicit low-latency path that waits for the next live keyframe |
| WebRTC-Live | WHEP Live mode; replays an atomic GOP snapshot, then continues from the matching ring-buffer cursor |

When the WHEP mode is omitted, the server and Console use `mode=live` so an available
GOP keyframe is sent immediately. After the SDP answer, read the session `Location`
and append `/status` to inspect `feed.state`: `waiting_keyframe`, `playing`,
`no_media_input`, `media_stalled`, `codec_mismatch`, `sample_write_failed`,
`target_audio_failed`, `generation_ended`, and `closed` are distinct states. The status also contains
`expected_video`, `expected_audio`, `first_media_at`, the stable millisecond
`first_media_wait_ms`, `last_video_at`, `last_audio_at`, the startup
generation/cursor, sent and dropped media counters, and a bounded `last_error`.
Dropped counters cover negotiated tracks only, and session close captures one
final monotonic transport snapshot before storing terminal status.
Both expected tracks must advance before a mixed feed reports `playing`. Complete
startup silence becomes `no_media_input` after eight seconds; after startup, any
expected track that does not advance for eight seconds makes the feed
`media_stalled`. A mixed realtime feed remains `waiting_keyframe` while audio
advances but video interframes are discarded before the first IDR. Recovery
requires every stale expected track to advance again. Console derives the displayed
stale media names from server timestamps instead of listing every expected kind.
Every real feed-state transition emits one structured log containing generation,
cursor, mode, previous/next state, and a bounded error when present. Same-state
per-frame updates do not emit transition logs.

An active WHEP reader overwrite is recovered per reader. The retained atomic
read result is never sent, and only that reader advances to live. One
condition-backed pump owns each reader's readiness check, atomic read, and
live advance; shutdown cancels and joins both pumps before releasing target
audio ownership once. A source
overwrite keeps established direct or transformed audio moving while video
returns to `waiting_keyframe`; video pacing/DTS/PTS state is reset and recovery
starts with the latest same-generation parameter sets plus a keyframe. An
audio-only feed resumes at the next live audio frame. A transformed target-audio
overwrite leaves clean source video continuous and resumes at the next valid
target frame. If that expected target reader closes while the publisher
generation is active, the feed terminates promptly as `target_audio_failed`
instead of waiting for `media_stalled` or silently becoming video-only. Each
overwrite warning identifies `reader=source|target_audio`, the exact
`overwritten` count, and `action=wait_keyframe|continue_audio`.

WHEP negotiation is fail closed per requested source media kind. A source track on
a non-zero receiving offer m-line that has no compatible codec returns HTTP 415;
an internal local-track or AddTrack failure returns HTTP 500. Setup releases its
subscriber lease, connection slot, PeerConnection, and session entry on either
failure. A source kind omitted by the offer, disabled with port zero, or marked
`inactive`/`sendonly` is intentionally omitted and does not block another compatible
requested kind. Media-level direction overrides session-level direction, and codec
matching requires an exact `rtpmap` name on a payload listed by that m-line.

When the source audio codec is not offered directly, WHEP selects the first offered
target in this order: Opus, PCMU, then PCMA. The target is selected only when the
running process can convert the source through its configured `audiocodec` registry;
portable no-CGO builds therefore still return 415 for a source that needs conversion.
The answer advertises Opus as 48 kHz stereo or G.711 as 8 kHz mono. A converted
reader setup failure is reported as `target_audio_failed` and is never silently
downgraded to video-only. `GET /api/v1/streams/{stream_key}` exposes the active
conversion as `transcode_tasks`, including source/target codec, scope, state,
subscriber count, and a bounded error. The Console renders that task beneath the
stream's codec tags.

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
- WHEP sends a post-gap P-frame, stalls after a source overwrite, or loses only transformed audio: verify atomic source/target read results are used, the retained result is discarded, only the named reader advances to live, source recovery requests the TrackSender keyframe gate with current parameter sets, and active target-audio EOF reports `target_audio_failed`.
- FMP4 fails on the second fragment or reports a large starting timestamp: verify the shared muxer rebases both DTS and PTS by one baseline, the Console SourceBuffer remains in `segments` mode, and playback seeks to the first buffered range.
- HLS fails at a segment boundary: verify each advertised independent segment starts at a video keyframe. The initial manifest must omit completed PART tags, while blocking reloads retain the latest completed PART identities so Hls.js neither appends cold-start media twice nor reloads a full segment after consuming its parts.
- WHIP H.265 + Opus has a fixed audio offset across HLS, DASH, FLV, or TS: inspect the WHIP session timeline. Audio and video must be mapped onto one session clock from their packet arrival offsets; each track must not independently reset DTS to zero. For transcoded output, confirm the audio reader starts at the cached GOP source position.
- WHEP video arrives in `80ms + 0ms` bursts after source audio pauses: the WebRTC audio transcode worker must wait on its ring reader condition and must not consume the shared source playback wakeup. Run `CGO_ENABLED=1 go test -tags audiocodec ./module/webrtc -run TestWHEPJitterDiagnostic/video_audio_transcode -count=1 -timeout=180s -v` and confirm zero bursts, no sequence gaps, and a stable jitter trend.
- `lf-test` realtime WHIP pacing rebases the current packet when processing falls behind the media timeline. This prevents an old absolute anchor from emitting a catch-up burst; the regression is covered by `TestWHIPRealtimePacerRebasesAfterFallingBehind`.
- HLS or LL-HLS pauses after cached playback: verify the segmenter-specific compatibility path uses its combined historical transcode reader and filters only duplicated cached video by video DTS. Shared HTTP FLV/TS/fMP4 workers instead use independent direct-video and transformed-audio readers. The bundled Hls.js requires one completed segment in its initial manifest, then consumes low-latency parts; the server must not wait for the old three-segment buffer. The Console starts playback only after the media element has buffered a small lead, so `MANIFEST_PARSED` cannot trigger an initial play/pause hiccup. The live reader begins at the atomic snapshot cursor, so no cross-track DTS watermark should discard a valid frame.
- DASH startup takes multiple GOPs or stalls after a segment: the initial MPD should return after one complete keyframe-bounded segment, advertise a `minimumUpdatePeriod` of at most two seconds, preserve measured GOP durations in `SegmentTimeline`, and use one fragment of player live delay. A cold DASH manager still needs the next keyframe to close its first segment; for a 30 fps publisher with `keyint=250`, this bounded wait can approach 8.3 seconds but must not multiply across three segments.
- FLV/TS playback has no audio: confirm the binary was built with `CGO_ENABLED=1 -tags audiocodec` and that FFmpeg libraries were found. For a same-session codec transition, confirm direct-AAC startup still has a shared target reader, generated target headers never claim direct ownership, a later G.711/Opus epoch creates fresh transform state, and unsupported epochs neither close the mapped track nor poison later readers. A late reader must begin at its snapshot epoch floor rather than retained older audio.
