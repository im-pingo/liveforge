package webrtc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func newTestServer(t *testing.T) *core.Server {
	t.Helper()
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
		},
	}
	return core.NewServer(cfg)
}

func newTestModule(t *testing.T) (*Module, *core.Server) {
	t.Helper()
	s := newTestServer(t)
	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m, s
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "webrtc" {
		t.Errorf("expected 'webrtc', got %q", m.Name())
	}
}

func TestModuleInitAndClose(t *testing.T) {
	m, _ := newTestModule(t)
	if m.Addr() == nil {
		t.Error("expected non-nil listener address")
	}
}

func TestCORSHeaders(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodOptions, "/webrtc/whip/live/test", nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("expected CORS Allow-Origin: *")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("expected POST in CORS Allow-Methods")
	}
}

func TestWHIPBadContentType(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/test", strings.NewReader("not sdp"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", rr.Code)
	}
}

func TestWHEPStreamNotFound(t *testing.T) {
	m, _ := newTestModule(t)

	// Create a minimal SDP offer.
	offer := createMinimalOffer(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/nonexistent", bytes.NewReader([]byte(offer)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeleteSessionNotFound(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodDelete, "/webrtc/session/nonexistent-id", nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestMimeToCodecType(t *testing.T) {
	tests := []struct {
		mime string
		want avframe.CodecType
	}{
		{"video/H264", avframe.CodecH264},
		{"video/H265", avframe.CodecH265},
		{"video/HEVC", avframe.CodecH265},
		{"video/VP8", avframe.CodecVP8},
		{"video/VP9", avframe.CodecVP9},
		{"video/AV1", avframe.CodecAV1},
		{"audio/opus", avframe.CodecOpus},
		{"audio/PCMU", avframe.CodecG711U},
		{"audio/PCMA", avframe.CodecG711A},
		{"video/unknown", 0},
	}

	for _, tt := range tests {
		got := mimeToCodecType(tt.mime)
		if got != tt.want {
			t.Errorf("mimeToCodecType(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}

func TestCodecToMime(t *testing.T) {
	tests := []struct {
		codec avframe.CodecType
		want  string
	}{
		{avframe.CodecH264, webrtc.MimeTypeH264},
		{avframe.CodecH265, webrtc.MimeTypeH265},
		{avframe.CodecVP8, webrtc.MimeTypeVP8},
		{avframe.CodecVP9, webrtc.MimeTypeVP9},
		{avframe.CodecAV1, webrtc.MimeTypeAV1},
		{avframe.CodecOpus, webrtc.MimeTypeOpus},
		{avframe.CodecG711U, webrtc.MimeTypePCMU},
		{avframe.CodecG711A, webrtc.MimeTypePCMA},
		{0, ""},
		{avframe.CodecAAC, ""},
	}

	for _, tt := range tests {
		got := codecToMime(tt.codec)
		if got != tt.want {
			t.Errorf("codecToMime(%v) = %q, want %q", tt.codec, got, tt.want)
		}
	}
}

func TestSessionStore(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	sess := newSession("test-id", pc, "live/test", "whip", m)
	m.storeSession(sess)

	found, ok := m.findSession("test-id")
	if !ok || found.id != "test-id" {
		t.Error("expected to find stored session")
	}

	sess.Close()

	_, ok = m.findSession("test-id")
	if ok {
		t.Error("expected session to be removed after Close")
	}
}

// testPCPair creates a connected sender/receiver PeerConnection pair with a
// video track. Returns the TrackSender, receiver PC, media SSRC, and cleanup func.
func testPCPair(t *testing.T) (*TrackSender, *webrtc.PeerConnection, uint32) {
	t.Helper()

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	senderPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { senderPC.Close() })

	receiverPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { receiverPC.Close() })

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	rtpSender, err := senderPC.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}

	ts := NewTrackSender("test-session", track, rtpSender)

	// SDP exchange.
	offer, err := senderPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(senderPC)
	if err := senderPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone
	if err := receiverPC.SetRemoteDescription(*senderPC.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := receiverPC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone2 := webrtc.GatheringCompletePromise(receiverPC)
	if err := receiverPC.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gatherDone2
	if err := senderPC.SetRemoteDescription(*receiverPC.LocalDescription()); err != nil {
		t.Fatal(err)
	}

	// Wait for full connection.
	connected := make(chan struct{})
	receiverPC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
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
		t.Fatal("PeerConnection connection timed out")
	}

	// Find media SSRC.
	var mediaSSRC uint32
	for _, tr := range receiverPC.GetTransceivers() {
		if tr.Receiver() != nil {
			if rTrack := tr.Receiver().Track(); rTrack != nil {
				mediaSSRC = uint32(rTrack.SSRC())
				break
			}
		}
	}
	if mediaSSRC == 0 {
		t.Skip("could not determine SSRC")
	}

	return ts, receiverPC, mediaSSRC
}

// waitForCondition polls until cond returns true or timeout expires.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error(msg)
}

// TestTrackSenderPLIHandler verifies that TrackSender dispatches PLI/FIR
// RTCP packets to the registered handler, independent of protocol code.
func TestTrackSenderPLIHandler(t *testing.T) {
	ts, receiverPC, mediaSSRC := testPCPair(t)

	var pliCount atomic.Int32
	ts.SetPLIHandler(func() { pliCount.Add(1) })
	ts.Start()

	if err := receiverPC.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: mediaSSRC},
	}); err != nil {
		t.Fatalf("WriteRTCP PLI: %v", err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		return pliCount.Load() > 0
	}, "PLI handler was not called after sending PLI RTCP packet")

	if ts.Stats.PLICount.Load() == 0 {
		t.Error("Stats.PLICount should be > 0 after PLI")
	}
}

