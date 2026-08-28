package dvr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/localfs"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
	"github.com/im-pingo/liveforge/pkg/util"
)

// Session manages DVR segment writing for a single stream.
type Session struct {
	streamKey   string
	stream      *core.Stream
	snapshot    core.StreamStartupSnapshot
	publisherID string
	cfg         config.DVRConfig
	index       *SegmentIndex
	segDir      string
	storage     *dvrStorage
	dir         *localfs.Dir
	ownsStore   bool

	muxer       *ts.Muxer
	segFile     segmentFile
	segPending  *localfs.Pending
	segFinal    string
	segStartDTS int64
	lastDTS     int64
	segSeqNum   int
	wallStart   time.Time

	videoSeq   []byte
	audioSeq   []byte
	videoCodec avframe.CodecType
	audioCodec avframe.CodecType

	reader      *util.RingReader[*avframe.AVFrame]
	done        chan struct{}
	finished    chan struct{}
	lifecycle   atomic.Uint32
	runMu       sync.Mutex
	runStarted  bool
	closeCalled bool
	finishOnce  sync.Once
	storageOnce sync.Once
	storageErr  error
	startedAt   time.Time
	lastError   atomic.Pointer[string]
	metrics     *DVRMetrics
	wrapSegment func(segmentFile) segmentFile
}

type segmentFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

const (
	sessionActive uint32 = iota
	sessionStopping
	sessionStopped
)

// NewSession creates a DVR session for the given stream.
func NewSession(streamKey string, stream *core.Stream, cfg config.DVRConfig, existingIndex *SegmentIndex, startSeq int) (*Session, error) {
	return newSession(streamKey, stream, cfg, existingIndex, startSeq, nil)
}

func newSession(streamKey string, stream *core.Stream, cfg config.DVRConfig, existingIndex *SegmentIndex, startSeq int, metrics *DVRMetrics) (*Session, error) {
	storage, err := newDVRStorage(cfg.Path)
	if err != nil {
		return nil, err
	}
	session, err := newSessionWithStorage(streamKey, stream, cfg, existingIndex, startSeq, metrics, storage, nil, true)
	if err != nil {
		_ = storage.Close()
		return nil, err
	}
	return session, nil
}

func newSessionWithStorage(streamKey string, stream *core.Stream, cfg config.DVRConfig, existingIndex *SegmentIndex, startSeq int, metrics *DVRMetrics, storage *dvrStorage, existingDir *localfs.Dir, ownsStore bool) (*Session, error) {
	if !validStreamKey(streamKey) {
		return nil, fmt.Errorf("dvr: invalid stream key")
	}
	dir := existingDir
	segDir := resolvePath(cfg.Path, streamKey)
	if dir == nil {
		var err error
		dir, segDir, err = storage.openStreamDir(cfg.Path, streamKey)
		if err != nil {
			return nil, err
		}
	}

	if metrics == nil {
		metrics = &DVRMetrics{}
	}
	idx := existingIndex
	recoveredPartials := 0
	if idx == nil {
		var err error
		idx, startSeq, recoveredPartials, err = rebuildSegmentIndex(dir, segDir, cfg.SegmentDuration, startSeq)
		if err != nil {
			if existingDir == nil {
				_ = dir.Close()
			}
			return nil, err
		}
	}

	session := &Session{
		streamKey:   streamKey,
		stream:      stream,
		snapshot:    stream.StartupSnapshot(),
		cfg:         cfg,
		index:       idx,
		segDir:      segDir,
		storage:     storage,
		dir:         dir,
		ownsStore:   ownsStore,
		segStartDTS: -1,
		lastDTS:     -1,
		segSeqNum:   startSeq,
		done:        make(chan struct{}),
		finished:    make(chan struct{}),
		startedAt:   time.Now().UTC(),
		metrics:     metrics,
	}
	if recoveredPartials > 0 {
		metrics.writeFailures.Add(uint64(recoveredPartials))
		session.setLastError(fmt.Errorf("dvr: recovered %d crash partial segment(s)", recoveredPartials))
	}
	return session, nil
}

