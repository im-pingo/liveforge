# Phase 3: RTP/SDP/RTSP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add RTSP publish (ANNOUNCE) and subscribe (DESCRIBE/PLAY) support with TCP interleaved and UDP transport, building shared RTP and SDP libraries for future WebRTC/SIP reuse.

**Architecture:** Three layers — `pkg/sdp/` (SDP parse/marshal), `pkg/rtp/` (RTP packetize/depacketize via pion/rtp), `module/rtsp/` (RTSP signaling + session + transport). RTSP subscribers read directly from Stream.RingBuffer (no MuxerManager) since RTP requires per-subscriber SSRC/seq state.

**Tech Stack:** Go, `pion/rtp` (RTP packet type), existing `pkg/avframe`, `pkg/codec/*`, `core/*`

**Spec:** `docs/superpowers/specs/2026-03-19-phase3-rtp-sdp-rtsp-design.md`

---

## File Structure

### New files: `pkg/sdp/`
- `sdp.go` — Types (SessionDescription, MediaDescription, Origin, etc.) + Parse/Marshal
- `sdp_test.go` — Parse/Marshal round-trip tests
- `builder.go` — BuildFromMediaInfo for generating DESCRIBE responses
- `builder_test.go` — Builder tests

### New files: `pkg/rtp/`
- `packetizer.go` — Packetizer/Depacketizer interfaces + factory functions
- `session.go` — RTP Session (SSRC, seq, timestamp management)
- `rtcp.go` — RTCP SR/RR build/parse
- `h264.go` — H264Packetizer + H264Depacketizer (FU-A, STAP-A)
- `h265.go` — H265Packetizer + H265Depacketizer (FU, AP)
- `aac.go` — AACPacketizer + AACDepacketizer (AAC-hbr, RFC 3640)
- `opus.go` — OpusPacketizer + OpusDepacketizer
- `g711.go` — G711Packetizer + G711Depacketizer (PCMU + PCMA)
- `vp8.go` — VP8Packetizer + VP8Depacketizer
- `vp9.go` — VP9Packetizer + VP9Depacketizer
- `av1.go` — AV1Packetizer + AV1Depacketizer
- `mp3.go` — MP3Packetizer + MP3Depacketizer
- `g722.go` — G722Packetizer + G722Depacketizer
- `g729.go` — G729Packetizer + G729Depacketizer
- `speex.go` — SpeexPacketizer + SpeexDepacketizer
- Tests: one `*_test.go` per codec file + `session_test.go` + `rtcp_test.go` + `packetizer_test.go`

### New files: `module/rtsp/`
- `server.go` — TCP listener, core.Module implementation
- `conn.go` — RTSP request/response parser (ReadRequest / WriteResponse)
- `session.go` — RTSP session state machine + timeout management
- `handler.go` — Command dispatch (OPTIONS/DESCRIBE/SETUP/PLAY/PAUSE/ANNOUNCE/RECORD/TEARDOWN/GET_PARAMETER)
- `publisher.go` — RTSP Publisher: RTP → Depacketize → AVFrame → Stream.WriteFrame
- `subscriber.go` — RTSP Subscriber: RingBuffer → AVFrame → Packetize → RTP → send
- `transport.go` — UDP/TCP transport + PortManager
- `interleaved.go` — TCP interleaved frame ($+channel+length+data)
- Tests: `conn_test.go`, `session_test.go`, `handler_test.go`, `publisher_test.go`, `subscriber_test.go`, `transport_test.go`, `server_test.go`

### Modified files
- `cmd/liveforge/main.go` — Register RTSP module
- `go.mod` / `go.sum` — Add `github.com/pion/rtp/v2` dependency

---

## Chunk 1: SDP Package

### Task 1: SDP types and parser

**Files:**
- Create: `pkg/sdp/sdp.go`
- Create: `pkg/sdp/sdp_test.go`

- [ ] **Step 1: Write failing test for SDP Parse**

```go
// pkg/sdp/sdp_test.go
package sdp

import (
	"testing"
)

func TestParseInvalidSDP(t *testing.T) {
	_, err := Parse([]byte("not valid sdp"))
	if err == nil {
		t.Fatal("expected error for invalid SDP")
	}

	_, err = Parse([]byte(""))
	if err == nil {
		t.Fatal("expected error for empty SDP")
	}

	// Missing required v= line
	_, err = Parse([]byte("s=Test\r\n"))
	if err == nil {
		t.Fatal("expected error for SDP without v= line")
	}
}

func TestParseMinimalSDP(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 12345 1 IN IP4 127.0.0.1\r\n" +
		"s=Test\r\n" +
		"t=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=control:trackID=0\r\n" +
		"m=audio 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 MPEG4-GENERIC/44100/2\r\n" +
		"a=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3\r\n" +
		"a=control:trackID=1\r\n"

	sd, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sd.Version != 0 {
		t.Errorf("Version = %d, want 0", sd.Version)
	}
	if sd.Origin.Address != "127.0.0.1" {
		t.Errorf("Origin.Address = %q", sd.Origin.Address)
	}
	if sd.Name != "Test" {
		t.Errorf("Name = %q", sd.Name)
	}
	if len(sd.Media) != 2 {
		t.Fatalf("Media count = %d, want 2", len(sd.Media))
	}
	// Video track
	v := sd.Media[0]
	if v.Type != "video" {
		t.Errorf("Media[0].Type = %q", v.Type)
	}
	if len(v.Formats) != 1 || v.Formats[0] != 96 {
		t.Errorf("Media[0].Formats = %v", v.Formats)
	}
	rm := v.RTPMap(96)
	if rm == nil || rm.EncodingName != "H264" || rm.ClockRate != 90000 {
		t.Errorf("RTPMap(96) = %+v", rm)
	}
	if v.Control() != "trackID=0" {
		t.Errorf("Control = %q", v.Control())
	}
	// Audio track
	a := sd.Media[1]
	if a.Type != "audio" {
		t.Errorf("Media[1].Type = %q", a.Type)
	}
	arm := a.RTPMap(97)
	if arm == nil || arm.EncodingName != "MPEG4-GENERIC" || arm.ClockRate != 44100 || arm.Channels != 2 {
		t.Errorf("RTPMap(97) = %+v", arm)
	}
	if a.FMTP(97) == "" {
		t.Error("FMTP(97) is empty")
	}
}

func TestParseMarshalRoundTrip(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s=LiveForge\r\n" +
		"t=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n"

	sd, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out := sd.Marshal()
	sd2, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if sd2.Name != sd.Name {
		t.Errorf("Name mismatch: %q vs %q", sd2.Name, sd.Name)
	}
	if len(sd2.Media) != len(sd.Media) {
		t.Errorf("Media count mismatch")
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/sdp/ -v`
Expected: FAIL — package doesn't exist

