package core

import (
	"context"
	"errors"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

func TestReadFrameContextRejectsOverwrittenRetainedFrame(t *testing.T) {
	ring := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := ring.NewReaderAt(0)
	for i := 0; i < 4; i++ {
		ring.Write(avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			avframe.FrameTypeInterframe,
			int64(i), int64(i), []byte{byte(i)},
		))
	}

	frame, ok, err := ReadFrameContext(context.Background(), reader)
	if frame != nil || ok {
		t.Fatalf("overwritten read = frame=%v ok=%v, want no retained frame", frame, ok)
	}
	var overwritten *ReaderOverwrittenError
	if !errors.As(err, &overwritten) || overwritten.Count != 2 {
		t.Fatalf("overwritten error = %v, want count 2", err)
	}
	if got, want := reader.ReadCursor(), ring.WriteCursor(); got != want {
		t.Fatalf("reader cursor = %d, want live cursor %d", got, want)
	}
}
