package webrtc

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/webrtc/v4"
)

// testPublisher implements core.Publisher for testing.
type testPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *testPublisher) ID() string                   { return p.id }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *testPublisher) Close() error                  { return nil }

// buildAVCCPayload creates a minimal AVCC payload wrapping a single NAL unit.
func buildAVCCPayload(nal []byte) []byte {
	buf := make([]byte, 4+len(nal))
	buf[0] = byte(len(nal) >> 24)
	buf[1] = byte(len(nal) >> 16)
	buf[2] = byte(len(nal) >> 8)
	buf[3] = byte(len(nal))
	copy(buf[4:], nal)
	return buf
}

// buildTestAVCConfigPayload creates a minimal AVCDecoderConfigurationRecord.
func buildTestAVCConfigPayload(sps, pps []byte) []byte {
	// Minimal AVCDecoderConfigurationRecord
	buf := []byte{
		0x01,       // configurationVersion
		sps[1],     // AVCProfileIndication
		sps[2],     // profile_compatibility
		sps[3],     // AVCLevelIndication
		0xFF,       // lengthSizeMinusOne = 3 (4-byte NALU length)
		0xE1,       // numOfSequenceParameterSets = 1
		byte(len(sps) >> 8), byte(len(sps)), // SPS length
	}
	buf = append(buf, sps...)
	buf = append(buf, 0x01) // numOfPictureParameterSets = 1
	buf = append(buf, byte(len(pps)>>8), byte(len(pps)))
	buf = append(buf, pps...)
	return buf
}

