package record

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// tsFrameWriter writes AVFrames as MPEG-TS segments with an HLS playlist.
type tsFrameWriter struct {
	cfg       config.RecordConfig
	streamKey string

	muxer      *ts.Muxer
	videoSeq   []byte
	audioSeq   []byte
	videoCodec avframe.CodecType
	audioCodec avframe.CodecType

	segmentDir  string
	segmentFile *os.File
	segmentPath string
	segmentIdx  int
	segmentDur  time.Duration
	segStartTS  int64 // DTS of first frame in segment (ms)
	lastDTS     int64

	segments []segmentInfo
}

type segmentInfo struct {
	filename string
	duration float64 // seconds
}

func newTSFrameWriter(streamKey string, cfg config.RecordConfig) *tsFrameWriter {
	return &tsFrameWriter{
		cfg:        cfg,
		streamKey:  streamKey,
		lastDTS:    -1,
		segStartTS: -1,
	}
}

func (w *tsFrameWriter) writeHeader(_ *os.File, _, _, _ *avframe.AVFrame) error {
	return nil
}

func (w *tsFrameWriter) writeFrame(f *os.File, frame *avframe.AVFrame) error {
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			w.videoSeq = append([]byte(nil), frame.Payload...)
			w.videoCodec = frame.Codec
		} else if frame.MediaType.IsAudio() {
			w.audioSeq = append([]byte(nil), frame.Payload...)
			w.audioCodec = frame.Codec
		}
		return nil
	}

	if w.muxer == nil {
		w.muxer = ts.NewMuxer(w.videoCodec, w.audioCodec, w.videoSeq, w.audioSeq)
		w.segmentDir = filepath.Dir(f.Name())
	}

	// Start new segment on keyframe or if no segment is open
	if w.segmentFile == nil || (frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && w.shouldSplit()) {
		if err := w.closeSegment(); err != nil {
			return err
		}
		if err := w.openSegment(); err != nil {
			return err
		}
	}

	data := w.muxer.WriteFrame(frame)
	if data != nil {
		if _, err := w.segmentFile.Write(data); err != nil {
			return fmt.Errorf("write TS data: %w", err)
		}
	}

	if w.segStartTS < 0 {
		w.segStartTS = frame.DTS
	}
	w.lastDTS = frame.DTS

	return nil
}

func (w *tsFrameWriter) shouldSplit() bool {
	if w.segStartTS < 0 {
		return false
	}
	dur := w.cfg.Segment.Duration
	if dur <= 0 {
		dur = 6 * time.Second
	}
	elapsed := time.Duration(w.lastDTS-w.segStartTS) * time.Millisecond
	return elapsed >= dur
}

func (w *tsFrameWriter) openSegment() error {
	filename := fmt.Sprintf("segment_%05d.ts", w.segmentIdx)
	path := filepath.Join(w.segmentDir, filename)

	sf, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create segment %s: %w", path, err)
	}

	w.segmentFile = sf
	w.segmentPath = path
	w.segStartTS = -1
	return nil
}

func (w *tsFrameWriter) closeSegment() error {
	if w.segmentFile == nil {
		return nil
	}

	w.segmentFile.Close()

	dur := float64(0)
	if w.segStartTS >= 0 && w.lastDTS > w.segStartTS {
		dur = float64(w.lastDTS-w.segStartTS) / 1000.0
	}

	w.segments = append(w.segments, segmentInfo{
		filename: filepath.Base(w.segmentPath),
		duration: dur,
	})
	w.segmentIdx++
	w.segmentFile = nil

	return nil
}

// flush writes the final segment and playlist.
func (w *tsFrameWriter) flush(f *os.File) error {
	if err := w.closeSegment(); err != nil {
		return err
	}
	return w.writePlaylist()
}

func (w *tsFrameWriter) writePlaylist() error {
	if len(w.segments) == 0 {
		return nil
	}

	dir := w.segmentDir
	playlistPath := filepath.Join(dir, "index.m3u8")

	var maxDur float64
	for _, seg := range w.segments {
		if seg.duration > maxDur {
			maxDur = seg.duration
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(maxDur)+1))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	for _, seg := range w.segments {
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", seg.duration))
		b.WriteString(seg.filename + "\n")
	}

	b.WriteString("#EXT-X-ENDLIST\n")

	return os.WriteFile(playlistPath, []byte(b.String()), 0644)
}
