package webrtc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type authorizationTestPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *authorizationTestPublisher) ID() string                    { return p.id }
func (p *authorizationTestPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *authorizationTestPublisher) Close() error                  { return nil }

func sessionCount(m *Module) int {
	count := 0
	m.sessions.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func TestModuleHooks(t *testing.T) {
	m := NewModule()
	if hooks := m.Hooks(); hooks != nil {
		t.Errorf("Hooks should be nil, got %v", hooks)
	}
}

func TestModuleAddrNil(t *testing.T) {
	m := NewModule()
	if m.Addr() != nil {
		t.Error("expected nil Addr before Init")
	}
}

func TestHandlePatchNotFound(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPatch, "/webrtc/session/nonexistent-id", nil)
	req.Header.Set("Content-Type", "application/trickle-ice-sdpfrag")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestHandleStatusReturnsWHEPFeedDiagnostics(t *testing.T) {
	m, _ := newTestModule(t)
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession("status-session", pc, "live/status", "whep", m)
	status := newWHEPFeedStatus(3, 17, "realtime")
	status.setExpectedMedia(true, true)
	status.RecordVideo(true)
	sess.setFeedStatus(status)
	if !m.storeSession(sess) {
		t.Fatal("failed to store test session")
	}
	defer sess.Close()

	req := httptest.NewRequest(http.MethodGet, "/webrtc/session/status-session/status", nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status response = %d: %s", rr.Code, rr.Body.String())
	}
	var response sessionStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := response.Feed
	if got.Generation != 3 || got.Cursor != 17 || got.State != WHEPFeedWaitingKeyframe {
		t.Fatalf("status = %+v", got)
	}
	if !got.ExpectedVideo || !got.ExpectedAudio || got.LastVideoAt.IsZero() || !got.LastAudioAt.IsZero() {
		t.Fatalf("per-kind feed diagnostics = %+v", got)
	}
}

func TestSessionFeedStatusIncludesActualRTPAndRTCPTransportCounters(t *testing.T) {
	trackSender, receiverPC, senderPC, rtpStats, mediaSSRC := testPCPair(t)
	trackSender.Start()
	status := newWHEPFeedStatus(4, 21, "live")
	sess := &Session{pc: senderPC, feedStatus: status, feedVideo: trackSender, feedRTPStats: rtpStats}

	if err := trackSender.WriteSample(media.Sample{
		Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84},
		Duration: 40 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	if err := receiverPC.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: mediaSSRC}}}}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		feed, ok := sess.FeedStatus()
		if ok && feed.RTPPacketsSent > 0 && feed.RTPBytesSent > 0 && feed.RTCPPacketsReceived > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	feed, _ := sess.FeedStatus()
	t.Fatalf("transport counters did not advance: %+v", feed)
}

