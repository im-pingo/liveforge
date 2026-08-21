package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	mu     sync.Mutex
	doc    Document
	err    error
	loads  int
	loaded chan struct{}
}

type writableOnlySource struct {
	*fakeSource
	stores int
}

func (s *writableOnlySource) Store(context.Context, Patch, string) (string, error) {
	s.stores++
	return "unsafe-revision", nil
}

func (s *fakeSource) Name() string { return "fake" }

func (s *fakeSource) Load(context.Context) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loaded != nil {
		select {
		case s.loaded <- struct{}{}:
		default:
		}
	}
	return s.doc, s.err
}

func (s *fakeSource) set(doc Document, err error) {
	s.mu.Lock()
	s.doc = doc
	s.err = err
	s.mu.Unlock()
}

func (s *fakeSource) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func validTestConfig(name string) *Config {
	cfg := defaults()
	cfg.Server.Name = name
	cfg.Stream.RingBufferSize = 1024
	return cfg
}

func TestManagerCurrentUsesCachedSnapshot(t *testing.T) {
	source := &fakeSource{doc: Document{Config: validTestConfig("one"), Revision: "r1", Source: "fake"}}
	manager := NewManager(source, time.Hour, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	loads := source.loadCount()
	for i := 0; i < 100; i++ {
		if got := manager.Current().Effective.Server.Name; got != "one" {
			t.Fatalf("Current name = %q", got)
		}
	}
	if source.loadCount() != loads {
		t.Fatalf("Current triggered source reads: before=%d after=%d", loads, source.loadCount())
	}
}

func TestManagerCopiesSourceConfigBeforePublishing(t *testing.T) {
	cfg := validTestConfig("source-owned")
	cfg.WebRTC.UDPPortRange = []int{10000, 10001}
	source := &fakeSource{doc: Document{Config: cfg, Revision: "r1", Source: "fake"}}
	manager := NewManager(source, time.Hour, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg.Server.Name = "source-mutated"
	cfg.WebRTC.UDPPortRange[0] = 20000
	current := manager.Current()
	if current.Effective.Server.Name != "source-owned" || current.Effective.WebRTC.UDPPortRange[0] != 10000 {
		t.Fatalf("source mutation reached manager snapshot: name=%q ports=%v",
			current.Effective.Server.Name, current.Effective.WebRTC.UDPPortRange)
	}
}

func TestManagerCurrentAndApplyResultAreDeepCopies(t *testing.T) {
	initial := validTestConfig("owned")
	initial.API.Listen = "127.0.0.1:8090"
	initial.WebRTC.UDPPortRange = []int{10000, 10001}
	source := &fakeSource{doc: Document{Config: initial, Revision: "r1", Source: "fake"}}
	manager := NewManager(source, time.Hour, nil)
	result, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	result.Snapshot.Desired.Server.Name = "result-mutated"
	result.Snapshot.Effective.WebRTC.UDPPortRange[0] = 20000
	current := manager.Current()
	if current.Desired.Server.Name != "owned" || current.Effective.WebRTC.UDPPortRange[0] != 10000 {
		t.Fatalf("ApplyResult mutation reached manager snapshot: name=%q ports=%v",
			current.Desired.Server.Name, current.Effective.WebRTC.UDPPortRange)
	}

	returned := manager.Current()
	returned.Desired.Server.Name = "desired-mutated"
	returned.Effective.WebRTC.UDPPortRange[0] = 30000
	if returned.Effective.Server.Name != "owned" || returned.Desired.WebRTC.UDPPortRange[0] != 10000 {
		t.Fatalf("desired/effective copies share memory: desired=%+v effective=%+v", returned.Desired, returned.Effective)
	}
	current = manager.Current()
	if current.Desired.Server.Name != "owned" || current.Effective.WebRTC.UDPPortRange[0] != 10000 {
		t.Fatalf("Current mutation reached manager snapshot: name=%q ports=%v",
			current.Desired.Server.Name, current.Effective.WebRTC.UDPPortRange)
	}

	next := validTestConfig("owned")
	next.API.Listen = "127.0.0.1:9090"
	next.WebRTC.UDPPortRange = []int{10000, 10001}
	source.set(Document{Config: next, Revision: "r2", Source: "fake"}, nil)
	result, err = manager.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PendingRestart) != 1 || len(result.Snapshot.PendingRestart) != 1 {
		t.Fatalf("pending restart = %#v / %#v", result.PendingRestart, result.Snapshot.PendingRestart)
	}
	result.PendingRestart[0] = "result-mutated"
	if result.Snapshot.PendingRestart[0] != "api.listen" {
		t.Fatalf("ApplyResult pending slices share memory: %#v / %#v", result.PendingRestart, result.Snapshot.PendingRestart)
	}
	result.Snapshot.PendingRestart[0] = "snapshot-mutated"
	if got := manager.Current().PendingRestart; len(got) != 1 || got[0] != "api.listen" {
		t.Fatalf("returned pending mutation reached manager snapshot: %#v", got)
	}
}

func TestManagerCurrentConcurrentMutationUsesNoSourceIO(t *testing.T) {
	cfg := validTestConfig("concurrent")
	cfg.WebRTC.UDPPortRange = []int{10000, 10001}
	source := &fakeSource{doc: Document{Config: cfg, Revision: "r1", Source: "fake"}}
	manager := NewManager(source, time.Hour, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	loads := source.loadCount()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				snapshot := manager.Current()
				snapshot.Desired.Server.Name = "caller-owned"
				snapshot.Effective.WebRTC.UDPPortRange[0] = port
			}
		}(20000 + i)
	}
	wg.Wait()
	current := manager.Current()
	if current.Desired.Server.Name != "concurrent" || current.Effective.WebRTC.UDPPortRange[0] != 10000 {
		t.Fatalf("concurrent caller mutation reached manager snapshot: name=%q ports=%v",
			current.Desired.Server.Name, current.Effective.WebRTC.UDPPortRange)
	}
	if source.loadCount() != loads {
		t.Fatalf("Current performed source I/O: before=%d after=%d", loads, source.loadCount())
	}
}