- [ ] **Step 2: Implement SDP types and Parse/Marshal**

```go
// pkg/sdp/sdp.go
package sdp

import (
	"fmt"
	"strconv"
	"strings"
)

type SessionDescription struct {
	Version    int
	Origin     Origin
	Name       string
	Info       string
	Connection *Connection
	Timing     Timing
	Attributes []Attribute
	Media      []MediaDescription
}

type Origin struct {
	Username       string
	SessionID      string
	SessionVersion string
	NetType        string
	AddrType       string
	Address        string
}

type Connection struct {
	NetType  string
	AddrType string
	Address  string
}

type Timing struct {
	Start int64
	Stop  int64
}

type Attribute struct {
	Key   string
	Value string
}

type MediaDescription struct {
	Type       string
	Port       int
	Proto      string
	Formats    []int
	Connection *Connection
	Bandwidth  int
	Attributes []Attribute
}

type RTPMap struct {
	PayloadType  int
	EncodingName string
	ClockRate    int
	Channels     int
}

// Parse parses SDP text into a SessionDescription.
func Parse(data []byte) (*SessionDescription, error) {
	// ... full implementation parsing v=, o=, s=, t=, c=, a=, m= lines
	// Detailed implementation: split lines by \r\n or \n, iterate and build struct
}

// Marshal serializes a SessionDescription to SDP text.
func (sd *SessionDescription) Marshal() []byte {
	// ... full implementation writing v=, o=, s=, t=, c=, a=, m= lines
}

// RTPMap extracts the rtpmap attribute for a given payload type.
func (md *MediaDescription) RTPMap(pt int) *RTPMap {
	// Parse "a=rtpmap:<pt> <encoding>/<clockrate>[/<channels>]"
}

// FMTP extracts the fmtp attribute value for a given payload type.
func (md *MediaDescription) FMTP(pt int) string {
	// Parse "a=fmtp:<pt> <params>"
}

// Control returns the a=control value.
func (md *MediaDescription) Control() string {
	for _, a := range md.Attributes {
		if a.Key == "control" {
			return a.Value
		}
	}
	return ""
}

// Direction returns the direction attribute (sendrecv/sendonly/recvonly/inactive).
func (md *MediaDescription) Direction() string {
	for _, a := range md.Attributes {
		switch a.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			return a.Key
		}
	}
	return "sendrecv"
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/sdp/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/sdp/sdp.go pkg/sdp/sdp_test.go
git commit -m "feat: add SDP parser and marshaler"
```

### Task 2: SDP builder (MediaInfo → SDP)

**Files:**
- Create: `pkg/sdp/builder.go`
- Create: `pkg/sdp/builder_test.go`

- [ ] **Step 1: Write failing test for BuildFromMediaInfo**

```go
// pkg/sdp/builder_test.go
package sdp

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestBuildFromMediaInfoH264AAC(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
		SampleRate: 44100,
		Channels:   2,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 2 {
		t.Fatalf("Media count = %d, want 2", len(sd.Media))
	}
	// Video
	vMap := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if vMap == nil || vMap.EncodingName != "H264" || vMap.ClockRate != 90000 {
		t.Errorf("video rtpmap = %+v", vMap)
	}
	// Audio
	aMap := sd.Media[1].RTPMap(sd.Media[1].Formats[0])
	if aMap == nil || aMap.EncodingName != "MPEG4-GENERIC" || aMap.ClockRate != 44100 {
		t.Errorf("audio rtpmap = %+v", aMap)
	}
	// Each media must have a=control
	if sd.Media[0].Control() == "" || sd.Media[1].Control() == "" {
		t.Error("missing control attribute")
	}
}

func TestBuildFromMediaInfoVideoOnly(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH265,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "H265" {
		t.Errorf("rtpmap = %+v", rm)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/sdp/ -run TestBuildFrom -v`
Expected: FAIL — BuildFromMediaInfo not defined

- [ ] **Step 2: Implement BuildFromMediaInfo**

```go
// pkg/sdp/builder.go
package sdp

import "github.com/im-pingo/liveforge/pkg/avframe"

// codecRTPInfo maps CodecType to (encoding name, clock rate, default PT).
var codecRTPInfo = map[avframe.CodecType]struct {
	Name      string
	ClockRate int
	PT        int
}{
	avframe.CodecH264:  {"H264", 90000, 96},
	avframe.CodecH265:  {"H265", 90000, 97},
	avframe.CodecVP8:   {"VP8", 90000, 98},
	avframe.CodecVP9:   {"VP9", 90000, 99},
	avframe.CodecAV1:   {"AV1", 90000, 100},
	avframe.CodecAAC:   {"MPEG4-GENERIC", 0, 101}, // clock rate = sample rate
	avframe.CodecOpus:  {"opus", 48000, 111},
	avframe.CodecMP3:   {"MPA", 90000, 14},
	avframe.CodecG711U: {"PCMU", 8000, 0},
	avframe.CodecG711A: {"PCMA", 8000, 8},
	avframe.CodecG722:  {"G722", 8000, 9},
	avframe.CodecG729:  {"G729", 8000, 18},
	avframe.CodecSpeex: {"speex", 0, 102}, // clock rate = sample rate
}

// BuildFromMediaInfo generates an SDP SessionDescription from stream MediaInfo.
// Uses MediaInfo.SampleRate and MediaInfo.Channels for audio rtpmap lines.
func BuildFromMediaInfo(info *avframe.MediaInfo, baseURL string, serverAddr string) *SessionDescription {
	// Build session-level fields (v=, o=, s=, c=, t=)
	// For each codec present in MediaInfo, add m= section with rtpmap, fmtp, control
	// AAC fmtp: "profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3"
	// Opus: add "a=rtpmap:111 opus/48000/2"
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/sdp/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/sdp/builder.go pkg/sdp/builder_test.go
git commit -m "feat: add SDP builder from MediaInfo"
```

---

## Chunk 2: RTP Package — Core + pion/rtp dependency

### Task 3: Add pion/rtp dependency and RTP interfaces

**Files:**
- Modify: `go.mod`
- Create: `pkg/rtp/packetizer.go`
- Create: `pkg/rtp/packetizer_test.go`

- [ ] **Step 1: Add pion/rtp dependency**

```bash
cd /Users/pingo-macmini/Documents/liveforge && go get github.com/pion/rtp/v2
```

- [ ] **Step 2: Write failing test for NewPacketizer factory**

