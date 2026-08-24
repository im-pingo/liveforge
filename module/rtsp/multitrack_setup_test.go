package rtsp

import (
	"net"
	"net/http"
	"strconv"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

func TestHandleMultiTrackSetupFromDescribedAndAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state SessionState
	}{
		{name: "describe-play", state: StateDescribed},
		{name: "announce-record", state: StateAnnounced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(nil, nil, nil)
			session := NewRTSPSession("test-id", "live/room1")
			if err := session.Transition(tc.state); err != nil {
				t.Fatalf("transition to setup state: %v", err)
			}
			session.MediaInfo = &avframe.MediaInfo{
				VideoCodec: avframe.CodecH264,
				AudioCodec: avframe.CodecAAC,
			}

			first := setupRequest("1", 0, "0-1")
			if resp := h.HandleSetup(first, session, "127.0.0.1:12345"); resp.StatusCode != 200 {
				t.Fatalf("first SETUP status = %d, want 200", resp.StatusCode)
			}

			second := setupRequest("2", 1, "2-3")
			if resp := h.HandleSetup(second, session, "127.0.0.1:12345"); resp.StatusCode != 200 {
				t.Fatalf("second SETUP status = %d, want 200", resp.StatusCode)
			}

			snapshot := session.Snapshot()
			if snapshot.State != StateReady {
				t.Fatalf("state = %d, want Ready", snapshot.State)
			}
			if len(snapshot.Tracks) != 2 {
				t.Fatalf("track count = %d, want 2", len(snapshot.Tracks))
			}
			if snapshot.Tracks[0].Codec != avframe.CodecH264 {
				t.Errorf("track 0 codec = %v, want H264", snapshot.Tracks[0].Codec)
			}
			if snapshot.Tracks[1].Codec != avframe.CodecAAC {
				t.Errorf("track 1 codec = %v, want AAC", snapshot.Tracks[1].Codec)
			}
		})
	}
}

func TestHandleMultiTrackInvalidSetupDoesNotMutate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state SessionState
	}{
		{name: "init", state: StateInit},
		{name: "playing", state: StatePlaying},
		{name: "recording", state: StateRecording},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(nil, nil, nil)
			session := NewRTSPSession("test-id", "live/room1")
			session.State = tc.state

			if resp := h.HandleSetup(setupRequest("1", 1, "2-3"), session, "127.0.0.1:12345"); resp.StatusCode != 455 {
				t.Fatalf("SETUP status = %d, want 455", resp.StatusCode)
			}

			snapshot := session.Snapshot()
			if snapshot.State != tc.state {
				t.Fatalf("state = %d, want %d", snapshot.State, tc.state)
			}
			if len(snapshot.Tracks) != 0 {
				t.Fatalf("rejected SETUP installed %d tracks, want 0", len(snapshot.Tracks))
			}
		})
	}
}

func TestHandleSetupRejectsDuplicateAndInvalidTrackIDs(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "duplicate", url: "rtsp://host/live/room1/trackID=0"},
		{name: "missing", url: "rtsp://host/live/room1"},
		{name: "invalid", url: "rtsp://host/live/room1/trackID=invalid"},
		{name: "trailing-junk", url: "rtsp://host/live/room1/trackID=1junk"},
		{name: "out-of-range", url: "rtsp://host/live/room1/trackID=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(nil, nil, nil)
			session := NewRTSPSession("test-id", "live/room1")
			session.State = StateDescribed
			session.MediaInfo = &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}
			if tt.name == "duplicate" {
				if response := h.HandleSetup(setupRequest("1", 0, "0-1"), session, "127.0.0.1:12345"); response.StatusCode != http.StatusOK {
					t.Fatalf("initial SETUP status = %d", response.StatusCode)
				}
			}

			req := &Request{Method: "SETUP", URL: tt.url, Headers: make(http.Header)}
			req.Headers.Set("CSeq", "2")
			req.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=2-3")
			response := h.HandleSetup(req, session, "127.0.0.1:12345")
			if response.StatusCode != 455 {
				t.Fatalf("SETUP status = %d, want 455", response.StatusCode)
			}
			wantTracks := 0
			if tt.name == "duplicate" {
				wantTracks = 1
			}
			if got := len(session.Snapshot().Tracks); got != wantTracks {
				t.Fatalf("rejected SETUP installed tracks = %d, want %d", got, wantTracks)
			}
		})
	}
}

func TestHandleSetupRejectsIneligibleTrackBeforeUDPAllocation(t *testing.T) {
	occupied := listenOnEvenUDPPort(t)
	port := occupied.LocalAddr().(*net.UDPAddr).Port
	allocator, err := portalloc.New(port, port+1)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, allocator, nil)
	session := NewRTSPSession("test-id", "live/room1")
	session.State = StateDescribed
	session.MediaInfo = &avframe.MediaInfo{VideoCodec: avframe.CodecH264}

	invalid := &Request{Method: "SETUP", URL: "rtsp://host/live/room1/trackID=1", Headers: make(http.Header)}
	invalid.Headers.Set("CSeq", "1")
	invalid.Headers.Set("Transport", "RTP/AVP;unicast;client_port=5000-5001")
	if response := h.HandleSetup(invalid, session, "127.0.0.1:12345"); response.StatusCode != 455 {
		t.Fatalf("ineligible SETUP status = %d, want 455 before occupied port allocation", response.StatusCode)
	}
	if got := len(session.Snapshot().Tracks); got != 0 {
		t.Fatalf("ineligible SETUP installed %d tracks", got)
	}

	if err := occupied.Close(); err != nil {
		t.Fatal(err)
	}
	valid := &Request{Method: "SETUP", URL: "rtsp://host/live/room1/trackID=0", Headers: make(http.Header)}
	valid.Headers.Set("CSeq", "2")
	valid.Headers.Set("Transport", "RTP/AVP;unicast;client_port=5000-5001")
	if response := h.HandleSetup(valid, session, "127.0.0.1:12345"); response.StatusCode != http.StatusOK {
		t.Fatalf("valid SETUP after rejection status = %d, want 200", response.StatusCode)
	}
	session.Close()
}

func listenOnEvenUDPPort(t *testing.T) *net.UDPConn {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		if port%2 == 0 && port < 65535 {
			return conn
		}
		_ = conn.Close()
	}
	t.Fatal("could not reserve an even UDP port")
	return nil
}

func setupRequest(cseq string, trackID int, interleaved string) *Request {
	req := &Request{
		Method:  "SETUP",
		URL:     "rtsp://host/live/room1/trackID=" + strconv.Itoa(trackID),
		Headers: make(map[string][]string),
	}
	req.Headers.Set("CSeq", cseq)
	req.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved="+interleaved)
	return req
}
