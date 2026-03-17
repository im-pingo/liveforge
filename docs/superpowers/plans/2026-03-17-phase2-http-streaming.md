# Phase 2: HTTP Streaming Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add HTTP-FLV, HTTP-TS, and HTTP-FMP4 subscribe modules with multi-codec support using the shared MuxerManager architecture.

**Architecture:** Single HTTP server serves all three formats. Per-format muxer goroutines read AVFrames from RingBuffer, mux into target format, write to SharedBuffer. HTTP subscribers read pre-muxed bytes. StreamHub moves to Server level so all modules (RTMP, HTTP) share the same streams.

**Tech Stack:** Go stdlib `net/http`, existing `pkg/muxer/flv`, new `pkg/muxer/ts` and `pkg/muxer/fmp4`, codec helpers in `pkg/codec/`

**Spec:** `docs/superpowers/specs/2026-03-17-phase2-http-streaming-design.md`

---

## Chunk 1: Core Infrastructure Changes

### Task 1: Move StreamHub to Server (shared across modules)

Currently each module creates its own StreamHub. For HTTP to access RTMP-published streams, the hub must be shared.

**Files:**
- Modify: `core/server.go`
- Modify: `module/rtmp/server.go`
- Modify: `cmd/liveforge/main.go`
- Modify: `core/server_test.go`
- Modify: `test/integration/rtmp_relay_test.go`

- [ ] **Step 1: Write test for Server.StreamHub()**

```go
// In core/server_test.go, add:
func TestServerStreamHub(t *testing.T) {
    cfg := &config.Config{}
    cfg.Stream.RingBufferSize = 256
    cfg.Stream.NoPublisherTimeout = 5 * time.Second
    s := NewServer(cfg)
    if s.StreamHub() == nil {
        t.Fatal("expected StreamHub to be initialized")
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -run TestServerStreamHub -v`
Expected: FAIL — `StreamHub()` method doesn't exist

- [ ] **Step 2: Add StreamHub to Server**

```go
// core/server.go — add hub field, create in NewServer, add accessor
type Server struct {
    config   *config.Config
    eventBus *EventBus
    hub      *StreamHub
    modules  []Module
}

func NewServer(cfg *config.Config) *Server {
    bus := NewEventBus()
    return &Server{
        config:   cfg,
        eventBus: bus,
        hub:      NewStreamHub(cfg.Stream, bus),
    }
}

func (s *Server) StreamHub() *StreamHub {
    return s.hub
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -run TestServerStreamHub -v`
Expected: PASS

- [ ] **Step 3: Update RTMP module to use shared hub**

```go
// module/rtmp/server.go — remove hub creation, use server's hub
func (m *Module) Init(s *core.Server) error {
    m.server = s
    m.eventBus = s.GetEventBus()
    m.hub = s.StreamHub() // was: core.NewStreamHub(cfg.Stream, m.eventBus)
    // ... rest unchanged
}
```

Run: `cd /Users/pingo/streamserver && go test ./... -v`
Expected: All existing tests pass

- [ ] **Step 4: Commit**

```bash
git add core/server.go module/rtmp/server.go core/server_test.go
git commit -m "refactor: move StreamHub to Server for cross-module sharing"
```

### Task 2: Add RingBuffer.Close() for subscriber unblocking

**Files:**
- Modify: `pkg/util/ringbuffer.go`
- Modify: `pkg/util/ringbuffer_test.go`

- [ ] **Step 1: Write test for RingBuffer.Close()**

```go
// In pkg/util/ringbuffer_test.go, add:
func TestRingBufferClose(t *testing.T) {
    rb := NewRingBuffer[int](8)
    rb.Write(1)

    r := rb.NewReader()
    // Read existing data
    val, ok := r.Read()
    if !ok || val != 1 {
        t.Fatalf("expected (1, true), got (%d, %v)", val, ok)
    }

    // Close in background, reader should unblock
    done := make(chan struct{})
    go func() {
        _, ok := r.Read() // blocks waiting for data
        if ok {
            t.Error("expected Read to return false after Close")
        }
        close(done)
    }()

    time.Sleep(10 * time.Millisecond) // ensure goroutine is blocking
    rb.Close()

    select {
    case <-done:
        // success
    case <-time.After(time.Second):
        t.Fatal("Read did not unblock after Close")
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/util/ -run TestRingBufferClose -v`
Expected: FAIL — `Close()` doesn't exist

- [ ] **Step 2: Implement RingBuffer.Close()**

Add `closed atomic.Bool` field to `RingBuffer`. `Close()` sets it and sends signal. `Read()` checks closed flag after waking. `Write()` is a no-op when closed.

```go
type RingBuffer[T any] struct {
    buf         []T
    size        int64
    writeCursor atomic.Int64
    signal      chan struct{}
    closed      atomic.Bool
}

func (rb *RingBuffer[T]) Close() {
    rb.closed.Store(true)
    // Wake all blocked readers
    select {
    case rb.signal <- struct{}{}:
    default:
    }
}

func (rb *RingBuffer[T]) IsClosed() bool {
    return rb.closed.Load()
}

// Update Read() to check closed:
func (r *RingReader[T]) Read() (T, bool) {
    for {
        val, ok := r.TryRead()
        if ok {
            return val, true
        }
        if r.rb.closed.Load() {
            var zero T
            return zero, false
        }
        <-r.rb.signal
        select {
        case r.rb.signal <- struct{}{}:
        default:
        }
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/util/ -v`
Expected: All tests pass including new TestRingBufferClose

- [ ] **Step 3: Commit**

```bash
git add pkg/util/ringbuffer.go pkg/util/ringbuffer_test.go
git commit -m "feat: add RingBuffer.Close() for subscriber unblocking"
```

### Task 3: Extend SharedBuffer with Close()

**Files:**
- Modify: `core/shared_buffer.go`
- Modify: `core/shared_buffer_test.go`

- [ ] **Step 1: Write test**

```go
func TestSharedBufferClose(t *testing.T) {
    sb := NewSharedBuffer(64)
    sb.Write([]byte{1})
    r := sb.NewReader()

    // Read existing
    data, ok := r.Read()
    if !ok || data[0] != 1 {
        t.Fatalf("expected [1], got %v", data)
    }

    done := make(chan struct{})
    go func() {
        _, ok := r.Read()
        if ok {
            t.Error("expected false after Close")
        }
        close(done)
    }()

    time.Sleep(10 * time.Millisecond)
    sb.Close()

    select {
    case <-done:
    case <-time.After(time.Second):
        t.Fatal("Read did not unblock after Close")
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -run TestSharedBufferClose -v`
Expected: FAIL

- [ ] **Step 2: Implement SharedBuffer.Close()**

```go
func (sb *SharedBuffer) Close() {
    sb.rb.Close()
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add core/shared_buffer.go core/shared_buffer_test.go
git commit -m "feat: add SharedBuffer.Close() delegating to RingBuffer"
```

### Task 4: Extend MuxerManager with Done channel, initData, start callbacks

