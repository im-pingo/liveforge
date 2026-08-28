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

func TestBuildInitSegmentH265UsesOutOfBandParameterSets(t *testing.T) {
	data := BuildInitSegment(avframe.CodecH265, avframe.CodecAAC, nil, nil, 1920, 1080, 44100, 2)
	if len(data) == 0 {
		t.Fatal("empty init segment")
	}
	// Verify ftyp present
	if string(data[4:8]) != "ftyp" {
		t.Errorf("expected ftyp, got %s", string(data[4:8]))
	}
	if !bytes.Contains(data, []byte("hvc1")) || bytes.Contains(data, []byte("hev1")) {
		t.Fatal("H.265 init segment must use hvc1 when parameter sets are carried by hvcC")
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

func TestBuildMediaSegmentBFrameCTS(t *testing.T) {
	// Simulate a GOP with B-frames: I, P, B, B (in decode/DTS order)
	// DTS increases monotonically; PTS reflects display order.
	frames := []*avframe.AVFrame{
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeKeyframe,
			DTS:       0,
			PTS:       66, // CTS = 66
			Payload:   []byte{0x65, 0x88, 0x00, 0x01},
		},
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       33,
			PTS:       132, // CTS = 99
			Payload:   []byte{0x41, 0x9A, 0x00, 0x02},
		},
		{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       66,
			PTS:       33, // CTS = -33
			Payload:   []byte{0x41, 0x9A, 0x00, 0x03},
		},
	}

	data := BuildMediaSegment(frames, 1, 0)
	if len(data) == 0 {
		t.Fatal("empty media segment")
	}

	// Find the trun box inside moof and verify it contains composition time offsets.
	// The trun flags should include 0x000800 (sample-composition-time-offsets-present).
	moofSize := int(binary.BigEndian.Uint32(data[0:4]))
	moofData := data[8:moofSize] // skip box header

	trunFound := false
	offset := 0
	for offset+8 <= len(moofData) {
		boxSize := int(binary.BigEndian.Uint32(moofData[offset : offset+4]))
		boxType := string(moofData[offset+4 : offset+8])
		if boxSize < 8 || offset+boxSize > len(moofData) {
			break
		}
		if boxType == "traf" {
			// Search inside traf for trun
			inner := offset + 8
			end := offset + boxSize
			for inner+8 <= end {
				innerSize := int(binary.BigEndian.Uint32(moofData[inner : inner+4]))
				innerType := string(moofData[inner+4 : inner+8])
				if innerSize < 8 || inner+innerSize > end {
					break
				}
				if innerType == "trun" {
					trunFound = true
					// version(1) + flags(3) at offset+8
					version := moofData[inner+8]
					flags := uint32(moofData[inner+9])<<16 | uint32(moofData[inner+10])<<8 | uint32(moofData[inner+11])
					if version != 1 {
						t.Errorf("trun version = %d, want 1 (signed CTS)", version)
					}
					if flags&0x000800 == 0 {
						t.Error("trun flags missing sample-composition-time-offsets-present (0x000800)")
					}
					break
				}
				inner += innerSize
			}
			break // only check first traf (video)
		}
		offset += boxSize
	}
	if !trunFound {
		t.Error("trun box not found in moof")
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

func TestMuxerDerivesAACConfigAndTimescaleWhenInitArgsOmitted(t *testing.T) {
	// AAC-LC, 48000 Hz, stereo.
	audioHeader := avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		avframe.CodecAAC,
		avframe.FrameTypeSequenceHeader,
		0,
		0,
		[]byte{0x11, 0x90},
	)
	muxer := NewMuxer(0, avframe.CodecAAC)
	initSegment := muxer.Init(nil, audioHeader, 0, 0, 0, 0)
	firstSegment := muxer.WriteSegment([]*avframe.AVFrame{avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		avframe.CodecAAC,
		avframe.FrameTypeInterframe,
		0,
		0,
		[]byte{1, 2, 3},
	)})
	secondSegment := muxer.WriteSegment([]*avframe.AVFrame{avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		avframe.CodecAAC,
		avframe.FrameTypeInterframe,
		20,
		20,
		[]byte{4, 5, 6},
	)})

	demuxer, err := NewDemuxer(initSegment)
	if err != nil {
		t.Fatal(err)
	}
	firstFrames, err := demuxer.Parse(firstSegment)
	if err != nil {
		t.Fatal(err)
	}
	secondFrames, err := demuxer.Parse(secondSegment)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstFrames) != 1 || !firstFrames[0].MediaType.IsAudio() {
		t.Fatalf("first fragment frames = %+v, want one audio frame", firstFrames)
	}
	if len(secondFrames) != 1 || !secondFrames[0].MediaType.IsAudio() {
		t.Fatalf("second fragment frames = %+v, want one audio frame", secondFrames)
	}
	if got := secondFrames[0].DTS - firstFrames[0].DTS; got != 20 {
		t.Fatalf("demuxed AAC DTS interval = %d ms, want 20 ms", got)
	}
	if got := demuxer.timescale[audioTrackID]; got != 48000 {
		t.Fatalf("AAC media timescale = %d, want 48000", got)
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

func TestBuildMediaSegmentH265StripsParameterSetsForHVC1(t *testing.T) {
	makeNAL := func(nalType byte, size int) []byte {
		nal := make([]byte, size)
		nal[0] = nalType << 1
		nal[1] = 1
		buf := make([]byte, 4+size)
		binary.BigEndian.PutUint32(buf, uint32(size))
		copy(buf[4:], nal)
		return buf
	}

	vps := makeNAL(32, 8)
	sps := makeNAL(33, 10)
	pps := makeNAL(34, 6)
	sei := makeNAL(39, 7)
	idr := makeNAL(19, 50)
	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH265,
		avframe.FrameTypeKeyframe,
		0,
		0,
		concat(vps, sps, pps, sei, idr),
	)

	segment := BuildMediaSegment([]*avframe.AVFrame{frame}, 1, 0)
	moofSize := int(binary.BigEndian.Uint32(segment[0:4]))
	mdatSize := int(binary.BigEndian.Uint32(segment[moofSize : moofSize+4]))
	if got, want := mdatSize-8, len(sei)+len(idr); got != want {
		t.Fatalf("H.265 mdat payload = %d bytes, want %d bytes without VPS/SPS/PPS", got, want)
	}
}

