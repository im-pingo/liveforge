package webrtc

import (
	"testing"
	"time"
)

// TestDTSPaceDecision tests the pacing decision logic extracted from
// the feed loop. This validates the simplified behavior:
//   - sleepDur > 0 && < 1s  → should sleep
//   - sleepDur in [-1s, 0]  → should deliver immediately (no drop)
//   - |sleepDur| > 1s       → should reset pace base
func TestDTSPaceDecision(t *testing.T) {
	tests := []struct {
		name       string
		sleepDur   time.Duration
		wantAction string // "sleep", "deliver", "reset"
	}{
		{"ahead_40ms", 40 * time.Millisecond, "sleep"},
		{"ahead_500ms", 500 * time.Millisecond, "sleep"},
		{"exactly_on_time", 0, "deliver"},
		{"behind_40ms", -40 * time.Millisecond, "deliver"},
		{"behind_200ms", -200 * time.Millisecond, "deliver"},
		{"behind_500ms", -500 * time.Millisecond, "deliver"},
		{"behind_999ms", -999 * time.Millisecond, "deliver"},
		{"behind_1001ms", -1001 * time.Millisecond, "reset"},
		{"ahead_1001ms", 1001 * time.Millisecond, "reset"},
		{"behind_2s", -2 * time.Second, "reset"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dtsPaceAction(tt.sleepDur)
			if got != tt.wantAction {
				t.Errorf("dtsPaceAction(%v) = %q, want %q", tt.sleepDur, got, tt.wantAction)
			}
		})
	}
}
