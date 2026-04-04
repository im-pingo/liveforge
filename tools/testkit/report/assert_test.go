package report

import "testing"

func TestAssertNumeric(t *testing.T) {
	r := &PlayReport{Video: VideoReport{FPS: 29.97}}
	top := &TopLevelReport{Play: r}
	tests := []struct {
		expr string
		pass bool
	}{
		{"video.fps>=29", true},
		{"video.fps>=30", false},
		{"video.fps<30", true},
		{"video.fps>29", true},
		{"video.fps>30", false},
		{"video.fps<=30", true},
		{"video.fps<=29", false},
		{"video.fps!=30", true},
		{"video.fps==29.97", true},
	}
	for _, tt := range tests {
		result, err := EvalAssert(top, tt.expr)
		if err != nil {
			t.Fatalf("EvalAssert(%q): %v", tt.expr, err)
		}
		if result != tt.pass {
			t.Errorf("EvalAssert(%q) = %v, want %v", tt.expr, result, tt.pass)
		}
	}
}

func TestAssertBool(t *testing.T) {
	r := &PlayReport{Video: VideoReport{DTSMonotonic: true}}
	top := &TopLevelReport{Play: r}
	result, err := EvalAssert(top, "video.dts_monotonic==true")
	if err != nil {
		t.Fatalf("EvalAssert: %v", err)
	}
	if !result {
		t.Error("expected true")
	}

	result, err = EvalAssert(top, "video.dts_monotonic!=false")
	if err != nil {
		t.Fatalf("EvalAssert: %v", err)
	}
	if !result {
		t.Error("expected true for !=false")
	}

	result, err = EvalAssert(top, "video.dts_monotonic==false")
	if err != nil {
		t.Fatalf("EvalAssert: %v", err)
	}
	if result {
		t.Error("expected false for ==false")
	}
}

func TestAssertString(t *testing.T) {
	r := &PlayReport{Video: VideoReport{Codec: "H264"}}
	top := &TopLevelReport{Play: r}

	result, err := EvalAssert(top, "video.codec==H264")
	if err != nil {
		t.Fatalf("EvalAssert: %v", err)
	}
	if !result {
		t.Error("expected true for ==H264")
	}

	result, err = EvalAssert(top, "video.codec!=AAC")
	if err != nil {
		t.Fatalf("EvalAssert: %v", err)
	}
	if !result {
		t.Error("expected true for !=AAC")
	}

	result, err = EvalAssert(top, "video.codec==AAC")
	if err != nil {
		t.Fatalf("EvalAssert: %v", err)
	}
	if result {
		t.Error("expected false for ==AAC")
	}
}

func TestAssertAudioFields(t *testing.T) {
	r := &PlayReport{Audio: AudioReport{
		Codec:        "AAC",
		SampleRate:   44100,
		Channels:     2,
		BitrateKbps:  128.0,
		DTSMonotonic: true,
		FrameCount:   500,
	}}
	top := &TopLevelReport{Play: r}
	tests := []struct {
		expr string
		pass bool
	}{
		{"audio.codec==AAC", true},
		{"audio.sample_rate>=44100", true},
		{"audio.channels==2", true},
		{"audio.bitrate_kbps>=100", true},
		{"audio.dts_monotonic==true", true},
		{"audio.frame_count>=500", true},
	}
	for _, tt := range tests {
		result, err := EvalAssert(top, tt.expr)
		if err != nil {
			t.Fatalf("EvalAssert(%q): %v", tt.expr, err)
		}
		if result != tt.pass {
			t.Errorf("EvalAssert(%q) = %v, want %v", tt.expr, result, tt.pass)
		}
	}
}

