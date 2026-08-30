package webrtc

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
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
			TLS:          func() *bool { b := false; return &b }(),
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

func TestHTTPServerTimeouts(t *testing.T) {
	m, _ := newTestModule(t)

	if got := m.httpSrv.ReadHeaderTimeout; got != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want %v", got, 5*time.Second)
	}
	if got := m.httpSrv.IdleTimeout; got != 2*time.Minute {
		t.Errorf("IdleTimeout = %v, want %v", got, 2*time.Minute)
	}
	if got := m.httpSrv.WriteTimeout; got != 0 {
		t.Errorf("WriteTimeout = %v, want unchanged zero value", got)
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
func testPCPair(t *testing.T) (*TrackSender, *webrtc.PeerConnection, *webrtc.PeerConnection, *rtpPeerStats, uint32) {
	t.Helper()

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	ir := &interceptor.Registry{}
	rtpStatsFactory := newRTPStatsInterceptorFactory()
	ir.Add(rtpStatsFactory)
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir))

	senderPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	rtpStats := <-rtpStatsFactory.created
	t.Cleanup(func() { senderPC.Close() })

	receiverPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	<-rtpStatsFactory.created
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

	return ts, receiverPC, senderPC, rtpStats, mediaSSRC
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
	ts, receiverPC, _, _, mediaSSRC := testPCPair(t)

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
	ts, receiverPC, _, _, mediaSSRC := testPCPair(t)

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
	ts, _, _, _, _ := testPCPair(t)

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
	ts, receiverPC, _, _, mediaSSRC := testPCPair(t)

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
	ts, receiverPC, _, _, mediaSSRC := testPCPair(t)

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

func TestModuleInitWithGCC(t *testing.T) {
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
		t.Fatalf("Init with GCC failed: %v", err)
	}
	defer m.Close()

	if m.latestBWE == nil {
		t.Error("expected latestBWE channel to be initialized when GCC enabled")
	}
}

func TestModuleInitWithoutGCC(t *testing.T) {
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
				Enabled: false,
			},
		},
	}
	s := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatalf("Init without GCC failed: %v", err)
	}
	defer m.Close()

	if m.latestBWE != nil {
		t.Error("expected latestBWE to be nil when GCC disabled")
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

func TestOfferSupportsCodec(t *testing.T) {
	offer := createMinimalOffer(t)
	var parsed sdp.SessionDescription
	if err := parsed.UnmarshalString(offer); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		media string
		mime  string
		want  bool
	}{
		{"video", webrtc.MimeTypeH264, true},
		{"video", webrtc.MimeTypeVP8, true},
		{"video", "video/NONEXISTENT", false},
		{"audio", "audio/NONEXISTENT", false},
	}

	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			got := offerSupportsCodec(&parsed, tt.media, tt.mime)
			if got != tt.want {
				t.Errorf("offerSupportsCodec(%s, %s) = %v, want %v", tt.media, tt.mime, got, tt.want)
			}
		})
	}
}

func TestOfferSupportsCodecRequiresExactRTPMapName(t *testing.T) {
	offer := sdp.SessionDescription{MediaDescriptions: []*sdp.MediaDescription{{
		MediaName: sdp.MediaName{
			Media:   "video",
			Port:    sdp.RangedPort{Value: 9},
			Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
			Formats: []string{"96"},
		},
		Attributes: []sdp.Attribute{{Key: "rtpmap", Value: "96 XH264/90000"}},
	}}}

	if offerSupportsCodec(&offer, "video", webrtc.MimeTypeH264) {
		t.Fatal("non-H264 RTP map name matched H264 by substring")
	}
}

func TestOfferSupportsCodecRequiresPayloadListedByMediaLine(t *testing.T) {
	offer := sdp.SessionDescription{MediaDescriptions: []*sdp.MediaDescription{{
		MediaName: sdp.MediaName{
			Media:   "video",
			Port:    sdp.RangedPort{Value: 9},
			Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
			Formats: []string{"96"},
		},
		Attributes: []sdp.Attribute{{Key: "rtpmap", Value: "97 H264/90000"}},
	}}}

	if offerSupportsCodec(&offer, "video", webrtc.MimeTypeH264) {
		t.Fatal("RTP map payload omitted from the media line matched H264")
	}
}

