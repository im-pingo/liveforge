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

// TestBFrameDropDTSDuration verifies that when B-frames are dropped,
// the DTS-based duration computation correctly spans the gap.
//
// For IBBBP (decode order), the sent frames are I and P. The duration
// between them should be the DTS delta of the two sent frames (I→P),
// not the DTS delta of adjacent decode-order frames.
//
// This ensures RTP timestamps advance at real-time rate even when
// intermediate B-frames are dropped.
func TestBFrameDropDTSDuration(t *testing.T) {
	type frame struct {
		dts        int64
		pts        int64
		isKeyframe bool
	}

	// Simulate the DTS-based duration logic from writeVideoSample.
	bframeDurationTest := func(frames []frame) []time.Duration {
		var maxSentPTS int64
		var lastSentDTS int64 = -1
		var durations []time.Duration

		for _, f := range frames {
			// B-frame drop: skip non-keyframes with PTS < maxSentPTS.
			if !f.isKeyframe && f.pts < maxSentPTS {
				continue
			}

			// Duration from DTS delta of sent frames.
			var dur time.Duration
			if lastSentDTS >= 0 && f.dts > lastSentDTS {
				dur = time.Duration(f.dts-lastSentDTS) * time.Millisecond
			} else {
				dur = 40 * time.Millisecond
			}
			durations = append(durations, dur)
			lastSentDTS = f.dts

			if f.pts > maxSentPTS {
				maxSentPTS = f.pts
			}
		}
		return durations
	}

	tests := []struct {
		name         string
		input        []frame
		wantDuration []time.Duration
	}{
		{
			name: "no B-frames 30fps",
			input: []frame{
				{dts: 0, pts: 0, isKeyframe: true},
				{dts: 33, pts: 33},
				{dts: 66, pts: 66},
				{dts: 100, pts: 100},
			},
			wantDuration: []time.Duration{
				40 * time.Millisecond, // first frame default
				33 * time.Millisecond,
				33 * time.Millisecond,
				34 * time.Millisecond,
			},
		},
		{
			name: "IBBP drops two B-frames",
			// Decode order: I(dts=0,pts=120) B(dts=33,pts=40) B(dts=66,pts=80) P(dts=100,pts=160)
			input: []frame{
				{dts: 0, pts: 120, isKeyframe: true},
				{dts: 33, pts: 40},  // B: dropped (40<120)
				{dts: 66, pts: 80},  // B: dropped (80<120)
				{dts: 100, pts: 160}, // P: sent
			},
			// Sent: I(dts=0) and P(dts=100). Duration I→P = 100ms.
			wantDuration: []time.Duration{
				40 * time.Millisecond,  // first frame default
				100 * time.Millisecond, // DTS gap spans dropped B-frames
			},
		},
		{
			name: "IBBBP drops three B-frames",
			input: []frame{
				{dts: 0, pts: 160, isKeyframe: true},
				{dts: 33, pts: 40},
				{dts: 66, pts: 80},
				{dts: 100, pts: 120},
				{dts: 133, pts: 200},
			},
			wantDuration: []time.Duration{
				40 * time.Millisecond,  // first frame default
				133 * time.Millisecond, // DTS gap spans 3 dropped B-frames
			},
		},
		{
			name: "two GOPs with B-frames",
			input: []frame{
				// GOP 1: IBBP
				{dts: 0, pts: 120, isKeyframe: true},
				{dts: 33, pts: 40},
				{dts: 66, pts: 80},
				{dts: 100, pts: 160},
				// GOP 2: IBBP
				{dts: 133, pts: 280, isKeyframe: true},
				{dts: 166, pts: 200},
				{dts: 200, pts: 240},
				{dts: 233, pts: 320},
			},
			wantDuration: []time.Duration{
				40 * time.Millisecond,  // GOP1 I (first frame)
				100 * time.Millisecond, // GOP1 P (dts 0→100)
				33 * time.Millisecond,  // GOP2 I (dts 100→133)
				100 * time.Millisecond, // GOP2 P (dts 133→233)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bframeDurationTest(tt.input)
			if len(got) != len(tt.wantDuration) {
				t.Fatalf("got %d durations, want %d: %v", len(got), len(tt.wantDuration), got)
			}
			for i := range got {
				if got[i] != tt.wantDuration[i] {
					t.Errorf("[%d] duration = %v, want %v", i, got[i], tt.wantDuration[i])
				}
			}
		})
	}
}

