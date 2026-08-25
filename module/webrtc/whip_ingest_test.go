package webrtc

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestWHIPOpusIngestWritesEveryPacketWithoutMarker(t *testing.T) {
	m, server := newTestModule(t)
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientPC.Close() })

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio",
		"whip-opus-no-marker",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientPC.AddTrack(track); err != nil {
		t.Fatal(err)
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

	const streamKey = "live/whip-opus-no-marker"
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/"+streamKey, bytes.NewBufferString(clientPC.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHIP status = %d, want 201: %s", rr.Code, rr.Body.String())
	}

	connected := make(chan struct{})
	var connectedOnce sync.Once
	clientPC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
	})
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rr.Body.String()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("WHIP client state = %s, want connected", clientPC.ConnectionState())
	}

	for i := range 3 {
		if err := track.WriteRTP(&pionrtp.Packet{
			Header: pionrtp.Header{
				Version:        2,
				Marker:         false,
				SequenceNumber: uint16(i + 1),
				Timestamp:      uint32(i * 960),
			},
			Payload: []byte{0xF8, 0xFF, 0xFE},
		}); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream, ok := server.StreamHub().Find(streamKey)
		if ok && stream.Stats().AudioFrames == 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	stream, ok := server.StreamHub().Find(streamKey)
	if !ok {
		t.Fatalf("stream %q was not created", streamKey)
	}
	if got := stream.Stats().AudioFrames; got != 3 {
		t.Fatalf("audio frames = %d, want 3; Opus packets must not depend on RTP Marker", got)
	}
}

func TestWHIPVideoIngestRequestsPublisherKeyframe(t *testing.T) {
	m, _ := newTestModule(t)
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientPC.Close() })

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video",
		"whip-keyframe-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := clientPC.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}

	pliReceived := make(chan struct{}, 1)
	go func() {
		for {
			packets, _, readErr := sender.ReadRTCP()
			if readErr != nil {
				return
			}
			for _, packet := range packets {
				if _, ok := packet.(*rtcp.PictureLossIndication); ok {
					select {
					case pliReceived <- struct{}{}:
					default:
					}
					return
				}
			}
		}
	}()

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/whip-keyframe-request", bytes.NewBufferString(clientPC.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHIP status = %d, want 201: %s", rr.Code, rr.Body.String())
	}

	connected := make(chan struct{})
	var connectedOnce sync.Once
	clientPC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
	})
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rr.Body.String()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("WHIP client state = %s, want connected", clientPC.ConnectionState())
	}

	if err := track.WriteRTP(&pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			Marker:         true,
			SequenceNumber: 1,
			Timestamp:      3000,
		},
		Payload: []byte{0x65, 0x88, 0x84},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-pliReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("WHIP publisher did not receive a PLI keyframe request")
	}
}

func TestWHIPH265MixedAggregationPacketStoresSequenceHeaderAndKeyframe(t *testing.T) {
	m, server := newTestModule(t)
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, ClockRate: 90000},
		PayloadType:        116,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	clientPC, err := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientPC.Close() })

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, ClockRate: 90000},
		"video",
		"whip-h265-mixed-ap",
	)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := clientPC.AddTrack(track)
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

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone

	const streamKey = "live/whip-h265-mixed-ap"
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/"+streamKey, bytes.NewBufferString(clientPC.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHIP status = %d, want 201: %s", rr.Code, rr.Body.String())
	}

	connected := make(chan struct{})
	var connectedOnce sync.Once
	clientPC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
	})
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rr.Body.String()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("WHIP client state = %s, want connected", clientPC.ConnectionState())
	}

	sps, err := hex.DecodeString("420103016000000300b0000003000003005a0000a0050201e162023b914842e7e13d0bea1bd50feaa08f554a6a02020207f08041")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{byte(48 << 1), 0x01}
	for _, nal := range [][]byte{
		{0x40, 0x01, 0x0C, 0x01},
		sps,
		{0x44, 0x01, 0xC0, 0xF7},
		{0x46, 0x01, 0x10},
		{0x4E, 0x01, 0x80},
		{0x26, 0x01, 0xAA, 0xBB},
	} {
		payload = append(payload, byte(len(nal)>>8), byte(len(nal)))
		payload = append(payload, nal...)
	}
	if err := track.WriteRTP(&pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, Marker: true, SequenceNumber: 1, Timestamp: 9000},
		Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream, ok := server.StreamHub().Find(streamKey)
		if ok && stream.VideoSeqHeader() != nil && stream.GOPCacheLen() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	stream, ok := server.StreamHub().Find(streamKey)
	if !ok {
		t.Fatalf("stream %q was not created", streamKey)
	}
	if stream.VideoSeqHeader() == nil {
		t.Error("mixed H.265 AP did not store the sequence header")
	}
	if stream.GOPCacheLen() == 0 {
		t.Error("mixed H.265 AP did not store the keyframe")
	}
}
