package record

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// RecordSession reads frames from a stream's RingBuffer and writes them to a recording file.
type RecordSession struct {
	streamKey   string
	stream      *core.Stream
	snapshot    core.StreamStartupSnapshot
	publisherID string
	cfg         config.RecordConfig
	writer      *FileWriter
	reader      *util.RingReader[*avframe.AVFrame]
	transcoder  *core.TranscodeManager
	stopCursor  atomic.Int64
	stopOnce    sync.Once
	inputVideo  avframe.CodecType
	inputAudio  avframe.CodecType
	done        chan struct{}
	finished    chan struct{}
	startedAt   time.Time
	state       atomic.Pointer[RecordingSessionStatus]
	onComplete  func(RecordingSessionStatus)
}

// NewRecordSession creates a recording session for the given stream.
func NewRecordSession(streamKey string, stream *core.Stream, cfg config.RecordConfig) (*RecordSession, error) {
	writer, err := NewFileWriter(streamKey, cfg)
	if err != nil {
		return nil, err
	}

	return newRecordSessionWithWriter(streamKey, stream, cfg, writer), nil
}

func newRecordSession(streamKey string, stream *core.Stream, cfg config.RecordConfig, storage Storage, pathTemplate string, metrics *RecordingMetrics) (*RecordSession, error) {
	writer, err := newFileWriterWithStorage(streamKey, cfg, storage, pathTemplate, metrics)
	if err != nil {
		return nil, err
	}
	return newRecordSessionWithWriter(streamKey, stream, cfg, writer), nil
}

func newRecordSessionWithWriter(streamKey string, stream *core.Stream, cfg config.RecordConfig, writer *FileWriter) *RecordSession {
	session := &RecordSession{
		streamKey: streamKey,
		stream:    stream,
		snapshot:  stream.StartupSnapshot(),
		cfg:       cfg,
		writer:    writer,
		done:      make(chan struct{}),
		finished:  make(chan struct{}),
		startedAt: time.Now().UTC(),
	}
	session.updateStatus(RecordingActive, nil)
	return session
}

// Run starts the recording loop. Blocks until Stop is called or the stream closes.
func (s *RecordSession) Run() {
	defer close(s.finished)
	err := s.run()
	closeErr := s.writer.CloseWithError(err)
	if err == nil {
		err = closeErr
	}
	state := RecordingCompleted
	if err != nil {
		state = RecordingFailed
	}
	s.updateStatus(state, err)
	if s.onComplete != nil {
		s.onComplete(s.Status())
	}
}

// Wait blocks until Run has finalized the current recording.
func (s *RecordSession) Wait() { <-s.finished }