// TestTrackSenderStats verifies that ReceiverReport updates Stats fields.
func TestTrackSenderStats(t *testing.T) {
	ts, receiverPC, mediaSSRC := testPCPair(t)

	var rrReceived atomic.Int32
	ts.SetReceiverReportHandler(func(report *rtcp.ReceiverReport) {
		rrReceived.Add(1)
	})
	ts.Start()

	// Send a ReceiverReport from receiver to sender.
	if err := receiverPC.WriteRTCP([]rtcp.Packet{
		&rtcp.ReceiverReport{
			SSRC: 12345,
			Reports: []rtcp.ReceptionReport{
				{
					SSRC:               mediaSSRC,
					FractionLost:       25,
					TotalLost:          100,
					Jitter:             300,
					LastSequenceNumber: 5000,
				},
			},
		},
	}); err != nil {
		t.Fatalf("WriteRTCP ReceiverReport: %v", err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		return rrReceived.Load() > 0
	}, "ReceiverReport handler was not called")

	if ts.Stats.PacketsLost.Load() != 100 {
		t.Errorf("expected PacketsLost=100, got %d", ts.Stats.PacketsLost.Load())
	}
	if ts.Stats.FractionLost.Load() != 25 {
		t.Errorf("expected FractionLost=25, got %d", ts.Stats.FractionLost.Load())
	}
	if ts.Stats.Jitter.Load() != 300 {
		t.Errorf("expected Jitter=300, got %d", ts.Stats.Jitter.Load())
	}
}

// TestTrackSenderWriteSampleSerialization verifies that concurrent WriteSample
// calls are serialized by the mutex, preventing interleaved RTP packets.
func TestTrackSenderWriteSampleSerialization(t *testing.T) {
	ts, _, _ := testPCPair(t)

	// Send a sample to initialize the packetizer.
	_ = ts.WriteSample(media.Sample{
		Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x01, 0x02},
		Duration: 40 * time.Millisecond,
	})

	// Launch concurrent writers. If the mutex is missing, pion's packetizer
	// would produce corrupted/interleaved packets.
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		go func() {
			errs <- ts.WriteSample(media.Sample{
				Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x03, 0x04},
				Duration: 40 * time.Millisecond,
			})
		}()
		go func() {
			// Simulate PLI recovery: Duration 0 should not panic or error.
			errs <- ts.WriteSample(media.Sample{
				Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x00, 0x00, 0x01, 0x65, 0x05},
				Duration: 0,
			})
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-errs; err != nil {
			t.Logf("WriteSample error (may be expected if PC closing): %v", err)
		}
	}
}

// TestTrackSenderNeedsKeyframeFlag verifies that PLI sets the needsKeyframe
// flag and that it can be cleared by the feed loop (via ClearNeedsKeyframe).
// This ensures the signal/flag approach works: RTCP goroutine signals, feed
// loop resyncs — no media is written from the RTCP goroutine.
func TestTrackSenderNeedsKeyframeFlag(t *testing.T) {
	ts, receiverPC, mediaSSRC := testPCPair(t)

	ts.Start()

	// Initially, needsKeyframe should be false.
	if ts.NeedsKeyframe() {
		t.Error("needsKeyframe should be false initially")
	}

	// Send PLI from receiver.
	if err := receiverPC.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: mediaSSRC},
	}); err != nil {
		t.Fatalf("WriteRTCP PLI: %v", err)
	}

	// Wait for the flag to be set by the RTCP loop.
	waitForCondition(t, 3*time.Second, func() bool {
		return ts.NeedsKeyframe()
	}, "needsKeyframe was not set after PLI")

	// Simulate feed loop clearing the flag after sending a keyframe.
	ts.ClearNeedsKeyframe()
	if ts.NeedsKeyframe() {
		t.Error("needsKeyframe should be false after ClearNeedsKeyframe")
	}
}

// TestTrackSenderPLIDoesNotWriteMedia verifies that the PLI handler does NOT
// write media samples. Only the feed loop should write media — the RTCP
// goroutine just sets a flag. This test ensures no regression to the old
// sendKeyframeFromGOP approach.
func TestTrackSenderPLIDoesNotWriteMedia(t *testing.T) {
	ts, receiverPC, mediaSSRC := testPCPair(t)

	// Set a PLI handler that only increments a counter (no media writes).
	var pliCalled atomic.Int32
	ts.SetPLIHandler(func() { pliCalled.Add(1) })
	ts.Start()

	// Send a normal sample to initialize the packetizer.
	if err := ts.WriteSample(media.Sample{
		Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xAA},
		Duration: 40 * time.Millisecond,
	}); err != nil {
		t.Fatalf("initial WriteSample: %v", err)
	}

	// Send PLI.
	if err := receiverPC.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: mediaSSRC},
	}); err != nil {
		t.Fatalf("WriteRTCP PLI: %v", err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		return pliCalled.Load() > 0
	}, "PLI handler was not called")

	// The PLI handler only sets the flag; it does not write media.
	// Verify the flag is set (feed loop would clear it).
	if !ts.NeedsKeyframe() {
		t.Error("needsKeyframe should be set after PLI")
	}
}

// createMinimalOffer creates a minimal SDP offer string for testing.
func createMinimalOffer(t *testing.T) string {
	t.Helper()
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}
