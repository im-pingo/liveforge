package webrtc

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type blockingOfferBody struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	reader  *strings.Reader
}

func newBlockingOfferBody(offer string) *blockingOfferBody {
	return &blockingOfferBody{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		reader:  strings.NewReader(offer),
	}
}

func (b *blockingOfferBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.reader.Read(p)
}

func (b *blockingOfferBody) Close() error { return nil }

var _ io.ReadCloser = (*blockingOfferBody)(nil)

func TestSessionLifecycleSerializesBlockedStartBeforeStop(t *testing.T) {
	for _, test := range []struct {
		name       string
		startEvent core.EventType
		stopEvent  core.EventType
		ctx        *core.EventContext
	}{
		{
			name:       "publish",
			startEvent: core.EventPublish,
			stopEvent:  core.EventPublishStop,
			ctx:        &core.EventContext{StreamKey: "live/whip", PublisherID: "whip-session"},
		},
		{
			name:       "subscribe",
			startEvent: core.EventSubscribe,
			stopEvent:  core.EventSubscribeStop,
			ctx:        &core.EventContext{StreamKey: "live/whep", SubscriberID: "whep-session"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := core.NewEventBus()
			startEntered := make(chan struct{})
			releaseStart := make(chan struct{})
			stopRan := make(chan struct{})
			bus.Register(core.HookRegistration{Event: test.startEvent, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
				close(startEntered)
				<-releaseStart
				return nil
			}})
			bus.Register(core.HookRegistration{Event: test.stopEvent, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
				close(stopRan)
				return nil
			}})
			session := &Session{}

			if !session.startLifecycle(bus, test.startEvent, test.ctx) {
				t.Fatal("session lifecycle start was rejected")
			}
			<-startEntered
			if !session.stopLifecycle(bus, test.stopEvent, test.ctx) {
				t.Fatal("session lifecycle stop was rejected")
			}
			select {
			case <-stopRan:
				t.Fatal("WebRTC stop overtook blocked start")
			case <-time.After(20 * time.Millisecond):
			}
			close(releaseStart)
			select {
			case <-stopRan:
			case <-time.After(time.Second):
				t.Fatal("WebRTC lifecycle did not drain")
			}
		})
	}
}

func TestSessionCloseRunsCleanupExactlyOnce(t *testing.T) {
	session := &Session{
		id:     "close-once",
		role:   "whep",
		done:   make(chan struct{}),
		module: &Module{},
	}
	var cleanupCalls atomic.Int32
	session.setCleanup(func() {
		cleanupCalls.Add(1)
	})

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session.Close()
		}()
	}
	wg.Wait()

	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
	select {
	case <-session.done:
	default:
		t.Fatal("session done was not closed")
	}
}

func TestSessionLifecycleSuppressesStartAfterStop(t *testing.T) {
	bus := core.NewEventBus()
	var events chan core.EventType = make(chan core.EventType, 2)
	bus.Register(core.HookRegistration{Event: core.EventPublish, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
		events <- core.EventPublish
		return nil
	}})
	bus.Register(core.HookRegistration{Event: core.EventPublishStop, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
		events <- core.EventPublishStop
		return nil
	}})
	session := &Session{}
	ctx := &core.EventContext{StreamKey: "live/stopped", PublisherID: "generation"}

	if session.stopLifecycle(bus, core.EventPublishStop, ctx) {
		t.Fatal("stop without a started lifecycle emitted a terminal event")
	}
	if session.startLifecycle(bus, core.EventPublish, ctx) {
		t.Fatal("start was accepted after lifecycle stop")
	}
	select {
	case event := <-events:
		t.Fatalf("start-after-stop emitted event %v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestModuleCloseWaitsForInFlightSetupAndRejectsLateSession(t *testing.T) {
	for _, test := range []struct {
		name  string
		path  string
		event core.EventType
	}{
		{name: "WHIP", path: "/webrtc/whip/live/shutdown", event: core.EventPublish},
		{name: "WHEP", path: "/webrtc/whep/live/shutdown", event: core.EventSubscribe},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, server := newTestModule(t)
			var lifecycleEvents atomic.Int32
			server.GetEventBus().Register(core.HookRegistration{
				Event: test.event,
				Mode:  core.HookAsync,
				Handler: func(*core.EventContext) error {
					lifecycleEvents.Add(1)
					return nil
				},
			})

			body := newBlockingOfferBody("v=0\r\n")
			req := httptest.NewRequest(http.MethodPost, test.path, body)
			req.Header.Set("Content-Type", "application/sdp")
			rr := httptest.NewRecorder()
			handlerDone := make(chan struct{})
			go func() {
				m.httpSrv.Handler.ServeHTTP(rr, req)
				close(handlerDone)
			}()

			select {
			case <-body.entered:
			case <-time.After(time.Second):
				t.Fatal("setup handler did not start reading the offer")
			}

			closeDone := make(chan struct{})
			go func() {
				_ = m.Close()
				close(closeDone)
			}()

			closeReturnedEarly := false
			select {
			case <-closeDone:
				closeReturnedEarly = true
			case <-time.After(50 * time.Millisecond):
			}
			close(body.release)

			select {
			case <-handlerDone:
			case <-time.After(3 * time.Second):
				t.Fatal("setup handler did not return after releasing the offer")
			}
			select {
			case <-closeDone:
			case <-time.After(3 * time.Second):
				t.Fatal("module close did not return after setup handler exited")
			}

			if closeReturnedEarly {
				t.Error("module close returned while a setup handler was still active")
			}
			if rr.Code != http.StatusServiceUnavailable {
				t.Errorf("setup status = %d, want 503: %s", rr.Code, rr.Body.String())
			}
			if got := sessionCount(m); got != 0 {
				t.Errorf("session count = %d, want 0", got)
			}
			if got := server.ConnectionCount(); got != 0 {
				t.Errorf("connection count = %d, want 0", got)
			}
			if got := lifecycleEvents.Load(); got != 0 {
				t.Errorf("lifecycle events = %d, want 0", got)
			}
		})
	}
}

