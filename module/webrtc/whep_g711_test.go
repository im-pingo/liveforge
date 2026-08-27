package webrtc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/webrtc/v4"
)

type g711TestPublisher struct {
	info *avframe.MediaInfo
}

func (p *g711TestPublisher) ID() string                    { return "g711-test-publisher" }
func (p *g711TestPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *g711TestPublisher) Close() error                  { return nil }

func TestWHEPPCMAudioPassthroughDeliversRTP(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/g711a")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&g711TestPublisher{info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatal(err)
	}

	clientME := &webrtc.MediaEngine{}
	if err := clientME.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	clientPC, err := webrtc.NewAPI(webrtc.WithMediaEngine(clientME)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()
	if _, err := clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var packets int
	clientPC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
			mu.Lock()
			packets++
			mu.Unlock()
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/g711a", bytes.NewReader([]byte(clientPC.LocalDescription().SDP)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP returned %d: %s", rr.Code, rr.Body.String())
	}
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  rr.Body.String(),
	}); err != nil {
		t.Fatal(err)
	}

	connected := make(chan struct{})
	clientPC.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
	})
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("ICE connection timed out: %s", clientPC.ICEConnectionState())
	}

	payload := make([]byte, 160)
	for i := range 30 {
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeAudio,
			Codec:     avframe.CodecG711A,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       int64(i * 20),
			PTS:       int64(i * 20),
			Payload:   payload,
		})
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := packets
		mu.Unlock()
		if got > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := packets
	mu.Unlock()
	t.Fatalf("received %d PCMA RTP packets, want at least one", got)
}
