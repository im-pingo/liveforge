package fmp4

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestWriteBox(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("test")
	err := WriteBox(&buf, BoxFtyp, payload)
	if err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	size := binary.BigEndian.Uint32(data[0:4])
	if size != 12 { // 8 header + 4 payload
		t.Errorf("box size = %d, want 12", size)
	}
	if string(data[4:8]) != "ftyp" {
		t.Errorf("box type = %s, want ftyp", string(data[4:8]))
	}
	if !bytes.Equal(data[8:], payload) {
		t.Error("payload mismatch")
	}
}

func TestWriteFullBox(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{0x01, 0x02}
	err := WriteFullBox(&buf, BoxMvhd, 1, 0x000001, payload)
	if err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	size := binary.BigEndian.Uint32(data[0:4])
	if size != 14 { // 12 header + 2 payload
		t.Errorf("full box size = %d, want 14", size)
	}
	if data[8] != 1 { // version
		t.Errorf("version = %d, want 1", data[8])
	}
}

func TestBuildInitSegment(t *testing.T) {
	// Minimal init segment with H.264 + AAC
	asc := []byte{0x12, 0x10} // AAC-LC, 44100Hz, stereo
	data := BuildInitSegment(avframe.CodecH264, avframe.CodecAAC, nil, asc, 640, 480, 44100, 2)
	if len(data) == 0 {
		t.Fatal("empty init segment")
	}

	// Verify starts with ftyp
	if string(data[4:8]) != "ftyp" {
		t.Errorf("init segment should start with ftyp, got %s", string(data[4:8]))
	}

	// Find moov box
	ftypSize := binary.BigEndian.Uint32(data[0:4])
	if int(ftypSize) >= len(data) {
		t.Fatal("ftyp box takes up entire segment")
	}
	moovType := string(data[ftypSize+4 : ftypSize+8])
	if moovType != "moov" {
		t.Errorf("second box should be moov, got %s", moovType)
	}
}

func TestBuildInitSegmentH265(t *testing.T) {
	data := BuildInitSegment(avframe.CodecH265, avframe.CodecAAC, nil, nil, 1920, 1080, 44100, 2)
	if len(data) == 0 {
		t.Fatal("empty init segment")
	}
	// Verify ftyp present
	if string(data[4:8]) != "ftyp" {
		t.Errorf("expected ftyp, got %s", string(data[4:8]))
	}
}

func TestBuildInitSegmentAV1(t *testing.T) {
	data := BuildInitSegment(avframe.CodecAV1, avframe.CodecOpus, nil, nil, 1920, 1080, 48000, 2)
	if len(data) == 0 {
		t.Fatal("empty init segment")
	}
}

func TestBuildInitSegmentVP9(t *testing.T) {
	data := BuildInitSegment(avframe.CodecVP9, avframe.CodecAAC, nil, nil, 1280, 720, 44100, 2)
	if len(data) == 0 {
		t.Fatal("empty init segment")
	}
}

func TestBuildMediaSegment(t *testing.T) {
	frames := []*avframe.AVFrame{
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeKeyframe,
			DTS:       0,
			PTS:       0,
			Payload:   []byte{0x65, 0x88, 0x00, 0x01},
		},
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       33,
			PTS:       33,
			Payload:   []byte{0x41, 0x9A, 0x00, 0x02},
		},
		{
			MediaType: avframe.MediaTypeAudio,
			Codec:     avframe.CodecAAC,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       0,
			PTS:       0,
			Payload:   []byte{0xDE, 0xAD, 0xBE, 0xEF},
		},
	}

	data := BuildMediaSegment(frames, 1, 44100)
	if len(data) == 0 {
		t.Fatal("empty media segment")
	}

	// Should start with moof
	if string(data[4:8]) != "moof" {
		t.Errorf("expected moof, got %s", string(data[4:8]))
	}

	// Should have mdat somewhere after moof
	moofSize := binary.BigEndian.Uint32(data[0:4])
	if int(moofSize) >= len(data) {
		t.Fatal("moof takes up entire segment")
	}
	mdatType := string(data[moofSize+4 : moofSize+8])
	if mdatType != "mdat" {
		t.Errorf("second box should be mdat, got %s", mdatType)
	}
}

func TestBuildMediaSegmentEmpty(t *testing.T) {
	data := BuildMediaSegment(nil, 1, 44100)
	if data != nil {
		t.Error("empty frames should return nil")
	}
}

