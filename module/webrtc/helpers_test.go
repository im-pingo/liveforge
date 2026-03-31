package webrtc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

func TestModuleHooks(t *testing.T) {
	m := NewModule()
	if hooks := m.Hooks(); hooks != nil {
		t.Errorf("Hooks should be nil, got %v", hooks)
	}
}

func TestModuleAddrNil(t *testing.T) {
	m := NewModule()
	if m.Addr() != nil {
		t.Error("expected nil Addr before Init")
	}
}

func TestHandlePatchNotFound(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPatch, "/webrtc/session/nonexistent-id", nil)
	req.Header.Set("Content-Type", "application/trickle-ice-sdpfrag")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestHandlePatchBadContentType(t *testing.T) {
	m, s := newTestModule(t)

	// Create a session first
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	sess := newSession("test-patch-id", pc, "live/test", "whep", m)
	m.storeSession(sess)
	defer sess.Close()

	// PATCH with wrong content type
	req := httptest.NewRequest(http.MethodPatch, "/webrtc/session/test-patch-id", strings.NewReader("candidate"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", rr.Code)
	}
	_ = s
}

func TestHandlePatchSuccess(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	sess := newSession("test-patch-ok", pc, "live/test", "whep", m)
	m.storeSession(sess)
	defer sess.Close()

	req := httptest.NewRequest(http.MethodPatch, "/webrtc/session/test-patch-ok", strings.NewReader("candidate"))
	req.Header.Set("Content-Type", "application/trickle-ice-sdpfrag")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rr.Code)
	}
}

func TestHandleDeleteSuccess(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	sess := newSession("test-del-id", pc, "live/test", "whep", m)
	m.storeSession(sess)

	req := httptest.NewRequest(http.MethodDelete, "/webrtc/session/test-del-id", nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	// Session should be removed
	_, ok := m.findSession("test-del-id")
	if ok {
		t.Error("session should be removed after DELETE")
	}
}

func TestIceServersFromConfig(t *testing.T) {
	cfg := &config.Config{
		Stream: config.StreamConfig{RingBufferSize: 256},
		WebRTC: config.WebRTCConfig{
			Enabled: true,
			Listen:  ":0",
			ICEServers: []config.ICEServer{
				{
					URLs:       []string{"stun:stun.example.com:3478"},
					Username:   "user",
					Credential: "pass",
				},
				{
					URLs: []string{"turn:turn.example.com:3478"},
				},
			},
		},
	}

	s := newTestServer(t)
	s = nil // We need a server with ICE config

	// Create module with ICE config
	srv := newServerWithConfig(t, cfg)
	m := NewModule()
	if err := m.Init(srv); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer m.Close()

	servers := m.iceServersFromConfig()
	if len(servers) != 2 {
		t.Fatalf("expected 2 ICE servers, got %d", len(servers))
	}
	if servers[0].URLs[0] != "stun:stun.example.com:3478" {
		t.Errorf("URL = %q", servers[0].URLs[0])
	}
	if servers[0].Username != "user" {
		t.Errorf("Username = %q", servers[0].Username)
	}
	_ = s
}

func newServerWithConfig(t *testing.T, cfg *config.Config) *core.Server {
	t.Helper()
	return core.NewServer(cfg)
}

func TestWHIPMissingStreamKey(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/", strings.NewReader("sdp"))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	// Go 1.22+ path routing: empty path should return 400 or 404
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
		t.Errorf("expected 400 or 404, got %d", rr.Code)
	}
}

func TestWHEPBadContentType(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/test", strings.NewReader("not sdp"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType && rr.Code != http.StatusNotFound {
		t.Errorf("expected 415 or 404, got %d", rr.Code)
	}
}

func TestWHIPBadSDP(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/test", strings.NewReader("not valid sdp"))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad SDP, got %d", rr.Code)
	}
}

func TestWHIPWithValidOffer(t *testing.T) {
	m, _ := newTestModule(t)

	offer := createMinimalPublishOffer(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/whiptest", bytes.NewReader([]byte(offer)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Should have Location header
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/webrtc/session/") {
		t.Errorf("Location = %q", loc)
	}

	// Content-Type should be application/sdp
	if !strings.Contains(rr.Header().Get("Content-Type"), "application/sdp") {
		t.Errorf("Content-Type = %q", rr.Header().Get("Content-Type"))
	}
}

func TestWHIPPublisherMethods(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	pub := &WHIPPublisher{
		id:   "test-whip-pub",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
		pc:   pc,
		done: make(chan struct{}),
	}

	if pub.ID() != "test-whip-pub" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo().VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v", pub.MediaInfo().VideoCodec)
	}

	// Close should work
	if err := pub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should not panic
	if err := pub.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestTrackSenderTrackMethod(t *testing.T) {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := &TrackSender{track: track}
	if ts.Track() != track {
		t.Error("Track() should return the underlying track")
	}
}

func TestTrackSenderSetHandlers(t *testing.T) {
	ts := &TrackSender{}

	// SetNACKHandler
	ts.SetNACKHandler(func(nack *rtcp.TransportLayerNack) {})
	if ts.onNACK == nil {
		t.Error("onNACK should not be nil after SetNACKHandler")
	}

	// SetREMBHandler
	ts.SetREMBHandler(func(bitrate uint64, ssrcs []uint32) {})
	if ts.onREMB == nil {
		t.Error("onREMB should not be nil after SetREMBHandler")
	}
}

func TestRegisterCodecs(t *testing.T) {
	me := &webrtc.MediaEngine{}
	err := registerCodecs(me)
	if err != nil {
		t.Fatalf("registerCodecs: %v", err)
	}
}

func TestCorsMiddleware(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS Allow-Origin")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("missing POST in Allow-Methods")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Expose-Headers"), "Location") {
		t.Error("missing Location in Expose-Headers")
	}
}

// createMinimalPublishOffer creates a minimal SDP offer with a sendonly video track.
func createMinimalPublishOffer(t *testing.T) string {
	t.Helper()
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	pc.AddTrack(track)

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}
