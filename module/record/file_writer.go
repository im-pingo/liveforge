package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/mp4"
)

// frameWriter is the interface for format-specific frame writing.
type mediaFile interface {
	io.WriteSeeker
	Name() string
}

type frameWriter interface {
	// writeHeader writes format-specific file header on the first frame.
	writeHeader(f mediaFile, frame *avframe.AVFrame) error
	// writeFrame writes a single frame.
	writeFrame(f mediaFile, frame *avframe.AVFrame) error
}

// flvFrameWriter writes AVFrames using the FLV muxer.
type flvFrameWriter struct {
	muxer             *flv.Muxer
	expectedTracksSet bool
	expectedVideo     bool
	expectedAudio     bool
	videoSeq          *avframe.AVFrame
	audioSeq          *avframe.AVFrame
	videoBaseDTS      int64
	audioBaseDTS      int64
	videoBaseSet      bool
	audioBaseSet      bool
}

func (w *flvFrameWriter) writeHeader(f mediaFile, frame *avframe.AVFrame) error {
	hasVideo := frame.MediaType.IsVideo() || w.videoSeq != nil
	hasAudio := frame.MediaType.IsAudio() || w.audioSeq != nil
	if w.expectedTracksSet {
		hasVideo = w.expectedVideo
		hasAudio = w.expectedAudio
	}
	if err := w.muxer.WriteHeader(f, hasVideo, hasAudio); err != nil {
		return err
	}
	for _, sequenceHeader := range []*avframe.AVFrame{w.videoSeq, w.audioSeq} {
		if sequenceHeader != nil {
			if err := w.writeFrame(f, sequenceHeader); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *flvFrameWriter) writeFrame(f mediaFile, frame *avframe.AVFrame) error {
	normalized := *frame
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		normalized.DTS = 0
		normalized.PTS = 0
	} else if frame.MediaType.IsVideo() {
		if !w.videoBaseSet {
			w.videoBaseDTS = frame.DTS
			w.videoBaseSet = true
		}
		normalized.DTS -= w.videoBaseDTS
		normalized.PTS -= w.videoBaseDTS
	} else if frame.MediaType.IsAudio() {
		if !w.audioBaseSet {
			w.audioBaseDTS = frame.DTS
			w.audioBaseSet = true
		}
		normalized.DTS -= w.audioBaseDTS
		normalized.PTS -= w.audioBaseDTS
	}
	return w.muxer.WriteFrame(f, &normalized)
}

func (w *flvFrameWriter) setExpectedTracks(videoCodec, audioCodec avframe.CodecType) {
	w.expectedTracksSet = true
	w.expectedVideo = videoCodec.IsVideo()
	w.expectedAudio = audioCodec.IsAudio()
}

// fmp4FrameWriter writes AVFrames using the fMP4 muxer.
type fmp4FrameWriter struct {
	muxer             *fmp4.Muxer
	initDone          bool
	gopBuffer         []*avframe.AVFrame
	pendingFrames     []*avframe.AVFrame
	videoSeq          *avframe.AVFrame
	audioSeq          *avframe.AVFrame
	expectedTracksSet bool
	expectedVideo     bool
	expectedAudio     bool
	videoBaseDTS      int64
	audioBaseDTS      int64
	videoBaseSet      bool
	audioBaseSet      bool
}

func (w *fmp4FrameWriter) writeHeader(f mediaFile, frame *avframe.AVFrame) error {
	// fMP4 init segment requires codec info; defer until we have sequence headers.
	return nil
}

func (w *fmp4FrameWriter) writeFrame(f mediaFile, frame *avframe.AVFrame) error {
	// Capture sequence headers for init segment
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			w.videoSeq = frame
		} else if frame.MediaType.IsAudio() {
			w.audioSeq = frame
		}
		if w.expectedTracksSet && w.tracksReady() && len(w.pendingFrames) > 0 {
			if err := w.initialize(f, nil); err != nil {
				return err
			}
			for _, pending := range w.pendingFrames {
				if err := w.writeMediaFrame(f, pending); err != nil {
					return err
				}
			}
			w.pendingFrames = nil
		}

		return nil
	}

	if !w.initDone {
		if w.expectedTracksSet && !w.tracksReady() {
			w.pendingFrames = append(w.pendingFrames, frame)
			return nil
		}
		if err := w.initialize(f, frame); err != nil {
			return err
		}
	}

	return w.writeMediaFrame(f, frame)
}

