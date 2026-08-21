# E-RTMP Support Design

**Status:** Approved design for Phase 1 implementation

**Date:** 2026-08-21

**Reference:** [Veovera Enhanced RTMP v2](https://github.com/veovera/enhanced-rtmp/blob/main/enhanced-rtmp.pdf)

## Goal

Add production-oriented Enhanced RTMP (E-RTMP) support to LiveForge while
preserving the existing RTMP and FLV behavior for legacy clients. The server
must accept classic FLV media and E-RTMP media from publishers, and must select
classic or FourCC-based output for each RTMP subscriber from the subscriber's
advertised capabilities.

Phase 1 is deliberately single-track and focuses on codecs already represented
by `AVFrame`:

- Video: H.264, H.265, AV1, VP8, and VP9
- Audio: AAC, Opus, and MP3

The implementation forwards codec payloads and does not add a codec encoder or
decoder. The existing audio transcoding path remains available for legacy
clients that cannot receive Opus.

## Non-Goals

The following are explicitly deferred because `AVFrame` and downstream muxers
currently have no track or metadata model:

- Audio and video multitrack packets
- HDR and `VideoPacketType.Metadata` color information
- ModEx packets and nanosecond timestamp offsets
- Reconnect signaling and reconnect state management
- VVC (`vvc1`) media handling

The parser will recognize these packet classes sufficiently to reject or skip
them without treating their control bytes as codec payload. It will not claim
their capability bits in the server handshake.

## Compatibility Contract

E-RTMP keeps the RTMP handshake, chunk stream, message IDs, stream IDs, and
millisecond message timestamps unchanged. A classic H.264/AAC/MP3 publisher or
subscriber continues to use the existing wire format.

Media compatibility is negotiated, not assumed. A legacy client generally
cannot parse an enhanced FourCC media header and may not decode H.265, AV1,
VP8, VP9, or Opus. A client that does not advertise E-RTMP codec support will
therefore receive classic H.264/AAC/MP3 where possible. Unsupported video is
reported as a playback failure rather than being sent in a format the client
cannot parse. Unsupported audio may use the existing AAC transcode path.

## Wire Format

All multi-byte values in the media headers are big-endian. RTMP message
timestamps remain the DTS in milliseconds; the composition offset is applied
only to derive `AVFrame.PTS`.

### Classic video

The existing five-byte H.264 body header remains:

```text
[frameType:4 | codecId:4][AVCPacketType][signed CTS:SI24][payload]
```

The signed 24-bit composition offset is sign-extended on decode and encoded
from the low 24 bits on write. Sequence headers use `AVCPacketType=0` and
coded frames use `AVCPacketType=1`.

### Enhanced video

The single-track ExVideoTagHeader and body are:

```text
[isExVideoHeader:1 | videoFrameType:3 | videoPacketType:4][FourCC:4][body]
```

Supported FourCC mappings are:

| AVFrame codec | FourCC | Sequence start | Coded frame CTS |
| --- | --- | --- | --- |
| H.264 | `avc1` | decoder configuration record | signed SI24 |
| H.265 | `hvc1` | decoder configuration record | signed SI24 |
| AV1 | `av01` | codec configuration record | not present |
| VP8 | `vp08` | codec configuration record | not present |
| VP9 | `vp09` | codec configuration record | not present |

`SequenceStart` is packet type 0 and has no CTS field. `CodedFrames` is packet
type 1. `CodedFramesX` is packet type 3 and implies a zero CTS; the writer will
use `CodedFrames` for AVC/HEVC and omit CTS for codecs whose E-RTMP format does
not define one. `SequenceEnd` (2) has no AVFrame equivalent and is skipped.

For AV1, VP8, and VP9, Phase 1 rejects an output frame whose `PTS` differs from
`DTS`, because their single-track E-RTMP coded-frame layout has no composition
offset field. This avoids silently changing timing.

### Classic audio

The legacy audio header is retained for AAC and MP3:

```text
[soundFormat:4 | soundRate:2 | soundSize:1 | soundType:1]
```

AAC has the existing one-byte packet type after the header. MP3 payload begins
immediately after the one-byte audio header.

### Enhanced audio

E-RTMP uses `SoundFormat.ExHeader=9`. The low nibble is the audio packet type,
not sound rate/size/type:

```text
[0x9 | audioPacketType:4][FourCC:4][payload]
```

Supported mappings are:

| AVFrame codec | FourCC |
| --- | --- |
| AAC | `mp4a` |
| Opus | `Opus` |
| MP3 | `.mp3` |

`SequenceStart=0`, `CodedFrames=1`, and `SequenceEnd=2` are supported. There
is no extra packet-type byte after the FourCC. In particular, an Opus coded
frame starts with `0x91` followed immediately by `Opus` and the codec payload.

## FLV Package Changes

`pkg/muxer/flv` becomes the single implementation of classic and enhanced
media payload parsing and writing. RTMP and cluster code will call its shared
parsers instead of maintaining subtly different FLV header logic.

### Encoding modes

The muxer will retain `NewMuxer()` and its current automatic behavior for
existing callers. It will add explicit per-media modes:

- `Auto`: classic for H.264/AAC/MP3 and enhanced FourCC for the other Phase 1
  codecs
- `Classic`: only legacy-compatible H.264/AAC/MP3 output is allowed
- `Enhanced`: use the Phase 1 FourCC mapping, including `avc1`, `mp4a`, and
  `.mp3`

An explicit mode that cannot represent a codec returns an error. The RTMP
subscriber uses the explicit modes; HTTP-FLV and existing relay callers keep
the automatic mode unless they opt into negotiation.

### Shared parsers

Exported FLV payload parsers will accept either classic or enhanced headers,
validate lengths and packet types, sign-extend CTS correctly, and return a
fresh payload copy suitable for asynchronous stream storage. Unsupported
metadata, multitrack, ModEx, and unknown FourCC packets return a typed parse
error. The RTMP ingest path logs and drops an unsupported media message while
keeping the connection alive; file demuxing returns the error to its caller.

The demuxer will skip script tags and packet types with no AVFrame equivalent,
including sequence-end packets, then continue reading the next media tag.

## AMF0 Support

The AMF0 implementation will add:

- Strict arrays (`0x0A`) encoded as dense `[]any`, required by `fourCcList`
- ECMA arrays (`0x08`) decoded as `map[string]any`, accepted anywhere an object
  is expected
- Nested arrays and objects in capability messages

Encoding remains deterministic for object keys. Optional capability fields with
malformed types are ignored rather than making an otherwise valid `connect`
command fail. Required legacy connect fields keep their current behavior.

## Capability Negotiation

`module/rtmp` will define a connection-local capability model. It is not added
to `AVFrame` or `core`, because it describes a peer connection rather than a
stream.

### Client input

The handler reads these optional properties from the `connect` command object:

- `fourCcList`: strict array of FourCC strings, with `"*"` as a wildcard
- `videoFourCcInfoMap`: FourCC-to-number capability flags
- `audioFourCcInfoMap`: FourCC-to-number capability flags
- `capsEx`: numeric extended capability flags

The flags are:

```text
CanDecode  = 0x01
CanEncode  = 0x02
CanForward = 0x04
```

For playback selection, `CanDecode` or `CanForward` permits delivery. A map
wildcard follows the specification and overrides codec-specific entries. The
legacy `fourCcList` is used as a compatibility fallback when the newer maps
are absent. Missing all E-RTMP fields means a legacy peer.

### Server response

The `_result` object returned for `connect` will preserve the existing
`fmsVer`, `capabilities`, and status fields and add deterministic capability
properties:

- `fourCcList` containing `vp08`, `vp09`, `av01`, `avc1`, `hvc1`, `mp4a`,
  `Opus`, and `.mp3`
- `videoFourCcInfoMap` with `CanForward` for the five supported video codecs
- `audioFourCcInfoMap` with `CanForward` for the three supported audio codecs
- `capsEx=0`, since Phase 1 does not implement the extended flags

The server does not advertise `CanEncode`; it forwards already encoded payloads
and wraps them in the negotiated media header. It also does not advertise
wildcard support, so clients can make an explicit codec decision.

### Subscriber selection

The subscriber chooses video and audio modes independently after it has a
publisher/media snapshot:

1. If the peer explicitly supports the source codec's FourCC, use enhanced
   output for that media type.
2. Otherwise use classic output for H.264, AAC, and MP3.
3. For Opus, use the existing AAC transcode path when enhanced Opus is not
   supported.
4. For H.265, AV1, VP8, or VP9 without enhanced support, fail playback with a
   clear unsupported-codec error; no misleading classic header is emitted.

The output decision is made per subscriber and never changes the stream's
internal frames or the format sent to other subscribers.

## RTMP Data Flow

### Publisher ingest

After the RTMP handshake and `connect`, media messages continue to use message
IDs 8 and 9. The handler passes each body to the shared FLV parser. Classic and
enhanced frames are normalized to the same `AVFrame` representation, sequence
headers update `MediaInfo`, and unsupported optional E-RTMP packet classes are
dropped with a bounded diagnostic. No publisher capability declaration is
required for the server to parse a recognized enhanced body.

### Subscriber output

The subscriber keeps its current GOP/ring-buffer lifecycle and RTMP chunk
writer. Only payload construction changes: it uses the negotiated video and
audio modes when converting each `AVFrame` to a tag body. RTMP message
timestamps and stream IDs remain unchanged. Legacy subscribers therefore see
the same message framing and classic media headers as before.

### Relay and test clients

Cluster RTMP transport and testkit helpers will use the shared FLV parsers and
advertise the same explicit FourCC capabilities when they act as E-RTMP peers.
This keeps server-to-server ingest and local protocol tests on the same wire
implementation. Relay behavior remains single-track and does not claim
multitrack or metadata support.

## Error Handling

- Truncated headers, invalid FourCC lengths, invalid packet types, and invalid
  signed CTS fields produce parse errors without panics.
- Unknown enhanced packet classes are isolated to the containing media message;
  the RTMP connection remains usable for later messages.
- A requested classic mode for a non-classic codec returns an explicit error.
- A subscriber that cannot receive the publisher's video codec logs the reason,
  closes its playback loop, and reports `NetStream.Play.Failed` where the
  connection lifecycle permits it.
- Capability parsing is tolerant: absent or malformed optional fields result in
  legacy behavior, not a rejected connection.

## Testing Strategy

Tests will be added before production changes and run in red-green cycles.

### Wire-level golden tests

Cover exact bytes for:

- Enhanced video sequence start and coded frames for `avc1`, `hvc1`, `av01`,
  `vp08`, and `vp09`
- Signed positive, zero, and negative CTS for AVC/HEVC
- Enhanced audio sequence start and coded frames for `mp4a`, `Opus`, and `.mp3`
- The corrected Opus layout with no trailing packet-type byte
- Classic H.264, AAC, and MP3 headers

### Parser and round-trip tests

Cover classic and enhanced payload parsing, all Phase 1 codec mappings,
sequence headers, key/inter frames, malformed lengths, unknown FourCCs, skipped
sequence ends, and mux-demux round trips that preserve DTS, PTS, frame type,
codec, and payload.

### Capability tests

Cover AMF0 strict-array and ECMA-array round trips, parsing both old and new
capability forms, wildcard precedence, server response fields, and the legacy
peer default.

### RTMP regression tests

Extend handler tests to ingest an enhanced publisher message and verify the
normalized `AVFrame`. Add subscriber payload tests for enhanced-capable and
legacy peers, including classic fallback and unsupported-video failure. Run
existing RTMP, cluster, and FLV tests unchanged to protect legacy behavior.

## Acceptance Criteria

The Phase 1 change is ready when:

1. A publisher can send classic or recognized E-RTMP single-track media and
   LiveForge stores equivalent `AVFrame` values.
2. An enhanced-capable subscriber receives exact FourCC headers for supported
   codecs.
3. A legacy subscriber receives classic H.264/AAC/MP3 output where possible
   and never receives an enhanced header by default.
4. Capability fields are exchanged through AMF0 without breaking existing
   clients that omit them.
5. Targeted tests, the full Go test suite, and the configured lint/build checks
   pass.
6. The implementation and issue response clearly document that E-RTMP is
   transport-compatible with RTMP but not automatically media-compatible with
   every legacy decoder.
