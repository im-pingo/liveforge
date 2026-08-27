package record

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// RecordSession reads frames from a stream's RingBuffer and writes them to an FLV file.
type RecordSession struct {
	streamKey   string
	stream      *core.Stream
	snapshot    core.StreamStartupSnapshot
	publisherID string
	cfg         config.RecordConfig
	writer      *FileWriter
	reader      *util.RingReader[*avframe.AVFrame]
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
		return nil
	}
	s.writer.SetExpectedTracks(snapshot.MediaInfo.VideoCodec, snapshot.MediaInfo.AudioCodec)
	generationCtx, cancelGeneration := context.WithCancel(readCtx)
	defer cancelGeneration()
	go func() {
		select {
		case <-snapshot.GenerationDone:
			cancelGeneration()
		case <-generationCtx.Done():
		}
	}()

	// Write the headers captured with the same snapshot as the replay frames.
	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		if err := s.writer.WriteFrame(vsh); err != nil {
			slog.Error("write video seq header error", "module", "record", "stream", s.streamKey, "error", err)
			return err
		}
	}
	if ash := snapshot.AudioSequenceHeader; ash != nil {
		if err := s.writer.WriteFrame(ash); err != nil {
			slog.Error("write audio seq header error", "module", "record", "stream", s.streamKey, "error", err)
			return err
		}
	}
	for _, frame := range snapshot.ReplayFrames {
		if snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(snapshot.Generation) {
			return nil
		}
		if err := s.writer.WriteFrame(frame); err != nil {
			slog.Error("write replay frame error", "module", "record", "stream", s.streamKey, "error", err)
			return err
		}
	}
	readerCursor := snapshot.LiveCursor
	// NewRecordSession is also used as a standalone writer by callers that feed
	// an idle stream directly. Preserve that legacy path; module-managed
	// sessions always have a nonzero publisher generation and start at LiveCursor.
	if snapshot.Generation == 0 {
		readerCursor = snapshot.GenerationStartCursor
	}
	s.reader = s.stream.RingBuffer().NewReaderAt(readerCursor)

	for {
		frame, ok := s.reader.ReadContext(generationCtx)
		if !ok {
			if isRecordStopRequested(s.done) {
				return s.drainPendingFrames()
			}
			return nil
		}
		if snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(snapshot.Generation) {
			return nil
		}
		if err := s.writer.WriteFrame(frame); err != nil {
			slog.Error("write frame error", "module", "record", "stream", s.streamKey, "error", err)
			return err
		}
	}
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
	for {
		if s.snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
			return nil
		}
		frame, ok := s.reader.TryRead()
		if !ok {
			return nil
		}
		if s.snapshot.Generation != 0 && !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
			return nil
		}
		if err := s.writer.WriteFrame(frame); err != nil {
			slog.Error("write drained frame error", "module", "record", "stream", s.streamKey, "error", err)
			return err
		}
	}
}

// Stop signals the recording session to exit.
func (s *RecordSession) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
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
