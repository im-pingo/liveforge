package rtsp

import (
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
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

func TestHandleSetupRepeatedDuplicateUDPDoesNotConsumeAllocator(t *testing.T) {
	basePort, releaseReservation := reserveContiguousUDPPorts(t, 2)
	releaseReservation()
	allocator, err := portalloc.New(basePort, basePort+1)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, allocator, nil)
	session := NewRTSPSession("duplicate-udp", "live/room1")
	session.State = StateDescribed
	session.MediaInfo = &avframe.MediaInfo{VideoCodec: avframe.CodecH264}

	if response := h.HandleSetup(udpSetupRequest("1", 0), session, "127.0.0.1:12345"); response.StatusCode != http.StatusOK {
		t.Fatalf("initial SETUP status = %d, want 200", response.StatusCode)
	}
	for attempt := 0; attempt < 3; attempt++ {
		response := h.HandleSetup(udpSetupRequest(strconv.Itoa(attempt+2), 0), session, "127.0.0.1:12345")
		if response.StatusCode != 455 {
			t.Fatalf("duplicate SETUP %d status = %d, want 455 without allocator access", attempt+1, response.StatusCode)
		}
		if got := len(session.Snapshot().Tracks); got != 1 {
			t.Fatalf("duplicate SETUP %d installed %d tracks, want 1", attempt+1, got)
		}
	}

	session.Close()
	probe, err := NewUDPTransport(allocator)
	if err != nil {
		t.Fatalf("allocator/socket pair was not reusable after session close: %v", err)
	}
	probe.Close()
}

func TestHandleSetupConcurrentInstallLoserClosesUDPTransport(t *testing.T) {
	basePort, releaseReservation := reserveContiguousUDPPorts(t, 4)
	releaseReservation()
	allocator, err := portalloc.New(basePort, basePort+3)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, allocator, nil)
	session := NewRTSPSession("concurrent-udp", "live/room1")
	session.State = StateDescribed
	session.MediaInfo = &avframe.MediaInfo{VideoCodec: avframe.CodecH264}

	unlockAllocator := lockPortAllocator(t, allocator)
	allocatorLocked := true
	defer func() {
		if allocatorLocked {
			unlockAllocator()
		}
	}()

	responses := make(chan *Response, 2)
	for i := 0; i < 2; i++ {
		go func(cseq string) {
			responses <- h.HandleSetup(udpSetupRequest(cseq, 0), session, "127.0.0.1:12345")
		}(strconv.Itoa(i + 1))
	}
	waitForBlockedPortAllocations(t, 2)
	unlockAllocator()
	allocatorLocked = false

	statusCounts := map[int]int{}
	for i := 0; i < 2; i++ {
		select {
		case response := <-responses:
			statusCounts[response.StatusCode]++
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent SETUP did not complete")
		}
	}
	if statusCounts[http.StatusOK] != 1 || statusCounts[455] != 1 {
		t.Fatalf("concurrent SETUP statuses = %v, want one 200 and one 455", statusCounts)
	}
	snapshot := session.Snapshot()
	if len(snapshot.Tracks) != 1 || snapshot.Tracks[0].UDP == nil {
		t.Fatalf("installed tracks = %+v, want one UDP track", snapshot.Tracks)
	}

	probe, err := NewUDPTransport(allocator)
	if err != nil {
		t.Fatalf("losing UDP allocation or sockets were not released: %v", err)
	}
	probe.Close()
	session.Close()
}

func TestHandleMultiTrackPublishCompletesRecord(t *testing.T) {
	server := core.NewServer(config.Defaults())
	h := NewHandler(server, nil, nil)
	session := NewRTSPSession("publish-two-track", "live/room1")
	t.Cleanup(func() { session.Close() })

	announce := &Request{
		Method:  "ANNOUNCE",
		URL:     "rtsp://host/live/room1",
		Headers: make(http.Header),
		Body: []byte("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n" +
			"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n" +
			"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/48000/2\r\n"),
	}
	announce.Headers.Set("CSeq", "1")
	if response := h.HandleAnnounce(announce, session, "127.0.0.1:12345"); response.StatusCode != http.StatusOK {
		t.Fatalf("ANNOUNCE status = %d, want 200", response.StatusCode)
	}
	setupTwoTCPTracks(t, h, session)

	record := &Request{Method: "RECORD", URL: announce.URL, Headers: make(http.Header)}
	record.Headers.Set("CSeq", "4")
	if response := h.HandleRecord(record, session); response.StatusCode != http.StatusOK {
		t.Fatalf("RECORD status = %d, want 200", response.StatusCode)
	}
	assertTwoTCPTracks(t, session.Snapshot(), StateRecording)
}

