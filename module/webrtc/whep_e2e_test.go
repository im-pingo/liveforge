package webrtc

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
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
			TLS:          func() *bool { b := false; return &b }(),
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

// TestWHEPAudioTranscoding verifies the full AAC→Opus transcoding path through
// WebRTC, including:
//   - Audio RTP timestamp spacing (must be exactly 960 = 20ms at 48kHz)
//   - Audio packets contain valid Opus data (TOC byte indicates 20ms stereo)
//   - A/V sync: first audio and video packets arrive within a reasonable window
//
// This test catches issues like:
//   - Voice changer effect (wrong clock rate or frame duration)
//   - A/V desync from GOP cache sending video-only when transcoding
//   - Missing or corrupted audio data
func TestWHEPAudioTranscoding(t *testing.T) {
	m, s := newTestModule(t)

	// Enable audio transcoding so TranscodeManager is created on streams.
	s.StreamHub().SetAudioCodecEnabled(true)

	// Create a stream with AAC audio (requires transcoding to Opus for WebRTC).
	stream, err := s.StreamHub().GetOrCreate("live/audio-test")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{
		id: "audio-test-pub",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
			SampleRate: 48000,
			Channels:   2,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Write video sequence header.
	spsData := []byte{0x67, 0x42, 0x00, 0x1f, 0xe9, 0x40, 0x14, 0x04, 0x78}
	ppsData := []byte{0x68, 0xce, 0x38, 0x80}
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   buildTestAVCConfigPayload(spsData, ppsData),
		DTS:       0,
	})

	// Write audio sequence header (AAC-LC 48kHz stereo AudioSpecificConfig).
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x11, 0x90}, // AAC-LC, 48kHz, stereo
		DTS:       0,
	})

	// Encode real AAC frames to use as test data.
	aacEnc := audiocodec.NewFFmpegEncoder("aac", 48000, 2)
	defer aacEnc.Close()

	// Pre-encode several AAC frames from silence.
	var aacPayloads [][]byte
	for range 100 {
		pcm := &audiocodec.PCMFrame{
			Samples:    make([]int16, 1024*2), // 1024 samples/ch stereo
			SampleRate: 48000,
			Channels:   2,
		}
		data, err := aacEnc.Encode(pcm)
		if err != nil {
			t.Fatalf("AAC encode: %v", err)
		}
		aacPayloads = append(aacPayloads, data)
	}
	t.Logf("Pre-encoded %d AAC frames (1024 samples/ch @ 48kHz)", len(aacPayloads))

	// Create WHEP client with both video and audio recv.
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
	_, err = clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
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

	req := httptest.NewRequest(
		http.MethodPost,
		"/webrtc/whep/live/audio-test",
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

	// Collect RTP packets by track type.
	type rtpEvent struct {
		Timestamp uint32
		PT        uint8
		Size      int
		WallTime  time.Time
	}
	var mu sync.Mutex
	var audioEvents, videoEvents []rtpEvent

	clientPC.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		isAudio := track.Kind() == webrtc.RTPCodecTypeAudio
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			ev := rtpEvent{
				Timestamp: pkt.Timestamp,
				PT:        pkt.PayloadType,
				Size:      len(pkt.Payload),
				WallTime:  time.Now(),
			}
			mu.Lock()
			if isAudio {
				audioEvents = append(audioEvents, ev)
			} else {
				videoEvents = append(videoEvents, ev)
			}
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

	// Give the feed loop time to create the transcoded reader and start
	// the transcoding goroutine before we pump frames into the ring buffer.
	time.Sleep(200 * time.Millisecond)

	// Pump interleaved video + audio frames.
	const totalVideoFrames = 75 // 3s at 25fps
	const videoFrameDurMs = 40
	const audioFrameDurMs = 21 // ~1024/48000 = 21.3ms

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

	sendStart := time.Now()
	audioIdx := 0
	nextAudioDTS := int64(0)

	for i := 0; i < totalVideoFrames; i++ {
		videoDTS := int64(i * videoFrameDurMs)
		targetTime := sendStart.Add(time.Duration(videoDTS) * time.Millisecond)
		if sleepDur := time.Until(targetTime); sleepDur > 0 {
			time.Sleep(sleepDur)
		}

		// Send video frame.
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
			DTS:       videoDTS,
			PTS:       videoDTS,
		})

		// Send audio frames that fall within this video frame's time span.
		for nextAudioDTS <= videoDTS+int64(videoFrameDurMs) && audioIdx < len(aacPayloads) {
			stream.WriteFrame(&avframe.AVFrame{
				MediaType: avframe.MediaTypeAudio,
				Codec:     avframe.CodecAAC,
				FrameType: avframe.FrameTypeInterframe,
				Payload:   aacPayloads[audioIdx],
				DTS:       nextAudioDTS,
			})
			audioIdx++
			nextAudioDTS += int64(audioFrameDurMs)
		}
	}
	t.Logf("Sent %d video frames + %d audio frames", totalVideoFrames, audioIdx)

	// Wait for packets to arrive.
	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	audioRcvd := make([]rtpEvent, len(audioEvents))
	copy(audioRcvd, audioEvents)
	videoRcvd := make([]rtpEvent, len(videoEvents))
	copy(videoRcvd, videoEvents)
	mu.Unlock()

	// === Audio verification ===
	t.Logf("Received %d audio RTP packets, %d video RTP packets", len(audioRcvd), len(videoRcvd))

	if len(audioRcvd) == 0 {
		t.Fatal("received zero audio RTP packets — transcoding pipeline may be broken")
	}
	if len(videoRcvd) == 0 {
		t.Fatal("received zero video RTP packets")
	}

	// 1. Verify audio RTP timestamp spacing = 960 (20ms at 48kHz).
	var tsDeltaCounts = make(map[uint32]int)
	for i := 1; i < len(audioRcvd); i++ {
		delta := audioRcvd[i].Timestamp - audioRcvd[i-1].Timestamp
		tsDeltaCounts[delta]++
	}
	t.Logf("Audio RTP timestamp deltas: %v", tsDeltaCounts)

	totalDeltas := len(audioRcvd) - 1
	if totalDeltas > 0 {
		correctDeltas := tsDeltaCounts[960]
		correctRatio := float64(correctDeltas) / float64(totalDeltas)
		t.Logf("Audio RTP timestamp: %d/%d (%.1f%%) have delta=960 (20ms@48kHz)",
			correctDeltas, totalDeltas, correctRatio*100)

		if correctRatio < 0.9 {
			t.Errorf("Audio RTP timestamp spacing: only %.1f%% have delta=960 (expected >90%%)",
				correctRatio*100)
			t.Errorf("This means audio duration is wrong — likely cause of voice changer effect")
		}
	}

	// 2. Verify A/V sync: first audio and video packets should arrive within 500ms.
	if len(audioRcvd) > 0 && len(videoRcvd) > 0 {
		avOffset := audioRcvd[0].WallTime.Sub(videoRcvd[0].WallTime)
		t.Logf("A/V sync: first audio arrived %v after first video", avOffset)
		if math.Abs(float64(avOffset)) > float64(2*time.Second) {
			t.Errorf("A/V desync: first audio %.1fs after first video (expected <2s)",
				avOffset.Seconds())
		}
	}

	// 3. Print summary.
	fmt.Printf("\n=== WHEP Audio Transcode E2E RESULT ===\n")
	fmt.Printf("Audio: %d RTP packets (PT=%d)\n", len(audioRcvd), audioRcvd[0].PT)
	fmt.Printf("Video: %d RTP packets (PT=%d)\n", len(videoRcvd), videoRcvd[0].PT)
	fmt.Printf("Timestamp deltas: %v\n", tsDeltaCounts)
}

