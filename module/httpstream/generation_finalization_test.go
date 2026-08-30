package httpstream

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type generationManagerHarness struct {
	manager    segmentManager
	stopped    <-chan struct{}
	segmentCnt func() int
	output     func() []byte
	registered func() bool
}

func TestTransformedActiveGenerationEndDiscardsPartialState(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *core.Stream, *controlledSegmentInput)
	}{
		{
			name: "hls",
			run: func(t *testing.T, stream *core.Stream, input *controlledSegmentInput) {
				manager := NewHLSManager(stream.Key(), "/live/transformed-hls", 6, 5)
				manager.inputFactory = input.factory
				manager.Run(stream)
				if got := manager.SegmentCount(); got != 0 {
					t.Fatalf("HLS finalized %d partial segments after active transformed EOF", got)
				}
			},
		},
		{
			name: "llhls",
			run: func(t *testing.T, stream *core.Stream, input *controlledSegmentInput) {
				manager := NewLLHLSManager(stream.Key(), "/live/transformed-llhls", 0.1, 1, 5, "ts")
				manager.segmenter.inputFactory = input.factory
				manager.Run(stream)
				manager.mu.Lock()
				defer manager.mu.Unlock()
				if len(manager.segments) != 0 || len(manager.currentParts) != 0 {
					t.Fatalf("LL-HLS retained %d segments/%d parts after active transformed EOF", len(manager.segments), len(manager.currentParts))
				}
			},
		},
		{
			name: "dash",
			run: func(t *testing.T, stream *core.Stream, input *controlledSegmentInput) {
				manager := NewDASHManager(stream.Key(), "/live/transformed-dash", 6, 5)
				manager.inputFactory = input.factory
				manager.Run(stream)
				if got := manager.SegmentCount(); got != 0 || !manager.isRetired() {
					t.Fatalf("DASH active transformed EOF segments/retired = %d/%v, want 0/true", got, manager.isRetired())
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := newVideoStreamWithoutGOPCache(t)
			input := newControlledSegmentInput(4, true)
			input.ring.Write(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
				0, 0, []byte{0, 0, 0, 2, 0x65, 0x70},
			))
			input.ring.Write(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				200, 200, []byte{0, 0, 0, 2, 0x41, 0x71},
			))
			input.ring.Close()
			test.run(t, stream, input)
			stream.RingBuffer().Close()
		})
	}
}

