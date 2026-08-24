package record

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

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

func TestModuleCloseSignalsEverySessionBeforeWaiting(t *testing.T) {
	release := make(chan struct{})
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
	close(release)
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