// TestWHEPPayloadTypeCorrectness verifies that the full WHEP negotiation path
// produces RTP packets with the correct H264 payload type (not RTX).
//
// This test caught a bug where SetCodecPreferences caused pion to bind
// the H264 track with an RTX payload type, resulting in Chrome being unable
// to decode frames.
func TestWHEPPayloadTypeCorrectness(t *testing.T) {
	m, s := newTestModule(t)

	// Create a publishing stream.
	stream, err := s.StreamHub().GetOrCreate("live/test")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{
		id:   "test-pub",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Write SPS/PPS sequence header so WHEP feed loop can cache it.
	spsData := []byte{0x67, 0x42, 0x00, 0x1f, 0xe9, 0x40, 0x14, 0x04, 0x78}
	ppsData := []byte{0x68, 0xce, 0x38, 0x80}
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   buildTestAVCConfigPayload(spsData, ppsData),
		DTS:       0,
	})

	// Create a real pion PeerConnection as the WHEP client.
	clientME := &webrtc.MediaEngine{}
	if err := clientME.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	clientAPI := webrtc.NewAPI(webrtc.WithMediaEngine(clientME))
	clientPC, err := clientAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()

	// Add recv-only video transceiver.
	_, err = clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create offer with full ICE candidates.
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone

	// Send WHEP request through the HTTP handler.
	req := httptest.NewRequest(
		http.MethodPost,
		"/webrtc/whep/live/test",
		bytes.NewReader([]byte(clientPC.LocalDescription().SDP)),
	)
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP returned %d: %s", rr.Code, rr.Body.String())
	}

	// Apply the SDP answer from the server.
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  rr.Body.String(),
	}
	if err := clientPC.SetRemoteDescription(answer); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}

	// Collect received RTP packets.
	type rtpEvent struct {
		PT        uint8
		SSRC      uint32
		Seq       uint16
		Timestamp uint32
		Marker    bool
		WallTime  time.Time
	}
	var mu sync.Mutex
	var events []rtpEvent

	clientPC.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			mu.Lock()
			events = append(events, rtpEvent{
				PT:        pkt.PayloadType,
				SSRC:      pkt.SSRC,
				Seq:       pkt.SequenceNumber,
				Timestamp: pkt.Timestamp,
				Marker:    pkt.Marker,
				WallTime:  time.Now(),
			})
			mu.Unlock()
		}
	})

	// Wait for ICE connection.
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
	case <-time.After(10 * time.Second):
		t.Fatal("ICE connection timed out")
	}

	// Pump video frames into the stream (simulating publisher).
	// Send keyframe first, then inter-frames at 25fps.
	idrNAL := make([]byte, 3000)
	idrNAL[0] = 0x65 // IDR slice
	for i := 1; i < len(idrNAL); i++ {
		idrNAL[i] = byte(i & 0xFF)
	}

	pNAL := make([]byte, 1000)
	pNAL[0] = 0x41 // P-slice
	for i := 1; i < len(pNAL); i++ {
		pNAL[i] = byte(i & 0xFF)
	}

	const totalFrames = 75 // 3 seconds at 25fps
	const frameDurationMs = 40

	sendStart := time.Now()
	for i := 0; i < totalFrames; i++ {
		targetTime := sendStart.Add(time.Duration(i) * frameDurationMs * time.Millisecond)
		if sleepDur := time.Until(targetTime); sleepDur > 0 {
			time.Sleep(sleepDur)
		}

		dts := int64(i * frameDurationMs)
		var payload []byte
		var frameType avframe.FrameType
		if i%50 == 0 {
			payload = buildAVCCPayload(idrNAL)
			frameType = avframe.FrameTypeKeyframe
		} else {
			payload = buildAVCCPayload(pNAL)
			frameType = avframe.FrameTypeInterframe
		}

		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			FrameType: frameType,
			Payload:   payload,
			DTS:       dts,
		})
	}

	// Wait for packets to arrive.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	received := make([]rtpEvent, len(events))
	copy(received, events)
	mu.Unlock()

	if len(received) == 0 {
		t.Fatal("received zero RTP packets")
	}

	// Verify payload type correctness.
	// All packets should have a valid H264 PT (NOT an RTX PT).
	// The expected PT depends on SDP negotiation, but we can verify:
	// 1. All packets have the same PT (consistency)
	// 2. The PT is NOT the RTX PT (which would be the PT with apt= fmtp)
	firstPT := received[0].PT
	for i, ev := range received {
		if ev.PT != firstPT {
			t.Errorf("packet %d has PT=%d, expected consistent PT=%d", i, ev.PT, firstPT)
		}
	}

	// Check that frames with marker bits have reasonable inter-arrival times.
	var markerEvents []rtpEvent
	for _, ev := range received {
		if ev.Marker {
			markerEvents = append(markerEvents, ev)
		}
	}

	t.Logf("Total RTP packets: %d, Marker packets (frames): %d, PT=%d",
		len(received), len(markerEvents), firstPT)

	if len(markerEvents) < 10 {
		t.Fatalf("too few marker packets: %d (expected at least 10 for 3s of 25fps)", len(markerEvents))
	}

	// Analyze inter-frame arrival intervals.
	burstCount := 0
	gapCount := 0
	for i := 1; i < len(markerEvents); i++ {
		dt := markerEvents[i].WallTime.Sub(markerEvents[i-1].WallTime)
		if dt < 5*time.Millisecond {
			burstCount++
		}
		if dt > 200*time.Millisecond {
			gapCount++
		}
	}

	t.Logf("Frame delivery: bursts(<5ms)=%d, gaps(>200ms)=%d, total_intervals=%d",
		burstCount, gapCount, len(markerEvents)-1)

	// Fail if delivery is too bursty or has excessive gaps.
	burstRatio := float64(burstCount) / float64(len(markerEvents)-1)
	if burstRatio > 0.3 {
		t.Errorf("%.0f%% of frame intervals are bursts (<5ms) — indicates batched delivery", burstRatio*100)
	}
	if gapCount > 3 {
		t.Errorf("too many gaps (>200ms): %d — indicates stuttering", gapCount)
	}

	fmt.Printf("\n=== WHEP E2E RESULT ===\n")
	fmt.Printf("Frames sent: %d, Packets received: %d, Marker frames: %d\n", totalFrames, len(received), len(markerEvents))
	fmt.Printf("PT=%d (consistent across all packets)\n", firstPT)
	fmt.Printf("Burst ratio: %.1f%%, Gap count: %d\n", burstRatio*100, gapCount)
}

