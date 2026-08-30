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
	ConsumerStateNormal ConsumerState = iota
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

// SlowConsumerFrameResult binds overwrite metadata to one frame read. Frame is
// always nil when OK is false; Overwritten remains set on threshold disconnect.
type SlowConsumerFrameResult struct {
	Frame       *avframe.AVFrame
	OK          bool
	Overwritten int64
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
		result := f.NextFrameResult()
		if !result.OK {
			return nil, false
		}
		if result.Frame == nil {
			continue
		}
		if result.Overwritten == 0 || f.applyPolicy(result.Frame) {
			return result.Frame, true
		}
	}
}

// NextFrameResult reads the next frame and returns overwrite metadata from the
// same Ring read. An overwrite is returned before frame-drop policy is applied
// so callers can discard the retained frame and advance to live safely.
func (f *SlowConsumerFilter) NextFrameResult() SlowConsumerFrameResult {
	for {
		read := f.reader.TryReadResult()
		if !read.OK {
			read = f.reader.ReadResult()
			if !read.OK {
				return SlowConsumerFrameResult{}
			}
		}

		result := SlowConsumerFrameResult{
			Frame:       read.Value,
			OK:          true,
			Overwritten: read.Overwritten,
		}
		if result.Overwritten > 0 {
			if f.recordOverwrite(result.Overwritten) {
				result.Frame = nil
				result.OK = false
			}
			return result
		}
		if result.Frame == nil {
			continue
		}
		if f.applyPolicy(result.Frame) {
			return result
		}
	}
}

func (f *SlowConsumerFilter) recordOverwrite(overwritten int64) bool {
	if f.skipTracker == nil {
		return false
	}
	if !f.skipTracker.RecordSkip() {
		return false
	}
	slog.Warn("subscriber exceeded skip threshold, disconnecting", "skipped", overwritten)
	return true
}

func (f *SlowConsumerFilter) applyPolicy(frame *avframe.AVFrame) bool {
	// If filter is disabled, pass through all frames
	if !f.config.Enabled {
		return true
	}

	// Update state based on current lag
	f.updateState()

	// Apply drop policy based on current state
	switch f.state {
	case ConsumerStateNormal:
		return true

	case ConsumerStateDropNonKey:
		if f.shouldDeliver(frame) {
			return true
		}
		f.dropped++
		return false

	case ConsumerStateSkipToKey:
		if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() {
			f.state = ConsumerStateDropNonKey
			return true
		}
		// Also deliver audio and sequence headers even in skip-to-key mode
		if frame.MediaType.IsAudio() || frame.FrameType == avframe.FrameTypeSequenceHeader {
			return true
		}
		f.dropped++
		return false
	}
	return false
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
