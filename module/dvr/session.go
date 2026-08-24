package dvr

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
	"github.com/im-pingo/liveforge/pkg/util"
)

// Session manages DVR segment writing for a single stream.
type Session struct {
	streamKey string
	stream    *core.Stream
	cfg       config.DVRConfig
	index     *SegmentIndex
	segDir    string

	muxer        *ts.Muxer
	segFile      segmentFile
	segFinalPath string
	segStartDTS  int64
	lastDTS      int64
	segSeqNum    int
	wallStart    time.Time

	videoSeq   []byte
	audioSeq   []byte
	videoCodec avframe.CodecType
	audioCodec avframe.CodecType

	reader        *util.RingReader[*avframe.AVFrame]
	done          chan struct{}
	finished      chan struct{}
	stopped       atomic.Bool
	startedAt     time.Time
	lastError     atomic.Pointer[string]
	metrics       *DVRMetrics
	createSegment func(string) (segmentFile, error)
}

type segmentFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

// NewSession creates a DVR session for the given stream.
func NewSession(streamKey string, stream *core.Stream, cfg config.DVRConfig, existingIndex *SegmentIndex, startSeq int) (*Session, error) {
	return newSession(streamKey, stream, cfg, existingIndex, startSeq, nil)
}

func newSession(streamKey string, stream *core.Stream, cfg config.DVRConfig, existingIndex *SegmentIndex, startSeq int, metrics *DVRMetrics) (*Session, error) {
	if !validStreamKey(streamKey) {
		return nil, fmt.Errorf("dvr: invalid stream key")
	}
	segDir := resolvePath(cfg.Path, streamKey)
	if err := os.MkdirAll(segDir, 0755); err != nil {
		return nil, fmt.Errorf("dvr: create dir %s: %w", segDir, err)
	}

	idx := existingIndex
	if idx == nil {
		idx = NewSegmentIndex()
	}

	if metrics == nil {
		metrics = &DVRMetrics{}
	}
	return &Session{
		streamKey:   streamKey,
		stream:      stream,
		cfg:         cfg,
		index:       idx,
		segDir:      segDir,
		segStartDTS: -1,
		lastDTS:     -1,
		segSeqNum:   startSeq,
		reader:      stream.RingBuffer().NewReader(),
		done:        make(chan struct{}),
		finished:    make(chan struct{}),
		startedAt:   time.Now().UTC(),
		metrics:     metrics,
		createSegment: func(path string) (segmentFile, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
		},
	}, nil
}

// Run starts the DVR segment writing loop. Blocks until Stop or stream closes.
func (s *Session) Run() {
	defer func() {
		s.closeSegment(false)
		s.stopped.Store(true)
		close(s.finished)
	}()

	if vsh := s.stream.VideoSeqHeader(); vsh != nil {
		s.videoSeq = append([]byte(nil), vsh.Payload...)
		s.videoCodec = vsh.Codec
	}
	if ash := s.stream.AudioSeqHeader(); ash != nil {
		s.audioSeq = append([]byte(nil), ash.Payload...)
		s.audioCodec = ash.Codec
	}

	for {
		select {
		case <-s.done:
			return
		default:
		}
		frame, ok := s.reader.TryRead()
		if ok {
			s.processFrame(frame)
			continue
		}

		if s.stream.RingBuffer().IsClosed() {
			return
		}

		select {
		case <-s.done:
			return
		case <-s.stream.RingBuffer().Signal():
		}
	}
}

// Wait blocks until Run has finalized the current segment.
func (s *Session) Wait() { <-s.finished }

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
	finalPath := filepath.Join(s.segDir, filename)
	partialPath := finalPath + ".partial"
	if _, err := os.Lstat(partialPath); err == nil {
		orphanPath := fmt.Sprintf("%s.orphan-%d.failed", finalPath, time.Now().UnixNano())
		if err := os.Rename(partialPath, orphanPath); err != nil {
			s.setLastError(err)
			s.metrics.writeFailures.Add(1)
			return
		}
		s.setLastError(fmt.Errorf("dvr: preserved orphan partial segment"))
		s.metrics.writeFailures.Add(1)
	} else if !os.IsNotExist(err) {
		s.setLastError(err)
		s.metrics.writeFailures.Add(1)
		return
	}

	f, err := s.createSegment(partialPath)
	if err != nil {
		slog.Error("dvr: create segment", "stream", s.streamKey, "error", err)
		s.setLastError(err)
		s.metrics.writeFailures.Add(1)
		return
	}

	s.segFile = f
	s.segFinalPath = finalPath
	s.wallStart = time.Now()
	s.segStartDTS = -1
}

func (s *Session) closeSegment(failed bool) {
	if s.segFile == nil {
		return
	}
	partialPath := s.segFile.Name()
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
		failedPath := s.segFinalPath + ".failed"
		if err := os.Rename(partialPath, failedPath); err != nil {
			s.setLastError(err)
		}
		s.segSeqNum++
		s.segFile = nil
		s.segFinalPath = ""
		s.segStartDTS = -1
		return
	}
	if err := os.Rename(partialPath, s.segFinalPath); err != nil {
		s.setLastError(err)
		s.metrics.writeFailures.Add(1)
		_ = os.Rename(partialPath, s.segFinalPath+".failed")
		s.segSeqNum++
		s.segFile = nil
		s.segFinalPath = ""
		s.segStartDTS = -1
		return
	}

	dur := float64(0)
	if s.segStartDTS >= 0 && s.lastDTS > s.segStartDTS {
		dur = float64(s.lastDTS-s.segStartDTS) / 1000.0
	}

	info, _ := os.Stat(s.segFinalPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	filename := filepath.Base(s.segFinalPath)
	s.index.Add(Segment{
		SeqNum:    s.segSeqNum,
		StartTime: s.wallStart,
		StartDTS:  s.segStartDTS,
		Duration:  dur,
		Filename:  filename,
		Size:      size,
		DiskPath:  s.segFinalPath,
	})
	s.metrics.segmentsWritten.Add(1)
	if size > 0 {
		s.metrics.segmentBytes.Add(uint64(size))
	}

	s.segSeqNum++
	s.segFile = nil
	s.segFinalPath = ""
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
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.stopped.Store(true)
}

// IsLive returns true if the stream is still publishing.
func (s *Session) IsLive() bool {
	return !s.stopped.Load()
}

// Index returns the session's segment index.
func (s *Session) Index() *SegmentIndex {
	return s.index
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
