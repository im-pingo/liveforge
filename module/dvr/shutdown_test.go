package dvr

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleRejectsPublishAfterCloseStarts(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{DrainTimeout: time.Second}, DVR: config.DVRConfig{StreamPattern: "*"}, Stream: config.StreamConfig{RingBufferSize: 16}}
	server := core.NewServer(cfg)
	m := NewModule()
	m.server = server
	m.storePolicy(cfg.DVR)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	stream, _ := server.StreamHub().GetOrCreate("live/late")
	if stream == nil {
		t.Fatal("missing stream")
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/late"}); err != nil {
		t.Fatal(err)
	}
	if len(m.sessions) != 0 {
		t.Fatalf("sessions admitted after close = %d", len(m.sessions))
	}
}

func TestModuleCloseSignalsEverySessionBeforeWaitingAndTimesOut(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{DrainTimeout: 20 * time.Millisecond}}
	m := NewModule()
	m.server = core.NewServer(cfg)
	first := &Session{done: make(chan struct{}), finished: make(chan struct{})}
	second := &Session{done: make(chan struct{}), finished: make(chan struct{})}
	m.sessions["live/one"] = first
	m.sessions["live/two"] = second
	started := time.Now()
	err := m.Close()
	if err == nil {
		t.Fatal("expected close timeout")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("close exceeded drain bound: %v", elapsed)
	}
	for i, session := range []*Session{first, second} {
		select {
		case <-session.done:
		default:
			t.Errorf("session %d was not signaled", i)
		}
	}
}

func TestModuleDoesNotReplaceStoppingSession(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{DrainTimeout: 20 * time.Millisecond}, DVR: config.DVRConfig{StreamPattern: "*"}, Stream: config.StreamConfig{RingBufferSize: 16}}
	server := core.NewServer(cfg)
	stream, _ := server.StreamHub().GetOrCreate("live/reconnect")
	m := NewModule()
	m.server = server
	m.storePolicy(cfg.DVR)
	old := &Session{streamKey: "live/reconnect", stream: stream, index: NewSegmentIndex(), done: make(chan struct{}), finished: make(chan struct{}), metrics: &m.metrics}
	m.sessions["live/reconnect"] = old
	old.Stop()

	if err := m.onPublish(&core.EventContext{StreamKey: "live/reconnect"}); err != nil {
		t.Fatal(err)
	}
	if m.sessions["live/reconnect"] != old {
		t.Fatal("stopping session was replaced before finalization")
	}
}

func TestModuleCloseKeepsStorageOpenUntilWorkersExit(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{DrainTimeout: time.Second}}
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/worker")
	root := t.TempDir()
	session, err := NewSession("live/worker", stream, config.DVRConfig{Path: filepath.Join(root, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "live/worker/probe.ts"), []byte("probe"), 0644); err != nil {
		t.Fatal(err)
	}
	m := NewModule()
	m.server = core.NewServer(cfg)
	m.sessions["live/worker"] = session

	workerErr := make(chan error, 1)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-session.done
		close(session.finished)
		time.Sleep(20 * time.Millisecond)
		file, _, err := session.dir.Open("probe.ts")
		if err == nil {
			err = file.Close()
		}
		workerErr <- err
	}()

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-workerErr; err != nil {
		t.Fatalf("storage closed before worker exit: %v", err)
	}
}
