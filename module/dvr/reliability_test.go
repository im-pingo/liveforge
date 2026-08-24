package dvr

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type flakySegmentFile struct {
	name     string
	failures int
	writes   int
	file     *os.File
}

func (f *flakySegmentFile) Write(p []byte) (int, error) {
	f.writes++
	if f.writes <= f.failures {
		return 0, errors.New("temporary write failure")
	}
	return f.file.Write(p)
}

func (f *flakySegmentFile) Sync() error  { return f.file.Sync() }
func (f *flakySegmentFile) Close() error { return f.file.Close() }
func (f *flakySegmentFile) Name() string { return f.name }

func TestSessionRetriesAndAtomicallyFinalizesSegment(t *testing.T) {
	dir := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/retry")
	session, err := NewSession("live/retry", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	session.createSegment = func(path string) (segmentFile, error) {
		file, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		return &flakySegmentFile{name: path, failures: 2, file: file}, nil
	}

	session.processFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}})
	session.closeSegment(false)

	segments := session.Index().Segments()
	if len(segments) != 1 {
		t.Fatalf("segments = %+v, status=%+v", segments, session.Status())
	}
	if filepath.Ext(segments[0].DiskPath) != ".ts" {
		t.Fatalf("segment path = %q", segments[0].DiskPath)
	}
	if _, err := os.Stat(segments[0].DiskPath + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial still exists: %v", err)
	}
	if got := session.Status().Metrics.WriteRetries; got != 2 {
		t.Fatalf("write retries = %d", got)
	}
}

func TestSessionPreservesFailedSegmentAndRecovers(t *testing.T) {
	dir := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/fail")
	session, err := NewSession("live/fail", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}"), SegmentDuration: time.Millisecond}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	creates := 0
	session.createSegment = func(path string) (segmentFile, error) {
		creates++
		file, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		failures := 0
		if creates == 1 {
			failures = 10
		}
		return &flakySegmentFile{name: path, failures: failures, file: file}, nil
	}

	frame := &avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}}
	session.processFrame(frame)
	session.processFrame(frame)
	session.closeSegment(false)

	status := session.Status()
	if status.Metrics.WriteFailures != 1 || status.Metrics.SegmentsWritten != 1 || status.LastError == "" {
		t.Fatalf("status = %+v", status)
	}
	failed, err := filepath.Glob(filepath.Join(dir, "live/fail/*.failed"))
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed files=%v err=%v", failed, err)
	}
}

func TestSegmentIndexCleanupRetainsDeleteFailures(t *testing.T) {
	dir := t.TempDir()
	idx := NewSegmentIndex()
	okPath := filepath.Join(dir, "ok.ts")
	if err := os.WriteFile(okPath, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(dir, "not-empty")
	if err := os.Mkdir(badPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badPath, "child"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	idx.Add(Segment{SeqNum: 1, StartTime: old, DiskPath: okPath, Size: 2})
	idx.Add(Segment{SeqNum: 2, StartTime: old, DiskPath: badPath, Size: 1})

	result := idx.CleanBeforeWithResult(time.Now())
	if result.Deleted != 1 || result.Bytes != 2 || result.Failures != 1 || idx.Len() != 1 {
		t.Fatalf("result=%+v remaining=%+v", result, idx.Segments())
	}
}

func TestNewSessionRejectsTraversalStreamKey(t *testing.T) {
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/safe")
	for _, key := range []string{"../outside", "live/../../outside", "/absolute"} {
		if _, err := NewSession(key, stream, config.DVRConfig{Path: filepath.Join(t.TempDir(), "{stream_key}")}, nil, 0); err == nil {
			t.Errorf("NewSession(%q) succeeded", key)
		}
	}
}

func TestSessionPreservesOrphanPartialBeforeRecovery(t *testing.T) {
	dir := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/orphan")
	session, err := NewSession("live/orphan", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "live/orphan/seg_000000.ts.partial")
	if err := os.WriteFile(orphan, []byte("previous process"), 0644); err != nil {
		t.Fatal(err)
	}

	session.processFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}})
	session.closeSegment(false)

	failed, err := filepath.Glob(filepath.Join(dir, "live/orphan/seg_000000.ts.orphan-*.failed"))
	if err != nil || len(failed) != 1 {
		t.Fatalf("orphan files=%v err=%v", failed, err)
	}
	data, err := os.ReadFile(failed[0])
	if err != nil || string(data) != "previous process" {
		t.Fatalf("orphan data=%q err=%v", data, err)
	}
	if session.Status().Metrics.WriteFailures != 1 || session.Index().Len() != 1 {
		t.Fatalf("status=%+v segments=%+v", session.Status(), session.Index().Segments())
	}
}
