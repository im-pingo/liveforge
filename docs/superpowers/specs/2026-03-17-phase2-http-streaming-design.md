# Phase 2: HTTP Streaming Module Design Spec

## Overview

Add HTTP-FLV, HTTP-TS, and HTTP-FMP4 subscribe (pull) support to LiveForge. A single HTTP server module serves all three formats. Uses the Shared MuxerManager architecture: one muxer goroutine per format per stream, subscribers read pre-muxed bytes from SharedBuffer.

## Architecture: Shared MuxerManager

```
RTMP Publisher → AVFrame → Stream.RingBuffer
                                ↓
                    ┌───────────┼───────────┐
                    ↓           ↓           ↓
              FLV Muxer    TS Muxer    FMP4 Muxer
              Goroutine    Goroutine   Goroutine
                    ↓           ↓           ↓
              SharedBuffer SharedBuffer SharedBuffer
                    ↓           ↓           ↓
              HTTP-FLV     HTTP-TS     HTTP-FMP4
              Subscribers  Subscribers Subscribers
```

When the first subscriber for a format arrives:
1. `MuxerManager.GetOrCreateMuxer("flv")` creates `MuxerInstance` + `SharedBuffer`
2. A muxer goroutine starts reading AVFrames from `stream.RingBuffer()`
3. Muxes each frame into the target format, writes `[]byte` to SharedBuffer
4. HTTP subscriber gets `SharedBufferReader`, reads pre-muxed bytes, writes to HTTP response

When the last subscriber disconnects:
1. `MuxerManager.ReleaseMuxer("flv")` decrements count to 0
2. Muxer goroutine exits via `done` channel

## HTTP Module

### URL Routing

| URL Pattern | Format | Content-Type |
|-------------|--------|-------------|
| `GET /{app}/{key}.flv` | HTTP-FLV | `video/x-flv` |
| `GET /{app}/{key}.ts` | HTTP-TS | `video/mp2t` |
| `GET /{app}/{key}.mp4` | HTTP-FMP4 | `video/mp4` |

- 404 if stream not found in hub
- 503 if stream exists but no publisher
- CORS headers if `http_stream.cors: true`
- `Transfer-Encoding: chunked` for all formats
- `Cache-Control: no-cache`

### Module Structure

`module/httpstream/module.go`: Implements `core.Module`. Starts `net/http` server on configured listen address (default `:8080`).

`module/httpstream/handler.go`: Parses URL path, extracts app/streamKey/format, dispatches to format-specific handler.

`module/httpstream/subscriber.go`: HTTP subscriber reads from `SharedBufferReader`, writes to `http.ResponseWriter` with `Flusher.Flush()`. Exits on `request.Context().Done()`.

`module/httpstream/muxer_worker.go`: Manages muxer goroutine lifecycle. Starts goroutine on first subscriber, stops on last subscriber disconnect.

## Codec Support Matrix

| Codec | FLV | TS | FMP4 |
|-------|-----|-----|------|
| H.264 | ✅ | ✅ | ✅ |
| H.265 | ✅ (Enhanced FLV, codecID=12) | ✅ (stream_type=0x24) | ✅ (hev1+hvcC) |
| VP8 | ❌ | ❌ | ✅ (vp08+vpcC) |
| VP9 | ❌ | ❌ | ✅ (vp09+vpcC) |
| AV1 | ✅ (Enhanced FLV, codecID=13) | ✅ (private+descriptor) | ✅ (av01+av1C) |
| AAC | ✅ | ✅ (ADTS, stream_type=0x0F) | ✅ (mp4a+esds) |
| MP3 | ✅ | ✅ (stream_type=0x03) | ✅ (.mp3+esds) |
| Opus | ✅ (Enhanced FLV, formatID=13) | ✅ (private+descriptor) | ✅ (Opus+dOps) |

Unsupported codec/format combinations are silently skipped by the muxer goroutine.

## TS Muxer Design

Package: `pkg/muxer/ts/`

### Core Concepts

- **188-byte fixed packet size** with sync byte 0x47
- **PAT** (PID 0): maps program → PMT PID
- **PMT** (PID 4096): lists stream PIDs and stream_type per codec
- **PES**: wraps codec data with PTS/DTS timestamps
- **Adaptation Field**: carries PCR at regular intervals (at least every 40ms, well within the 100ms MPEG-TS spec limit)
- **Continuity Counter**: 4-bit per-PID counter for packet ordering

### Stream Type Mapping

| Codec | stream_type | Notes |
|-------|-------------|-------|
| H.264 | 0x1B | AVC video |
| H.265 | 0x24 | HEVC video |
| AV1 | 0x06 | Private data + AV1 registration descriptor |
| AAC | 0x0F | AAC with ADTS transport |
| MP3 | 0x03 | MPEG-1 Audio Layer III |
| Opus | 0x06 | Private data + Opus descriptor |

