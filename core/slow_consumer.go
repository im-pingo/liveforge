package core

import (
	"log/slog"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// ConsumerState represents the current frame dropping state.
type ConsumerState uint8

const (
	ConsumerStateNormal     ConsumerState = iota
	ConsumerStateDropNonKey
	ConsumerStateSkipToKey
)

func (cs ConsumerState) String() string {
	switch cs {
	case ConsumerStateNormal:
		return "normal"
	case ConsumerStateDropNonKey:
		return "drop_non_key"
	case ConsumerStateSkipToKey:
		return "skip_to_key"
	default:
		return "unknown"
	}
}

// SlowConsumerFilter wraps a RingReader and applies frame dropping policy
// based on lag ratio and EWMA send time to handle slow consumers gracefully.
// It also integrates SkipTracker to detect ring buffer overwrites.
type SlowConsumerFilter struct {
	reader      *util.RingReader[*avframe.AVFrame]
	config      config.SlowConsumerConfig
	skipTracker *SkipTracker
	state       ConsumerState
	ewmaSend    float64 // EWMA of send duration in milliseconds
	dropped     int64   // total dropped frame count
}

// NewSlowConsumerFilter creates a new filter. If cfg.Enabled is false,
// the filter acts as a passthrough (no dropping).
// skipCfg controls the ring buffer skip tracker; nil disables skip tracking.
func NewSlowConsumerFilter(
	reader *util.RingReader[*avframe.AVFrame],
	cfg config.SlowConsumerConfig,
	skipCfg *config.SkipTrackerConfig,
) *SlowConsumerFilter {
	var st *SkipTracker
	if skipCfg != nil && skipCfg.MaxCount > 0 && skipCfg.Window > 0 {
		st = NewSkipTracker(skipCfg.MaxCount, skipCfg.Window)
	}
	return &SlowConsumerFilter{
		reader:      reader,
		config:      cfg,
		skipTracker: st,
		state:       ConsumerStateNormal,
	}
}

// NextFrame reads the next frame from the ring buffer, applying the drop policy.
// Returns (frame, true) on success, or (nil, false) if the ring buffer is closed
// or the subscriber exceeded the skip threshold.
func (f *SlowConsumerFilter) NextFrame() (*avframe.AVFrame, bool) {
	for {
		frame, ok := f.reader.TryRead()
		if !ok {
			frame, ok = f.reader.Read()
			if !ok {
				return nil, false
			}
		}
		if frame == nil {
			continue
		}

		// Check if ring buffer frames were skipped (overwritten)
		if f.skipTracker != nil && f.reader.Skipped() > 0 {
			if f.skipTracker.RecordSkip() {
				slog.Warn("subscriber exceeded skip threshold, disconnecting",
					"skipped", f.reader.Skipped())
				return nil, false
			}
		}

		// If filter is disabled, pass through all frames
		if !f.config.Enabled {
			return frame, true
		}

		// Update state based on current lag
		f.updateState()

		// Apply drop policy based on current state
		switch f.state {
		case ConsumerStateNormal:
			return frame, true

		case ConsumerStateDropNonKey:
			if f.shouldDeliver(frame) {
				return frame, true
			}
			f.dropped++
			continue // skip this frame, read next

		case ConsumerStateSkipToKey:
			if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() {
				f.state = ConsumerStateDropNonKey
				return frame, true
			}
			// Also deliver audio and sequence headers even in skip-to-key mode
			if frame.MediaType.IsAudio() || frame.FrameType == avframe.FrameTypeSequenceHeader {
				return frame, true
			}
			f.dropped++
			continue
		}
	}
}

// ReportSendTime updates the EWMA with the duration of the last frame send.
// Call this after each successful sendFrame().
func (f *SlowConsumerFilter) ReportSendTime(d time.Duration) {
	ms := float64(d.Milliseconds())
	if f.ewmaSend == 0 {
		f.ewmaSend = ms
	} else {
		f.ewmaSend = f.config.EWMAAlpha*ms + (1-f.config.EWMAAlpha)*f.ewmaSend
	}
}

// State returns the current consumer state.
func (f *SlowConsumerFilter) State() ConsumerState {
	return f.state
}

// Dropped returns the total number of frames dropped.
func (f *SlowConsumerFilter) Dropped() int64 {
	return f.dropped
}

// Close marks the underlying reader as closed, unblocking any in-progress Read().
func (f *SlowConsumerFilter) Close() {
	f.reader.Close()
}

// updateState transitions the state machine based on lag ratio and EWMA.
func (f *SlowConsumerFilter) updateState() {
	lag := f.reader.Lag()

	switch f.state {
	case ConsumerStateNormal:
		if lag > f.config.LagDropRatio && f.isSendSlow() {
			f.state = ConsumerStateDropNonKey
		}
	case ConsumerStateDropNonKey:
		if lag > f.config.LagCriticalRatio {
			f.state = ConsumerStateSkipToKey
		} else if lag < f.config.LagRecoverRatio {
			f.state = ConsumerStateNormal
		}
	case ConsumerStateSkipToKey:
		if lag < f.config.LagRecoverRatio {
			f.state = ConsumerStateNormal
		}
	}
}

// isSendSlow returns true if the EWMA send time exceeds the threshold.
func (f *SlowConsumerFilter) isSendSlow() bool {
	if f.ewmaSend == 0 {
		return false // no data yet, assume normal
	}
	// Default frame interval: assume 30fps = ~33ms per frame
	frameIntervalMs := 33.0
	return f.ewmaSend > f.config.SendTimeRatio*frameIntervalMs
}

// shouldDeliver returns true if the frame should be delivered in DropNonKey state.
// Keyframes, sequence headers, and audio frames are always delivered.
func (f *SlowConsumerFilter) shouldDeliver(frame *avframe.AVFrame) bool {
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		return true
	}
	if frame.MediaType.IsAudio() {
		return true
	}
	if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() {
		return true
	}
	return false
}
