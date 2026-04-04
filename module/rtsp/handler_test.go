package rtsp

import (
	"net/http"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/pkg/portalloc"
)

func TestHandleOptions(t *testing.T) {
	h := NewHandler(nil, nil)
	req := &Request{Method: "OPTIONS", URL: "*", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "1")
	resp := h.HandleOptions(req)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	public := resp.Headers.Get("Public")
	for _, method := range []string{"DESCRIBE", "SETUP", "PLAY", "PAUSE", "ANNOUNCE", "RECORD", "TEARDOWN", "GET_PARAMETER"} {
		if !strings.Contains(public, method) {
			t.Errorf("Public missing %s: %q", method, public)
		}
	}
	if resp.Headers.Get("CSeq") != "1" {
		t.Errorf("CSeq = %q", resp.Headers.Get("CSeq"))
	}
}

func TestHandleGetParameter(t *testing.T) {
	h := NewHandler(nil, nil)
	req := &Request{Method: "GET_PARAMETER", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "2")
	resp := h.HandleGetParameter(req)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
}

func TestHandleSetupTCPInterleaved(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	req := &Request{Method: "SETUP", URL: "rtsp://host/live/test/trackID=0", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "3")
	req.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=0-1")
	resp := h.HandleSetup(req, session, "127.0.0.1:12345")
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	transport := resp.Headers.Get("Transport")
	if !strings.Contains(transport, "interleaved=0-1") {
		t.Errorf("Transport = %q", transport)
	}
	if session.State != StateReady {
		t.Errorf("state = %d, want Ready", session.State)
	}
}

func TestHandleSetupUDP(t *testing.T) {
	pa, _ := portalloc.New(10000, 10010)
	h := NewHandler(nil, pa)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	req := &Request{Method: "SETUP", URL: "rtsp://host/live/test/trackID=0", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "4")
	req.Headers.Set("Transport", "RTP/AVP;unicast;client_port=5000-5001")
	resp := h.HandleSetup(req, session, "127.0.0.1:12345")
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	transport := resp.Headers.Get("Transport")
	if !strings.Contains(transport, "server_port=") {
		t.Errorf("Transport missing server_port: %q", transport)
	}
}

func TestHandleAnnounce(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	sdpBody := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"
	req := &Request{Method: "ANNOUNCE", URL: "rtsp://host/live/test", Headers: make(http.Header), Body: []byte(sdpBody)}
	req.Headers.Set("CSeq", "1")
	resp := h.HandleAnnounce(req, session, "127.0.0.1:12345")
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	if session.State != StateAnnounced {
		t.Errorf("state = %d, want Announced", session.State)
	}
}

func TestHandleAnnounceNoBody(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	req := &Request{Method: "ANNOUNCE", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "1")
	resp := h.HandleAnnounce(req, session, "127.0.0.1:12345")
	if resp.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", resp.StatusCode)
	}
}

func TestHandleRecord(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateAnnounced)
	session.Transition(StateReady)
	req := &Request{Method: "RECORD", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "5")
	resp := h.HandleRecord(req, session)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	if session.State != StateRecording {
		t.Errorf("state = %d, want Recording", session.State)
	}
}

func TestHandleRecordInvalidState(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	// Init state -> cannot RECORD directly
	req := &Request{Method: "RECORD", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "5")
	resp := h.HandleRecord(req, session)
	if resp.StatusCode != 455 {
		t.Errorf("StatusCode = %d, want 455", resp.StatusCode)
	}
}

func TestHandlePlay(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	session.Transition(StateReady)
	req := &Request{Method: "PLAY", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "6")
	resp := h.HandlePlay(req, session, "127.0.0.1:12345")
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	if session.State != StatePlaying {
		t.Errorf("state = %d, want Playing", session.State)
	}
	if resp.Headers.Get("RTP-Info") == "" {
		t.Error("missing RTP-Info header")
	}
}

func TestHandlePause(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	session.Transition(StateReady)
	session.Transition(StatePlaying)
	req := &Request{Method: "PAUSE", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "7")
	resp := h.HandlePause(req, session)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	if session.State != StateReady {
		t.Errorf("state = %d, want Ready", session.State)
	}
}

func TestHandleTeardown(t *testing.T) {
	h := NewHandler(nil, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.Transition(StateDescribed)
	session.Transition(StateReady)
	session.Transition(StatePlaying)
	req := &Request{Method: "TEARDOWN", URL: "rtsp://host/live/test", Headers: make(http.Header)}
	req.Headers.Set("CSeq", "8")
	resp := h.HandleTeardown(req, session)
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
	if session.State != StateClosed {
		t.Errorf("state = %d, want Closed", session.State)
	}
}

func TestExtractStreamKey(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"rtsp://host/live/test", "live/test"},
		{"rtsp://host:554/live/test", "live/test"},
		{"rtsp://host/live/test/trackID=0", "live/test"},
		{"rtsp://host:8554/app/stream1", "app/stream1"},
	}
	for _, tt := range tests {
		got := extractStreamKey(tt.url)
		if got != tt.want {
			t.Errorf("extractStreamKey(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestParseTransportHeader(t *testing.T) {
	tc := parseTransportHeader("RTP/AVP/TCP;unicast;interleaved=0-1")
	if !tc.IsTCP {
		t.Error("expected TCP")
	}
	if tc.Interleaved[0] != 0 || tc.Interleaved[1] != 1 {
		t.Errorf("Interleaved = %v", tc.Interleaved)
	}

	tc2 := parseTransportHeader("RTP/AVP;unicast;client_port=5000-5001")
	if tc2.IsTCP {
		t.Error("expected UDP")
	}
	if tc2.ClientPorts[0] != 5000 || tc2.ClientPorts[1] != 5001 {
		t.Errorf("ClientPorts = %v", tc2.ClientPorts)
	}
}