**Files:**
- Modify: `core/muxer_manager.go`
- Modify: `core/muxer_manager_test.go`

- [ ] **Step 1: Write tests for new MuxerManager features**

```go
func TestMuxerManagerStartCallback(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    stream := NewStream("live/test", cfg, bus)
    mm := NewMuxerManager(stream, 256)

    started := false
    mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
        started = true
    })

    mm.GetOrCreateMuxer("flv")
    if !started {
        t.Error("start callback was not invoked")
    }

    // Second subscriber should NOT re-trigger callback
    started = false
    mm.GetOrCreateMuxer("flv")
    if started {
        t.Error("start callback should not fire for existing muxer")
    }
}

func TestMuxerInstanceDoneChannel(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    stream := NewStream("live/test", cfg, bus)
    mm := NewMuxerManager(stream, 256)

    var capturedInst *MuxerInstance
    mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
        capturedInst = inst
    })

    mm.GetOrCreateMuxer("flv")

    select {
    case <-capturedInst.Done:
        t.Fatal("Done should not be closed yet")
    default:
    }

    mm.ReleaseMuxer("flv")

    select {
    case <-capturedInst.Done:
        // success
    default:
        t.Fatal("Done should be closed after last release")
    }
}

func TestMuxerInstanceInitData(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    stream := NewStream("live/test", cfg, bus)
    mm := NewMuxerManager(stream, 256)

    mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
        inst.SetInitData([]byte("FLV-HEADER"))
    })

    _, inst := mm.GetOrCreateMuxer("flv")
    if string(inst.InitData()) != "FLV-HEADER" {
        t.Errorf("expected FLV-HEADER, got %s", inst.InitData())
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -run TestMuxerManager -v`
Expected: FAIL

- [ ] **Step 2: Implement MuxerManager extensions**

```go
// MuxerStartFunc is invoked when a muxer instance is first created.
type MuxerStartFunc func(inst *MuxerInstance, stream *Stream)

type MuxerInstance struct {
    Buffer   *SharedBuffer
    subCount int
    Done     chan struct{} // closed when last subscriber leaves
    initOnce sync.Once
    initData []byte
}

func (inst *MuxerInstance) SetInitData(data []byte) {
    inst.initOnce.Do(func() { inst.initData = data })
}

func (inst *MuxerInstance) InitData() []byte { return inst.initData }

type MuxerManager struct {
    mu      sync.Mutex
    muxers  map[string]*MuxerInstance
    stream  *Stream
    bufSize int
    onStart map[string]MuxerStartFunc
}

func NewMuxerManager(stream *Stream, bufSize int) *MuxerManager {
    return &MuxerManager{
        muxers:  make(map[string]*MuxerInstance),
        stream:  stream,
        bufSize: bufSize,
        onStart: make(map[string]MuxerStartFunc),
    }
}

func (mm *MuxerManager) RegisterMuxerStart(format string, fn MuxerStartFunc) {
    mm.mu.Lock()
    defer mm.mu.Unlock()
    mm.onStart[format] = fn
}

func (mm *MuxerManager) GetOrCreateMuxer(format string) (*SharedBufferReader, *MuxerInstance) {
    mm.mu.Lock()
    defer mm.mu.Unlock()

    inst, ok := mm.muxers[format]
    isNew := !ok
    if isNew {
        inst = &MuxerInstance{
            Buffer: NewSharedBuffer(mm.bufSize),
            Done:   make(chan struct{}),
        }
        mm.muxers[format] = inst
    }
    inst.subCount++

    if isNew {
        if fn, ok := mm.onStart[format]; ok {
            fn(inst, mm.stream)
        }
    }

    return inst.Buffer.NewReader(), inst
}

func (mm *MuxerManager) ReleaseMuxer(format string) {
    mm.mu.Lock()
    defer mm.mu.Unlock()

    inst, ok := mm.muxers[format]
    if !ok {
        return
    }
    inst.subCount--
    if inst.subCount <= 0 {
        close(inst.Done)
        delete(mm.muxers, format)
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -v`
Expected: All tests pass

- [ ] **Step 3: Commit**

```bash
git add core/muxer_manager.go core/muxer_manager_test.go
git commit -m "feat: extend MuxerManager with Done channel, initData, start callbacks"
```

### Task 5: Add MuxerManager to Stream

**Files:**
- Modify: `core/stream.go`
- Modify: `core/stream_test.go`

- [ ] **Step 1: Write test**

```go
func TestStreamMuxerManager(t *testing.T) {
    bus := NewEventBus()
    cfg := newTestStreamConfig()
    stream := NewStream("live/test", cfg, bus)
    if stream.MuxerManager() == nil {
        t.Fatal("expected MuxerManager to be initialized")
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -run TestStreamMuxerManager -v`
Expected: FAIL

- [ ] **Step 2: Add MuxerManager field to Stream**

Add `muxerManager *MuxerManager` to `Stream` struct, initialize in `NewStream()`, add `MuxerManager()` accessor.

```go
func NewStream(key string, cfg config.StreamConfig, bus *EventBus) *Stream {
    return &Stream{
        key:          key,
        config:       cfg,
        state:        StreamStateIdle,
        ringBuffer:   util.NewRingBuffer[*avframe.AVFrame](cfg.RingBufferSize),
        eventBus:     bus,
        muxerManager: NewMuxerManager(nil, cfg.RingBufferSize), // stream set after creation
    }
}
// Note: pass `s` (self) after construction or use lazy init pattern:
func (s *Stream) MuxerManager() *MuxerManager {
    return s.muxerManager
}
```

Actually, since MuxerManager needs a reference to Stream, use a two-phase approach: create MuxerManager after Stream, set stream reference.

```go
func NewStream(key string, cfg config.StreamConfig, bus *EventBus) *Stream {
    s := &Stream{
        key:        key,
        config:     cfg,
        state:      StreamStateIdle,
        ringBuffer: util.NewRingBuffer[*avframe.AVFrame](cfg.RingBufferSize),
        eventBus:   bus,
    }
    s.muxerManager = NewMuxerManager(s, cfg.RingBufferSize)
    return s
}
```

Run: `cd /Users/pingo/streamserver && go test ./core/ -v`
Expected: All pass

- [ ] **Step 3: Commit**

```bash
git add core/stream.go core/stream_test.go
git commit -m "feat: add MuxerManager to Stream"
```

## Chunk 2: Codec Helpers

### Task 6: H.264 AVCC → Annex-B conversion

The existing h264 parser has `ExtractNALUs` (Annex-B → NALUs) but not AVCC → Annex-B.

**Files:**
- Modify: `pkg/codec/h264/parser.go`
- Modify: `pkg/codec/h264/parser_test.go`

- [ ] **Step 1: Write test**

