package dvr

import (
	"fmt"
	"log/slog"
	"os"
	"path"
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

	muxer       *ts.Muxer
	segFile     *os.File
	segStartDTS int64
	lastDTS     int64
	segSeqNum   int
	wallStart   time.Time

	videoSeq   []byte
	audioSeq   []byte
	videoCodec avframe.CodecType
	audioCodec avframe.CodecType

	reader  *util.RingReader[*avframe.AVFrame]
	done    chan struct{}
	stopped atomic.Bool
}

// NewSession creates a DVR session for the given stream.
func NewSession(streamKey string, stream *core.Stream, cfg config.DVRConfig, existingIndex *SegmentIndex, startSeq int) (*Session, error) {
	if err := core.ValidateStreamKey(streamKey); err != nil {
		return nil, fmt.Errorf("dvr: invalid stream key: %w", err)
	}
	segDir := resolvePath(cfg.Path, streamKey)
	if err := core.ValidatePathWithinRoot(dvrPathRoot(cfg.Path, segDir), segDir); err != nil {
		return nil, fmt.Errorf("dvr: path escapes configured root: %w", err)
	}
	if err := os.MkdirAll(segDir, 0755); err != nil {
		return nil, fmt.Errorf("dvr: create dir %s: %w", segDir, err)
	}

	idx := existingIndex
	if idx == nil {
		idx = NewSegmentIndex()
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
	}, nil
}

// Run starts the DVR segment writing loop. Blocks until Stop or stream closes.
func (s *Session) Run() {
	defer s.closeSegment()

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

		frame, ok := s.reader.Read()
		if !ok {
			return
		}
		s.processFrame(frame)
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
		s.closeSegment()
		s.openSegment()
	}

	data := s.muxer.WriteFrame(frame)
	if data != nil && s.segFile != nil {
		s.segFile.Write(data)
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
	path := filepath.Join(s.segDir, filename)

	f, err := os.Create(path)
	if err != nil {
		slog.Error("dvr: create segment", "stream", s.streamKey, "error", err)
		return
	}

	s.segFile = f
	s.wallStart = time.Now()
	s.segStartDTS = -1
}

func (s *Session) closeSegment() {
	if s.segFile == nil {
		return
	}

	s.segFile.Close()

	dur := float64(0)
	if s.segStartDTS >= 0 && s.lastDTS > s.segStartDTS {
		dur = float64(s.lastDTS-s.segStartDTS) / 1000.0
	}

	info, _ := os.Stat(s.segFile.Name())
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	filename := filepath.Base(s.segFile.Name())
	s.index.Add(Segment{
		SeqNum:    s.segSeqNum,
		StartTime: s.wallStart,
		StartDTS:  s.segStartDTS,
		Duration:  dur,
		Filename:  filename,
		Size:      size,
		DiskPath:  s.segFile.Name(),
	})

	s.segSeqNum++
	s.segFile = nil
}

// Stop signals the session to exit its write loop.
func (s *Session) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.reader.Close()
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
		pathTemplate = "./dvr/{stream_key}"
	}
	p := strings.ReplaceAll(pathTemplate, "{stream_key}", streamKey)
	p = strings.ReplaceAll(p, "{stream}", path.Base(streamKey))
	return filepath.Clean(p)
}

func dvrPathRoot(template, expanded string) string {
	if template == "" {
		template = "./dvr/{stream_key}"
	}
	if index := strings.IndexByte(template, '{'); index >= 0 {
		prefix := template[:index]
		if prefix == "" {
			return "."
		}
		return filepath.Clean(filepath.Dir(prefix))
	}
	return filepath.Dir(expanded)
}
