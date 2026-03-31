package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
)

// frameWriter is the interface for format-specific frame writing.
type frameWriter interface {
	// writeHeader writes format-specific file header on the first frame.
	writeHeader(f *os.File, frame *avframe.AVFrame) error
	// writeFrame writes a single frame.
	writeFrame(f *os.File, frame *avframe.AVFrame) error
}

// flvFrameWriter writes AVFrames using the FLV muxer.
type flvFrameWriter struct {
	muxer *flv.Muxer
}

func (w *flvFrameWriter) writeHeader(f *os.File, frame *avframe.AVFrame) error {
	hasVideo := frame.MediaType.IsVideo()
	hasAudio := frame.MediaType.IsAudio()
	return w.muxer.WriteHeader(f, hasVideo, hasAudio)
}

func (w *flvFrameWriter) writeFrame(f *os.File, frame *avframe.AVFrame) error {
	return w.muxer.WriteFrame(f, frame)
}

// fmp4FrameWriter writes AVFrames using the fMP4 muxer.
type fmp4FrameWriter struct {
	muxer     *fmp4.Muxer
	initDone  bool
	gopBuffer []*avframe.AVFrame
	videoSeq  *avframe.AVFrame
	audioSeq  *avframe.AVFrame
}

func (w *fmp4FrameWriter) writeHeader(f *os.File, frame *avframe.AVFrame) error {
	// fMP4 init segment requires codec info; defer until we have sequence headers.
	return nil
}

func (w *fmp4FrameWriter) writeFrame(f *os.File, frame *avframe.AVFrame) error {
	// Capture sequence headers for init segment
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			w.videoSeq = frame
		} else if frame.MediaType.IsAudio() {
			w.audioSeq = frame
		}

		// Write init segment once we have at least one sequence header
		if !w.initDone && (w.videoSeq != nil || w.audioSeq != nil) {
			videoCodec := avframe.CodecType(0)
			audioCodec := avframe.CodecType(0)
			if w.videoSeq != nil {
				videoCodec = w.videoSeq.Codec
			}
			if w.audioSeq != nil {
				audioCodec = w.audioSeq.Codec
			}

			w.muxer = fmp4.NewMuxer(videoCodec, audioCodec)
			initData := w.muxer.Init(w.videoSeq, w.audioSeq, 0, 0, 0, 0)
			if _, err := f.Write(initData); err != nil {
				return fmt.Errorf("write fMP4 init segment: %w", err)
			}
			w.initDone = true
		}
		return nil
	}

	if !w.initDone {
		return nil // skip media frames before init
	}

	w.gopBuffer = append(w.gopBuffer, frame)

	// Flush on keyframe (start of new GOP) — write the previous GOP
	if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && len(w.gopBuffer) > 1 {
		// Write all frames except the current keyframe (which starts a new GOP)
		toWrite := w.gopBuffer[:len(w.gopBuffer)-1]
		segData := w.muxer.WriteSegment(toWrite)
		if _, err := f.Write(segData); err != nil {
			return fmt.Errorf("write fMP4 segment: %w", err)
		}
		w.gopBuffer = w.gopBuffer[len(w.gopBuffer)-1:]
	}

	return nil
}

// flush writes any remaining buffered frames.
func (w *fmp4FrameWriter) flush(f *os.File) error {
	if !w.initDone || len(w.gopBuffer) == 0 || w.muxer == nil {
		return nil
	}
	segData := w.muxer.WriteSegment(w.gopBuffer)
	w.gopBuffer = nil
	if _, err := f.Write(segData); err != nil {
		return fmt.Errorf("flush fMP4 segment: %w", err)
	}
	return nil
}

// FileWriter manages writing AVFrames to files with optional segmentation.
type FileWriter struct {
	cfg          config.RecordConfig
	streamKey    string
	format       frameWriter
	file         *os.File
	filePath     string
	startTime    time.Time
	bytesWritten int64
	headerDone   bool
	segmentIndex int
}

// NewFileWriter creates a new file writer for the given stream key.
func NewFileWriter(streamKey string, cfg config.RecordConfig) (*FileWriter, error) {
	w := &FileWriter{
		cfg:       cfg,
		streamKey: streamKey,
		format:    newFrameWriter(cfg.Format),
		startTime: time.Now(),
	}

	if err := w.openFile(); err != nil {
		return nil, err
	}
	return w, nil
}

