package core

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

func TestStreamTotalSubscribersTracksProtocolChurn(t *testing.T) {
	s := NewStream("live/subscriber-total", newTestStreamConfig(), config.LimitsConfig{MaxSubscribersPerStream: 4}, NewEventBus())
	t.Cleanup(s.Close)
	if got := s.TotalSubscribers(); got != 0 {
		t.Fatalf("initial total subscribers = %d, want 0", got)
	}
	if err := s.AddSubscriber("rtmp"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSubscriber("hls"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSubscriber("rtmp"); err != nil {
		t.Fatal(err)
	}
	if got := s.TotalSubscribers(); got != 3 {
		t.Fatalf("total subscribers = %d, want 3", got)
	}
	s.RemoveSubscriber("rtmp")
	s.RemoveSubscriber("hls")
	s.RemoveSubscriber("rtmp")
	if got := s.TotalSubscribers(); got != 0 {
		t.Fatalf("final total subscribers = %d, want 0", got)
	}
}

func TestStreamMultiStreamConcurrentIngress(t *testing.T) {
	const streamCount, framesPerStream = 8, 2000
	streams := make([]*Stream, streamCount)
	publishers := make([]*testPublisher, streamCount)
	for i := range streams {
		streams[i] = NewStream(fmt.Sprintf("live/capacity/%d", i), newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
		publishers[i] = &testPublisher{id: fmt.Sprintf("publisher-%d", i), info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, VideoSequenceHeader: []byte{1, 2, 3}}}
		if err := streams[i].SetPublisher(publishers[i]); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(streams[i].Close)
	}

	var wait sync.WaitGroup
	var failures atomic.Int64
	for i := range streams {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for n := 0; n < framesPerStream; n++ {
				payload := make([]byte, 8)
				binary.BigEndian.PutUint64(payload, uint64(n))
				frameType := avframe.FrameTypeInterframe
				if n == 0 {
					frameType = avframe.FrameTypeKeyframe
				}
				frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, frameType, int64(n), int64(n), payload)
				if !streams[index].WriteFrameForPublisher(publishers[index], frame) {
					failures.Add(1)
					return
				}
			}
		}(i)
	}
	wait.Wait()
	if got := failures.Load(); got != 0 {
		t.Fatalf("concurrent ingress rejected %d frames", got)
	}
	for i, stream := range streams {
		if got := stream.RingBuffer().WriteCursor(); got != framesPerStream {
			t.Fatalf("stream %d cursor = %d, want %d", i, got, framesPerStream)
		}
		if snapshot := stream.StartupSnapshot(); !snapshot.Ready || len(snapshot.ReplayFrames) == 0 {
			t.Fatalf("stream %d has invalid startup snapshot", i)
		}
	}
}

func TestStreamMultiReaderIngress(t *testing.T) {
	const frameCount, readerCount = 4000, 4
	cfg := newTestStreamConfig()
	cfg.RingBufferSize = 256
	s := NewStream("live/multi-reader", cfg, config.LimitsConfig{}, NewEventBus())
	t.Cleanup(s.Close)
	pub := &testPublisher{id: "multi-reader-publisher", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := s.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	readers := make([]*util.RingReader[*avframe.AVFrame], readerCount)
	for i := range readers {
		readers[i] = s.RingBuffer().NewReaderAt(0)
	}
	for n := 0; n < frameCount; n++ {
		payload := make([]byte, 8)
		binary.BigEndian.PutUint64(payload, uint64(n))
		frameType := avframe.FrameTypeInterframe
		if n == 0 {
			frameType = avframe.FrameTypeKeyframe
		}
		if !s.WriteFrameForPublisher(pub, avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, frameType, int64(n), int64(n), payload)) {
			t.Fatalf("frame %d rejected", n)
		}
	}
	finalCursor := s.RingBuffer().WriteCursor()
	for readerIndex, reader := range readers {
		var previous int64 = -1
		for reader.ReadCursor() < finalCursor {
			result := reader.TryReadResult()
			if !result.OK {
				t.Fatalf("reader %d stopped at cursor %d before %d", readerIndex, reader.ReadCursor(), finalCursor)
			}
			if result.Value == nil || len(result.Value.Payload) != 8 {
				t.Fatalf("reader %d returned malformed frame", readerIndex)
			}
			identity := int64(binary.BigEndian.Uint64(result.Value.Payload))
			if identity < previous {
				t.Fatalf("reader %d frame identity regressed from %d to %d", readerIndex, previous, identity)
			}
			previous = identity
		}
		if got := reader.ReadCursor(); got != finalCursor {
			t.Fatalf("reader %d cursor = %d, want %d", readerIndex, got, finalCursor)
		}
	}
}

func TestStreamPublisherReplacementDuringIngress(t *testing.T) {
	s := NewStream("live/replacement-ingress", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	t.Cleanup(s.Close)
	oldPublisher := &testPublisher{id: "old-publisher", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	newPublisher := &testPublisher{id: "new-publisher", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := s.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	started := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.WriteFrameForPublisher(oldPublisher, avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 0, 0, []byte{1}))
		}
	}()
	<-started
	if !s.RemovePublisherIf(oldPublisher) {
		t.Fatal("old publisher was not removed")
	}
	if err := s.SetPublisher(newPublisher); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wait.Wait()
	for i := 0; i < 100; i++ {
		if s.WriteFrameForPublisher(oldPublisher, avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, int64(i), int64(i), []byte{2})) {
			t.Fatalf("old publisher frame %d accepted after replacement", i)
		}
	}
}
