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
