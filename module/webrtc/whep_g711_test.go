package webrtc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gb28181 "github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
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

func TestWHEPH264AndPCMACombinationNegotiates(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/h264-pcma")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&g711TestPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   buildTestAVCConfigPayload([]byte{0x67, 0x42, 0x00, 0x1f, 0xe9, 0x40}, []byte{0x68, 0xce, 0x38, 0x80}),
	})

	clientME := &webrtc.MediaEngine{}
	if err := clientME.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	clientPC, err := webrtc.NewAPI(webrtc.WithMediaEngine(clientME)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := clientPC.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/h264-pcma", bytes.NewReader([]byte(clientPC.LocalDescription().SDP)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP returned %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWHEPActualGBPublisherH264AndPCMANegotiates(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/gb-publisher")
	if err != nil {
		t.Fatal(err)
	}
	var publisher *gb28181.Publisher
	publisher = gb28181.NewPublisher("gb-publisher", func(frame *avframe.AVFrame) {
		stream.WriteFrameForPublisher(publisher, frame)
	})
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	muxer := ps.NewMuxer()
	feed := func(frame *avframe.AVFrame) {
		data, err := muxer.Pack(frame)
		if err != nil {
			t.Fatal(err)
		}
		publisher.FeedRTP(&pionrtp.Packet{Header: pionrtp.Header{Marker: true}, Payload: data})
	}
	feed(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0x1f, 0, 0, 0, 1, 0x68, 0xce, 0x38, 0x80, 0, 0, 0, 1, 0x65, 0x88}))
	feed(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe,
		0, 0, make([]byte, 160)))

	if got := stream.StartupSnapshot(); !got.Ready || got.MediaInfo.VideoCodec != avframe.CodecH264 || got.MediaInfo.AudioCodec != avframe.CodecG711A || got.MediaInfo.SampleRate != 8000 || got.MediaInfo.Channels != 1 {
		t.Fatalf("GB startup snapshot = ready=%v video=%v audio=%v sample_rate=%d channels=%d", got.Ready, got.MediaInfo.VideoCodec, got.MediaInfo.AudioCodec, got.MediaInfo.SampleRate, got.MediaInfo.Channels)
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
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := clientPC.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/gb-publisher", bytes.NewReader([]byte(clientPC.LocalDescription().SDP)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP returned %d: %s", rr.Code, rr.Body.String())
	}
}
