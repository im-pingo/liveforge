package flv

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func buildFLVTag(tagType uint8, timestamp uint32, data []byte) []byte {
	dataSize := len(data)
	tag := make([]byte, TagHeaderSize+dataSize+4) // header + data + previous tag size

	tag[0] = tagType
	tag[1] = byte(dataSize >> 16)
	tag[2] = byte(dataSize >> 8)
	tag[3] = byte(dataSize)
	tag[4] = byte(timestamp >> 16)
	tag[5] = byte(timestamp >> 8)
	tag[6] = byte(timestamp)
	tag[7] = byte(timestamp >> 24) // timestamp extension
	// StreamID = 0 (bytes 8-10)

	copy(tag[TagHeaderSize:], data)

	// Previous tag size
	totalSize := uint32(TagHeaderSize + dataSize)
	tag[TagHeaderSize+dataSize] = byte(totalSize >> 24)
	tag[TagHeaderSize+dataSize+1] = byte(totalSize >> 16)
	tag[TagHeaderSize+dataSize+2] = byte(totalSize >> 8)
	tag[TagHeaderSize+dataSize+3] = byte(totalSize)

	return tag
}

func TestDemuxVideoKeyframe(t *testing.T) {
	// Video tag: H.264, keyframe, NALU packet, CTS=0
	videoData := []byte{
		(VideoFrameKeyframe << 4) | VideoCodecH264, // frame type + codec
		AVCPacketNALU,                                // AVC packet type
		0x00, 0x00, 0x00,                             // composition time offset
		0x65, 0x88, 0x00, 0x01,                       // NALU data
	}
	tag := buildFLVTag(TagTypeVideo, 1000, videoData)

	// Prepend FLV header + PreviousTagSize0
	var buf bytes.Buffer
	buf.Write(FLVHeader)
	buf.Write(PreviousTagSize0)
	buf.Write(tag)

	d := NewDemuxer(&buf)
	frame, err := d.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag error: %v", err)
	}

	if frame.MediaType != avframe.MediaTypeVideo {
		t.Errorf("expected video, got %v", frame.MediaType)
	}
	if frame.Codec != avframe.CodecH264 {
		t.Errorf("expected H264, got %v", frame.Codec)
	}
	if frame.FrameType != avframe.FrameTypeKeyframe {
		t.Errorf("expected keyframe, got %v", frame.FrameType)
	}
	if frame.DTS != 1000 {
		t.Errorf("expected DTS 1000, got %d", frame.DTS)
	}
}

func TestDemuxAudioAAC(t *testing.T) {
	// Audio tag: AAC, raw packet
	audioData := []byte{
		(AudioFormatAAC << 4) | 0x0F, // format + sound info (44100, 16bit, stereo)
		AACPacketRaw,                   // AAC packet type
		0xFF, 0x01, 0x02,              // AAC raw data
	}
	tag := buildFLVTag(TagTypeAudio, 2000, audioData)

	var buf bytes.Buffer
	buf.Write(FLVHeader)
	buf.Write(PreviousTagSize0)
	buf.Write(tag)

	d := NewDemuxer(&buf)
	frame, err := d.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag error: %v", err)
	}

	if frame.MediaType != avframe.MediaTypeAudio {
		t.Errorf("expected audio, got %v", frame.MediaType)
	}
	if frame.Codec != avframe.CodecAAC {
		t.Errorf("expected AAC, got %v", frame.Codec)
	}
	if frame.DTS != 2000 {
		t.Errorf("expected DTS 2000, got %d", frame.DTS)
	}
}

func TestDemuxVideoBFrameCTS(t *testing.T) {
	tests := []struct {
		name    string
		cts     int32
		dts     uint32
		wantPTS int64
	}{
		{"CTS=66ms", 66, 1000, 1066},
		{"CTS=100ms", 100, 2000, 2100},
		{"CTS=33ms", 33, 500, 533},
		{"CTS=0", 0, 1000, 1000},
		{"CTS=negative", -33, 100, 67},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctsBytes := [3]byte{
				byte(tt.cts >> 16),
				byte(tt.cts >> 8),
				byte(tt.cts),
			}
			videoData := []byte{
				(VideoFrameInterframe << 4) | VideoCodecH264,
				AVCPacketNALU,
				ctsBytes[0], ctsBytes[1], ctsBytes[2],
				0x41, 0x9A, 0x00, 0x01,
			}
			tag := buildFLVTag(TagTypeVideo, tt.dts, videoData)

			var buf bytes.Buffer
			buf.Write(FLVHeader)
			buf.Write(PreviousTagSize0)
			buf.Write(tag)

			d := NewDemuxer(&buf)
			frame, err := d.ReadTag()
			if err != nil {
				t.Fatalf("ReadTag error: %v", err)
			}
			if frame.DTS != int64(tt.dts) {
				t.Errorf("DTS = %d, want %d", frame.DTS, tt.dts)
			}
			if frame.PTS != tt.wantPTS {
				t.Errorf("PTS = %d, want %d", frame.PTS, tt.wantPTS)
			}
		})
	}
}

func TestDemuxSequenceHeader(t *testing.T) {
	// Video sequence header
	videoData := []byte{
		(VideoFrameKeyframe << 4) | VideoCodecH264,
		AVCPacketSequenceHeader,
		0x00, 0x00, 0x00, // CTS
		0x01, 0x64, 0x00, 0x28, // AVC decoder config
	}
	tag := buildFLVTag(TagTypeVideo, 0, videoData)

	var buf bytes.Buffer
	buf.Write(FLVHeader)
	buf.Write(PreviousTagSize0)
	buf.Write(tag)

	d := NewDemuxer(&buf)
	frame, err := d.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag error: %v", err)
	}

	if frame.FrameType != avframe.FrameTypeSequenceHeader {
		t.Errorf("expected sequence header, got %v", frame.FrameType)
	}
}
