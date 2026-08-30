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

Record format validation accepts only `flv`, `fmp4`, `mp4`, `ts`, or `hls`; `hls` is a TS storage alias and uses a `.ts` extension. `record.segment.max_size` accepts an empty/whitespace value or `0` to disable size rotation, or a non-negative decimal byte count with an optional `B`, `KB`, `MB`, or `GB` suffix. Fractional values, negatives, unknown suffixes, and values that overflow the byte counter are rejected consistently by runtime validation and the configuration schema.

The default recording format is fMP4 and the default extension is `.mp4`. fMP4
and MP4 are the preferred unified browser playback formats; media tracks are
initialized lazily so a late audio track is not silently omitted. fMP4 writes AAC
directly. Its init metadata derives omitted AAC sample rate and channel count
from the AudioSpecificConfig and reuses the resolved sample rate as the media
timescale, preserving source DTS intervals. File rotation retains the publisher's
declared tracks and deep-copied latest video/audio sequence headers; each new FLV,
fMP4, MP4, or TS file therefore writes its own complete container initialization
instead of depending on an earlier file. Each file also rebases its audio and video
decode timelines independently to zero. TS writes PAT/PMT before the first media
PES even when audio arrives first, and classic MP4 computes sample durations with
separate video and audio clocks. Classic MP4 saturates sample composition,
duration, and version-0 movie-duration fields at their representable limits
instead of wrapping. It emits `ctts` version 1 when any PTS-DTS composition
offset is negative and keeps version 0 for non-negative offsets, so B-frame
timing is not decoded as a huge unsigned delay. It also writes expandable AAC
ESDS descriptor lengths. Keep
rotation enabled for files that could approach the version-0 duration limit.
When the record module is not enabled,
`GET /api/v1/recordings/status` still returns HTTP 200 with
`enabled=false`, `available=true`, and `state=disabled`, allowing Storage to render
an explicit unavailable state. Recording item, download, and play routes return
503 when the module itself is absent.

For fMP4 recordings, non-AAC source audio such as G.711, Opus, and MP3 is
converted to AAC through the generation-bound shared `audiocodec`/FFmpeg path
when the tagged build is available. Without that optional dependency, the
unsupported audio track is omitted and the output remains playable video-only.
DVR MPEG-TS applies the same conversion to audio unsupported by its target. When
a transformed fMP4 recording is stopped, its source-cursor boundary is captured
and generated output already owed for frames before that boundary is drained
before the file is finalized; this prevents an immediate stop from producing a
zero-media recording while the asynchronous AAC transform is catching up. At a
publisher-generation boundary, a fixed-size transform flushes samples retained
by its resampling filter, encodes complete frames, pads its final partial PCM
frame with silence, and emits every delayed encoder packet exactly once with
monotonic target-frame-size DTS before its output ring closes. Record
and DVR generation-tail drains therefore retain all transformed audio owed by
that finite source generation. Last-consumer cancellation can still discard a
tail that no remaining consumer owns.
AAC remains direct in fMP4, and SIP/GB28181 G.711 recordings retain the existing
G.711-to-AAC behavior without claiming audio transcoding in a portable no-CGO
build.

The same publisher lifecycle applies to SIP Gateway and GB28181 inbound media:
the protocol must successfully establish its publisher before Record/DVR start,
and the matching publisher stop event finalizes the active session. A SIP
INVITE is rejected before RTP allocation when synchronous publish authorization
fails.

DVR validates one publisher-generation startup snapshot and carries that exact
snapshot through retained-index and storage recovery into session construction.
It checks the same stream generation immediately before installation. If a
replacement publisher arrives during setup, the stale candidate is discarded,
resources opened by that candidate are closed, and the newer session is not
replaced.

DVR shutdown captures one absolute drain deadline before waiting for active
publish setup to release module ownership. If setup or finalization exceeds the
configured `server.drain_timeout`, `Close` returns a timeout at that original
deadline while the already-started cleanup continues in the background; a
delayed lock acquisition does not start a second full drain window.

A publish session that ends before any media frame arrives is preserved as
`state=failed` and is never offered as a completed playable recording. This
prevents sequence-header-only or zero-byte files from returning a misleading
successful playback response.

DVR video rotation waits for a valid video keyframe boundary after the duration
threshold. An audio-only session has no such boundary: it rotates when audio
DTS reaches `segment_duration` and publishes the non-empty segment immediately
while its publisher remains online. Portable `!audiocodec` builds retain this
behavior for H.264 plus G.711 by filtering unsupported audio and publishing a
demuxable video-only TS; tagged builds can normalize the audio to AAC when the
shared FFmpeg path is available.