func TestMuxerWriteSegmentUntilUsesVideoBoundaryForFinalDuration(t *testing.T) {
	muxer := NewMuxer(avframe.CodecH265, 0)
	muxer.Init(nil, nil, 640, 480, 0, 0)
	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH265,
		avframe.FrameTypeKeyframe,
		0,
		0,
		[]byte{0, 0, 0, 2, 0x26, 0x01},
	)

	segment := muxer.WriteSegmentUntil([]*avframe.AVFrame{frame}, 30)
	_, duration := readSingleSampleTiming(t, segment)
	if duration != 2700 {
		t.Fatalf("final video sample duration = %d, want 2700 ticks at the segment boundary", duration)
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

func TestMuxerOpusSingletonFragmentsHaveContinuousTimeline(t *testing.T) {
	muxer := NewMuxer(0, avframe.CodecOpus)
	muxer.Init(nil, nil, 0, 0, 48000, 2)

	first := muxer.WriteSegment([]*avframe.AVFrame{avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
		0, 0, []byte{0xF8, 0xFF, 0xFE},
	)})
	second := muxer.WriteSegment([]*avframe.AVFrame{avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
		20, 20, []byte{0xF8, 0xFF, 0xFE},
	)})

	firstBase, firstDuration := readSingleSampleTiming(t, first)
	secondBase, secondDuration := readSingleSampleTiming(t, second)
	if firstBase != 0 || firstDuration != 960 {
		t.Fatalf("first Opus fragment timeline = base:%d duration:%d, want 0/960", firstBase, firstDuration)
	}
	if secondBase != 960 || secondDuration != 960 {
		t.Fatalf("second Opus fragment timeline = base:%d duration:%d, want 960/960", secondBase, secondDuration)
	}
	if firstBase+uint64(firstDuration) != secondBase {
		t.Fatalf("Opus fragments overlap or leave a gap: first ends at %d, second starts at %d", firstBase+uint64(firstDuration), secondBase)
	}
}

func TestBuildMediaSegmentUsesOpusTOCDuration(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    uint32
	}{
		{name: "CELT 2.5ms", payload: []byte{0x80}, want: 120},
		{name: "CELT 20ms", payload: []byte{0xF8}, want: 960},
		{name: "two CELT frames", payload: []byte{0xF9}, want: 1920},
		{name: "three CELT frames", payload: []byte{0xFB, 0x03}, want: 2880},
		{name: "hybrid 10ms", payload: []byte{0x60}, want: 480},
		{name: "SILK 60ms", payload: []byte{0x18}, want: 2880},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segment := BuildMediaSegment([]*avframe.AVFrame{avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
				0, 0, tt.payload,
			)}, 1, 48000)
			_, duration := readSingleSampleTiming(t, segment)
			if duration != tt.want {
				t.Fatalf("Opus sample duration = %d, want %d", duration, tt.want)
			}
		})
	}
}

func readSingleSampleTiming(t *testing.T, segment []byte) (uint64, uint32) {
	t.Helper()
	if len(segment) < 8 || string(segment[4:8]) != "moof" {
		t.Fatal("media segment is missing moof")
	}
	moofEnd := int(binary.BigEndian.Uint32(segment[:4]))
	if moofEnd > len(segment) {
		t.Fatal("moof is truncated")
	}
	var base uint64
	var duration uint32
	foundTFDT := false
	foundTRUN := false
	for offset := 8; offset+8 <= moofEnd; {
		size := int(binary.BigEndian.Uint32(segment[offset : offset+4]))
		if size < 8 || offset+size > moofEnd {
			t.Fatal("invalid child box in moof")
		}
		if string(segment[offset+4:offset+8]) == "traf" {
			for inner := offset + 8; inner+8 <= offset+size; {
				innerSize := int(binary.BigEndian.Uint32(segment[inner : inner+4]))
				if innerSize < 8 || inner+innerSize > offset+size {
					t.Fatal("invalid child box in traf")
				}
				switch string(segment[inner+4 : inner+8]) {
				case "tfdt":
					if innerSize < 20 || segment[inner+8] != 1 {
						t.Fatal("tfdt is not a complete version 1 box")
					}
					base = binary.BigEndian.Uint64(segment[inner+12 : inner+20])
					foundTFDT = true
				case "trun":
					if innerSize < 24 || binary.BigEndian.Uint32(segment[inner+12:inner+16]) != 1 {
						t.Fatal("trun does not contain exactly one sample")
					}
					duration = binary.BigEndian.Uint32(segment[inner+20 : inner+24])
					foundTRUN = true
				}
				inner += innerSize
			}
		}
		offset += size
	}
	if !foundTFDT || !foundTRUN {
		t.Fatal("media segment is missing tfdt or trun")
	}
	return base, duration
}
