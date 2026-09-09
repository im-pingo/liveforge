# WHIP Simulcast And WHEP Layer Selection

LiveForge accepts one WHIP video m-line with one to three configured sending
RIDs and keeps each encoding in an independent stream buffer. The highest-ranked
offered RID is the canonical public stream; up to two other layers remain
private. H.264 and VP8 have real three-RID transport coverage. Simulcast requires
no audio transcoding dependency; codec conversion still requires the optional
`audiocodec` build and FFmpeg.

## Configure And Publish

Keep signaling on loopback during local validation. Production publishers still
need the normal stream authorization, TLS, and ICE/TURN configuration.

```yaml
stream:
  simulcast:
    enabled: true
    auto_pause_layer: true
    layers:
      - {rid: h, max_bitrate: 2500000}
      - {rid: m, max_bitrate: 1200000}
      - {rid: l, max_bitrate: 500000}
```

All `stream.simulcast` settings require restart. RIDs are case-sensitive, unique,
1 to 16 ASCII characters from `[A-Za-z0-9_-]`; `auto`, `high`, and `low` are
reserved selection aliases. Each configured `max_bitrate` must be positive. It
is advisory ranking metadata in bits per second, not an enforced encoder limit.
Configure the actual publisher's bitrate and resolution separately. Layers sort
by descending configured bitrate, then lexical RID to break ties.

Only one sending video m-line and at most one shared audio m-line are accepted.
Publisher startup waits for the declared video and audio codecs so recorders
observe complete media metadata. The WHIP publisher must offer matching
`a=rid:<rid> send` attributes and one
`a=simulcast:send h;m;l` list in its active video m-line, using negotiated MID/RID
RTP extensions. Every offered RID must be configured and appear exactly once
in that list; a configured subset may be offered. Disabled Simulcast, unknown
or duplicate RIDs, more than three encodings, legacy SSRC `SIM` groups, SDP
alternatives, and initially paused (`~rid`) encodings fail with 400 before
publisher/session allocation. This does not add a Console publisher encoding UI.

## Select Playback

Create WHEP sessions at the normal stream key:

```text
POST /webrtc/whep/live/camera?layer=low
POST /webrtc/whep/live/camera?layer=m
POST /webrtc/whep/live/camera?layer=high
```

Omitted `layer`, `auto`, and `high` select the highest-ranked offered encoding.
`low` selects the lowest-ranked encoding; an exact RID selects that encoding.
The choice is fixed for that session. Unknown RIDs return 400; a non-Simulcast
stream accepts only omitted `layer` or `auto`. Exhausted subscriber capacity
returns 503. `GET /webrtc/session/{sessionId}/status` reports the selected RID in
the optional top-level `layer` field.

All existing protocol outputs, recorders, and relays use the canonical parent
at its original stream key. Private layers are not separate StreamHub keys.
Publisher identity, ring buffers, GOPs, and video sequence headers are isolated
per layer. Shared audio uses separate frame envelopes for each layer. Admission
across canonical and selected-layer subscribers shares the parent's configured
subscriber ceiling. Parent retirement closes the private layers, and stale
media/session setup cannot attach to a replacement publisher generation.

## Automatic Pause

With `auto_pause_layer: true`, an unused noncanonical layer suspends server-local
video processing while RTP reads continue. The canonical layer continues for
ordinary protocol outputs. A WHEP subscriber reserves family capacity before
waking its chosen layer; resume clears partial assembly and stale cached video,
requests a fresh keyframe, and waits for valid new video headers/keyframes.
This is not upstream encoder control, a bandwidth-saving guarantee, adaptive
network selection, or a mechanism to switch an existing WHEP session's layer.

## Verify

```bash
go test -race ./config ./core ./module/webrtc -run 'TestValidateSimulcast|TestSimulcast|TestWHIP.*RID|TestWHIP.*Simulcast|TestWHEPSimulcast' -count=1
```

The H.264 three-layer test checks separate resolutions and headers, selected
WHEP RTP, canonical fMP4 roundtrip, and decoded frames when the FFmpeg executable
is present. Pause/resume and generation/policy tests cover resource and lifecycle
boundaries. These are bounded local correctness checks, not deployment capacity
or long-duration network guarantees.

VP8 uses the shared Pion descriptor parser with extended picture-ID support,
fresh frame-start gating, continuity checks and a 16 MiB/16384-fragment bound;
ordinary WHIP benefits from the same depacketizer fix.
