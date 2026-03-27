package httpstream

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLLHLSManager_HasContent(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 4, "fmp4")

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

func TestLLHLSManager_BlockingPlaylist(t *testing.T) {
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 4, "fmp4")

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
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 4, "fmp4")

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
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 3, "fmp4")

	for i := 0; i < 5; i++ {
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
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 4, "fmp4")

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
	m := NewLLHLSManager("test/stream", "/test/stream", 0.2, 4, "fmp4")

	result, err := m.GeneratePlaylist(context.Background(), -1, -1, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty playlist")
	}
}
