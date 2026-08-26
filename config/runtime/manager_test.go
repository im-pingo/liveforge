package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

type serializedWriterSource struct {
	active  atomic.Int32
	overlap atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *serializedWriterSource) Load(context.Context, Version) (Snapshot, error) {
	return Snapshot{Data: []byte("server:\n  name: initial\n"), Version: "initial"}, nil
}

func (s *serializedWriterSource) Close() error { return nil }

func (s *serializedWriterSource) Write(ctx context.Context, _ []byte) error {
	if s.active.Add(1) != 1 {
		s.overlap.Store(true)
	}
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		s.active.Add(-1)
		return ctx.Err()
	}
	s.active.Add(-1)
	return nil
}

func TestManagerSerializesSourceWrites(t *testing.T) {
	source := &serializedWriterSource{entered: make(chan struct{}, 2), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(source.release) }) }
	m, err := NewManager(Options{Source: source, Initial: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer release()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- m.Write(context.Background(), []byte("server:\n  name: first\n")) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("first source write did not start")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- m.Write(context.Background(), []byte("server:\n  name: second\n")) }()
	time.Sleep(30 * time.Millisecond)
	if source.overlap.Load() {
		t.Fatal("source writes entered concurrently")
	}
	release()

	for name, done := range map[string]<-chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s write failed: %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s write did not finish", name)
		}
	}
}

func TestManagerWriteHonorsContextWhileWaitingForSourceIO(t *testing.T) {
	source := &serializedWriterSource{entered: make(chan struct{}, 2), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(source.release) }) }
	m, err := NewManager(Options{Source: source, Initial: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer release()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- m.Write(context.Background(), []byte("server:\n  name: first\n")) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("first source write did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Write(ctx, []byte("server:\n  name: second\n")); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting write error=%v, want context deadline", err)
	}
	release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first write failed: %v", err)
	}
}

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

func TestManagerRetainsAcceptedSourceDocument(t *testing.T) {
	document := []byte("# source comment\nserver:\n  name: liveforge\n  # nested comment\n  log_level: info\n")
	source := &mutableSource{snapshot: Snapshot{Data: document, Version: "document-1"}}
	m, err := NewManager(Options{Source: source, Initial: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := m.Snapshot().DesiredDocument
	original := string(document)
	if string(got) != original {
		t.Fatalf("desired document = %q, want original source document %q", got, document)
	}
	document[0] = 'x'
	if string(m.Snapshot().DesiredDocument) != original {
		t.Fatal("published desired document retained source-owned bytes")
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

func TestManagerApplicationFailureDoesNotPublishCandidateOrTypedKeys(t *testing.T) {
	initial := config.Defaults()
	desired := config.Defaults()
	desired.Limits.MaxStreams = 42
	data, err := normalizedBytes(desired)
	if err != nil {
		t.Fatal(err)
	}
	applyErr := errors.New("module rejected policy")
	m, err := NewManager(Options{
		Source:  &mutableSource{snapshot: Snapshot{Data: data, Version: "rejected"}},
		Initial: initial,
		Apply: func(snapshot *ConfigSnapshot, _ ChangeSet) error {
			if snapshot.Config.Limits.MaxStreams != 42 {
				t.Fatalf("candidate max_streams=%d want=42", snapshot.Config.Limits.MaxStreams)
			}
			return applyErr
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

	if err := m.load(context.Background()); !errors.Is(err, applyErr) {
		t.Fatalf("load error=%v want=%v", err, applyErr)
	}
	if got := m.Snapshot().Config.Limits.MaxStreams; got != initial.Limits.MaxStreams {
		t.Fatalf("active max_streams=%d want=%d", got, initial.Limits.MaxStreams)
	}
	if got := key.Load(); got != initial.Limits.MaxStreams {
		t.Fatalf("typed key max_streams=%d want=%d", got, initial.Limits.MaxStreams)
	}
	status := m.Status()
	if status.ActiveVersion.Value == "rejected" || status.ConfigChangesApplicationFailed != 1 || status.ConfigChangesAccepted != 0 {
		t.Fatalf("status after application rejection=%+v", status)
	}
}

func TestManagerCoalescesNotificationsWithoutLosingLatestSnapshot(t *testing.T) {
	initial := config.Defaults()
	source := &mutableSource{}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	notified := make(chan string, 4)
	var once sync.Once
	m, err := NewManager(Options{
		Source:         source,
		Initial:        initial,
		CallbackBuffer: 1,
		OnChange: func(set ChangeSet) error {
			once.Do(func() {
				close(firstStarted)
				<-releaseFirst
			})
			notified <- set.Current.Value
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	loadVersion := func(version string, maxStreams int) {
		t.Helper()
		cfg := config.Defaults()
		cfg.Limits.MaxStreams = maxStreams
		data, err := normalizedBytes(cfg)
		if err != nil {
			t.Fatal(err)
		}
		source.Set(Snapshot{Data: data, Version: version})
		if err := m.load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	loadVersion("one", 1)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first notification did not start")
	}
	loadVersion("two", 2)
	loadVersion("three", 3)
	close(releaseFirst)

	deadline := time.After(time.Second)
	for {
		select {
		case version := <-notified:
			if version == "three" {
				if got := m.Status().DroppedCallbacks; got != 1 {
					t.Fatalf("dropped callbacks=%d want=1", got)
				}
				return
			}
		case <-deadline:
			t.Fatal("latest accepted snapshot was never notified")
		}
	}
}

func TestManagerCountsAcceptedRejectedAndApplicationFailedChanges(t *testing.T) {
	initial := config.Defaults()
	source := &mutableSource{}
	m, err := NewManager(Options{
		Source:  source,
		Initial: initial,
		Apply: func(snapshot *ConfigSnapshot, _ ChangeSet) error {
			if snapshot.Config.Limits.MaxStreams == 3 {
				return errors.New("application failed")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	valid := config.Defaults()
	valid.Limits.MaxStreams = 1
	data, _ := normalizedBytes(valid)
	source.Set(Snapshot{Data: data, Version: "accepted"})
	if err := m.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.Set(Snapshot{Data: []byte("api:\n  console:\n    username: admin\n"), Version: "rejected"})
	if err := m.load(context.Background()); err == nil {
		t.Fatal("expected invalid document rejection")
	}
	failed := config.Defaults()
	failed.Limits.MaxStreams = 3
	data, _ = normalizedBytes(failed)
	source.Set(Snapshot{Data: data, Version: "application-failed"})
	if err := m.load(context.Background()); err == nil {
		t.Fatal("expected application failure")
	}

	status := m.Status()
	if status.ConfigChangesAccepted != 1 || status.ConfigChangesRejected != 1 || status.ConfigChangesApplicationFailed != 1 {
		t.Fatalf("config change counters=%+v", status)
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