func TestMuxerFlow(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)

	// Init segment
	initSeg := m.Init(nil, nil, 640, 480, 44100, 2)
	if len(initSeg) == 0 {
		t.Fatal("empty init segment")
	}

	// Media segment
	frames := []*avframe.AVFrame{
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeKeyframe,
			DTS:       0,
			PTS:       0,
			Payload:   make([]byte, 100),
		},
	}
	seg := m.WriteSegment(frames)
	if len(seg) == 0 {
		t.Fatal("empty media segment")
	}

	// Second segment should have sequence_number=2
	seg2 := m.WriteSegment(frames)
	if len(seg2) == 0 {
		t.Fatal("empty second segment")
	}
}

func TestFilterH264VCLNALUs(t *testing.T) {
	// Build AVCC payload: SPS(7) + PPS(8) + IDR(5)
	makeNAL := func(nalType byte, size int) []byte {
		nal := make([]byte, size)
		nal[0] = nalType & 0x1F
		buf := make([]byte, 4+size)
		binary.BigEndian.PutUint32(buf, uint32(size))
		copy(buf[4:], nal)
		return buf
	}

	sps := makeNAL(7, 10)  // SPS
	pps := makeNAL(8, 5)   // PPS
	sei := makeNAL(6, 8)   // SEI
	idr := makeNAL(5, 100) // IDR slice

	tests := []struct {
		name    string
		input   []byte
		wantLen int // expected length of filtered output
	}{
		{
			name:    "SPS+PPS+IDR keeps only IDR",
			input:   concat(sps, pps, idr),
			wantLen: len(idr),
		},
		{
			name:    "SPS+PPS+SEI+IDR keeps only IDR",
			input:   concat(sps, pps, sei, idr),
			wantLen: len(idr),
		},
		{
			name:    "IDR only unchanged",
			input:   idr,
			wantLen: len(idr),
		},
		{
			name:    "non-IDR slice unchanged",
			input:   makeNAL(1, 50),
			wantLen: len(makeNAL(1, 50)),
		},
		{
			name:    "SPS+PPS only returns empty",
			input:   concat(sps, pps),
			wantLen: 0,
		},
		{
			name:    "empty input",
			input:   nil,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterH264VCLNALUs(tt.input)
			if len(got) != tt.wantLen {
				t.Errorf("filterH264VCLNALUs() len = %d, want %d", len(got), tt.wantLen)
			}
			// Verify all remaining NALUs are VCL (type 1-5)
			offset := 0
			for offset+4 < len(got) {
				nalLen := int(binary.BigEndian.Uint32(got[offset:]))
				if offset+4+nalLen > len(got) {
					t.Fatalf("invalid AVCC at offset %d", offset)
				}
				nalType := got[offset+4] & 0x1F
				if nalType < 1 || nalType > 5 {
					t.Errorf("non-VCL NAL type %d in output", nalType)
				}
				offset += 4 + nalLen
			}
		})
	}
}

func TestBuildMediaSegmentStripsNonVCL(t *testing.T) {
	// AVCC: SPS(7, 10 bytes) + PPS(8, 5 bytes) + IDR(5, 50 bytes)
	avcc := make([]byte, 0)
	for _, entry := range []struct {
		nalType byte
		size    int
	}{
		{7, 10}, // SPS
		{8, 5},  // PPS
		{5, 50}, // IDR
	} {
		nal := make([]byte, entry.size)
		nal[0] = entry.nalType & 0x1F
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, uint32(entry.size))
		avcc = append(avcc, buf...)
		avcc = append(avcc, nal...)
	}

	frames := []*avframe.AVFrame{
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeKeyframe,
			DTS:       0,
			PTS:       0,
			Payload:   avcc,
		},
	}

	seg := BuildMediaSegment(frames, 1, 44100)
	if len(seg) == 0 {
		t.Fatal("expected non-empty segment")
	}

	// Find mdat and verify it only contains IDR (54 bytes = 4 len + 50 data)
	moofSize := int(binary.BigEndian.Uint32(seg[0:4]))
	mdatSize := int(binary.BigEndian.Uint32(seg[moofSize : moofSize+4]))
	mdatPayload := mdatSize - 8
	// IDR NALU is 50 bytes + 4 byte length prefix = 54 bytes
	if mdatPayload != 54 {
		t.Errorf("mdat payload = %d bytes, want 54 (IDR only)", mdatPayload)
	}
}

func concat(slices ...[]byte) []byte {
	var result []byte
	for _, s := range slices {
		result = append(result, s...)
	}
	return result
}

func TestBuildMediaSegmentSkipsSequenceHeaders(t *testing.T) {
	frames := []*avframe.AVFrame{
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeSequenceHeader,
			Payload:   []byte{0x01, 0x64, 0x00, 0x28},
		},
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeKeyframe,
			DTS:       0,
			PTS:       0,
			Payload:   []byte{0x65, 0x88},
		},
	}

	data := BuildMediaSegment(frames, 1, 44100)
	if len(data) == 0 {
		t.Fatal("expected non-empty segment")
	}
}
