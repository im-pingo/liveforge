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
)

// FileWriter manages writing AVFrames to FLV files with optional segmentation.
type FileWriter struct {
	cfg          config.RecordConfig
	streamKey    string
	muxer        *flv.Muxer
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
		muxer:     flv.NewMuxer(),
		startTime: time.Now(),
	}

	if err := w.openFile(); err != nil {
		return nil, err
	}
	return w, nil
}

// WriteFrame writes an AVFrame to the current FLV file.
// Handles FLV header writing on first frame and file segmentation.
func (w *FileWriter) WriteFrame(frame *avframe.AVFrame) error {
	if !w.headerDone {
		hasVideo := frame.MediaType.IsVideo()
		hasAudio := frame.MediaType.IsAudio()
		if err := w.muxer.WriteHeader(w.file, hasVideo, hasAudio); err != nil {
			return fmt.Errorf("write FLV header: %w", err)
		}
		w.headerDone = true
	}

	if err := w.muxer.WriteFrame(w.file, frame); err != nil {
		return fmt.Errorf("write FLV frame: %w", err)
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
	// Close current file
	if w.file != nil {
		w.file.Close()
		w.notifyFileComplete()
		slog.Info("segment complete", "module", "record", "path", w.filePath, "bytes", w.bytesWritten)
	}

	w.segmentIndex++

	// Open new file
	return w.openFile()
}

func (w *FileWriter) expandPath() string {
	now := time.Now()
	p := w.cfg.Path
	if p == "" {
		p = "./recordings/{stream_key}/{date}_{time}.flv"
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
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec
	if err != nil {
		slog.Error("file complete callback error", "module", "record", "error", err)
		return
	}
	resp.Body.Close()
}
