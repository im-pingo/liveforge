//go:build audiocodec

package httpstream

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

type switchingSegmentOpen struct {
	mode     muxerAudioMode
	snapshot core.StreamStartupSnapshot
	ring     *util.RingBuffer[*avframe.AVFrame]
	reader   *util.RingReader[*avframe.AVFrame]
	releases atomic.Int32
}

type switchingSegmentInput struct {
	mu     sync.Mutex
	opens  []*switchingSegmentOpen
	opened chan *switchingSegmentOpen
	ready  chan struct{}
	permit chan struct{}
}

func newSwitchingSegmentInput() *switchingSegmentInput {
	return &switchingSegmentInput{
		opened: make(chan *switchingSegmentOpen, 4),
		ready:  make(chan struct{}, 1),
		permit: make(chan struct{}),
	}
}

func (s *switchingSegmentInput) factory(
	_ *core.Stream,
	snapshot core.StreamStartupSnapshot,
	plan muxerAudioPlan,
) (*util.RingReader[*avframe.AVFrame], func(), muxerAudioPlan) {
	ring := util.NewRingBuffer[*avframe.AVFrame](2)
	open := &switchingSegmentOpen{
		mode: plan.mode, snapshot: snapshot, ring: ring, reader: ring.NewReaderAt(0),
	}
	s.mu.Lock()
	s.opens = append(s.opens, open)
	s.mu.Unlock()
	s.opened <- open
	return open.reader, func() { open.releases.Add(1) }, plan
}

func (s *switchingSegmentInput) beforeRead(done <-chan struct{}) func() {
	return func() {
		select {
		case s.ready <- struct{}{}:
		case <-done:
			return
		}
		select {
		case <-s.permit:
		case <-done:
		}
	}
}

func (s *switchingSegmentInput) waitOpen(t *testing.T) *switchingSegmentOpen {
	t.Helper()
	select {
	case open := <-s.opened:
		return open
	case <-time.After(time.Second):
		t.Fatal("segmenter did not open the expected recovery input")
		return nil
	}
}

func (s *switchingSegmentInput) writeAndRead(t *testing.T, frame *avframe.AVFrame) {
	t.Helper()
	s.waitReady(t)
	s.active(t).ring.Write(frame)
	s.permit <- struct{}{}
}

func (s *switchingSegmentInput) overwriteAndRead(t *testing.T, frames ...*avframe.AVFrame) {
	t.Helper()
	s.waitReady(t)
	active := s.active(t)
	for _, frame := range frames {
		active.ring.Write(frame)
	}
	s.permit <- struct{}{}
}

func (s *switchingSegmentInput) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-s.ready:
	case <-time.After(time.Second):
		t.Fatal("segmenter did not reach the controlled read boundary")
	}
}

func (s *switchingSegmentInput) active(t *testing.T) *switchingSegmentOpen {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.opens) == 0 {
		t.Fatal("segmenter has no active input")
	}
	return s.opens[len(s.opens)-1]
}

func (s *switchingSegmentInput) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, open := range s.opens {
		open.ring.Close()
	}
}