func (s *RecordSession) WaitUntil(deadline time.Time) bool {
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

func (s *RecordSession) run() error {
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
		if _, ended := snapshot.GenerationEndCursor(); !ended {
			return nil
		}
	}
	videoCodec := snapshot.MediaInfo.VideoCodec
	audioCodec := snapshot.MediaInfo.AudioCodec
	allowUndeclaredTracks := snapshot.Generation == 0
	transcodedAudio := false
	var releaseInput func()
	releaseInput = func() {}

	// fMP4 stores a browser-compatible audio track. G.711 has no sequence
	// header and is not an ISO-BMFF audio sample entry, so use the same shared
	// generation-bound AAC transform as the HTTP muxers when available.
	if strings.EqualFold(strings.TrimSpace(s.cfg.Format), "fmp4") &&
		!isRecordFMP4Audio(audioCodec) && audioCodec != 0 {
		if tm := s.stream.TranscodeManager(); tm != nil &&
			audiocodec.Global().CanTranscode(audioCodec, avframe.CodecAAC) &&
			len(audiocodec.Global().SequenceHeader(avframe.CodecAAC)) > 0 {
			reader, release, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, snapshot)
			if err == nil {
				s.reader = reader
				s.transcoder = tm
				releaseInput = release
				audioCodec = avframe.CodecAAC
				transcodedAudio = true
			} else {
				slog.Warn("record: audio transcode unavailable", "stream", s.streamKey, "codec", audioCodec, "error", err)
			}
		}
		if !transcodedAudio {
			// Preserve a playable video-only recording when the optional audio
			// dependency is unavailable instead of waiting for G.711 config.
			audioCodec = 0
		}
	}
	s.inputVideo = videoCodec
	s.inputAudio = audioCodec
	if !allowUndeclaredTracks {
		s.writer.SetExpectedTracks(videoCodec, audioCodec)
	}
	generationCtx, cancelGeneration := context.WithCancel(readCtx)
	defer cancelGeneration()
	defer releaseInput()
	go func() {
		select {
		case <-snapshot.GenerationDone:
			cancelGeneration()
		case <-generationCtx.Done():
		}
	}()

	// Transcoded input contains retained source video and generated AAC audio.
	// Its source cursor can begin after the original video header, so write the
	// captured video header and generated AAC header before consuming it.
	if transcodedAudio {
		if vsh := snapshot.VideoSequenceHeader; vsh != nil {
			if err := s.writer.WriteFrame(vsh); err != nil {
				return err
			}
		}
		ash := avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
			0, 0, audiocodec.Global().SequenceHeader(avframe.CodecAAC),
		)
		if err := s.writer.WriteFrame(ash); err != nil {
			return err
		}
	} else {
		// Write the headers captured with the same snapshot as the replay frames.
		if vsh := snapshot.VideoSequenceHeader; vsh != nil && videoCodec != 0 {
			if err := s.writer.WriteFrame(vsh); err != nil {
				slog.Error("write video seq header error", "module", "record", "stream", s.streamKey, "error", err)
				return err
			}
		}
		if ash := snapshot.AudioSequenceHeader; ash != nil && ash.Codec == audioCodec {
			if err := s.writer.WriteFrame(ash); err != nil {
				slog.Error("write audio seq header error", "module", "record", "stream", s.streamKey, "error", err)
				return err
			}
		}
		for _, frame := range snapshot.ReplayFrames {
			if snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(snapshot.Generation) {
				if _, ended := snapshot.GenerationEndCursor(); !ended {
					return nil
				}
			}
			if !recordFrameAccepted(frame, videoCodec, audioCodec, allowUndeclaredTracks) {
				continue
			}
			videoCodec, audioCodec = recordFrameCodecs(frame, videoCodec, audioCodec, allowUndeclaredTracks)
			s.inputVideo, s.inputAudio = videoCodec, audioCodec
			if err := s.writer.WriteFrame(frame); err != nil {
				slog.Error("write replay frame error", "module", "record", "stream", s.streamKey, "error", err)
				return err
			}
		}
	}
	readerCursor := snapshot.LiveCursor
	// NewRecordSession is also used as a standalone writer by callers that feed
	// an idle stream directly. Preserve that legacy path; module-managed
	// sessions always have a nonzero publisher generation and start at LiveCursor.
	if snapshot.Generation == 0 {
		readerCursor = snapshot.GenerationStartCursor
	}
	if s.reader == nil {
		s.reader = s.stream.RingBuffer().NewReaderAt(readerCursor)
	}

	for {
		frame, ok, readErr := core.ReadFrameContext(generationCtx, s.reader)
		if readErr != nil {
			slog.Error("record source continuity lost", "module", "record", "stream", s.streamKey, "error", readErr)
			return readErr
		}
		if !ok {
			_, generationEnded := snapshot.GenerationEndCursor()
			if isRecordStopRequested(s.done) || generationEnded {
				return s.drainPendingFrames()
			}
			return nil
		}
		if snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(snapshot.Generation) {
			endCursor, ended := snapshot.GenerationEndCursor()
			if !ended || (s.transcoder == nil && s.reader.ReadCursor() > endCursor) {
				return nil
			}
		}
		if !recordFrameAccepted(frame, videoCodec, audioCodec, allowUndeclaredTracks) {
			continue
		}
		videoCodec, audioCodec = recordFrameCodecs(frame, videoCodec, audioCodec, allowUndeclaredTracks)
		s.inputVideo, s.inputAudio = videoCodec, audioCodec
		if err := s.writer.WriteFrame(frame); err != nil {
			slog.Error("write frame error", "module", "record", "stream", s.streamKey, "error", err)
			return err
		}
	}
}

func isRecordFMP4Audio(codec avframe.CodecType) bool {
	return codec == avframe.CodecAAC
}

func recordFrameAccepted(frame *avframe.AVFrame, videoCodec, audioCodec avframe.CodecType, allowUndeclaredTracks bool) bool {
	if frame == nil {
		return false
	}
	if frame.MediaType.IsVideo() {
		if videoCodec == 0 {
			return allowUndeclaredTracks && frame.Codec != 0
		}
		return videoCodec != 0 && frame.Codec == videoCodec
	}
	if frame.MediaType.IsAudio() {
		if audioCodec == 0 {
			return allowUndeclaredTracks && frame.Codec != 0
		}
		return audioCodec != 0 && frame.Codec == audioCodec
	}
	return false
}

