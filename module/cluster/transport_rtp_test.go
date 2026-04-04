package cluster

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

func TestRTPTransportScheme(t *testing.T) {
	pa, _ := parsePortRange("30000-30010")
	tr := &RTPTransport{
		cfg:   defaultClusterRTPConfig(),
		ports: pa,
	}
	if tr.Scheme() != "rtp" {
		t.Errorf("Scheme() = %q, want %q", tr.Scheme(), "rtp")
	}
}

func TestPortAllocator(t *testing.T) {
	pa, err := parsePortRange("30000-30004")
	if err != nil {
		t.Fatalf("parsePortRange: %v", err)
	}

	// Allocate all 5 ports.
	allocated := make([]int, 0, 5)
	for range 5 {
		port, err := pa.Allocate()
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		allocated = append(allocated, port)
	}

	// Verify all ports are in range.
	for _, p := range allocated {
		if p < 30000 || p > 30004 {
			t.Errorf("allocated port %d out of range", p)
		}
	}

	// Should be exhausted.
	_, err = pa.Allocate()
	if err == nil {
		t.Error("expected error when all ports exhausted")
	}

	// Free one and reallocate.
	pa.Free(allocated[0])
	port, err := pa.Allocate()
	if err != nil {
		t.Fatalf("Allocate after Free: %v", err)
	}
	if port != allocated[0] {
		// The freed port should be the one we get back (only one available).
		if port < 30000 || port > 30004 {
			t.Errorf("reallocated port %d out of range", port)
		}
	}
}

func TestPortAllocatorBadRange(t *testing.T) {
	tests := []struct {
		name      string
		portRange string
	}{
		{"no dash", "20000"},
		{"inverted", "20100-20000"},
		{"non-numeric min", "abc-20100"},
		{"non-numeric max", "20000-xyz"},
		{"port too low", "0-100"},
		{"port too high", "20000-70000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePortRange(tt.portRange)
			if err == nil {
				t.Errorf("expected error for range %q", tt.portRange)
			}
		})
	}
}

func TestPortAllocatorParseEdgeCases(t *testing.T) {
	// Single port range.
	pa, err := parsePortRange("20000-20000")
	if err != nil {
		t.Fatalf("single port range: %v", err)
	}

	port, err := pa.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if port != 20000 {
		t.Errorf("port = %d, want 20000", port)
	}

	// Should be exhausted after one allocation.
	_, err = pa.Allocate()
	if err == nil {
		t.Error("expected error for single-port exhaustion")
	}

	// Free and reallocate.
	pa.Free(20000)
	port, err = pa.Allocate()
	if err != nil {
		t.Fatalf("Allocate after free: %v", err)
	}
	if port != 20000 {
		t.Errorf("port = %d, want 20000", port)
	}
}

func TestRTPTransportPushBadURL(t *testing.T) {
	pa, _ := parsePortRange("30100-30110")
	tr := &RTPTransport{
		cfg:   defaultClusterRTPConfig(),
		ports: pa,
	}

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
	}}
	stream.SetPublisher(pub)

	ctx := context.Background()
	err := tr.Push(ctx, "rtp://127.0.0.1:19999/live/test", stream)
	if err == nil {
		t.Error("expected error for signaling to non-existent server")
	}
}

func TestRTPTransportInterfaceCompliance(t *testing.T) {
	var _ RelayTransport = (*RTPTransport)(nil)
}

func TestBuildBYE(t *testing.T) {
	ssrc := uint32(0xDEADBEEF)
	data := pkgrtp.BuildBYE(ssrc)

	// RTCP BYE is 8 bytes: header(4) + SSRC(4)
	if len(data) != 8 {
		t.Fatalf("BuildBYE length = %d, want 8", len(data))
	}

	// Check version = 2, SC = 1
	version := data[0] >> 6
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}
	sc := data[0] & 0x1f
	if sc != 1 {
		t.Errorf("SC = %d, want 1", sc)
	}

	// Check PT = 203
	if data[1] != pkgrtp.RTCPTypeBYE {
		t.Errorf("PT = %d, want %d", data[1], pkgrtp.RTCPTypeBYE)
	}

	// Check length = 1 (in 32-bit words minus one)
	length := binary.BigEndian.Uint16(data[2:4])
	if length != 1 {
		t.Errorf("length = %d, want 1", length)
	}

	// Check SSRC
	gotSSRC := binary.BigEndian.Uint32(data[4:8])
	if gotSSRC != ssrc {
		t.Errorf("SSRC = 0x%x, want 0x%x", gotSSRC, ssrc)
	}
}

func TestRTPSignalingPushHandler(t *testing.T) {
	pa, err := parsePortRange("31000-31010")
	if err != nil {
		t.Fatalf("parsePortRange: %v", err)
	}

	hub, _ := newTestHub()
	// Create the stream so the push handler can find it.
	hub.GetOrCreate("live/test")

	tr := &RTPTransport{
		cfg:   defaultClusterRTPConfig(),
		ports: pa,
		hub:   hub,
	}

	// Build a valid SDP offer with H264 video.
	mi := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
		SampleRate: 44100,
	}
	offerSD := sdp.BuildFromMediaInfo(mi, "rtp://localhost/live/test", "0.0.0.0")
	offerSDP := offerSD.Marshal()

	server := httptest.NewServer(http.HandlerFunc(tr.handleSignalingPush))
	defer server.Close()

	resp, err := http.Post(
		server.URL+"?stream=live/test",
		"application/sdp",
		strings.NewReader(string(offerSDP)),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body: %s", resp.StatusCode, string(body))
	}

	// Verify response is valid SDP.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	answerSD, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("parse SDP answer: %v", err)
	}
	if len(answerSD.Media) == 0 {
		t.Error("answer SDP has no media sections")
	}
}

