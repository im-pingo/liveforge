package cluster

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

func TestParseRTMPURL(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		wantHost   string
		wantApp    string
		wantStream string
		wantErr    bool
	}{
		{
			name:       "standard URL",
			url:        "rtmp://example.com/live/stream1",
			wantHost:   "example.com:1935",
			wantApp:    "live",
			wantStream: "stream1",
		},
		{
			name:       "URL with port",
			url:        "rtmp://example.com:1936/live/stream1",
			wantHost:   "example.com:1936",
			wantApp:    "live",
			wantStream: "stream1",
		},
		{
			name:       "nested stream name",
			url:        "rtmp://example.com/live/room1/cam2",
			wantHost:   "example.com:1935",
			wantApp:    "live",
			wantStream: "room1/cam2",
		},
		{
			name:    "no scheme",
			url:     "example.com/live/stream1",
			wantErr: true,
		},
		{
			name:    "no path",
			url:     "rtmp://example.com",
			wantErr: true,
		},
		{
			name:    "no stream name",
			url:     "rtmp://example.com/live",
			wantErr: true,
		},
		{
			name:    "empty stream name",
			url:     "rtmp://example.com/live/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, app, stream, err := parseRTMPURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if app != tt.wantApp {
				t.Errorf("app = %q, want %q", app, tt.wantApp)
			}
			if stream != tt.wantStream {
				t.Errorf("stream = %q, want %q", stream, tt.wantStream)
			}
		})
	}
}

func TestParseVideoPayload(t *testing.T) {
	// Keyframe H.264 NALU
	data := []byte{
		0x17,                   // keyframe (1) + H.264 (7)
		0x01,                   // AVC NALU
		0x00, 0x00, 0x00,      // CTS = 0
		0x65, 0x88, 0x00, 0x01, // NALU data
	}
	frame := parseVideoPayload(data, 1000)
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Codec != avframe.CodecH264 {
		t.Errorf("Codec = %v, want H264", frame.Codec)
	}
	if frame.FrameType != avframe.FrameTypeKeyframe {
		t.Errorf("FrameType = %v, want Keyframe", frame.FrameType)
	}
	if frame.DTS != 1000 {
		t.Errorf("DTS = %d, want 1000", frame.DTS)
	}

	// Sequence header
	seqData := []byte{
		0x17, // keyframe + H.264
		flvpkg.AVCPacketSequenceHeader,
		0x00, 0x00, 0x00,
		0x01, 0x64, 0x00, 0x28,
	}
	frame = parseVideoPayload(seqData, 0)
	if frame == nil {
		t.Fatal("expected non-nil frame for seq header")
	}
	if frame.FrameType != avframe.FrameTypeSequenceHeader {
		t.Errorf("FrameType = %v, want SequenceHeader", frame.FrameType)
	}

	// Interframe
	interData := []byte{
		0x27,                   // interframe (2) + H.264 (7)
		0x01,                   // AVC NALU
		0x00, 0x00, 0x00,      // CTS = 0
		0x41, 0x9A, 0x00, 0x01,
	}
	frame = parseVideoPayload(interData, 2000)
	if frame != nil && frame.FrameType != avframe.FrameTypeInterframe {
		t.Errorf("FrameType = %v, want Interframe", frame.FrameType)
	}

	// Too short
	frame = parseVideoPayload([]byte{0x17, 0x01, 0x00}, 0)
	if frame != nil {
		t.Error("expected nil for short payload")
	}

	// Unknown codec
	frame = parseVideoPayload([]byte{0x10, 0x01, 0x00, 0x00, 0x00, 0x01}, 0)
	if frame != nil {
		t.Error("expected nil for unknown codec")
	}
}

func TestParseAudioPayload(t *testing.T) {
	// AAC sequence header
	seqData := []byte{
		0xAF, // AAC (10) + config
		flvpkg.AACPacketSequenceHeader,
		0x12, 0x10,
	}
	frame := parseAudioPayload(seqData, 0)
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.FrameType != avframe.FrameTypeSequenceHeader {
		t.Errorf("FrameType = %v, want SequenceHeader", frame.FrameType)
	}
	if frame.Codec != avframe.CodecAAC {
		t.Errorf("Codec = %v, want AAC", frame.Codec)
	}

	// AAC raw data
	rawData := []byte{
		0xAF, // AAC
		0x01, // Raw
		0xDE, 0xAD,
	}
	frame = parseAudioPayload(rawData, 1000)
	if frame == nil {
		t.Fatal("expected non-nil frame for raw audio")
	}
	if frame.FrameType != avframe.FrameTypeInterframe {
		t.Errorf("FrameType = %v, want Interframe", frame.FrameType)
	}

	// Too short
	frame = parseAudioPayload([]byte{0xAF}, 0)
	if frame != nil {
		t.Error("expected nil for short payload")
	}

	// Unknown format
	frame = parseAudioPayload([]byte{0x0F, 0x01}, 0)
	if frame != nil {
		t.Error("expected nil for unknown format")
	}
}

func TestBuildRTMPPayload(t *testing.T) {
	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeKeyframe,
		DTS:       1000,
		PTS:       1000,
		Payload:   []byte{0x65, 0x88, 0x00, 0x01},
	}

	payload, err := buildRTMPPayload(frame)
	if err != nil {
		t.Fatalf("buildRTMPPayload: %v", err)
	}
	if payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if len(payload) == 0 {
		t.Error("expected non-empty payload")
	}
}

func TestBuildRTMPPayloadAudio(t *testing.T) {
	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		DTS:       0,
		PTS:       0,
		Payload:   []byte{0x12, 0x10},
	}

	payload, err := buildRTMPPayload(frame)
	if err != nil {
		t.Fatalf("buildRTMPPayload: %v", err)
	}
	if payload == nil {
		t.Fatal("expected non-nil payload")
	}
}
