package httpstream

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
)

func TestLLHLSManager_HasContent(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 1.0, 4, "fmp4")

	m.mu.Lock()
	if m.hasContent(0, 0) {
		t.Error("should have no content initially")
	}
	m.mu.Unlock()

	m.mu.Lock()
	m.currentParts = append(m.currentParts, &LLHLSPart{Index: 0, Duration: 0.2, Data: []byte{1}})
	if !m.hasContent(0, 0) {
		t.Error("should have content after adding partial 0.0")
	}
	if m.hasContent(0, 1) {
		t.Error("should not have content for partial 0.1")
	}
	m.mu.Unlock()

	m.mu.Lock()
	m.segments = append(m.segments, &LLHLSSegment{
		MSN: 0, Duration: 6.0, Parts: m.currentParts,
	})
	m.currentParts = nil
	m.currentMSN = 1
	if !m.hasContent(0, 0) {
		t.Error("should have content for completed segment 0")
	}
	if m.hasContent(1, 0) {
		t.Error("should not have content for segment 1 yet")
	}
	m.mu.Unlock()
}

func TestLLHLSInitialPlaylistWaitDuration(t *testing.T) {
	tests := []struct {
		name            string
		segmentDuration float64
		partDuration    float64
		want            time.Duration
	}{
		{name: "preserves ten second minimum", segmentDuration: 1, partDuration: 0.2, want: 10 * time.Second},
		{name: "extends to segment plus one part", segmentDuration: 15, partDuration: 0.2, want: 15*time.Second + 200*time.Millisecond},
		{name: "rounds up to media timestamp unit", segmentDuration: 10, partDuration: 0.0001, want: 10*time.Second + time.Millisecond},
		{name: "caps finite overflow", segmentDuration: math.MaxFloat64, partDuration: math.MaxFloat64, want: 30 * time.Second},
		{name: "caps positive infinity", segmentDuration: math.Inf(1), partDuration: 0.2, want: 30 * time.Second},
		{name: "caps not a number", segmentDuration: math.NaN(), partDuration: 0.2, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := llhlsInitialPlaylistWaitDuration(tt.segmentDuration, tt.partDuration); got != tt.want {
				t.Fatalf("initial LL-HLS playlist wait = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLLHLSManagerAudioOnlyLiveSegments(t *testing.T) {
	for _, container := range []string{"ts", "fmp4"} {
		t.Run(container, func(t *testing.T) {
			stream := newAudioOnlyAACStream(t, "live/llhls-audio-only-"+container)
			cfg, err := configruntime.ParseDocument([]byte("http_stream:\n  llhls:\n    enabled: true\n    part_duration: 0.05\n    segment_duration: 0.2\n    segment_count: 4\n    container: " + container + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			server := core.NewServer(cfg)
			module := NewModule()
			module.server = server
			mgr := module.getOrCreateLLHLS(stream.Key(), stream)
			t.Cleanup(func() {
				mgr.Stop()
				stream.RingBuffer().Close()
			})

			time.Sleep(20 * time.Millisecond)
			payloads := writeLiveAACFrames(stream, 22, 20)
			waitForSegmentCount(t, mgr.SegmentCount, 2)

			firstData, ok := mgr.GetFullSegment(0)
			if !ok {
				t.Fatal("first audio-only LL-HLS full segment is unavailable while source is live")
			}
			secondData, ok := mgr.GetFullSegment(1)
			if !ok {
				t.Fatal("second audio-only LL-HLS full segment is unavailable while source is live")
			}

			var firstFrames, secondFrames []*avframe.AVFrame
			var firstBoundaryFrames, secondBoundaryFrames []*avframe.AVFrame
			switch container {
			case "ts":
				firstFrames = demuxTSAudioFrames(firstData)
				secondFrames = demuxTSAudioFrames(secondData)
				firstBoundaryFrames = firstFrames
				secondBoundaryFrames = secondFrames
			case "fmp4":
				initData, found := mgr.GetInitSegment()
				if !found {
					t.Fatal("audio-only LL-HLS fMP4 init segment is unavailable while source is live")
				}
				demuxer, demuxErr := fmp4.NewDemuxer(initData)
				if demuxErr != nil {
					t.Fatal(demuxErr)
				}
				firstFrames, demuxErr = demuxer.Parse(firstData)
				if demuxErr != nil {
					t.Fatalf("demux first audio-only LL-HLS fMP4 segment: %v", demuxErr)
				}
				secondFrames, demuxErr = demuxer.Parse(secondData)
				if demuxErr != nil {
					t.Fatalf("demux second audio-only LL-HLS fMP4 segment: %v", demuxErr)
				}
				firstBoundaryFrames = demuxLLHLSFMP4Parts(t, demuxer, mgr, 0)
				secondBoundaryFrames = demuxLLHLSFMP4Parts(t, demuxer, mgr, 1)
			}
			if len(firstFrames) == 0 || len(secondFrames) == 0 {
				t.Fatalf("audio-only LL-HLS %s demuxed frames = %d/%d, want audio in both full segments", container, len(firstFrames), len(secondFrames))
			}
			assertBoundaryPayloadStartsNextSegmentOnce(t, firstBoundaryFrames, secondBoundaryFrames, payloads[10])
		})
	}
}

func demuxLLHLSFMP4Parts(t *testing.T, demuxer *fmp4.Demuxer, mgr *LLHLSManager, msn int) []*avframe.AVFrame {
	t.Helper()
	mgr.mu.Lock()
	var parts []*LLHLSPart
	for _, segment := range mgr.segments {
		if segment.MSN == msn {
			parts = append(parts, segment.Parts...)
			break
		}
	}
	mgr.mu.Unlock()
	var frames []*avframe.AVFrame
	for _, part := range parts {
		parsed, err := demuxer.Parse(part.Data)
		if err != nil {
			t.Fatalf("demux LL-HLS fMP4 segment %d part %d: %v", msn, part.Index, err)
		}
		frames = append(frames, parsed...)
	}
	return frames
}

func TestLLHLSManager_BlockingPlaylist(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 1.0, 4, "fmp4")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var result string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, _ = m.GeneratePlaylist(ctx, 0, 0, false)
	}()

	time.Sleep(50 * time.Millisecond)

	m.mu.Lock()
	m.currentParts = append(m.currentParts, &LLHLSPart{Index: 0, Duration: 0.2, Data: []byte{1}})
	m.cond.Broadcast()
	m.mu.Unlock()

	wg.Wait()

	if result == "" {
		t.Error("expected non-empty playlist")
	}
}

func TestLLHLSManager_BlockingPlaylistTimeout(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 1.0, 4, "fmp4")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, _ := m.GeneratePlaylist(ctx, 99, 0, false)
	elapsed := time.Since(start)

	if result == "" {
		t.Error("expected non-empty playlist even on timeout")
	}
	if elapsed > 2*time.Second {
		t.Errorf("should have timed out quickly, took %v", elapsed)
	}
}

func TestLLHLSManager_SlidingWindow(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 1.0, 3, "fmp4")

	for i := range 5 {
		m.mu.Lock()
		m.segments = append(m.segments, &LLHLSSegment{
			MSN: i, Duration: 6.0,
			Parts: []*LLHLSPart{{Index: 0, Duration: 0.2, Data: []byte{byte(i)}}},
		})
		m.currentMSN = i + 1
		if len(m.segments) > m.segmentCount {
			excess := len(m.segments) - m.segmentCount
			m.segments = m.segments[excess:]
		}
		m.mu.Unlock()
	}

	if m.SegmentCount() != 3 {
		t.Errorf("segment count = %d, want 3", m.SegmentCount())
	}

	m.mu.Lock()
	if m.segments[0].MSN != 2 {
		t.Errorf("oldest segment MSN = %d, want 2", m.segments[0].MSN)
	}
	m.mu.Unlock()

	_, found := m.GetFullSegment(0)
	if found {
		t.Error("MSN=0 should have been evicted")
	}
}

func TestLLHLSManager_GetPartialSegment(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 1.0, 4, "fmp4")

	m.mu.Lock()
	m.segments = append(m.segments, &LLHLSSegment{
		MSN: 0, Duration: 0.4,
		Parts: []*LLHLSPart{
			{Index: 0, Duration: 0.2, Data: []byte{0xAA}},
			{Index: 1, Duration: 0.2, Data: []byte{0xBB}},
		},
	})
	m.currentMSN = 1
	m.currentParts = []*LLHLSPart{
		{Index: 0, Duration: 0.2, Data: []byte{0xCC}},
	}
	m.mu.Unlock()

	data, ok := m.GetPartialSegment(0, 1)
	if !ok || len(data) != 1 || data[0] != 0xBB {
		t.Errorf("GetPartialSegment(0,1) = %v, %v", data, ok)
	}

	data, ok = m.GetPartialSegment(1, 0)
	if !ok || len(data) != 1 || data[0] != 0xCC {
		t.Errorf("GetPartialSegment(1,0) = %v, %v", data, ok)
	}

	_, ok = m.GetPartialSegment(5, 0)
	if ok {
		t.Error("expected not found for non-existent MSN")
	}
}

func TestLLHLSManager_NonBlockingPlaylist(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 1.0, 4, "fmp4")

	result, err := m.GeneratePlaylist(context.Background(), -1, -1, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty playlist")
	}
}
