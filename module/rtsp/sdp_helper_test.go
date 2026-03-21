package rtsp

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

func TestSdpToMediaInfoH264(t *testing.T) {
	raw := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"
	sd, err := sdp.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	info := sdpToMediaInfo(sd)
	if info.VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v, want H264", info.VideoCodec)
	}
	if info.AudioCodec != 0 {
		t.Errorf("AudioCodec = %v, want 0", info.AudioCodec)
	}
}

func TestSdpToMediaInfoH264AAC(t *testing.T) {
	raw := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n" +
		"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/44100/2\r\n"
	sd, err := sdp.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	info := sdpToMediaInfo(sd)
	if info.VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v, want H264", info.VideoCodec)
	}
	if info.AudioCodec != avframe.CodecAAC {
		t.Errorf("AudioCodec = %v, want AAC", info.AudioCodec)
	}
	if info.SampleRate != 44100 {
		t.Errorf("SampleRate = %d, want 44100", info.SampleRate)
	}
	if info.Channels != 2 {
		t.Errorf("Channels = %d, want 2", info.Channels)
	}
}

func TestSdpToMediaInfoOpus(t *testing.T) {
	raw := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n" +
		"m=audio 0 RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n"
	sd, err := sdp.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	info := sdpToMediaInfo(sd)
	if info.AudioCodec != avframe.CodecOpus {
		t.Errorf("AudioCodec = %v, want Opus", info.AudioCodec)
	}
	if info.SampleRate != 48000 {
		t.Errorf("SampleRate = %d, want 48000", info.SampleRate)
	}
}

func TestSdpToMediaInfoEmpty(t *testing.T) {
	raw := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n"
	sd, err := sdp.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	info := sdpToMediaInfo(sd)
	if info.VideoCodec != 0 || info.AudioCodec != 0 {
		t.Errorf("expected no codecs, got video=%v audio=%v", info.VideoCodec, info.AudioCodec)
	}
}

func TestExtractTrackID(t *testing.T) {
	tests := []struct {
		url    string
		wantID int
		wantOk bool
	}{
		{"rtsp://host/live/test/trackID=0", 0, true},
		{"rtsp://host/live/test/trackID=1", 1, true},
		{"rtsp://host/live/test/trackID=12", 12, true},
		{"rtsp://host/live/test", -1, false},
		{"rtsp://host/live/test/trackID=", -1, false},
	}
	for _, tt := range tests {
		id, ok := extractTrackID(tt.url)
		if ok != tt.wantOk || id != tt.wantID {
			t.Errorf("extractTrackID(%q) = (%d, %v), want (%d, %v)", tt.url, id, ok, tt.wantID, tt.wantOk)
		}
	}
}

func TestParseSPropParameterSetsProducesAVCConfig(t *testing.T) {
	// Base64-encoded SPS and PPS
	// SPS: 0x67 0x42 0xC0 0x0D → "Z0LADQ=="
	// PPS: 0x68 0xCB 0x83 → "aMuD"
	fmtp := "96 packetization-mode=1; sprop-parameter-sets=Z0LADQ==,aMuD"
	config := parseSPropParameterSets(fmtp)
	if config == nil {
		t.Fatal("expected non-nil config")
	}
	// Should be AVCDecoderConfigurationRecord (version=1)
	if config[0] != 1 {
		t.Errorf("expected version=1, got %d", config[0])
	}
	// Profile should match SPS[1] = 0x42
	if config[1] != 0x42 {
		t.Errorf("expected profile=0x42, got 0x%x", config[1])
	}
}

func TestSdpToMediaInfoDefaultChannels(t *testing.T) {
	raw := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n" +
		"m=audio 0 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	sd, err := sdp.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	info := sdpToMediaInfo(sd)
	if info.AudioCodec != avframe.CodecG711U {
		t.Errorf("AudioCodec = %v, want G711U", info.AudioCodec)
	}
	if info.Channels != 1 {
		t.Errorf("Channels = %d, want 1 (default)", info.Channels)
	}
}
