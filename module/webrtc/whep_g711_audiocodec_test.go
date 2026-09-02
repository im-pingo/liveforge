//go:build audiocodec

package webrtc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/webrtc/v4"
)

func TestWHEPAACTranscodeToPCMUUsesOfferClockRate(t *testing.T) {
	m, s := newTestModule(t)
	s.StreamHub().SetAudioCodecEnabled(true)
	stream, err := s.StreamHub().GetOrCreate("live/aac-pcmu")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&g711TestPublisher{info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecAAC,
		SampleRate: 44100,
		Channels:   2,
	}}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x12, 0x10},
	})

	clientME := &webrtc.MediaEngine{}
	if err := clientME.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
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
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-webrtc.GatheringCompletePromise(clientPC)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/aac-pcmu", bytes.NewReader([]byte(clientPC.LocalDescription().SDP)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP returned %d: %s", rr.Code, rr.Body.String())
	}
	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rr.Body.String()}
	if err := clientPC.SetRemoteDescription(answer); err != nil {
		t.Fatalf("SetRemoteDescription: %v\n%s", err, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "PCMU/8000") {
		t.Fatalf("WHEP answer omitted PCMU/8000: %s", rr.Body.String())
	}
}