func TestSessionCloseCapturesTransportCountersBeforeTerminalState(t *testing.T) {
	trackSender, _, senderPC, rtpStats, _ := testPCPair(t)
	status := newWHEPFeedStatus(5, 22, "live")
	sess := newSession("final-transport-stats", senderPC, "live/final-stats", "whep", nil)
	sess.setFeedStatus(status)
	sess.setFeedTracks(trackSender, nil, rtpStats)

	if err := trackSender.WriteSample(media.Sample{
		Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84},
		Duration: 40 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		packets, _ := trackRTPTransportStats(trackSender, rtpStats)
		if packets > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.Close()

	feed := status.Snapshot()
	if feed.State != WHEPFeedClosed {
		t.Fatalf("terminal feed state = %q, want %q", feed.State, WHEPFeedClosed)
	}
	if feed.RTPPacketsSent == 0 || feed.RTPBytesSent == 0 {
		t.Fatalf("terminal transport counters were not captured: %+v", feed)
	}
}

func TestSessionCloseCapturesTransportCountersForExistingTerminalState(t *testing.T) {
	trackSender, _, senderPC, rtpStats, _ := testPCPair(t)
	status := newWHEPFeedStatus(6, 23, "live")
	sess := newSession("existing-terminal-stats", senderPC, "live/existing-terminal", "whep", nil)
	sess.setFeedStatus(status)
	sess.setFeedTracks(trackSender, nil, rtpStats)

	if err := trackSender.WriteSample(media.Sample{
		Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84},
		Duration: 40 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		packets, _ := trackRTPTransportStats(trackSender, rtpStats)
		if packets > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	status.SetError(WHEPFeedSampleWriteFailed, errors.New("terminal before close"))
	sess.Close()

	feed := status.Snapshot()
	if feed.State != WHEPFeedSampleWriteFailed || feed.LastError != "terminal before close" {
		t.Fatalf("terminal feed changed during close: %+v", feed)
	}
	if feed.RTPPacketsSent == 0 || feed.RTPBytesSent == 0 {
		t.Fatalf("existing terminal transport counters were not captured: %+v", feed)
	}
}

func TestClosedWHEPStatusTombstonesAreBounded(t *testing.T) {
	m := &Module{}
	for index := 0; index < maxSessionStatusTombstones+5; index++ {
		id := fmt.Sprintf("closed-%d", index)
		m.storeStatusTombstone(sessionStatusResponse{
			SessionID: id,
			StreamKey: "live/tombstone",
			Role:      "whep",
			Feed:      WHEPFeedStatus{State: WHEPFeedClosed},
		})
	}
	m.statusMu.Lock()
	count := len(m.statusTombstones)
	m.statusMu.Unlock()
	if count != maxSessionStatusTombstones {
		t.Fatalf("status tombstones = %d, want %d", count, maxSessionStatusTombstones)
	}
	if _, ok := m.findSessionStatus("closed-0"); ok {
		t.Fatal("oldest status tombstone was not evicted")
	}
	if _, ok := m.findSessionStatus(fmt.Sprintf("closed-%d", maxSessionStatusTombstones+4)); !ok {
		t.Fatal("newest status tombstone was evicted")
	}
}

func TestHandlePatchBadContentType(t *testing.T) {
	m, s := newTestModule(t)

	// Create a session first
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	sess := newSession("test-patch-id", pc, "live/test", "whep", m)
	m.storeSession(sess)
	defer sess.Close()

	// PATCH with wrong content type
	req := httptest.NewRequest(http.MethodPatch, "/webrtc/session/test-patch-id", strings.NewReader("candidate"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", rr.Code)
	}
	_ = s
}

func TestHandlePatchSuccess(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	sess := newSession("test-patch-ok", pc, "live/test", "whep", m)
	m.storeSession(sess)
	defer sess.Close()

	req := httptest.NewRequest(http.MethodPatch, "/webrtc/session/test-patch-ok", strings.NewReader("candidate"))
	req.Header.Set("Content-Type", "application/trickle-ice-sdpfrag")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rr.Code)
	}
}

func TestHandleDeleteSuccess(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	sess := newSession("test-del-id", pc, "live/test", "whep", m)
	m.storeSession(sess)

	req := httptest.NewRequest(http.MethodDelete, "/webrtc/session/test-del-id", nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	// Session should be removed
	_, ok := m.findSession("test-del-id")
	if ok {
		t.Error("session should be removed after DELETE")
	}
}

func TestIceServersFromConfig(t *testing.T) {
	cfg := &config.Config{
		Stream: config.StreamConfig{RingBufferSize: 256},
		WebRTC: config.WebRTCConfig{
			Enabled: true,
			Listen:  ":0",
			TLS:     func() *bool { b := false; return &b }(),
			ICEServers: []config.ICEServer{
				{
					URLs:       []string{"stun:stun.example.com:3478"},
					Username:   "user",
					Credential: "pass",
				},
				{
					URLs: []string{"turn:turn.example.com:3478"},
				},
			},
		},
	}

	s := newTestServer(t)
	s = nil // We need a server with ICE config

	// Create module with ICE config
	srv := newServerWithConfig(t, cfg)
	m := NewModule()
	if err := m.Init(srv); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer m.Close()

	servers := m.iceServersFromConfig()
	if len(servers) != 2 {
		t.Fatalf("expected 2 ICE servers, got %d", len(servers))
	}
	if servers[0].URLs[0] != "stun:stun.example.com:3478" {
		t.Errorf("URL = %q", servers[0].URLs[0])
	}
	if servers[0].Username != "user" {
		t.Errorf("Username = %q", servers[0].Username)
	}
	_ = s
}

func newServerWithConfig(t *testing.T, cfg *config.Config) *core.Server {
	t.Helper()
	return core.NewServer(cfg)
}

func TestWHIPMissingStreamKey(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/", strings.NewReader("sdp"))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	// Go 1.22+ path routing: empty path should return 400 or 404
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
		t.Errorf("expected 400 or 404, got %d", rr.Code)
	}
}

func TestWHIPRejectsAuthorizationBeforeMutationAndMapsBearer(t *testing.T) {
	m, s := newTestModule(t)

	var authorized *core.EventContext
	s.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublish,
		Mode:  core.HookSync,
		Handler: func(ctx *core.EventContext) error {
			authorized = ctx
			return errors.New("rejected")
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/rejected", strings.NewReader("not read"))
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer publish-token")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
	if authorized == nil || authorized.Params["token"] != "publish-token" {
		t.Fatalf("authorization context = %#v, want bearer token", authorized)
	}
	if _, ok := s.StreamHub().Find("live/rejected"); ok {
		t.Fatal("authorization rejection created a stream")
	}
	if got := sessionCount(m); got != 0 {
		t.Fatalf("stored sessions = %d, want 0", got)
	}
	if got := s.ConnectionCount(); got != 0 {
		t.Fatalf("connection count = %d, want 0", got)
	}
}

func TestWHEPRejectsAuthorizationBeforeSubscriberMutationAndMapsQuery(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/rejected")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id:   "source",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}

	var authorized *core.EventContext
	s.GetEventBus().Register(core.HookRegistration{
		Event: core.EventSubscribe,
		Mode:  core.HookSync,
		Handler: func(ctx *core.EventContext) error {
			authorized = ctx
			return errors.New("rejected")
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/rejected?token=query-token", strings.NewReader("not read"))
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer bearer-token")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
	if authorized == nil || authorized.Params["token"] != "query-token" {
		t.Fatalf("authorization context = %#v, want query token precedence", authorized)
	}
	if got := stream.Subscribers()["webrtc"]; got != 0 {
		t.Fatalf("webrtc subscribers = %d, want 0", got)
	}
	if got := sessionCount(m); got != 0 {
		t.Fatalf("stored sessions = %d, want 0", got)
	}
	if got := s.ConnectionCount(); got != 0 {
		t.Fatalf("connection count = %d, want 0", got)
	}
}

func TestWHEPPeerConnectionFailureReleasesGenerationLease(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/peer-connection-failure")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id:   "source",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64},
	))

	cfg := *s.Config()
	cfg.WebRTC.ICEServers = []config.ICEServer{{URLs: []string{"turn:127.0.0.1:3478"}}}
	s.UpdateConfig(&cfg)

	req := httptest.NewRequest(
		http.MethodPost,
		"/webrtc/whep/live/peer-connection-failure",
		bytes.NewBufferString(createMinimalOffer(t)),
	)
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}
	if got := stream.Subscribers()["webrtc"]; got != 0 {
		t.Fatalf("webrtc subscribers = %d, want 0", got)
	}
	leases := reflect.ValueOf(stream).Elem().FieldByName("generationSubscribers")
	if got := leases.Len(); got != 0 {
		t.Fatalf("generation subscriber lease maps = %d, want 0", got)
	}
	if got := s.ConnectionCount(); got != 0 {
		t.Fatalf("connection count = %d, want 0", got)
	}
}

func TestWHEPMixedRequestedTracksRejectUnsupportedSourceVideo(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/mixed-codec-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id: "mixed-source",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH265,
			VideoSequenceHeader: []byte{0x01},
			AudioCodec:          avframe.CodecG711A,
			SampleRate:          8000,
			Channels:            1,
		},
	}); err != nil {
		t.Fatal(err)
	}

	offer := createH264PCMAReceiveOffer(t)
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/mixed-codec-mismatch", strings.NewReader(offer))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusUnsupportedMediaType, rr.Body.String())
	}
	if got := stream.Subscribers()["webrtc"]; got != 0 {
		t.Fatalf("webrtc subscribers = %d, want 0", got)
	}
	if got := sessionCount(m); got != 0 {
		t.Fatalf("stored sessions = %d, want 0", got)
	}
	if got := s.ConnectionCount(); got != 0 {
		t.Fatalf("connection count = %d, want 0", got)
	}
}

