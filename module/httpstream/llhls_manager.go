package httpstream

import (
	"context"
	"log/slog"
	"math"
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
	initSegments map[string][]byte

	segmenter           *LLHLSSegmenter
	playlist            *LLHLSPlaylist
	streamKey           string
	basePath            string
	streamInstanceID    uint64
	publisherGeneration uint64
	publisherID         string
	container           string
	segmentCount        int
	partDuration        float64
	segmentDuration     float64
	initialPlaylistWait time.Duration
	blockingReloadHold  time.Duration

	done     chan struct{}
	stopOnce sync.Once

	beforeBlockingWait func()
}

// NewLLHLSManager creates a new LL-HLS manager.
func NewLLHLSManager(streamKey, basePath string, partDuration, segmentDuration float64, segmentCount int, container string) *LLHLSManager {
	m := &LLHLSManager{
		streamKey:           streamKey,
		basePath:            basePath,
		container:           container,
		segmentCount:        segmentCount,
		partDuration:        partDuration,
		segmentDuration:     segmentDuration,
		initialPlaylistWait: llhlsInitialPlaylistWaitDuration(segmentDuration, partDuration),
		blockingReloadHold:  llhlsBlockingReloadHoldDuration(segmentCount, partDuration),
		done:                make(chan struct{}),
	}
	m.cond = sync.NewCond(&m.mu)

	m.playlist = NewLLHLSPlaylist(partDuration, segmentDuration, basePath, container)

	m.segmenter = NewLLHLSSegmenter(partDuration, segmentDuration, container, LLHLSSegmenterCallbacks{
		OnInit: func(data []byte) {
			m.mu.Lock()
			m.initSegment = copyBytes(data)
			m.playlist.initVersion = initSegmentVersion(data)
			if m.initSegments == nil {
				m.initSegments = make(map[string][]byte)
			}
			if m.playlist.initVersion != "" {
				m.initSegments[m.playlist.initVersion] = copyBytes(data)
			}
			m.pruneInitSegmentsLocked()
			m.mu.Unlock()
		},
		OnPart: func(part *LLHLSPart) {
			m.mu.Lock()
			part.initVersion = m.playlist.initVersion
			m.currentParts = append(m.currentParts, part)
			m.cond.Broadcast()
			m.mu.Unlock()
		},
		OnSegment: func(seg *LLHLSSegment) {
			m.mu.Lock()
			seg.initVersion = m.playlist.initVersion
			for _, part := range seg.Parts {
				if part.initVersion == "" {
					part.initVersion = seg.initVersion
				}
			}
			m.segments = append(m.segments, seg)
			m.currentParts = nil
			m.currentMSN = seg.MSN + 1
			if len(m.segments) > m.segmentCount {
				excess := len(m.segments) - m.segmentCount
				m.segments = m.segments[excess:]
			}
			m.pruneInitSegmentsLocked()
			m.cond.Broadcast()
			m.mu.Unlock()
		},
		OnDiscontinuity: func() {
			m.mu.Lock()
			m.currentParts = nil
			m.currentMSN++
			m.pruneInitSegmentsLocked()
			m.cond.Broadcast()
			m.mu.Unlock()
		},
	})

	return m
}

func llhlsBlockingReloadHoldDuration(segmentCount int, partDuration float64) time.Duration {
	const (
		minimumHold = 10 * time.Second
		maximumHold = 30 * time.Second
	)
	holdSeconds := float64(segmentCount) * partDuration * 30
	if math.IsNaN(holdSeconds) || math.IsInf(holdSeconds, 0) || holdSeconds >= maximumHold.Seconds() {
		return maximumHold
	}
	if holdSeconds <= minimumHold.Seconds() {
		return minimumHold
	}
	return time.Duration(math.Ceil(holdSeconds*float64(time.Second/time.Millisecond))) * time.Millisecond
}

func llhlsInitialPlaylistWaitDuration(segmentDuration, partDuration float64) time.Duration {
	const (
		minimumWait = 10 * time.Second
		maximumWait = 30 * time.Second
	)
	if math.IsNaN(segmentDuration) || math.IsNaN(partDuration) ||
		math.IsInf(segmentDuration, 0) || math.IsInf(partDuration, 0) {
		return maximumWait
	}

	targetSeconds := segmentDuration + partDuration
	if math.IsNaN(targetSeconds) || math.IsInf(targetSeconds, 0) ||
		targetSeconds >= maximumWait.Seconds() {
		return maximumWait
	}
	targetMilliseconds := math.Ceil(targetSeconds * float64(time.Second/time.Millisecond))
	wait := time.Duration(targetMilliseconds) * time.Millisecond
	if wait < minimumWait {
		return minimumWait
	}
	return wait
}

