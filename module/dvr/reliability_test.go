package dvr

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
	file     segmentFile
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
	session.wrapSegment = func(file segmentFile) segmentFile {
		return &flakySegmentFile{name: file.Name(), failures: 2, file: file}
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
	session.wrapSegment = func(file segmentFile) segmentFile {
		creates++
		failures := 0
		if creates == 1 {
			failures = 10
		}
		return &flakySegmentFile{name: file.Name(), failures: failures, file: file}
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

func TestNewSessionRebuildsRetainedIndexAndNeverOverwritesFinal(t *testing.T) {
	dir := t.TempDir()
	streamDir := filepath.Join(dir, "live/restart")
	if err := os.MkdirAll(streamDir, 0755); err != nil {
		t.Fatal(err)
	}
	retainedPath := filepath.Join(streamDir, "seg_000000.ts")
	if err := os.WriteFile(retainedPath, []byte("retained"), 0644); err != nil {
		t.Fatal(err)
	}

	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/restart")
	session, err := NewSession("live/restart", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if segments := session.Index().Segments(); len(segments) != 1 || segments[0].SeqNum != 0 {
		t.Fatalf("rebuilt segments = %+v", segments)
	}

	session.processFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}})
	session.closeSegment(false)

	retained, err := os.ReadFile(retainedPath)
	if err != nil || string(retained) != "retained" {
		t.Fatalf("retained data=%q err=%v", retained, err)
	}
	if _, err := os.Stat(filepath.Join(streamDir, "seg_000001.ts")); err != nil {
		t.Fatalf("next segment missing: %v", err)
	}
}

func TestNewSessionRecoversEveryCrashPartialDuringIndexRebuild(t *testing.T) {
	dir := t.TempDir()
	streamDir := filepath.Join(dir, "live/recover-all")
	if err := os.MkdirAll(streamDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(streamDir, "seg_000010.ts"), []byte("retained"), 0644); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(streamDir, "seg_000005.ts.partial")
	if err := os.WriteFile(partial, []byte("crash"), 0644); err != nil {
		t.Fatal(err)
	}

	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/recover-all")
	session, err := NewSession("live/recover-all", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("crash partial remains hidden: %v", err)
	}
	failed, err := filepath.Glob(filepath.Join(streamDir, "seg_000005.ts.orphan-*.failed"))
	if err != nil || len(failed) != 1 {
		t.Fatalf("recovered partials=%v err=%v", failed, err)
	}
	data, err := os.ReadFile(failed[0])
	if err != nil || string(data) != "crash" {
		t.Fatalf("recovered data=%q err=%v", data, err)
	}
	if got := session.Status(); got.Metrics.WriteFailures != 1 || got.LastError == "" {
		t.Fatalf("status=%+v", got)
	}
}

func TestNewSessionDoesNotFollowIntermediateStorageSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "live")); err != nil {
		t.Fatal(err)
	}
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/cam")
	_, err := NewSession("live/cam", stream, config.DVRConfig{Path: filepath.Join(root, "{stream_key}")}, nil, 0)
	if err == nil {
		t.Fatal("session followed an intermediate storage symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "cam")); !os.IsNotExist(err) {
		t.Fatalf("outside directory was created: %v", err)
	}
}

func TestSessionFinalizePinsOriginalStorageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/pin")
	session, err := NewSession("live/pin", stream, config.DVRConfig{Path: filepath.Join(root, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	session.processFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}})
	if err := os.Rename(filepath.Join(root, "live"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "live")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "pin"), 0755); err != nil {
		t.Fatal(err)
	}
	attackPartial := filepath.Join(outside, "pin/seg_000000.ts.partial")
	if err := os.WriteFile(attackPartial, []byte("attack"), 0644); err != nil {
		t.Fatal(err)
	}

	session.closeSegment(false)
	if _, err := os.Stat(filepath.Join(root, "moved/pin/seg_000000.ts")); err != nil {
		t.Fatalf("safe final missing: %v", err)
	}
	if data, err := os.ReadFile(attackPartial); err != nil || string(data) != "attack" {
		t.Fatalf("outside partial data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "pin/seg_000000.ts")); !os.IsNotExist(err) {
		t.Fatalf("outside final created: %v", err)
	}
}

