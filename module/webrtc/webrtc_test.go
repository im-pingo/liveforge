package webrtc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

func newTestServer(t *testing.T) *core.Server {
	t.Helper()
	cfg := &config.Config{
		Stream: config.StreamConfig{
			RingBufferSize:     256,
			GOPCache:           true,
			GOPCacheNum:        1,
			IdleTimeout:        30 * time.Second,
			NoPublisherTimeout: 15 * time.Second,
		},
		WebRTC: config.WebRTCConfig{
			Enabled:      true,
			Listen:       ":0",
			UDPPortRange: []int{20000, 20100},
		},
	}
	return core.NewServer(cfg)
}

func newTestModule(t *testing.T) (*Module, *core.Server) {
	t.Helper()
	s := newTestServer(t)
	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m, s
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "webrtc" {
		t.Errorf("expected 'webrtc', got %q", m.Name())
	}
}

func TestModuleInitAndClose(t *testing.T) {
	m, _ := newTestModule(t)
	if m.Addr() == nil {
		t.Error("expected non-nil listener address")
	}
}

func TestCORSHeaders(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodOptions, "/webrtc/whip/live/test", nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("expected CORS Allow-Origin: *")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("expected POST in CORS Allow-Methods")
	}
}

func TestWHIPBadContentType(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/test", strings.NewReader("not sdp"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", rr.Code)
	}
}

func TestWHEPStreamNotFound(t *testing.T) {
	m, _ := newTestModule(t)

	// Create a minimal SDP offer.
	offer := createMinimalOffer(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/nonexistent", bytes.NewReader([]byte(offer)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeleteSessionNotFound(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodDelete, "/webrtc/session/nonexistent-id", nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestMimeToCodecType(t *testing.T) {
	tests := []struct {
		mime string
		want avframe.CodecType
	}{
		{"video/H264", avframe.CodecH264},
		{"video/VP8", avframe.CodecVP8},
		{"video/VP9", avframe.CodecVP9},
		{"video/AV1", avframe.CodecAV1},
		{"audio/opus", avframe.CodecOpus},
		{"audio/PCMU", avframe.CodecG711U},
		{"audio/PCMA", avframe.CodecG711A},
		{"video/unknown", 0},
	}

	for _, tt := range tests {
		got := mimeToCodecType(tt.mime)
		if got != tt.want {
			t.Errorf("mimeToCodecType(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}

func TestCodecToMime(t *testing.T) {
	tests := []struct {
		codec avframe.CodecType
		want  string
	}{
		{avframe.CodecH264, webrtc.MimeTypeH264},
		{avframe.CodecOpus, webrtc.MimeTypeOpus},
		{avframe.CodecVP8, webrtc.MimeTypeVP8},
		{avframe.CodecG711U, webrtc.MimeTypePCMU},
		{0, ""},
	}

	for _, tt := range tests {
		got := codecToMime(tt.codec)
		if got != tt.want {
			t.Errorf("codecToMime(%v) = %q, want %q", tt.codec, got, tt.want)
		}
	}
}

func TestSessionStore(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	sess := newSession("test-id", pc, "live/test", "whip", m)
	m.storeSession(sess)

	found, ok := m.findSession("test-id")
	if !ok || found.id != "test-id" {
		t.Error("expected to find stored session")
	}

	sess.Close()

	_, ok = m.findSession("test-id")
	if ok {
		t.Error("expected session to be removed after Close")
	}
}

// TestTrackSenderPLIHandler verifies that TrackSender dispatches PLI/FIR
// RTCP packets to the registered handler, independent of protocol code.
func TestTrackSenderPLIHandler(t *testing.T) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	// Create sender PC (simulates server sending video).
	senderPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer senderPC.Close()

	// Create receiver PC (simulates browser receiving video).
	receiverPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer receiverPC.Close()

	// Add a video track to the sender.
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	rtpSender, err := senderPC.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}

	// Wrap in TrackSender and register PLI handler.
	var pliCount atomic.Int32
	ts := NewTrackSender("test-session", track, rtpSender)
	ts.SetPLIHandler(func() { pliCount.Add(1) })
	ts.Start()

	// Perform SDP exchange to connect the two PCs.
	offer, err := senderPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(senderPC)
	if err := senderPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone
	if err := receiverPC.SetRemoteDescription(*senderPC.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := receiverPC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone2 := webrtc.GatheringCompletePromise(receiverPC)
	if err := receiverPC.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone2
	if err := senderPC.SetRemoteDescription(*receiverPC.LocalDescription()); err != nil {
		t.Fatal(err)
	}

	// Wait for full connection (ICE + DTLS) on receiver side.
	connected := make(chan struct{})
	receiverPC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
	})
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("PeerConnection connection timed out")
	}

	// Find receiver's track SSRC to send a PLI.
	var mediaSSRC uint32
	for _, tr := range receiverPC.GetTransceivers() {
		if tr.Receiver() != nil {
			if track := tr.Receiver().Track(); track != nil {
				mediaSSRC = uint32(track.SSRC())
				break
			}
		}
	}
	if mediaSSRC == 0 {
		t.Skip("could not determine SSRC for PLI test")
	}

	// Send PLI from receiver to sender.
	if err := receiverPC.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: mediaSSRC},
	}); err != nil {
		t.Fatalf("WriteRTCP PLI: %v", err)
	}

	// Give the RTCP goroutine time to process.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pliCount.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if pliCount.Load() == 0 {
		t.Error("PLI handler was not called after sending PLI RTCP packet")
	}
}

// createMinimalOffer creates a minimal SDP offer string for testing.
func createMinimalOffer(t *testing.T) string {
	t.Helper()
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}
