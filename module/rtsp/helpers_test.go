package rtsp

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
)

func TestToNTP(t *testing.T) {
	// Test with a known time
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ntp := toNTP(ts)
	if ntp == 0 {
		t.Error("expected non-zero NTP timestamp")
	}
	// High 32 bits should be seconds since NTP epoch
	secs := ntp >> 32
	const ntpEpochOffset = 2208988800
	expected := uint64(ts.Unix()) + ntpEpochOffset
	if secs != expected {
		t.Errorf("NTP seconds = %d, want %d", secs, expected)
	}
}

func TestPionPayloaderForCodec(t *testing.T) {
	tests := []struct {
		codec    avframe.CodecType
		wantNil  bool
	}{
		{avframe.CodecH264, false},
		{avframe.CodecVP8, false},
		{avframe.CodecVP9, false},
		{avframe.CodecOpus, false},
		{avframe.CodecG711U, false},
		{avframe.CodecG711A, false},
		{avframe.CodecG722, false},
		{avframe.CodecAAC, true},      // No pion payloader for AAC
		{avframe.CodecH265, true},     // No pion payloader for H.265
		{avframe.CodecType(99), true}, // Unknown
	}
	for _, tt := range tests {
		p := pionPayloaderForCodec(tt.codec)
		if tt.wantNil && p != nil {
			t.Errorf("pionPayloaderForCodec(%v) should be nil", tt.codec)
		}
		if !tt.wantNil && p == nil {
			t.Errorf("pionPayloaderForCodec(%v) should not be nil", tt.codec)
		}
	}
}

func TestInterleavedChannelToMediaType(t *testing.T) {
	session := NewRTSPSession("test-id", "live/test")
	session.Tracks = []TrackSetup{
		{TrackID: 0, Codec: avframe.CodecH264, Transport: TransportConfig{IsTCP: true, Interleaved: [2]int{0, 1}}},
		{TrackID: 1, Codec: avframe.CodecAAC, Transport: TransportConfig{IsTCP: true, Interleaved: [2]int{2, 3}}},
	}

	if mt := interleavedChannelToMediaType(session, 0); mt != avframe.MediaTypeVideo {
		t.Errorf("channel 0: got %v, want Video", mt)
	}
	if mt := interleavedChannelToMediaType(session, 2); mt != avframe.MediaTypeAudio {
		t.Errorf("channel 2: got %v, want Audio", mt)
	}
	if mt := interleavedChannelToMediaType(session, 99); mt != 0 {
		t.Errorf("channel 99: got %v, want 0", mt)
	}
}

func TestNewRTSPPublisherWithPTMap(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
	}

	// Create a dummy stream for the publisher
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
	stream, _ := hub.GetOrCreate("live/test")

	ptMap := PTMap{
		96:  avframe.CodecH264,
		101: avframe.CodecAAC,
	}
	pub, err := NewRTSPPublisher("test-pub", info, stream, ptMap)
	if err != nil {
		t.Fatalf("NewRTSPPublisher: %v", err)
	}
	if pub.ID() != "test-pub" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo() != info {
		t.Error("MediaInfo mismatch")
	}
}

func TestNewRTSPPublisherNoPTMap(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecOpus,
	}

	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
	stream, _ := hub.GetOrCreate("live/test")

	pub, err := NewRTSPPublisher("test-pub", info, stream, nil)
	if err != nil {
		t.Fatalf("NewRTSPPublisher: %v", err)
	}
	// Should have depacketizers for default PTs
	if len(pub.depacketizers) != 2 {
		t.Errorf("expected 2 depacketizers, got %d", len(pub.depacketizers))
	}
}

func TestNewRTSPSubscriberWithOpusAudio(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecOpus,
	}
	var buf bytes.Buffer
	sub, err := NewRTSPSubscriber("test-sub", info, &buf, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}
	defer sub.Close()

	if sub.videoPacketizer == nil {
		t.Error("expected video packetizer")
	}
	if sub.audioPacketizer == nil {
		t.Error("expected audio packetizer (Opus has pion payloader)")
	}
}