func TestRTPSignalingPullHandler(t *testing.T) {
	pa, err := parsePortRange("31020-31030")
	if err != nil {
		t.Fatalf("parsePortRange: %v", err)
	}

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/pulltest")
	pub := &originPublisher{id: "test-pub", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecOpus,
		SampleRate: 48000,
	}}
	stream.SetPublisher(pub)

	tr := &RTPTransport{
		cfg:   defaultClusterRTPConfig(),
		ports: pa,
		hub:   hub,
	}

	// Build SDP offer for pull (receiver's perspective).
	offerSD := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username: "-", SessionID: "0", SessionVersion: "1",
			NetType: "IN", AddrType: "IP4", Address: "127.0.0.1",
		},
		Name: "Pull Offer",
		Connection: &sdp.Connection{
			NetType: "IN", AddrType: "IP4", Address: "127.0.0.1",
		},
		Timing: sdp.Timing{Start: 0, Stop: 0},
		Media: []*sdp.MediaDescription{
			{
				Type:    "video",
				Port:    31050,
				Proto:   "RTP/AVP",
				Formats: []int{96},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "96 H264/90000"},
					{Key: "recvonly"},
				},
			},
		},
	}
	offerSDP := offerSD.Marshal()

	server := httptest.NewServer(http.HandlerFunc(tr.handleSignalingPull))
	defer server.Close()

	resp, err := http.Post(
		server.URL+"?stream=live/pulltest",
		"application/sdp",
		strings.NewReader(string(offerSDP)),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	answerSD, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("parse SDP answer: %v", err)
	}
	if len(answerSD.Media) == 0 {
		t.Error("answer SDP has no media sections")
	}
}

func TestRTPSignaling406(t *testing.T) {
	pa, err := parsePortRange("31040-31050")
	if err != nil {
		t.Fatalf("parsePortRange: %v", err)
	}

	hub, _ := newTestHub()
	tr := &RTPTransport{
		cfg:   defaultClusterRTPConfig(),
		ports: pa,
		hub:   hub,
	}

	// Build SDP with no recognized codecs.
	offerSD := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username: "-", SessionID: "0", SessionVersion: "1",
			NetType: "IN", AddrType: "IP4", Address: "0.0.0.0",
		},
		Name: "Test",
		Timing: sdp.Timing{Start: 0, Stop: 0},
		Media: []*sdp.MediaDescription{
			{
				Type:    "video",
				Port:    9000,
				Proto:   "RTP/AVP",
				Formats: []int{120},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "120 UNSUPPORTED/90000"},
				},
			},
		},
	}
	offerSDP := offerSD.Marshal()

	server := httptest.NewServer(http.HandlerFunc(tr.handleSignalingPush))
	defer server.Close()

	resp, err := http.Post(
		server.URL+"?stream=live/test406",
		"application/sdp",
		strings.NewReader(string(offerSDP)),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotAcceptable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotAcceptable)
	}
}

func TestParseRTPURL(t *testing.T) {
	tests := []struct {
		url       string
		host      string
		streamKey string
		wantErr   bool
	}{
		{"rtp://host:5000/live/test", "host:5000", "live/test", false},
		{"rtp://host:5000/key", "host:5000", "key", false},
		{"rtp://host:5000", "", "", true},           // no stream key
		{"rtp://host:5000/", "", "", true},           // empty path
		{"rtmp://host:5000/live/test", "", "", true},  // wrong scheme
		{"not-a-url", "", "", true},                   // invalid URL
	}
	for _, tt := range tests {
		host, key, err := parseRTPURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRTPURL(%q) err=%v, wantErr=%v", tt.url, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if host != tt.host {
				t.Errorf("parseRTPURL(%q) host=%q, want %q", tt.url, host, tt.host)
			}
			if key != tt.streamKey {
				t.Errorf("parseRTPURL(%q) key=%q, want %q", tt.url, key, tt.streamKey)
			}
		}
	}
}

func TestDefaultClusterRTPConfig(t *testing.T) {
	cfg := defaultClusterRTPConfig()
	if cfg.PortRange != "20000-20100" {
		t.Errorf("PortRange = %q, want %q", cfg.PortRange, "20000-20100")
	}
	if cfg.SignalingPath != "/api/relay" {
		t.Errorf("SignalingPath = %q, want %q", cfg.SignalingPath, "/api/relay")
	}
}

func TestRTPTransportClose(t *testing.T) {
	pa, _ := parsePortRange("30200-30210")
	tr := &RTPTransport{
		cfg:   defaultClusterRTPConfig(),
		ports: pa,
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}