```go
func TestAVCCToAnnexB(t *testing.T) {
    // Build AVCC data: 4-byte length prefix + NALU
    nalu := []byte{0x65, 0x88, 0x00, 0x01} // IDR slice
    avcc := make([]byte, 4+len(nalu))
    binary.BigEndian.PutUint32(avcc, uint32(len(nalu)))
    copy(avcc[4:], nalu)

    annexB := AVCCToAnnexB(avcc)
    expected := append([]byte{0x00, 0x00, 0x00, 0x01}, nalu...)
    if !bytes.Equal(annexB, expected) {
        t.Errorf("expected %x, got %x", expected, annexB)
    }
}

func TestAVCCToAnnexBMultiNALU(t *testing.T) {
    // Two NALUs in AVCC format
    nalu1 := []byte{0x67, 0x64, 0x00, 0x28} // SPS
    nalu2 := []byte{0x68, 0xEE, 0x38, 0x80} // PPS
    var avcc []byte
    buf := make([]byte, 4)
    binary.BigEndian.PutUint32(buf, uint32(len(nalu1)))
    avcc = append(avcc, buf...)
    avcc = append(avcc, nalu1...)
    binary.BigEndian.PutUint32(buf, uint32(len(nalu2)))
    avcc = append(avcc, buf...)
    avcc = append(avcc, nalu2...)

    annexB := AVCCToAnnexB(avcc)
    // Should have two start codes
    nalus := ExtractNALUs(annexB)
    if len(nalus) != 2 {
        t.Fatalf("expected 2 NALUs, got %d", len(nalus))
    }
}

func TestExtractSPSPPSFromAVCRecord(t *testing.T) {
    // Minimal AVCDecoderConfigurationRecord
    sps := []byte{0x67, 0x64, 0x00, 0x28, 0xAC, 0xD9, 0x40}
    pps := []byte{0x68, 0xEE, 0x38, 0x80}
    record := buildAVCRecord(sps, pps)

    gotSPS, gotPPS, err := ExtractSPSPPSFromAVCRecord(record)
    if err != nil {
        t.Fatal(err)
    }
    if !bytes.Equal(gotSPS, sps) {
        t.Errorf("SPS mismatch: %x vs %x", gotSPS, sps)
    }
    if !bytes.Equal(gotPPS, pps) {
        t.Errorf("PPS mismatch: %x vs %x", gotPPS, pps)
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/h264/ -run TestAVCC -v`
Expected: FAIL

- [ ] **Step 2: Implement AVCC→Annex-B and AVCRecord parsing**

```go
// AVCCToAnnexB converts AVCC format (length-prefixed NALUs) to Annex-B (start code prefixed).
func AVCCToAnnexB(data []byte) []byte {
    var result []byte
    startCode := []byte{0x00, 0x00, 0x00, 0x01}
    offset := 0
    for offset+4 <= len(data) {
        naluLen := int(binary.BigEndian.Uint32(data[offset:]))
        offset += 4
        if offset+naluLen > len(data) {
            break
        }
        result = append(result, startCode...)
        result = append(result, data[offset:offset+naluLen]...)
        offset += naluLen
    }
    return result
}

// ExtractSPSPPSFromAVCRecord extracts SPS and PPS NALUs from AVCDecoderConfigurationRecord.
func ExtractSPSPPSFromAVCRecord(data []byte) (sps, pps []byte, err error) {
    if len(data) < 7 {
        return nil, nil, errors.New("AVCRecord too short")
    }
    // data[5]: numOfSequenceParameterSets & 0x1F
    numSPS := int(data[5] & 0x1F)
    offset := 6
    for i := 0; i < numSPS; i++ {
        if offset+2 > len(data) {
            return nil, nil, errors.New("AVCRecord: truncated SPS length")
        }
        spsLen := int(binary.BigEndian.Uint16(data[offset:]))
        offset += 2
        if offset+spsLen > len(data) {
            return nil, nil, errors.New("AVCRecord: truncated SPS data")
        }
        if i == 0 {
            sps = data[offset : offset+spsLen]
        }
        offset += spsLen
    }
    // numPPS
    if offset >= len(data) {
        return nil, nil, errors.New("AVCRecord: missing PPS count")
    }
    numPPS := int(data[offset])
    offset++
    for i := 0; i < numPPS; i++ {
        if offset+2 > len(data) {
            return nil, nil, errors.New("AVCRecord: truncated PPS length")
        }
        ppsLen := int(binary.BigEndian.Uint16(data[offset:]))
        offset += 2
        if offset+ppsLen > len(data) {
            return nil, nil, errors.New("AVCRecord: truncated PPS data")
        }
        if i == 0 {
            pps = data[offset : offset+ppsLen]
        }
        offset += ppsLen
    }
    return sps, pps, nil
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/h264/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/codec/h264/parser.go pkg/codec/h264/parser_test.go
git commit -m "feat: add AVCC-to-AnnexB conversion and AVCRecord SPS/PPS extraction"
```

### Task 7: H.265 codec helper

**Files:**
- Create: `pkg/codec/h265/parser.go`
- Create: `pkg/codec/h265/parser_test.go`

- [ ] **Step 1: Write test**

```go
package h265

import (
    "bytes"
    "encoding/binary"
    "testing"
)

func TestHVCCToAnnexB(t *testing.T) {
    nalu := []byte{0x40, 0x01, 0x0C, 0x01} // VPS
    hvcc := make([]byte, 4+len(nalu))
    binary.BigEndian.PutUint32(hvcc, uint32(len(nalu)))
    copy(hvcc[4:], nalu)

    annexB := HVCCToAnnexB(hvcc)
    expected := append([]byte{0x00, 0x00, 0x00, 0x01}, nalu...)
    if !bytes.Equal(annexB, expected) {
        t.Errorf("expected %x, got %x", expected, annexB)
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/h265/ -v`
Expected: FAIL

- [ ] **Step 2: Implement H.265 helpers**

```go
package h265

import "encoding/binary"

// HEVC NAL unit types
const (
    NALTypeVPS = 32
    NALTypeSPS = 33
    NALTypePPS = 34
    NALTypeIDR = 19
)

// HVCCToAnnexB converts HVCC format (length-prefixed) to Annex-B (start code prefixed).
// Same format as AVCC: 4-byte big-endian length + NALU.
func HVCCToAnnexB(data []byte) []byte {
    var result []byte
    startCode := []byte{0x00, 0x00, 0x00, 0x01}
    offset := 0
    for offset+4 <= len(data) {
        naluLen := int(binary.BigEndian.Uint32(data[offset:]))
        offset += 4
        if offset+naluLen > len(data) {
            break
        }
        result = append(result, startCode...)
        result = append(result, data[offset:offset+naluLen]...)
        offset += naluLen
    }
    return result
}

// ExtractVPSSPSPPSFromHVCRecord extracts VPS, SPS, PPS from HEVCDecoderConfigurationRecord.
func ExtractVPSSPSPPSFromHVCRecord(data []byte) (vps, sps, pps []byte, err error) {
    // HEVCDecoderConfigurationRecord: skip first 22 bytes of config
    // then numOfArrays, each array has: array_completeness(1bit)+reserved(1bit)+NAL_unit_type(6bits),
    // numNalus(2bytes), then for each: naluLength(2bytes)+naluData
    if len(data) < 23 {
        return nil, nil, nil, fmt.Errorf("HVCRecord too short")
    }
    numArrays := int(data[22])
    offset := 23
    for i := 0; i < numArrays; i++ {
        if offset >= len(data) { break }
        nalType := data[offset] & 0x3F
        offset++
        if offset+2 > len(data) { break }
        numNalus := int(binary.BigEndian.Uint16(data[offset:]))
        offset += 2
        for j := 0; j < numNalus; j++ {
            if offset+2 > len(data) { break }
            naluLen := int(binary.BigEndian.Uint16(data[offset:]))
            offset += 2
            if offset+naluLen > len(data) { break }
            naluData := data[offset : offset+naluLen]
            switch nalType {
            case NALTypeVPS: if vps == nil { vps = naluData }
            case NALTypeSPS: if sps == nil { sps = naluData }
            case NALTypePPS: if pps == nil { pps = naluData }
            }
            offset += naluLen
        }
    }
    return
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/h265/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/codec/h265/
git commit -m "feat: add H.265 codec helper (HVCC-to-AnnexB, VPS/SPS/PPS extraction)"
```

