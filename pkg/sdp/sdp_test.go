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

func TestMediaDirectionDefault(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s=Test\r\n" +
		"t=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n"

	sd, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sd.Media[0].Direction() != "sendrecv" {
		t.Errorf("Direction = %q, want sendrecv", sd.Media[0].Direction())
	}
}

func TestMediaDirectionExplicit(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s=Test\r\n" +
		"t=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=recvonly\r\n"

	sd, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sd.Media[0].Direction() != "recvonly" {
		t.Errorf("Direction = %q, want recvonly", sd.Media[0].Direction())
	}
}

func TestParseConnectionLine(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s=Test\r\n" +
		"c=IN IP4 192.168.1.1\r\n" +
		"t=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n"

	sd, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sd.Connection == nil {
		t.Fatal("Connection is nil")
	}
	if sd.Connection.Address != "192.168.1.1" {
		t.Errorf("Connection.Address = %q", sd.Connection.Address)
	}
}

func TestParseMultipleFormats(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s=Test\r\n" +
		"t=0 0\r\n" +
		"m=video 0 RTP/AVP 96 97\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=rtpmap:97 H265/90000\r\n"

	sd, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(sd.Media[0].Formats) != 2 {
		t.Fatalf("Formats count = %d, want 2", len(sd.Media[0].Formats))
	}
	if sd.Media[0].Formats[0] != 96 || sd.Media[0].Formats[1] != 97 {
		t.Errorf("Formats = %v", sd.Media[0].Formats)
	}
}

func TestMarshalFullSDP(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s=Test\r\n" +
		"i=Session Info\r\n" +
		"c=IN IP4 10.0.0.1\r\n" +
		"t=0 0\r\n" +
		"a=tool:liveforge\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"c=IN IP4 10.0.0.2\r\n" +
		"b=AS:500\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=recvonly\r\n"

	sd, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sd.Info != "Session Info" {
		t.Errorf("Info = %q", sd.Info)
	}
	if len(sd.Attributes) != 1 || sd.Attributes[0].Key != "tool" {
		t.Errorf("session Attributes = %v", sd.Attributes)
	}

	out := sd.Marshal()
	sd2, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if sd2.Info != "Session Info" {
		t.Errorf("re-parsed Info = %q", sd2.Info)
	}
	if sd2.Connection == nil || sd2.Connection.Address != "10.0.0.1" {
		t.Error("session Connection lost in roundtrip")
	}
	m := sd2.Media[0]
	if m.Connection == nil || m.Connection.Address != "10.0.0.2" {
		t.Error("media Connection lost in roundtrip")
	}
	if m.Bandwidth != "AS:500" {
		t.Errorf("Bandwidth = %q", m.Bandwidth)
	}
	if m.Direction() != "recvonly" {
		t.Errorf("Direction = %q", m.Direction())
	}
}

func TestRTPMapNotFound(t *testing.T) {
	md := &MediaDescription{
		Attributes: []Attribute{
			{Key: "rtpmap", Value: "96 H264/90000"},
		},
	}
	if md.RTPMap(99) != nil {
		t.Error("expected nil for unknown payload type")
	}
}

func TestFMTPNotFound(t *testing.T) {
	md := &MediaDescription{
		Attributes: []Attribute{
			{Key: "fmtp", Value: "96 some-params"},
		},
	}
	if md.FMTP(99) != "" {
		t.Error("expected empty for unknown payload type")
	}
}

func TestControlNotFound(t *testing.T) {
	md := &MediaDescription{}
	if md.Control() != "" {
		t.Error("expected empty control")
	}
}

func TestParseInvalidVersion(t *testing.T) {
	_, err := Parse([]byte("v=abc\r\n"))
	if err == nil {
		t.Fatal("expected error for non-numeric version")
	}
}

func TestParseInvalidOrigin(t *testing.T) {
	_, err := Parse([]byte("v=0\r\no=bad\r\n"))
	if err == nil {
		t.Fatal("expected error for invalid origin")
	}
}

func TestParseInvalidConnection(t *testing.T) {
	_, err := Parse([]byte("v=0\r\nc=bad\r\n"))
	if err == nil {
		t.Fatal("expected error for invalid connection")
	}
}

func TestParseInvalidTiming(t *testing.T) {
	_, err := Parse([]byte("v=0\r\nt=abc 0\r\n"))
	if err == nil {
		t.Fatal("expected error for invalid timing")
	}
	_, err = Parse([]byte("v=0\r\nt=0 abc\r\n"))
	if err == nil {
		t.Fatal("expected error for invalid timing stop")
	}
	_, err = Parse([]byte("v=0\r\nt=0\r\n"))
	if err == nil {
		t.Fatal("expected error for timing with one field")
	}
}

func TestParseInvalidMedia(t *testing.T) {
	_, err := Parse([]byte("v=0\r\nm=video\r\n"))
	if err == nil {
		t.Fatal("expected error for invalid media line")
	}
	_, err = Parse([]byte("v=0\r\nm=video abc RTP/AVP 96\r\n"))
	if err == nil {
		t.Fatal("expected error for non-numeric port")
	}
	_, err = Parse([]byte("v=0\r\nm=video 0 RTP/AVP abc\r\n"))
	if err == nil {
		t.Fatal("expected error for non-numeric format")
	}
}
