package flv

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func muxPayload(t *testing.T, muxer *Muxer, frame *avframe.AVFrame) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := muxer.WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	data := buf.Bytes()
	if len(data) < TagHeaderSize+4 {
		t.Fatalf("encoded tag too short: %d", len(data))
	}
	dataSize := int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if len(data) != TagHeaderSize+dataSize+4 {
		t.Fatalf("tag size mismatch: len=%d dataSize=%d", len(data), dataSize)
	}
	return append([]byte(nil), data[TagHeaderSize:TagHeaderSize+dataSize]...)
}

func TestEnhancedVideoWireLayout(t *testing.T) {
	tests := []struct {
		name     string
		codec    avframe.CodecType
		payload  []byte
		cts      int64
		fourCC   string
		wantBody []byte
	}{
		{
			name:     "avc1 signed cts",
			codec:    avframe.CodecH264,
			payload:  []byte{0x65, 0x01},
			cts:      -33,
			fourCC:   "avc1",
			wantBody: []byte{0x91, 'a', 'v', 'c', '1', 0xff, 0xff, 0xdf, 0x65, 0x01},
		},
		{
			name:     "hvc1 signed cts",
			codec:    avframe.CodecH265,
			payload:  []byte{0x26, 0x01},
			cts:      -33,
			fourCC:   "hvc1",
			wantBody: []byte{0x91, 'h', 'v', 'c', '1', 0xff, 0xff, 0xdf, 0x26, 0x01},
		},
		{
			name:     "av01 no cts",
			codec:    avframe.CodecAV1,
			payload:  []byte{0x0a, 0x01},
			cts:      0,
			fourCC:   "av01",
			wantBody: []byte{0x91, 'a', 'v', '0', '1', 0x0a, 0x01},
		},
		{
			name:     "vp08 no cts",
			codec:    avframe.CodecVP8,
			payload:  []byte{0x9d, 0x01},
			cts:      0,
			fourCC:   "vp08",
			wantBody: []byte{0x91, 'v', 'p', '0', '8', 0x9d, 0x01},
		},
		{
			name:     "vp09 no cts",
			codec:    avframe.CodecVP9,
			payload:  []byte{0x9d, 0x01},
			cts:      0,
			fourCC:   "vp09",
			wantBody: []byte{0x91, 'v', 'p', '0', '9', 0x9d, 0x01},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			muxer := NewMuxerWithModes(EncodingEnhanced, EncodingClassic)
			frame := avframe.NewAVFrame(
				avframe.MediaTypeVideo,
				tt.codec,
				avframe.FrameTypeKeyframe,
				100,
				100+tt.cts,
				tt.payload,
			)
			got := muxPayload(t, muxer, frame)
			if !bytes.Equal(got, tt.wantBody) {
				t.Fatalf("body = %x, want %x", got, tt.wantBody)
			}
			if string(got[1:5]) != tt.fourCC {
				t.Fatalf("FourCC = %q, want %q", got[1:5], tt.fourCC)
			}
		})
	}
}

func TestUnknownVideoCodecHasNoEnhancedFourCC(t *testing.T) {
	if got := VideoFourCC(avframe.CodecG711A); got != "" {
		t.Fatalf("VideoFourCC(CodecG711A) = %q, want empty", got)
	}
	if IsEnhancedVideoCodec(avframe.CodecG711A) {
		t.Fatal("audio codec should not be reported as an enhanced video codec")
	}
}

func TestEnhancedVideoRejectsCompositionOffsetWithoutField(t *testing.T) {
	muxer := NewMuxerWithModes(EncodingEnhanced, EncodingClassic)
	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecAV1,
		avframe.FrameTypeInterframe,
		100,
		101,
		[]byte{0x0a},
	)
	if err := muxer.WriteFrame(&bytes.Buffer{}, frame); err == nil {
		t.Fatal("expected an error for AV1 composition offset")
	}
}

