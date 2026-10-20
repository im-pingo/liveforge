package webrtc

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	rtpv1 "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func newWHIPBoundedInputPeer(t *testing.T) (*core.Stream, *webrtc.TrackLocalStaticRTP) {
	t.Helper()
	m, server := newTestModule(t)
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "bounded-whip")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, _, readErr := sender.ReadRTCP(); readErr != nil {
				return
			}
		}
	}()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	const streamKey = "live/whip-bounded-input"
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/"+streamKey, bytes.NewBufferString(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	response := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("WHIP status=%d: %s", response.Code, response.Body.String())
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: response.Body.String()}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, pc)
	stream, ok := server.StreamHub().Find(streamKey)
	if !ok {
		t.Fatal("WHIP did not create its stream")
	}
	return stream, track
}

func TestWHIPVideoReceiveDiscardsDamagedAccessUnits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packets []*rtpv1.Packet
	}{
		{"malformed packet", []*rtpv1.Packet{
			{Header: rtpv1.Header{SequenceNumber: 1, Timestamp: 100}, Payload: []byte{0x65, 0xaa}},
			{Header: rtpv1.Header{SequenceNumber: 2, Timestamp: 100}, Payload: []byte{0x78, 0, 1, 0x65, 0}},
			{Header: rtpv1.Header{SequenceNumber: 3, Timestamp: 100, Marker: true}, Payload: []byte{0x65, 0xbb}},
		}},
		{"timestamp transition", []*rtpv1.Packet{
			{Header: rtpv1.Header{SequenceNumber: 1, Timestamp: 100}, Payload: []byte{0x65, 0xaa}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, track := newWHIPBoundedInputPeer(t)
			reader := stream.RingBuffer().NewReaderAt(0)
			t.Cleanup(reader.Close)
			nextSequence := tc.packets[len(tc.packets)-1].SequenceNumber + 1
			packets := append(tc.packets, &rtpv1.Packet{Header: rtpv1.Header{SequenceNumber: nextSequence, Timestamp: 200, Marker: true}, Payload: []byte{0x65, 0xcc}})
			for _, packet := range packets {
				packet.Version = 2
				if err := track.WriteRTP(packet); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var received []*avframe.AVFrame
			for {
				frame, ok := reader.ReadContext(ctx)
				if !ok {
					t.Fatalf("clean recovery frame missing; received=%d", len(received))
				}
				received = append(received, frame)
				if bytes.Equal(frame.Payload, []byte{0, 0, 0, 2, 0x65, 0xcc}) {
					break
				}
			}
			if len(received) != 1 {
				t.Fatalf("published %d frames including a damaged access unit", len(received))
			}
		})
	}
}
