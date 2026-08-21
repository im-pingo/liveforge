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
