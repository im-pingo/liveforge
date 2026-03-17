package ts

import (
	"encoding/binary"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestCRC32MPEG2(t *testing.T) {
	// Known test vector: "123456789" → 0x0376E6E7
	data := []byte("123456789")
	got := CRC32MPEG2(data)
	expected := uint32(0x0376E6E7)
	if got != expected {
		t.Errorf("CRC32MPEG2(%q) = 0x%08X, want 0x%08X", data, got, expected)
	}
}

func TestStreamType(t *testing.T) {
	tests := []struct {
		codec    avframe.CodecType
		expected byte
	}{
		{avframe.CodecH264, 0x1B},
		{avframe.CodecH265, 0x24},
		{avframe.CodecAAC, 0x0F},
		{avframe.CodecMP3, 0x03},
	}
	for _, tt := range tests {
		if got := StreamType(tt.codec); got != tt.expected {
			t.Errorf("StreamType(%v) = 0x%02X, want 0x%02X", tt.codec, got, tt.expected)
		}
	}
}

func TestBuildPAT(t *testing.T) {
	pat := BuildPAT(0)
	if len(pat) != PacketSize {
		t.Fatalf("PAT length = %d, want %d", len(pat), PacketSize)
	}
	if pat[0] != SyncByte {
		t.Errorf("PAT[0] = 0x%02X, want 0x%02X", pat[0], SyncByte)
	}
	// PID should be 0
	pid := uint16(pat[1]&0x1F)<<8 | uint16(pat[2])
	if pid != PIDPat {
		t.Errorf("PAT PID = 0x%04X, want 0x%04X", pid, PIDPat)
	}
	// payload_unit_start_indicator should be 1
	if pat[1]&0x40 == 0 {
		t.Error("PAT payload_unit_start_indicator should be 1")
	}
}

func TestBuildPMT(t *testing.T) {
	pmt := BuildPMT(0x1B, 0x0F, 0)
	if len(pmt) != PacketSize {
		t.Fatalf("PMT length = %d, want %d", len(pmt), PacketSize)
	}
	if pmt[0] != SyncByte {
		t.Errorf("PMT[0] = 0x%02X, want 0x%02X", pmt[0], SyncByte)
	}
	pid := uint16(pmt[1]&0x1F)<<8 | uint16(pmt[2])
	if pid != PIDPmt {
		t.Errorf("PMT PID = 0x%04X, want 0x%04X", pid, PIDPmt)
	}
}

func TestBuildPMTVideoOnly(t *testing.T) {
	pmt := BuildPMT(0x1B, 0, 0)
	if len(pmt) != PacketSize {
		t.Fatalf("PMT length = %d, want %d", len(pmt), PacketSize)
	}
}

func TestBuildPESHeader(t *testing.T) {
	// PTS only
	header := BuildPESHeader(0xC0, 1000, 1000, 100)
	if header[0] != 0x00 || header[1] != 0x00 || header[2] != 0x01 {
		t.Error("invalid PES start code")
	}
	if header[3] != 0xC0 {
		t.Errorf("stream_id = 0x%02X, want 0xC0", header[3])
	}
	// PTS_DTS_flags should be 10 (PTS only)
	if header[7]&0xC0 != 0x80 {
		t.Errorf("PTS_DTS_flags = 0x%02X, want 0x80", header[7]&0xC0)
	}
	// header_data_length should be 5
	if header[8] != 5 {
		t.Errorf("header_data_length = %d, want 5", header[8])
	}
}

func TestBuildPESHeaderWithDTS(t *testing.T) {
	header := BuildPESHeader(0xE0, 1100, 1000, 500)
	// PTS_DTS_flags should be 11 (PTS + DTS)
	if header[7]&0xC0 != 0xC0 {
		t.Errorf("PTS_DTS_flags = 0x%02X, want 0xC0", header[7]&0xC0)
	}
	if header[8] != 10 {
		t.Errorf("header_data_length = %d, want 10", header[8])
	}
}

func TestPacketizePES(t *testing.T) {
	// Small payload that fits in one packet
	pesData := make([]byte, 100)
	pesData[0] = 0x00
	pesData[1] = 0x00
	pesData[2] = 0x01

	var cc uint8
	result := PacketizePES(PIDVideo, pesData, &cc, nil)

	if len(result)%PacketSize != 0 {
		t.Fatalf("result length %d not multiple of %d", len(result), PacketSize)
	}
	if result[0] != SyncByte {
		t.Errorf("first byte = 0x%02X, want 0x%02X", result[0], SyncByte)
	}
	// payload_unit_start_indicator should be set on first packet
	if result[1]&0x40 == 0 {
		t.Error("payload_unit_start_indicator should be 1 on first packet")
	}
}

func TestPacketizePESMultiPacket(t *testing.T) {
	// Payload larger than one TS packet
	pesData := make([]byte, 500)
	var cc uint8
	result := PacketizePES(PIDVideo, pesData, &cc, nil)

	numPackets := len(result) / PacketSize
	if numPackets < 3 {
		t.Errorf("expected >= 3 packets for 500-byte PES, got %d", numPackets)
	}

	// Verify all packets start with sync byte
	for i := 0; i < numPackets; i++ {
		if result[i*PacketSize] != SyncByte {
			t.Errorf("packet %d: first byte = 0x%02X, want 0x%02X", i, result[i*PacketSize], SyncByte)
		}
	}

	// Only first packet should have payload_unit_start_indicator
	if result[PacketSize+1]&0x40 != 0 {
		t.Error("second packet should not have payload_unit_start_indicator")
	}
}

func TestMuxerWriteVideoFrame(t *testing.T) {
	// Build a minimal AVCDecoderConfigurationRecord
	sps := []byte{0x67, 0x64, 0x00, 0x28, 0xAC}
	pps := []byte{0x68, 0xEE, 0x38}
	record := buildTestAVCRecord(sps, pps)

	// AAC AudioSpecificConfig: LC, 44100Hz, stereo
	asc := []byte{0x12, 0x10}

	muxer := NewMuxer(avframe.CodecH264, avframe.CodecAAC, record, asc)

	// Build AVCC keyframe: 4-byte length + IDR NALU
	nalu := []byte{0x65, 0x88, 0x00, 0x01, 0x02, 0x03}
	payload := make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(payload, uint32(len(nalu)))
	copy(payload[4:], nalu)

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeKeyframe,
		DTS:       0,
		PTS:       0,
		Payload:   payload,
	}

	result := muxer.WriteFrame(frame)
	if len(result) == 0 {
		t.Fatal("expected non-empty result for keyframe")
	}
	if len(result)%PacketSize != 0 {
		t.Fatalf("result length %d not multiple of %d", len(result), PacketSize)
	}

	// First two packets should be PAT and PMT
	if result[0] != SyncByte {
		t.Error("first packet should start with sync byte")
	}
	// PAT PID = 0
	pid0 := uint16(result[1]&0x1F)<<8 | uint16(result[2])
	if pid0 != PIDPat {
		t.Errorf("first packet PID = 0x%04X, want PAT (0x%04X)", pid0, PIDPat)
	}
	// Second packet: PMT
	pid1 := uint16(result[PacketSize+1]&0x1F)<<8 | uint16(result[PacketSize+2])
	if pid1 != PIDPmt {
		t.Errorf("second packet PID = 0x%04X, want PMT (0x%04X)", pid1, PIDPmt)
	}
}