### Task 8: AAC ADTS header builder

The existing AAC parser can parse AudioSpecificConfig but cannot build ADTS headers. Add this for TS muxer.

**Files:**
- Modify: `pkg/codec/aac/parser.go`
- Modify: `pkg/codec/aac/parser_test.go`

- [ ] **Step 1: Write test**

```go
func TestBuildADTSHeader(t *testing.T) {
    // AAC-LC 44100Hz stereo = AudioSpecificConfig 0x12 0x10
    info := &AACInfo{ObjectType: 2, SampleRate: 44100, Channels: 2}
    frameLen := 100
    header := BuildADTSHeader(info, frameLen)
    if len(header) != 7 {
        t.Fatalf("expected 7 bytes, got %d", len(header))
    }
    // Verify sync word
    if header[0] != 0xFF || (header[1]&0xF0) != 0xF0 {
        t.Error("invalid ADTS sync word")
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/aac/ -run TestBuildADTS -v`
Expected: FAIL

- [ ] **Step 2: Implement ADTS header builder**

```go
// SampleRateIndex returns the MPEG-4 frequency index for a given sample rate.
func SampleRateIndex(rate int) int {
    for i, r := range sampleRates {
        if r == rate {
            return i
        }
    }
    return 0x0F // explicit frequency
}

// BuildADTSHeader builds a 7-byte ADTS header for an AAC frame.
func BuildADTSHeader(info *AACInfo, frameLength int) []byte {
    header := make([]byte, 7)
    totalLen := 7 + frameLength
    freqIdx := SampleRateIndex(info.SampleRate)
    profile := info.ObjectType - 1 // ADTS profile = objectType - 1

    header[0] = 0xFF
    header[1] = 0xF1 // sync + MPEG-4 + Layer 0 + no CRC
    header[2] = byte((profile<<6)&0xC0) | byte((freqIdx<<2)&0x3C) | byte((info.Channels>>2)&0x01)
    header[3] = byte((info.Channels<<6)&0xC0) | byte((totalLen>>11)&0x03)
    header[4] = byte((totalLen >> 3) & 0xFF)
    header[5] = byte((totalLen<<5)&0xE0) | 0x1F
    header[6] = 0xFC
    return header
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/aac/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/codec/aac/parser.go pkg/codec/aac/parser_test.go
git commit -m "feat: add AAC ADTS header builder for TS muxer"
```

### Task 9: MP3 frame header parser

**Files:**
- Create: `pkg/codec/mp3/parser.go`
- Create: `pkg/codec/mp3/parser_test.go`

- [ ] **Step 1: Write test**

```go
package mp3

import "testing"

func TestParseMP3FrameHeader(t *testing.T) {
    // 128kbps, 44100Hz, stereo MP3 frame header: 0xFF 0xFB 0x90 0x00
    header := []byte{0xFF, 0xFB, 0x90, 0x00}
    info, err := ParseFrameHeader(header)
    if err != nil {
        t.Fatal(err)
    }
    if info.SampleRate != 44100 {
        t.Errorf("expected 44100, got %d", info.SampleRate)
    }
    if info.Channels != 2 {
        t.Errorf("expected 2 channels, got %d", info.Channels)
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/mp3/ -v`
Expected: FAIL

- [ ] **Step 2: Implement**

```go
package mp3

import "fmt"

type MP3Info struct {
    SampleRate int
    Channels   int
    Bitrate    int
    Version    int // 1=MPEG1, 2=MPEG2, 3=MPEG2.5
    Layer      int // 1,2,3
}

var mp3SampleRates = [3][3]int{
    {44100, 48000, 32000}, // MPEG1
    {22050, 24000, 16000}, // MPEG2
    {11025, 12000, 8000},  // MPEG2.5
}

var mp3Bitrates = [2][3][15]int{
    // MPEG1: Layer1, Layer2, Layer3
    {
        {0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
        {0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
        {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
    },
    // MPEG2/2.5: Layer1, Layer2, Layer3
    {
        {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
        {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
        {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
    },
}

func ParseFrameHeader(data []byte) (*MP3Info, error) {
    if len(data) < 4 {
        return nil, fmt.Errorf("MP3 header too short")
    }
    if data[0] != 0xFF || (data[1]&0xE0) != 0xE0 {
        return nil, fmt.Errorf("invalid MP3 sync word")
    }
    versionBits := (data[1] >> 3) & 0x03
    layerBits := (data[1] >> 1) & 0x03
    bitrateIdx := (data[2] >> 4) & 0x0F
    srIdx := (data[2] >> 2) & 0x03
    channelMode := (data[3] >> 6) & 0x03

    var versionIdx int
    var version int
    switch versionBits {
    case 3: versionIdx = 0; version = 1 // MPEG1
    case 2: versionIdx = 1; version = 2 // MPEG2
    case 0: versionIdx = 2; version = 3 // MPEG2.5
    default: return nil, fmt.Errorf("reserved MPEG version")
    }

    var layer int
    switch layerBits {
    case 3: layer = 1
    case 2: layer = 2
    case 1: layer = 3
    default: return nil, fmt.Errorf("reserved layer")
    }

    if srIdx > 2 {
        return nil, fmt.Errorf("reserved sample rate index")
    }
    sampleRate := mp3SampleRates[versionIdx][srIdx]

    channels := 2
    if channelMode == 3 { channels = 1 }

    bitrateRow := 0
    if versionIdx > 0 { bitrateRow = 1 }
    bitrate := 0
    if bitrateIdx > 0 && bitrateIdx < 15 {
        bitrate = mp3Bitrates[bitrateRow][layer-1][bitrateIdx] * 1000
    }

    return &MP3Info{
        SampleRate: sampleRate,
        Channels:   channels,
        Bitrate:    bitrate,
        Version:    version,
        Layer:      layer,
    }, nil
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/mp3/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/codec/mp3/
git commit -m "feat: add MP3 frame header parser"
```