func TestWHIPLifecycleUsesSessionGenerationAndDeleteCleansUp(t *testing.T) {
	m, server := newTestModule(t)
	starts := make(chan *core.EventContext, 1)
	stops := make(chan *core.EventContext, 1)
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublish,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			starts <- ctx
			return nil
		},
	})
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublishStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stops <- ctx
			return nil
		},
	})

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video",
		"lifecycle",
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

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/lifecycle", bytes.NewBufferString(clientPC.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHIP status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	sessionID := strings.TrimPrefix(rr.Header().Get("Location"), "/webrtc/session/")
	if sessionID == "" {
		t.Fatal("WHIP response did not contain a session ID")
	}
	connected := make(chan struct{})
	var connectedOnce sync.Once
	clientPC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
	})
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  rr.Body.String(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("WHIP client state = %s, want connected", clientPC.ConnectionState())
	}
	for range 5 {
		if err := track.WriteSample(media.Sample{
			Data:     []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84},
			Duration: 33 * time.Millisecond,
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := receiveLifecycleContext(t, starts, "WHIP start")
	if start.PublisherID != sessionID {
		t.Fatalf("WHIP start publisher ID = %q, want %q", start.PublisherID, sessionID)
	}

	deleteWebRTCSession(t, m, sessionID, http.StatusOK)
	stop := receiveLifecycleContext(t, stops, "WHIP stop")
	if stop.PublisherID != sessionID {
		t.Fatalf("WHIP stop publisher ID = %q, want %q", stop.PublisherID, sessionID)
	}
	deleteWebRTCSession(t, m, sessionID, http.StatusNotFound)
	assertNoLifecycleContext(t, stops, "duplicate WHIP stop")
	if got := server.ConnectionCount(); got != 0 {
		t.Fatalf("WHIP connection count = %d, want 0", got)
	}
}

func TestWHEPLifecycleUsesSubscriberGenerationAndDeleteCleansUp(t *testing.T) {
	m, server := newTestModule(t)
	stream, err := server.StreamHub().GetOrCreate("live/lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id:   "source",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}
	starts := make(chan *core.EventContext, 1)
	stops := make(chan *core.EventContext, 1)
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventSubscribe,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			starts <- ctx
			return nil
		},
	})
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventSubscribeStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stops <- ctx
			return nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/lifecycle", bytes.NewBufferString(createMinimalOffer(t)))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("WHEP status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	sessionID := strings.TrimPrefix(rr.Header().Get("Location"), "/webrtc/session/")
	if sessionID == "" {
		t.Fatal("WHEP response did not contain a session ID")
	}
	start := receiveLifecycleContext(t, starts, "WHEP start")
	if start.SubscriberID != sessionID {
		t.Fatalf("WHEP start subscriber ID = %q, want %q", start.SubscriberID, sessionID)
	}

	deleteWebRTCSession(t, m, sessionID, http.StatusOK)
	stop := receiveLifecycleContext(t, stops, "WHEP stop")
	if stop.SubscriberID != sessionID {
		t.Fatalf("WHEP stop subscriber ID = %q, want %q", stop.SubscriberID, sessionID)
	}
	deleteWebRTCSession(t, m, sessionID, http.StatusNotFound)
	assertNoLifecycleContext(t, stops, "duplicate WHEP stop")
	if got := stream.Subscribers()["webrtc"]; got != 0 {
		t.Fatalf("WHEP subscriber count = %d, want 0", got)
	}
	if got := server.ConnectionCount(); got != 0 {
		t.Fatalf("WHEP connection count = %d, want 0", got)
	}
}

func deleteWebRTCSession(t *testing.T, m *Module, sessionID string, wantStatus int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/webrtc/session/"+sessionID, nil)
	rr := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(rr, req)
	if rr.Code != wantStatus {
		t.Fatalf("DELETE session status = %d, want %d: %s", rr.Code, wantStatus, rr.Body.String())
	}
}

func receiveLifecycleContext(t *testing.T, events <-chan *core.EventContext, name string) *core.EventContext {
	t.Helper()
	select {
	case ctx := <-events:
		return ctx
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

func assertNoLifecycleContext(t *testing.T, events <-chan *core.EventContext, name string) {
	t.Helper()
	select {
	case ctx := <-events:
		t.Fatalf("unexpected %s: %+v", name, ctx)
	case <-time.After(50 * time.Millisecond):
	}
}