```go
// pkg/rtp/packetizer_test.go
package rtp

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestNewPacketizerH264(t *testing.T) {
	p, err := NewPacketizer(avframe.CodecH264)
	if err != nil {
		t.Fatalf("NewPacketizer: %v", err)
	}
	if p == nil {
		t.Fatal("packetizer is nil")
	}
}

func TestNewDepacketizerH264(t *testing.T) {
	d, err := NewDepacketizer(avframe.CodecH264)
	if err != nil {
		t.Fatalf("NewDepacketizer: %v", err)
	}
	if d == nil {
		t.Fatal("depacketizer is nil")
	}
}

func TestNewPacketizerUnknown(t *testing.T) {
	_, err := NewPacketizer(avframe.CodecType(255))
	if err == nil {
		t.Fatal("expected error for unknown codec")
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -v`
Expected: FAIL — package doesn't exist

- [ ] **Step 3: Implement Packetizer/Depacketizer interfaces and factory**

```go
// pkg/rtp/packetizer.go
package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

const DefaultMTU = 1400

// Packetizer splits an AVFrame into RTP packets.
// Does not track SSRC/seq/timestamp — those are managed by Session.
type Packetizer interface {
	Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error)
}

// Depacketizer reassembles RTP packets into AVFrames.
// Stateful — buffers fragments until a complete frame is received.
// Returns nil AVFrame when the packet is a fragment that doesn't complete a frame.
type Depacketizer interface {
	Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error)
}

func NewPacketizer(codec avframe.CodecType) (Packetizer, error) {
	switch codec {
	case avframe.CodecH264:
		return &H264Packetizer{}, nil
	// ... other codecs added in subsequent tasks
	default:
		return nil, fmt.Errorf("unsupported codec for packetizer: %v", codec)
	}
}

func NewDepacketizer(codec avframe.CodecType) (Depacketizer, error) {
	switch codec {
	case avframe.CodecH264:
		return &H264Depacketizer{}, nil
	// ... other codecs added in subsequent tasks
	default:
		return nil, fmt.Errorf("unsupported codec for depacketizer: %v", codec)
	}
}
```

Note: Initially the factory only supports H.264. Include stub types so the package compiles before Task 6 adds real implementations:

```go
// Stub types — replaced with real implementations in Task 6
type H264Packetizer struct{}

func (p *H264Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	return nil, fmt.Errorf("H264Packetizer not yet implemented")
}

type H264Depacketizer struct{}

func (d *H264Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	return nil, fmt.Errorf("H264Depacketizer not yet implemented")
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -v`
Expected: PASS (stubs satisfy interfaces, factory returns them, unknown codec returns error)

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum pkg/rtp/packetizer.go pkg/rtp/packetizer_test.go
git commit -m "feat: add RTP packetizer/depacketizer interfaces with pion/rtp"
```

### Task 4: RTP Session

**Files:**
- Create: `pkg/rtp/session.go`
- Create: `pkg/rtp/session_test.go`

- [ ] **Step 1: Write failing test**

```go
// pkg/rtp/session_test.go
package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp/v2"
)

func TestNewSession(t *testing.T) {
	s := NewSession(96, 90000)
	if s.SSRC == 0 {
		t.Error("SSRC should be non-zero random")
	}
	if s.PayloadType != 96 {
		t.Errorf("PayloadType = %d", s.PayloadType)
	}
	if s.ClockRate != 90000 {
		t.Errorf("ClockRate = %d", s.ClockRate)
	}
}