func TestModulePublishStopDrainsExactManagerGeneration(t *testing.T) {
	tests := []struct {
		name   string
		create func(*Module, *core.Stream) generationManagerHarness
	}{
		{
			name: "hls",
			create: func(module *Module, stream *core.Stream) generationManagerHarness {
				manager := module.getOrCreateHLS(stream.Key(), stream)
				return generationManagerHarness{
					manager: manager, stopped: manager.done, segmentCnt: manager.SegmentCount,
					output: func() []byte {
						manager.mu.RLock()
						defer manager.mu.RUnlock()
						var output []byte
						for _, segment := range manager.segments {
							output = append(output, segment.Data...)
						}
						return output
					},
					registered: func() bool {
						module.hlsMu.Lock()
						defer module.hlsMu.Unlock()
						return module.hlsManagers[stream.Key()] != nil
					},
				}
			},
		},
		{
			name: "dash",
			create: func(module *Module, stream *core.Stream) generationManagerHarness {
				manager := module.getOrCreateDASH(stream.Key(), stream)
				return generationManagerHarness{
					manager: manager, stopped: manager.done, segmentCnt: manager.SegmentCount,
					output: func() []byte {
						manager.mu.RLock()
						defer manager.mu.RUnlock()
						var output []byte
						for _, segment := range manager.audioSegments {
							output = append(output, segment.Data...)
						}
						return output
					},
					registered: func() bool {
						module.dashMu.Lock()
						defer module.dashMu.Unlock()
						return module.dashManagers[stream.Key()] != nil
					},
				}
			},
		},
		{
			name: "llhls",
			create: func(module *Module, stream *core.Stream) generationManagerHarness {
				manager := module.getOrCreateLLHLS(stream.Key(), stream)
				return generationManagerHarness{
					manager: manager, stopped: manager.done, segmentCnt: manager.SegmentCount,
					output: func() []byte {
						manager.mu.Lock()
						defer manager.mu.Unlock()
						var output []byte
						for _, segment := range manager.segments {
							for _, part := range segment.Parts {
								output = append(output, part.Data...)
							}
						}
						return output
					},
					registered: func() bool {
						module.llhlsMu.Lock()
						defer module.llhlsMu.Unlock()
						return module.llhlsManagers[stream.Key()] != nil
					},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.HTTP.HLS = config.HLSConfig{SegmentDuration: 0.2, PlaylistSize: 20}
			cfg.HTTP.DASH = config.DASHConfig{SegmentDuration: 0.2, PlaylistSize: 20}
			cfg.HTTP.LLHLS = config.LLHLSConfig{
				Enabled: true, PartDuration: 0.05, SegmentDuration: 0.2, SegmentCount: 20, Container: "fmp4",
			}
			module := NewModule()
			module.server = core.NewServer(cfg)
			stream := newGenerationLifecycleStream(t, "live/hook-drain-"+test.name, "publisher-old")
			t.Cleanup(func() {
				_ = module.Close()
				stream.RingBuffer().Close()
			})

			oldManager := test.create(module, stream)
			nextDTS := writeUntilGenerationManagerHasSegment(t, stream, oldManager, "old-warmup", 0)
			oldMarker := writeGenerationMarkers(stream, "old-boundary", nextDTS, 8)
			retireGenerationThroughPublishStop(t, module, stream, oldManager, oldMarker)

			replacementPublisher := &muxerWorkerPublisher{id: "publisher-new", info: aacGenerationMediaInfo()}
			if err := stream.SetPublisher(replacementPublisher); err != nil {
				t.Fatal(err)
			}
			replacementManager := test.create(module, stream)
			if replacementManager.manager == oldManager.manager {
				t.Fatal("replacement publisher generation reused the retired manager")
			}
			nextDTS = writeUntilGenerationManagerHasSegment(t, stream, replacementManager, "new-warmup", 10_000)
			replacementMarker := writeGenerationMarkers(stream, "new-boundary", nextDTS, 8)
			retireGenerationThroughPublishStop(t, module, stream, replacementManager, replacementMarker)

			if got := bytes.Count(oldManager.output(), replacementMarker); got != 0 {
				t.Fatalf("retired manager contains %d replacement-generation markers, want 0", got)
			}
			if got := bytes.Count(replacementManager.output(), oldMarker); got != 0 {
				t.Fatalf("replacement manager contains %d old-generation markers, want 0", got)
			}
		})
	}
}

func newGenerationLifecycleStream(t *testing.T, key, publisherID string) *core.Stream {
	t.Helper()
	stream := core.NewStream(key, config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{id: publisherID, info: aacGenerationMediaInfo()}); err != nil {
		t.Fatal(err)
	}
	return stream
}

func aacGenerationMediaInfo() *avframe.MediaInfo {
	return &avframe.MediaInfo{
		AudioCodec: avframe.CodecAAC, AudioSequenceHeader: []byte{0x12, 0x10}, SampleRate: 44100, Channels: 2,
	}
}

func writeUntilGenerationManagerHasSegment(
	t *testing.T,
	stream *core.Stream,
	manager generationManagerHarness,
	label string,
	startDTS int64,
) int64 {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	dts := startDTS
	for index := 0; ; index++ {
		payload := []byte(fmt.Sprintf("liveforge-%s-%04d", label, index))
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, dts, dts, payload,
		))
		dts += 20
		if manager.segmentCnt() > 0 {
			return dts
		}
		select {
		case <-deadline.C:
			t.Fatal("generation manager did not produce its startup segment")
		case <-ticker.C:
		}
	}
}

func writeGenerationMarkers(stream *core.Stream, label string, startDTS int64, count int) []byte {
	var last []byte
	for index := range count {
		last = []byte(fmt.Sprintf("liveforge-%s-%04d", label, index))
		dts := startDTS + int64(index)*20
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, dts, dts, last,
		))
	}
	return last
}