func TestHandleMultiTrackPlaybackCompletesPlay(t *testing.T) {
	server := core.NewServer(config.Defaults())
	stream, err := server.StreamHub().GetOrCreate("live/room1")
	if err != nil {
		t.Fatal(err)
	}
	mediaInfo := &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		AudioCodec:          avframe.CodecAAC,
		VideoSequenceHeader: []byte{0x01, 0x64},
		AudioSequenceHeader: []byte{0x12, 0x10},
		SampleRate:          48000,
		Channels:            2,
	}
	publisher, err := NewRTSPPublisher("playback-source", mediaInfo, stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stream.RemovePublisherIf(publisher)
		_ = publisher.Close()
	})

	h := NewHandler(server, nil, nil)
	session := NewRTSPSession("play-two-track", "live/room1")
	t.Cleanup(func() { session.Close() })
	describe := &Request{Method: "DESCRIBE", URL: "rtsp://host/live/room1", Headers: make(http.Header)}
	describe.Headers.Set("CSeq", "1")
	if response := h.HandleDescribe(describe, session); response.StatusCode != http.StatusOK {
		t.Fatalf("DESCRIBE status = %d, want 200", response.StatusCode)
	}
	setupTwoTCPTracks(t, h, session)

	play := &Request{Method: "PLAY", URL: describe.URL, Headers: make(http.Header)}
	play.Headers.Set("CSeq", "4")
	if response := h.HandlePlay(play, session, "127.0.0.1:12345"); response.StatusCode != http.StatusOK {
		t.Fatalf("PLAY status = %d, want 200", response.StatusCode)
	}
	assertTwoTCPTracks(t, session.Snapshot(), StatePlaying)
}

func setupTwoTCPTracks(t *testing.T, h *Handler, session *RTSPSession) {
	t.Helper()
	for trackID, channels := range []string{"0-1", "2-3"} {
		if response := h.HandleSetup(setupRequest(strconv.Itoa(trackID+2), trackID, channels), session, "127.0.0.1:12345"); response.StatusCode != http.StatusOK {
			t.Fatalf("track %d SETUP status = %d, want 200", trackID, response.StatusCode)
		}
	}
}

func assertTwoTCPTracks(t *testing.T, snapshot RTSPSessionSnapshot, wantState SessionState) {
	t.Helper()
	if snapshot.State != wantState {
		t.Fatalf("state = %d, want %d", snapshot.State, wantState)
	}
	if len(snapshot.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(snapshot.Tracks))
	}
	wantCodecs := []avframe.CodecType{avframe.CodecH264, avframe.CodecAAC}
	wantChannels := [][2]int{{0, 1}, {2, 3}}
	for i, track := range snapshot.Tracks {
		if track.TrackID != i || track.Codec != wantCodecs[i] {
			t.Errorf("track %d identity/codec = %d/%v, want %d/%v", i, track.TrackID, track.Codec, i, wantCodecs[i])
		}
		if !track.Transport.IsTCP || track.Transport.Interleaved != wantChannels[i] {
			t.Errorf("track %d transport = %+v, want TCP channels %v", i, track.Transport, wantChannels[i])
		}
		if track.UDP != nil || track.Multicast != nil {
			t.Errorf("track %d retained unexpected UDP/multicast resources", i)
		}
	}
}

func udpSetupRequest(cseq string, trackID int) *Request {
	req := &Request{
		Method:  "SETUP",
		URL:     "rtsp://host/live/room1/trackID=" + strconv.Itoa(trackID),
		Headers: make(http.Header),
	}
	req.Headers.Set("CSeq", cseq)
	req.Headers.Set("Transport", "RTP/AVP;unicast;client_port=5000-5001")
	return req
}

func reserveContiguousUDPPorts(t *testing.T, count int) (int, func()) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		first, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		basePort := first.LocalAddr().(*net.UDPAddr).Port
		if basePort%2 != 0 || basePort+count-1 > 65535 {
			_ = first.Close()
			continue
		}
		connections := []*net.UDPConn{first}
		available := true
		for port := basePort + 1; port < basePort+count; port++ {
			conn, listenErr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
			if listenErr != nil {
				available = false
				break
			}
			connections = append(connections, conn)
		}
		if !available {
			for _, conn := range connections {
				_ = conn.Close()
			}
			continue
		}
		return basePort, func() {
			for _, conn := range connections {
				_ = conn.Close()
			}
		}
	}
	t.Fatal("could not reserve contiguous even-start UDP ports")
	return 0, func() {}
}

func lockPortAllocator(t *testing.T, allocator *portalloc.PortAllocator) func() {
	t.Helper()
	mutexField := reflect.ValueOf(allocator).Elem().FieldByName("mu")
	if !mutexField.IsValid() || !mutexField.CanAddr() {
		t.Fatal("port allocator mutex is unavailable")
	}
	// The test holds the real allocator lock to pause requests after validation.
	mutex := (*sync.Mutex)(unsafe.Pointer(mutexField.UnsafeAddr()))
	mutex.Lock()
	return mutex.Unlock
}

func waitForBlockedPortAllocations(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	stack := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(stack, true)
		if strings.Count(string(stack[:n]), "portalloc.(*PortAllocator).AllocatePair") >= want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("did not observe %d SETUP requests blocked after pre-allocation validation", want)
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
