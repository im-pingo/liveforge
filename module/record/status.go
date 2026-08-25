package record

import (
	"sync/atomic"
	"time"
)

type RecordingSessionStatus struct {
	StreamKey    string         `json:"stream_key"`
	RecordingID  string         `json:"recording_id,omitempty"`
	State        RecordingState `json:"state"`
	StartedAt    time.Time      `json:"started_at"`
	CompletedAt  time.Time      `json:"completed_at,omitempty"`
	DurationSec  float64        `json:"duration_sec"`
	Bytes        int64          `json:"bytes"`
	WriteRetries uint64         `json:"write_retries"`
	LastError    string         `json:"last_error,omitempty"`
}

type RecordingMetricsSnapshot struct {
	FilesCompleted uint64 `json:"files_completed"`
	FilesFailed    uint64 `json:"files_failed"`
	WriteRetries   uint64 `json:"write_retries"`
	WriteFailures  uint64 `json:"write_failures"`
	FilesDeleted   uint64 `json:"files_deleted"`
	BytesWritten   uint64 `json:"bytes_written"`
	StorageErrors  uint64 `json:"storage_errors"`
}

type RecordingStatusSnapshot struct {
	Sessions []RecordingSessionStatus `json:"sessions"`
	Storage  StorageHealth            `json:"storage"`
	Metrics  RecordingMetricsSnapshot `json:"metrics"`
}

type RecordingMetrics struct {
	completed    atomic.Uint64
	failed       atomic.Uint64
	retries      atomic.Uint64
	writeFailure atomic.Uint64
	deleted      atomic.Uint64
	bytesWritten atomic.Uint64
	storageError atomic.Uint64
}

func (m *RecordingMetrics) Snapshot() RecordingMetricsSnapshot {
	return RecordingMetricsSnapshot{
		FilesCompleted: m.completed.Load(),
		FilesFailed:    m.failed.Load(),
		WriteRetries:   m.retries.Load(),
		WriteFailures:  m.writeFailure.Load(),
		FilesDeleted:   m.deleted.Load(),
		BytesWritten:   m.bytesWritten.Load(),
		StorageErrors:  m.storageError.Load(),
	}
}
