package rtsp

import (
	"net"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestNewMulticastTransport(t *testing.T) {
	InitMulticastPorts(40000)
	cfg := config.MulticastConfig{
		Enabled:  true,
		Address:  "239.0.0.1",
		BasePort: 40000,
		TTL:      4,
	}

	mt, err := NewMulticastTransport(cfg)
	if err != nil {
		t.Fatalf("NewMulticastTransport: %v", err)
	}
	defer mt.Close()

	rtpPort, rtcpPort := mt.ServerPorts()
	if rtpPort != 40000 {
		t.Errorf("expected RTP port 40000, got %d", rtpPort)
	}
	if rtcpPort != 40001 {
		t.Errorf("expected RTCP port 40001, got %d", rtcpPort)
	}

	addr := mt.MulticastAddr()
	if !addr.Equal(net.ParseIP("239.0.0.1")) {
		t.Errorf("unexpected multicast addr: %v", addr)
	}
}

func TestMulticastTransportPortIncrement(t *testing.T) {
	InitMulticastPorts(42000)
	cfg := config.MulticastConfig{
		Enabled: true,
		Address: "239.0.0.2",
		TTL:     4,
	}

	mt1, err := NewMulticastTransport(cfg)
	if err != nil {
		t.Fatalf("first transport: %v", err)
	}
	defer mt1.Close()

	mt2, err := NewMulticastTransport(cfg)
	if err != nil {
		t.Fatalf("second transport: %v", err)
	}
	defer mt2.Close()

	rtp1, _ := mt1.ServerPorts()
	rtp2, _ := mt2.ServerPorts()
	if rtp2 != rtp1+2 {
		t.Errorf("expected port increment by 2: first=%d second=%d", rtp1, rtp2)
	}
}

func TestMulticastTransportInvalidAddress(t *testing.T) {
	cfg := config.MulticastConfig{
		Enabled: true,
		Address: "not-an-ip",
		TTL:     4,
	}
	_, err := NewMulticastTransport(cfg)
	if err == nil {
		t.Error("expected error for invalid address")
	}
}

func TestMulticastTransportNonMulticast(t *testing.T) {
	cfg := config.MulticastConfig{
		Enabled: true,
		Address: "192.168.1.1",
		TTL:     4,
	}
	_, err := NewMulticastTransport(cfg)
	if err == nil {
		t.Error("expected error for non-multicast address")
	}
}

func TestMulticastTransportSendRTP(t *testing.T) {
	InitMulticastPorts(44000)
	cfg := config.MulticastConfig{
		Enabled: true,
		Address: "239.0.0.3",
		TTL:     1,
	}

	mt, err := NewMulticastTransport(cfg)
	if err != nil {
		t.Fatalf("NewMulticastTransport: %v", err)
	}
	defer mt.Close()

	data := []byte{0x80, 0x60, 0x00, 0x01, 0x00, 0x00, 0x00, 0xA0, 0x12, 0x34, 0x56, 0x78}
	if err := mt.SendRTP(data); err != nil {
		t.Errorf("SendRTP: %v", err)
	}
	if err := mt.SendRTCP(data); err != nil {
		t.Errorf("SendRTCP: %v", err)
	}
}

func TestMulticastTransportDoubleClose(t *testing.T) {
	InitMulticastPorts(46000)
	cfg := config.MulticastConfig{
		Enabled: true,
		Address: "239.0.0.4",
		TTL:     1,
	}

	mt, err := NewMulticastTransport(cfg)
	if err != nil {
		t.Fatalf("NewMulticastTransport: %v", err)
	}

	mt.Close()
	mt.Close() // should not panic
}

func TestParseTransportHeaderMulticast(t *testing.T) {
	tc := parseTransportHeader("RTP/AVP;multicast")
	if !tc.IsMulticast {
		t.Error("expected IsMulticast=true")
	}
	if tc.IsTCP {
		t.Error("expected IsTCP=false")
	}
}

func TestHandleSetupMulticastEnabled(t *testing.T) {
	mcastCfg := &config.MulticastConfig{
		Enabled: true,
		Address: "239.0.0.10",
		TTL:     8,
	}
	InitMulticastPorts(48000)
	h := NewHandler(nil, nil, mcastCfg)
	session := NewRTSPSession("mcast-test", "live/mcast")
	session.Transition(StateDescribed)
	session.MediaInfo = &avframe.MediaInfo{VideoCodec: avframe.CodecH264}

	req := &Request{
		Method:  "SETUP",
		URL:     "rtsp://host/live/mcast/trackID=0",
		Headers: make(map[string][]string),
	}
	req.Headers.Set("CSeq", "3")
	req.Headers.Set("Transport", "RTP/AVP;multicast")

	resp := h.HandleSetup(req, session, "192.168.1.100:0")
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d %s", resp.StatusCode, resp.Reason)
	}

	transport := resp.Headers.Get("Transport")
	if transport == "" {
		t.Fatal("missing Transport header")
	}
	if !contains(transport, "multicast") {
		t.Errorf("expected multicast in Transport: %s", transport)
	}
	if !contains(transport, "239.0.0.10") {
		t.Errorf("expected multicast address in Transport: %s", transport)
	}

	if len(session.Tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(session.Tracks))
	}
	if session.Tracks[0].Multicast == nil {
		t.Error("expected Multicast transport on track")
	}
	session.Tracks[0].Multicast.Close()
}

func TestHandleSetupMulticastDisabled(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	session := NewRTSPSession("no-mcast", "live/nomcast")
	session.Transition(StateDescribed)
	session.MediaInfo = &avframe.MediaInfo{VideoCodec: avframe.CodecH264}

	req := &Request{
		Method:  "SETUP",
		URL:     "rtsp://host/live/nomcast/trackID=0",
		Headers: make(map[string][]string),
	}
	req.Headers.Set("CSeq", "3")
	req.Headers.Set("Transport", "RTP/AVP;multicast")

	resp := h.HandleSetup(req, session, "192.168.1.100:0")
	if resp.StatusCode != 461 {
		t.Errorf("expected 461 Unsupported Transport, got %d", resp.StatusCode)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
