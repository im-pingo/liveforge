package rtsp

import (
	"strconv"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
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