DVR media routes preserve nested stream-key hierarchy. The application prefix
and each stream-key segment are validated before authorization; encoded `/` or
`\\`, empty segments, and `.`/`..` segments are rejected without redirecting or
looking up storage. Playlist URIs escape each key segment independently, so
reserved `?`, `#`, and `%` characters remain part of the key rather than
starting a query, fragment, or second path component.

DVR playlist and segment GETs run only synchronous `EventSubscribe` authorization hooks. They do not emit asynchronous subscribe lifecycle, notification, or cluster-origin work. Authorization denial keeps the existing 401/403 response behavior.

Finite DVR playlist and segment responses have a 10-second server write bound.
Successful, error, client-canceled, and timed-out requests each release exactly
one global connection slot. The bound does not change range handling,
`ServeContent` metadata, CORS, authorization, or media routing.

Recording inline-play and download responses use the management listener but
apply the same resource discipline: they acquire one global connection slot
before opening the recording, release it exactly once on every return path, and
set a 10-second write deadline immediately before `ServeContent`. Metadata,
list, status, and delete requests are not media responses and do not consume
this additional media slot.

## Inspect And Download

```bash
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings/status"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4"
curl -fS -H "Authorization: Bearer $VIEWER_TOKEN" -H 'Range: bytes=0-1023' \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4?action=download" -o /tmp/liveforge-recording.part
curl -fS -H "Authorization: Bearer $VIEWER_TOKEN" -H 'Range: bytes=0-1023' \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4?action=play" -o /tmp/liveforge-recording-preview.part
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/dvr/status"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/dvr/sessions/live/camera"
curl -fS http://127.0.0.1:8070/dvr/live/camera.m3u8 -o /tmp/liveforge-dvr.m3u8
```

Successful metadata/status requests return 200. A complete download or inline play returns 200, a valid range returns 206, a cache validator can return 304, and an invalid range can return 416. Inline play sets a media MIME type and `Content-Disposition: inline`, so the Console can preview MP4/fMP4 natively and FLV/TS through mpegts.js. Invalid/traversing IDs return 400, missing objects 404, active/not-ready recordings 409, storage failures 500, and absent modules 503. Authentication failures return 401; a valid token without permission returns 403; rate limiting can return 429.

The active and failed recording states both use the 409 JSON error response for
download and inline play; no media body is written before this state check.

The explicit `?action=play` and `?action=download` forms apply to the complete URL-decoded recording ID and are safe when that ID itself ends in `/play` or `/download`. The older `/{recordingPath}/play` and `/{recordingPath}/download` forms remain compatible only when no exact ID includes that final action-looking segment. A plain GET always returns an existing exact ID's metadata first. The Console uses the explicit query form.

The Storage view exposes Play for completed recordings and for DVR sessions with available segments. Recording playback is served by the authenticated management API and reuses the Console session cookie. DVR playback is an HLS URL on the separate `dvr.listen` media listener; its playlist and segment requests run the normal synchronous subscribe authorization hooks. The media listener returns non-credentialed CORS headers so a Console on another port can fetch HLS resources. A Console session cookie is not automatically shared with that listener, and the Console never stores or appends a bearer token. Configure DVR subscribe authorization accordingly when using the online browser action.

The Console obtains the DVR URL from `/api/v1/server/info`: after DVR
initialization, `endpoints.dvr` is the actual bound host and non-zero port, and
`endpoint_schemes.dvr` is `http` or `https` according to the listener. Before
initialization, the configured address is only a fallback and must not be used
as evidence that a listener is ready.

## Delete A Recording

Deletion requires `recordings:delete`, which only the admin role has. Confirm the recording ID and state before issuing the request.

```bash
curl -fsS -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$LIVEFORGE_API/api/v1/recordings/live/camera.mp4"
```

Success is 200. DELETE always treats the complete path as the recording ID, including an ID ending in `/play` or `/download`. The same 400/404/409/500/503 storage states apply. A viewer or operator receives 403.

Local TS deletion recognizes only `<owner>.ts.segment_<digits>.ts` and `<owner>.ts.m3u8`, plus their `.partial`, `.failed`, and `.orphan-<digits>-<digits>.failed` recovery variants, as owned sidecars. Arbitrary longer names such as `.ts.notes` remain independent recordings. Deletion removes owned sidecars and metadata before the primary. If cleanup returns 500, the primary remains authoritative; repair the filesystem problem and retry the same DELETE. Already removed cleanup artifacts do not make the retry fail.

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