### PES Packing Per Codec

- **H.264**: Convert AVCC → Annex-B (length prefix → `00 00 00 01` start code). Prepend AUD + SPS/PPS NALUs before keyframes.
- **H.265**: Convert HVCC → Annex-B. Prepend AUD + VPS/SPS/PPS before keyframes.
- **AV1**: OBU format, direct encapsulation with AV1 descriptor in PMT.
- **AAC**: Reconstruct 7-byte ADTS header from AudioSpecificConfig, prepend to each frame.
- **MP3**: Direct encapsulation (MP3 frames are self-delimiting with sync word `0xFF 0xFx`).
- **Opus**: Direct encapsulation with Opus descriptor in PMT.

### PAT/PMT Insertion

PAT + PMT are inserted before every keyframe to enable random access for late-joining subscribers.

### Files

- `muxer.go` — Main muxer: `WriteFrame(frame) → []byte` (TS packets)
- `types.go` — Constants, PIDs, stream type mapping
- `pat_pmt.go` — PAT/PMT packet generation with CRC32
- `pes.go` — PES header construction with PTS/DTS
- `adaptation.go` — Adaptation field with PCR, stuffing

## FMP4 Muxer Design

Package: `pkg/muxer/fmp4/`

### Initialization Segment (sent once per muxer goroutine start)

```
ftyp (brand: isom, compatible: isom, iso5, dash, mp41)
moov
  mvhd (timescale: 1000)
  trak (video)
    tkhd
    mdia
      mdhd (timescale: 90000 for video)
      hdlr (vide)
      minf → stbl → stsd → [codec-specific sample entry]
  trak (audio)
    tkhd
    mdia
      mdhd (timescale: sample_rate)
      hdlr (soun)
      minf → stbl → stsd → [codec-specific sample entry]
  mvex
    trex (video track)
    trex (audio track)
```

### Sample Entry Boxes Per Codec

| Codec | Sample Entry | Config Box | Config Source |
|-------|-------------|-----------|---------------|
| H.264 | `avc1` | `avcC` | AVCDecoderConfigurationRecord (sequence header) |
| H.265 | `hev1` | `hvcC` | HEVCDecoderConfigurationRecord (sequence header) |
| VP8 | `vp08` | `vpcC` | VPCodecConfigurationRecord |
| VP9 | `vp09` | `vpcC` | VPCodecConfigurationRecord |
| AV1 | `av01` | `av1C` | AV1CodecConfigurationRecord (sequence header) |
| AAC | `mp4a` | `esds` | AudioSpecificConfig from sequence header |
| MP3 | `.mp3` | `esds` | object_type=0x6B |
| Opus | `Opus` | `dOps` | OpusHead from sequence header |

### Media Segments (one per keyframe / GOP boundary)

```
moof
  mfhd (sequence_number)
  traf (video)
    tfhd (track_id, default_sample_duration, default_sample_flags)
    tfdt (baseMediaDecodeTime in track timescale)
    trun (sample_count, data_offset, per-sample: duration, size, flags, composition_time_offset)
  traf (audio)
    tfhd
    tfdt
    trun
mdat
  [video samples: H.264/H.265 in AVCC/HVCC length-prefix format, VP8/VP9/AV1 raw]
  [audio samples: AAC raw, MP3 raw, Opus raw]
```

### Key Points

- Video stays in length-prefixed NALU format (AVCC/HVCC) — no Annex-B conversion needed
- Audio stays as raw frames (no ADTS wrapper)
- Each segment starts at a keyframe for random access
- `tfdt` carries base DTS in track timescale
- `trun` has `first_sample_flags_present` to mark keyframe

### Files

- `muxer.go` — Main muxer: `Init(videoSeqHeader, audioSeqHeader) → []byte` + `WriteSegment(frames) → []byte`
- `box.go` — Generic box writing utilities (box header, full box header)
- `init_segment.go` — ftyp + moov generation per codec
- `media_segment.go` — moof + mdat generation
- `types.go` — Constants, box types

## Enhanced FLV Muxer Updates

The existing FLV muxer (`pkg/muxer/flv/muxer.go`) uses classic FLV tag format (4-bit frameType + 4-bit codecID) which only supports H.264, AAC, and MP3. For H.265, AV1, and Opus, the muxer must be extended with **Enhanced RTMP / Enhanced FLV** tag format:

### Enhanced Video Tag (ExVideoTagHeader)

For H.265 and AV1, the first byte changes to `0x80 | (frameType << 4) | packetType`, followed by a 4-byte FourCC codec identifier:

| Codec | FourCC |
|-------|--------|
| H.265 | `hvc1` |
| AV1 | `av01` |

