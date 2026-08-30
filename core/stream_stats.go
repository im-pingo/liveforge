package core

import (
	"sync"
	"sync/atomic"
	"time"
)

// StreamStats collects protocol-agnostic statistics for a stream.
// All counters use atomic operations for lock-free updates on the hot path.
// Instantaneous bitrate and FPS use a sliding window (2 seconds) for accuracy.
type StreamStats struct {
	bytesIn     atomic.Int64
	videoFrames atomic.Int64
	audioFrames atomic.Int64

	// recordFrame updates the window counters atomically. windowMu only
	// serializes snapshot rotations, so frame ingestion never waits for a stats
	// reader.
	windowMu    sync.Mutex
	startTime   atomic.Int64
	windowBytes atomic.Int64
	windowVideo atomic.Int64
	windowStart atomic.Int64
	snapBytes   atomic.Int64
	snapVideo   atomic.Int64
	snapTime    atomic.Int64
}

const statsWindowDuration = 2 * time.Second

// initStats sets the start time. Called once when the stream begins publishing.
func (s *StreamStats) initStats() {
	now := time.Now().UnixNano()
	s.windowMu.Lock()
	s.startTime.Store(now)
	s.windowStart.Store(now)
	s.snapTime.Store(now)
	s.windowBytes.Store(0)
	s.windowVideo.Store(0)
	s.snapBytes.Store(0)
	s.snapVideo.Store(0)
	s.windowMu.Unlock()
}

// recordFrame updates counters for an incoming frame.
func (s *StreamStats) recordFrame(payloadSize int, isVideo bool) {
	s.bytesIn.Add(int64(payloadSize))
	if isVideo {
		s.videoFrames.Add(1)
	} else {
		s.audioFrames.Add(1)
	}
	// Update sliding window counters without contending with stats readers.
	s.windowBytes.Add(int64(payloadSize))
	if isVideo {
		s.windowVideo.Add(1)
	}
}

// StreamStatsSnapshot is a point-in-time copy of stream statistics.
type StreamStatsSnapshot struct {
	BytesIn     int64         `json:"bytes_in"`
	VideoFrames int64         `json:"video_frames"`
	AudioFrames int64         `json:"audio_frames"`
	StartTime   time.Time     `json:"start_time"`
	Uptime      time.Duration `json:"uptime"`
	BitrateKbps int64         `json:"bitrate_kbps"`
	FPS         float64       `json:"fps"`
}

// snapshot returns a point-in-time copy with computed fields.
// BitrateKbps and FPS are instantaneous (sliding window), not cumulative averages.
func (s *StreamStats) snapshot() StreamStatsSnapshot {
	now := time.Now()

	snap := StreamStatsSnapshot{
		BytesIn:     s.bytesIn.Load(),
		VideoFrames: s.videoFrames.Load(),
		AudioFrames: s.audioFrames.Load(),
	}

	// Compute instantaneous bitrate and FPS from the sliding window. Rotation is
	// serialized, but frame ingestion uses atomic counters and does not wait.
	s.windowMu.Lock()
	startNano := s.startTime.Load()
	windowStartNano := s.windowStart.Load()
	snapTimeNano := s.snapTime.Load()
	startTime := time.Unix(0, startNano)
	windowStart := time.Unix(0, windowStartNano)
	snapTime := time.Unix(0, snapTimeNano)
	snap.StartTime = startTime
	elapsed := now.Sub(startTime)
	snap.Uptime = elapsed
	windowElapsed := now.Sub(windowStart)
	if windowElapsed >= statsWindowDuration {
		// Window has enough data: compute rates from current window,
		// then rotate: current window becomes the new snapshot.
		if ms := windowElapsed.Milliseconds(); ms > 0 {
			snap.BitrateKbps = s.windowBytes.Load() * 8 / ms
			snap.FPS = float64(s.windowVideo.Load()) / windowElapsed.Seconds()
		}
		s.snapBytes.Store(s.windowBytes.Swap(0))
		s.snapVideo.Store(s.windowVideo.Swap(0))
		s.snapTime.Store(windowStartNano)
		s.windowStart.Store(now.UnixNano())
	} else if snapTimeNano != startNano || windowElapsed > 0 {
		// Window too short: use snapshot + current window combined.
		totalBytes := s.snapBytes.Load() + s.windowBytes.Load()
		totalVideo := s.snapVideo.Load() + s.windowVideo.Load()
		totalElapsed := now.Sub(snapTime)
		if ms := totalElapsed.Milliseconds(); ms > 0 {
			snap.BitrateKbps = totalBytes * 8 / ms
			snap.FPS = float64(totalVideo) / totalElapsed.Seconds()
		}
	} else {
		// Very early: fall back to cumulative average.
		if ms := elapsed.Milliseconds(); ms > 0 {
			snap.BitrateKbps = snap.BytesIn * 8 / ms
			snap.FPS = float64(snap.VideoFrames) / elapsed.Seconds()
		}
	}
	s.windowMu.Unlock()

	return snap
}