func TestMuxerWriteAudioFrame(t *testing.T) {
	asc := []byte{0x12, 0x10} // AAC-LC, 44100Hz, stereo
	muxer := NewMuxer(avframe.CodecH264, avframe.CodecAAC, nil, asc)

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       100,
		PTS:       100,
		Payload:   make([]byte, 50),
	}

	result := muxer.WriteFrame(frame)
	if len(result) == 0 {
		t.Fatal("expected non-empty result for audio frame")
	}
	if len(result)%PacketSize != 0 {
		t.Fatalf("result length %d not multiple of %d", len(result), PacketSize)
	}

	// Should be on audio PID
	pid := uint16(result[1]&0x1F)<<8 | uint16(result[2])
	if pid != PIDAudio {
		t.Errorf("audio packet PID = 0x%04X, want 0x%04X", pid, PIDAudio)
	}
}

func TestMuxerSkipsSequenceHeader(t *testing.T) {
	muxer := NewMuxer(avframe.CodecH264, avframe.CodecAAC, nil, nil)

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x01, 0x64, 0x00, 0x28},
	}

	result := muxer.WriteFrame(frame)
	if result != nil {
		t.Error("sequence header frames should return nil")
	}
}

func TestMuxerH265(t *testing.T) {
	muxer := NewMuxer(avframe.CodecH265, avframe.CodecAAC, nil, nil)

	// Build HVCC keyframe
	nalu := []byte{0x26, 0x01, 0x02, 0x03} // IDR
	payload := make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(payload, uint32(len(nalu)))
	copy(payload[4:], nalu)

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH265,
		FrameType: avframe.FrameTypeKeyframe,
		DTS:       0,
		PTS:       0,
		Payload:   payload,
	}

	result := muxer.WriteFrame(frame)
	if len(result) == 0 {
		t.Fatal("expected non-empty result")
	}
	if len(result)%PacketSize != 0 {
		t.Fatalf("result length %d not multiple of %d", len(result), PacketSize)
	}
}

func TestMuxerMP3Audio(t *testing.T) {
	muxer := NewMuxer(avframe.CodecH264, avframe.CodecMP3, nil, nil)

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecMP3,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       0,
		PTS:       0,
		Payload:   make([]byte, 100),
	}

	result := muxer.WriteFrame(frame)
	if len(result) == 0 {
		t.Fatal("expected non-empty result")
	}
}

// buildTestAVCRecord builds a minimal AVCDecoderConfigurationRecord for testing.
func buildTestAVCRecord(sps, pps []byte) []byte {
	var record []byte
	// configurationVersion=1, profile, compatibility, level, lengthSizeMinusOne=3
	record = append(record, 0x01, 0x64, 0x00, 0x28, 0xFF)
	// numOfSequenceParameterSets (1 in lower 5 bits)
	record = append(record, 0xE1)
	// SPS length + data
	spsLen := make([]byte, 2)
	binary.BigEndian.PutUint16(spsLen, uint16(len(sps)))
	record = append(record, spsLen...)
	record = append(record, sps...)
	// numOfPictureParameterSets
	record = append(record, 0x01)
	// PPS length + data
	ppsLen := make([]byte, 2)
	binary.BigEndian.PutUint16(ppsLen, uint16(len(pps)))
	record = append(record, ppsLen...)
	record = append(record, pps...)
	return record
}