// newFrameWriter creates a format-specific writer based on the config format string.
func newFrameWriter(format string) frameWriter {
	switch strings.ToLower(format) {
	case "fmp4", "mp4":
		return &fmp4FrameWriter{}
	default:
		return &flvFrameWriter{muxer: flv.NewMuxer()}
	}
}

// Format returns the recording format string ("flv" or "fmp4").
func (w *FileWriter) Format() string {
	switch w.format.(type) {
	case *fmp4FrameWriter:
		return "fmp4"
	default:
		return "flv"
	}
}

// WriteFrame writes an AVFrame to the current file.
// Handles header writing on first frame and file segmentation.
func (w *FileWriter) WriteFrame(frame *avframe.AVFrame) error {
	if !w.headerDone {
		if err := w.format.writeHeader(w.file, frame); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
		w.headerDone = true
	}

	if err := w.format.writeFrame(w.file, frame); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	w.bytesWritten += int64(len(frame.Payload))

	// Check segmentation by duration
	if w.cfg.Segment.Duration > 0 && time.Since(w.startTime) >= w.cfg.Segment.Duration {
		if err := w.rotate(); err != nil {
			return fmt.Errorf("rotate file: %w", err)
		}
	}

	// Check segmentation by max file size
	if maxBytes := parseSize(w.cfg.Segment.MaxSize); maxBytes > 0 && w.bytesWritten >= maxBytes {
		if err := w.rotate(); err != nil {
			return fmt.Errorf("rotate file (size): %w", err)
		}
	}

	return nil
}

// Close flushes and closes the current file.
func (w *FileWriter) Close() {
	if w.file != nil {
		// Flush any buffered data for fMP4
		if fmp4w, ok := w.format.(*fmp4FrameWriter); ok {
			fmp4w.flush(w.file) //nolint:errcheck
		}
		w.file.Close()
		w.notifyFileComplete()
		slog.Info("file closed", "module", "record", "path", w.filePath, "bytes", w.bytesWritten)
		w.file = nil
	}
}

// FilePath returns the current file path (for testing).
func (w *FileWriter) FilePath() string {
	return w.filePath
}

func (w *FileWriter) openFile() error {
	filePath := w.expandPath()

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("create file %s: %w", filePath, err)
	}

	w.file = f
	w.filePath = filePath
	w.headerDone = false
	w.bytesWritten = 0
	w.startTime = time.Now()
	return nil
}

func (w *FileWriter) rotate() error {
	// Flush any buffered data for fMP4
	if fmp4w, ok := w.format.(*fmp4FrameWriter); ok {
		fmp4w.flush(w.file) //nolint:errcheck
	}

	// Close current file
	if w.file != nil {
		w.file.Close()
		w.notifyFileComplete()
		slog.Info("segment complete", "module", "record", "path", w.filePath, "bytes", w.bytesWritten)
	}

	w.segmentIndex++

	// Reset format writer for new segment
	w.format = newFrameWriter(w.cfg.Format)

	// Open new file
	return w.openFile()
}

func (w *FileWriter) expandPath() string {
	now := time.Now()
	p := w.cfg.Path
	if p == "" {
		ext := "flv"
		if w.Format() == "fmp4" {
			ext = "mp4"
		}
		p = fmt.Sprintf("./recordings/{stream_key}/{date}_{time}.%s", ext)
	}

	// Replace stream_key slashes with OS path separator for directory structure
	streamDir := strings.ReplaceAll(w.streamKey, "/", string(filepath.Separator))

	p = strings.ReplaceAll(p, "{stream_key}", streamDir)
	p = strings.ReplaceAll(p, "{date}", now.Format("2006-01-02"))
	p = strings.ReplaceAll(p, "{time}", fmt.Sprintf("%s_%04d", now.Format("150405"), w.segmentIndex))
	return p
}

// parseSize parses a human-readable size string like "512MB", "1GB", "100KB".
// Returns the size in bytes. Returns 0 if the string is empty or unparseable.
func parseSize(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0
	}
	return n * multiplier
}

func (w *FileWriter) notifyFileComplete() {
	url := w.cfg.OnFileComplete.URL
	if url == "" {
		return
	}

	payload := map[string]any{
		"stream_key": w.streamKey,
		"file_path":  w.filePath,
		"bytes":      w.bytesWritten,
		"duration":   time.Since(w.startTime).Seconds(),
		"format":     w.Format(),
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec
	if err != nil {
		slog.Error("file complete callback error", "module", "record", "error", err)
		return
	}
	resp.Body.Close()
}
