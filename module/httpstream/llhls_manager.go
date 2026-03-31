package httpstream

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
)

// LLHLSManager orchestrates LL-HLS segment production and playlist serving.
type LLHLSManager struct {
	mu   sync.Mutex
	cond *sync.Cond

	segments     []*LLHLSSegment
	currentParts []*LLHLSPart
	currentMSN   int
	initSegment  []byte

	segmenter    *LLHLSSegmenter
	playlist     *LLHLSPlaylist
	streamKey    string
	basePath     string
	container    string
	segmentCount int
	partDuration float64

	done chan struct{}
}

// NewLLHLSManager creates a new LL-HLS manager.
func NewLLHLSManager(streamKey, basePath string, partDuration float64, segmentCount int, container string) *LLHLSManager {
	m := &LLHLSManager{
		streamKey:    streamKey,
		basePath:     basePath,
		container:    container,
		segmentCount: segmentCount,
		partDuration: partDuration,
		done:         make(chan struct{}),
	}
	m.cond = sync.NewCond(&m.mu)

	m.playlist = NewLLHLSPlaylist(partDuration, basePath, container)

	m.segmenter = NewLLHLSSegmenter(partDuration, container, LLHLSSegmenterCallbacks{
		OnInit: func(data []byte) {
			m.mu.Lock()
			m.initSegment = data
			m.mu.Unlock()
		},
		OnPart: func(part *LLHLSPart) {
			m.mu.Lock()
			m.currentParts = append(m.currentParts, part)
			m.cond.Broadcast()
			m.mu.Unlock()
		},
		OnSegment: func(seg *LLHLSSegment) {
			m.mu.Lock()
			m.segments = append(m.segments, seg)
			m.currentParts = nil
			m.currentMSN = seg.MSN + 1
			if len(m.segments) > m.segmentCount {
				excess := len(m.segments) - m.segmentCount
				m.segments = m.segments[excess:]
			}
			m.cond.Broadcast()
			m.mu.Unlock()
		},
	})

	return m
}

// Run starts the segmenter loop. Blocks until stream ends or Stop() is called.
func (m *LLHLSManager) Run(stream *core.Stream) {
	slog.Info("manager started", "module", "llhls", "stream", m.streamKey)
	defer slog.Info("manager stopped", "module", "llhls", "stream", m.streamKey)
	m.segmenter.Run(stream)
}

// Stop signals shutdown.
func (m *LLHLSManager) Stop() {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
	m.segmenter.Stop()
	m.cond.Broadcast()
}

// GeneratePlaylist returns the m3u8 playlist. If targetMSN >= 0, blocks until
// the requested MSN/part is available (blocking playlist reload).
func (m *LLHLSManager) GeneratePlaylist(ctx context.Context, targetMSN, targetPart int, skip bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if targetMSN >= 0 {
		cancelCh := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
			case <-m.done:
			case <-cancelCh:
				return
			}
			m.cond.Broadcast()
		}()
		defer close(cancelCh)

		// Server-side max hold timeout
		holdDuration := min(max(
			time.Duration(float64(m.segmentCount)*m.partDuration*30*float64(time.Second)),
			10*time.Second,
		), 30*time.Second)
		deadline := time.Now().Add(holdDuration)

		for !m.hasContent(targetMSN, targetPart) {
			if ctx.Err() != nil {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			m.cond.Wait()
		}
	}

	return m.playlist.Generate(m.segments, m.currentParts, m.currentMSN, skip), nil
}

func (m *LLHLSManager) hasContent(targetMSN, targetPart int) bool {
	for _, seg := range m.segments {
		if seg.MSN > targetMSN {
			return true
		}
		if seg.MSN == targetMSN {
			return true
		}
	}
	if m.currentMSN > targetMSN {
		return true
	}
	if m.currentMSN == targetMSN && len(m.currentParts) > targetPart {
		return true
	}
	return false
}

// GetInitSegment returns the fMP4 init segment.
func (m *LLHLSManager) GetInitSegment() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initSegment == nil {
		return nil, false
	}
	return m.initSegment, true
}

// GetPartialSegment returns a partial segment by MSN and part index.
func (m *LLHLSManager) GetPartialSegment(msn, partIdx int) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, seg := range m.segments {
		if seg.MSN == msn {
			if partIdx < len(seg.Parts) {
				return seg.Parts[partIdx].Data, true
			}
			return nil, false
		}
	}
	if m.currentMSN == msn && partIdx < len(m.currentParts) {
		return m.currentParts[partIdx].Data, true
	}
	return nil, false
}

// GetFullSegment returns a full segment by MSN (concatenated partial data).
func (m *LLHLSManager) GetFullSegment(msn int) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, seg := range m.segments {
		if seg.MSN == msn {
			total := 0
			for _, p := range seg.Parts {
				total += len(p.Data)
			}
			data := make([]byte, 0, total)
			for _, p := range seg.Parts {
				data = append(data, p.Data...)
			}
			return data, true
		}
	}
	return nil, false
}

// SegmentCount returns the number of completed segments.
func (m *LLHLSManager) SegmentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.segments)
}