func TestEnhancedAudioWireLayout(t *testing.T) {
	tests := []struct {
		name     string
		codec    avframe.CodecType
		frame    avframe.FrameType
		fourCC   string
		payload  []byte
		wantBody []byte
	}{
		{
			name:     "opus coded",
			codec:    avframe.CodecOpus,
			frame:    avframe.FrameTypeInterframe,
			fourCC:   "Opus",
			payload:  []byte{0xfc, 0x01},
			wantBody: []byte{0x91, 'O', 'p', 'u', 's', 0xfc, 0x01},
		},
		{
			name:     "aac sequence start",
			codec:    avframe.CodecAAC,
			frame:    avframe.FrameTypeSequenceHeader,
			fourCC:   "mp4a",
			payload:  []byte{0x12, 0x10},
			wantBody: []byte{0x90, 'm', 'p', '4', 'a', 0x12, 0x10},
		},
		{
			name:     "mp3 coded",
			codec:    avframe.CodecMP3,
			frame:    avframe.FrameTypeInterframe,
			fourCC:   ".mp3",
			payload:  []byte{0xff, 0xfb},
			wantBody: []byte{0x91, '.', 'm', 'p', '3', 0xff, 0xfb},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			muxer := NewMuxerWithModes(EncodingClassic, EncodingEnhanced)
			frame := avframe.NewAVFrame(avframe.MediaTypeAudio, tt.codec, tt.frame, 0, 0, tt.payload)
			got := muxPayload(t, muxer, frame)
			if !bytes.Equal(got, tt.wantBody) {
				t.Fatalf("body = %x, want %x", got, tt.wantBody)
			}
			if string(got[1:5]) != tt.fourCC {
				t.Fatalf("FourCC = %q, want %q", got[1:5], tt.fourCC)
			}
		})
	}
}

func TestParseEnhancedVideoPayload(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		dts       int64
		codec     avframe.CodecType
		frameType avframe.FrameType
		pts       int64
		payload   []byte
	}{
		{
			name:      "hvc1 coded with negative cts",
			body:      []byte{0x91, 'h', 'v', 'c', '1', 0xff, 0xff, 0xdf, 0x26, 0x01},
			dts:       100,
			codec:     avframe.CodecH265,
			frameType: avframe.FrameTypeKeyframe,
			pts:       67,
			payload:   []byte{0x26, 0x01},
		},
		{
			name:      "av01 sequence start",
			body:      []byte{0x90, 'a', 'v', '0', '1', 0x01, 0x02},
			dts:       0,
			codec:     avframe.CodecAV1,
			frameType: avframe.FrameTypeSequenceHeader,
			pts:       0,
			payload:   []byte{0x01, 0x02},
		},
		{
			name:      "vp08 coded without cts",
			body:      []byte{0x91, 'v', 'p', '0', '8', 0x9d, 0x01},
			dts:       33,
			codec:     avframe.CodecVP8,
			frameType: avframe.FrameTypeKeyframe,
			pts:       33,
			payload:   []byte{0x9d, 0x01},
		},
		{
			name:      "hvc1 coded frames x",
			body:      []byte{0xa3, 'h', 'v', 'c', '1', 0x26, 0x02},
			dts:       44,
			codec:     avframe.CodecH265,
			frameType: avframe.FrameTypeInterframe,
			pts:       44,
			payload:   []byte{0x26, 0x02},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := append([]byte(nil), tt.body...)
			frame, err := ParseVideoPayload(body, tt.dts)
			if err != nil {
				t.Fatalf("ParseVideoPayload: %v", err)
			}
			if frame == nil {
				t.Fatal("expected frame")
			}
			if frame.Codec != tt.codec || frame.FrameType != tt.frameType {
				t.Fatalf("frame identity = (%v, %v), want (%v, %v)", frame.Codec, frame.FrameType, tt.codec, tt.frameType)
			}
			if frame.DTS != tt.dts || frame.PTS != tt.pts {
				t.Fatalf("timestamps = (%d, %d), want (%d, %d)", frame.DTS, frame.PTS, tt.dts, tt.pts)
			}
			if !bytes.Equal(frame.Payload, tt.payload) {
				t.Fatalf("payload = %x, want %x", frame.Payload, tt.payload)
			}
			body[len(body)-1] ^= 0xff
			if bytes.Equal(frame.Payload, body[len(body)-len(frame.Payload):]) {
				t.Fatal("parser did not copy payload")
			}
		})
	}
}

