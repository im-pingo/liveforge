package source

import (
	"io"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// drainAll reads all frames from a Source and returns them.
func drainAll(t *testing.T, src Source) []*avframe.AVFrame {
	t.Helper()
	var frames []*avframe.AVFrame
	for {
		f, err := src.NextFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextFrame unexpected error: %v", err)
		}
		frames = append(frames, f)
	}
	return frames
}

func TestFLVSourceMediaInfo(t *testing.T) {
	src := NewFLVSource(0)
	info := src.MediaInfo()

	if info == nil {
		t.Fatal("MediaInfo returned nil")
	}
	if info.VideoCodec != avframe.CodecH264 {
		t.Errorf("expected video codec H264, got %v", info.VideoCodec)
	}
	if info.AudioCodec != avframe.CodecAAC {
		t.Errorf("expected audio codec AAC, got %v", info.AudioCodec)
	}
	if !info.HasVideo() {
		t.Error("HasVideo should be true")
	}
	if !info.HasAudio() {
		t.Error("HasAudio should be true")
	}
}

func TestFLVSourceFrameCount(t *testing.T) {
	src := NewFLVSource(0) // no duration limit, play once
	frames := drainAll(t, src)

	// The test source.flv has 62 video + 89 audio = 151 total frames.
	if len(frames) < 100 || len(frames) > 200 {
		t.Fatalf("expected 100-200 frames, got %d", len(frames))
	}

	var videoCount, audioCount int
	var videoSeqHdr, audioSeqHdr int
	for _, f := range frames {
		switch f.MediaType {
		case avframe.MediaTypeVideo:
			videoCount++
			if f.FrameType == avframe.FrameTypeSequenceHeader {
				videoSeqHdr++
			}
		case avframe.MediaTypeAudio:
			audioCount++
			if f.FrameType == avframe.FrameTypeSequenceHeader {
				audioSeqHdr++
			}
		}
	}

	// Golden reference: ~62 video, ~89 audio
	if videoCount < 50 || videoCount > 80 {
		t.Errorf("expected 50-80 video frames, got %d", videoCount)
	}
	if audioCount < 70 || audioCount > 110 {
		t.Errorf("expected 70-110 audio frames, got %d", audioCount)
	}
	// Exactly 1 video seq header and 1 audio seq header for single pass
	if videoSeqHdr != 1 {
		t.Errorf("expected 1 video sequence header, got %d", videoSeqHdr)
	}
	if audioSeqHdr != 1 {
		t.Errorf("expected 1 audio sequence header, got %d", audioSeqHdr)
	}
}

func TestFLVSourceDTSMonotonic(t *testing.T) {
	src := NewFLVSource(0)
	frames := drainAll(t, src)

	// DTS is monotonic within each media type (audio/video are interleaved
	// so global DTS across both tracks is not strictly ordered).
	var lastVideoDTS, lastAudioDTS int64 = -1, -1
	for i, f := range frames {
		switch f.MediaType {
		case avframe.MediaTypeVideo:
			if f.DTS < lastVideoDTS {
				t.Fatalf("video DTS not monotonic at frame %d: %d < %d", i, f.DTS, lastVideoDTS)
			}
			lastVideoDTS = f.DTS
		case avframe.MediaTypeAudio:
			if f.DTS < lastAudioDTS {
				t.Fatalf("audio DTS not monotonic at frame %d: %d < %d", i, f.DTS, lastAudioDTS)
			}
			lastAudioDTS = f.DTS
		}
	}
}

func TestFLVSourceDuration(t *testing.T) {
	// Request only 1 second of media
	src := NewFLVSource(1 * time.Second)
	frames := drainAll(t, src)

	if len(frames) == 0 {
		t.Fatal("expected at least some frames for 1s duration")
	}

	// All frames should be within the duration window
	for _, f := range frames {
		if f.DTS > 1000 {
			t.Errorf("frame DTS %d exceeds 1s duration limit", f.DTS)
		}
	}

	// Fewer frames than a full pass (151 total)
	if len(frames) >= 151 {
		t.Errorf("expected fewer frames than full pass (151), got %d", len(frames))
	}
}

func TestFLVSourceEOFSticky(t *testing.T) {
	src := NewFLVSource(0)
	_ = drainAll(t, src)

	// After EOF, further calls should keep returning EOF
	for i := 0; i < 3; i++ {
		_, err := src.NextFrame()
		if err != io.EOF {
			t.Fatalf("call %d after EOF: expected io.EOF, got %v", i, err)
		}
	}
}

