package core

import (
	"sync/atomic"
	"time"
)

// StreamStats collects protocol-agnostic statistics for a stream.
// All counters use atomic operations for lock-free updates on the hot path.
type StreamStats struct {
	bytesIn     atomic.Int64
	videoFrames atomic.Int64
	audioFrames atomic.Int64
	startTime   time.Time
	lastFrame   atomic.Value // time.Time
}

// initStats sets the start time. Called once when the stream begins publishing.
func (s *StreamStats) initStats() {
	s.startTime = time.Now()
	s.lastFrame.Store(time.Now())
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

	if ms := elapsed.Milliseconds(); ms > 0 {
		snap.BitrateKbps = snap.BytesIn * 8 / ms
		snap.FPS = float64(snap.VideoFrames) / elapsed.Seconds()
	}

	return snap
}