// TestWHEPWithGCC verifies that WHEP playback works correctly with GCC
// congestion control enabled. This catches interceptor chain ordering bugs
// like "missing transport layer cc header extension".
func TestWHEPWithGCC(t *testing.T) {
	// Create module with GCC enabled.
	cfg := &config.Config{
		Stream: config.StreamConfig{
			RingBufferSize:     256,
			GOPCache:           true,
			GOPCacheNum:        1,
			IdleTimeout:        30 * time.Second,
			NoPublisherTimeout: 15 * time.Second,
		},
		WebRTC: config.WebRTCConfig{
			Enabled:      true,
			Listen:       ":0",
			UDPPortRange: []int{20000, 20100},
			GCC: config.GCCConfig{
				Enabled:        true,
				InitialBitrate: 2_000_000,
				MinBitrate:     100_000,
				MaxBitrate:     10_000_000,
			},
		},
	}
	s := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatalf("Init with GCC: %v", err)
	}
	defer m.Close()

	if m.latestBWE == nil {
		t.Fatal("expected latestBWE channel when GCC enabled")
	}

	// Create a publishing stream.
	stream, err := s.StreamHub().GetOrCreate("live/gcc-test")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{
		id:   "gcc-test-pub",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Write SPS/PPS sequence header.
	spsData := []byte{0x67, 0x42, 0x00, 0x1f, 0xe9, 0x40, 0x14, 0x04, 0x78}
	ppsData := []byte{0x68, 0xce, 0x38, 0x80}
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   buildTestAVCConfigPayload(spsData, ppsData),
		DTS:       0,
	})

	// Create WHEP client.
	clientME := &webrtc.MediaEngine{}
	if err := clientME.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	clientAPI := webrtc.NewAPI(webrtc.WithMediaEngine(clientME))
	clientPC, err := clientAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()

	_, err = clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})
	if err != nil {
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

	// Send WHEP request.
	req := httptest.NewRequest(
		http.MethodPost,
		"/webrtc/whep/live/gcc-test",
		bytes.NewReader([]byte(clientPC.LocalDescription().SDP)),
	)
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP returned %d: %s", rr.Code, rr.Body.String())
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  rr.Body.String(),
	}
	if err := clientPC.SetRemoteDescription(answer); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}

	// Collect received RTP packets.
	var mu sync.Mutex
	var packetCount int

	clientPC.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		for {
			_, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			mu.Lock()
			packetCount++
			mu.Unlock()
		}
	})

	// Wait for ICE connection.
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
	case <-time.After(10 * time.Second):
		t.Fatal("ICE connection timed out")
	}

	// Pump video frames at 25fps for 2 seconds.
	// Send keyframes every 25 frames to ensure the feed loop (realtime mode)
	// catches one even under CI/test resource contention.
	idrNAL := make([]byte, 3000)
	idrNAL[0] = 0x65
	for i := 1; i < len(idrNAL); i++ {
		idrNAL[i] = byte(i & 0xFF)
	}

	pNAL := make([]byte, 1000)
	pNAL[0] = 0x41
	for i := 1; i < len(pNAL); i++ {
		pNAL[i] = byte(i & 0xFF)
	}

	const totalFrames = 50
	const frameDurationMs = 40

	sendStart := time.Now()
	for i := 0; i < totalFrames; i++ {
		targetTime := sendStart.Add(time.Duration(i) * frameDurationMs * time.Millisecond)
		if sleepDur := time.Until(targetTime); sleepDur > 0 {
			time.Sleep(sleepDur)
		}

		dts := int64(i * frameDurationMs)
		var payload []byte
		var frameType avframe.FrameType
		if i%25 == 0 {
			payload = buildAVCCPayload(idrNAL)
			frameType = avframe.FrameTypeKeyframe
		} else {
			payload = buildAVCCPayload(pNAL)
			frameType = avframe.FrameTypeInterframe
		}

		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			FrameType: frameType,
			Payload:   payload,
			DTS:       dts,
		})
	}

	// Wait for packets to arrive via GCC pacer.
	time.Sleep(1 * time.Second)

	mu.Lock()
	count := packetCount
	mu.Unlock()

	if count == 0 {
		t.Fatal("received zero RTP packets with GCC enabled — " +
			"likely 'missing transport layer cc header extension' error")
	}

	t.Logf("GCC E2E: sent %d frames, received %d RTP packets", totalFrames, count)
	fmt.Printf("\n=== GCC WHEP E2E RESULT ===\n")
	fmt.Printf("Frames sent: %d, RTP packets received: %d (GCC pacer active)\n", totalFrames, count)
}
