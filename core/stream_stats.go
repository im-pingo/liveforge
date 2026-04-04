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
	startTime   time.Time
	lastFrame   atomic.Value // time.Time

	// Sliding window for instantaneous bitrate/FPS.
	windowMu    sync.Mutex
	windowBytes int64
	windowVideo int64
	windowStart time.Time
	snapBytes   int64
	snapVideo   int64
	snapTime    time.Time
}

const statsWindowDuration = 2 * time.Second

// initStats sets the start time. Called once when the stream begins publishing.
func (s *StreamStats) initStats() {
	now := time.Now()
	s.startTime = now
	s.lastFrame.Store(now)
	s.windowStart = now
	s.snapTime = now
}

// recordFrame updates counters for an incoming frame.
func (s *StreamStats) recordFrame(payloadSize int, isVideo bool) {
	s.bytesIn.Add(int64(payloadSize))
	if isVideo {
		s.videoFrames.Add(1)
	} else {
		s.audioFrames.Add(1)
	}
	s.lastFrame.Store(time.Now())

	// Update sliding window counters.
	s.windowMu.Lock()
	s.windowBytes += int64(payloadSize)
	if isVideo {
		s.windowVideo++
	}
	s.windowMu.Unlock()
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
	elapsed := now.Sub(s.startTime)

	snap := StreamStatsSnapshot{
		BytesIn:     s.bytesIn.Load(),
		VideoFrames: s.videoFrames.Load(),
		AudioFrames: s.audioFrames.Load(),
		StartTime:   s.startTime,
		Uptime:      elapsed,
	}

	// Compute instantaneous bitrate and FPS from sliding window.
	s.windowMu.Lock()
	windowElapsed := now.Sub(s.windowStart)
	if windowElapsed >= statsWindowDuration {
		// Window has enough data: compute rates from current window,
		// then rotate: current window becomes the new snapshot.
		if ms := windowElapsed.Milliseconds(); ms > 0 {
			snap.BitrateKbps = s.windowBytes * 8 / ms
			snap.FPS = float64(s.windowVideo) / windowElapsed.Seconds()
		}
		s.snapBytes = s.windowBytes
		s.snapVideo = s.windowVideo
		s.snapTime = s.windowStart
		s.windowBytes = 0
		s.windowVideo = 0
		s.windowStart = now
	} else if s.snapTime != s.startTime || windowElapsed > 0 {
		// Window too short: use snapshot + current window combined.
		totalBytes := s.snapBytes + s.windowBytes
		totalVideo := s.snapVideo + s.windowVideo
		totalElapsed := now.Sub(s.snapTime)
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