func TestWHEPDisabledSourceVideoMLineDoesNotBlockRequestedAudio(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/audio-requested-only")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id: "mixed-source",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH265,
			VideoSequenceHeader: []byte{0x01},
			AudioCodec:          avframe.CodecG711A,
			SampleRate:          8000,
			Channels:            1,
		},
	}); err != nil {
		t.Fatal(err)
	}

	offer := createH264PCMAReceiveOffer(t)
	offer = strings.Replace(offer, "m=video 9 ", "m=video 0 ", 1)
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/audio-requested-only", strings.NewReader(offer))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
}

func TestWHEPRequestedUnsupportedAudioFailsButOmittedAudioDoesNot(t *testing.T) {
	for _, test := range []struct {
		name       string
		offer      func(*testing.T) string
		wantStatus int
	}{
		{name: "requested", offer: createH264PCMAReceiveOffer, wantStatus: http.StatusUnsupportedMediaType},
		{name: "omitted", offer: createMinimalOffer, wantStatus: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, s := newTestModule(t)
			stream, err := s.StreamHub().GetOrCreate("live/unsupported-audio-" + test.name)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.SetPublisher(&authorizationTestPublisher{
				id: "aac-source",
				info: &avframe.MediaInfo{
					VideoCodec:          avframe.CodecH264,
					VideoSequenceHeader: []byte{0x01},
					AudioCodec:          avframe.CodecAAC,
					AudioSequenceHeader: []byte{0x12, 0x10},
					SampleRate:          44100,
					Channels:            2,
				},
			}); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/unsupported-audio-"+test.name, strings.NewReader(test.offer(t)))
			req.Header.Set("Content-Type", "application/sdp")
			rr := httptest.NewRecorder()
			m.httpSrv.Handler.ServeHTTP(rr, req)
			if rr.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
			if test.wantStatus != http.StatusCreated {
				if got := stream.Subscribers()["webrtc"]; got != 0 {
					t.Fatalf("webrtc subscribers = %d, want 0", got)
				}
				if got := sessionCount(m); got != 0 {
					t.Fatalf("stored sessions = %d, want 0", got)
				}
				if got := s.ConnectionCount(); got != 0 {
					t.Fatalf("connection count = %d, want 0", got)
				}
			}
		})
	}
}

