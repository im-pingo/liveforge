package record

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

type sequencedStorage struct {
	Storage
	next int
}

func (s *sequencedStorage) Create(ctx context.Context, id string, info RecordingInfo) (WriteObject, error) {
	s.next++
	return s.Storage.Create(ctx, fmt.Sprintf("%d-%s", s.next, id), info)
}

func newLifecycleTestModule(t *testing.T, key string) (*Module, *core.Stream, *sequencedStorage) {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		Record: config.RecordConfig{StreamPattern: "*", Format: "flv"},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, err := server.StreamHub().GetOrCreate(key)
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storage := &sequencedStorage{Storage: local}
	t.Cleanup(func() { _ = local.Close() })
	m := NewModule()
	m.server = server
	m.runtime.Store(&recordRuntime{cfg: cfg.Record, storage: storage, template: "record.flv"})
	return m, stream, storage
}

func TestModuleRejectsPublishAfterCloseStarts(t *testing.T) {
	cfg := newTestConfig(t.TempDir())
	server := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	stream, err := server.StreamHub().GetOrCreate("live/late")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&testPublisher{id: "late"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/late"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	count := len(m.sessions)
	m.mu.Unlock()
	if count != 0 {
		t.Fatalf("sessions admitted after close = %d", count)
	}
}

func TestEventBusStopCannotOvertakeBlockedRecordStart(t *testing.T) {
	m, stream, _ := newLifecycleTestModule(t, "live/blocked-start")
	pub := &testPublisher{id: "generation-1"}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	bus := m.server.GetEventBus()
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	bus.Register(core.HookRegistration{
		Event: core.EventPublish,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			close(startEntered)
			<-releaseStart
			return m.onPublish(ctx)
		},
	})
	bus.Register(core.HookRegistration{Event: core.EventPublishStop, Mode: core.HookAsync, Handler: m.onPublishStop})
	ctx := &core.EventContext{StreamKey: "live/blocked-start", PublisherID: pub.ID()}
	bus.EmitAsync(core.EventPublish, ctx)
	<-startEntered
	stream.RemovePublisherIf(pub)
	bus.EmitAsync(core.EventPublishStop, ctx)
	close(releaseStart)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		remaining := len(m.sessions)
		m.mu.Unlock()
		if remaining == 0 {
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("record session survived publish-stop for the same generation")
}

func TestModuleCloseSignalsEverySessionBeforeWaiting(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFinalizer := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFinalizer()
	requests := make(chan struct{}, 2)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		Record: config.RecordConfig{
			Format:         "flv",
			Path:           filepath.Join(t.TempDir(), "{stream_key}.flv"),
			OnFileComplete: config.FileCompleteConfig{URL: callback.URL},
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	sessions := make([]*RecordSession, 0, 2)
	for _, key := range []string{"live/one", "live/two"} {
		stream, _ := server.StreamHub().GetOrCreate(key)
		session, err := NewRecordSession(key, stream, cfg.Record)
		if err != nil {
			t.Fatal(err)
		}
		m.sessions[key] = session
		sessions = append(sessions, session)
		go session.Run()
	}

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("no session entered finalization")
	}
	for i, session := range sessions {
		select {
		case <-session.done:
		default:
			t.Errorf("session %d was not signaled before close waited", i)
		}
	}
	releaseFinalizer()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish")
	}
}

func TestModuleCloseIsBoundedByDrainTimeout(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{DrainTimeout: 20 * time.Millisecond}}
	m := NewModule()
	m.server = core.NewServer(cfg)
	m.sessions["live/stuck"] = &RecordSession{done: make(chan struct{}), finished: make(chan struct{})}
	started := time.Now()
	err := m.Close()
	if err == nil {
		t.Fatal("expected close timeout")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("close exceeded drain bound: %v", elapsed)
	}
}