### Task 10: Opus header parser

**Files:**
- Create: `pkg/codec/opus/parser.go`
- Create: `pkg/codec/opus/parser_test.go`

- [ ] **Step 1: Write test**

```go
package opus

import (
    "encoding/binary"
    "testing"
)

func TestParseOpusHead(t *testing.T) {
    // Build OpusHead: "OpusHead" + version(1) + channels(2) + pre_skip(312) + sample_rate(48000)
    head := make([]byte, 19)
    copy(head[0:8], "OpusHead")
    head[8] = 1 // version
    head[9] = 2 // channels
    binary.LittleEndian.PutUint16(head[10:12], 312) // pre-skip
    binary.LittleEndian.PutUint32(head[12:16], 48000) // sample rate
    binary.LittleEndian.PutUint16(head[16:18], 0) // output gain
    head[18] = 0 // channel mapping family

    info, err := ParseOpusHead(head)
    if err != nil {
        t.Fatal(err)
    }
    if info.Channels != 2 {
        t.Errorf("expected 2 channels, got %d", info.Channels)
    }
    if info.SampleRate != 48000 {
        t.Errorf("expected 48000, got %d", info.SampleRate)
    }
    if info.PreSkip != 312 {
        t.Errorf("expected pre-skip 312, got %d", info.PreSkip)
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/opus/ -v`
Expected: FAIL

- [ ] **Step 2: Implement**

```go
package opus

import (
    "encoding/binary"
    "fmt"
)

type OpusInfo struct {
    Version    int
    Channels   int
    PreSkip    int
    SampleRate int
}

func ParseOpusHead(data []byte) (*OpusInfo, error) {
    if len(data) < 19 {
        return nil, fmt.Errorf("OpusHead too short: %d bytes", len(data))
    }
    if string(data[0:8]) != "OpusHead" {
        return nil, fmt.Errorf("invalid OpusHead magic")
    }
    return &OpusInfo{
        Version:    int(data[8]),
        Channels:   int(data[9]),
        PreSkip:    int(binary.LittleEndian.Uint16(data[10:12])),
        SampleRate: int(binary.LittleEndian.Uint32(data[12:16])),
    }, nil
}

// BuildDOpsBox builds the dOps box content for FMP4 from OpusHead data.
// dOps is a modified OpusHead without the "OpusHead" magic and version byte.
func BuildDOpsBox(info *OpusInfo) []byte {
    buf := make([]byte, 11)
    buf[0] = 0 // version
    buf[1] = byte(info.Channels)
    binary.BigEndian.PutUint16(buf[2:4], uint16(info.PreSkip))
    binary.BigEndian.PutUint32(buf[4:8], uint32(info.SampleRate))
    binary.BigEndian.PutUint16(buf[8:10], 0) // output gain
    buf[10] = 0 // channel mapping family
    return buf
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/opus/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/codec/opus/
git commit -m "feat: add Opus header parser and dOps builder"
```

### Task 11: AV1 OBU parser

**Files:**
- Create: `pkg/codec/av1/parser.go`
- Create: `pkg/codec/av1/parser_test.go`

- [ ] **Step 1: Write test**

```go
package av1

import "testing"

func TestParseOBUHeader(t *testing.T) {
    // OBU header: obu_type=1 (sequence_header), obu_has_size=1
    header := []byte{0x0A} // 0b00001010: forbidden=0, type=1, extension=0, has_size=1, reserved=0
    obuType, hasSize, err := ParseOBUHeader(header)
    if err != nil {
        t.Fatal(err)
    }
    if obuType != OBUSequenceHeader {
        t.Errorf("expected type %d, got %d", OBUSequenceHeader, obuType)
    }
    if !hasSize {
        t.Error("expected has_size=true")
    }
}

func TestBuildAV1CodecConfigRecord(t *testing.T) {
    // Minimal sequence header OBU
    seqHeader := []byte{0x0A, 0x05, 0x00, 0x00, 0x00, 0x24, 0xF8}
    record := BuildAV1CodecConfigRecord(seqHeader)
    if len(record) < 4 {
        t.Fatal("record too short")
    }
    if record[0]&0x80 != 0x80 { // marker bit
        t.Error("marker bit not set")
    }
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/av1/ -v`
Expected: FAIL

- [ ] **Step 2: Implement**

```go
package av1

import "fmt"

const (
    OBUSequenceHeader = 1
    OBUTemporalDelimiter = 2
    OBUFrameHeader = 3
    OBUFrame = 6
)

func ParseOBUHeader(data []byte) (obuType int, hasSize bool, err error) {
    if len(data) < 1 {
        return 0, false, fmt.Errorf("OBU header too short")
    }
    forbidden := (data[0] >> 7) & 0x01
    if forbidden != 0 {
        return 0, false, fmt.Errorf("forbidden bit set")
    }
    obuType = int((data[0] >> 3) & 0x0F)
    hasSize = (data[0]>>1)&0x01 == 1
    return obuType, hasSize, nil
}

// BuildAV1CodecConfigRecord builds an AV1CodecConfigurationRecord for FMP4.
// Input is the raw sequence header OBU bytes.
func BuildAV1CodecConfigRecord(seqHeaderOBU []byte) []byte {
    // AV1CodecConfigurationRecord:
    // marker(1) + version(7) + seq_profile(3) + seq_level_idx_0(5)
    // + seq_tier_0(1) + high_bitdepth(1) + twelve_bit(1) + monochrome(1)
    // + chroma_subsampling_x(1) + chroma_subsampling_y(1) + chroma_sample_position(2)
    // + configOBUs[]
    record := make([]byte, 4+len(seqHeaderOBU))
    record[0] = 0x81 // marker=1, version=1
    record[1] = 0x00 // profile=0, level=0 (placeholder)
    record[2] = 0x00 // tier=0, flags (placeholder)
    record[3] = 0x00 // chroma (placeholder)
    copy(record[4:], seqHeaderOBU)
    return record
}
```

Run: `cd /Users/pingo/streamserver && go test ./pkg/codec/av1/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/codec/av1/
git commit -m "feat: add AV1 OBU parser and codec config record builder"
```

## Chunk 3: TS Muxer

### Task 12: TS types and constants

**Files:**
- Create: `pkg/muxer/ts/types.go`

- [ ] **Step 1: Create types file with constants, PIDs, stream type mapping**

```go
package ts

import "github.com/im-pingo/liveforge/pkg/avframe"

const (
    PacketSize  = 188
    SyncByte    = 0x47
    PIDPat      = 0x0000
    PIDPmt      = 0x1000
    PIDVideo    = 0x0100
    PIDAudio    = 0x0101
    PIDPCR      = PIDVideo
    MaxPCRInterval = 40 // ms
)

func StreamType(codec avframe.CodecType) byte {
    switch codec {
    case avframe.CodecH264: return 0x1B
    case avframe.CodecH265: return 0x24
    case avframe.CodecAV1:  return 0x06
    case avframe.CodecAAC:  return 0x0F
    case avframe.CodecMP3:  return 0x03
    case avframe.CodecOpus: return 0x06
    default: return 0
    }
}
```

