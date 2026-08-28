# Recording And DVR Management

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

## Prerequisites

- Build and start LiveForge with the `record`, `dvr`, `api`, and `metrics` modules configured.
- Keep storage paths outside the repository and grant the process account read/write/delete access.
- Export a viewer token for reads and an admin token for deletion:

```bash
export LIVEFORGE_API=http://127.0.0.1:8090
export VIEWER_TOKEN='replace-me'
export ADMIN_TOKEN='replace-me'
```

A loopback-safe configuration is:

```yaml
record:
  enabled: true
  stream_pattern: "live/*"
  format: fmp4
  path: "/var/lib/liveforge/record/{date}/{stream_key}_{time}.{ext}"
  segment:
    mode: duration
    duration: 30m
    max_size: 512MB
  on_file_complete:
    url: ""
dvr:
  enabled: true
  listen: "127.0.0.1:8070"
  stream_pattern: "live/*"
  path: "/var/lib/liveforge/dvr/{stream_key}"
  window: 2h
  segment_duration: 6s
  cleanup_interval: 30s
api:
  auth:
    tokens:
      - {name: recording-reader, token: "${VIEWER_TOKEN}", role: viewer}
      - {name: recording-admin, token: "${ADMIN_TOKEN}", role: admin}
```

`record.enabled`, `record.path`, `dvr.enabled`, `dvr.listen`, and `dvr.path` require a restart. Recording format, stream pattern, segmentation, DVR window, segment duration, and cleanup interval are hot-reload candidates. Formats are `flv`, `fmp4`, `mp4`, `ts`, and `hls`.

The default recording format is fMP4 and the default extension is `.mp4`. fMP4
and MP4 are the preferred unified browser playback formats; media tracks are
initialized lazily so a late audio track is not silently omitted. When the record
module is not enabled, `GET /api/v1/recordings/status` still returns HTTP 200 with
`enabled=false`, `available=true`, and `state=disabled`, allowing Storage to render
an explicit unavailable state. Recording item, download, and play routes return
503 when the module itself is absent.

For fMP4 recordings and DVR MPEG-TS segments, G.711 and other audio codecs that
the target container cannot describe are converted to AAC through the shared
`audiocodec`/FFmpeg path when the tagged build is available. Without that optional
dependency, the incompatible audio track is omitted and the output remains
playable video-only; AAC and MP3 tracks that the target supports are passed
through. This keeps SIP/GB28181 G.711 recordings consistent with HTTP playback
without claiming audio support in a portable no-CGO build.

The same publisher lifecycle applies to SIP Gateway and GB28181 inbound media:
the protocol must successfully establish its publisher before Record/DVR start,
and the matching publisher stop event finalizes the active session. A SIP
INVITE is rejected before RTP allocation when synchronous publish authorization
fails.

A publish session that ends before any media frame arrives is preserved as
`state=failed` and is never offered as a completed playable recording. This
prevents sequence-header-only or zero-byte files from returning a misleading
successful playback response.

DVR playlist and segment GETs run only synchronous `EventSubscribe` authorization hooks. They do not emit asynchronous subscribe lifecycle, notification, or cluster-origin work. Authorization denial keeps the existing 401/403 response behavior.

## Inspect And Download

```bash
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings/status"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4"
curl -fS -H "Authorization: Bearer $VIEWER_TOKEN" -H 'Range: bytes=0-1023' \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4/download" -o /tmp/liveforge-recording.part
curl -fS -H "Authorization: Bearer $VIEWER_TOKEN" -H 'Range: bytes=0-1023' \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4/play" -o /tmp/liveforge-recording-preview.part
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/dvr/status"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/dvr/sessions/live/camera"
curl -fS http://127.0.0.1:8070/dvr/live/camera.m3u8 -o /tmp/liveforge-dvr.m3u8
```

Successful metadata/status requests return 200. A complete download or inline play returns 200, a valid range returns 206, a cache validator can return 304, and an invalid range can return 416. Inline play sets a media MIME type and `Content-Disposition: inline`, so the Console can preview MP4/fMP4 natively and FLV/TS through mpegts.js. Invalid/traversing IDs return 400, missing objects 404, active/not-ready recordings 409, storage failures 500, and absent modules 503. Authentication failures return 401; a valid token without permission returns 403; rate limiting can return 429.

The Storage view exposes Play for completed recordings and for DVR sessions with available segments. Recording playback is served by the authenticated management API and reuses the Console session cookie. DVR playback is an HLS URL on the separate `dvr.listen` media listener; its playlist and segment requests run the normal synchronous subscribe authorization hooks. The media listener returns non-credentialed CORS headers so a Console on another port can fetch HLS resources. A Console session cookie is not automatically shared with that listener, and the Console never stores or appends a bearer token. Configure DVR subscribe authorization accordingly when using the online browser action.

## Delete A Recording

Deletion requires `recordings:delete`, which only the admin role has. Confirm the recording ID and state before issuing the request.

```bash
curl -fsS -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4"
```

Success is 200. The same 400/404/409/500/503 storage states apply. A viewer or operator receives 403.

## Metrics And Diagnostics

Query `/api/v1/recordings/status`, `/api/v1/dvr/status`, and Prometheus on the configured metrics listener. Important metrics are:

```text
liveforge_record_sessions_active
liveforge_record_files_completed_total
liveforge_record_files_failed_total
liveforge_record_write_retries_total
liveforge_record_write_failures_total
liveforge_record_files_deleted_total
liveforge_record_bytes_written_total
liveforge_record_storage_errors_total
liveforge_dvr_sessions_active
liveforge_dvr_segments_written_total
liveforge_dvr_segment_bytes_total
liveforge_dvr_write_retries_total
liveforge_dvr_write_failures_total
liveforge_dvr_cleanup_deleted_total
liveforge_dvr_cleanup_bytes_total
liveforge_dvr_cleanup_failures_total
```

Low space and backend errors are also reported in the storage health objects. Recording IDs are storage-relative paths; do not construct them from untrusted input.

## Rollback And Recovery

1. Restore the last valid hot policy document and request a runtime refresh.
2. If a path/listener/enablement change appears in `pending_restart`, restore it or restart during a controlled window.
3. On write failures, stop new publishing or narrow `stream_pattern`, preserve failed files, repair capacity/permissions, then resume.
4. Do not delete partial files as recovery. A recording in a non-ready state intentionally returns 409.
5. If DVR cleanup fails, repair filesystem permissions and free space before reducing the retention window.
