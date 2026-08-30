package dvr

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/localfs"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type blockingSyncSegment struct {
	segmentFile
	entered chan<- struct{}
	release <-chan struct{}
}

func (f *blockingSyncSegment) Sync() error {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-f.release
	return f.segmentFile.Sync()
}

type lifecyclePublisher struct {
	id   string
	info *avframe.MediaInfo
}

type observedDVRStorage struct {
	*dvrStorage
	opened  chan<- *localfs.Dir
	release <-chan struct{}
}

func (s *observedDVRStorage) openStreamDir(pathTemplate, streamKey string) (*localfs.Dir, string, error) {
	dir, path, err := s.dvrStorage.openStreamDir(pathTemplate, streamKey)
	if err != nil {
		return nil, "", err
	}
	s.opened <- dir
	<-s.release
	return dir, path, nil
}

func (p *lifecyclePublisher) ID() string { return p.id }
func (p *lifecyclePublisher) MediaInfo() *avframe.MediaInfo {
	if p.info == nil {
		return &avframe.MediaInfo{}
	}
	return p.info
}
func (p *lifecyclePublisher) Close() error { return nil }

func TestModulePublishAdmissionDoesNotMixPublisherIdentityAndReplacementSnapshot(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		DVR: config.DVRConfig{
			StreamPattern: "*",
			Path:          filepath.Join(t.TempDir(), "{stream_key}"),
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, err := server.StreamHub().GetOrCreate("live/generation-swap")
	if err != nil {
		t.Fatal(err)
	}
	publisherA := &lifecyclePublisher{
		id:   "publisher-a",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if publisherErr := stream.SetPublisher(publisherA); publisherErr != nil {
		t.Fatal(publisherErr)
	}
	snapshotA := stream.StartupSnapshot()
	eventA := &core.EventContext{
		StreamKey:           stream.Key(),
		StreamInstanceID:    snapshotA.StreamInstanceID,
		PublisherGeneration: snapshotA.Generation,
		PublisherID:         snapshotA.PublisherID,
	}

	storage, err := newDVRStorage(cfg.DVR.Path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewModule()
	m.server = server
	m.storage = storage
	m.storePolicy(cfg.DVR)
	t.Cleanup(func() {
		if closeErr := m.Close(); closeErr != nil {
			t.Errorf("close DVR module: %v", closeErr)
		}
	})
	retained, err := newSessionWithStorage(stream.Key(), stream, snapshotA, cfg.DVR, nil, 0, &m.metrics, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	const retainedProbe = "seg_000000.ts"
	retainedData := []byte("retained-directory-owned-data")
	retainedPath := filepath.Join(retained.dir.Path(), retainedProbe)
	if writeErr := os.WriteFile(retainedPath, retainedData, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	retainedInfo, err := os.Stat(retainedPath)
	if err != nil {
		t.Fatal(err)
	}
	retainedSegment := Segment{
		SeqNum:    0,
		StartTime: retainedInfo.ModTime(),
		Filename:  retainedProbe,
		Size:      retainedInfo.Size(),
		DiskPath:  retainedPath,
	}
	retained.index.Add(retainedSegment)
	retained.publisherID = "retained"
	retained.Stop()
	retained.finish()
	m.sessions[stream.Key()] = retained

	retained.index.mu.Lock()
	publishDone := make(chan error, 1)
	go func() { publishDone <- m.onPublish(eventA) }()
	waitForDVRModuleLock(t, &m.mu)

	stream.RemovePublisherIf(publisherA)
	publisherB := &lifecyclePublisher{
		id:   "publisher-b",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH265},
	}
	if err := stream.SetPublisher(publisherB); err != nil {
		retained.index.mu.Unlock()
		t.Fatal(err)
	}
	snapshotB := stream.StartupSnapshot()
	retained.index.mu.Unlock()
	if err := <-publishDone; err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	installed := m.sessions[stream.Key()]
	m.mu.Unlock()
	if installed != retained {
		t.Fatalf("stale A admission installed session identity=%q generation=%d codec=%v; replacement B is generation=%d codec=%v",
			installed.publisherID, installed.snapshot.Generation, installed.snapshot.MediaInfo.VideoCodec,
			snapshotB.Generation, snapshotB.MediaInfo.VideoCodec)
	}
	assertDVRSegmentReadable(t, retained, retainedSegment, retainedData)

	eventB := &core.EventContext{
		StreamKey:           stream.Key(),
		StreamInstanceID:    snapshotB.StreamInstanceID,
		PublisherGeneration: snapshotB.Generation,
		PublisherID:         snapshotB.PublisherID,
	}
	if err := m.onPublish(eventB); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	installed = m.sessions[stream.Key()]
	m.mu.Unlock()
	if installed == nil || installed == retained || installed.publisherID != publisherB.ID() ||
		installed.snapshot.Generation != snapshotB.Generation || installed.snapshot.MediaInfo.VideoCodec != avframe.CodecH265 {
		t.Fatalf("replacement B session = %#v, want generation=%d publisher=%q codec=%v",
			installed, snapshotB.Generation, publisherB.ID(), avframe.CodecH265)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleRejectsStalePublishAndClosesCandidateOwnedDirectory(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		DVR: config.DVRConfig{
			StreamPattern: "*",
			Path:          filepath.Join(t.TempDir(), "{stream_key}"),
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, err := server.StreamHub().GetOrCreate("live/candidate-owned")
	if err != nil {
		t.Fatal(err)
	}
	publisherA := &lifecyclePublisher{id: "publisher-a", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if publisherErr := stream.SetPublisher(publisherA); publisherErr != nil {
		t.Fatal(publisherErr)
	}
	snapshotA := stream.StartupSnapshot()
	eventA := &core.EventContext{
		StreamKey:           stream.Key(),
		StreamInstanceID:    snapshotA.StreamInstanceID,
		PublisherGeneration: snapshotA.Generation,
		PublisherID:         snapshotA.PublisherID,
	}

	storage, err := newDVRStorage(cfg.DVR.Path)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan *localfs.Dir, 1)
	release := make(chan struct{})
	trackedStorage := &observedDVRStorage{dvrStorage: storage, opened: opened, release: release}
	m := NewModule()
	m.server = server
	m.storage = trackedStorage
	m.storePolicy(cfg.DVR)
	var releaseOnce sync.Once
	releaseConstruction := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseConstruction()
		if err := m.Close(); err != nil {
			t.Errorf("close DVR module: %v", err)
		}
	})

	publishDone := make(chan error, 1)
	go func() { publishDone <- m.onPublish(eventA) }()
	var candidateDir *localfs.Dir
	select {
	case candidateDir = <-opened:
	case <-time.After(time.Second):
		t.Fatal("stale A did not open its candidate directory")
	}
	t.Cleanup(func() { _ = candidateDir.Close() })
	const candidateProbe = "candidate-owned-probe.ts"
	candidateData := []byte("candidate-owned-data")
	if err := os.WriteFile(filepath.Join(candidateDir.Path(), candidateProbe), candidateData, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := candidateDir.Stat(candidateProbe); err != nil {
		t.Fatalf("candidate directory was not usable before rejection: %v", err)
	}

	stream.RemovePublisherIf(publisherA)
	publisherB := &lifecyclePublisher{id: "publisher-b", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH265}}
	if err := stream.SetPublisher(publisherB); err != nil {
		t.Fatal(err)
	}
	releaseConstruction()
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale A admission did not finish")
	}

	m.mu.Lock()
	installed := m.sessions[stream.Key()]
	m.mu.Unlock()
	if installed != nil {
		t.Fatalf("stale A installed session %#v", installed)
	}
	if _, err := candidateDir.Stat(candidateProbe); err == nil {
		t.Fatal("directory opened exclusively by stale A remained usable after rejection")
	}
	if _, err := storage.root.Stat("live/candidate-owned/" + candidateProbe); err != nil {
		t.Fatalf("module-owned storage root closed with stale A candidate: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertDVRSegmentReadable(t *testing.T, session *Session, segment Segment, want []byte) {
	t.Helper()
	file, _, err := session.openIndexedSegment(segment)
	if err != nil {
		t.Fatalf("open retained segment after stale cleanup: %v", err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		t.Fatalf("read retained segment after stale cleanup: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close retained segment after stale cleanup: %v", closeErr)
	}
	if string(data) != string(want) {
		t.Fatalf("retained segment data = %q, want %q", data, want)
	}
}

func waitForDVRModuleLock(t *testing.T, mu *sync.Mutex) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !mu.TryLock() {
			return
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("DVR publish admission did not reach the retained-index phase")
}

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

func TestEventBusStopCannotOvertakeBlockedDVRStart(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		DVR: config.DVRConfig{
			StreamPattern: "*",
			Path:          filepath.Join(t.TempDir(), "{stream_key}"),
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, err := server.StreamHub().GetOrCreate("live/blocked-start")
	if err != nil {
		t.Fatal(err)
	}
	pub := &lifecyclePublisher{id: "generation-1"}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	m := NewModule()
	m.server = server
	m.storePolicy(cfg.DVR)
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	bus := server.GetEventBus()
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

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if err := bus.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	session := m.sessions[ctx.StreamKey]
	m.mu.Unlock()
	if session != nil {
		t.Fatalf("stale DVR session was installed after its publisher retired: %+v", session.Status())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
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

func TestModuleCloseDeadlineIncludesAdmissionLockWait(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{DrainTimeout: 20 * time.Millisecond}}
	m := NewModule()
	m.server = core.NewServer(cfg)

	// Publish setup currently owns this mutex while opening/recovering storage.
	// Close must start its drain deadline before waiting for that ownership.
	m.mu.Lock()
	result := make(chan error, 1)
	started := time.Now()
	go func() { result <- m.Close() }()

	select {
	case err := <-result:
		if err == nil {
			m.mu.Unlock()
			t.Fatal("expected close timeout while admission lock is held")
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			m.mu.Unlock()
			t.Fatalf("close exceeded drain bound before lock release: %v", elapsed)
		}
	case <-time.After(250 * time.Millisecond):
		m.mu.Unlock()
		<-result
		t.Fatal("close waited for admission lock before starting drain timeout")
	}

	m.mu.Unlock()
	select {
	case <-m.closeDone:
	case <-time.After(time.Second):
		t.Fatal("background close did not finish after admission lock release")
	}
}

func TestModuleRepublishStartsReplacementWhenNewPublishArrivesBeforeOldStop(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		DVR: config.DVRConfig{
			StreamPattern: "*",
			Path:          filepath.Join(t.TempDir(), "{stream_key}"),
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, _ := server.StreamHub().GetOrCreate("live/reorder")
	oldPublisher := &lifecyclePublisher{id: "old"}
	if err := stream.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	m := NewModule()
	m.server = server
	m.storePolicy(cfg.DVR)
	if err := m.onPublish(&core.EventContext{StreamKey: "live/reorder", PublisherID: oldPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	oldSession := m.sessions["live/reorder"]

	stream.RemovePublisher()
	newPublisher := &lifecyclePublisher{id: "new"}
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
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		DVR: config.DVRConfig{
			StreamPattern: "*",
			Path:          filepath.Join(t.TempDir(), "{stream_key}"),
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, _ := server.StreamHub().GetOrCreate("live/stale-stop")
	oldPublisher := &lifecyclePublisher{id: "old"}
	if err := stream.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	m := NewModule()
	m.server = server
	m.storePolicy(cfg.DVR)
	if err := m.onPublish(&core.EventContext{StreamKey: "live/stale-stop", PublisherID: oldPublisher.ID()}); err != nil {
		t.Fatal(err)
	}
	oldSession := m.sessions["live/stale-stop"]
	oldSession.Stop()
	oldSession.Wait()

	stream.RemovePublisher()
	newPublisher := &lifecyclePublisher{id: "new"}
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
	if !newSession.IsLive() {
		t.Fatal("stale old stop stopped the replacement session")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleStaleStreamInstanceStopDoesNotStopReplacementWithSamePublisherID(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: time.Second},
		DVR: config.DVRConfig{
			StreamPattern: "*",
			Path:          filepath.Join(t.TempDir(), "{stream_key}"),
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	m := NewModule()
	m.server = server
	m.storePolicy(cfg.DVR)

	const streamKey = "live/recreated"
	oldStream, err := server.StreamHub().GetOrCreate(streamKey)
	if err != nil {
		t.Fatal(err)
	}
	oldPublisher := &lifecyclePublisher{id: "device-1"}
	if publisherErr := oldStream.SetPublisher(oldPublisher); publisherErr != nil {
		t.Fatal(publisherErr)
	}
	oldSnapshot := oldStream.StartupSnapshot()
	oldCtx := &core.EventContext{
		StreamKey:           streamKey,
		StreamInstanceID:    oldSnapshot.StreamInstanceID,
		PublisherGeneration: oldSnapshot.Generation,
		PublisherID:         oldSnapshot.PublisherID,
	}
	if publishErr := m.onPublish(oldCtx); publishErr != nil {
		t.Fatal(publishErr)
	}

	server.StreamHub().Remove(streamKey)
	newStream, err := server.StreamHub().GetOrCreate(streamKey)
	if err != nil {
		t.Fatal(err)
	}
	newPublisher := &lifecyclePublisher{id: "device-1"}
	if err := newStream.SetPublisher(newPublisher); err != nil {
		t.Fatal(err)
	}
	newSnapshot := newStream.StartupSnapshot()
	newCtx := &core.EventContext{
		StreamKey:           streamKey,
		StreamInstanceID:    newSnapshot.StreamInstanceID,
		PublisherGeneration: newSnapshot.Generation,
		PublisherID:         newSnapshot.PublisherID,
	}
	if err := m.onPublish(newCtx); err != nil {
		t.Fatal(err)
	}
	newSession := m.sessions[streamKey]
	if newSession == nil {
		t.Fatal("replacement DVR session was not installed")
	}

	if err := m.onPublishStop(oldCtx); err != nil {
		t.Fatal(err)
	}
	if !newSession.IsLive() {
		t.Fatal("stale stream-instance stop stopped the replacement DVR session")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleRepublishSurvivesFinalizerLongerThanDrainTimeout(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: 30 * time.Millisecond},
		DVR: config.DVRConfig{
			StreamPattern: "*",
			Path:          filepath.Join(t.TempDir(), "{stream_key}"),
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	stream, _ := server.StreamHub().GetOrCreate("live/slow-republish")
	oldPublisher := &lifecyclePublisher{id: "old"}
	if err := stream.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	storage, err := newDVRStorage(cfg.DVR.Path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewModule()
	m.server = server
	m.storage = storage
	m.storePolicy(cfg.DVR)
	oldSession, err := newSessionWithStorage("live/slow-republish", stream, stream.StartupSnapshot(), cfg.DVR, nil, 0, &m.metrics, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFinalizer := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFinalizer()
	oldSession.wrapSegment = func(file segmentFile) segmentFile {
		return &blockingSyncSegment{segmentFile: file, entered: entered, release: release}
	}
	oldSession.processFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}})
	m.sessions["live/slow-republish"] = oldSession
	go oldSession.Run()

	stream.RemovePublisher()
	newPublisher := &lifecyclePublisher{id: "new"}
	if err := stream.SetPublisher(newPublisher); err != nil {
		t.Fatal(err)
	}
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- m.onPublish(&core.EventContext{StreamKey: "live/slow-republish", PublisherID: newPublisher.ID()})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old DVR finalizer did not block")
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
	if replacement := m.sessions["live/slow-republish"]; replacement == nil || replacement == oldSession || !replacement.IsLive() {
		t.Fatalf("new publish was lost: replacement=%p old=%p", replacement, oldSession)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
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

func TestModuleCloseAndStopHookBoundBlockedFinalizer(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{DrainTimeout: 30 * time.Millisecond}}
	server := core.NewServer(cfg)
	stream, _ := server.StreamHub().GetOrCreate("live/blocked-finalizer")
	root := t.TempDir()
	session, err := NewSession("live/blocked-finalizer", stream, config.DVRConfig{Path: filepath.Join(root, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFinalizer := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFinalizer()
	session.wrapSegment = func(file segmentFile) segmentFile {
		return &blockingSyncSegment{segmentFile: file, entered: entered, release: release}
	}
	session.processFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}})
	m := NewModule()
	m.server = server
	m.sessions["live/blocked-finalizer"] = session
	go session.Run()

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- m.onPublishStop(&core.EventContext{StreamKey: "live/blocked-finalizer"})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("DVR finalizer did not block in Sync")
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
		t.Fatal("DVR finalizer did not finish after release")
	}
}