// Run starts the DVR segment writing loop. Blocks until Stop or stream closes.
func (s *Session) Run() {
	s.runMu.Lock()
	if s.runStarted {
		s.runMu.Unlock()
		return
	}
	s.runStarted = true
	closeCalled := s.closeCalled
	s.runMu.Unlock()
	if closeCalled {
		s.finish()
		return
	}
	defer s.finish()
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	go func() {
		select {
		case <-s.done:
			cancelRead()
		case <-readCtx.Done():
		}
	}()
	snapshot := s.snapshot
	if snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(snapshot.Generation) {
		return
	}
	s.videoCodec = snapshot.MediaInfo.VideoCodec
	s.audioCodec = snapshot.MediaInfo.AudioCodec
	allowUndeclaredTracks := snapshot.Generation == 0 &&
		snapshot.MediaInfo.VideoCodec == 0 && snapshot.MediaInfo.AudioCodec == 0
	transcodedAudio := false
	var releaseInput func()
	releaseInput = func() {}
	if s.audioCodec != 0 && !isDVRSupportedAudio(s.audioCodec) {
		if tm := s.stream.TranscodeManager(); tm != nil &&
			audiocodec.Global().CanTranscode(s.audioCodec, avframe.CodecAAC) &&
			len(audiocodec.Global().SequenceHeader(avframe.CodecAAC)) > 0 {
			reader, release, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, snapshot)
			if err == nil {
				s.reader = reader
				releaseInput = release
				s.audioCodec = avframe.CodecAAC
				s.audioSeq = append([]byte(nil), audiocodec.Global().SequenceHeader(avframe.CodecAAC)...)
				transcodedAudio = true
			} else {
				slog.Warn("dvr: audio transcode unavailable", "stream", s.streamKey, "codec", s.audioCodec, "error", err)
			}
		}
		if !transcodedAudio {
			// Keep DVR segments playable as video-only when optional audio
			// transcoding is not installed.
			s.audioCodec = 0
			s.audioSeq = nil
		}
	}
	defer releaseInput()
	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		s.videoSeq = append([]byte(nil), vsh.Payload...)
	}
	if ash := snapshot.AudioSequenceHeader; ash != nil && ash.Codec == s.audioCodec {
		s.audioSeq = append([]byte(nil), ash.Payload...)
	}
	generationCtx, cancelGeneration := context.WithCancel(readCtx)
	defer cancelGeneration()
	go func() {
		select {
		case <-snapshot.GenerationDone:
			cancelGeneration()
		case <-generationCtx.Done():
		}
	}()

	if !transcodedAudio {
		for _, frame := range snapshot.ReplayFrames {
			if snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(snapshot.Generation) {
				return
			}
			if !dvrFrameAccepted(frame, s.videoCodec, s.audioCodec, allowUndeclaredTracks) {
				continue
			}
			s.processFrame(frame)
		}
	}
	if s.reader == nil {
		s.reader = s.stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)
	}

	for {
		frame, ok := s.reader.ReadContext(generationCtx)
		if !ok {
			return
		}
		if snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if !dvrFrameAccepted(frame, s.videoCodec, s.audioCodec, allowUndeclaredTracks) {
			continue
		}
		s.processFrame(frame)
	}
}

func isDVRSupportedAudio(codec avframe.CodecType) bool {
	return codec == avframe.CodecAAC || codec == avframe.CodecMP3
}

func dvrFrameAccepted(frame *avframe.AVFrame, videoCodec, audioCodec avframe.CodecType, allowUndeclaredTracks bool) bool {
	if frame == nil {
		return false
	}
	if frame.MediaType.IsVideo() {
		if allowUndeclaredTracks && frame.FrameType == avframe.FrameTypeSequenceHeader {
			return true
		}
		return videoCodec != 0 && frame.Codec == videoCodec
	}
	if frame.MediaType.IsAudio() {
		if allowUndeclaredTracks && frame.FrameType == avframe.FrameTypeSequenceHeader {
			return true
		}
		return audioCodec != 0 && frame.Codec == audioCodec
	}
	return false
}

func (s *Session) finish() {
	s.finishOnce.Do(func() {
		s.closeSegment(false)
		s.lifecycle.Store(sessionStopped)
		if s.ownsStore {
			_ = s.closeStorage()
		}
		close(s.finished)
	})
}

// Wait blocks until Run has finalized the current segment.
func (s *Session) Wait() { <-s.finished }

func (s *Session) WaitUntil(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-s.finished:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-s.finished:
		return true
	case <-timer.C:
		return false
	}
}

func (s *Session) processFrame(frame *avframe.AVFrame) {
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			s.videoSeq = append([]byte(nil), frame.Payload...)
			s.videoCodec = frame.Codec
		} else if frame.MediaType.IsAudio() {
			s.audioSeq = append([]byte(nil), frame.Payload...)
			s.audioCodec = frame.Codec
		}
		return
	}

	if s.muxer == nil {
		s.muxer = ts.NewMuxer(s.videoCodec, s.audioCodec, s.videoSeq, s.audioSeq)
	}

	if s.segFile == nil || (frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && s.shouldSplit()) {
		s.closeSegment(false)
		s.openSegment()
	}

	data := s.muxer.WriteFrame(frame)
	if data != nil && s.segFile != nil {
		if err := s.writeSegment(data); err != nil {
			s.setLastError(err)
			s.metrics.writeFailures.Add(1)
			s.closeSegment(true)
			return
		}
	}

	if s.segStartDTS < 0 {
		s.segStartDTS = frame.DTS
	}
	s.lastDTS = frame.DTS
}