- [ ] **Step 2: Commit**

```bash
git add pkg/muxer/ts/types.go
git commit -m "feat: add TS muxer types, constants, and codec-to-stream-type mapping"
```

### Task 13: PAT/PMT generation

**Files:**
- Create: `pkg/muxer/ts/pat_pmt.go`
- Create: `pkg/muxer/ts/pat_pmt_test.go`

- [ ] **Step 1: Write tests**

Test that BuildPAT returns exactly 188 bytes starting with 0x47, PID=0.
Test that BuildPMT returns 188 bytes with correct stream_type entries for given video/audio codecs.

- [ ] **Step 2: Implement PAT/PMT builders**

PAT: TS header (4 bytes) + pointer field (1 byte) + PAT table with CRC32 + stuffing to 188 bytes.
PMT: TS header + pointer + PMT table with stream entries + CRC32 + stuffing.

CRC32 uses the MPEG-2 polynomial (0x04C11DB7).

- [ ] **Step 3: Run tests, verify pass**
- [ ] **Step 4: Commit**

```bash
git add pkg/muxer/ts/pat_pmt.go pkg/muxer/ts/pat_pmt_test.go
git commit -m "feat: add TS PAT/PMT packet generation with CRC32"
```

### Task 14: PES packetization

**Files:**
- Create: `pkg/muxer/ts/pes.go`
- Create: `pkg/muxer/ts/pes_test.go`

- [ ] **Step 1: Write tests**

Test PES header generation with PTS only (audio) and PTS+DTS (video with B-frames).
Test that PES payload is correctly split into 188-byte TS packets with continuity counters.

- [ ] **Step 2: Implement PES builder**

Build PES header (start code + stream_id + PES_packet_length + PTS/DTS fields).
Split PES into TS packets: first packet has `payload_unit_start_indicator=1`, subsequent packets have it `0`. Handle adaptation field for stuffing when payload doesn't fill a packet exactly.

- [ ] **Step 3: Run tests, verify pass**
- [ ] **Step 4: Commit**

```bash
git add pkg/muxer/ts/pes.go pkg/muxer/ts/pes_test.go
git commit -m "feat: add TS PES packetization with PTS/DTS"
```

### Task 15: Adaptation field and PCR

**Files:**
- Create: `pkg/muxer/ts/adaptation.go`
- Create: `pkg/muxer/ts/adaptation_test.go`

- [ ] **Step 1: Write tests**

Test adaptation field with PCR flag set. Verify PCR encoding (33-bit base + 6 reserved + 9-bit extension).
Test stuffing-only adaptation field.

- [ ] **Step 2: Implement**

Build adaptation field with optional PCR, stuffing bytes, random_access_indicator for keyframes.

- [ ] **Step 3: Run tests, verify pass**
- [ ] **Step 4: Commit**

```bash
git add pkg/muxer/ts/adaptation.go pkg/muxer/ts/adaptation_test.go
git commit -m "feat: add TS adaptation field with PCR"
```

### Task 16: TS Muxer main logic

**Files:**
- Create: `pkg/muxer/ts/muxer.go`
- Create: `pkg/muxer/ts/muxer_test.go`

- [ ] **Step 1: Write test**

Test full AVFrame→TS packet flow: feed a keyframe AVFrame, verify output is N*188 bytes, starts with PAT+PMT, has correct PES structure.

- [ ] **Step 2: Implement TS Muxer**

```go
type Muxer struct {
    videoCodec    avframe.CodecType
    audioCodec    avframe.CodecType
    videoSeqHeader []byte // SPS/PPS or VPS/SPS/PPS in raw form for prepending
    audioSeqHeader []byte // AudioSpecificConfig for ADTS reconstruction
    videoContinuity uint8
    audioContinuity uint8
    patContinuity   uint8
    pmtContinuity   uint8
    lastPCR         int64
    pat             []byte // cached PAT packet
    pmt             []byte // cached PMT packet
}

func NewMuxer(videoCodec, audioCodec avframe.CodecType, videoSeqHeader, audioSeqHeader []byte) *Muxer

// WriteFrame converts an AVFrame to TS packets.
// Returns []byte of N*188 bytes.
// Prepends PAT+PMT before keyframes.
// Inserts PCR every MaxPCRInterval ms.
func (m *Muxer) WriteFrame(frame *avframe.AVFrame) []byte
```

Per-codec handling:
- H.264: AVCC→Annex-B, prepend AUD+SPS/PPS on keyframe
- H.265: HVCC→Annex-B, prepend AUD+VPS/SPS/PPS on keyframe
- AV1: direct OBU encapsulation
- AAC: prepend ADTS header
- MP3: direct (self-delimiting)
- Opus: direct with descriptor

- [ ] **Step 3: Run tests, verify pass**
- [ ] **Step 4: Commit**

```bash
git add pkg/muxer/ts/muxer.go pkg/muxer/ts/muxer_test.go
git commit -m "feat: add TS muxer with multi-codec support"
```

## Chunk 4: FMP4 Muxer

### Task 17: FMP4 box utilities

**Files:**
- Create: `pkg/muxer/fmp4/box.go`
- Create: `pkg/muxer/fmp4/types.go`
- Create: `pkg/muxer/fmp4/box_test.go`

- [ ] **Step 1: Write tests for box writing utilities**
- [ ] **Step 2: Implement box writing helpers**

```go
// WriteBox writes a standard box: [size(4)][type(4)][payload]
func WriteBox(w io.Writer, boxType [4]byte, payload []byte) error

// WriteFullBox writes a full box: [size(4)][type(4)][version(1)][flags(3)][payload]
func WriteFullBox(w io.Writer, boxType [4]byte, version byte, flags uint32, payload []byte) error
```

- [ ] **Step 3: Commit**

```bash
git add pkg/muxer/fmp4/
git commit -m "feat: add FMP4 box writing utilities"
```

### Task 18: FMP4 init segment generation

**Files:**
- Create: `pkg/muxer/fmp4/init_segment.go`
- Create: `pkg/muxer/fmp4/init_segment_test.go`

- [ ] **Step 1: Write tests**

Test that BuildInitSegment produces valid ftyp+moov with correct sample entries per codec (avc1, hev1, av01, vp08, vp09, mp4a, .mp3, Opus).

- [ ] **Step 2: Implement**

Build ftyp box, moov box with mvhd, trak(video) with codec-specific stsd entry, trak(audio), mvex with trex boxes.

- [ ] **Step 3: Commit**

```bash
git add pkg/muxer/fmp4/init_segment.go pkg/muxer/fmp4/init_segment_test.go
git commit -m "feat: add FMP4 init segment generation with multi-codec support"
```

### Task 19: FMP4 media segment generation