func (w *fmp4FrameWriter) setExpectedTracks(videoCodec, audioCodec avframe.CodecType) {
	w.expectedTracksSet = true
	w.expectedVideo = videoCodec.IsVideo()
	w.expectedAudio = audioCodec.IsAudio()
}

func (w *fmp4FrameWriter) tracksReady() bool {
	return (!w.expectedVideo || w.videoSeq != nil) && (!w.expectedAudio || w.audioSeq != nil)
}

func (w *fmp4FrameWriter) initialize(f mediaFile, frame *avframe.AVFrame) error {
	videoCodec := avframe.CodecType(0)
	audioCodec := avframe.CodecType(0)
	if w.videoSeq != nil {
		videoCodec = w.videoSeq.Codec
	}
	if w.audioSeq != nil {
		audioCodec = w.audioSeq.Codec
	}
	if frame != nil && videoCodec == 0 && frame.MediaType.IsVideo() {
		videoCodec = frame.Codec
	}
	if frame != nil && audioCodec == 0 && frame.MediaType.IsAudio() {
		audioCodec = frame.Codec
	}
	w.muxer = fmp4.NewMuxer(videoCodec, audioCodec)
	initData := w.muxer.Init(w.videoSeq, w.audioSeq, 0, 0, 0, 0)
	if _, err := f.Write(initData); err != nil {
		return fmt.Errorf("write fMP4 init segment: %w", err)
	}
	w.initDone = true
	return nil
}

func (w *fmp4FrameWriter) writeMediaFrame(f mediaFile, frame *avframe.AVFrame) error {
	if frame.MediaType.IsVideo() && !w.videoBaseSet {
		w.videoBaseDTS = frame.DTS
		w.videoBaseSet = true
	}
	if frame.MediaType.IsAudio() && !w.audioBaseSet {
		w.audioBaseDTS = frame.DTS
		w.audioBaseSet = true
	}
	w.gopBuffer = append(w.gopBuffer, frame)

	// Flush on keyframe (start of new GOP) — write the previous GOP
	if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && len(w.gopBuffer) > 1 {
		// Write all frames except the current keyframe (which starts a new GOP)
		toWrite := w.gopBuffer[:len(w.gopBuffer)-1]
		segData := w.muxer.WriteSegmentWithBaseDTS(toWrite, w.videoBaseDTS, w.audioBaseDTS)
		if _, err := f.Write(segData); err != nil {
			return fmt.Errorf("write fMP4 segment: %w", err)
		}
		w.gopBuffer = w.gopBuffer[len(w.gopBuffer)-1:]
	}
	return nil
}

// flush writes any remaining buffered frames.
func (w *fmp4FrameWriter) flush(f mediaFile) error {
	if !w.initDone {
		if len(w.pendingFrames) > 0 {
			return ErrRecordingCodecConfig
		}
		return nil
	}
	if len(w.gopBuffer) == 0 || w.muxer == nil {
		return nil
	}
	segData := w.muxer.WriteSegmentWithBaseDTS(w.gopBuffer, w.videoBaseDTS, w.audioBaseDTS)
	w.gopBuffer = nil
	if _, err := f.Write(segData); err != nil {
		return fmt.Errorf("flush fMP4 segment: %w", err)
	}
	return nil
}

// mp4FrameWriter writes AVFrames as a classic MP4 with moov atom at end.
type mp4FrameWriter struct {
	muxer   *mp4.Muxer
	started bool
}

func (w *mp4FrameWriter) writeHeader(f mediaFile, frame *avframe.AVFrame) error {
	return nil // defer until we have codec info
}