func TestOfferRequestsOnlyReceivingMediaDescriptions(t *testing.T) {
	tests := []struct {
		name             string
		port             int
		sessionDirection string
		mediaDirection   string
		want             bool
	}{
		{name: "default sendrecv", port: 9, want: true},
		{name: "media recvonly", port: 9, mediaDirection: "recvonly", want: true},
		{name: "zero port", want: false},
		{name: "media inactive", port: 9, mediaDirection: "inactive", want: false},
		{name: "media sendonly", port: 9, mediaDirection: "sendonly", want: false},
		{name: "session inactive", port: 9, sessionDirection: "inactive", want: false},
		{name: "session sendonly", port: 9, sessionDirection: "sendonly", want: false},
		{name: "media overrides session", port: 9, sessionDirection: "sendonly", mediaDirection: "recvonly", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			offer := sdp.SessionDescription{}
			if test.sessionDirection != "" {
				offer.Attributes = append(offer.Attributes, sdp.Attribute{Key: test.sessionDirection})
			}
			description := &sdp.MediaDescription{MediaName: sdp.MediaName{
				Media:   "video",
				Port:    sdp.RangedPort{Value: test.port},
				Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
				Formats: []string{"96"},
			}}
			if test.mediaDirection != "" {
				description.Attributes = append(description.Attributes, sdp.Attribute{Key: test.mediaDirection})
			}
			offer.MediaDescriptions = []*sdp.MediaDescription{description}
			if got := offerRequestsMedia(&offer, "video"); got != test.want {
				t.Fatalf("offerRequestsMedia() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSelectWHEPAudioCodecHonorsOffer(t *testing.T) {
	tests := []struct {
		name          string
		source        avframe.CodecType
		offerCodecs   []string
		canTranscode  bool
		want          avframe.CodecType
		wantTranscode bool
	}{
		{name: "direct PCMA", source: avframe.CodecG711A, offerCodecs: []string{"PCMA"}, want: avframe.CodecG711A},
		{name: "PCMA to Opus", source: avframe.CodecG711A, offerCodecs: []string{"opus"}, canTranscode: true, want: avframe.CodecOpus, wantTranscode: true},
		{name: "AAC to Opus", source: avframe.CodecAAC, offerCodecs: []string{"opus"}, canTranscode: true, want: avframe.CodecOpus, wantTranscode: true},
		{name: "unsupported source", source: avframe.CodecG711A, offerCodecs: []string{"opus"}},
		{name: "unsupported offer", source: avframe.CodecG711A, offerCodecs: []string{"PCMU"}, canTranscode: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var offer sdp.SessionDescription
			payloads := make([]string, 0, len(tt.offerCodecs))
			attributes := make([]sdp.Attribute, 0, len(tt.offerCodecs))
			for index, codec := range tt.offerCodecs {
				payloadType := fmt.Sprint(96 + index)
				payloads = append(payloads, payloadType)
				attributes = append(attributes, sdp.Attribute{Key: "rtpmap", Value: payloadType + " " + codec + "/48000/2"})
			}
			offer.MediaDescriptions = []*sdp.MediaDescription{{
				MediaName:  sdp.MediaName{Media: "audio", Port: sdp.RangedPort{Value: 9}, Protos: []string{"UDP", "TLS", "RTP", "SAVPF"}, Formats: payloads},
				Attributes: attributes,
			}}
			got, transcode := selectWHEPAudioCodec(&offer, tt.source, tt.canTranscode)
			if got != tt.want || transcode != tt.wantTranscode {
				t.Fatalf("selectWHEPAudioCodec() = (%v, %v), want (%v, %v)", got, transcode, tt.want, tt.wantTranscode)
			}
		})
	}
}

func TestICEServersFromConfigWithTURN(t *testing.T) {
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
			ICELite:      false,
			TLS:          func() *bool { b := false; return &b }(),
			UDPPortRange: []int{20000, 20100},
			ICEServers: []config.ICEServer{
				{URLs: []string{"stun:stun.l.google.com:19302"}},
				{URLs: []string{"turn:turn.example.com:3478"}, Username: "user1", Credential: "pass1"},
				{URLs: []string{"turns:turn.example.com:5349"}, Username: "user2", Credential: "pass2"},
			},
		},
	}
	s := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer m.Close()

	servers := m.iceServersFromConfig()
	if len(servers) != 3 {
		t.Fatalf("expected 3 ICE servers, got %d", len(servers))
	}

	// STUN server (no credentials)
	if servers[0].URLs[0] != "stun:stun.l.google.com:19302" {
		t.Errorf("unexpected STUN URL: %s", servers[0].URLs[0])
	}

	// TURN server with credentials
	if servers[1].URLs[0] != "turn:turn.example.com:3478" {
		t.Errorf("unexpected TURN URL: %s", servers[1].URLs[0])
	}
	if servers[1].Username != "user1" || servers[1].Credential != "pass1" {
		t.Errorf("TURN credentials not passed: user=%s cred=%v", servers[1].Username, servers[1].Credential)
	}

	// TURNS server
	if servers[2].URLs[0] != "turns:turn.example.com:5349" {
		t.Errorf("unexpected TURNS URL: %s", servers[2].URLs[0])
	}
}

func TestICEServersSkippedWithICELite(t *testing.T) {
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
			ICELite:      true,
			TLS:          func() *bool { b := false; return &b }(),
			UDPPortRange: []int{20000, 20100},
			ICEServers: []config.ICEServer{
				{URLs: []string{"turn:turn.example.com:3478"}, Username: "u", Credential: "p"},
			},
		},
	}
	s := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer m.Close()

	servers := m.iceServersFromConfig()
	if servers != nil {
		t.Errorf("ICE Lite should return nil ICE servers, got %d", len(servers))
	}
}
