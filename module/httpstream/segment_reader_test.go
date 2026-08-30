package httpstream

import (
	"context"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

type controlledSegmentInput struct {
	ring        *util.RingBuffer[*avframe.AVFrame]
	reader      *util.RingReader[*avframe.AVFrame]
	ready       chan struct{}
	permit      chan struct{}
	transformed bool
}

func newControlledSegmentInput(capacity int, transformed bool) *controlledSegmentInput {
	ring := util.NewRingBuffer[*avframe.AVFrame](capacity)
	return &controlledSegmentInput{
		ring:        ring,
		reader:      ring.NewReaderAt(0),
		ready:       make(chan struct{}, 1),
		permit:      make(chan struct{}),
		transformed: transformed,
	}
}

func (c *controlledSegmentInput) factory(
	_ *core.Stream,
	_ core.StreamStartupSnapshot,
	plan muxerAudioPlan,
) (*util.RingReader[*avframe.AVFrame], func(), muxerAudioPlan) {
	if c.transformed {
		plan.mode = muxerAudioTranscode
	}
	return c.reader, func() {}, plan
}

func (c *controlledSegmentInput) beforeRead(done <-chan struct{}) func() {
	return func() {
		select {
		case c.ready <- struct{}{}:
		case <-done:
			return
		}
		select {
		case <-c.permit:
		case <-done:
		}
	}
}

func (c *controlledSegmentInput) writeAndRead(t *testing.T, frame *avframe.AVFrame) {
	t.Helper()
	c.waitReady(t)
	c.ring.Write(frame)
	c.permit <- struct{}{}
}

func (c *controlledSegmentInput) writeBurstAndRead(t *testing.T, frames ...*avframe.AVFrame) {
	t.Helper()
	c.waitReady(t)
	for _, frame := range frames {
		c.ring.Write(frame)
	}
	c.permit <- struct{}{}
}

func (c *controlledSegmentInput) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-c.ready:
	case <-time.After(time.Second):
		t.Fatal("segmenter did not reach the controlled read boundary")
	}
}

func TestReadSegmentFrameReportsOverwriteAtomically(t *testing.T) {
	ring := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := ring.NewReaderAt(0)
	retained := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		20, 20, []byte("retained-unsendable"),
	)
	ring.Write(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte("lost-0")))
	ring.Write(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 10, 10, []byte("lost-1")))
	ring.Write(retained)
	ring.Write(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 30, 30, []byte("retained-1")))

	result := readSegmentFrame(context.Background(), reader, core.StreamStartupSnapshot{}, true)
	if result.Frame != retained || !result.OK || result.Overwritten != 2 {
		t.Fatalf("read result = %+v, want retained frame %p, OK, overwritten=2", result, retained)
	}
}

func TestReadSegmentFrameCancellationDoesNotAdvanceCursor(t *testing.T) {
	ring := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := ring.NewReaderAt(0)
	ring.Write(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		0, 0, []byte("unread"),
	))
	cursor := reader.ReadCursor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := readSegmentFrame(ctx, reader, core.StreamStartupSnapshot{}, true)
	if result.OK || result.Frame != nil || result.Overwritten != 0 {
		t.Fatalf("canceled read result = %+v, want zero result", result)
	}
	if got := reader.ReadCursor(); got != cursor {
		t.Fatalf("cursor after canceled read = %d, want %d", got, cursor)
	}
}