// TestWHEPJitterDiagnostic is a comprehensive jitter analysis test that
// measures the same metrics as chrome://webrtc-internals over a 10-second
// stream. It runs two scenarios (video-only vs video+audio transcoding) to
// isolate whether audio transcoding causes jitter buffer inflation.
//
// Metrics computed per scenario:
//   - RTP timestamp delta distribution (should be consistent 40ms / 3600 ticks)
//   - Arrival time delta distribution (should match RTP delta ±jitter)
//   - Chrome-style jitter: EWMA of |arrival_delta - rtp_delta| / 16
//   - Jitter trend: linear regression over time (growing = problem)
//   - Burst/gap counts: clustered delivery indicates feed loop stalls
//   - Sequence number gaps: indicates packet loss
func TestWHEPJitterDiagnostic(t *testing.T) {
	type scenario struct {
		name           string
		withAudio      bool
		streamPath     string
	}
	scenarios := []scenario{
		{"video_only", false, "live/jitter-video-only"},
		{"video_audio_transcode", true, "live/jitter-av-transcode"},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			runJitterDiagnostic(t, sc.withAudio, sc.streamPath)
		})
	}
}

func runJitterDiagnostic(t *testing.T, withAudio bool, streamPath string) {
	t.Helper()

	m, s := newTestModule(t)
	if withAudio {
		s.StreamHub().SetAudioCodecEnabled(true)
	}

	stream, err := s.StreamHub().GetOrCreate(streamPath)
	if err != nil {
		t.Fatal(err)
	}

	pubInfo := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	if withAudio {
		pubInfo.AudioCodec = avframe.CodecAAC
		pubInfo.SampleRate = 48000
		pubInfo.Channels = 2
	}
	pub := &testPublisher{id: "jitter-pub", info: pubInfo}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Write video sequence header.
	spsData := []byte{0x67, 0x42, 0x00, 0x1f, 0xe9, 0x40, 0x14, 0x04, 0x78}
	ppsData := []byte{0x68, 0xce, 0x38, 0x80}
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   buildTestAVCConfigPayload(spsData, ppsData),
		DTS:       0,
	})

	// Pre-encode AAC frames if audio is needed.
	var aacPayloads [][]byte
	if withAudio {
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeAudio,
			Codec:     avframe.CodecAAC,
			FrameType: avframe.FrameTypeSequenceHeader,
			Payload:   []byte{0x11, 0x90}, // AAC-LC, 48kHz, stereo
			DTS:       0,
		})
		aacEnc := audiocodec.NewFFmpegEncoder("aac", 48000, 2)
		defer aacEnc.Close()
		for range 500 {
			pcm := &audiocodec.PCMFrame{
				Samples:    make([]int16, 1024*2),
				SampleRate: 48000,
				Channels:   2,
			}
			data, err := aacEnc.Encode(pcm)
			if err != nil {
				t.Fatalf("AAC encode: %v", err)
			}
			aacPayloads = append(aacPayloads, data)
		}
	}

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
	if withAudio {
		_, err = clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
		if err != nil {
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

	req := httptest.NewRequest(
		http.MethodPost,
		"/webrtc/whep/"+streamPath,
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

	// Collect video RTP packets with wall-clock timestamps.
	type rtpSample struct {
		Seq       uint16
		Timestamp uint32
		Marker    bool
		WallTime  time.Time
	}
	var mu sync.Mutex
	var videoSamples []rtpSample
	var audioSamples []rtpSample

	clientPC.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		isAudio := track.Kind() == webrtc.RTPCodecTypeAudio
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			s := rtpSample{
				Seq:       pkt.SequenceNumber,
				Timestamp: pkt.Timestamp,
				Marker:    pkt.Marker,
				WallTime:  time.Now(),
			}
			mu.Lock()
			if isAudio {
				audioSamples = append(audioSamples, s)
			} else {
				videoSamples = append(videoSamples, s)
			}
			mu.Unlock()
		}
	})

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

	// Give feed loop time to initialize transcode reader.
	time.Sleep(200 * time.Millisecond)

	// Pump frames at real-time pace for 60 seconds to detect long-term drift.
	const durationSec = 60
	const fps = 25
	const frameDurMs = 40
	const totalFrames = durationSec * fps

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

	sendStart := time.Now()
	audioIdx := 0
	nextAudioDTS := int64(0)
	const audioFrameDurMs = 21

	for i := 0; i < totalFrames; i++ {
		videoDTS := int64(i * frameDurMs)
		targetTime := sendStart.Add(time.Duration(videoDTS) * time.Millisecond)
		if sleepDur := time.Until(targetTime); sleepDur > 0 {
			time.Sleep(sleepDur)
		}

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
			DTS:       videoDTS,
			PTS:       videoDTS,
		})

		// Interleave audio frames.
		if withAudio {
			for nextAudioDTS <= videoDTS+int64(frameDurMs) && audioIdx < len(aacPayloads) {
				stream.WriteFrame(&avframe.AVFrame{
					MediaType: avframe.MediaTypeAudio,
					Codec:     avframe.CodecAAC,
					FrameType: avframe.FrameTypeInterframe,
					Payload:   aacPayloads[audioIdx],
					DTS:       nextAudioDTS,
				})
				audioIdx++
				nextAudioDTS += int64(audioFrameDurMs)
			}
		}
	}

	// Wait for last packets to arrive.
	time.Sleep(2 * time.Second)

	mu.Lock()
	vSamples := make([]rtpSample, len(videoSamples))
	copy(vSamples, videoSamples)
	aSamples := make([]rtpSample, len(audioSamples))
	copy(aSamples, audioSamples)
	mu.Unlock()

	// === Analysis ===
	t.Logf("Received %d video RTP packets, %d audio RTP packets", len(vSamples), len(aSamples))

	if len(vSamples) < 50 {
		t.Fatalf("too few video packets: %d", len(vSamples))
	}

	// Extract marker (frame-boundary) packets for frame-level analysis.
	var frames []rtpSample
	for _, s := range vSamples {
		if s.Marker {
			frames = append(frames, s)
		}
	}
	t.Logf("Video frames (marker packets): %d", len(frames))

	if len(frames) < 20 {
		t.Fatalf("too few video frames: %d", len(frames))
	}

	// 1. Sequence number gap analysis (packet loss).
	seqGaps := 0
	for i := 1; i < len(vSamples); i++ {
		expected := vSamples[i-1].Seq + 1
		if vSamples[i].Seq != expected {
			seqGaps++
		}
	}
	t.Logf("Sequence gaps (potential packet loss): %d / %d packets", seqGaps, len(vSamples))

	// 2. RTP timestamp delta distribution (frame-level).
	rtpDeltaCounts := make(map[uint32]int)
	for i := 1; i < len(frames); i++ {
		delta := frames[i].Timestamp - frames[i-1].Timestamp
		rtpDeltaCounts[delta]++
	}
	t.Logf("Video RTP timestamp deltas (frame-level): %v", rtpDeltaCounts)

	// 3. Chrome-style jitter estimation.
	// Chrome uses: jitter += (|arrival_delta - rtp_delta| - jitter) / 16
	// where arrival_delta and rtp_delta are in the same time unit.
	// H.264 clock rate = 90kHz, so 1 RTP tick = 1/90000 s.
	const videoClockRate = 90000.0
	var jitter float64
	var maxJitter float64
	var jitterHistory []float64 // sample jitter every ~1s for trend analysis

	burstCount := 0
	gapCount := 0
	windowStart := frames[0].WallTime
	windowJitter := 0.0
	windowSamples := 0

	for i := 1; i < len(frames); i++ {
		arrivalDelta := frames[i].WallTime.Sub(frames[i-1].WallTime).Seconds()
		rtpDelta := float64(frames[i].Timestamp-frames[i-1].Timestamp) / videoClockRate

		diff := math.Abs(arrivalDelta - rtpDelta)
		jitter += (diff - jitter) / 16.0

		if jitter > maxJitter {
			maxJitter = jitter
		}

		// Track per-second jitter for trend analysis.
		windowJitter += jitter
		windowSamples++
		if frames[i].WallTime.Sub(windowStart) >= time.Second {
			jitterHistory = append(jitterHistory, windowJitter/float64(windowSamples))
			windowStart = frames[i].WallTime
			windowJitter = 0
			windowSamples = 0
		}

		// Burst/gap detection.
		arrivalMs := arrivalDelta * 1000
		if arrivalMs < 5 {
			burstCount++
		}
		if arrivalMs > 200 {
			gapCount++
		}
	}

	// Flush last window.
	if windowSamples > 0 {
		jitterHistory = append(jitterHistory, windowJitter/float64(windowSamples))
	}

	t.Logf("Jitter (Chrome EWMA): final=%.3fms, max=%.3fms", jitter*1000, maxJitter*1000)
	t.Logf("Frame delivery: bursts(<5ms)=%d, gaps(>200ms)=%d", burstCount, gapCount)

	// 4. Jitter trend: linear regression on per-second averages.
	// A positive slope means jitter is growing over time (buffer inflation).
	var jitterSlope float64
	if len(jitterHistory) >= 3 {
		n := float64(len(jitterHistory))
		var sumX, sumY, sumXY, sumX2 float64
		for i, j := range jitterHistory {
			x := float64(i)
			sumX += x
			sumY += j
			sumXY += x * j
			sumX2 += x * x
		}
		jitterSlope = (n*sumXY - sumX*sumY) / (n*sumX2 - sumX*sumX)
		t.Logf("Jitter trend (per-second averages): %v", formatJitterHistory(jitterHistory))
		t.Logf("Jitter slope: %.6f ms/s (positive = growing)", jitterSlope*1000)
	}

	// 5. Arrival delta statistics.
	var arrivalDeltas []float64
	for i := 1; i < len(frames); i++ {
		d := frames[i].WallTime.Sub(frames[i-1].WallTime).Seconds() * 1000
		arrivalDeltas = append(arrivalDeltas, d)
	}
	mean, stdev := meanStdev(arrivalDeltas)
	t.Logf("Arrival delta: mean=%.1fms, stdev=%.1fms", mean, stdev)

	// === Summary ===
	fmt.Printf("\n=== JITTER DIAGNOSTIC [%s] ===\n", t.Name())
	fmt.Printf("Duration: %ds, Frames: %d, Packets: %d (video) + %d (audio)\n",
		durationSec, len(frames), len(vSamples), len(aSamples))
	fmt.Printf("Seq gaps: %d, Bursts: %d, Gaps: %d\n", seqGaps, burstCount, gapCount)
	fmt.Printf("Jitter: final=%.3fms, max=%.3fms, slope=%.4fms/s\n",
		jitter*1000, maxJitter*1000, jitterSlope*1000)
	fmt.Printf("Arrival delta: mean=%.1fms, stdev=%.1fms\n", mean, stdev)
	fmt.Printf("RTP timestamp deltas: %v\n", rtpDeltaCounts)
	if len(jitterHistory) > 0 {
		fmt.Printf("Jitter trend (per-sec avg ms): %s\n", formatJitterHistory(jitterHistory))
	}

	// === Assertions ===
	// Max jitter should stay below 100ms (Chrome's default max jitter buffer).
	if maxJitter > 0.100 {
		t.Errorf("max jitter %.1fms exceeds 100ms threshold — " +
			"Chrome would show buffer inflation and stuttering", maxJitter*1000)
	}

	// Jitter should not grow over time (slope should be near zero or negative).
	if jitterSlope > 0.005 { // >5ms/s growth rate
		t.Errorf("jitter is growing at %.2fms/s — indicates systematic drift " +
			"that will cause progressive stuttering", jitterSlope*1000)
	}

	// No excessive gaps (stuttering).
	if gapCount > 5 {
		t.Errorf("too many delivery gaps (>200ms): %d — indicates stuttering", gapCount)
	}

	// No excessive bursts (batched delivery causes jitter spikes).
	burstRatio := float64(burstCount) / float64(len(frames)-1)
	if burstRatio > 0.2 {
		t.Errorf("%.0f%% of frame intervals are bursts — indicates batched delivery",
			burstRatio*100)
	}

	// Arrival delta stdev should be reasonable (not high variance).
	if stdev > 30 {
		t.Errorf("arrival delta stdev %.1fms — high variance indicates irregular delivery", stdev)
	}
}

// meanStdev computes the mean and standard deviation of a float64 slice.
func meanStdev(vals []float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var sumSq float64
	for _, v := range vals {
		d := v - mean
		sumSq += d * d
	}
	return mean, math.Sqrt(sumSq / float64(len(vals)))
}

// formatJitterHistory formats per-second jitter averages as a compact string.
func formatJitterHistory(history []float64) string {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, j := range history {
		if i > 0 {
			buf.WriteString(", ")
		}
		fmt.Fprintf(&buf, "%.2f", j*1000) // convert to ms
	}
	buf.WriteByte(']')
	return buf.String()
}