func (w *mp4FrameWriter) writeFrame(f mediaFile, frame *avframe.AVFrame) error {
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if w.muxer == nil {
			videoCodec := avframe.CodecType(0)
			audioCodec := avframe.CodecType(0)
			if frame.MediaType.IsVideo() {
				videoCodec = frame.Codec
			} else {
				audioCodec = frame.Codec
			}
			w.muxer = mp4.NewMuxer(videoCodec, audioCodec)
		}
		_, err := w.muxer.WriteFrame(f, frame)
		return err
	}

	if !w.started || w.muxer == nil {
		if w.muxer == nil {
			w.muxer = mp4.NewMuxer(frame.Codec, 0)
		}
		if err := w.muxer.WriteFtyp(f); err != nil {
			return err
		}
		if _, err := w.muxer.WriteMdatHeader(f); err != nil {
			return err
		}
		w.started = true
	}

	_, err := w.muxer.WriteFrame(f, frame)
	return err
}

func (w *mp4FrameWriter) finalize(f mediaFile) error {
	if w.muxer == nil || !w.started {
		return nil
	}
	return w.muxer.Finalize(f)
}

// FileWriter manages writing AVFrames to files with optional segmentation.
type FileWriter struct {
	cfg           config.RecordConfig
	streamKey     string
	format        frameWriter
	storage       Storage
	pathTemplate  string
	pathToken     string
	object        WriteObject
	file          mediaFile
	filePath      string
	recordingID   string
	startTime     time.Time
	bytesWritten  atomic.Int64
	totalBytes    atomic.Int64
	writeRetries  atomic.Uint64
	lastError     atomic.Pointer[string]
	headerDone    bool
	hasMedia      bool
	videoSeen     bool
	rotatePending bool
	segmentIndex  int
	expectedSet   bool
	videoCodec    avframe.CodecType
	audioCodec    avframe.CodecType
	videoSeq      *avframe.AVFrame
	audioSeq      *avframe.AVFrame
	metrics       *RecordingMetrics
	ownsStorage   bool
	storageOnce   sync.Once
	storageErr    error
}

var fileWriterPathSequence atomic.Uint64

// NewFileWriter creates a new file writer for the given stream key.
func NewFileWriter(streamKey string, cfg config.RecordConfig) (*FileWriter, error) {
	storage, template, err := newStorageForConfig(cfg)
	if err != nil {
		return nil, err
	}
	writer, err := newFileWriterWithStorage(streamKey, cfg, storage, template, nil)
	if err != nil {
		_ = storage.Close()
		return nil, err
	}
	writer.ownsStorage = true
	return writer, nil
}

func newFileWriterWithStorage(streamKey string, cfg config.RecordConfig, storage Storage, pathTemplate string, metrics *RecordingMetrics) (*FileWriter, error) {
	now := time.Now().UTC()
	w := &FileWriter{
		cfg:          cfg,
		streamKey:    streamKey,
		format:       newFrameWriterWithContext(cfg.Format, streamKey, cfg),
		storage:      storage,
		pathTemplate: pathTemplate,
		pathToken:    fmt.Sprintf("%x-%x", now.UnixNano(), fileWriterPathSequence.Add(1)),
		startTime:    now,
		metrics:      metrics,
	}

	if err := w.openFile(); err != nil {
		return nil, err
	}
	return w, nil
}

// newFrameWriter creates a format-specific writer based on the config format string.
func newFrameWriter(format string) frameWriter {
	return newFrameWriterWithContext(format, "", config.RecordConfig{})
}

func newFrameWriterWithContext(format, streamKey string, cfg config.RecordConfig) frameWriter {
	if strings.TrimSpace(format) == "" {
		format = "fmp4"
	}
	switch strings.ToLower(format) {
	case "mp4":
		return &mp4FrameWriter{}
	case "fmp4":
		return &fmp4FrameWriter{}
	case "ts", "hls":
		return newTSFrameWriter(streamKey, cfg)
	default:
		return &flvFrameWriter{muxer: flv.NewMuxer()}
	}
}

