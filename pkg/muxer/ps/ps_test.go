package ps

import (
	"encoding/binary"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestMuxDemuxRoundTrip(t *testing.T) {
	muxer := NewMuxer()
	demuxer := NewDemuxer()

	// Create a fake H.264 keyframe with SPS/PPS NALUs (Annex-B format)
	sps := []byte{0x67, 0x42, 0x00, 0x1E, 0xAB, 0x40, 0x50} // SPS NAL type=7
	pps := []byte{0x68, 0xCE, 0x38, 0x80}                     // PPS NAL type=8
	idr := make([]byte, 100)
	idr[0] = 0x65 // IDR NAL type=5
	for i := 1; i < len(idr); i++ {
		idr[i] = byte(i)
	}

	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	var payload []byte
	payload = append(payload, startCode...)
	payload = append(payload, sps...)
	payload = append(payload, startCode...)
	payload = append(payload, pps...)
	payload = append(payload, startCode...)
	payload = append(payload, idr...)

	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264,
		avframe.FrameTypeKeyframe, 1000, 1000,
		payload,
	)

	packed, err := muxer.Pack(frame)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	// Verify pack starts with PS pack header
	if binary.BigEndian.Uint32(packed[0:4]) != PackHeaderStartCode {
		t.Fatal("missing pack header start code")
	}

	// Demux
	frames, err := demuxer.Feed(packed)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}

	if len(frames) == 0 {
		t.Fatal("no frames returned")
	}

	// First frame should be sequence header (SPS/PPS extraction)
	if frames[0].FrameType != avframe.FrameTypeSequenceHeader {
		// Or it could be the keyframe directly
		if frames[0].MediaType != avframe.MediaTypeVideo {
			t.Errorf("frame media type = %d, want video", frames[0].MediaType)
		}
	}

	if demuxer.VideoCodec() != avframe.CodecH264 {
		t.Errorf("detected codec = %v, want H264", demuxer.VideoCodec())
	}
}

func TestMuxAudio(t *testing.T) {
	muxer := NewMuxer()

	audioData := make([]byte, 160) // G.711 frame
	for i := range audioData {
		audioData[i] = byte(i)
	}

	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711A,
		avframe.FrameTypeKeyframe, 500, 500,
		audioData,
	)

	packed, err := muxer.Pack(frame)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	if len(packed) == 0 {
		t.Fatal("empty pack")
	}

	// Verify starts with pack header
	if binary.BigEndian.Uint32(packed[0:4]) != PackHeaderStartCode {
		t.Fatal("missing pack header")
	}
}

func TestDemuxEmptyData(t *testing.T) {
	d := NewDemuxer()
	_, err := d.Feed(nil)
	if err == nil {
		t.Error("expected error for nil data")
	}
	_, err = d.Feed([]byte{0x00})
	if err == nil {
		t.Error("expected error for too-short data")
	}
}

func TestPESTimestamp(t *testing.T) {
	// Test encode/decode round-trip
	ts := int64(90000) // 1 second at 90kHz
	encoded := encodePESTimestamp(ts, 0x02)
	if len(encoded) != 5 {
		t.Fatalf("encoded length = %d, want 5", len(encoded))
	}

	decoded := parsePESTimestamp(encoded)
	if decoded != ts {
		t.Errorf("decoded = %d, want %d", decoded, ts)
	}
}

func TestPESTimestampLargeValue(t *testing.T) {
	// Test with a larger timestamp
	ts := int64(8100000) // 90 seconds at 90kHz
	encoded := encodePESTimestamp(ts, 0x03)
	decoded := parsePESTimestamp(encoded)
	if decoded != ts {
		t.Errorf("decoded = %d, want %d", decoded, ts)
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
		{avframe.CodecG711A, 0x90},
		{avframe.CodecG711U, 0x91},
		{avframe.CodecOpus, 0}, // unsupported in PS
	}
	for _, tt := range tests {
		got := StreamType(tt.codec)
		if got != tt.expected {
			t.Errorf("StreamType(%v) = 0x%02x, want 0x%02x", tt.codec, got, tt.expected)
		}
	}
}

func TestCodecFromStreamType(t *testing.T) {
	tests := []struct {
		st       byte
		expected avframe.CodecType
	}{
		{0x1B, avframe.CodecH264},
		{0x24, avframe.CodecH265},
		{0x0F, avframe.CodecAAC},
		{0x90, avframe.CodecG711A},
		{0x91, avframe.CodecG711U},
		{0xFF, 0},
	}
	for _, tt := range tests {
		got := CodecFromStreamType(tt.st)
		if got != tt.expected {
			t.Errorf("CodecFromStreamType(0x%02x) = %v, want %v", tt.st, got, tt.expected)
		}
	}
}

func TestPackHeaderParsing(t *testing.T) {
	muxer := NewMuxer()
	header := muxer.buildPackHeader(0)
	if len(header) != 14 {
		t.Fatalf("pack header length = %d, want 14", len(header))
	}
	if binary.BigEndian.Uint32(header[0:4]) != PackHeaderStartCode {
		t.Error("wrong start code")
	}

	// Should be parseable by demuxer
	d := NewDemuxer()
	n, err := d.skipPackHeader(header)
	if err != nil {
		t.Fatalf("skipPackHeader: %v", err)
	}
	if n != 14 {
		t.Errorf("consumed = %d, want 14", n)
	}
}

func TestMuxEmptyPayload(t *testing.T) {
	muxer := NewMuxer()
	_, err := muxer.Pack(nil)
	if err == nil {
		t.Error("expected error for nil frame")
	}

	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nil)
	_, err = muxer.Pack(frame)
	if err == nil {
		t.Error("expected error for empty payload")
	}
}