func TestWHEPTrackCreationFailureReturns500AndReleasesResources(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/track-creation-failure")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id: "track-failure-source",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			VideoSequenceHeader: []byte{0x01},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var capturedPC *webrtc.PeerConnection
	factoryCalls := 0
	m.whepTrackFactory = func(pc *webrtc.PeerConnection, _ webrtc.RTPCodecCapability, _, _, _ string) (*TrackSender, error) {
		factoryCalls++
		capturedPC = pc
		return nil, errors.New("injected track creation failure")
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/webrtc/whep/live/track-creation-failure",
		strings.NewReader(createMinimalOffer(t)),
	)
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}
	if factoryCalls != 1 {
		t.Fatalf("track factory calls = %d, want 1", factoryCalls)
	}
	if capturedPC == nil || capturedPC.ConnectionState() != webrtc.PeerConnectionStateClosed {
		t.Fatalf("peer connection state = %v, want closed", capturedPC)
	}
	if got := stream.Subscribers()["webrtc"]; got != 0 {
		t.Fatalf("webrtc subscribers = %d, want 0", got)
	}
	leases := reflect.ValueOf(stream).Elem().FieldByName("generationSubscribers")
	if got := leases.Len(); got != 0 {
		t.Fatalf("generation subscriber lease maps = %d, want 0", got)
	}
	if got := sessionCount(m); got != 0 {
		t.Fatalf("stored sessions = %d, want 0", got)
	}
	if got := s.ConnectionCount(); got != 0 {
		t.Fatalf("connection count = %d, want 0", got)
	}
}