// Format returns the recording format string ("flv", "fmp4", "mp4", or "ts").
func (w *FileWriter) Format() string {
	switch w.format.(type) {
	case *mp4FrameWriter:
		return "mp4"
	case *fmp4FrameWriter:
		return "fmp4"
	case *tsFrameWriter:
		return "ts"
	default:
		return "flv"
	}
}

// SetExpectedTracks tells the writer which tracks the publisher declared.
// The declaration is retained across file rotation.
func (w *FileWriter) SetExpectedTracks(videoCodec, audioCodec avframe.CodecType) {
	w.expectedSet = true
	w.videoCodec = videoCodec
	w.audioCodec = audioCodec
	w.applyFormatState()
}

// WriteFrame writes an AVFrame to the current file.
// Handles header writing on first frame and file segmentation.
func (w *FileWriter) WriteFrame(frame *avframe.AVFrame) error {
	if w.hasMedia && w.cfg.Segment.Duration > 0 && time.Since(w.startTime) >= w.cfg.Segment.Duration {
		w.rotatePending = true
	}
	if w.rotatePending && w.canStartAutomaticSegment(frame) {
		if err := w.rotate(); err != nil {
			return fmt.Errorf("rotate file: %w", err)
		}
	}
	if frame.FrameType != avframe.FrameTypeSequenceHeader {
		w.hasMedia = true
	}
	if frame.MediaType.IsVideo() {
		w.videoSeen = true
	}
	if !w.headerDone {
		if err := w.format.writeHeader(w.file, frame); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
		w.headerDone = true
	}

	if err := w.format.writeFrame(w.file, frame); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			w.videoSeq = cloneRecordFrame(frame)
		} else if frame.MediaType.IsAudio() {
			w.audioSeq = cloneRecordFrame(frame)
		}
	}
	w.bytesWritten.Add(int64(len(frame.Payload)))
	w.totalBytes.Add(int64(len(frame.Payload)))
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		return nil
	}

	// Check segmentation by max file size
	if maxBytes := parseSize(w.cfg.Segment.MaxSize); maxBytes > 0 && w.bytesWritten.Load() >= maxBytes {
		w.rotatePending = true
	}

	return nil
}

func (w *FileWriter) canStartAutomaticSegment(frame *avframe.AVFrame) bool {
	if frame == nil || frame.FrameType == avframe.FrameTypeSequenceHeader {
		return false
	}
	hasVideo := w.videoCodec.IsVideo() || w.videoSeq != nil || w.videoSeen || frame.MediaType.IsVideo()
	if hasVideo {
		return frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe()
	}
	return frame.MediaType.IsAudio()
}

// Close flushes and closes the current file.
func (w *FileWriter) Close() {
	_ = w.CloseWithError(nil)
}

// CloseWithError finalizes a successful file or preserves it as failed.
func (w *FileWriter) CloseWithError(cause error) error {
	return errors.Join(w.finishCurrent(cause), w.closeOwnedStorage())
}

func (w *FileWriter) closeOwnedStorage() error {
	if !w.ownsStorage {
		return nil
	}
	w.storageOnce.Do(func() {
		if closer, ok := w.storage.(interface{ Close() error }); ok {
			w.storageErr = closer.Close()
		}
	})
	return w.storageErr
}

// FilePath returns the current file path (for testing).
func (w *FileWriter) FilePath() string {
	return w.filePath
}

func (w *FileWriter) RecordingID() string { return w.recordingID }

func (w *FileWriter) BytesWritten() int64 { return w.totalBytes.Load() }

func (w *FileWriter) WriteRetries() uint64 { return w.writeRetries.Load() }

func (w *FileWriter) LastError() string {
	if value := w.lastError.Load(); value != nil {
		return *value
	}
	return ""
}