func TestSessionPublishNeverReplacesConcurrentFinal(t *testing.T) {
	dir := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/collision")
	session, err := NewSession("live/collision", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	session.processFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{1}})

	finalPath := filepath.Join(dir, "live/collision/seg_000000.ts")
	if err := os.WriteFile(finalPath, []byte("retained"), 0644); err != nil {
		t.Fatal(err)
	}
	session.closeSegment(false)

	retained, err := os.ReadFile(finalPath)
	if err != nil || string(retained) != "retained" {
		t.Fatalf("concurrent final data=%q err=%v", retained, err)
	}
	failed, err := filepath.Glob(finalPath + ".publish-*.failed")
	if err != nil || len(failed) != 1 {
		t.Fatalf("preserved pending files=%v err=%v", failed, err)
	}
	if session.Index().Len() != 0 || session.Status().Metrics.WriteFailures != 1 {
		t.Fatalf("index=%+v status=%+v", session.Index().Segments(), session.Status())
	}
}

func TestSegmentHandlerReadsFromPinnedStorageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/serve")
	session, err := NewSession("live/serve", stream, config.DVRConfig{Path: filepath.Join(root, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	filename := "seg_000000.ts"
	originalPath := filepath.Join(root, "live/serve", filename)
	if err := os.WriteFile(originalPath, []byte("safe-segment"), 0644); err != nil {
		t.Fatal(err)
	}
	session.Index().Add(Segment{SeqNum: 0, StartTime: time.Now(), Filename: filename, DiskPath: originalPath})

	if err := os.Rename(filepath.Join(root, "live"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "live")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "serve"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "serve", filename), []byte("attack-segment"), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewModule()
	m.server = core.NewServer(config.Defaults())
	m.sessions["live/serve"] = session
	req := httptest.NewRequest(http.MethodGet, "/dvr/live/serve/"+filename, nil)
	req.SetPathValue("app", "live")
	req.SetPathValue("key", "serve")
	req.SetPathValue("filename", filename)
	response := httptest.NewRecorder()
	m.handleSegment(response, req)
	if response.Code != http.StatusOK || response.Body.String() != "safe-segment" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCleanupDeletesFromPinnedStorageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/cleanup-pin")
	session, err := NewSession("live/cleanup-pin", stream, config.DVRConfig{Path: filepath.Join(root, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	session.Stop()
	filename := "seg_000000.ts"
	originalPath := filepath.Join(root, "live/cleanup-pin", filename)
	if err := os.WriteFile(originalPath, []byte("safe-segment"), 0644); err != nil {
		t.Fatal(err)
	}
	session.Index().Add(Segment{SeqNum: 0, StartTime: time.Now().Add(-time.Hour), Filename: filename, Size: 12, DiskPath: originalPath})

	if err := os.Rename(filepath.Join(root, "live"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "live")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "cleanup-pin"), 0755); err != nil {
		t.Fatal(err)
	}
	attackPath := filepath.Join(outside, "cleanup-pin", filename)
	if err := os.WriteFile(attackPath, []byte("attack-segment"), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewModule()
	m.storePolicy(config.DVRConfig{Path: filepath.Join(root, "{stream_key}"), Window: time.Minute})
	m.sessions["live/cleanup-pin"] = session
	m.cleanExpiredSegments()
	if _, err := os.Stat(filepath.Join(root, "moved/cleanup-pin", filename)); !os.IsNotExist(err) {
		t.Fatalf("safe segment was not deleted: %v", err)
	}
	attack, err := os.ReadFile(attackPath)
	if err != nil || string(attack) != "attack-segment" {
		t.Fatalf("attack segment data=%q err=%v", attack, err)
	}
	if got := m.DVRStatus().Metrics; got.CleanupDeleted != 1 || got.CleanupFailures != 0 {
		t.Fatalf("cleanup metrics=%+v", got)
	}
}
