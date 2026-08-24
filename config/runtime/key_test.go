package runtime

import (
	"context"
	"testing"

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
	source := &sequenceSource{values: []string{"one", "two"}}
	m, err := NewManager(Options{Source: source, Initial: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	key, err := RegisterKey(m, "server.name", ChangeHot, func(cfg *config.Config) string { return cfg.Server.Name })
	if err != nil {
		t.Fatal(err)
	}
	if got := key.Load(); got != "liveforge" {
		t.Fatalf("initial key = %q", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := key.Load(); got != "liveforge" && got != "one" {
		t.Fatalf("key changed before commit: %q", got)
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
