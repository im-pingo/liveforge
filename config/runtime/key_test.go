package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
)

type sequenceSource struct {
	values []string
	index  int
}

func (s *sequenceSource) Load(_ context.Context, _ Version) (Snapshot, error) {
	value := s.values[s.index]
	if s.index < len(s.values)-1 {
		s.index++
	}
	return Snapshot{Data: []byte("server:\n  name: " + value + "\n"), Version: value}, nil
}

func (s *sequenceSource) Close() error { return nil }

func TestKeyLoadChangesOnlyAtSnapshotCommit(t *testing.T) {
	initial := config.Defaults()
	desired := config.Defaults()
	desired.Limits.MaxStreams = initial.Limits.MaxStreams + 17
	data, err := normalizedBytes(desired)
	if err != nil {
		t.Fatal(err)
	}
	source := &mutableSource{snapshot: Snapshot{Data: data, Version: "next"}}
	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	m, err := NewManager(Options{
		Source:  source,
		Initial: initial,
		Apply: func(*ConfigSnapshot, ChangeSet) error {
			close(applyStarted)
			<-releaseApply
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	key, err := RegisterKey(m, "limits.max_streams", ChangeHot, func(cfg *config.Config) int { return cfg.Limits.MaxStreams })
	if err != nil {
		t.Fatal(err)
	}
	if got := key.Load(); got != initial.Limits.MaxStreams {
		t.Fatalf("initial key=%d want=%d", got, initial.Limits.MaxStreams)
	}

	loadDone := make(chan error, 1)
	go func() {
		loadDone <- m.load(context.Background())
	}()
	select {
	case <-applyStarted:
	case <-time.After(time.Second):
		t.Fatal("apply did not start")
	}

	if got := m.Snapshot().Config.Limits.MaxStreams; got != initial.Limits.MaxStreams {
		t.Fatalf("snapshot before commit=%d want=%d", got, initial.Limits.MaxStreams)
	}
	if got := key.Load(); got != initial.Limits.MaxStreams {
		t.Fatalf("typed key before commit=%d want=%d", got, initial.Limits.MaxStreams)
	}

	close(releaseApply)
	select {
	case err := <-loadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("load did not finish after apply")
	}

	if got := m.Snapshot().Config.Limits.MaxStreams; got != desired.Limits.MaxStreams {
		t.Fatalf("snapshot after commit=%d want=%d", got, desired.Limits.MaxStreams)
	}
	if got := key.Load(); got != desired.Limits.MaxStreams {
		t.Fatalf("typed key after commit=%d want=%d", got, desired.Limits.MaxStreams)
	}
}

func TestDuplicateKeyRegistrationFails(t *testing.T) {
	m, err := NewManager(Options{Source: &sequenceSource{values: []string{"one"}}, Initial: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := RegisterKey(m, "server.name", ChangeHot, func(cfg *config.Config) string { return cfg.Server.Name }); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterKey(m, "server.name", ChangeHot, func(cfg *config.Config) string { return cfg.Server.Name }); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}
