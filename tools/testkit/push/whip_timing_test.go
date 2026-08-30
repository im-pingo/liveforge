package push

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/tools/testkit/source"
	"github.com/pion/webrtc/v4"
)

func TestWHIPDirectOpusUsesPacketDurationsForRTPTimestamps(t *testing.T) {
	capture := newWHIPRTPCapture(t)
	src := &whipTimingSource{
		info: source.MediaInfo{AudioCodec: avframe.CodecOpus},
		frames: []*avframe.AVFrame{
			avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{0x00, 0x01}),
			avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 10, 10, []byte{0x10, 0x01}),
			avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 50, 50, []byte{0x80, 0x01}),
		},
		tailDelay: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (&whipPusher{}).Push(ctx, src, PushConfig{Protocol: "whip", Target: capture.URL()}); err != nil {
		t.Fatalf("WHIP push: %v", err)
	}

	packets := capture.Wait(t, 3)
	timestamps := []uint32{packets[0].Timestamp, packets[1].Timestamp, packets[2].Timestamp}
	want := []uint32{0, 480, 2400}
	if !slices.Equal(timestamps, want) {
		t.Fatalf("direct Opus RTP timestamps = %v, want packet-duration timeline %v", timestamps, want)
	}
}

func TestWHIPRealtimePacerRebasesAfterFallingBehind(t *testing.T) {
	pacer := whipRealtimePacer{enabled: true}
	ctx := context.Background()
	if err := pacer.Wait(ctx, 0); err != nil {
		t.Fatalf("initial pacer wait: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := pacer.Wait(ctx, 20*time.Millisecond); err != nil {
		t.Fatalf("late packet pacer wait: %v", err)
	}

	start := time.Now()
	if err := pacer.Wait(ctx, 40*time.Millisecond); err != nil {
		t.Fatalf("re-anchored packet pacer wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("re-anchored packet waited %s, want at least 15ms", elapsed)
	}
}

type whipCapturedRTP struct {
	Timestamp uint32
	Arrival   time.Time
	Payload   []byte
}

type whipRTPCapture struct {
	server  *httptest.Server
	packets chan whipCapturedRTP
	mu      sync.Mutex
	peers   []*webrtc.PeerConnection
}

func newWHIPRTPCapture(t *testing.T) *whipRTPCapture {
	t.Helper()
	capture := &whipRTPCapture{packets: make(chan whipCapturedRTP, 256)}
	capture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offer, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		capture.mu.Lock()
		capture.peers = append(capture.peers, pc)
		capture.mu.Unlock()
		pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			if track.Kind() != webrtc.RTPCodecTypeAudio {
				return
			}
			go func() {
				for {
					packet, _, readErr := track.ReadRTP()
					if readErr != nil {
						return
					}
					capture.packets <- whipCapturedRTP{Timestamp: packet.Timestamp, Arrival: time.Now(), Payload: append([]byte(nil), packet.Payload...)}
				}
			}()
		})
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		gathered := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(answer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		<-gathered
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(pc.LocalDescription().SDP))
	}))
	t.Cleanup(func() {
		capture.server.Close()
		capture.mu.Lock()
		defer capture.mu.Unlock()
		for _, pc := range capture.peers {
			_ = pc.Close()
		}
	})
	return capture
}

func (c *whipRTPCapture) URL() string {
	return c.server.URL + "/webrtc/whip/live/timing"
}

func (c *whipRTPCapture) Wait(t *testing.T, count int) []whipCapturedRTP {
	t.Helper()
	packets := make([]whipCapturedRTP, 0, count)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for len(packets) < count {
		select {
		case packet := <-c.packets:
			packets = append(packets, packet)
		case <-deadline.C:
			t.Fatalf("captured %d Opus RTP packets, want %d", len(packets), count)
		}
	}
	return packets
}

type whipTimingSource struct {
	info      source.MediaInfo
	frames    []*avframe.AVFrame
	index     int
	tailDelay time.Duration
}

func (s *whipTimingSource) NextFrame() (*avframe.AVFrame, error) {
	if s.index >= len(s.frames) {
		if s.tailDelay > 0 {
			time.Sleep(s.tailDelay)
			s.tailDelay = 0
		}
		return nil, io.EOF
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

func (s *whipTimingSource) MediaInfo() *source.MediaInfo { return &s.info }

func (s *whipTimingSource) Reset() {
	s.index = 0
}
