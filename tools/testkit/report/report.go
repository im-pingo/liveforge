// Package report defines the data structures used by all testkit components
// to represent test results. It also provides JSON and human-readable formatters
// and an assertion engine for CI/CD pass/fail evaluation.
package report

import "time"

// PlayReport contains the results of a play (subscribe) test session.
type PlayReport struct {
	Video      VideoReport  `json:"video"`
	Audio      AudioReport  `json:"audio"`
	Sync       SyncReport   `json:"sync"`
	Stalls     []StallEvent `json:"stalls"`
	Codec      CodecReport  `json:"codec"`
	DurationMs int64        `json:"duration_ms"`
	Error      string       `json:"error,omitempty"`
}

// VideoReport contains video stream analysis results.
type VideoReport struct {
	Codec            string  `json:"codec"`
	Profile          string  `json:"profile"`
	Level            string  `json:"level"`
	Resolution       string  `json:"resolution"`
	FPS              float64 `json:"fps"`
	BitrateKbps      float64 `json:"bitrate_kbps"`
	KeyframeInterval float64 `json:"keyframe_interval"`
	DTSMonotonic     bool    `json:"dts_monotonic"`
	FrameCount       int64   `json:"frame_count"`
}

// AudioReport contains audio stream analysis results.
type AudioReport struct {
	Codec        string  `json:"codec"`
	SampleRate   int     `json:"sample_rate"`
	Channels     int     `json:"channels"`
	BitrateKbps  float64 `json:"bitrate_kbps"`
	DTSMonotonic bool    `json:"dts_monotonic"`
	FrameCount   int64   `json:"frame_count"`
}

// SyncReport contains audio-video synchronization measurements.
type SyncReport struct {
	MaxDriftMs float64 `json:"max_drift_ms"`
	AvgDriftMs float64 `json:"avg_drift_ms"`
}

// StallEvent represents a detected stall (gap) in the media stream.
type StallEvent struct {
	TimestampMs int64   `json:"timestamp_ms"`
	GapMs       float64 `json:"gap_ms"`
	MediaType   string  `json:"media_type"`
}

// CodecReport contains codec parameter validation results.
type CodecReport struct {
	VideoMatch bool   `json:"video_match"`
	AudioMatch bool   `json:"audio_match"`
	Details    string `json:"details,omitempty"`
}

// PushReport contains the results of a push (publish) test session.
type PushReport struct {
	Protocol   string `json:"protocol"`
	Target     string `json:"target"`
	DurationMs int64  `json:"duration_ms"`
	FramesSent int64  `json:"frames_sent"`
	BytesSent  int64  `json:"bytes_sent"`
}

// AuthReport contains the results of an authentication test matrix.
type AuthReport struct {
	Total  int          `json:"total"`
	Passed int          `json:"passed"`
	Failed int          `json:"failed"`
	Cases  []CaseResult `json:"cases"`
}

// CaseResult represents a single authentication test case outcome.
type CaseResult struct {
	Protocol    string `json:"protocol"`
	Action      string `json:"action"`
	Credential  string `json:"credential"`
	ExpectAllow bool   `json:"expect_allow"`
	ActualAllow bool   `json:"actual_allow"`
	Pass        bool   `json:"pass"`
	Error       string `json:"error,omitempty"`
	LatencyMs   int64  `json:"latency_ms"`
}

// ClusterReport contains the results of a multi-node cluster test.
type ClusterReport struct {
	Topology string       `json:"topology"`
	Nodes    []NodeStatus `json:"nodes"`
	Push     *PushReport  `json:"push"`
	Play     *PlayReport  `json:"play"`
	RelayMs  int64        `json:"relay_ms"`
}

// NodeStatus represents the health status of a cluster node.
type NodeStatus struct {
	Name    string         `json:"name"`
	Role    string         `json:"role"`
	Healthy bool           `json:"healthy"`
	Ports   map[string]int `json:"ports"`
}

// TopLevelReport is the root report structure returned by every lf-test command.
type TopLevelReport struct {
	Command    string         `json:"command"`
	Timestamp  time.Time      `json:"timestamp"`
	DurationMs int64          `json:"duration_ms"`
	Pass       bool           `json:"pass"`
	Play       *PlayReport    `json:"play,omitempty"`
	Auth       *AuthReport    `json:"auth,omitempty"`
	Cluster    *ClusterReport `json:"cluster,omitempty"`
	Push       *PushReport    `json:"push,omitempty"`
	Errors     []ErrorDetail  `json:"errors,omitempty"`
}

// ErrorDetail describes a specific error encountered during testing.
type ErrorDetail struct {
	Code    string `json:"code"`    // CONNECT_FAILED, AUTH_REJECTED, TIMEOUT, DEMUX_ERROR, PROTOCOL_ERROR
	Message string `json:"message"`
}