func TestAssertSyncFields(t *testing.T) {
	r := &PlayReport{Sync: SyncReport{MaxDriftMs: 15.0, AvgDriftMs: 5.0}}
	top := &TopLevelReport{Play: r}
	tests := []struct {
		expr string
		pass bool
	}{
		{"sync.max_drift_ms<=20", true},
		{"sync.max_drift_ms<=10", false},
		{"sync.avg_drift_ms<10", true},
	}
	for _, tt := range tests {
		result, err := EvalAssert(top, tt.expr)
		if err != nil {
			t.Fatalf("EvalAssert(%q): %v", tt.expr, err)
		}
		if result != tt.pass {
			t.Errorf("EvalAssert(%q) = %v, want %v", tt.expr, result, tt.pass)
		}
	}
}

func TestAssertPushFields(t *testing.T) {
	top := &TopLevelReport{Push: &PushReport{
		Protocol:   "rtmp",
		FramesSent: 1000,
		BytesSent:  500000,
	}}
	tests := []struct {
		expr string
		pass bool
	}{
		{"push.protocol==rtmp", true},
		{"push.frames_sent>=1000", true},
		{"push.bytes_sent>=500000", true},
	}
	for _, tt := range tests {
		result, err := EvalAssert(top, tt.expr)
		if err != nil {
			t.Fatalf("EvalAssert(%q): %v", tt.expr, err)
		}
		if result != tt.pass {
			t.Errorf("EvalAssert(%q) = %v, want %v", tt.expr, result, tt.pass)
		}
	}
}

func TestAssertUnknownField(t *testing.T) {
	top := &TopLevelReport{Play: &PlayReport{}}
	_, err := EvalAssert(top, "video.nonexistent>=0")
	if err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestAssertInvalidExpression(t *testing.T) {
	top := &TopLevelReport{Play: &PlayReport{}}
	_, err := EvalAssert(top, "invalid")
	if err == nil {
		t.Error("expected error for invalid expression")
	}
}

func TestAssertUnsupportedOperator(t *testing.T) {
	top := &TopLevelReport{Play: &PlayReport{Video: VideoReport{Codec: "H264"}}}
	_, err := EvalAssert(top, "video.codec>=H264")
	if err == nil {
		t.Error("expected error for >= on string field")
	}
}

func TestEvalAssertions(t *testing.T) {
	r := &PlayReport{
		Video: VideoReport{FPS: 29.97, DTSMonotonic: true, Codec: "H264"},
	}
	top := &TopLevelReport{Play: r}
	exprs := []string{
		"video.fps>=29",
		"video.dts_monotonic==true",
		"video.codec==H264",
		"video.fps>=60",
	}
	results, allPass := EvalAssertions(top, exprs)
	if allPass {
		t.Error("expected allPass=false because video.fps>=60 should fail")
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	if !results[0].Pass {
		t.Error("results[0] should pass")
	}
	if !results[1].Pass {
		t.Error("results[1] should pass")
	}
	if !results[2].Pass {
		t.Error("results[2] should pass")
	}
	if results[3].Pass {
		t.Error("results[3] should fail")
	}
}

func TestAssertNilSubReport(t *testing.T) {
	top := &TopLevelReport{}
	_, err := EvalAssert(top, "video.fps>=0")
	if err == nil {
		t.Error("expected error when Play is nil and accessing video fields")
	}
}

func TestAssertClusterFields(t *testing.T) {
	top := &TopLevelReport{
		Cluster: &ClusterReport{
			Topology: "origin-edge",
			RelayMs:  150,
		},
	}
	tests := []struct {
		expr string
		pass bool
	}{
		{"cluster.relay_ms<=200", true},
		{"cluster.relay_ms<100", false},
		{"cluster.topology==origin-edge", true},
	}
	for _, tt := range tests {
		result, err := EvalAssert(top, tt.expr)
		if err != nil {
			t.Fatalf("EvalAssert(%q): %v", tt.expr, err)
		}
		if result != tt.pass {
			t.Errorf("EvalAssert(%q) = %v, want %v", tt.expr, result, tt.pass)
		}
	}
}
