package httpstream

import (
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type managerGenerationPublisher struct{ id string }

func (p *managerGenerationPublisher) ID() string                    { return p.id }
func (p *managerGenerationPublisher) MediaInfo() *avframe.MediaInfo { return nil }
func (p *managerGenerationPublisher) Close() error                  { return nil }

func TestPublishStopBeforeManagerCreationRejectsRetiredGeneration(t *testing.T) {
	tests := []struct {
		name     string
		rejected func(*Module, string, *core.Stream) bool
		present  func(*Module, string) bool
	}{
		{
			name: "hls",
			rejected: func(module *Module, key string, stream *core.Stream) bool {
				return module.getOrCreateHLS(key, stream) == nil
			},
			present: func(module *Module, key string) bool {
				module.hlsMu.Lock()
				defer module.hlsMu.Unlock()
				return module.hlsManagers[key] != nil
			},
		},
		{
			name: "dash",
			rejected: func(module *Module, key string, stream *core.Stream) bool {
				return module.getOrCreateDASH(key, stream) == nil
			},
			present: func(module *Module, key string) bool {
				module.dashMu.Lock()
				defer module.dashMu.Unlock()
				return module.dashManagers[key] != nil
			},
		},
		{
			name: "llhls",
			rejected: func(module *Module, key string, stream *core.Stream) bool {
				return module.getOrCreateLLHLS(key, stream) == nil
			},
			present: func(module *Module, key string) bool {
				module.llhlsMu.Lock()
				defer module.llhlsMu.Unlock()
				return module.llhlsManagers[key] != nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			module, server, _ := newHTTPTestServer(t)
			key := "live/hook-before-create-" + test.name
			stream, err := server.StreamHub().GetOrCreate(key)
			if err != nil {
				t.Fatal(err)
			}
			publisher := &managerGenerationPublisher{id: "publisher-retired"}
			if err := stream.SetPublisher(publisher); err != nil {
				t.Fatal(err)
			}
			snapshot := stream.StartupSnapshot()
			if err := module.onPublishStop(&core.EventContext{
				StreamKey:           key,
				StreamInstanceID:    snapshot.StreamInstanceID,
				PublisherGeneration: snapshot.Generation,
				PublisherID:         snapshot.PublisherID,
			}); err != nil {
				t.Fatal(err)
			}

			if !test.rejected(module, key, stream) {
				t.Fatal("request registered a manager after its exact publisher generation was retired")
			}
			if test.present(module, key) {
				t.Fatal("retired generation remained in the manager lookup registry")
			}
			module.managerMu.Lock()
			tracked := len(module.runningManagers)
			module.managerMu.Unlock()
			if tracked != 0 {
				t.Fatalf("tracked managers after rejected late registration = %d, want 0", tracked)
			}
		})
	}
}

func TestStalePublishStopAfterReplacementDoesNotBlockReplacementGeneration(t *testing.T) {
	tests := []struct {
		name   string
		create func(*Module, string, *core.Stream) (segmentManager, bool)
	}{
		{
			name: "hls",
			create: func(module *Module, key string, stream *core.Stream) (segmentManager, bool) {
				manager := module.getOrCreateHLS(key, stream)
				return manager, manager != nil
			},
		},
		{
			name: "dash",
			create: func(module *Module, key string, stream *core.Stream) (segmentManager, bool) {
				manager := module.getOrCreateDASH(key, stream)
				return manager, manager != nil
			},
		},
		{
			name: "llhls",
			create: func(module *Module, key string, stream *core.Stream) (segmentManager, bool) {
				manager := module.getOrCreateLLHLS(key, stream)
				return manager, manager != nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			module, server, _ := newHTTPTestServer(t)
			key := "live/stale-hook-after-replacement-" + test.name
			stream, err := server.StreamHub().GetOrCreate(key)
			if err != nil {
				t.Fatal(err)
			}
			oldPublisher := &managerGenerationPublisher{id: "publisher-old"}
			if err := stream.SetPublisher(oldPublisher); err != nil {
				t.Fatal(err)
			}
			oldSnapshot := stream.StartupSnapshot()
			if !stream.RemovePublisherIf(oldPublisher) {
				t.Fatal("old publisher was not removed")
			}
			if err := module.onPublishStop(&core.EventContext{
				StreamKey:           key,
				StreamInstanceID:    oldSnapshot.StreamInstanceID,
				PublisherGeneration: oldSnapshot.Generation,
				PublisherID:         oldSnapshot.PublisherID,
			}); err != nil {
				t.Fatal(err)
			}

			replacementPublisher := &managerGenerationPublisher{id: "publisher-replacement"}
			if err := stream.SetPublisher(replacementPublisher); err != nil {
				t.Fatal(err)
			}
			replacement, available := test.create(module, key, stream)
			if !available {
				t.Fatal("retired generation blocked a valid replacement manager")
			}

			if err := module.onPublishStop(&core.EventContext{
				StreamKey:           key,
				StreamInstanceID:    oldSnapshot.StreamInstanceID,
				PublisherGeneration: oldSnapshot.Generation,
				PublisherID:         oldSnapshot.PublisherID,
			}); err != nil {
				t.Fatal(err)
			}
			if got, available := test.create(module, key, stream); !available || got != replacement {
				t.Fatal("stale publisher stop blocked or replaced the active generation manager")
			}
		})
	}
}

func TestManagersAreReplacedAndIgnoreStalePublishStop(t *testing.T) {
	m, srv, _ := newHTTPTestServer(t)
	stream, err := srv.StreamHub().GetOrCreate("live/manager-generation")
	if err != nil {
		t.Fatal(err)
	}

	pubA := &managerGenerationPublisher{id: "publisher-a"}
	if err := stream.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}
	hlsA := m.getOrCreateHLS(stream.Key(), stream)
	dashA := m.getOrCreateDASH(stream.Key(), stream)
	llhlsA := m.getOrCreateLLHLS(stream.Key(), stream)

	if !stream.RemovePublisherIf(pubA) {
		t.Fatal("publisher A was not removed")
	}
	pubB := &managerGenerationPublisher{id: "publisher-b"}
	if err := stream.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	hlsB := m.getOrCreateHLS(stream.Key(), stream)
	dashB := m.getOrCreateDASH(stream.Key(), stream)
	llhlsB := m.getOrCreateLLHLS(stream.Key(), stream)

	if hlsA == hlsB || dashA == dashB || llhlsA == llhlsB {
		t.Fatal("republished stream reused a manager from the previous publisher")
	}

	if err := m.onPublishStop(&core.EventContext{StreamKey: stream.Key(), PublisherID: pubA.ID()}); err != nil {
		t.Fatal(err)
	}

	m.hlsMu.Lock()
	currentHLS := m.hlsManagers[stream.Key()]
	m.hlsMu.Unlock()
	m.dashMu.Lock()
	currentDASH := m.dashManagers[stream.Key()]
	m.dashMu.Unlock()
	m.llhlsMu.Lock()
	currentLLHLS := m.llhlsManagers[stream.Key()]
	m.llhlsMu.Unlock()
	if currentHLS != hlsB || currentDASH != dashB || currentLLHLS != llhlsB {
		t.Fatal("stale publisher stop removed the replacement managers")
	}

	for name, done := range map[string]<-chan struct{}{
		"HLS":   hlsB.done,
		"DASH":  dashB.done,
		"LLHLS": llhlsB.done,
	} {
		select {
		case <-done:
			t.Errorf("stale publisher stop closed replacement %s manager", name)
		default:
		}
	}
}

