package record

import (
	"log/slog"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// RecordSession reads frames from a stream's RingBuffer and writes them to an FLV file.
type RecordSession struct {
	streamKey string
	stream    *core.Stream
	cfg       config.RecordConfig
	writer    *FileWriter
	reader    *util.RingReader[*avframe.AVFrame]
	done      chan struct{}
}

// NewRecordSession creates a recording session for the given stream.
func NewRecordSession(streamKey string, stream *core.Stream, cfg config.RecordConfig) (*RecordSession, error) {
	writer, err := NewFileWriter(streamKey, cfg)
	if err != nil {
		return nil, err
	}

	return &RecordSession{
		streamKey: streamKey,
		stream:    stream,
		cfg:       cfg,
		writer:    writer,
		reader:    stream.RingBuffer().NewReader(),
		done:      make(chan struct{}),
	}, nil
}

// Run starts the recording loop. Blocks until Stop is called or the stream closes.
func (s *RecordSession) Run() {
	defer s.writer.Close()

	// Write sequence headers first if available
	if vsh := s.stream.VideoSeqHeader(); vsh != nil {
		if err := s.writer.WriteFrame(vsh); err != nil {
			slog.Error("write video seq header error", "module", "record", "stream", s.streamKey, "error", err)
			return
		}
	}
	if ash := s.stream.AudioSeqHeader(); ash != nil {
		if err := s.writer.WriteFrame(ash); err != nil {
			slog.Error("write audio seq header error", "module", "record", "stream", s.streamKey, "error", err)
			return
		}
	}

	for {
		// Non-blocking read + done check to allow clean shutdown
		frame, ok := s.reader.TryRead()
		if ok {
			if err := s.writer.WriteFrame(frame); err != nil {
				slog.Error("write frame error", "module", "record", "stream", s.streamKey, "error", err)
				return
			}
			continue
		}

		// No data available — wait for signal or done
		select {
		case <-s.done:
			return
		case <-s.stream.RingBuffer().Signal():
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
