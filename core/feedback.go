package core

import (
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
)

// FeedbackRouter controls how subscriber feedback (PLI, NACK) is forwarded
// to the publisher. It selects a routing strategy based on the configured mode
// and subscriber count thresholds.
type FeedbackRouter struct {
	cfg config.FeedbackConfig

	mu              sync.Mutex
	subscriberCount int
	lastPLITime     time.Time
	aggregateWindow time.Duration // minimum interval between forwarded PLIs in aggregate mode
}

// NewFeedbackRouter creates a feedback router with the given configuration.
func NewFeedbackRouter(cfg config.FeedbackConfig) *FeedbackRouter {
	return &FeedbackRouter{
		cfg:             cfg,
		aggregateWindow: 500 * time.Millisecond,
	}
}

// SetSubscriberCount updates the subscriber count used by auto mode.
func (r *FeedbackRouter) SetSubscriberCount(n int) {
	r.mu.Lock()
	r.subscriberCount = n
	r.mu.Unlock()
}

// EffectiveMode returns the active feedback mode based on configuration
// and current subscriber count. For auto mode, it selects passthrough,
// aggregate, or drop based on thresholds.
func (r *FeedbackRouter) EffectiveMode() FeedbackMode {
	mode := parseFeedbackMode(r.cfg.DefaultMode)
	if mode != FeedbackAuto {
		return mode
	}

	r.mu.Lock()
	count := r.subscriberCount
	r.mu.Unlock()

	pt := r.cfg.AutoThresholds.PassthroughMax
	ag := r.cfg.AutoThresholds.AggregateMax

	switch {
	case pt > 0 && count <= pt:
		return FeedbackPassthrough
	case ag > 0 && count <= ag:
		return FeedbackAggregate
	default:
		return FeedbackDrop
	}
}

// ShouldForwardPLI returns true if a PLI request from a subscriber should
// be forwarded to the publisher given the current routing mode.
func (r *FeedbackRouter) ShouldForwardPLI() bool {
	mode := r.EffectiveMode()

	switch mode {
	case FeedbackPassthrough:
		return true

	case FeedbackAggregate:
		r.mu.Lock()
		defer r.mu.Unlock()
		now := time.Now()
		if now.Sub(r.lastPLITime) >= r.aggregateWindow {
			r.lastPLITime = now
			return true
		}
		return false

	case FeedbackDrop, FeedbackServerSide:
		return false

	default:
		return true
	}
}

// ShouldForwardNACK returns true if a NACK request should be forwarded.
// NACK is always forwarded except in drop mode, since retransmission is
// handled by pion's NACK responder interceptor at the WebRTC layer.
func (r *FeedbackRouter) ShouldForwardNACK() bool {
	mode := r.EffectiveMode()
	return mode != FeedbackDrop
}

// parseFeedbackMode converts a config string to FeedbackMode.
func parseFeedbackMode(s string) FeedbackMode {
	switch s {
	case "passthrough":
		return FeedbackPassthrough
	case "aggregate":
		return FeedbackAggregate
	case "drop":
		return FeedbackDrop
	case "server_side":
		return FeedbackServerSide
	default:
		return FeedbackAuto
	}
}
