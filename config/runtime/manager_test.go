package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
)

type mutableSource struct {
	mu       sync.Mutex
	snapshot Snapshot
}

func (s *mutableSource) Load(context.Context, Version) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot, nil
}

func (s *mutableSource) Close() error { return nil }

func (s *mutableSource) Set(snapshot Snapshot) {
	s.mu.Lock()
	s.snapshot = snapshot
	s.mu.Unlock()
}

type blockingSource struct {
	release chan struct{}
	err     error
}

func (s *blockingSource) Load(ctx context.Context, previous Version) (Snapshot, error) {
	select {
	case <-s.release:
		if s.err != nil {
			return Snapshot{}, s.err
		}
		return Snapshot{Data: []byte("server:\n  name: refreshed\n"), Version: "2"}, nil
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

func (s *blockingSource) Close() error { return nil }

func TestSnapshotReadDoesNotWaitForRefresh(t *testing.T) {
	source := &blockingSource{release: make(chan struct{})}
	m, err := NewManager(Options{
		Source:  source,
		Initial: &config.Config{Server: config.ServerConfig{Name: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	done := make(chan struct{})
	go func() {
		_ = m.Refresh(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("refresh call blocked while source I/O was in progress")
	}
	if got := m.Snapshot().Config.Server.Name; got != "initial" {
		t.Fatalf("snapshot name = %q", got)
	}
	close(source.release)
}

func TestFailedRefreshRetainsLastValidSnapshot(t *testing.T) {
	source := &blockingSource{release: make(chan struct{}), err: errors.New("source unavailable")}
	m, err := NewManager(Options{
		Source:  source,
		Initial: &config.Config{Server: config.ServerConfig{Name: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_ = m.Refresh(context.Background())
	close(source.release)
	time.Sleep(20 * time.Millisecond)
	if got := m.Snapshot().Config.Server.Name; got != "initial" {
		t.Fatalf("snapshot changed after failed refresh: %q", got)
	}
	if m.Status().ConsecutiveFailures == 0 {
		t.Fatal("expected source failure status")
	}
}

func TestManagerPublishesEffectiveConfigAndKeepsDesiredRestartValuesPending(t *testing.T) {
	initial := config.Defaults()
	initial.RTMP.Listen = ":1935"
	desired := config.Defaults()
	desired.RTMP.Listen = ":2935"
	desired.Limits.MaxStreams = 12
	data, err := normalizedBytes(desired)
	if err != nil {
		t.Fatal(err)
	}
	source := &mutableSource{snapshot: Snapshot{Data: data, Version: "one"}}
	m, err := NewManager(Options{Source: source, Initial: initial})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.load(context.Background()); err != nil {
		t.Fatal(err)
	}

	snapshot := m.Snapshot()
	if snapshot.Config.RTMP.Listen != ":1935" {
		t.Fatalf("effective RTMP listen=%q want bootstrap value", snapshot.Config.RTMP.Listen)
	}
	if snapshot.DesiredConfig == nil || snapshot.DesiredConfig.RTMP.Listen != ":2935" {
		t.Fatalf("desired RTMP listen was not retained: %+v", snapshot.DesiredConfig)
	}
	if snapshot.Config.Limits.MaxStreams != 12 {
		t.Fatalf("hot max_streams=%d want=12", snapshot.Config.Limits.MaxStreams)
	}
	if got := m.Status().PendingRestart; len(got) != 1 || got[0] != "rtmp.listen" {
		t.Fatalf("pending restart=%v", got)
	}

	desired.Limits.MaxStreams = 24
	data, err = normalizedBytes(desired)
	if err != nil {
		t.Fatal(err)
	}
	source.Set(Snapshot{Data: data, Version: "two"})
	if err := m.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().Config.Limits.MaxStreams; got != 24 {
		t.Fatalf("second hot max_streams=%d want=24", got)
	}
	if got := m.Status().PendingRestart; len(got) != 1 || got[0] != "rtmp.listen" {
		t.Fatalf("pending restart was lost after hot refresh: %v", got)
	}
}

func TestManagerRejectsImmutableChanges(t *testing.T) {
	initial := config.Defaults()
	desired := config.Defaults()
	desired.Server.Name = "different-identity"
	data, err := normalizedBytes(desired)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{
		Source:  &mutableSource{snapshot: Snapshot{Data: data, Version: "immutable"}},
		Initial: initial,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.load(context.Background()); !errors.Is(err, ErrImmutableChange) {
		t.Fatalf("load error=%v want ErrImmutableChange", err)
	}
	if got := m.Snapshot().Config.Server.Name; got != initial.Server.Name {
		t.Fatalf("immutable change was published: %q", got)
	}
	if m.Status().ConsecutiveFailures != 1 {
		t.Fatalf("immutable rejection was not observable: %+v", m.Status())
	}
}

func BenchmarkSnapshotRead(b *testing.B) {
	m, err := NewManager(Options{Source: &blockingSource{release: make(chan struct{})}, Initial: &config.Config{}})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	for i := 0; i < b.N; i++ {
		if m.Snapshot() == nil {
			b.Fatal("missing snapshot")
		}
	}
}