func (w *FileWriter) openFile() error {
	w.startTime = time.Now().UTC()
	id := w.expandPath()
	object, err := w.storage.Create(context.Background(), id, RecordingInfo{
		StreamKey: w.streamKey,
		Format:    w.Format(),
		StartedAt: w.startTime,
	})
	if err != nil {
		return err
	}
	retry := newRetryWriteSeeker(object, 3, func() {
		w.writeRetries.Add(1)
		if w.metrics != nil {
			w.metrics.retries.Add(1)
		}
	})
	w.object = object
	w.file = retry
	w.filePath = object.Name()
	w.recordingID = id
	w.headerDone = false
	w.hasMedia = false
	w.bytesWritten.Store(0)
	return nil
}

func (w *FileWriter) rotate() error {
	if err := w.finishCurrent(nil); err != nil {
		return err
	}

	w.segmentIndex++

	// Reset format writer for new segment
	w.format = newFrameWriterWithContext(w.cfg.Format, w.streamKey, w.cfg)
	w.applyFormatState()

	// Open new file
	if err := w.openFile(); err != nil {
		return err
	}
	w.rotatePending = false
	return nil
}

func (w *FileWriter) applyFormatState() {
	switch writer := w.format.(type) {
	case *flvFrameWriter:
		if w.expectedSet {
			writer.setExpectedTracks(w.videoCodec, w.audioCodec)
		}
		writer.videoSeq = cloneRecordFrame(w.videoSeq)
		writer.audioSeq = cloneRecordFrame(w.audioSeq)
	case *fmp4FrameWriter:
		if w.expectedSet {
			writer.setExpectedTracks(w.videoCodec, w.audioCodec)
		}
		writer.videoSeq = cloneRecordFrame(w.videoSeq)
		writer.audioSeq = cloneRecordFrame(w.audioSeq)
	case *mp4FrameWriter:
		videoCodec, audioCodec := w.restoredCodecs()
		if videoCodec != 0 || audioCodec != 0 {
			writer.muxer = mp4.NewMuxer(videoCodec, audioCodec)
		}
		for _, sequenceHeader := range []*avframe.AVFrame{w.videoSeq, w.audioSeq} {
			if sequenceHeader != nil {
				_, _ = writer.muxer.WriteFrame(nil, sequenceHeader)
			}
		}
	case *tsFrameWriter:
		writer.videoCodec, writer.audioCodec = w.restoredCodecs()
		if w.videoSeq != nil {
			writer.videoSeq = append([]byte(nil), w.videoSeq.Payload...)
		}
		if w.audioSeq != nil {
			writer.audioSeq = append([]byte(nil), w.audioSeq.Payload...)
		}
	}
}

func (w *FileWriter) restoredCodecs() (avframe.CodecType, avframe.CodecType) {
	videoCodec, audioCodec := w.videoCodec, w.audioCodec
	if !w.expectedSet {
		if w.videoSeq != nil {
			videoCodec = w.videoSeq.Codec
		}
		if w.audioSeq != nil {
			audioCodec = w.audioSeq.Codec
		}
	}
	return videoCodec, audioCodec
}

func cloneRecordFrame(frame *avframe.AVFrame) *avframe.AVFrame {
	if frame == nil {
		return nil
	}
	clone := *frame
	clone.Payload = append([]byte(nil), frame.Payload...)
	return &clone
}

func (w *FileWriter) expandPath() string {
	now := time.Now()
	p := w.pathTemplate
	hasTimePlaceholder := strings.Contains(p, "{time}")

	// Replace stream_key slashes with OS path separator for directory structure
	streamDir := strings.ReplaceAll(filepath.ToSlash(w.streamKey), "/", string(filepath.Separator))

	p = strings.ReplaceAll(p, "{stream_key}", streamDir)
	p = strings.ReplaceAll(p, "{date}", now.Format("2006-01-02"))
	p = strings.ReplaceAll(p, "{time}", fmt.Sprintf("%s_%04d", now.Format("150405"), w.segmentIndex))
	ext := "flv"
	switch w.Format() {
	case "mp4", "fmp4":
		ext = "mp4"
	case "ts":
		ext = "ts"
	}
	p = strings.ReplaceAll(p, "{ext}", ext)
	pathExt := strings.ToLower(filepath.Ext(p))
	switch pathExt {
	case ".flv", ".mp4", ".m4v", ".ts", ".m3u8":
		if pathExt != "."+ext {
			p = strings.TrimSuffix(p, filepath.Ext(p)) + "." + ext
		}
	}
	if w.segmentIndex > 0 && !hasTimePlaceholder {
		pathExt = filepath.Ext(p)
		p = strings.TrimSuffix(p, pathExt) + fmt.Sprintf(".segment_%s_%04d", w.pathToken, w.segmentIndex) + pathExt
	}
	return filepath.ToSlash(filepath.Clean(p))
}