func TestNewRTSPSubscriberVideoOnly(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecVP8}
	var buf bytes.Buffer
	sub, err := NewRTSPSubscriber("test-sub", info, &buf, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}
	defer sub.Close()

	if sub.videoPacketizer == nil {
		t.Error("expected video packetizer for VP8")
	}
}

func TestHandleDescribeNoServer(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/test")
	req := &Request{Method: "DESCRIBE", URL: "rtsp://host/live/test", Headers: make(map[string][]string)}
	req.Headers.Set("CSeq", "1")
	resp := h.HandleDescribe(req, session)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Headers.Get("Content-Type") != "application/sdp" {
		t.Errorf("Content-Type = %q", resp.Headers.Get("Content-Type"))
	}
}

func TestModuleInitAndClose(t *testing.T) {
	cfg := &config.Config{}
	cfg.RTSP.Enabled = true
	cfg.RTSP.Listen = "127.0.0.1:0"
	cfg.RTSP.RTPPortRange = []int{30000, 30100}
	cfg.Stream.RingBufferSize = 256

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)
	if err := srv.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Try connecting to the RTSP port
	conn, err := net.DialTimeout("tcp", m.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()

	srv.Shutdown()
}

func TestNewResponseCopiesCSeq(t *testing.T) {
	req := &Request{Headers: make(map[string][]string)}
	req.Headers.Set("CSeq", "42")
	resp := newResponse(200, "OK", req)
	if resp.Headers.Get("CSeq") != "42" {
		t.Errorf("CSeq = %q, want %q", resp.Headers.Get("CSeq"), "42")
	}
}

func TestNewResponseNilRequest(t *testing.T) {
	resp := newResponse(404, "Not Found", nil)
	if resp.StatusCode != 404 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	if resp.Headers.Get("CSeq") != "" {
		t.Errorf("CSeq should be empty, got %q", resp.Headers.Get("CSeq"))
	}
}

func TestHandlePlayInvalidState(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/test")
	// Init state -> cannot PLAY directly
	req := &Request{Method: "PLAY", URL: "rtsp://host/live/test", Headers: make(map[string][]string)}
	req.Headers.Set("CSeq", "1")
	resp := h.HandlePlay(req, session, "127.0.0.1:12345")
	if resp.StatusCode != 455 {
		t.Errorf("StatusCode = %d, want 455", resp.StatusCode)
	}
}

func TestHandleSetupNoPortManager(t *testing.T) {
	// TCP transport should work without port manager
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	req := &Request{Method: "SETUP", URL: "rtsp://host/live/test/trackID=0", Headers: make(map[string][]string)}
	req.Headers.Set("CSeq", "3")
	req.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=0-1")
	resp := h.HandleSetup(req, session, "127.0.0.1:12345")
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
}

func TestHandleSetupWithMediaInfo(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	session.MediaInfo = &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
	}

	// First track should get video codec
	req := &Request{Method: "SETUP", URL: "rtsp://host/live/test/trackID=0", Headers: make(map[string][]string)}
	req.Headers.Set("CSeq", "3")
	req.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=0-1")
	h.HandleSetup(req, session, "127.0.0.1:12345")

	if len(session.Tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(session.Tracks))
	}
	if session.Tracks[0].Codec != avframe.CodecH264 {
		t.Errorf("track 0 codec = %v, want H264", session.Tracks[0].Codec)
	}

	// Second track should get audio codec
	req2 := &Request{Method: "SETUP", URL: "rtsp://host/live/test/trackID=1", Headers: make(map[string][]string)}
	req2.Headers.Set("CSeq", "4")
	req2.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=2-3")
	h.HandleSetup(req2, session, "127.0.0.1:12345")

	if len(session.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(session.Tracks))
	}
	if session.Tracks[1].Codec != avframe.CodecAAC {
		t.Errorf("track 1 codec = %v, want AAC", session.Tracks[1].Codec)
	}
}