// Run starts the segmenter loop. Blocks until stream ends or Stop() is called.
func (m *LLHLSManager) Run(stream *core.Stream) {
	slog.Info("manager started", "module", "llhls", "stream", m.streamKey)
	defer slog.Info("manager stopped", "module", "llhls", "stream", m.streamKey)
	m.segmenter.Run(stream, m.streamInstanceID, m.publisherGeneration, m.publisherID)
}

// Stop signals shutdown.
func (m *LLHLSManager) Stop() {
	m.stopOnce.Do(func() {
		close(m.done)
		m.segmenter.Stop()
		m.mu.Lock()
		m.cond.Broadcast()
		m.mu.Unlock()
	})
}

// GeneratePlaylist returns the m3u8 playlist. If targetMSN >= 0, blocks until
// the requested MSN/part is available (blocking playlist reload).
func (m *LLHLSManager) GeneratePlaylist(ctx context.Context, targetMSN, targetPart int, skip bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if targetMSN >= 0 {
		holdCtx, cancelHold := context.WithTimeout(ctx, m.blockingReloadHold)
		defer cancelHold()
		cancelCh := make(chan struct{})
		go func() {
			select {
			case <-holdCtx.Done():
			case <-m.done:
			case <-cancelCh:
				return
			}
			m.mu.Lock()
			m.cond.Broadcast()
			m.mu.Unlock()
		}()
		defer close(cancelCh)

		for !m.hasContent(targetMSN, targetPart) {
			if holdCtx.Err() != nil || managerStopped(m.done) {
				break
			}
			if m.beforeBlockingWait != nil {
				m.beforeBlockingWait()
			}
			m.cond.Wait()
		}
	}

	return m.playlist.Generate(m.segments, m.currentParts, m.currentMSN, skip, targetMSN >= 0), nil
}

func managerStopped(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// WaitForCompletedSegment blocks until the playlist contains one complete
// segment. The bundled Hls.js treats a part-only initial manifest as an empty
// level, even though it consumes low-latency parts after startup.
func (m *LLHLSManager) WaitForCompletedSegment(ctx context.Context, maxWait time.Duration) bool {
	waitCtx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasCompletedSegment() {
		return true
	}

	wakeDone := make(chan struct{})
	go func() {
		select {
		case <-waitCtx.Done():
		case <-m.done:
		case <-wakeDone:
			return
		}
		m.mu.Lock()
		m.cond.Broadcast()
		m.mu.Unlock()
	}()
	defer close(wakeDone)

	for !m.hasCompletedSegment() && waitCtx.Err() == nil {
		select {
		case <-m.done:
			return false
		default:
		}
		m.cond.Wait()
	}
	return m.hasCompletedSegment()
}

func (m *LLHLSManager) hasCompletedSegment() bool {
	return len(m.segments) > 0
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
	return copyBytes(m.initSegment), true
}

// GetInitSegmentVersion returns immutable fMP4 initialization bytes for a
// retained content-derived version. An empty version selects the current init.
func (m *LLHLSManager) GetInitSegmentVersion(version string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if version == "" {
		if m.initSegment == nil {
			return nil, false
		}
		return copyBytes(m.initSegment), true
	}
	data, ok := m.initSegments[version]
	if !ok {
		return nil, false
	}
	return copyBytes(data), true
}

func (m *LLHLSManager) pruneInitSegmentsLocked() {
	if len(m.initSegments) == 0 {
		return
	}
	retained := make(map[string]struct{}, len(m.segments)+1)
	if m.playlist.initVersion != "" {
		retained[m.playlist.initVersion] = struct{}{}
	}
	for _, seg := range m.segments {
		if seg.initVersion != "" {
			retained[seg.initVersion] = struct{}{}
		}
	}
	for _, part := range m.currentParts {
		if part.initVersion != "" {
			retained[part.initVersion] = struct{}{}
		}
	}
	for version := range m.initSegments {
		if _, ok := retained[version]; !ok {
			delete(m.initSegments, version)
		}
	}
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