func TestFLVSourceReset(t *testing.T) {
	src := NewFLVSource(0)
	first := drainAll(t, src)

	src.Reset()
	second := drainAll(t, src)

	if len(first) != len(second) {
		t.Fatalf("reset should produce same frame count: first=%d, second=%d", len(first), len(second))
	}
}

func TestFLVSourceLoopFrameCount(t *testing.T) {
	src := NewFLVSourceLoop(3)
	frames := drainAll(t, src)

	// 3 loops of ~151 frames each, plus extra seq headers at loop boundaries.
	// Each loop re-emits 2 seq headers (video + audio) at the boundary.
	// First loop: 151 frames (including original seq headers)
	// Loops 2-3: 151 frames each + 2 seq headers at each boundary = 2 * 2 = 4 extra
	// Total: 151*3 + 4 = 457
	// Allow a reasonable range.
	if len(frames) < 400 || len(frames) > 520 {
		t.Fatalf("expected 400-520 frames for 3 loops, got %d", len(frames))
	}
}

func TestFLVSourceLoopDTSMonotonic(t *testing.T) {
	src := NewFLVSourceLoop(3)
	frames := drainAll(t, src)

	if len(frames) == 0 {
		t.Fatal("no frames from looped source")
	}

	// DTS is monotonic within each media type.
	var lastVideoDTS, lastAudioDTS int64 = -1, -1
	for i, f := range frames {
		switch f.MediaType {
		case avframe.MediaTypeVideo:
			if f.DTS < lastVideoDTS {
				t.Fatalf("video DTS not monotonic at frame %d: DTS=%d < previous=%d", i, f.DTS, lastVideoDTS)
			}
			lastVideoDTS = f.DTS
		case avframe.MediaTypeAudio:
			if f.DTS < lastAudioDTS {
				t.Fatalf("audio DTS not monotonic at frame %d: DTS=%d < previous=%d", i, f.DTS, lastAudioDTS)
			}
			lastAudioDTS = f.DTS
		}
	}

	// After 3 loops of ~2s each, last DTS should be around 6000ms.
	// Use the max of both tracks.
	lastDTS := lastVideoDTS
	if lastAudioDTS > lastDTS {
		lastDTS = lastAudioDTS
	}
	if lastDTS < 5000 || lastDTS > 7000 {
		t.Errorf("expected last DTS ~6000ms for 3 loops, got %d", lastDTS)
	}
}

func TestFLVSourceLoopSeqHeaders(t *testing.T) {
	src := NewFLVSourceLoop(3)
	frames := drainAll(t, src)

	var videoSeqHdr, audioSeqHdr int
	for _, f := range frames {
		if f.FrameType != avframe.FrameTypeSequenceHeader {
			continue
		}
		if f.MediaType == avframe.MediaTypeVideo {
			videoSeqHdr++
		} else {
			audioSeqHdr++
		}
	}

	// 3 loops: original headers in first pass (1 each) + re-emitted at each
	// loop boundary (2 more of each) = 3 video seq headers and 3 audio seq headers.
	if videoSeqHdr < 3 {
		t.Errorf("expected >= 3 video sequence headers for 3 loops, got %d", videoSeqHdr)
	}
	if audioSeqHdr < 3 {
		t.Errorf("expected >= 3 audio sequence headers for 3 loops, got %d", audioSeqHdr)
	}
	// Total seq headers should be >= 6
	total := videoSeqHdr + audioSeqHdr
	if total < 6 {
		t.Errorf("expected >= 6 total sequence headers, got %d", total)
	}
}

func TestFLVSourceLoopPTSMonotonic(t *testing.T) {
	src := NewFLVSourceLoop(2)
	frames := drainAll(t, src)

	// PTS for video can differ from DTS (due to B-frames / composition time),
	// but should still be globally non-decreasing within each media type.
	var lastVideoPTS, lastAudioPTS int64 = -1, -1
	for i, f := range frames {
		switch f.MediaType {
		case avframe.MediaTypeVideo:
			if f.PTS < lastVideoPTS {
				t.Fatalf("video PTS not monotonic at frame %d: %d < %d", i, f.PTS, lastVideoPTS)
			}
			lastVideoPTS = f.PTS
		case avframe.MediaTypeAudio:
			if f.PTS < lastAudioPTS {
				t.Fatalf("audio PTS not monotonic at frame %d: %d < %d", i, f.PTS, lastAudioPTS)
			}
			lastAudioPTS = f.PTS
		}
	}
}