func TestManagersIgnoreStaleGenerationPublishStop(t *testing.T) {
	m, srv, _ := newHTTPTestServer(t)
	stream, err := srv.StreamHub().GetOrCreate("live/manager-generation-reused-id")
	if err != nil {
		t.Fatal(err)
	}

	pubA := &managerGenerationPublisher{id: "device-1"}
	if err := stream.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}
	genA := stream.StartupSnapshot().Generation
	hlsA := m.getOrCreateHLS(stream.Key(), stream)

	if !stream.RemovePublisherIf(pubA) {
		t.Fatal("publisher A was not removed")
	}
	pubB := &managerGenerationPublisher{id: "device-2"}
	if err := stream.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	genB := stream.StartupSnapshot().Generation
	hlsB := m.getOrCreateHLS(stream.Key(), stream)
	if genA == genB || hlsA == hlsB {
		t.Fatal("replacement did not create a new publisher generation")
	}

	if err := m.onPublishStop(&core.EventContext{
		StreamKey:           stream.Key(),
		PublisherID:         pubA.ID(),
		PublisherGeneration: genA,
	}); err != nil {
		t.Fatal(err)
	}

	m.hlsMu.Lock()
	current := m.hlsManagers[stream.Key()]
	m.hlsMu.Unlock()
	if current != hlsB {
		t.Fatal("stale generation stop removed the replacement manager")
	}
}

func TestManagersReplaceRetiredEmptyPublisherManagerOnRepublish(t *testing.T) {
	m, srv, _ := newHTTPTestServer(t)
	stream, err := srv.StreamHub().GetOrCreate("live/retired-empty-manager")
	if err != nil {
		t.Fatal(err)
	}
	retiredPublisher := &managerGenerationPublisher{id: "publisher-retired"}
	if err := stream.SetPublisher(retiredPublisher); err != nil {
		t.Fatal(err)
	}
	if !stream.RemovePublisherIf(retiredPublisher) {
		t.Fatal("retired publisher was not removed")
	}

	retiredHLS := m.getOrCreateHLS(stream.Key(), stream)
	retiredDASH := m.getOrCreateDASH(stream.Key(), stream)
	retiredLLHLS := m.getOrCreateLLHLS(stream.Key(), stream)

	activePublisher := &managerGenerationPublisher{id: "publisher-active"}
	if err := stream.SetPublisher(activePublisher); err != nil {
		t.Fatal(err)
	}
	activeHLS := m.getOrCreateHLS(stream.Key(), stream)
	activeDASH := m.getOrCreateDASH(stream.Key(), stream)
	activeLLHLS := m.getOrCreateLLHLS(stream.Key(), stream)

	if activeHLS == retiredHLS || activeDASH == retiredDASH || activeLLHLS == retiredLLHLS {
		t.Fatal("republish reused a manager whose generation worker had already exited")
	}
}
