package dvr

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// DVRStatusProvider exposes bounded DVR management state to the API module.
type DVRStatusProvider interface {
	DVRStatus() DVRStatusSnapshot
	DVRSession(string) (DVRSessionStatus, bool)
}

type DVRMetricsSnapshot struct {
	SegmentsWritten uint64 `json:"segments_written"`
	SegmentBytes    uint64 `json:"segment_bytes"`
	WriteRetries    uint64 `json:"write_retries"`
	WriteFailures   uint64 `json:"write_failures"`
	CleanupDeleted  uint64 `json:"cleanup_deleted"`
	CleanupBytes    uint64 `json:"cleanup_bytes"`
	CleanupFailures uint64 `json:"cleanup_failures"`
}

func dvrStorageHealth(pathTemplate string) DVRStorageHealth {
	root := dvrStorageRoot(pathTemplate)
	health := DVRStorageHealth{Root: root}
	probe := root
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			health.Error = "no existing storage parent"
			return health
		}
		probe = parent
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(probe, &stat); err != nil {
		health.Error = err.Error()
		return health
	}
	health.TotalBytes = stat.Blocks * uint64(stat.Bsize)
	health.AvailableBytes = stat.Bavail * uint64(stat.Bsize)
	health.Healthy = health.TotalBytes > 0
	health.LowSpace = health.AvailableBytes < uint64(100<<20) || (health.TotalBytes > 0 && health.AvailableBytes*100/health.TotalBytes < 5)
	return health
}

func dvrStorageRoot(pathTemplate string) string {
	if pathTemplate == "" {
		pathTemplate = filepath.Join(".", "dvr", "{stream_key}")
	}
	if i := strings.IndexByte(pathTemplate, '{'); i >= 0 {
		prefix := pathTemplate[:i]
		if strings.HasSuffix(prefix, string(filepath.Separator)) {
			return filepath.Clean(prefix)
		}
		return filepath.Dir(prefix)
	}
	return filepath.Clean(pathTemplate)
}

type DVRMetrics struct {
	segmentsWritten atomic.Uint64
	segmentBytes    atomic.Uint64
	writeRetries    atomic.Uint64
	writeFailures   atomic.Uint64
	cleanupDeleted  atomic.Uint64
	cleanupBytes    atomic.Uint64
	cleanupFailures atomic.Uint64
}

func (m *DVRMetrics) Snapshot() DVRMetricsSnapshot {
	return DVRMetricsSnapshot{
		SegmentsWritten: m.segmentsWritten.Load(),
		SegmentBytes:    m.segmentBytes.Load(),
		WriteRetries:    m.writeRetries.Load(),
		WriteFailures:   m.writeFailures.Load(),
		CleanupDeleted:  m.cleanupDeleted.Load(),
		CleanupBytes:    m.cleanupBytes.Load(),
		CleanupFailures: m.cleanupFailures.Load(),
	}
}

type DVRStorageHealth struct {
	Root           string `json:"root"`
	Healthy        bool   `json:"healthy"`
	LowSpace       bool   `json:"low_space"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	Error          string `json:"error,omitempty"`
}

type DVRStatusSnapshot struct {
	Sessions []DVRSessionStatus `json:"sessions"`
	Storage  DVRStorageHealth   `json:"storage"`
	Metrics  DVRMetricsSnapshot `json:"metrics"`
}

type SessionStatus struct {
	StreamKey string             `json:"stream_key"`
	Live      bool               `json:"live"`
	StartedAt time.Time          `json:"started_at"`
	LastError string             `json:"last_error,omitempty"`
	Metrics   DVRMetricsSnapshot `json:"metrics"`
}
