package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
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

	// Check segmentation
	if w.cfg.Segment.Duration > 0 && time.Since(w.startTime) >= w.cfg.Segment.Duration {
		if err := w.rotate(); err != nil {
			return fmt.Errorf("rotate file: %w", err)
		}
	}

	return nil
}

// Close flushes and closes the current file.
func (w *FileWriter) Close() {
	if w.file != nil {
		w.file.Close()
		w.notifyFileComplete()
		log.Printf("[record] closed %s (%d bytes)", w.filePath, w.bytesWritten)
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
		log.Printf("[record] segment complete: %s (%d bytes)", w.filePath, w.bytesWritten)
	}

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
	p = strings.ReplaceAll(p, "{time}", now.Format("150405"))
	return p
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
		log.Printf("[record] file complete callback error: %v", err)
		return
	}
	resp.Body.Close()
}