func TestManagerApplyCannotMutatePublishedSnapshot(t *testing.T) {
	initial := validTestConfig("initial")
	initial.API.Listen = "127.0.0.1:8090"
	initial.WebRTC.UDPPortRange = []int{10000, 10001}
	source := &fakeSource{doc: Document{Config: initial, Revision: "r1", Source: "fake"}}
	manager := NewManager(source, time.Hour, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.SetApply(func(_ context.Context, previous, effective *Config, changes ChangeSet) error {
		previous.Server.Name = "mutated previous"
		effective.Server.Name = "mutated effective"
		effective.WebRTC.UDPPortRange[0] = 20000
		delete(changes, "api.listen")
		changes["injected"] = ReloadRestart
		return nil
	})

	next := validTestConfig("next")
	next.API.Listen = "127.0.0.1:9090"
	next.WebRTC.UDPPortRange = []int{10000, 10001}
	source.set(Document{Config: next, Revision: "r2", Source: "fake"}, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := manager.Current()
	if current.Effective.Server.Name != "next" || current.Effective.WebRTC.UDPPortRange[0] != 10000 {
		t.Fatalf("apply callback mutated effective snapshot: name=%q ports=%v",
			current.Effective.Server.Name, current.Effective.WebRTC.UDPPortRange)
	}
	if len(current.PendingRestart) != 1 || current.PendingRestart[0] != "api.listen" {
		t.Fatalf("apply callback mutated change set: %#v", current.PendingRestart)
	}
}

func TestManagerNormalizesDocumentsFromCustomSource(t *testing.T) {
	cfg := validTestConfig("custom source")
	cfg.Auth.Publish.Token.Algorithm = " hs256 "
	cfg.Auth.Subscribe.Token.Algorithm = ""
	source := &fakeSource{doc: Document{Config: cfg, Revision: "r1", Source: "custom"}}
	manager := NewManager(source, time.Hour, nil)

	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Current()
	if got := snapshot.Desired.Auth.Publish.Token.Algorithm; got != "HS256" {
		t.Errorf("desired publish algorithm = %q, want HS256", got)
	}
	if got := snapshot.Effective.Auth.Subscribe.Token.Algorithm; got != "HS256" {
		t.Errorf("effective subscribe algorithm = %q, want HS256", got)
	}
}

func TestManagerRetainsLastKnownGoodConfig(t *testing.T) {
	source := &fakeSource{doc: Document{Config: validTestConfig("one"), Revision: "r1", Source: "fake"}}
	manager := NewManager(source, time.Hour, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := validTestConfig("bad")
	bad.Stream.RingBufferSize = 0
	source.set(Document{Config: bad, Revision: "r2", Source: "fake"}, nil)
	if _, err := manager.Refresh(context.Background()); err == nil {
		t.Fatal("expected invalid refresh to fail")
	}
	if got := manager.Current(); got.Revision != "r1" || got.Effective.Server.Name != "one" {
		t.Fatalf("last known good was replaced: revision=%q name=%q", got.Revision, got.Effective.Server.Name)
	}
}

func TestManagerSeparatesRestartRequiredFromHotChanges(t *testing.T) {
	initial := validTestConfig("one")
	initial.API.Listen = "127.0.0.1:8090"
	source := &fakeSource{doc: Document{Config: initial, Revision: "r1", Source: "fake"}}
	var applied ChangeSet
	manager := NewManager(source, time.Hour, func(_ context.Context, _ *Config, _ *Config, changes ChangeSet) error {
		applied = changes
		return nil
	})
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	next := validTestConfig("one")
	next.Server.LogLevel = "debug"
	next.API.Listen = "127.0.0.1:9090"
	source.set(Document{Config: next, Revision: "r2", Source: "fake"}, nil)
	result, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := manager.Current()
	if got.Desired.API.Listen != "127.0.0.1:9090" || got.Effective.API.Listen != "127.0.0.1:8090" {
		t.Fatalf("desired/effective listen = %q/%q", got.Desired.API.Listen, got.Effective.API.Listen)
	}
	if got.Effective.Server.LogLevel != "debug" {
		t.Fatalf("hot log level = %q", got.Effective.Server.LogLevel)
	}
	if len(result.PendingRestart) != 1 || result.PendingRestart[0] != "api.listen" {
		t.Fatalf("pending restart = %#v", result.PendingRestart)
	}
	if result.Snapshot.Revision != "r2" || result.Snapshot.Desired.API.Listen != "127.0.0.1:9090" ||
		result.Snapshot.Effective.API.Listen != "127.0.0.1:8090" {
		t.Fatalf("accepted snapshot = %+v", result.Snapshot)
	}
	if applied.Class("server.log_level") != ReloadHot || applied.Class("api.listen") != ReloadRestart {
		t.Fatalf("applied classes = %#v", applied)
	}
}

func TestManagerDoesNotPublishWhenApplyFails(t *testing.T) {
	source := &fakeSource{doc: Document{Config: validTestConfig("one"), Revision: "r1", Source: "fake"}}
	manager := NewManager(source, time.Hour, func(context.Context, *Config, *Config, ChangeSet) error { return errors.New("reject") })
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.set(Document{Config: validTestConfig("two"), Revision: "r2", Source: "fake"}, nil)
	if _, err := manager.Refresh(context.Background()); err == nil {
		t.Fatal("expected apply rejection")
	}
	if got := manager.Current().Effective.Server.Name; got != "one" {
		t.Fatalf("rejected config was published: %q", got)
	}
}

func TestManagerUpdateRollsBackSourceWhenApplyFails(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "liveforge.yaml")
	overridePath := filepath.Join(dir, "liveforge.runtime.yaml")
	if err := os.WriteFile(basePath, []byte("server:\n  name: original\nstream:\n  ring_buffer_size: 1024\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := NewFileSource(basePath, overridePath)
	reject := true
	manager := NewManager(source, time.Hour, func(_ context.Context, _, next *Config, _ ChangeSet) error {
		if reject && next.Server.Name == "rejected" {
			return errors.New("module rejected config")
		}
		return nil
	})
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	originalRevision := manager.Current().Revision

	if _, err := manager.Update(context.Background(), Patch{
		"server": map[string]any{"name": "rejected"},
	}, originalRevision); err == nil {
		t.Fatal("expected rejected runtime update to fail")
	}
	loaded, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != originalRevision || loaded.Config.Server.Name != "original" {
		t.Fatalf("source after rollback = revision %q name %q, want %q/original", loaded.Revision, loaded.Config.Server.Name, originalRevision)
	}
	if current := manager.Current(); current.Revision != originalRevision || current.Effective.Server.Name != "original" {
		t.Fatalf("manager after rollback = revision %q name %q", current.Revision, current.Effective.Server.Name)
	}

	reject = false
	result, err := manager.Update(context.Background(), Patch{
		"server": map[string]any{"name": "recovered"},
	}, originalRevision)
	if err != nil {
		t.Fatalf("update with original ETag after rollback failed: %v", err)
	}
	if !result.Changed || manager.Current().Effective.Server.Name != "recovered" {
		t.Fatalf("recovery update result = %+v, name = %q", result, manager.Current().Effective.Server.Name)
	}
}

func TestManagerUpdateRejectsNonTransactionalWritableSource(t *testing.T) {
	source := &writableOnlySource{fakeSource: &fakeSource{
		doc: Document{Config: validTestConfig("one"), Revision: "r1", Source: "writable-only"},
	}}
	manager := NewManager(source, time.Hour, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := manager.Update(context.Background(), Patch{"server": map[string]any{"name": "two"}}, "r1")
	if err == nil || errors.Is(err, ErrSourceReadOnly) || !strings.Contains(err.Error(), "transaction") {
		t.Fatalf("Update error = %v, want explicit transactional-update rejection", err)
	}
	if source.stores != 0 {
		t.Fatalf("non-transactional Store called %d times", source.stores)
	}
	if current := manager.Current(); current.Revision != "r1" || current.Effective.Server.Name != "one" {
		t.Fatalf("manager changed after rejected update: revision=%q name=%q", current.Revision, current.Effective.Server.Name)
	}
}

func TestManagerRunPollsSourceAndStopsWithContext(t *testing.T) {
	source := &fakeSource{
		doc:    Document{Config: validTestConfig("one"), Revision: "r1", Source: "fake"},
		loaded: make(chan struct{}, 1),
	}
	manager := NewManager(source, time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		manager.Run(ctx, nil)
		close(done)
	}()

	select {
	case <-source.loaded:
	case <-time.After(time.Second):
		t.Fatal("manager did not poll source")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("manager did not stop after context cancellation")
	}
}

func TestRuntimeOverridePath(t *testing.T) {
	tests := map[string]string{
		"configs/liveforge.yaml":    "configs/liveforge.runtime.yaml",
		"/etc/liveforge/config.yml": "/etc/liveforge/config.runtime.yml",
		"config":                    "config.runtime.yaml",
	}
	for input, want := range tests {
		if got := RuntimeOverridePath(input); got != want {
			t.Errorf("RuntimeOverridePath(%q) = %q, want %q", input, got, want)
		}
	}
}