func TestSegmentersOverwriteReopensChangedAudioSource(t *testing.T) {
	for _, format := range []string{"hls", "llhls"} {
		for _, direction := range []struct {
			name        string
			initial     avframe.CodecType
			refreshed   avframe.CodecType
			initialMode muxerAudioMode
			newMode     muxerAudioMode
		}{
			{name: "direct_to_transformed", initial: avframe.CodecAAC, refreshed: avframe.CodecG711U, initialMode: muxerAudioPassthrough, newMode: muxerAudioTranscode},
			{name: "transformed_to_direct", initial: avframe.CodecG711U, refreshed: avframe.CodecAAC, initialMode: muxerAudioTranscode, newMode: muxerAudioPassthrough},
		} {
			t.Run(format+"/"+direction.name, func(t *testing.T) {
				stream := newAudioRecoveryStream(t, format+"-"+direction.name, direction.initial)
				input := newSwitchingSegmentInput()
				var completedSegments atomic.Int32
				var completed func() int
				done := make(chan struct{})
				var stop func()
				switch format {
				case "hls":
					manager := NewHLSManager(stream.Key(), "/"+stream.Key(), 0.05, 4)
					manager.inputFactory = input.factory
					manager.beforeLiveRead = input.beforeRead(manager.done)
					stop = manager.Stop
					completed = manager.SegmentCount
					go func() {
						manager.Run(stream)
						close(done)
					}()
				case "llhls":
					segmenter := NewLLHLSSegmenter(0.05, 0.1, "ts", LLHLSSegmenterCallbacks{
						OnSegment: func(*LLHLSSegment) { completedSegments.Add(1) },
					})
					segmenter.inputFactory = input.factory
					segmenter.beforeLiveRead = input.beforeRead(segmenter.done)
					stop = segmenter.Stop
					completed = func() int { return int(completedSegments.Load()) }
					go func() {
						segmenter.Run(stream)
						close(done)
					}()
				}
				t.Cleanup(func() {
					stop()
					input.closeAll()
					stream.RingBuffer().Close()
					select {
					case <-done:
					case <-time.After(time.Second):
					}
				})

				first := input.waitOpen(t)
				if first.mode != direction.initialMode {
					t.Fatalf("initial input mode = %d, want %d", first.mode, direction.initialMode)
				}
				input.writeAndRead(t, recoveryAACFrame(0, 0x10))
				setRecoveryAudioCodec(stream, direction.refreshed)
				input.overwriteAndRead(t,
					recoveryAACFrame(20, 0x20), recoveryAACFrame(40, 0x21),
					recoveryAACFrame(60, 0x22), recoveryAACFrame(80, 0x23),
				)

				second := input.waitOpen(t)
				if second.mode != direction.newMode {
					t.Fatalf("recovery input mode = %d, want %d", second.mode, direction.newMode)
				}
				if second.snapshot.SourceCursor != second.snapshot.LiveCursor {
					t.Fatalf("recovery source cursor = %d, want live cursor %d", second.snapshot.SourceCursor, second.snapshot.LiveCursor)
				}
				if got := first.releases.Load(); got != 1 {
					t.Fatalf("old input release count = %d, want 1 before recovery read", got)
				}
				first.ring.Write(recoveryAACFrame(90, 0x30))
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if first.reader.WaitContext(ctx) {
					t.Fatal("old input reader remained open after source switch")
				}

				for i, dts := range []int64{1000, 1060, 1120} {
					input.writeAndRead(t, recoveryAACFrame(dts, byte(0x40+i)))
				}
				input.waitReady(t)
				if got := completed(); got == 0 {
					t.Fatal("segmenter published no audio after reopening the refreshed source")
				}

				stop()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("segmenter did not stop")
				}
				if got := first.releases.Load(); got != 1 {
					t.Fatalf("old input final release count = %d, want 1", got)
				}
				if got := second.releases.Load(); got != 1 {
					t.Fatalf("recovery input release count = %d, want 1", got)
				}
			})
		}
	}
}

func newAudioRecoveryStream(t *testing.T, suffix string, codec avframe.CodecType) *core.Stream {
	t.Helper()
	stream := core.NewStream("live/"+suffix, config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	info := &avframe.MediaInfo{AudioCodec: codec, SampleRate: 8000, Channels: 1}
	if codec == avframe.CodecAAC {
		info.AudioSequenceHeader = []byte{0x12, 0x10}
		info.SampleRate = 44100
		info.Channels = 2
	}
	if err := stream.SetPublisher(&muxerWorkerPublisher{id: "recovery-" + suffix, info: info}); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 16))
	return stream
}

func setRecoveryAudioCodec(stream *core.Stream, codec avframe.CodecType) {
	frameType := avframe.FrameTypeInterframe
	payload := make([]byte, 160)
	if codec == avframe.CodecAAC {
		frameType = avframe.FrameTypeSequenceHeader
		payload = []byte{0x12, 0x10}
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, codec, frameType, 500, 500, payload))
}

func recoveryAACFrame(dts int64, marker byte) *avframe.AVFrame {
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		dts, dts, []byte{0x21, marker, 0x34, 0x55},
	)
}