func TestSessionWrapPackets(t *testing.T) {
	s := NewSession(96, 90000)
	pkts := []*pionrtp.Packet{
		{Header: pionrtp.Header{}},
		{Header: pionrtp.Header{}},
	}
	wrapped := s.WrapPackets(pkts, 1000) // 1000ms DTS
	if len(wrapped) != 2 {
		t.Fatalf("len = %d", len(wrapped))
	}
	// Both packets should have same SSRC
	if wrapped[0].SSRC != s.SSRC || wrapped[1].SSRC != s.SSRC {
		t.Error("SSRC mismatch")
	}
	// PayloadType
	if wrapped[0].PayloadType != 96 {
		t.Errorf("PT = %d", wrapped[0].PayloadType)
	}
	// Sequence numbers should be sequential
	if wrapped[1].SequenceNumber != wrapped[0].SequenceNumber+1 {
		t.Error("seq not sequential")
	}
	// Timestamp: 1000ms * 90000 / 1000 = 90000
	if wrapped[0].Timestamp != 90000 {
		t.Errorf("Timestamp = %d, want 90000", wrapped[0].Timestamp)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -run TestSession -v`
Expected: FAIL

- [ ] **Step 2: Implement Session**

```go
// pkg/rtp/session.go
package rtp

import (
	"crypto/rand"
	"encoding/binary"

	pionrtp "github.com/pion/rtp/v2"
)

type Session struct {
	SSRC        uint32
	PayloadType uint8
	ClockRate   uint32
	seqNum      uint16
}

func NewSession(pt uint8, clockRate uint32) *Session {
	var buf [4]byte
	rand.Read(buf[:])
	return &Session{
		SSRC:        binary.BigEndian.Uint32(buf[:]),
		PayloadType: pt,
		ClockRate:   clockRate,
	}
}

func (s *Session) WrapPackets(pkts []*pionrtp.Packet, dtsMsec int64) []*pionrtp.Packet {
	ts := uint32(dtsMsec * int64(s.ClockRate) / 1000)
	for _, pkt := range pkts {
		pkt.SSRC = s.SSRC
		pkt.PayloadType = s.PayloadType
		pkt.Timestamp = ts
		pkt.SequenceNumber = s.seqNum
		s.seqNum++
	}
	return pkts
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -run TestSession -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/session.go pkg/rtp/session_test.go
git commit -m "feat: add RTP session with SSRC/seq/timestamp management"
```

### Task 5: RTCP helpers

**Files:**
- Create: `pkg/rtp/rtcp.go`
- Create: `pkg/rtp/rtcp_test.go`

- [ ] **Step 1: Write failing test for RTCP SR build/parse round-trip**

```go
// pkg/rtp/rtcp_test.go
package rtp

import "testing"

func TestBuildParseSR(t *testing.T) {
	data := BuildSR(0x12345678, 0xAABBCCDD00112233, 90000, 100, 50000)
	sr, err := ParseSR(data)
	if err != nil {
		t.Fatalf("ParseSR: %v", err)
	}
	if sr.SSRC != 0x12345678 {
		t.Errorf("SSRC = 0x%x", sr.SSRC)
	}
	if sr.NTPTime != 0xAABBCCDD00112233 {
		t.Errorf("NTPTime = 0x%x", sr.NTPTime)
	}
	if sr.RTPTime != 90000 {
		t.Errorf("RTPTime = %d", sr.RTPTime)
	}
	if sr.PacketCount != 100 {
		t.Errorf("PacketCount = %d", sr.PacketCount)
	}
	if sr.OctetCount != 50000 {
		t.Errorf("OctetCount = %d", sr.OctetCount)
	}
}

func TestBuildParseRR(t *testing.T) {
	reports := []ReceptionReport{
		{SSRC: 0xAABBCCDD, FractionLost: 10, TotalLost: 5, HighestSeq: 1000, Jitter: 20},
	}
	data := BuildRR(0x12345678, reports)
	rr, err := ParseRR(data)
	if err != nil {
		t.Fatalf("ParseRR: %v", err)
	}
	if rr.SSRC != 0x12345678 {
		t.Errorf("SSRC = 0x%x", rr.SSRC)
	}
	if len(rr.Reports) != 1 {
		t.Fatalf("Reports count = %d", len(rr.Reports))
	}
	if rr.Reports[0].SSRC != 0xAABBCCDD {
		t.Errorf("Report SSRC = 0x%x", rr.Reports[0].SSRC)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -run TestBuildParse -v`
Expected: FAIL

- [ ] **Step 2: Implement RTCP types and build/parse functions**

RTCP SR packet format (RFC 3550 Section 6.4.1): Version=2, PT=200, length, SSRC, NTP timestamp (8 bytes), RTP timestamp (4 bytes), packet count (4 bytes), octet count (4 bytes).

RTCP RR packet format (RFC 3550 Section 6.4.2): Version=2, PT=201, length, SSRC, then per report block: SSRC (4), fraction lost + total lost (4), highest seq (4), jitter (4), last SR (4), delay since last SR (4).

```go
// pkg/rtp/rtcp.go
package rtp

import (
	"encoding/binary"
	"errors"
)

type SenderReport struct {
	SSRC        uint32
	NTPTime     uint64
	RTPTime     uint32
	PacketCount uint32
	OctetCount  uint32
}

type ReceiverReport struct {
	SSRC    uint32
	Reports []ReceptionReport
}

type ReceptionReport struct {
	SSRC             uint32
	FractionLost     uint8
	TotalLost        uint32
	HighestSeq       uint32
	Jitter           uint32
	LastSR           uint32
	DelaySinceLastSR uint32
}

func BuildSR(ssrc uint32, ntpTime uint64, rtpTime, packetCount, octetCount uint32) []byte { ... }
func ParseSR(data []byte) (*SenderReport, error) { ... }
func BuildRR(ssrc uint32, reports []ReceptionReport) []byte { ... }
func ParseRR(data []byte) (*ReceiverReport, error) { ... }
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -run TestBuildParse -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/rtcp.go pkg/rtp/rtcp_test.go
git commit -m "feat: add RTCP SR/RR build and parse helpers"
```

---

## Chunk 3: RTP Codec Packetizers/Depacketizers

Each task follows the same TDD pattern: write test → implement → add to factory switch → commit.

### Task 6: H.264 packetizer/depacketizer (FU-A, STAP-A)

**Files:**
- Create: `pkg/rtp/h264.go`
- Create: `pkg/rtp/h264_test.go`

- [ ] **Step 1: Write failing tests**

```go
// pkg/rtp/h264_test.go
package rtp

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestH264PacketizeSingleNAL(t *testing.T) {
	// Small NAL that fits in one packet
	nalData := make([]byte, 100)
	nalData[0] = 0x65 // IDR slice
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)
	p := &H264Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(pkts))
	}
	if !pkts[0].Marker {
		t.Error("expected marker bit on single NAL")
	}
}

func TestH264PacketizeFUA(t *testing.T) {
	// Large NAL that needs FU-A fragmentation
	nalData := make([]byte, 3000)
	nalData[0] = 0x65
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)
	p := &H264Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected >=3 FU-A packets, got %d", len(pkts))
	}
	// First FU-A has S bit, last has E bit + marker
	if pkts[0].Payload[1]&0x80 == 0 {
		t.Error("first FU-A missing S bit")
	}
	if !pkts[len(pkts)-1].Marker {
		t.Error("last FU-A missing marker bit")
	}
}

