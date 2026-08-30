# RTMP Publish To HLS Playback

## Start the server

Use the source or Compose recipe first. The default HTTP stream listener is `:8080` and RTMP listener is `:1935`.

## Publish

```bash
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://127.0.0.1:1935/live/demo
```

## Play

```text
http://127.0.0.1:8080/live/demo.m3u8
```

LL-HLS uses the same playlist path when `http_stream.llhls.enabled` is true. Confirm the stream is live before diagnosing a player with:

```bash
curl http://127.0.0.1:8090/api/v1/streams/live/demo
```

The Console reads the active HTTP listener from `GET /api/v1/server/info`. Diagnose browser playback separately from RTMP ingest:

```bash
curl -sv --noproxy '*' http://127.0.0.1:8080/live/demo.m3u8
```

The response should be from LiveForge with an HLS content type. A `404` from `nginx` or another server means the loopback media port is occupied by a different process; `ffplay` on RTMP and WHEP on their separate ports can still succeed. Release the conflicting port or change `http_stream.listen`, then reload the Console.

HLS, LL-HLS, and DASH wait for the active publisher's required codec sequence headers before creating a playable segmenter. If a publisher is connected but has not sent its video or AAC configuration header yet, the playlist can remain empty until that header arrives; this avoids advertising a segment initialized with the wrong codec metadata.

When a publisher disconnects, LiveForge immediately retires that generation's HLS, LL-HLS, and DASH managers from new request lookup, then lets each manager drain frames already accepted through the captured generation boundary and finalize them once. A replacement publisher receives a distinct manager. Server or HTTP module shutdown still force-stops and joins active or draining managers, and a stopped LL-HLS manager releases blocking playlist reloads.

HTTP-FLV and fMP4 use a bounded one-second initialization wait. If the active publisher has not supplied enough codec metadata by then, LiveForge returns HTTP 503 instead of an empty successful response; retry after the sequence header arrives.

Continuous HTTP-FLV, HTTP-TS, fMP4, and matching WebSocket outputs fail closed if either their direct/transformed media input or shared muxed-output ring is overwritten. Bytes already delivered remain visible, but LiveForge discards the retained post-gap value and ends that response instead of bridging the media gap. WebSocket clients receive a retry-later continuity-loss close; a clean producer end remains a normal close.

HLS and LL-HLS instead discard their unfinished segment/part, advance to live input in the same publisher generation, refresh sequence headers and container state, and put one `#EXT-X-DISCONTINUITY` before the first recovered output. If the refreshed audio plan changes between direct and shared transformed input, the old reader is closed and released once and the replacement opens at the refreshed live cursor without GOP-history replay. Video emits nothing until the next keyframe, including when video first appears in the refreshed same-generation topology; audio-only resumes on the next live audio frame. LL-HLS abandons the affected MSN, removes its current-part URLs, and wakes blocked reloads. Retained fMP4 media keeps its matching immutable versioned init bytes; those init URLs remain available until the corresponding media leaves the playlist window, while unknown or evicted versions return 404. DASH does not continue across the gap in its existing single Period: it keeps already completed segments but retires that manager, so clients must reacquire playback. An unexpectedly closed transformed reader while the source generation remains active follows the same no-flush terminal rule rather than clean end-of-generation finalization.