func TestCreateWHEPTrackSenderReturnsRealAddTrackFailure(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := pc.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	m := NewModule()
	sender, err := m.createWHEPTrackSender(
		pc,
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"closed-session",
		"closed-video",
		"liveforge",
	)
	if err == nil || sender != nil {
		t.Fatalf("closed PeerConnection track creation = (%v, %v), want nil sender and error", sender, err)
	}
}

func createH264PCMAReceiveOffer(t *testing.T) string {
	t.Helper()
	clientME := &webrtc.MediaEngine{}
	if err := clientME.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	if err := clientME.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMA,
			ClockRate: 8000,
			Channels:  1,
		},
		PayloadType: 8,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	clientPC, err := webrtc.NewAPI(webrtc.WithMediaEngine(clientME)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, transceiverErr := clientPC.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); transceiverErr != nil {
			t.Fatal(transceiverErr)
		}
	}
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}

func TestWHEPBadContentType(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/test", strings.NewReader("not sdp"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType && rr.Code != http.StatusNotFound {
		t.Errorf("expected 415 or 404, got %d", rr.Code)
	}
}

func TestWHIPBadSDP(t *testing.T) {
	m, _ := newTestModule(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/test", strings.NewReader("not valid sdp"))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad SDP, got %d", rr.Code)
	}
}

func TestWHEPWaitsForPublisherReadinessBeforeNegotiating(t *testing.T) {
	m, s := newTestModule(t)
	stream, err := s.StreamHub().GetOrCreate("live/whep-late-header")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id:   "whep-late-header-publisher",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/whep-late-header", strings.NewReader(createMinimalOffer(t)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		m.httpSrv.Handler.ServeHTTP(rr, req)
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("WHEP negotiated before the publisher sequence header, status=%d", rr.Code)
	case <-time.After(50 * time.Millisecond):
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WHEP did not continue after the publisher became ready")
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP status = %d, want %d: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
}

func TestWHIPAndWHEPSDPOfferBodyLimit(t *testing.T) {
	const limit = 1 << 20
	tests := []struct {
		name       string
		path       string
		size       int
		wantStatus int
		streamKey  string
	}{
		{name: "whip-exact-limit", path: "/webrtc/whip/live/exact", size: limit, wantStatus: http.StatusBadRequest, streamKey: "live/exact"},
		{name: "whip-over-limit", path: "/webrtc/whip/live/oversized", size: limit + 1, wantStatus: http.StatusRequestEntityTooLarge, streamKey: "live/oversized"},
		{name: "whep-exact-limit", path: "/webrtc/whep/live/missing-exact", size: limit, wantStatus: http.StatusNotFound},
		{name: "whep-over-limit", path: "/webrtc/whep/live/missing-oversized", size: limit + 1, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, server := newTestModule(t)
			body := bytes.Repeat([]byte("x"), tt.size)
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/sdp")
			rr := httptest.NewRecorder()
			m.httpSrv.Handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if got := server.ConnectionCount(); got != 0 {
				t.Fatalf("connection count = %d, want 0", got)
			}
			if got := sessionCount(m); got != 0 {
				t.Fatalf("stored sessions = %d, want 0", got)
			}
			if strings.Contains(tt.name, "over-limit") {
				if tt.streamKey != "" {
					if _, ok := server.StreamHub().Find(tt.streamKey); ok {
						t.Fatal("oversized offer created stream state")
					}
				}
			}
		})
	}
}