func recordFrameCodecs(frame *avframe.AVFrame, videoCodec, audioCodec avframe.CodecType, allowUndeclaredTracks bool) (avframe.CodecType, avframe.CodecType) {
	if !allowUndeclaredTracks || frame == nil {
		return videoCodec, audioCodec
	}
	if frame.MediaType.IsVideo() && videoCodec == 0 {
		videoCodec = frame.Codec
	}
	if frame.MediaType.IsAudio() && audioCodec == 0 {
		audioCodec = frame.Codec
	}
	return videoCodec, audioCodec
}

func isRecordStopRequested(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (s *RecordSession) drainPendingFrames() error {
	if s.reader == nil {
		return nil
	}
	generationEndCursor, generationEnded := s.snapshot.GenerationEndCursor()
	targetCursor := s.stopCursor.Load()
	finiteTarget := isRecordStopRequested(s.done)
	if generationEnded {
		targetCursor = generationEndCursor
		finiteTarget = true
	}
	if s.transcoder != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s.transcoder.WaitForSourceCursor(avframe.CodecAAC, targetCursor, ctx)
		cancel()
	}
	drainCtx := context.Background()
	var cancelDrain context.CancelFunc
	if s.transcoder != nil && generationEnded {
		drainCtx, cancelDrain = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelDrain()
	}
	for {
		allowUndeclaredTracks := s.snapshot.Generation == 0
		// A transcoded reader owns a generation-specific output ring. It remains
		// safe to drain after the source publisher detaches and cannot expose a
		// replacement generation. A direct source-ring reader still requires the
		// active-generation guard below.
		if s.transcoder == nil && finiteTarget && s.reader.ReadCursor() >= targetCursor {
			return nil
		}
		if s.transcoder == nil && !generationEnded && s.snapshot.Generation != 0 &&
			!s.stream.IsPublisherGeneration(s.snapshot.Generation) {
			return nil
		}
		var frame *avframe.AVFrame
		var ok bool
		var readErr error
		if s.transcoder != nil && generationEnded {
			frame, ok, readErr = core.ReadFrameContext(drainCtx, s.reader)
		} else {
			frame, ok, readErr = core.TryReadFrame(s.reader)
		}
		if readErr != nil {
			return readErr
		}
		if !ok {
			return nil
		}
		if s.transcoder == nil && finiteTarget && s.reader.ReadCursor() > targetCursor {
			return nil
		}
		if s.transcoder == nil && !generationEnded && s.snapshot.Generation != 0 &&
			!s.stream.IsPublisherGeneration(s.snapshot.Generation) {
			return nil
		}
		if !recordFrameAccepted(frame, s.inputVideo, s.inputAudio, allowUndeclaredTracks) {
			continue
		}
		s.inputVideo, s.inputAudio = recordFrameCodecs(frame, s.inputVideo, s.inputAudio, allowUndeclaredTracks)
		if err := s.writer.WriteFrame(frame); err != nil {
			slog.Error("write drained frame error", "module", "record", "stream", s.streamKey, "error", err)
			return err
		}
	}
}

// Stop signals the recording session to exit.
func (s *RecordSession) Stop() {
	if s == nil || s.done == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stream != nil {
			s.stopCursor.Store(s.stream.RingBuffer().WriteCursor())
		}
		close(s.done)
	})
}

func (s *RecordSession) updateStatus(state RecordingState, err error) {
	status := &RecordingSessionStatus{
		StreamKey:    s.streamKey,
		RecordingID:  s.writer.RecordingID(),
		State:        state,
		StartedAt:    s.startedAt,
		DurationSec:  time.Since(s.startedAt).Seconds(),
		Bytes:        s.writer.BytesWritten(),
		WriteRetries: s.writer.WriteRetries(),
	}
	if err != nil {
		status.LastError = err.Error()
	} else {
		status.LastError = s.writer.LastError()
	}
	if state != RecordingActive {
		status.CompletedAt = time.Now().UTC()
	}
	s.state.Store(status)
}

// Status returns an immutable point-in-time session status.
func (s *RecordSession) Status() RecordingSessionStatus {
	current := s.state.Load()
	if current == nil {
		return RecordingSessionStatus{StreamKey: s.streamKey, State: RecordingActive, StartedAt: s.startedAt}
	}
	status := *current
	if status.State == RecordingActive {
		status.RecordingID = s.writer.RecordingID()
		status.DurationSec = time.Since(status.StartedAt).Seconds()
		status.Bytes = s.writer.BytesWritten()
		status.WriteRetries = s.writer.WriteRetries()
		if last := s.writer.LastError(); last != "" {
			status.LastError = last
		}
	}
	return status
}