func TestParseClassicVideoAndAudioPayload(t *testing.T) {
	video, err := ParseVideoPayload([]byte{(VideoFrameInterframe << 4) | VideoCodecH264, 0x01, 0xff, 0xff, 0xdf, 0x41}, 100)
	if err != nil {
		t.Fatalf("ParseVideoPayload: %v", err)
	}
	if video.Codec != avframe.CodecH264 || video.PTS != 67 || video.FrameType != avframe.FrameTypeInterframe {
		t.Fatalf("unexpected classic video frame: %+v", video)
	}

	audio, err := ParseAudioPayload([]byte{AudioFormatMP3 << 4, 0xff, 0xfb}, 200)
	if err != nil {
		t.Fatalf("ParseAudioPayload: %v", err)
	}
	if audio.Codec != avframe.CodecMP3 || !bytes.Equal(audio.Payload, []byte{0xff, 0xfb}) {
		t.Fatalf("unexpected classic audio frame: %+v", audio)
	}
}

func TestParseEnhancedAudioPayload(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		codec     avframe.CodecType
		frameType avframe.FrameType
		payload   []byte
	}{
		{
			name:      "opus coded",
			body:      []byte{0x91, 'O', 'p', 'u', 's', 0xfc, 0x01},
			codec:     avframe.CodecOpus,
			frameType: avframe.FrameTypeInterframe,
			payload:   []byte{0xfc, 0x01},
		},
		{
			name:      "mp4a sequence start",
			body:      []byte{0x90, 'm', 'p', '4', 'a', 0x12, 0x10},
			codec:     avframe.CodecAAC,
			frameType: avframe.FrameTypeSequenceHeader,
			payload:   []byte{0x12, 0x10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame, err := ParseAudioPayload(tt.body, 55)
			if err != nil {
				t.Fatalf("ParseAudioPayload: %v", err)
			}
			if frame.Codec != tt.codec || frame.FrameType != tt.frameType || frame.DTS != 55 || frame.PTS != 55 {
				t.Fatalf("unexpected frame: %+v", frame)
			}
			if !bytes.Equal(frame.Payload, tt.payload) {
				t.Fatalf("payload = %x, want %x", frame.Payload, tt.payload)
			}
		})
	}
}

func TestParseUnsupportedEnhancedPayload(t *testing.T) {
	badBodies := [][]byte{
		{0x96, 'a', 'v', '0', '1', 0x00}, // multitrack packet type
		{0x91, 'x', 'x', 'x', 'x', 0x00}, // unknown video FourCC
		{0x91, 'O', 'p', 'u'},            // truncated audio FourCC
	}
	for _, body := range badBodies {
		if frame, err := ParseVideoPayload(body, 0); err == nil && frame != nil {
			t.Fatalf("expected unsupported video body to fail: %x", body)
		}
	}
}

func TestDemuxSkipsEnhancedSequenceEnd(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(FLVHeader)
	buf.Write(PreviousTagSize0)
	buf.Write(buildFLVTag(TagTypeVideo, 0, []byte{0x92, 'a', 'v', '0', '1'}))
	buf.Write(buildFLVTag(TagTypeVideo, 10, []byte{0x91, 'a', 'v', '0', '1', 0x01}))

	demuxer := NewDemuxer(&buf)
	frame, err := demuxer.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag: %v", err)
	}
	if frame.Codec != avframe.CodecAV1 || frame.DTS != 10 {
		t.Fatalf("unexpected frame after sequence end: %+v", frame)
	}
}
