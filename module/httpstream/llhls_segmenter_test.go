package httpstream

import (
	"sync"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

func TestLLHLSSegmenter_PartDurationSplit(t *testing.T) {
	var parts []*LLHLSPart
	var mu sync.Mutex

	seg := NewLLHLSSegmenter(0.2, "ts", LLHLSSegmenterCallbacks{
		OnPart: func(p *LLHLSPart) {
			mu.Lock()
			parts = append(parts, p)
			mu.Unlock()
		},
		OnSegment: func(s *LLHLSSegment) {},
	})

	seg.tsMuxer = ts.NewMuxer(avframe.CodecH264, avframe.CodecAAC, nil, nil)
	seg.hasVideo = true

	// 30 frames, 33ms apart = ~1 second. partDuration=0.2s => ~5 parts
	frames := makeTestFrames(30, 33)
	frames[0].FrameType = avframe.FrameTypeKeyframe

	for _, f := range frames {
		seg.processFrame(f)
	}
	seg.flushCurrentPart(frames[len(frames)-1].DTS + 33)

	mu.Lock()
	defer mu.Unlock()

	if len(parts) < 4 {
		t.Errorf("expected at least 4 parts for 1s at 0.2s part duration, got %d", len(parts))
	}
	if len(parts) > 0 && !parts[0].Independent {
		t.Error("first part should be independent (starts with keyframe)")
	}
	for i := 1; i < len(parts); i++ {
		if parts[i].Independent {
			t.Errorf("part %d should not be independent", i)
		}
	}
}

func TestLLHLSSegmenter_KeyframeSplitsSegment(t *testing.T) {
	var segments []*LLHLSSegment
	var mu sync.Mutex

	seg := NewLLHLSSegmenter(0.2, "ts", LLHLSSegmenterCallbacks{
		OnPart: func(p *LLHLSPart) {},
		OnSegment: func(s *LLHLSSegment) {
			mu.Lock()
			segments = append(segments, s)
			mu.Unlock()
		},
	})

	seg.tsMuxer = ts.NewMuxer(avframe.CodecH264, avframe.CodecAAC, nil, nil)
	seg.hasVideo = true

	// First GOP: keyframe at DTS=0, 30 frames
	frames1 := makeTestFrames(30, 33)
	frames1[0].FrameType = avframe.FrameTypeKeyframe

	// Second GOP: keyframe at DTS=990, 30 frames
	frames2 := makeTestFrames(30, 33)
	frames2[0].FrameType = avframe.FrameTypeKeyframe
	for i := range frames2 {
		frames2[i].DTS += 990
		frames2[i].PTS += 990
	}

	for _, f := range frames1 {
		seg.processFrame(f)
	}
	for _, f := range frames2 {
		seg.processFrame(f)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(segments) < 1 {
		t.Fatalf("expected at least 1 completed segment, got %d", len(segments))
	}
	if segments[0].MSN != 0 {
		t.Errorf("first segment MSN = %d, want 0", segments[0].MSN)
	}
}

func TestLLHLSSegmenter_AudioOnlyTimeBasedSplit(t *testing.T) {
	var parts []*LLHLSPart
	var mu sync.Mutex

	seg := NewLLHLSSegmenter(0.2, "ts", LLHLSSegmenterCallbacks{
		OnPart: func(p *LLHLSPart) {
			mu.Lock()
			parts = append(parts, p)
			mu.Unlock()
		},
		OnSegment: func(s *LLHLSSegment) {},
	})

	seg.tsMuxer = ts.NewMuxer(avframe.CodecH264, avframe.CodecAAC, nil, nil)

	// Audio-only: 50 frames, 23ms apart = ~1.15s
	frames := make([]*avframe.AVFrame, 50)
	for i := range frames {
		frames[i] = &avframe.AVFrame{
			MediaType: avframe.MediaTypeAudio,
			Codec:     avframe.CodecAAC,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       int64(i * 23),
			PTS:       int64(i * 23),
			Payload:   []byte{0xFF, 0xF1, 0x50, 0x80, 0x02, 0x00, 0xFC, 0xDE, 0xAD},
		}
	}

	for _, f := range frames {
		seg.processFrame(f)
	}
	seg.flushCurrentPart(frames[len(frames)-1].DTS + 23)

	mu.Lock()
	defer mu.Unlock()

	if len(parts) < 4 {
		t.Errorf("expected at least 4 parts for audio-only stream, got %d", len(parts))
	}
}

func TestLLHLSSegmenter_SkipsFramesBeforeFirstKeyframe(t *testing.T) {
	var parts []*LLHLSPart
	var mu sync.Mutex

	seg := NewLLHLSSegmenter(0.2, "ts", LLHLSSegmenterCallbacks{
		OnPart: func(p *LLHLSPart) {
			mu.Lock()
			parts = append(parts, p)
			mu.Unlock()
		},
		OnSegment: func(s *LLHLSSegment) {},
	})

	seg.tsMuxer = ts.NewMuxer(avframe.CodecH264, avframe.CodecAAC, nil, nil)
	seg.hasVideo = true

	// 10 non-keyframe frames (should be skipped)
	for i := range 10 {
		seg.processFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       int64(i * 33),
			PTS:       int64(i * 33),
			Payload:   []byte{0x41, 0x00, 0x00, 0x01},
		})
	}

	mu.Lock()
	skippedParts := len(parts)
	mu.Unlock()
	if skippedParts != 0 {
		t.Errorf("expected 0 parts before first keyframe, got %d", skippedParts)
	}

	// Now send keyframe + more frames
	frames := makeTestFrames(15, 33)
	frames[0].FrameType = avframe.FrameTypeKeyframe
	for i := range frames {
		frames[i].DTS += 330 // offset past the skipped frames
		frames[i].PTS += 330
	}
	for _, f := range frames {
		seg.processFrame(f)
	}
	seg.flushCurrentPart(frames[len(frames)-1].DTS + 33)

	mu.Lock()
	defer mu.Unlock()
	if len(parts) == 0 {
		t.Error("expected parts after keyframe arrived")
	}
	if len(parts) > 0 && !parts[0].Independent {
		t.Error("first part should be independent (starts with keyframe)")
	}
}

// --- test helpers ---

func makeTestFrames(count int, intervalMs int64) []*avframe.AVFrame {
	frames := make([]*avframe.AVFrame, count)
	for i := range frames {
		frames[i] = &avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeInterframe,
			DTS:       int64(i) * intervalMs,
			PTS:       int64(i) * intervalMs,
			Payload:   []byte{0x65, 0x00, 0x00, 0x01},
		}
	}
	return frames
}