func TestHandleConnOptionsRequest(t *testing.T) {
	cfg := &config.Config{}
	cfg.RTSP.Enabled = true
	cfg.RTSP.Listen = "127.0.0.1:0"
	cfg.Stream.RingBufferSize = 256

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)
	if err := srv.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer srv.Shutdown()

	conn, err := net.DialTimeout("tcp", m.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Send OPTIONS request
	_, err = conn.Write([]byte("OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read response
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	resp := string(buf[:n])
	if !bytes.Contains([]byte(resp), []byte("RTSP/1.0 200 OK")) {
		t.Errorf("unexpected response: %s", resp)
	}
	// Public header may appear in the same or next read. Checking 200 OK is sufficient
	// to confirm handleConn dispatched OPTIONS correctly.
}

func TestHandleConnDescribeTeardown(t *testing.T) {
	cfg := &config.Config{}
	cfg.RTSP.Enabled = true
	cfg.RTSP.Listen = "127.0.0.1:0"
	cfg.Stream.RingBufferSize = 256

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)
	if err := srv.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer srv.Shutdown()

	conn, err := net.DialTimeout("tcp", m.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Use bufio.Scanner for line-based reading to handle TCP buffering
	reader := bufio.NewReader(conn)

	// Send DESCRIBE (stream doesn't exist, should get 404)
	_, err = conn.Write([]byte("DESCRIBE rtsp://localhost/live/test RTSP/1.0\r\nCSeq: 1\r\n\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read until we find the status line
	descResp, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("Read DESCRIBE: %v", err)
	}
	if !bytes.Contains([]byte(descResp), []byte("404")) {
		t.Errorf("expected 404 for non-existent stream: %s", descResp)
	}

	// Drain remaining DESCRIBE response headers until blank line
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("Read DESCRIBE header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Send TEARDOWN
	_, err = conn.Write([]byte("TEARDOWN rtsp://localhost/live/test RTSP/1.0\r\nCSeq: 2\r\n\r\n"))
	if err != nil {
		t.Fatalf("Write TEARDOWN: %v", err)
	}

	teardownResp, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("Read TEARDOWN: %v", err)
	}
	if !bytes.Contains([]byte(teardownResp), []byte("200")) {
		t.Errorf("expected 200 for TEARDOWN: %s", teardownResp)
	}
}

func TestCodecDefaultPTAdditional(t *testing.T) {
	tests := []struct {
		codec avframe.CodecType
		want  uint8
	}{
		{avframe.CodecVP8, 98},
		{avframe.CodecVP9, 99},
		{avframe.CodecAV1, 100},
		{avframe.CodecG722, 9},
		{avframe.CodecG729, 18},
		{avframe.CodecSpeex, 102},
		{avframe.CodecType(255), 96}, // unknown defaults to 96
	}
	for _, tt := range tests {
		got := codecDefaultPT(tt.codec)
		if got != tt.want {
			t.Errorf("codecDefaultPT(%v) = %d, want %d", tt.codec, got, tt.want)
		}
	}
}

func TestRTSPPublisherCloseIdempotent(t *testing.T) {
	pub := &RTSPPublisher{
		depacketizers: make(map[uint8]pkgrtp.Depacketizer),
		done:          make(chan struct{}),
	}
	if err := pub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should not panic
	if err := pub.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestRTSPSubscriberCloseIdempotent(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	sub, err := NewRTSPSubscriber("test-sub", info, &bytes.Buffer{}, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should not panic
	if err := sub.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestReadRequestMalformedHeader(t *testing.T) {
	raw := "OPTIONS * RTSP/1.0\r\n" +
		"malformed-no-colon\r\n" +
		"\r\n"
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	_, err := ReadRequest(r)
	if err == nil {
		t.Error("expected error for malformed header")
	}
}
