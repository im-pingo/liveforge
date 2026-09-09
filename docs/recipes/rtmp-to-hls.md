# RTMP Publish To HLS Playback

## Start the server

Use the source or Compose recipe first. The default HTTP stream listener is `:8080` and RTMP listener is `:1935`.

## Publish

```bash
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://127.0.0.1:1935/live/demo
```

`-c copy` preserves the input codecs. For a repeatable browser-compatible live test, keep this publisher running while checking all three Console tabs:

```bash
ffmpeg -re -f lavfi -i 'testsrc2=size=640x360:rate=25' \
  -f lavfi -i 'sine=frequency=1000:sample_rate=48000' \
  -c:v libx264 -preset ultrafast -tune zerolatency -pix_fmt yuv420p \
  -g 50 -keyint_min 50 -c:a aac -ar 48000 -ac 2 \
  -f flv rtmp://127.0.0.1:1935/live/demo
```

This test needs an FFmpeg executable with libx264 and lavfi support. Stop the publisher with Ctrl-C after verification. The sample configuration enables recording, so live tests also create recordings at its configured path.

Without an explicit `-g`, libx264 may use a 250-frame GOP, about 8.33 seconds at 30 fps. The sample's 300-frame GOP cache counts both video and audio, so a H.264+AAC GOP can exceed the cache after about 3.9 seconds. Startup now recovers a complete sequence from the retained ring before joining live frames; the persistent GOP cache remains bounded. If its keyframe has already left the ring, playback waits for the next keyframe. `-g 60 -keyint_min 60` at 30 fps is useful for predictable two-second keyframe intervals, especially with explicit WebRTC-Realtime mode, which intentionally bypasses cached startup.

Console hls.js playback disables automatic element playback until it has at least 0.8 seconds of contiguous buffered media at the playhead. Buffered media after a gap does not count toward this threshold. The 1.5-second live latency target leaves room for incoming LL-HLS parts; it is not a network-independent latency guarantee. Native Safari HLS keeps its browser-managed startup behavior.

## Play

```text
http://127.0.0.1:8080/live/demo.m3u8
```

LL-HLS uses the same playlist path when `http_stream.llhls.enabled` is true. Confirm the stream is live before diagnosing a player with:

```bash
curl http://127.0.0.1:8090/api/v1/streams/live/demo
```

The Console reads the active HTTP listener from `GET /api/v1/server/info`. Diagnose browser playback separately from RTMP ingest:

- For H.264+AAC, HLS and DASH preserve audio without the native transcoding build. Allow the first keyframe and segments to arrive, then verify decoded dimensions and an advancing playback clock.
- Browsers do not normally offer AAC/MP3 for WebRTC. When server info explicitly reports `capabilities.audio_transcoding=false`, Console mixed-stream WHEP previews omit that audio m-line and show a persistent video-only notice. Audio-only AAC/MP3 previews report the missing capability. Missing capability/codec metadata and unknown codecs retain normal negotiation.
- For WebRTC sound from AAC, run a source build with `CGO_ENABLED=1 go build -tags audiocodec -o bin/liveforge ./cmd/liveforge`, FFmpeg development libraries, and `audio_codec.enabled: true`; installing the FFmpeg executable alone does not add transcoding to a portable binary. WebRTC starts muted and exposes Unmute after receiving an audio track.
- External WHEP clients still receive 415 when they request unsupported source audio. An intentionally omitted audio track allocates no audio transcode reader.

```bash
curl -sv --noproxy '*' http://127.0.0.1:8080/live/demo.m3u8
```

The response should be from LiveForge with an HLS content type. A `404` from `nginx` or another server means the loopback media port is occupied by a different process; `ffplay` on RTMP and WHEP on their separate ports can still succeed. Release the conflicting port or change `http_stream.listen`, then reload the Console.

HLS, LL-HLS, and DASH wait for the active publisher's required codec sequence headers before creating a playable segmenter. If a publisher is connected but has not sent its video or AAC configuration header yet, the playlist can remain empty until that header arrives; this avoids advertising a segment initialized with the wrong codec metadata.

When a publisher disconnects, LiveForge immediately retires that generation's HLS, LL-HLS, and DASH managers from new request lookup, then lets each manager drain frames already accepted through the captured generation boundary and finalize them once. A replacement publisher receives a distinct manager. Server or HTTP module shutdown still force-stops and joins active or draining managers, and a stopped LL-HLS manager releases blocking playlist reloads.

HTTP-FLV and fMP4 use a bounded one-second initialization wait. If the active publisher has not supplied enough codec metadata by then, LiveForge returns HTTP 503 instead of an empty successful response; retry after the sequence header arrives.

Continuous HTTP-FLV, HTTP-TS, fMP4, and matching WebSocket outputs fail closed if either their direct/transformed media input or shared muxed-output ring is overwritten. Bytes already delivered remain visible, but LiveForge discards the retained post-gap value and ends that response instead of bridging the media gap. WebSocket clients receive a retry-later continuity-loss close; a clean producer end remains a normal close.

HLS and LL-HLS instead discard their unfinished segment/part, advance to live input in the same publisher generation, refresh sequence headers and container state, and put one `#EXT-X-DISCONTINUITY` before the first recovered output. If the refreshed audio plan changes between direct and shared transformed input, the old reader is closed and released once and the replacement opens at the refreshed live cursor without GOP-history replay. Video emits nothing until the next keyframe, including when video first appears in the refreshed same-generation topology; audio-only resumes on the next live audio frame. LL-HLS abandons the affected MSN, removes its current-part URLs, and wakes blocked reloads. Retained fMP4 media keeps its matching immutable versioned init bytes; those init URLs remain available until the corresponding media leaves the playlist window, while unknown or evicted versions return 404. DASH does not continue across the gap in its existing single Period: it keeps already completed segments but retires that manager, so clients must reacquire playback. An unexpectedly closed transformed reader while the source generation remains active follows the same no-flush terminal rule rather than clean end-of-generation finalization.
