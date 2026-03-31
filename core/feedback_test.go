package core

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
)

func TestParseFeedbackMode(t *testing.T) {
	tests := []struct {
		input string
		want  FeedbackMode
	}{
		{"auto", FeedbackAuto},
		{"passthrough", FeedbackPassthrough},
		{"aggregate", FeedbackAggregate},
		{"drop", FeedbackDrop},
		{"server_side", FeedbackServerSide},
		{"", FeedbackAuto},
		{"unknown", FeedbackAuto},
	}
	for _, tt := range tests {
		got := parseFeedbackMode(tt.input)
		if got != tt.want {
			t.Errorf("parseFeedbackMode(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFeedbackRouterFixedModes(t *testing.T) {
	tests := []struct {
		mode      string
		wantPLI   bool
		wantNACK  bool
	}{
		{"passthrough", true, true},
		{"drop", false, false},
		{"server_side", false, true},
	}

	for _, tt := range tests {
		r := NewFeedbackRouter(config.FeedbackConfig{DefaultMode: tt.mode})
		if got := r.ShouldForwardPLI(); got != tt.wantPLI {
			t.Errorf("mode=%q: ShouldForwardPLI() = %v, want %v", tt.mode, got, tt.wantPLI)
		}
		if got := r.ShouldForwardNACK(); got != tt.wantNACK {
			t.Errorf("mode=%q: ShouldForwardNACK() = %v, want %v", tt.mode, got, tt.wantNACK)
		}
	}
}

func TestFeedbackRouterAutoMode(t *testing.T) {
	cfg := config.FeedbackConfig{
		DefaultMode: "auto",
		AutoThresholds: config.AutoThresholdsConfig{
			PassthroughMax: 1,
			AggregateMax:   5,
		},
	}
	r := NewFeedbackRouter(cfg)

	// 0 subscribers: passthrough (<=1)
	r.SetSubscriberCount(0)
	if r.EffectiveMode() != FeedbackPassthrough {
		t.Errorf("0 subs: got %v, want Passthrough", r.EffectiveMode())
	}

	// 1 subscriber: passthrough (<=1)
	r.SetSubscriberCount(1)
	if r.EffectiveMode() != FeedbackPassthrough {
		t.Errorf("1 sub: got %v, want Passthrough", r.EffectiveMode())
	}

	// 2 subscribers: aggregate (>1, <=5)
	r.SetSubscriberCount(2)
	if r.EffectiveMode() != FeedbackAggregate {
		t.Errorf("2 subs: got %v, want Aggregate", r.EffectiveMode())
	}

	// 5 subscribers: aggregate (<=5)
	r.SetSubscriberCount(5)
	if r.EffectiveMode() != FeedbackAggregate {
		t.Errorf("5 subs: got %v, want Aggregate", r.EffectiveMode())
	}

	// 6 subscribers: drop (>5)
	r.SetSubscriberCount(6)
	if r.EffectiveMode() != FeedbackDrop {
		t.Errorf("6 subs: got %v, want Drop", r.EffectiveMode())
	}
}

func TestFeedbackRouterAggregatePLIRateLimit(t *testing.T) {
	r := NewFeedbackRouter(config.FeedbackConfig{DefaultMode: "aggregate"})
	r.aggregateWindow = 50 * time.Millisecond // short window for testing

	// First PLI should be forwarded
	if !r.ShouldForwardPLI() {
		t.Error("first PLI should be forwarded")
	}

	// Immediate second PLI should be rate-limited
	if r.ShouldForwardPLI() {
		t.Error("second PLI should be rate-limited")
	}

	// After the window, PLI should be forwarded again
	time.Sleep(60 * time.Millisecond)
	if !r.ShouldForwardPLI() {
		t.Error("PLI after window should be forwarded")
	}
}

func TestFeedbackRouterAutoModePLI(t *testing.T) {
	cfg := config.FeedbackConfig{
		DefaultMode: "auto",
		AutoThresholds: config.AutoThresholdsConfig{
			PassthroughMax: 1,
			AggregateMax:   3,
		},
	}
	r := NewFeedbackRouter(cfg)

	// 1 subscriber: passthrough — all PLIs forwarded
	r.SetSubscriberCount(1)
	if !r.ShouldForwardPLI() {
		t.Error("passthrough mode should forward PLI")
	}
	if !r.ShouldForwardPLI() {
		t.Error("passthrough mode should forward all PLIs")
	}

	// 5 subscribers: drop — no PLIs forwarded
	r.SetSubscriberCount(5)
	if r.ShouldForwardPLI() {
		t.Error("drop mode should not forward PLI")
	}
}

func TestStreamFeedbackRouter(t *testing.T) {
	cfg := config.StreamConfig{
		RingBufferSize: 256,
		Feedback: config.FeedbackConfig{
			DefaultMode: "auto",
			AutoThresholds: config.AutoThresholdsConfig{
				PassthroughMax: 2,
				AggregateMax:   5,
			},
		},
	}
	bus := NewEventBus()
	s := NewStream("test/stream", cfg, config.LimitsConfig{}, bus)

	if s.FeedbackRouter() == nil {
		t.Fatal("FeedbackRouter should not be nil")
	}

	// Initially 0 subscribers: passthrough
	if s.FeedbackRouter().EffectiveMode() != FeedbackPassthrough {
		t.Error("expected passthrough with 0 subscribers")
	}

	// Add subscribers and check mode transitions
	s.AddSubscriber("webrtc")
	s.AddSubscriber("webrtc")
	if s.FeedbackRouter().EffectiveMode() != FeedbackPassthrough {
		t.Error("expected passthrough with 2 subscribers")
	}

	s.AddSubscriber("webrtc")
	if s.FeedbackRouter().EffectiveMode() != FeedbackAggregate {
		t.Error("expected aggregate with 3 subscribers")
	}

	s.RemoveSubscriber("webrtc")
	s.RemoveSubscriber("webrtc")
	if s.FeedbackRouter().EffectiveMode() != FeedbackPassthrough {
		t.Error("expected passthrough after removing subscribers")
	}
}
