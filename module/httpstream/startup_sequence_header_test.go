package httpstream

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

func TestHLSManagerWaitsForLateSequenceHeader(t *testing.T) {
	stream := newStreamWaitingForVideoSequenceHeader(t, "live/hls-late-sequence-header")
	mgr := NewHLSManager(stream.Key(), "/live/hls-late-sequence-header", 1, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	defer stopStartupSequenceHeaderTest(t, mgr.Stop, stream, done)

	writeLateVideoSequenceHeaderAndFrames(stream)
	waitForSegmentCount(t, mgr.SegmentCount, 1)
	segment, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("HLS did not produce a segment after a late video sequence header")
	}
	var videoFrames int
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			videoFrames++
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	if videoFrames == 0 {
		t.Fatal("HLS segment contains no playable video after a late sequence header")
	}
}

func TestDASHManagerWaitsForLateSequenceHeader(t *testing.T) {
	stream := newStreamWaitingForVideoSequenceHeader(t, "live/dash-late-sequence-header")
	mgr := NewDASHManager(stream.Key(), "/live/dash-late-sequence-header", 1, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	defer stopStartupSequenceHeaderTest(t, mgr.Stop, stream, done)

	writeLateVideoSequenceHeaderAndFrames(stream)
	waitForSegmentCount(t, mgr.SegmentCount, 1)
	if _, ok := mgr.GetInitSegment(); !ok {
		t.Fatal("DASH did not publish a video init segment after a late sequence header")
	}
	segment, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("DASH did not produce a segment after a late video sequence header")
	}
	demuxer, err := fmp4.NewDemuxer(mustDASHInit(t, mgr))
	if err != nil {
		t.Fatalf("create DASH demuxer: %v", err)
	}
	frames, err := demuxer.Parse(segment)
	if err != nil {
		t.Fatalf("parse DASH segment: %v", err)
	}
	if !hasPlayableVideo(frames) {
		t.Fatal("DASH segment contains no playable video after a late sequence header")
	}
}

func TestDASHInitFromStreamDoesNotPublishBeforeStartup(t *testing.T) {
	stream := newStreamWaitingForVideoSequenceHeader(t, "live/dash-init-before-startup")
	mgr := NewDASHManager(stream.Key(), "/live/dash-init-before-startup", 1, 5)
	mgr.InitFromStream(stream)
	if _, ok := mgr.GetInitSegment(); ok {
		t.Fatal("DASH published an init segment before the stream startup snapshot was ready")
	}
}

func TestLLHLSManagerWaitsForLateSequenceHeader(t *testing.T) {
	stream := newStreamWaitingForVideoSequenceHeader(t, "live/llhls-late-sequence-header")
	mgr := NewLLHLSManager(stream.Key(), "/live/llhls-late-sequence-header", 0.2, 1, 5, "fmp4")
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	defer stopStartupSequenceHeaderTest(t, mgr.Stop, stream, done)

	writeLateVideoSequenceHeaderAndFrames(stream)
	waitForSegmentCount(t, mgr.SegmentCount, 1)
	if _, ok := mgr.GetInitSegment(); !ok {
		t.Fatal("LL-HLS did not publish a video init segment after a late sequence header")
	}
	segment, ok := mgr.GetFullSegment(0)
	if !ok {
		t.Fatal("LL-HLS did not produce a segment after a late video sequence header")
	}
	demuxer, err := fmp4.NewDemuxer(mustLLHLSInit(t, mgr))
	if err != nil {
		t.Fatalf("create LL-HLS demuxer: %v", err)
	}
	frames, err := demuxer.Parse(segment)
	if err != nil {
		t.Fatalf("parse LL-HLS segment: %v", err)
	}
	if !hasPlayableVideo(frames) {
		t.Fatal("LL-HLS segment contains no playable video after a late sequence header")
	}
}

func newStreamWaitingForVideoSequenceHeader(t *testing.T, key string) *core.Stream {
	t.Helper()
	stream := core.NewStream(key, config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    2,
		RingBufferSize: 64,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
	}}); err != nil {
		t.Fatal(err)
	}
	if stream.StartupSnapshot().Ready {
		t.Fatal("test stream unexpectedly became ready before its video sequence header")
	}
	return stream
}

func writeLateVideoSequenceHeaderAndFrames(stream *core.Stream) {
	// Give the old implementation time to capture its incomplete startup state.
	time.Sleep(20 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	))
	// Let the corrected implementation capture the now-ready startup snapshot
	// before the first live keyframe arrives.
	time.Sleep(20 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 2, 0x65, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		500, 500, []byte{0, 0, 0, 2, 0x41, 0x02},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		1000, 1000, []byte{0, 0, 0, 2, 0x65, 0x03},
	))
}

func mustDASHInit(t *testing.T, mgr *DASHManager) []byte {
	t.Helper()
	data, ok := mgr.GetInitSegment()
	if !ok {
		t.Fatal("DASH init segment is unavailable")
	}
	return data
}

func mustLLHLSInit(t *testing.T, mgr *LLHLSManager) []byte {
	t.Helper()
	data, ok := mgr.GetInitSegment()
	if !ok {
		t.Fatal("LL-HLS init segment is unavailable")
	}
	return data
}

func hasPlayableVideo(frames []*avframe.AVFrame) bool {
	for _, frame := range frames {
		if frame != nil && frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			return true
		}
	}
	return false
}

func stopStartupSequenceHeaderTest(t *testing.T, stop func(), stream *core.Stream, done <-chan struct{}) {
	t.Helper()
	stop()
	stream.RingBuffer().Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("segment manager did not stop")
	}
}