`packetType`: 0=sequence_start, 1=coded_frames, 2=sequence_end

### Enhanced Audio Tag

For Opus, use Enhanced FLV audio format with `audioFormatID=13` and similar structure.

### Implementation

The existing `writeVideoTag()` and `writeAudioTag()` methods in `pkg/muxer/flv/muxer.go` must branch on codec type: classic format for H.264/AAC/MP3, enhanced format for H.265/AV1/Opus. The `types.go` already defines the codec IDs (`VideoCodecH265=12`, `VideoCodecAV1=13`, `AudioFormatOpus=13`).

## Codec Helper Packages

### Existing

- `pkg/codec/h264/parser.go` — AVCC ↔ Annex-B, SPS parsing
- `pkg/codec/aac/parser.go` — AudioSpecificConfig parsing, ADTS header construction

### New

- `pkg/codec/h265/parser.go` — HVCC ↔ Annex-B conversion, VPS/SPS/PPS extraction
- `pkg/codec/mp3/parser.go` — MP3 frame header parsing (sample rate, channels, bitrate)
- `pkg/codec/opus/parser.go` — OpusHead parsing (channels, pre-skip, sample rate)
- `pkg/codec/av1/parser.go` — OBU parsing, AV1CodecConfigurationRecord construction

## Core Infrastructure Changes

### Add MuxerManager to Stream

The `Stream` struct must have a `MuxerManager` field, initialized in `NewStream()`:

```go
type Stream struct {
    // ... existing fields ...
    muxerManager *MuxerManager
}

func (s *Stream) MuxerManager() *MuxerManager { return s.muxerManager }
```

### MuxerManager API Extensions

```go
// MuxerStartFunc is called when a new muxer instance is created (first subscriber).
// The function should start the muxer goroutine.
type MuxerStartFunc func(inst *MuxerInstance, stream *Stream)

type MuxerManager struct {
    mu        sync.Mutex
    muxers    map[string]*MuxerInstance
    stream    *Stream
    bufSize   int
    onStart   map[string]MuxerStartFunc // per-format start callbacks
}

// RegisterMuxerStart registers a callback for a format.
// Called by the HTTP module during Init() for "flv", "ts", "mp4".
func (mm *MuxerManager) RegisterMuxerStart(format string, fn MuxerStartFunc)

// GetOrCreateMuxer: if creating new instance (subCount 0→1),
// initializes done channel and invokes the registered start callback.
func (mm *MuxerManager) GetOrCreateMuxer(format string) (*SharedBufferReader, *MuxerInstance)

// ReleaseMuxer: when subCount reaches 0, closes inst.Done channel,
// then removes the instance from the map.
func (mm *MuxerManager) ReleaseMuxer(format string)
```

### MuxerInstance Extensions

```go
type MuxerInstance struct {
    Buffer   *SharedBuffer
    subCount int
    Done     chan struct{}     // closed when last subscriber leaves
    initOnce sync.Once        // guards initData write
    initData []byte           // FLV header or FMP4 init segment (nil for TS)
}

// SetInitData stores format-specific init data (thread-safe via sync.Once).
// Called by the muxer goroutine after generating the init segment.
func (inst *MuxerInstance) SetInitData(data []byte) {
    inst.initOnce.Do(func() { inst.initData = data })
}

// InitData returns the stored init data. May return nil if not yet set.
func (inst *MuxerInstance) InitData() []byte { return inst.initData }
```

### SharedBuffer Close Support

Add a `Close()` method to `SharedBuffer` that signals all readers to unblock:

```go
func (sb *SharedBuffer) Close() {
    sb.rb.Close() // RingBuffer.Close() sets a closed flag and wakes all blocked readers
}
```

`RingBuffer[T].Close()` must be added to `pkg/util/ringbuffer.go`: sets an atomic `closed` flag and broadcasts to all waiting readers. `Read()` returns `(zero, false)` when closed and no data remains.

When `ReleaseMuxer()` closes `inst.Done`, the muxer goroutine exits and calls `inst.Buffer.Close()`, unblocking all subscriber reads.

## Muxer Goroutine Ownership

Muxer goroutines are owned by **core** (started via registered callbacks), not by any specific module. This enables future reuse by WebSocket-FLV/TS/FMP4 without duplication. The HTTP module registers callbacks during `Init()`, but any module can register callbacks for the same formats — only the first subscriber triggers the goroutine.

## Muxer Goroutine Data Flow