**Files:**
- Create: `pkg/muxer/fmp4/media_segment.go`
- Create: `pkg/muxer/fmp4/media_segment_test.go`

- [ ] **Step 1: Write tests**

Test that WriteSegment with a slice of AVFrames produces valid moof+mdat. Verify trun sample entries match frame sizes/durations.

- [ ] **Step 2: Implement**

Build moof (mfhd + traf per track with tfhd, tfdt, trun) and mdat (concatenated sample data).

- [ ] **Step 3: Commit**

```bash
git add pkg/muxer/fmp4/media_segment.go pkg/muxer/fmp4/media_segment_test.go
git commit -m "feat: add FMP4 media segment generation"
```

### Task 20: FMP4 Muxer main API

**Files:**
- Create: `pkg/muxer/fmp4/muxer.go`
- Create: `pkg/muxer/fmp4/muxer_test.go`

- [ ] **Step 1: Write tests**
- [ ] **Step 2: Implement**

```go
type Muxer struct {
    videoCodec     avframe.CodecType
    audioCodec     avframe.CodecType
    sequenceNumber uint32
}

func NewMuxer(videoCodec, audioCodec avframe.CodecType) *Muxer

// Init generates the init segment from sequence headers.
func (m *Muxer) Init(videoSeqHeader, audioSeqHeader *avframe.AVFrame) []byte

// WriteSegment generates a moof+mdat segment from a GOP of frames.
func (m *Muxer) WriteSegment(frames []*avframe.AVFrame) []byte
```

- [ ] **Step 3: Commit**

```bash
git add pkg/muxer/fmp4/muxer.go pkg/muxer/fmp4/muxer_test.go
git commit -m "feat: add FMP4 muxer main API"
```

## Chunk 5: Enhanced FLV Muxer

### Task 21: Update FLV muxer for Enhanced RTMP (H.265/AV1/Opus)

**Files:**
- Modify: `pkg/muxer/flv/muxer.go`
- Modify: `pkg/muxer/flv/types.go`
- Modify: `pkg/muxer/flv/muxer_test.go`

- [ ] **Step 1: Write tests for Enhanced FLV video/audio tags**

Test that H.265 video frame produces ExVideoTagHeader with FourCC `hvc1`.
Test that AV1 video frame uses FourCC `av01`.
Test that Opus audio uses enhanced audio format.

- [ ] **Step 2: Add Enhanced FLV constants to types.go**

```go
// Enhanced FLV FourCC codes
var (
    FourCCHEVC = [4]byte{'h', 'v', 'c', '1'}
    FourCCAV1  = [4]byte{'a', 'v', '0', '1'}
    FourCCVP9  = [4]byte{'v', 'p', '0', '9'}
)

// Enhanced video packet types
const (
    ExVideoPacketSequenceStart = 0
    ExVideoPacketCodedFrames   = 1
    ExVideoPacketSequenceEnd   = 2
)
```

- [ ] **Step 3: Update writeVideoTag for Enhanced codecs**

Branch on codec: H.264 uses classic format, H.265/AV1 use ExVideoTagHeader with FourCC.

- [ ] **Step 4: Update writeAudioTag for Opus**
- [ ] **Step 5: Run all FLV tests**

Run: `cd /Users/pingo/streamserver && go test ./pkg/muxer/flv/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/muxer/flv/
git commit -m "feat: add Enhanced FLV support for H.265, AV1, and Opus"
```

## Chunk 6: HTTP Streaming Module

### Task 22: HTTP module skeleton

**Files:**
- Create: `module/httpstream/module.go`

- [ ] **Step 1: Write test**

```go
// In test/integration/http_stream_test.go or module/httpstream/module_test.go
func TestHTTPModuleStartStop(t *testing.T) {
    cfg := &config.Config{}
    cfg.HTTP.Enabled = true
    cfg.HTTP.Listen = "127.0.0.1:0"
    cfg.HTTP.CORS = true
    cfg.Stream.RingBufferSize = 256
    cfg.Stream.NoPublisherTimeout = 5 * time.Second

    s := core.NewServer(cfg)
    mod := httpstream.NewModule()
    s.RegisterModule(mod)

    if err := s.Init(); err != nil {
        t.Fatalf("init: %v", err)
    }
    s.Shutdown()
}
```

- [ ] **Step 2: Implement module skeleton**

```go
package httpstream

import (
    "log"
    "net"
    "net/http"
    "sync"
    "github.com/im-pingo/liveforge/core"
)

type Module struct {
    server   *core.Server
    listener net.Listener
    httpSrv  *http.Server
    wg       sync.WaitGroup
}

func NewModule() *Module { return &Module{} }
func (m *Module) Name() string { return "httpstream" }

func (m *Module) Init(s *core.Server) error {
    m.server = s
    cfg := s.Config()

    ln, err := net.Listen("tcp", cfg.HTTP.Listen)
    if err != nil { return err }
    m.listener = ln

    mux := http.NewServeMux()
    mux.HandleFunc("/", m.handleStream)
    m.httpSrv = &http.Server{Handler: mux}

    log.Printf("HTTP stream listening on %s", ln.Addr())
    m.wg.Add(1)
    go func() {
        defer m.wg.Done()
        m.httpSrv.Serve(ln)
    }()
    return nil
}

func (m *Module) Hooks() []core.HookRegistration { return nil }
func (m *Module) Close() error {
    if m.httpSrv != nil { m.httpSrv.Close() }
    m.wg.Wait()
    log.Println("HTTP stream module stopped")
    return nil
}

func (m *Module) handleStream(w http.ResponseWriter, r *http.Request) {
    http.Error(w, "not implemented", http.StatusNotImplemented)
}
```

- [ ] **Step 3: Register in main.go**

```go
// cmd/liveforge/main.go, add:
import "github.com/im-pingo/liveforge/module/httpstream"

if cfg.HTTP.Enabled {
    s.RegisterModule(httpstream.NewModule())
}
```

- [ ] **Step 4: Commit**

```bash
git add module/httpstream/module.go cmd/liveforge/main.go
git commit -m "feat: add HTTP stream module skeleton"
```

### Task 23: HTTP handler with URL routing

**Files:**
- Create: `module/httpstream/handler.go`

- [ ] **Step 1: Implement URL parsing and routing**

Parse URL like `/live/test.flv` → app=`live`, key=`test`, format=`flv`.
Set response headers (Content-Type, CORS, Cache-Control).
Look up stream from hub, return 404/503 as appropriate.
Get or create muxer via MuxerManager, get SharedBufferReader.

- [ ] **Step 2: Write test**

```go
func TestParseStreamPath(t *testing.T) {
    tests := []struct{
        path, app, key, format string
        ok bool
    }{
        {"/live/test.flv", "live", "test", "flv", true},
        {"/live/test.ts", "live", "test", "ts", true},
        {"/app/stream.mp4", "app", "stream", "mp4", true},
        {"/noext", "", "", "", false},
    }
    for _, tt := range tests {
        app, key, format, ok := parseStreamPath(tt.path)
        if ok != tt.ok || app != tt.app || key != tt.key || format != tt.format {
            t.Errorf("parseStreamPath(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
                tt.path, app, key, format, ok, tt.app, tt.key, tt.format, tt.ok)
        }
    }
}
```

