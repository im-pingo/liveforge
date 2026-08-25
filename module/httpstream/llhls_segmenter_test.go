package httpstream

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
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

func TestLLHLSSegmenter_VideoSegmentsOnlySplitOnKeyframes(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecAAC)
	var initData []byte
	var segments []*LLHLSSegment
	seg := NewLLHLSSegmenter(0.2, "fmp4", LLHLSSegmenterCallbacks{
		OnInit: func(data []byte) {
			initData = append([]byte(nil), data...)
		},
		OnSegment: func(segment *LLHLSSegment) {
			segments = append(segments, segment)
		},
	})
	seg.initMuxer(stream)

	for _, frame := range []*avframe.AVFrame{
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			0, 0, []byte{0, 0, 0, 2, 0x65, 0x01},
		),
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			1000, 1000, []byte{0, 0, 0, 2, 0x41, 0x02},
		),
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			5000, 5000, []byte{0, 0, 0, 2, 0x41, 0x03},
		),
		// This audio frame crosses the old six-second timer. A video stream
		// must remain in the same segment until the next video keyframe.
		avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
			6010, 6010, []byte{0x21, 0x10},
		),
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			7000, 7000, []byte{0, 0, 0, 2, 0x41, 0x04},
		),
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			8300, 8300, []byte{0, 0, 0, 2, 0x65, 0x05},
		),
	} {
		seg.processFrame(frame)
	}

	if len(segments) != 1 {
		t.Fatalf("completed segments = %d, want one keyframe-bounded segment", len(segments))
	}
	if len(segments[0].Parts) == 0 || !segments[0].Parts[0].Independent {
		t.Fatal("video segment does not advertise an independent first part")
	}

	demuxer, err := fmp4.NewDemuxer(initData)
	if err != nil {
		t.Fatalf("parse init segment: %v", err)
	}
	frames, err := demuxer.Parse(segments[0].Parts[0].Data)
	if err != nil {
		t.Fatalf("parse first part: %v", err)
	}
	for _, frame := range frames {
		if frame.MediaType.IsVideo() {
			if !frame.FrameType.IsKeyframe() {
				t.Fatal("independent first part starts with an interframe")
			}
			return
		}
	}
	t.Fatal("independent first part contains no video frame")
}

func TestLLHLSSegmenter_CachedGOPContinuesWithLiveInterframes(t *testing.T) {
	stream := newMuxerWorkerStream(t, 0)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0, 0, 0, 2, 0x41, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		66, 66, []byte{0, 0, 0, 2, 0x41, 0x02},
	))

	var mu sync.Mutex
	var initData []byte
	var segments []*LLHLSSegment
	cachedPartReady := make(chan struct{})
	var cachedPartOnce sync.Once
	seg := NewLLHLSSegmenter(0.2, "fmp4", LLHLSSegmenterCallbacks{
		OnInit: func(data []byte) {
			initData = append([]byte(nil), data...)
		},
		OnPart: func(part *LLHLSPart) {
			cachedPartOnce.Do(func() { close(cachedPartReady) })
		},
		OnSegment: func(segment *LLHLSSegment) {
			mu.Lock()
			segments = append(segments, segment)
			mu.Unlock()
		},
	})

	done := make(chan struct{})
	go func() {
		seg.Run(stream)
		close(done)
	}()

	select {
	case <-cachedPartReady:
	case <-time.After(2 * time.Second):
		t.Fatal("cached GOP part was not published")
	}

	// These interframes arrive after the atomic cache snapshot. They are still
	// part of the cached keyframe's GOP and must remain in segment 0.
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		99, 99, []byte{0, 0, 0, 2, 0x41, 0x03},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		400, 400, []byte{0, 0, 0, 2, 0x65, 0x04},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		433, 433, []byte{0, 0, 0, 2, 0x41, 0x05},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		800, 800, []byte{0, 0, 0, 2, 0x65, 0x06},
	))
	stream.RingBuffer().Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("segmenter did not stop after source closed")
	}

	mu.Lock()
	var firstSegment *LLHLSSegment
	for _, segment := range segments {
		if segment.MSN == 0 {
			firstSegment = segment
			break
		}
	}
	mu.Unlock()
	if firstSegment == nil || len(firstSegment.Parts) < 2 {
		t.Fatal("segment 0 did not retain cached and live parts")
	}
	if !firstSegment.Parts[0].Independent {
		t.Fatal("segment 0 does not advertise an independent first part")
	}

	demuxer, err := fmp4.NewDemuxer(initData)
	if err != nil {
		t.Fatalf("parse init segment: %v", err)
	}
	var firstVideo *avframe.AVFrame
	foundSnapshotLiveFrame := false
	for _, part := range firstSegment.Parts {
		frames, err := demuxer.Parse(part.Data)
		if err != nil {
			t.Fatalf("parse segment 0 part %d: %v", part.Index, err)
		}
		for _, frame := range frames {
			if frame.MediaType.IsVideo() && firstVideo == nil {
				firstVideo = frame
			}
			if frame.MediaType.IsVideo() && frame.DTS == 99 {
				foundSnapshotLiveFrame = true
			}
		}
	}
	if firstVideo == nil || !firstVideo.FrameType.IsKeyframe() {
		t.Fatal("segment 0 does not actually start with a video keyframe")
	}
	if !foundSnapshotLiveFrame {
		t.Fatal("segment 0 dropped the live interframe immediately after the cache snapshot")
	}
}

func TestLLHLSFMP4SynthesizesWHIPOpusTrackConfiguration(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecOpus)
	var initData []byte
	seg := NewLLHLSSegmenter(0.2, "fmp4", LLHLSSegmenterCallbacks{
		OnInit: func(data []byte) {
			initData = append([]byte(nil), data...)
		},
	})
	seg.initMuxer(stream)

	if !bytes.Contains(initData, []byte("Opus")) || !bytes.Contains(initData, []byte("dOps")) {
		t.Fatal("LL-HLS FMP4 init segment is missing Opus/dOps boxes")
	}
}

func TestLLHLSFMP4DerivesH265TrackDimensions(t *testing.T) {
	stream := newH265MuxerWorkerStream(t)
	var initData []byte
	seg := NewLLHLSSegmenter(0.2, "fmp4", LLHLSSegmenterCallbacks{
		OnInit: func(data []byte) {
			initData = append([]byte(nil), data...)
		},
	})
	seg.initMuxer(stream)

	assertH265SampleEntryDimensions(t, initData, 640, 480)
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
