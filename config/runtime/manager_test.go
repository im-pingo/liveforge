package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
)

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
