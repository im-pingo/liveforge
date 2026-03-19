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
	vMap := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if vMap == nil || vMap.EncodingName != "H264" || vMap.ClockRate != 90000 {
		t.Errorf("video rtpmap = %+v", vMap)
	}
	aMap := sd.Media[1].RTPMap(sd.Media[1].Formats[0])
	if aMap == nil || aMap.EncodingName != "MPEG4-GENERIC" || aMap.ClockRate != 44100 {
		t.Errorf("audio rtpmap = %+v", aMap)
	}
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

func TestBuildFromMediaInfoAudioOnly(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecOpus,
		SampleRate: 48000,
		Channels:   2,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "opus" || rm.ClockRate != 48000 {
		t.Errorf("rtpmap = %+v", rm)
	}
	// Opus should have channels in rtpmap
	if rm.Channels != 2 {
		t.Errorf("opus channels = %d, want 2", rm.Channels)
	}
}

func TestBuildFromMediaInfoVP8(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecVP8,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "VP8" || rm.ClockRate != 90000 {
		t.Errorf("rtpmap = %+v", rm)
	}
}

func TestBuildFromMediaInfoVP9(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecVP9,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "VP9" || rm.ClockRate != 90000 {
		t.Errorf("rtpmap = %+v", rm)
	}
}

func TestBuildFromMediaInfoAV1(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecAV1,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "AV1" || rm.ClockRate != 90000 {
		t.Errorf("rtpmap = %+v", rm)
	}
}

func TestBuildFromMediaInfoMP3(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecMP3,
		SampleRate: 44100,
		Channels:   2,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "MPA" || rm.ClockRate != 90000 {
		t.Errorf("rtpmap = %+v", rm)
	}
}

func TestBuildFromMediaInfoG711U(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711U,
		SampleRate: 8000,
		Channels:   1,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "PCMU" || rm.ClockRate != 8000 {
		t.Errorf("rtpmap = %+v", rm)
	}
	// G.711 uses static PT 0
	if sd.Media[0].Formats[0] != 0 {
		t.Errorf("G711U PT = %d, want 0", sd.Media[0].Formats[0])
	}
}

func TestBuildFromMediaInfoG711A(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "PCMA" || rm.ClockRate != 8000 {
		t.Errorf("rtpmap = %+v", rm)
	}
	if sd.Media[0].Formats[0] != 8 {
		t.Errorf("G711A PT = %d, want 8", sd.Media[0].Formats[0])
	}
}

func TestBuildFromMediaInfoG722(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecG722,
		SampleRate: 16000,
		Channels:   1,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "G722" || rm.ClockRate != 8000 {
		t.Errorf("rtpmap = %+v", rm)
	}
	if sd.Media[0].Formats[0] != 9 {
		t.Errorf("G722 PT = %d, want 9", sd.Media[0].Formats[0])
	}
}

func TestBuildFromMediaInfoG729(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecG729,
		SampleRate: 8000,
		Channels:   1,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "G729" || rm.ClockRate != 8000 {
		t.Errorf("rtpmap = %+v", rm)
	}
	if sd.Media[0].Formats[0] != 18 {
		t.Errorf("G729 PT = %d, want 18", sd.Media[0].Formats[0])
	}
}

func TestBuildFromMediaInfoSpeex(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecSpeex,
		SampleRate: 16000,
		Channels:   1,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	if len(sd.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(sd.Media))
	}
	rm := sd.Media[0].RTPMap(sd.Media[0].Formats[0])
	if rm == nil || rm.EncodingName != "speex" || rm.ClockRate != 16000 {
		t.Errorf("rtpmap = %+v", rm)
	}
}

func TestBuildFromMediaInfoAACFmtp(t *testing.T) {
	info := &avframe.MediaInfo{
		AudioCodec: avframe.CodecAAC,
		SampleRate: 48000,
		Channels:   2,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	pt := sd.Media[0].Formats[0]
	fmtp := sd.Media[0].FMTP(pt)
	if fmtp == "" {
		t.Fatal("missing fmtp for AAC")
	}
	expected := "profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3"
	if fmtp != expected {
		t.Errorf("fmtp = %q, want %q", fmtp, expected)
	}
}

func TestBuildFromMediaInfoSessionFields(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")

	if sd.Version != 0 {
		t.Errorf("version = %d, want 0", sd.Version)
	}
	if sd.Origin.Address != "192.168.1.1" {
		t.Errorf("origin address = %q, want %q", sd.Origin.Address, "192.168.1.1")
	}
	if sd.Connection == nil || sd.Connection.Address != "0.0.0.0" {
		t.Error("missing or wrong connection address")
	}
	// Session-level control should be the base URL
	found := false
	for _, a := range sd.Attributes {
		if a.Key == "control" && a.Value == "rtsp://host/live/test" {
			found = true
		}
	}
	if !found {
		t.Error("missing session-level control attribute")
	}
}

func TestBuildFromMediaInfoMarshalRoundTrip(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
		SampleRate: 44100,
		Channels:   2,
	}
	sd := BuildFromMediaInfo(info, "rtsp://host/live/test", "192.168.1.1")
	raw := sd.Marshal()
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(Marshal()) failed: %v", err)
	}
	if len(parsed.Media) != 2 {
		t.Fatalf("round-trip media count = %d, want 2", len(parsed.Media))
	}
}