func TestWHIPWithValidOffer(t *testing.T) {
	m, _ := newTestModule(t)

	offer := createMinimalPublishOffer(t)

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/whiptest", bytes.NewReader([]byte(offer)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Should have Location header
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/webrtc/session/") {
		t.Errorf("Location = %q", loc)
	}

	// Content-Type should be application/sdp
	if !strings.Contains(rr.Header().Get("Content-Type"), "application/sdp") {
		t.Errorf("Content-Type = %q", rr.Header().Get("Content-Type"))
	}
}

func TestWHIPPublisherMethods(t *testing.T) {
	m, _ := newTestModule(t)

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	pub := &WHIPPublisher{
		id:   "test-whip-pub",
		pc:   pc,
		done: make(chan struct{}),
	}
	pub.info.Store(&avframe.MediaInfo{VideoCodec: avframe.CodecH264})

	if pub.ID() != "test-whip-pub" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo().VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v", pub.MediaInfo().VideoCodec)
	}

	// Close should work
	if err := pub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should not panic
	if err := pub.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestWHIPPublisherConcurrentCloseDoesNotPanic(t *testing.T) {
	m, _ := newTestModule(t)
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	for range 200 {
		pub := &WHIPPublisher{id: "concurrent-close", pc: pc, done: make(chan struct{})}
		start := make(chan struct{})
		panics := make(chan any, 32)
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						panics <- recovered
					}
				}()
				<-start
				_ = pub.Close()
			}()
		}
		close(start)
		wg.Wait()
		if len(panics) > 0 {
			t.Fatalf("concurrent WHIP publisher close panicked: %v", <-panics)
		}
	}
}

func TestTrackSenderTrackMethod(t *testing.T) {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := &TrackSender{track: track}
	if ts.Track() != track {
		t.Error("Track() should return the underlying track")
	}
}

func TestTrackSenderSetHandlers(t *testing.T) {
	ts := &TrackSender{}

	// SetNACKHandler
	ts.SetNACKHandler(func(nack *rtcp.TransportLayerNack) {})
	if ts.onNACK == nil {
		t.Error("onNACK should not be nil after SetNACKHandler")
	}

	// SetREMBHandler
	ts.SetREMBHandler(func(bitrate uint64, ssrcs []uint32) {})
	if ts.onREMB == nil {
		t.Error("onREMB should not be nil after SetREMBHandler")
	}
}

func TestRegisterCodecs(t *testing.T) {
	me := &webrtc.MediaEngine{}
	err := registerCodecs(me)
	if err != nil {
		t.Fatalf("registerCodecs: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("AddTransceiverFromKind(audio): %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	for _, codec := range []string{"PCMA/8000", "PCMU/8000"} {
		if !strings.Contains(offer.SDP, codec) {
			t.Errorf("registered WebRTC audio codecs do not include %s", codec)
		}
	}
}

func TestCorsMiddleware(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS Allow-Origin")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("missing POST in Allow-Methods")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Expose-Headers"), "Location") {
		t.Error("missing Location in Expose-Headers")
	}
}

// createMinimalPublishOffer creates a minimal SDP offer with a sendonly video track.
func createMinimalPublishOffer(t *testing.T) string {
	t.Helper()
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	pc.AddTrack(track)

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}