func retireGenerationThroughPublishStop(
	t *testing.T,
	module *Module,
	stream *core.Stream,
	manager generationManagerHarness,
	lastMarker []byte,
) {
	t.Helper()
	snapshot := stream.StartupSnapshot()
	stream.RemovePublisher()
	if err := module.onPublishStop(&core.EventContext{
		StreamKey:           stream.Key(),
		StreamInstanceID:    snapshot.StreamInstanceID,
		PublisherGeneration: snapshot.Generation,
		PublisherID:         snapshot.PublisherID,
	}); err != nil {
		t.Fatal(err)
	}
	if manager.registered() {
		t.Fatal("publish-stop left the retired generation manager available for lookup")
	}
	select {
	case <-manager.stopped:
		t.Fatal("publish-stop force-stopped the generation manager before its boundary drain")
	default:
	}

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for generationManagerTracked(module, manager.manager) {
		select {
		case <-deadline.C:
			t.Fatal("generation manager did not finish and leave module tracking after publish-stop")
		case <-ticker.C:
		}
	}
	if manager.registered() {
		t.Fatal("naturally completed generation manager remained available for lookup")
	}
	if got := bytes.Count(manager.output(), lastMarker); got != 1 {
		t.Fatalf("last pre-boundary marker occurrences after natural worker completion = %d, want exactly 1", got)
	}
}

func generationManagerTracked(module *Module, manager segmentManager) bool {
	module.managerMu.Lock()
	defer module.managerMu.Unlock()
	_, tracked := module.runningManagers[manager]
	return tracked
}

func TestGenerationBoundaryDrainFinalizesBufferedSegmentExactlyOnce(t *testing.T) {
	tests := []struct {
		name  string
		start func(*testing.T, string) (stop func(), done <-chan struct{}, segmentCount func() int, segmentDuration func() float64)
	}{
		{
			name: "hls",
			start: func(t *testing.T, streamKey string) (func(), <-chan struct{}, func() int, func() float64) {
				stream := newAudioOnlyAACStream(t, streamKey)
				manager := NewHLSManager(streamKey, "/"+streamKey, 10, 5)
				done := make(chan struct{})
				go func() {
					manager.Run(stream)
					close(done)
				}()
				time.Sleep(20 * time.Millisecond)
				writeLiveAACFrames(stream, 5, 20)
				stream.RemovePublisher()
				return func() {
						manager.Stop()
						stream.RingBuffer().Close()
					}, done, manager.SegmentCount, func() float64 {
						manager.mu.RLock()
						defer manager.mu.RUnlock()
						if len(manager.segments) == 0 {
							return 0
						}
						return manager.segments[0].Duration
					}
			},
		},
		{
			name: "dash",
			start: func(t *testing.T, streamKey string) (func(), <-chan struct{}, func() int, func() float64) {
				stream := newAudioOnlyAACStream(t, streamKey)
				manager := NewDASHManager(streamKey, "/"+streamKey, 10, 5)
				done := make(chan struct{})
				go func() {
					manager.Run(stream)
					close(done)
				}()
				time.Sleep(20 * time.Millisecond)
				writeLiveAACFrames(stream, 5, 20)
				stream.RemovePublisher()
				return func() {
						manager.Stop()
						stream.RingBuffer().Close()
					}, done, manager.SegmentCount, func() float64 {
						manager.mu.RLock()
						defer manager.mu.RUnlock()
						if len(manager.audioSegments) == 0 {
							return 0
						}
						return manager.audioSegments[0].Duration
					}
			},
		},
		{
			name: "llhls",
			start: func(t *testing.T, streamKey string) (func(), <-chan struct{}, func() int, func() float64) {
				stream := newAudioOnlyAACStream(t, streamKey)
				manager := NewLLHLSManager(streamKey, "/"+streamKey, 1, 10, 5, "fmp4")
				done := make(chan struct{})
				go func() {
					manager.Run(stream)
					close(done)
				}()
				time.Sleep(20 * time.Millisecond)
				writeLiveAACFrames(stream, 5, 20)
				stream.RemovePublisher()
				return func() {
						manager.Stop()
						stream.RingBuffer().Close()
					}, done, manager.SegmentCount, func() float64 {
						manager.mu.Lock()
						defer manager.mu.Unlock()
						if len(manager.segments) == 0 {
							return 0
						}
						return manager.segments[0].Duration
					}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stop, done, segmentCount, segmentDuration := test.start(t, "live/finalize-"+test.name)
			defer stop()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("manager did not stop when publisher generation ended")
			}
			if got := segmentCount(); got != 1 {
				t.Fatalf("completed segments after publisher generation end = %d, want 1", got)
			}
			if got := segmentDuration(); got < 0.1025 || got > 0.1035 {
				t.Fatalf("final segment duration = %.3fs, want 0.103s of buffered media", got)
			}
		})
	}
}