- [ ] **Step 3: Commit**

```bash
git add module/httpstream/handler.go
git commit -m "feat: add HTTP stream URL routing and path parsing"
```

### Task 24: Muxer worker goroutines

**Files:**
- Create: `module/httpstream/muxer_worker.go`

- [ ] **Step 1: Implement muxer worker registration**

During module Init, register muxer start callbacks for "flv", "ts", "mp4" on each stream's MuxerManager. Actually, since streams are created dynamically, the callback registration happens when the HTTP handler first accesses a stream.

```go
func (m *Module) registerMuxerCallbacks(stream *core.Stream) {
    mm := stream.MuxerManager()
    mm.RegisterMuxerStart("flv", func(inst *core.MuxerInstance, s *core.Stream) {
        go m.runFLVMuxer(inst, s)
    })
    mm.RegisterMuxerStart("ts", func(inst *core.MuxerInstance, s *core.Stream) {
        go m.runTSMuxer(inst, s)
    })
    mm.RegisterMuxerStart("mp4", func(inst *core.MuxerInstance, s *core.Stream) {
        go m.runFMP4Muxer(inst, s)
    })
}
```

- [ ] **Step 2: Implement FLV muxer worker**

```go
func (m *Module) runFLVMuxer(inst *core.MuxerInstance, stream *core.Stream) {
    defer inst.Buffer.Close()
    muxer := flv.NewMuxer()
    var buf bytes.Buffer

    // Write FLV header as init data
    hasVideo := stream.VideoSeqHeader() != nil
    hasAudio := stream.AudioSeqHeader() != nil
    muxer.WriteHeader(&buf, hasVideo, hasAudio)
    inst.SetInitData(buf.Bytes())
    buf.Reset()

    // Send sequence headers
    // Send GOP cache
    // Read live frames from ring buffer
    // For each frame: muxer.WriteFrame(&buf, frame), inst.Buffer.Write(buf.Bytes()), buf.Reset()
    // Exit on <-inst.Done
}
```

- [ ] **Step 3: Implement TS muxer worker** (similar pattern)
- [ ] **Step 4: Implement FMP4 muxer worker** (with GOP buffering)
- [ ] **Step 5: Commit**

```bash
git add module/httpstream/muxer_worker.go
git commit -m "feat: add muxer worker goroutines for FLV, TS, FMP4"
```

### Task 25: HTTP subscriber

**Files:**
- Create: `module/httpstream/subscriber.go`

- [ ] **Step 1: Implement HTTP subscriber serve loop**

```go
func (m *Module) serveStream(w http.ResponseWriter, r *http.Request, format string, stream *core.Stream) {
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming not supported", http.StatusInternalServerError)
        return
    }

    // Register muxer callbacks if not already registered for this stream
    m.registerMuxerCallbacks(stream)

    mm := stream.MuxerManager()
    reader, inst := mm.GetOrCreateMuxer(format)
    defer mm.ReleaseMuxer(format)

    // Set headers
    switch format {
    case "flv": w.Header().Set("Content-Type", "video/x-flv")
    case "ts":  w.Header().Set("Content-Type", "video/mp2t")
    case "mp4": w.Header().Set("Content-Type", "video/mp4")
    }
    w.Header().Set("Transfer-Encoding", "chunked")
    w.Header().Set("Cache-Control", "no-cache")
    if m.server.Config().HTTP.CORS {
        w.Header().Set("Access-Control-Allow-Origin", "*")
    }

    // Write init data (FLV header, FMP4 init segment)
    // Spin briefly if not yet available
    for i := 0; i < 10; i++ {
        if data := inst.InitData(); data != nil {
            w.Write(data)
            flusher.Flush()
            break
        }
        time.Sleep(10 * time.Millisecond)
    }

    // Read loop
    for {
        select {
        case <-r.Context().Done():
            return
        default:
        }
        data, ok := reader.Read()
        if !ok {
            return
        }
        if _, err := w.Write(data); err != nil {
            return
        }
        flusher.Flush()
    }
}
```

- [ ] **Step 2: Commit**

```bash
git add module/httpstream/subscriber.go
git commit -m "feat: add HTTP subscriber with chunked streaming"
```

### Task 26: Enable http_stream in config and update YAML

**Files:**
- Modify: `configs/streamserver.yaml`

- [ ] **Step 1: Set http_stream.enabled to true**

```yaml
http_stream:
  enabled: true
  listen: ":8080"
  cors: true
```

- [ ] **Step 2: Commit**

```bash
git add configs/streamserver.yaml
git commit -m "chore: enable HTTP streaming in default config"
```

## Chunk 7: Integration Testing

### Task 27: End-to-end test

**Files:**
- Create: `test/integration/http_stream_test.go`

- [ ] **Step 1: Write integration test**

Test: start server with RTMP+HTTP modules → push via RTMP using ffmpeg test source → verify HTTP-FLV, HTTP-TS, HTTP-FMP4 are accessible and return valid data via HTTP GET.

Use `net/http` client to GET `/live/test.flv`, read a few KB, verify FLV header (bytes "FLV").
GET `/live/test.ts`, verify first bytes are 0x47 (TS sync).
GET `/live/test.mp4`, verify first bytes are ftyp box.

- [ ] **Step 2: Run integration tests**

```bash
cd /Users/pingo/streamserver && go test ./test/integration/ -run TestHTTPStream -v -timeout 30s
```

- [ ] **Step 3: Manual E2E test**

```bash
# Terminal 1: start server
cd /Users/pingo/streamserver && go run ./cmd/liveforge/

# Terminal 2: push test stream
ffmpeg -re -f lavfi -i testsrc=size=640x480:rate=30 -f lavfi -i sine=frequency=1000:sample_rate=44100 -pix_fmt yuv420p -c:v libx264 -profile:v baseline -c:a aac -f flv rtmp://localhost:1935/live/test

# Terminal 3: test each format
ffprobe http://localhost:8080/live/test.flv
ffprobe http://localhost:8080/live/test.ts
ffprobe http://localhost:8080/live/test.mp4

ffplay http://localhost:8080/live/test.flv
```

- [ ] **Step 4: Fix any issues found during testing**
- [ ] **Step 5: Commit**

```bash
git add test/integration/http_stream_test.go
git commit -m "test: add HTTP streaming integration tests"
```

### Task 28: Final verification

- [ ] **Step 1: Run all tests**

```bash
cd /Users/pingo/streamserver && go test ./... -v
```

- [ ] **Step 2: Run go vet**

```bash
cd /Users/pingo/streamserver && go vet ./...
```

- [ ] **Step 3: Verify no build errors**

```bash
cd /Users/pingo/streamserver && go build ./...
```