func (s *Session) shouldSplit() bool {
	if s.segStartDTS < 0 {
		return false
	}
	dur := s.cfg.SegmentDuration
	if dur <= 0 {
		dur = 6 * time.Second
	}
	elapsed := time.Duration(s.lastDTS-s.segStartDTS) * time.Millisecond
	return elapsed >= dur
}

func (s *Session) openSegment() {
	filename := fmt.Sprintf("seg_%06d.ts", s.segSeqNum)
	for {
		if _, err := s.dir.Stat(filename); errors.Is(err, localfs.ErrNotFound) {
			break
		} else if err != nil {
			s.setLastError(err)
			s.metrics.writeFailures.Add(1)
			return
		}
		s.segSeqNum++
		filename = fmt.Sprintf("seg_%06d.ts", s.segSeqNum)
	}
	partialName := filename + ".partial"
	if _, err := s.dir.Stat(partialName); err == nil {
		stamp := time.Now().UnixNano()
		if _, err := s.dir.MoveToUnique(partialName, func(attempt int) string {
			return fmt.Sprintf("%s.orphan-%d.failed", filename, stamp+int64(attempt))
		}); err != nil {
			s.setLastError(err)
			s.metrics.writeFailures.Add(1)
			return
		}
		s.setLastError(fmt.Errorf("dvr: preserved orphan partial segment"))
		s.metrics.writeFailures.Add(1)
	} else if !errors.Is(err, localfs.ErrNotFound) {
		s.setLastError(err)
		s.metrics.writeFailures.Add(1)
		return
	}

	pending, err := s.dir.CreatePending(partialName, 0644)
	if err != nil {
		slog.Error("dvr: create segment", "stream", s.streamKey, "error", err)
		s.setLastError(err)
		s.metrics.writeFailures.Add(1)
		return
	}

	file := segmentFile(pending.File)
	if s.wrapSegment != nil {
		file = s.wrapSegment(file)
	}
	s.segFile = file
	s.segPending = pending
	s.segFinal = filename
	s.wallStart = time.Now()
	s.segStartDTS = -1
}

func (s *Session) closeSegment(failed bool) {
	if s.segFile == nil {
		return
	}
	if !failed {
		if err := s.segFile.Sync(); err != nil {
			failed = true
			s.setLastError(err)
			s.metrics.writeFailures.Add(1)
		}
	}
	if err := s.segFile.Close(); err != nil && !failed {
		failed = true
		s.setLastError(err)
		s.metrics.writeFailures.Add(1)
	}
	if failed {
		s.preservePending("write")
		s.segSeqNum++
		s.resetSegment()
		return
	}
	if err := s.segPending.PublishAs(s.segFinal); err != nil {
		s.setLastError(err)
		s.metrics.writeFailures.Add(1)
		s.preservePending("publish")
		s.segSeqNum++
		s.resetSegment()
		return
	}

	dur := float64(0)
	if s.segStartDTS >= 0 && s.lastDTS > s.segStartDTS {
		dur = float64(s.lastDTS-s.segStartDTS) / 1000.0
	}

	info, _ := s.segPending.StatSibling(s.segFinal)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	filename := s.segFinal
	s.index.Add(Segment{
		SeqNum:    s.segSeqNum,
		StartTime: s.wallStart,
		StartDTS:  s.segStartDTS,
		Duration:  dur,
		Filename:  filename,
		Size:      size,
		DiskPath:  filepath.Join(s.segDir, filename),
	})
	s.metrics.segmentsWritten.Add(1)
	if size > 0 {
		s.metrics.segmentBytes.Add(uint64(size))
	}

	s.segSeqNum++
	s.resetSegment()
}

func (s *Session) preservePending(label string) {
	stamp := time.Now().UnixNano()
	if _, err := s.segPending.PreserveAs(func(attempt int) string {
		return fmt.Sprintf("%s.%s-%d.failed", s.segFinal, label, stamp+int64(attempt))
	}); err != nil {
		s.setLastError(err)
	}
}

func (s *Session) resetSegment() {
	_ = s.segPending.Close()
	s.segFile = nil
	s.segPending = nil
	s.segFinal = ""
	s.segStartDTS = -1
}

func (s *Session) writeSegment(data []byte) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		n, err := s.segFile.Write(data)
		if err == nil && n == len(data) {
			return nil
		}
		if n > 0 {
			return fmt.Errorf("dvr: partial segment write: %d/%d", n, len(data))
		}
		if err == nil {
			err = fmt.Errorf("dvr: short segment write")
		}
		lastErr = err
		if attempt < 2 {
			s.metrics.writeRetries.Add(1)
		}
	}
	return fmt.Errorf("dvr: write segment after 3 attempts: %w", lastErr)
}