```go
func flvMuxerWorker(inst *MuxerInstance, stream *Stream) {
    defer inst.Buffer.Close()

    muxer := flv.NewMuxer()
    var buf bytes.Buffer

    // 1. Generate FLV header + store as init data
    muxer.WriteHeader(&buf, true, true)
    inst.SetInitData(buf.Bytes())
    buf.Reset()

    // 2. Mux sequence headers into FLV tags, write to SharedBuffer
    // 3. Mux GOP cache frames, track lastDTS
    // 4. Read live frames from RingBuffer, skip DTS <= lastDTS
    for {
        select {
        case <-inst.Done:
            return
        default:
        }
        frame := readNextFrame(stream)
        muxer.WriteFrame(&buf, frame)
        inst.Buffer.Write(buf.Bytes())
        buf.Reset()
    }
}

func fmp4MuxerWorker(inst *MuxerInstance, stream *Stream) {
    defer inst.Buffer.Close()

    muxer := fmp4.NewMuxer()

    // 1. Wait for sequence headers, generate init segment
    initSeg := muxer.Init(stream.VideoSeqHeader(), stream.AudioSeqHeader())
    inst.SetInitData(initSeg)

    // 2. Buffer frames until next keyframe (GOP accumulation)
    var gopBuf []*avframe.AVFrame

    // 3. On keyframe arrival: flush previous GOP as moof+mdat segment
    // For audio-only streams: flush every 1 second of accumulated frames
    for {
        select {
        case <-inst.Done:
            return
        default:
        }
        frame := readNextFrame(stream)
        if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && len(gopBuf) > 0 {
            segment := muxer.WriteSegment(gopBuf)
            inst.Buffer.Write(segment)
            gopBuf = gopBuf[:0]
        }
        gopBuf = append(gopBuf, frame)
    }
}

func tsMuxerWorker(inst *MuxerInstance, stream *Stream) {
    defer inst.Buffer.Close()

    muxer := ts.NewMuxer(stream.VideoSeqHeader(), stream.AudioSeqHeader())
    // TS has no init segment — initData stays nil

    // 1. Mux sequence headers (PAT/PMT + PES)
    // 2. Mux GOP cache, track lastDTS
    // 3. Read live frames, mux to TS packets, write to SharedBuffer
    // PCR is inserted every 40ms (tracked by muxer internally)
    for {
        select {
        case <-inst.Done:
            return
        default:
        }
        frame := readNextFrame(stream)
        packets := muxer.WriteFrame(frame) // returns []byte of 188*N bytes
        inst.Buffer.Write(packets)
    }
}
```

### FMP4 Segmentation Strategy

- **Video+Audio streams**: Segment boundary at each video keyframe. Accumulate frames in a GOP buffer. When a new keyframe arrives, flush the buffered GOP as a `moof+mdat` segment.
- **Audio-only streams**: Segment every ~1 second of audio frames (based on accumulated DTS duration).

## HTTP Subscriber Data Flow

```go
func (s *HTTPSubscriber) serve(w http.ResponseWriter, r *http.Request) {
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming not supported", http.StatusInternalServerError)
        return
    }

    // 1. Write init data if available (FLV header, FMP4 init segment)
    //    Retry briefly if initData is nil (goroutine still starting)
    if initData := inst.InitData(); initData != nil {
        w.Write(initData)
        flusher.Flush()
    }

    // 2. Read pre-muxed bytes from SharedBufferReader in loop
    reader := inst.Buffer.NewReader() // via MuxerManager.GetOrCreateMuxer()
    for {
        select {
        case <-r.Context().Done():
            return
        default:
        }
        data, ok := reader.Read() // blocks until data or buffer closed
        if !ok {
            return // SharedBuffer closed (muxer goroutine exited)
        }
        if _, err := w.Write(data); err != nil {
            return // client disconnected
        }
        flusher.Flush()
    }
}
```

### Init Data Race Resolution

The `initData` race between muxer goroutine startup and subscriber arrival is resolved by:
1. `sync.Once` ensures thread-safe single write of `initData`
2. The subscriber checks `InitData()` after getting `SharedBufferReader` — the goroutine was already started by `GetOrCreateMuxer`'s callback
3. If `initData` is still nil (goroutine hasn't written it yet), the subscriber spins briefly (100ms timeout with 10ms ticks) before falling through. For FLV/FMP4 this is the header/init segment; for TS it's always nil.

## Configuration

```yaml
http_stream:
  enabled: true
  listen: ":8080"
  cors: true
```

No per-format enable/disable — all three formats are always available when the HTTP module is enabled.

## Testing Strategy

1. **Unit tests** for TS muxer: verify PAT/PMT generation, PES packing, packet structure for each codec
2. **Unit tests** for FMP4 muxer: verify init segment box structure, media segment structure for each codec
3. **Unit tests** for codec helpers: Annex-B conversion, ADTS construction, OpusHead parsing
4. **Integration test**: push via RTMP → pull via HTTP-FLV/TS/FMP4 with ffprobe verification
5. **End-to-end test**: ffmpeg push + ffplay/ffprobe pull for each format
