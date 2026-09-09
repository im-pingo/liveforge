package core

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

var ErrUnknownSimulcastLayer = errors.New("unknown simulcast layer")

// SimulcastFamily owns at most two private streams beside its canonical parent.
// Only the parent is registered in StreamHub and emits publisher lifecycle events.
type SimulcastFamily struct {
	parent     *Stream
	publisher  Publisher
	generation uint64
	ordered    []string
	layers     map[string]*simulcastLayer
	pause      bool
	closed     atomic.Bool
}

type simulcastLayer struct {
	stream    *Stream
	publisher Publisher
	mu        sync.Mutex
	users     int
	epoch     uint64
}

type simulcastPublisher struct {
	id   string
	info avframe.MediaInfo
}

func (p *simulcastPublisher) ID() string                    { return p.id }
func (p *simulcastPublisher) MediaInfo() *avframe.MediaInfo { return &p.info }
func (p *simulcastPublisher) Close() error                  { return nil }

// AttachSimulcast binds independently buffered encodings to the current parent
// publisher. max_bitrate ranks layers; it does not configure an upstream encoder.
func (s *Stream) AttachSimulcast(pub Publisher, layers []config.LayerConfig, pause bool) (*SimulcastFamily, error) {
	if err := config.ValidateSimulcastConfig(config.SimulcastConfig{Enabled: true, Layers: layers}); err != nil {
		return nil, err
	}
	ordered := append([]config.LayerConfig(nil), layers...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].MaxBitrate != ordered[j].MaxBitrate {
			return ordered[i].MaxBitrate > ordered[j].MaxBitrate
		}
		return ordered[i].RID < ordered[j].RID
	})
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.publisherMatchesLocked(pub) || s.simulcast != nil {
		return nil, fmt.Errorf("simulcast publisher is unavailable or already attached")
	}
	f := &SimulcastFamily{parent: s, publisher: pub, generation: s.publisherGeneration, layers: make(map[string]*simulcastLayer), pause: pause}
	for i, cfg := range ordered {
		stream, publisher := s, pub
		if i != 0 {
			childCfg := s.config
			childCfg.NoPublisherTimeout, childCfg.IdleTimeout = 0, 0
			stream = NewStream(s.key, childCfg, s.limits, nil)
			stream.publisherGeneration = s.publisherGeneration - 1
			if s.transcodeManager != nil {
				stream.transcodeManager = NewTranscodeManager(stream, audiocodec.Global(), childCfg.RingBufferSize)
			}
			info := cloneMediaInfo(s.mediaInfo)
			publisher = &simulcastPublisher{id: pub.ID() + "/rid/" + cfg.RID, info: info}
			if err := stream.SetPublisher(publisher); err != nil {
				stream.Close()
				f.closePrivateLayers()
				return nil, err
			}
		}
		f.ordered = append(f.ordered, cfg.RID)
		f.layers[cfg.RID] = &simulcastLayer{stream: stream, publisher: publisher, epoch: 1}
	}
	s.simulcast = f
	return f, nil
}

func (s *Stream) Simulcast() *SimulcastFamily {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.simulcast
}

func (f *SimulcastFamily) Active() bool { return !f.closed.Load() }

func (f *SimulcastFamily) Generation() uint64 { return f.generation }

func (f *SimulcastFamily) updatePrivatePolicy(cfg config.StreamConfig, limits config.LimitsConfig) {
	cfg.NoPublisherTimeout, cfg.IdleTimeout = 0, 0
	for _, rid := range f.ordered[1:] {
		f.layers[rid].stream.UpdatePolicy(cfg, limits)
	}
}

func (f *SimulcastFamily) Layer(rid string) (*Stream, bool) {
	layer, ok := f.layers[rid]
	if !ok {
		return nil, false
	}
	return layer.stream, true
}

func (f *SimulcastFamily) closePrivateLayers() {
	if f.closed.Swap(true) {
		return
	}
	for _, layer := range f.layers {
		if layer.stream != f.parent {
			layer.stream.Close()
		}
	}
}