func TestH264DepacketizeRoundTrip(t *testing.T) {
	nalData := make([]byte, 3000)
	nalData[0] = 0x65
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)

	p := &H264Packetizer{}
	pkts, _ := p.Packetize(frame, 1400)

	d := &H264Depacketizer{}
	var result *avframe.AVFrame
	for _, pkt := range pkts {
		f, err := d.Depacketize(pkt)
		if err != nil {
			t.Fatalf("Depacketize: %v", err)
		}
		if f != nil {
			result = f
		}
	}
	if result == nil {
		t.Fatal("no frame reassembled")
	}
	if len(result.Payload) != len(nalData) {
		t.Errorf("payload len = %d, want %d", len(result.Payload), len(nalData))
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -run TestH264 -v`
Expected: FAIL

- [ ] **Step 2: Implement H264Packetizer and H264Depacketizer**

H264Packetizer: if NAL ≤ MTU → single NAL packet. If > MTU → FU-A fragments. FU indicator = (nal[0] & 0xE0) | 28, FU header = (S/E bits << 6) | (nal[0] & 0x1F). Set marker bit on last fragment.

H264Depacketizer: check NAL type. Type 28 (FU-A) → buffer fragments until E bit, reconstruct NAL. Type 1-23 → single NAL, return frame immediately. Type 24 (STAP-A) → extract individual NALs.

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./pkg/rtp/ -run TestH264 -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/h264.go pkg/rtp/h264_test.go pkg/rtp/packetizer.go
git commit -m "feat: add H.264 RTP packetizer/depacketizer (FU-A, STAP-A)"
```

### Task 7: H.265 packetizer/depacketizer (FU, AP)

**Files:**
- Create: `pkg/rtp/h265.go`
- Create: `pkg/rtp/h265_test.go`

Same TDD pattern as Task 6. H.265 FU uses 3-byte header (2-byte NAL header + 1-byte FU header). AP aggregation for VPS+SPS+PPS. Add `avframe.CodecH265` case to factory.

- [ ] **Step 1: Write failing tests** (single NAL, FU fragment, round-trip)
- [ ] **Step 2: Implement H265Packetizer and H265Depacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/h265.go pkg/rtp/h265_test.go pkg/rtp/packetizer.go
git commit -m "feat: add H.265 RTP packetizer/depacketizer (FU, AP)"
```

### Task 8: AAC packetizer/depacketizer (RFC 3640 AAC-hbr)

**Files:**
- Create: `pkg/rtp/aac.go`
- Create: `pkg/rtp/aac_test.go`

AAC-hbr: 2-byte AU-headers-length + per-AU: 13-bit size + 3-bit index. Typically one AAC frame per RTP packet. Test: packetize a 400-byte AAC frame, verify AU-header. Round-trip test.

- [ ] **Step 1: Write failing tests**
- [ ] **Step 2: Implement AACPacketizer and AACDepacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/aac.go pkg/rtp/aac_test.go pkg/rtp/packetizer.go
git commit -m "feat: add AAC RTP packetizer/depacketizer (RFC 3640 AAC-hbr)"
```

### Task 9: Opus packetizer/depacketizer

**Files:**
- Create: `pkg/rtp/opus.go`
- Create: `pkg/rtp/opus_test.go`

Opus: single frame per RTP packet. Trivial — payload = opus frame bytes. Marker bit on first packet after silence.

- [ ] **Step 1: Write failing tests**
- [ ] **Step 2: Implement OpusPacketizer and OpusDepacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/opus.go pkg/rtp/opus_test.go pkg/rtp/packetizer.go
git commit -m "feat: add Opus RTP packetizer/depacketizer"
```

### Task 10: G.711 packetizer/depacketizer (PCMU + PCMA)

**Files:**
- Create: `pkg/rtp/g711.go`
- Create: `pkg/rtp/g711_test.go`

G.711: raw samples directly in payload. 160 bytes per 20ms frame at 8kHz. Single struct handles both PCMU (PT 0) and PCMA (PT 8) since packetization is identical.

- [ ] **Step 1: Write failing tests**
- [ ] **Step 2: Implement G711Packetizer and G711Depacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/g711.go pkg/rtp/g711_test.go pkg/rtp/packetizer.go
git commit -m "feat: add G.711 RTP packetizer/depacketizer (PCMU/PCMA)"
```

### Task 11: VP8 packetizer/depacketizer

**Files:**
- Create: `pkg/rtp/vp8.go`
- Create: `pkg/rtp/vp8_test.go`

VP8 (RFC 7741): 1+ byte payload descriptor. X/R/N/S/PartID fields. Fragmentation via S bit (start of partition). If > MTU, split payload and set S=1 on first fragment only.

- [ ] **Step 1: Write failing tests**
- [ ] **Step 2: Implement VP8Packetizer and VP8Depacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/vp8.go pkg/rtp/vp8_test.go pkg/rtp/packetizer.go
git commit -m "feat: add VP8 RTP packetizer/depacketizer"
```

### Task 12: VP9 packetizer/depacketizer

**Files:**
- Create: `pkg/rtp/vp9.go`
- Create: `pkg/rtp/vp9_test.go`

VP9 (draft-ietf-payload-vp9): Payload descriptor with I/P/L/F/B/E/V/Z flags. B=beginning of frame, E=end of frame for fragmentation.

- [ ] **Step 1: Write failing tests**
- [ ] **Step 2: Implement VP9Packetizer and VP9Depacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/vp9.go pkg/rtp/vp9_test.go pkg/rtp/packetizer.go
git commit -m "feat: add VP9 RTP packetizer/depacketizer"
```

### Task 13: AV1 packetizer/depacketizer

**Files:**
- Create: `pkg/rtp/av1.go`
- Create: `pkg/rtp/av1_test.go`

AV1 (draft-ietf-avt-rtp-av1): Aggregation header (Z/Y/W/N fields) + OBU elements. OBU size prefix for aggregated OBUs. Fragmentation: Z=continuation, Y=new OBU start.

- [ ] **Step 1: Write failing tests**
- [ ] **Step 2: Implement AV1Packetizer and AV1Depacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/av1.go pkg/rtp/av1_test.go pkg/rtp/packetizer.go
git commit -m "feat: add AV1 RTP packetizer/depacketizer"
```

### Task 14: MP3 packetizer/depacketizer

**Files:**
- Create: `pkg/rtp/mp3.go`
- Create: `pkg/rtp/mp3_test.go`

MP3 (RFC 2250): 4-byte header: 2 bytes MBZ + 2 bytes fragment offset. Usually one frame per packet (offset=0).

- [ ] **Step 1: Write failing tests**
- [ ] **Step 2: Implement MP3Packetizer and MP3Depacketizer**
- [ ] **Step 3: Commit**

```bash
git add pkg/rtp/mp3.go pkg/rtp/mp3_test.go pkg/rtp/packetizer.go
git commit -m "feat: add MP3 RTP packetizer/depacketizer"
```

### Task 15: G.722, G.729, Speex packetizers/depacketizers

**Files:**
- Create: `pkg/rtp/g722.go`, `pkg/rtp/g722_test.go`
- Create: `pkg/rtp/g729.go`, `pkg/rtp/g729_test.go`
- Create: `pkg/rtp/speex.go`, `pkg/rtp/speex_test.go`

All three are simple: raw coded frames directly in payload (similar to G.711).
- G.722: direct payload, clock rate 8000 per RFC (actual 16kHz)
- G.729: 10 bytes per 10ms frame, possibly multiple frames per packet
- Speex: single frame per packet

- [ ] **Step 1: Write failing tests for all three**
- [ ] **Step 2: Implement all three packetizers/depacketizers**
- [ ] **Step 3: Add all three to factory switch**
- [ ] **Step 4: Commit**

```bash
git add pkg/rtp/g722.go pkg/rtp/g722_test.go pkg/rtp/g729.go pkg/rtp/g729_test.go pkg/rtp/speex.go pkg/rtp/speex_test.go pkg/rtp/packetizer.go
git commit -m "feat: add G.722, G.729, Speex RTP packetizers/depacketizers"
```

---

## Chunk 4: RTSP Module — Connection and Session

### Task 16: RTSP request/response parser

**Files:**
- Create: `module/rtsp/conn.go`
- Create: `module/rtsp/conn_test.go`

- [ ] **Step 1: Write failing test for ReadRequest**

```go
// module/rtsp/conn_test.go
package rtsp

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestReadRequest(t *testing.T) {
	raw := "DESCRIBE rtsp://host/live/test RTSP/1.0\r\n" +
		"CSeq: 1\r\n" +
		"Accept: application/sdp\r\n" +
		"\r\n"
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	req, err := ReadRequest(r)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "DESCRIBE" {
		t.Errorf("Method = %q", req.Method)
	}
	if req.URL != "rtsp://host/live/test" {
		t.Errorf("URL = %q", req.URL)
	}
	if req.Headers.Get("CSeq") != "1" {
		t.Errorf("CSeq = %q", req.Headers.Get("CSeq"))
	}
}

func TestReadRequestWithBody(t *testing.T) {
	body := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n"
	raw := "ANNOUNCE rtsp://host/live/test RTSP/1.0\r\n" +
		"CSeq: 2\r\n" +
		"Content-Type: application/sdp\r\n" +
		"Content-Length: " + fmt.Sprintf("%d", len(body)) + "\r\n" +
		"\r\n" + body
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	req, err := ReadRequest(r)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "ANNOUNCE" {
		t.Errorf("Method = %q", req.Method)
	}
	if string(req.Body) != body {
		t.Errorf("Body = %q", string(req.Body))
	}
}

func TestWriteResponse(t *testing.T) {
	var buf bytes.Buffer
	resp := &Response{
		StatusCode: 200,
		Reason:     "OK",
	}
	resp.Headers = make(http.Header)
	resp.Headers.Set("CSeq", "1")
	resp.Headers.Set("Session", "abc123")
	err := WriteResponse(&buf, resp)
	if err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "RTSP/1.0 200 OK\r\n") {
		t.Errorf("missing status line in: %q", out)
	}
	if !strings.Contains(out, "CSeq: 1\r\n") {
		t.Errorf("missing CSeq in: %q", out)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run TestRead -v`
Expected: FAIL — package doesn't exist

- [ ] **Step 2: Implement conn.go (Request/Response types, ReadRequest, WriteResponse)**

Parse RTSP request line: split first line by space into Method, URL, "RTSP/1.0". Read headers until blank line. If Content-Length header present, read body. Use `http.Header` for case-insensitive header access.

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/conn.go module/rtsp/conn_test.go
git commit -m "feat: add RTSP request/response parser"
```

### Task 17: RTSP session state machine

**Files:**
- Create: `module/rtsp/session.go`
- Create: `module/rtsp/session_test.go`

- [ ] **Step 1: Write failing tests for state transitions**

```go
// module/rtsp/session_test.go
package rtsp

import "testing"

func TestSessionStateTransitions(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	if s.State != StateInit {
		t.Fatalf("initial state = %d", s.State)
	}

	// DESCRIBE → Described
	if err := s.Transition(StateDescribed); err != nil {
		t.Fatalf("Transition to Described: %v", err)
	}

	// SETUP → Ready
	if err := s.Transition(StateReady); err != nil {
		t.Fatalf("Transition to Ready: %v", err)
	}

	// PLAY → Playing
	if err := s.Transition(StatePlaying); err != nil {
		t.Fatalf("Transition to Playing: %v", err)
	}

	// TEARDOWN → Closed
	if err := s.Transition(StateClosed); err != nil {
		t.Fatalf("Transition to Closed: %v", err)
	}
}

func TestSessionInvalidTransition(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	// Cannot go directly to Playing from Init
	err := s.Transition(StatePlaying)
	if err == nil {
		t.Fatal("expected error for invalid transition")
	}
}

func TestSessionTimeout(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	s.Timeout = 10 * time.Millisecond
	s.Touch()
	time.Sleep(20 * time.Millisecond)
	if !s.IsExpired() {
		t.Error("session should be expired")
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run TestSession -v`
Expected: FAIL

- [ ] **Step 2: Implement session.go**

States: Init, Described, Announced, Ready, Playing, Recording, Closed. Transition validates allowed state changes. Session has timeout (default 60s), Touch() resets timer, IsExpired() checks.

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run TestSession -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/session.go module/rtsp/session_test.go
git commit -m "feat: add RTSP session state machine with timeout"
```

### Task 18: TCP interleaved framing

**Files:**
- Create: `module/rtsp/interleaved.go`
- Create: `module/rtsp/transport.go`
- Create: `module/rtsp/transport_test.go`

- [ ] **Step 1: Write failing tests**

```go
// module/rtsp/transport_test.go
package rtsp

import (
	"bytes"
	"testing"
)

func TestWriteReadInterleaved(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{0x80, 0x60, 0x00, 0x01} // minimal RTP header
	err := WriteInterleaved(&buf, 0, payload)
	if err != nil {
		t.Fatalf("WriteInterleaved: %v", err)
	}
	// Read back
	channel, data, err := ReadInterleaved(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadInterleaved: %v", err)
	}
	if channel != 0 {
		t.Errorf("channel = %d", channel)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("data mismatch")
	}
}

func TestPortManagerAllocateRelease(t *testing.T) {
	pm := NewPortManager(10000, 10010)
	rtp1, rtcp1, err := pm.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if rtp1%2 != 0 {
		t.Errorf("RTP port not even: %d", rtp1)
	}
	if rtcp1 != rtp1+1 {
		t.Errorf("RTCP port = %d, want %d", rtcp1, rtp1+1)
	}

	// Allocate again, should get different ports
	rtp2, _, err := pm.Allocate()
	if err != nil {
		t.Fatalf("Allocate 2: %v", err)
	}
	if rtp2 == rtp1 {
		t.Error("got same port twice")
	}

	// Release and re-allocate
	pm.Release(rtp1)
	rtp3, _, err := pm.Allocate()
	if err != nil {
		t.Fatalf("Allocate 3: %v", err)
	}
	if rtp3 != rtp1 {
		t.Errorf("expected reuse of released port %d, got %d", rtp1, rtp3)
	}
}

func TestPortManagerExhausted(t *testing.T) {
	pm := NewPortManager(10000, 10004) // only 2 pairs available
	_, _, _ = pm.Allocate()
	_, _, _ = pm.Allocate()
	_, _, err := pm.Allocate()
	if err == nil {
		t.Fatal("expected port exhaustion error")
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run "TestWriteRead|TestPortManager" -v`
Expected: FAIL

- [ ] **Step 2: Implement interleaved.go and transport.go**

interleaved.go: WriteInterleaved writes `$` + channel(1) + length(2 big-endian) + payload. ReadInterleaved reads and returns channel + data.

transport.go: PortManager with Allocate (scan from minPort upward in steps of 2, find first even port where both port and port+1 are free) and Release. UDPTransport and TCPTransport types.

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/interleaved.go module/rtsp/transport.go module/rtsp/transport_test.go
git commit -m "feat: add RTSP TCP interleaved framing and UDP port manager"
```

---

## Chunk 5: RTSP Module — Handler, Publisher, Subscriber

### Task 19a: RTSP handler — OPTIONS, DESCRIBE, GET_PARAMETER

**Files:**
- Create: `module/rtsp/handler.go`
- Create: `module/rtsp/handler_test.go`

- [ ] **Step 1: Write failing tests for OPTIONS, DESCRIBE, GET_PARAMETER**

```go
// module/rtsp/handler_test.go
package rtsp

import (
	"net/http"
	"strings"
	"testing"
)

func TestHandleOptions(t *testing.T) {
	h := NewHandler(nil)
	req := &Request{Method: "OPTIONS", URL: "*", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "1")
	resp := h.HandleOptions(req)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	public := resp.Headers.Get("Public")
	for _, method := range []string{"DESCRIBE", "SETUP", "PLAY", "PAUSE", "ANNOUNCE", "RECORD", "TEARDOWN", "GET_PARAMETER"} {
		if !strings.Contains(public, method) {
			t.Errorf("Public missing %s: %q", method, public)
		}
	}
}

func TestHandleGetParameter(t *testing.T) {
	h := NewHandler(nil)
	req := &Request{Method: "GET_PARAMETER", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "2")
	resp := h.HandleGetParameter(req)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run "TestHandleOptions|TestHandleGetParameter" -v`
Expected: FAIL

- [ ] **Step 2: Implement handler.go with OPTIONS, DESCRIBE, GET_PARAMETER**

Handler struct holds reference to core.Server (for StreamHub, EventBus access). DESCRIBE looks up stream in StreamHub, builds SDP from MediaInfo. GET_PARAMETER returns 200 OK (keepalive). Define Handler type, NewHandler, response builder helpers.

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/handler.go module/rtsp/handler_test.go
git commit -m "feat: add RTSP handler for OPTIONS, DESCRIBE, GET_PARAMETER"
```

### Task 19b: RTSP handler — SETUP + Transport header parsing

**Files:**
- Modify: `module/rtsp/handler.go`
- Modify: `module/rtsp/handler_test.go`

- [ ] **Step 1: Write failing tests for SETUP**

```go
func TestHandleSetupTCPInterleaved(t *testing.T) {
	h := NewHandler(nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	req := &Request{Method: "SETUP", URL: "rtsp://host/live/test/trackID=0", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "3")
	req.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=0-1")
	resp := h.HandleSetup(req, session)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	transport := resp.Headers.Get("Transport")
	if !strings.Contains(transport, "interleaved=0-1") {
		t.Errorf("Transport = %q", transport)
	}
}

func TestHandleSetupUDP(t *testing.T) {
	h := NewHandler(nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	req := &Request{Method: "SETUP", URL: "rtsp://host/live/test/trackID=0", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "4")
	req.Headers.Set("Transport", "RTP/AVP;unicast;client_port=5000-5001")
	resp := h.HandleSetup(req, session)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	transport := resp.Headers.Get("Transport")
	if !strings.Contains(transport, "server_port=") {
		t.Errorf("Transport missing server_port: %q", transport)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run TestHandleSetup -v`
Expected: FAIL

- [ ] **Step 2: Implement HandleSetup with Transport header parsing**

Parse Transport header into TransportMode (TCP/UDP). For TCP: extract interleaved channel pair. For UDP: extract client ports, allocate server ports via PortManager. Add track to session. Return negotiated Transport in response + Session header with timeout.

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/handler.go module/rtsp/handler_test.go
git commit -m "feat: add RTSP SETUP handler with Transport negotiation"
```

### Task 19c: RTSP handler — ANNOUNCE + RECORD (publish path)

**Files:**
- Modify: `module/rtsp/handler.go`
- Modify: `module/rtsp/handler_test.go`

- [ ] **Step 1: Write failing tests for ANNOUNCE and RECORD**

Test ANNOUNCE: provide SDP body, verify session transitions to Announced, MediaInfo extracted.
Test RECORD: verify session transitions to Recording, verify EventPublish emitted.
Test ANNOUNCE with auth failure: mock EventBus returns error, verify 401 response.

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run "TestHandleAnnounce|TestHandleRecord" -v`
Expected: FAIL

- [ ] **Step 2: Implement HandleAnnounce and HandleRecord**

ANNOUNCE: parse SDP body, extract codecs into MediaInfo, emit EventPublish on EventBus, create Stream in StreamHub. RECORD: verify session state, start RTP receiving.

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/handler.go module/rtsp/handler_test.go
git commit -m "feat: add RTSP ANNOUNCE/RECORD handler (publish path)"
```

### Task 19d: RTSP handler — PLAY + PAUSE + TEARDOWN (subscribe path)

**Files:**
- Modify: `module/rtsp/handler.go`
- Modify: `module/rtsp/handler_test.go`

- [ ] **Step 1: Write failing tests**

Test PLAY: verify session transitions to Playing, EventSubscribe emitted, RTP-Info header in response.
Test PAUSE: verify 200 OK response (live stream — delivery continues).
Test TEARDOWN: verify session transitions to Closed, resources released.
Test PLAY with codec incompatibility: verify 415 response.

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run "TestHandlePlay|TestHandlePause|TestHandleTeardown" -v`
Expected: FAIL

- [ ] **Step 2: Implement HandlePlay, HandlePause, HandleTeardown**

PLAY: verify Ready state, emit EventSubscribe (triggers auth + codec check), create subscriber, start WriteLoop, return RTP-Info header. PAUSE: return 200 OK. TEARDOWN: clean up subscriber/publisher, release transport resources.

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/handler.go module/rtsp/handler_test.go
git commit -m "feat: add RTSP PLAY/PAUSE/TEARDOWN handler (subscribe path)"
```

### Task 20: RTSP Publisher

**Files:**
- Create: `module/rtsp/publisher.go`
- Create: `module/rtsp/publisher_test.go`

- [ ] **Step 1: Write failing test**

Test that RTSPPublisher implements `core.Publisher`. Test that feeding RTP packets through the publisher results in AVFrames being written to a mock stream. Use H.264 test data: create FU-A packets → feed to publisher → verify WriteFrame called with correct AVFrame.

- [ ] **Step 2: Implement publisher.go**

```go
type RTSPPublisher struct {
	id           string
	mediaInfo    *avframe.MediaInfo
	stream       *core.Stream
	depacketizers map[uint8]rtp.Depacketizer // PT → depacketizer
}

func (p *RTSPPublisher) ID() string                    { return p.id }
func (p *RTSPPublisher) MediaInfo() *avframe.MediaInfo  { return p.mediaInfo }
func (p *RTSPPublisher) Close() error                   { ... }

// FeedRTP processes an incoming RTP packet, depacketizes, and writes to stream.
func (p *RTSPPublisher) FeedRTP(pkt *pionrtp.Packet) error {
	depkt := p.depacketizers[pkt.PayloadType]
	frame, err := depkt.Depacketize(pkt)
	if err != nil || frame == nil {
		return err
	}
	p.stream.WriteFrame(frame)
	return nil
}
```

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/publisher.go module/rtsp/publisher_test.go
git commit -m "feat: add RTSP publisher with RTP depacketization"
```

### Task 21: RTSP Subscriber

**Files:**
- Create: `module/rtsp/subscriber.go`
- Create: `module/rtsp/subscriber_test.go`

- [ ] **Step 1: Write failing test**

Test WriteLoop: create a Stream with known AVFrames in GOP cache, create subscriber with mock transport, start WriteLoop, verify RTP packets are sent in correct order with correct timestamps.

- [ ] **Step 2: Implement subscriber.go**

```go
type RTSPSubscriber struct {
	id          string
	stream      *core.Stream
	session     *RTSPSession
	packetizers map[avframe.MediaType]struct {
		packetizer rtp.Packetizer
		session    *rtp.Session
	}
	transport   TransportMode
	tcpWriter   *TCPTransport
	udpWriters  map[avframe.MediaType]*UDPTransport
	done        chan struct{}
}

func (s *RTSPSubscriber) WriteLoop() {
	// 1. Create RTP sessions + packetizers from MediaInfo
	// 2. Send sequence headers
	// 3. Send GOP cache
	// 4. Read from RingBuffer, packetize, send via transport
}

func (s *RTSPSubscriber) sendPackets(mediaType avframe.MediaType, pkts []*pionrtp.Packet) error {
	// TCP: WriteInterleaved per packet
	// UDP: rtpConn.WriteTo per packet
}
```

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/subscriber.go module/rtsp/subscriber_test.go
git commit -m "feat: add RTSP subscriber with RTP packetization and GOP cache"
```

### Task 22: RTCP periodic sending (publisher RR + subscriber SR)

**Files:**
- Modify: `module/rtsp/publisher.go`
- Modify: `module/rtsp/subscriber.go`
- Modify: `module/rtsp/publisher_test.go`
- Modify: `module/rtsp/subscriber_test.go`

- [ ] **Step 1: Write failing test for publisher RTCP RR sending**

```go
func TestPublisherSendsRR(t *testing.T) {
	// Create publisher with mock transport
	// Feed some RTP packets
	// Wait >5s or trigger RTCP tick
	// Verify RR packet was sent via transport with correct SSRC and reception report
}
```

Test subscriber SR sending similarly: after sending RTP packets, verify SR is sent periodically with correct NTP/RTP timestamps and packet/octet counts.

- [ ] **Step 2: Implement RTCP periodic sending**

In publisher: start a goroutine that sends RR every 5 seconds via the transport. Track received packet count, highest seq, etc. for the reception report. Parse incoming SR from the publishing client for NTP/RTP timing sync.

In subscriber: start a goroutine that sends SR every 5 seconds. Track sent packet count and octet count. Include NTP timestamp (current wall clock) and corresponding RTP timestamp.

Both goroutines stop when the session closes (select on done channel).

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/publisher.go module/rtsp/subscriber.go module/rtsp/publisher_test.go module/rtsp/subscriber_test.go
git commit -m "feat: add RTCP periodic SR/RR sending for RTSP publisher and subscriber"
```

---

## Chunk 6: RTSP Module — Server and Integration

### Task 23: RTSP server (Module implementation)

**Files:**
- Create: `module/rtsp/server.go`
- Create: `module/rtsp/server_test.go`
- Modify: `cmd/liveforge/main.go`

- [ ] **Step 1: Write failing test for Module interface compliance**

```go
// module/rtsp/server_test.go
package rtsp

import (
	"testing"

	"github.com/im-pingo/liveforge/core"
)

func TestModuleInterface(t *testing.T) {
	m := NewModule()
	// Verify it satisfies core.Module
	var _ core.Module = m

	if m.Name() != "rtsp" {
		t.Errorf("Name = %q, want %q", m.Name(), "rtsp")
	}
	if hooks := m.Hooks(); hooks != nil {
		t.Errorf("Hooks should be nil, got %v", hooks)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./module/rtsp/ -run TestModule -v`
Expected: FAIL — NewModule not defined

- [ ] **Step 2: Implement server.go**

```go
type Module struct{}

func NewModule() *Module { return &Module{} }

func (m *Module) Name() string { return "rtsp" }

func (m *Module) Init(s *core.Server) error {
	// Parse config: s.Config().RTSP
	// Validate RTPPortRange has 2 elements
	// Create PortManager
	// Start TCP listener on config listen address
	// Start session timeout checker goroutine (every 10s)
	// Accept connections → spawn handleConn goroutine per connection
}

func (m *Module) Hooks() []core.HookRegistration { return nil }
func (m *Module) Close() error { ... }

func (m *Module) handleConn(conn net.Conn) {
	// Read RTSP requests in a loop
	// Check for interleaved data ($ prefix) when in Playing/Recording state
	// Dispatch to handler based on method
}
```

- [ ] **Step 2: Register in main.go**

```go
// Add import: "github.com/im-pingo/liveforge/module/rtsp"
// After HTTP module registration:
if cfg.RTSP.Enabled {
    s.RegisterModule(rtsp.NewModule())
}
```

- [ ] **Step 3: Commit**

```bash
git add module/rtsp/server.go cmd/liveforge/main.go
git commit -m "feat: add RTSP server module with TCP listener and session management"
```

### Task 24: Integration test — RTMP push → RTSP pull

**Files:**
- Create: `test/integration/rtsp_subscribe_test.go`

- [ ] **Step 1: Write integration test**

Create a test that:
1. Starts Server with RTMP + RTSP modules enabled
2. Simulates RTMP publish (reuse existing integration test pattern from `test/integration/rtmp_relay_test.go`)
3. Simulates RTSP client: DESCRIBE → SETUP (TCP interleaved) → PLAY
4. Verify that RTP packets arrive with correct codec data
5. Depacketize received RTP → verify matches original AVFrames

- [ ] **Step 2: Run test and iterate until passing**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./test/integration/ -run TestRTSPSubscribe -v -timeout 30s`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add test/integration/rtsp_subscribe_test.go
git commit -m "test: add RTMP push → RTSP pull integration test"
```

### Task 25: Integration test — RTSP push → RTMP pull

**Files:**
- Create: `test/integration/rtsp_publish_test.go`

- [ ] **Step 1: Write integration test**

Create a test that:
1. Starts Server with RTMP + RTSP modules enabled
2. Simulates RTSP publish: ANNOUNCE (with SDP) → SETUP → RECORD → send RTP packets
3. Simulates RTMP subscribe on the same stream key
4. Verify that FLV data arrives with correct frames

- [ ] **Step 2: Run test and iterate until passing**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test ./test/integration/ -run TestRTSPPublish -v -timeout 30s`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add test/integration/rtsp_publish_test.go
git commit -m "test: add RTSP push → RTMP pull integration test"
```

### Task 26: Run all tests, final verification

- [ ] **Step 1: Run full test suite**

```bash
cd /Users/pingo-macmini/Documents/liveforge && go test ./... -v -timeout 120s
```

Expected: ALL PASS

- [ ] **Step 2: Run go vet and verify no issues**

```bash
cd /Users/pingo-macmini/Documents/liveforge && go vet ./...
```

Expected: no output (clean)

- [ ] **Step 3: Final commit if any cleanup needed**

```bash
git add -A && git commit -m "chore: Phase 3 RTP/SDP/RTSP cleanup"
```