// TestBFrameDrop verifies the B-frame detection logic used in whepFeedLoop.
// Chrome's WebRTC H.264 decoder assumes Baseline profile and does not perform
// B-frame reordering. Sending B-frames causes mosaic corruption, so we drop
// them and only send I/P reference frames.
//
// Detection: track maxSentPTS. Any non-keyframe whose PTS < maxSentPTS is a
// B-frame (its display time precedes a previously sent reference frame).
func TestBFrameDrop(t *testing.T) {
	type frame struct {
		pts        int64
		isKeyframe bool
	}

	// Simulate B-frame drop logic from writeVideoSample.
	bframeDrop := func(frames []frame) []int64 {
		var maxSentPTS int64
		var sent []int64
		for _, f := range frames {
			if !f.isKeyframe && f.pts < maxSentPTS {
				// B-frame: drop silently.
				continue
			}
			sent = append(sent, f.pts)
			if f.pts > maxSentPTS {
				maxSentPTS = f.pts
			}
		}
		return sent
	}

	tests := []struct {
		name    string
		input   []frame
		wantPTS []int64
	}{
		{
			name: "no B-frames (PTS==DTS monotonic)",
			input: []frame{
				{pts: 0, isKeyframe: true},
				{pts: 33},
				{pts: 66},
				{pts: 100},
			},
			wantPTS: []int64{0, 33, 66, 100},
		},
		{
			name: "IBBP drops two B-frames",
			input: []frame{
				{pts: 120, isKeyframe: true}, // I: sent, max=120
				{pts: 40},                    // B: 40<120, drop
				{pts: 80},                    // B: 80<120, drop
				{pts: 160},                   // P: 160>=120, sent
			},
			wantPTS: []int64{120, 160},
		},
		{
			name: "IBBBP drops three B-frames",
			input: []frame{
				{pts: 160, isKeyframe: true}, // I: sent, max=160
				{pts: 40},                    // B: drop
				{pts: 80},                    // B: drop
				{pts: 120},                   // B: drop
				{pts: 200},                   // P: sent
			},
			wantPTS: []int64{160, 200},
		},
		{
			name: "two GOPs with B-frames",
			input: []frame{
				{pts: 120, isKeyframe: true}, // I: sent
				{pts: 40},                    // B: drop
				{pts: 80},                    // B: drop
				{pts: 160},                   // P: sent
				// New GOP
				{pts: 280, isKeyframe: true}, // I: sent (keyframe always sent)
				{pts: 200},                   // B: drop (200<280)
				{pts: 240},                   // B: drop (240<280)
				{pts: 320},                   // P: sent
			},
			wantPTS: []int64{120, 160, 280, 320},
		},
		{
			name: "keyframe with lower PTS still sent",
			input: []frame{
				{pts: 100, isKeyframe: true},
				{pts: 200},
				{pts: 50, isKeyframe: true}, // keyframe: always sent regardless of PTS
			},
			wantPTS: []int64{100, 200, 50},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bframeDrop(tt.input)
			if len(got) != len(tt.wantPTS) {
				t.Fatalf("got %d frames, want %d: %v", len(got), len(tt.wantPTS), got)
			}
			for i := range got {
				if got[i] != tt.wantPTS[i] {
					t.Errorf("[%d] PTS = %d, want %d", i, got[i], tt.wantPTS[i])
				}
			}
		})
	}
}