func TestModuleRepublishStartsReplacementWhenNewPublishArrivesBeforeOldStop(t *testing.T) {
	m, stream, _ := newLifecycleTestModule(t, "live/reorder")
	oldPublisher := &testPublisher{id: "old"}
	if err := stream.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/reorder", PublisherID: oldPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	oldSession := m.sessions["live/reorder"]

	stream.RemovePublisher()
	newPublisher := &testPublisher{id: "new"}
	if err := stream.SetPublisher(newPublisher); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/reorder", PublisherID: newPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	newSession := m.sessions["live/reorder"]
	if newSession == nil || newSession == oldSession {
		t.Fatal("new publish was lost while old session was active")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleStalePublishStopDoesNotStopReplacement(t *testing.T) {
	m, stream, _ := newLifecycleTestModule(t, "live/stale-stop")
	oldPublisher := &testPublisher{id: "old"}
	if err := stream.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/stale-stop", PublisherID: oldPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	oldSession := m.sessions["live/stale-stop"]
	oldSession.Stop()
	oldSession.Wait()

	stream.RemovePublisher()
	newPublisher := &testPublisher{id: "new"}
	if err := stream.SetPublisher(newPublisher); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/stale-stop", PublisherID: newPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	newSession := m.sessions["live/stale-stop"]
	if newSession == nil || newSession == oldSession {
		t.Fatal("replacement session was not installed")
	}
	if err := m.onPublishStop(&core.EventContext{StreamKey: "live/stale-stop", PublisherID: oldPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-newSession.done:
		t.Fatal("stale old stop stopped the replacement session")
	default:
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleRepublishSurvivesFinalizerLongerThanDrainTimeout(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFinalizer := func() { releaseOnce.Do(func() { close(release) }) }
	callbackEntered := make(chan struct{}, 2)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callbackEntered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()
	defer releaseFinalizer()

	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: 30 * time.Millisecond},
		Record: config.RecordConfig{
			StreamPattern: "*",
			Format:        "flv",
			OnFileComplete: config.FileCompleteConfig{
				URL: callback.URL,
			},
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, _ := server.StreamHub().GetOrCreate("live/slow-republish")
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	storage := &sequencedStorage{Storage: local}
	m := NewModule()
	m.server = server
	m.runtime.Store(&recordRuntime{cfg: cfg.Record, storage: storage, template: "record.flv"})
	oldPublisher := &testPublisher{id: "old"}
	if err := stream.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/slow-republish", PublisherID: oldPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	oldSession := m.sessions["live/slow-republish"]

	stream.RemovePublisher()
	newPublisher := &testPublisher{id: "new"}
	if err := stream.SetPublisher(newPublisher); err != nil {
		t.Fatal(err)
	}
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- m.onPublish(&core.EventContext{StreamKey: "live/slow-republish", PublisherID: newPublisher.ID()})
	}()
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		t.Fatal("old record finalizer did not block")
	}
	returnedEarly := false
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatal(err)
		}
		returnedEarly = true
	case <-time.After(3 * cfg.Server.DrainTimeout):
	}
	releaseFinalizer()
	if !returnedEarly {
		select {
		case err := <-publishDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("new publish did not resume after old finalizer")
		}
	}
	replacement := m.sessions["live/slow-republish"]
	if replacement == nil || replacement == oldSession {
		t.Fatalf("new publish was lost: replacement=%p old=%p", replacement, oldSession)
	}
	replacement.Stop()
	if !replacement.WaitUntil(time.Now().Add(time.Second)) {
		t.Fatal("replacement finalizer did not finish")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleCloseAccountsForStopHookBlockedFinalizer(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFinalizer := func() { releaseOnce.Do(func() { close(release) }) }
	callbackEntered := make(chan struct{}, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callbackEntered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()
	defer releaseFinalizer()

	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: 30 * time.Millisecond},
		Record: config.RecordConfig{
			StreamPattern: "*",
			Format:        "flv",
			Path:          filepath.Join(t.TempDir(), "{stream_key}.flv"),
			OnFileComplete: config.FileCompleteConfig{
				URL: callback.URL,
			},
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, _ := server.StreamHub().GetOrCreate("live/blocked-finalizer")
	publisher := &testPublisher{id: "blocked"}
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/blocked-finalizer", PublisherID: publisher.ID()}); err != nil {
		t.Fatal(err)
	}
	session := m.sessions["live/blocked-finalizer"]
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- m.onPublishStop(&core.EventContext{StreamKey: "live/blocked-finalizer", PublisherID: publisher.ID()})
	}()
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		t.Fatal("record finalizer did not block in callback")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close() }()
	for name, done := range map[string]<-chan error{"stop hook": stopDone, "module close": closeDone} {
		select {
		case err := <-done:
			if name == "module close" && err == nil {
				t.Fatal("module close did not report blocked finalizer")
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("%s exceeded drain timeout", name)
		}
	}
	releaseFinalizer()
	if !session.WaitUntil(time.Now().Add(time.Second)) {
		t.Fatal("record finalizer did not finish after release")
	}
}