func (w *FileWriter) finishCurrent(cause error) error {
	if w.object == nil {
		return nil
	}
	if cause == nil {
		if !w.hasMedia {
			cause = ErrRecordingNoMedia
		} else {
			switch fw := w.format.(type) {
			case *fmp4FrameWriter:
				cause = fw.flush(w.file)
			case *mp4FrameWriter:
				cause = fw.finalize(w.file)
			case *tsFrameWriter:
				cause = fw.flush(w.file)
			}
		}
	} else if fw, ok := w.format.(*tsFrameWriter); ok {
		fw.abort()
	}
	update := RecordingInfo{DurationSec: time.Since(w.startTime).Seconds(), Format: w.Format(), StreamKey: w.streamKey}
	var info RecordingInfo
	var err error
	if cause != nil {
		w.setLastError(cause)
		info, err = w.object.Fail(context.Background(), cause)
		if w.metrics != nil {
			w.metrics.failed.Add(1)
			w.metrics.writeFailure.Add(1)
			w.metrics.storageError.Add(1)
		}
	} else {
		info, err = w.object.Complete(context.Background(), update)
		if err == nil && w.metrics != nil {
			w.metrics.completed.Add(1)
			if info.Size > 0 {
				w.metrics.bytesWritten.Add(uint64(info.Size))
			}
		}
	}
	if info.ID != "" {
		w.recordingID = info.ID
		if local, ok := w.storage.(*LocalStorage); ok {
			w.filePath = filepath.Join(local.Root(), filepath.FromSlash(info.ID))
		} else if strings.HasSuffix(w.filePath, ".partial") {
			w.filePath = strings.TrimSuffix(w.filePath, ".partial")
			if info.State == RecordingFailed {
				w.filePath += ".failed"
			}
		}
	}
	w.object = nil
	w.file = nil
	if err != nil {
		if w.metrics != nil && cause == nil {
			w.metrics.failed.Add(1)
			w.metrics.storageError.Add(1)
		}
		w.setLastError(err)
		return err
	}
	if cause != nil {
		return cause
	}
	w.notifyFileComplete()
	slog.Info("file closed", "module", "record", "path", w.filePath, "bytes", w.bytesWritten.Load())
	return nil
}

func (w *FileWriter) setLastError(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	w.lastError.Store(&message)
}

// parseSize parses a human-readable size string like "512MB", "1GB", "100KB".
// Returns the size in bytes. Returns 0 if the string is empty or unparseable.
func parseSize(s string) int64 {
	size, err := config.ParseByteSize(s)
	if err != nil {
		return 0
	}
	return size
}

func (w *FileWriter) notifyFileComplete() {
	url := w.cfg.OnFileComplete.URL
	if url == "" {
		return
	}

	payload := map[string]any{
		"stream_key": w.streamKey,
		"file_path":  w.filePath,
		"bytes":      w.bytesWritten.Load(),
		"duration":   time.Since(w.startTime).Seconds(),
		"format":     w.Format(),
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec
	if err != nil {
		slog.Error("file complete callback error", "module", "record", "endpoint", callbackLogTarget(url), "reason", "delivery failed")
		return
	}
	resp.Body.Close()
}

func callbackLogTarget(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "invalid"
	}
	return parsed.Scheme + "://" + parsed.Host
}
