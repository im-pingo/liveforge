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