func (s *Session) setLastError(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	s.lastError.Store(&message)
}

func (s *Session) Status() SessionStatus {
	status := SessionStatus{StreamKey: s.streamKey, Live: s.IsLive(), StartedAt: s.startedAt, Metrics: s.metrics.Snapshot()}
	if last := s.lastError.Load(); last != nil {
		status.LastError = *last
	}
	return status
}

// Stop signals the session to exit its write loop.
func (s *Session) Stop() {
	if !s.lifecycle.CompareAndSwap(sessionActive, sessionStopping) {
		return
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// Close stops the session, waits for finalization when Run has started, and
// releases any storage boundary owned by this standalone session.
func (s *Session) Close() error {
	s.runMu.Lock()
	s.closeCalled = true
	started := s.runStarted
	s.runMu.Unlock()
	s.Stop()
	if started {
		s.Wait()
	} else {
		s.finish()
	}
	return s.closeStorage()
}

// IsLive returns true if the stream is still publishing.
func (s *Session) IsLive() bool {
	return s.lifecycle.Load() == sessionActive
}

func (s *Session) IsStopping() bool { return s.lifecycle.Load() == sessionStopping }

// Index returns the session's segment index.
func (s *Session) Index() *SegmentIndex {
	return s.index
}

func (s *Session) openIndexedSegment(segment Segment) (*os.File, os.FileInfo, error) {
	return s.dir.Open(segment.Filename)
}

func (s *Session) cleanBefore(cutoff time.Time) CleanupResult {
	return s.index.CleanBeforeWithRemover(cutoff, func(segment Segment) error {
		err := s.dir.Remove(segment.Filename)
		if errors.Is(err, localfs.ErrNotFound) {
			return nil
		}
		return err
	})
}

func (s *Session) closeStorage() error {
	s.storageOnce.Do(func() {
		if s.dir != nil {
			s.storageErr = errors.Join(s.storageErr, s.dir.Close())
		}
		if s.ownsStore && s.storage != nil {
			s.storageErr = errors.Join(s.storageErr, s.storage.Close())
		}
	})
	return s.storageErr
}

func resolvePath(pathTemplate, streamKey string) string {
	if pathTemplate == "" {
		pathTemplate = filepath.Join(".", "dvr", "{stream_key}")
	}
	p := strings.ReplaceAll(pathTemplate, "{stream_key}", streamKey)
	return filepath.Clean(p)
}

func validStreamKey(streamKey string) bool {
	if streamKey == "" || filepath.IsAbs(streamKey) || strings.Contains(streamKey, "\\") {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(streamKey))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func rebuildSegmentIndex(dir *localfs.Dir, segDir string, segmentDuration time.Duration, minimumNext int) (*SegmentIndex, int, int, error) {
	entries, err := dir.List(context.Background())
	if err != nil {
		return nil, 0, 0, fmt.Errorf("dvr: scan retained segments: %w", err)
	}
	recovered := 0
	for _, entry := range entries {
		filename := filepath.Base(filepath.FromSlash(entry.RelPath))
		if strings.HasSuffix(filename, ".ts.partial") {
			finalName := strings.TrimSuffix(filename, ".partial")
			stamp := time.Now().UnixNano()
			if _, err := dir.MoveToUnique(filename, func(attempt int) string {
				return fmt.Sprintf("%s.orphan-%d.failed", finalName, stamp+int64(attempt))
			}); err != nil {
				return nil, 0, recovered, fmt.Errorf("dvr: preserve crash partial: %w", err)
			}
			recovered++
			continue
		}
	}
	segments, next := buildRetainedSegments(entries, segDir, segmentDuration, minimumNext)
	idx := NewSegmentIndex()
	for _, segment := range segments {
		idx.Add(segment)
	}
	return idx, next, recovered, nil
}

func buildRetainedSegments(entries []localfs.Entry, segDir string, segmentDuration time.Duration, minimumNext int) ([]Segment, int) {
	duration := segmentDuration.Seconds()
	if duration <= 0 {
		duration = 6
	}
	next := minimumNext
	seen := make(map[int]struct{}, len(entries))
	segments := make([]Segment, 0, len(entries))
	for _, entry := range entries {
		filename := filepath.Base(filepath.FromSlash(entry.RelPath))
		seq := parseSeqNum(filename)
		if seq < 0 {
			continue
		}
		if _, duplicate := seen[seq]; duplicate {
			continue
		}
		seen[seq] = struct{}{}
		segments = append(segments, Segment{
			SeqNum:    seq,
			StartTime: entry.ModTime,
			Duration:  duration,
			Filename:  filename,
			Size:      entry.Size,
			DiskPath:  filepath.Join(segDir, filename),
		})
		if seq >= next {
			next = seq + 1
		}
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].SeqNum < segments[j].SeqNum })
	return segments, next
}