func (f *SimulcastFamily) Processing(rid string) (bool, uint64) {
	layer, ok := f.layers[rid]
	if !ok || f.closed.Load() {
		return false, 0
	}
	layer.mu.Lock()
	defer layer.mu.Unlock()
	return !f.pause || layer.stream == f.parent || layer.users > 0, layer.epoch
}

// Acquire reserves the parent's shared subscriber budget before waking a layer.
// Call this before waiting for headers; suspended layers cannot produce them.
func (f *SimulcastFamily) Acquire(selection, protocol string) (*Stream, string, func(), error) {
	rid := selection
	switch selection {
	case "", "auto", "high":
		rid = f.ordered[0]
	case "low":
		rid = f.ordered[len(f.ordered)-1]
	}
	layer, ok := f.layers[rid]
	if !ok {
		return nil, "", nil, fmt.Errorf("%w %q", ErrUnknownSimulcastLayer, selection)
	}
	releaseParent, err := f.parent.AddSubscriberForGeneration(protocol, f.generation)
	if err != nil {
		return nil, "", nil, err
	}
	if f.closed.Load() {
		releaseParent()
		return nil, "", nil, fmt.Errorf("simulcast generation ended")
	}
	if layer.stream == f.parent {
		return f.parent, rid, releaseParent, nil
	}
	layer.mu.Lock()
	releaseChild, err := layer.stream.AddSubscriberForGeneration(protocol, f.generation)
	if err != nil {
		layer.mu.Unlock()
		releaseParent()
		return nil, "", nil, err
	}
	if layer.users == 0 && f.pause {
		layer.stream.resetSimulcastVideo()
		layer.epoch++
	}
	layer.users++
	layer.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			layer.mu.Lock()
			layer.users--
			layer.mu.Unlock()
			releaseChild()
			releaseParent()
		})
	}
	return layer.stream, rid, release, nil
}

func (s *Stream) resetSimulcastVideo() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gopCache, s.gopStarts, s.gopBytes, s.gopMinDTS, s.gopMaxDTS = nil, nil, nil, nil, nil
	s.gopGeneration, s.gopCacheSealed = 0, false
	s.videoSeqHeader = nil
	s.mediaInfo.VideoSequenceHeader = nil
	s.startupReady = s.startupReadyLocked()
	s.signalStartupStateChangedLocked()
}

func (f *SimulcastFamily) WriteVideo(rid string, frame *avframe.AVFrame) bool {
	_, epoch := f.Processing(rid)
	return f.WriteVideoEpoch(rid, epoch, frame)
}

// WriteVideoEpoch rejects a depacketizer's retained output after pause/resume.
func (f *SimulcastFamily) WriteVideoEpoch(rid string, epoch uint64, frame *avframe.AVFrame) bool {
	layer, ok := f.layers[rid]
	if !ok || frame == nil || !frame.MediaType.IsVideo() || f.closed.Load() {
		return false
	}
	layer.mu.Lock()
	defer layer.mu.Unlock()
	if layer.epoch != epoch || f.pause && layer.stream != f.parent && layer.users == 0 {
		return false
	}
	if layer.stream == f.parent {
		return f.parent.WriteFrameForPublisher(f.publisher, frame)
	}
	accepted := false
	f.parent.WithActivePublisher(f.publisher, func() { accepted = layer.stream.WriteFrameForPublisher(layer.publisher, frame) })
	return accepted
}

// Audio shares immutable payload bytes, never envelopes: Stream annotates each
// envelope with its own audio epoch and provenance during publication.
func (f *SimulcastFamily) WriteAudio(frame *avframe.AVFrame) bool {
	if frame == nil || !frame.MediaType.IsAudio() || f.closed.Load() {
		return false
	}
	copyFrame := *frame
	accepted := f.parent.WriteFrameForPublisher(f.publisher, &copyFrame)
	for _, rid := range f.ordered[1:] {
		layer := f.layers[rid]
		copyFrame := *frame
		f.parent.WithActivePublisher(f.publisher, func() { layer.stream.WriteFrameForPublisher(layer.publisher, &copyFrame) })
	}
	return accepted
}
