package dvr

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
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

type lifecyclePublisher struct{ id string }

func (p *lifecyclePublisher) ID() string                    { return p.id }
func (p *lifecyclePublisher) MediaInfo() *avframe.MediaInfo { return &avframe.MediaInfo{} }
func (p *lifecyclePublisher) Close() error                  { return nil }

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
	oldSession, err := newSessionWithStorage("live/slow-republish", stream, cfg.DVR, nil, 0, &m.metrics, storage, nil, false)
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
