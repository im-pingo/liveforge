package record

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type flakyWriteSeeker struct {
	failures int
	writes   int
	data     []byte
}

func (f *flakyWriteSeeker) Write(p []byte) (int, error) {
	f.writes++
	if f.writes <= f.failures {
		return 0, errors.New("temporary write failure")
	}
	f.data = append(f.data, p...)
	return len(p), nil
}

func (f *flakyWriteSeeker) Seek(int64, int) (int64, error) { return 0, nil }

func TestRetryWriteSeekerRecoversWithinBound(t *testing.T) {
	underlying := &flakyWriteSeeker{failures: 2}
	retries := 0
	w := newRetryWriteSeeker(underlying, 3, func() { retries++ })
	n, err := w.Write([]byte("frame"))
	if err != nil || n != 5 || string(underlying.data) != "frame" {
		t.Fatalf("write n=%d data=%q err=%v", n, underlying.data, err)
	}
	if retries != 2 || underlying.writes != 3 {
		t.Fatalf("retries=%d writes=%d", retries, underlying.writes)
	}
}

func TestRetryWriteSeekerStopsAtBound(t *testing.T) {
	underlying := &flakyWriteSeeker{failures: 10}
	retries := 0
	w := newRetryWriteSeeker(underlying, 3, func() { retries++ })
	n, err := w.Write([]byte("frame"))
	if err == nil || n != 0 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if retries != 2 || underlying.writes != 3 {
		t.Fatalf("retries=%d writes=%d", retries, underlying.writes)
	}
}

func TestRetryWriteSeekerDoesNotReplayPartialWrite(t *testing.T) {
	w := newRetryWriteSeeker(partialWriter{}, 3, nil)
	n, err := w.Write([]byte("frame"))
	if n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("write n=%d err=%v", n, err)
	}
}

type partialWriter struct{}

func (partialWriter) Write([]byte) (int, error)      { return 2, errors.New("partial") }
func (partialWriter) Seek(int64, int) (int64, error) { return 0, nil }

type flakyStorage struct {
	Storage
	failures int
}

func (s flakyStorage) Create(ctx context.Context, id string, info RecordingInfo) (WriteObject, error) {
	object, err := s.Storage.Create(ctx, id, info)
	if err != nil {
		return nil, err
	}
	return &flakyObject{WriteObject: object, failures: s.failures}, nil
}

type flakyObject struct {
	WriteObject
	failures int
	writes   int
}

func (o *flakyObject) Write(p []byte) (int, error) {
	o.writes++
	if o.writes <= o.failures {
		return 0, errors.New("temporary storage error")
	}
	return o.WriteObject.Write(p)
}

func TestFileWriterRetriesAndPublishesStatus(t *testing.T) {
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metrics := &RecordingMetrics{}
	w, err := newFileWriterWithStorage("live/retry", config.RecordConfig{Format: "flv"}, flakyStorage{Storage: local, failures: 2}, "live/retry.flv", metrics)
	if err != nil {
		t.Fatal(err)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1, 2, 3})
	if err := w.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseWithError(nil); err != nil {
		t.Fatal(err)
	}
	if w.WriteRetries() != 2 || metrics.Snapshot().FilesCompleted != 1 {
		t.Fatalf("writer retries=%d metrics=%+v", w.WriteRetries(), metrics.Snapshot())
	}
	if filepath.Ext(w.FilePath()) != ".flv" {
		t.Fatalf("published path = %q", w.FilePath())
	}
}

func TestFileWriterPreservesFailureStatus(t *testing.T) {
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metrics := &RecordingMetrics{}
	w, err := newFileWriterWithStorage("live/fail", config.RecordConfig{Format: "flv"}, flakyStorage{Storage: local, failures: 10}, "live/fail.flv", metrics)
	if err != nil {
		t.Fatal(err)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1})
	writeErr := w.WriteFrame(frame)
	if writeErr == nil {
		t.Fatal("expected persistent write error")
	}
	if err := w.CloseWithError(writeErr); err == nil {
		t.Fatal("expected close to report original write error")
	}
	items, err := local.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].State != RecordingFailed || metrics.Snapshot().FilesFailed != 1 {
		t.Fatalf("items=%+v metrics=%+v", items, metrics.Snapshot())
	}
}
