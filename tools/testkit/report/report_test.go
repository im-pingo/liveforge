package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newTestPlayReport() *PlayReport {
	return &PlayReport{
		Video: VideoReport{
			Codec:            "H264",
			Profile:          "Baseline",
			Level:            "3.0",
			Resolution:       "640x360",
			FPS:              29.97,
			BitrateKbps:      800.5,
			KeyframeInterval: 2.0,
			DTSMonotonic:     true,
			FrameCount:       900,
		},
		Audio: AudioReport{
			Codec:        "AAC",
			SampleRate:   44100,
			Channels:     2,
			BitrateKbps:  128.0,
			DTSMonotonic: true,
			FrameCount:   430,
		},
		Sync: SyncReport{
			MaxDriftMs: 12.5,
			AvgDriftMs: 3.2,
		},
		Stalls: []StallEvent{
			{TimestampMs: 1500, GapMs: 200.0, MediaType: "video"},
		},
		Codec: CodecReport{
			VideoMatch: true,
			AudioMatch: true,
		},
		DurationMs: 5000,
	}
}

func newTestTopLevelReport() *TopLevelReport {
	return &TopLevelReport{
		Command:    "play",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 5000,
		Pass:       true,
		Play:       newTestPlayReport(),
	}
}

func TestFormatJSON_RoundTrip(t *testing.T) {
	original := newTestTopLevelReport()

	data, err := FormatJSON(original)
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var decoded TopLevelReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Verify key fields survived round-trip.
	if decoded.Command != original.Command {
		t.Errorf("Command: got %q, want %q", decoded.Command, original.Command)
	}
	if decoded.Pass != original.Pass {
		t.Errorf("Pass: got %v, want %v", decoded.Pass, original.Pass)
	}
	if decoded.DurationMs != original.DurationMs {
		t.Errorf("DurationMs: got %d, want %d", decoded.DurationMs, original.DurationMs)
	}
	if decoded.Play == nil {
		t.Fatal("Play is nil after round-trip")
	}
	if decoded.Play.Video.FPS != original.Play.Video.FPS {
		t.Errorf("Video.FPS: got %f, want %f", decoded.Play.Video.FPS, original.Play.Video.FPS)
	}
	if decoded.Play.Video.Codec != original.Play.Video.Codec {
		t.Errorf("Video.Codec: got %q, want %q", decoded.Play.Video.Codec, original.Play.Video.Codec)
	}
	if decoded.Play.Video.DTSMonotonic != original.Play.Video.DTSMonotonic {
		t.Errorf("Video.DTSMonotonic: got %v, want %v", decoded.Play.Video.DTSMonotonic, original.Play.Video.DTSMonotonic)
	}
	if decoded.Play.Audio.SampleRate != original.Play.Audio.SampleRate {
		t.Errorf("Audio.SampleRate: got %d, want %d", decoded.Play.Audio.SampleRate, original.Play.Audio.SampleRate)
	}
	if decoded.Play.Sync.MaxDriftMs != original.Play.Sync.MaxDriftMs {
		t.Errorf("Sync.MaxDriftMs: got %f, want %f", decoded.Play.Sync.MaxDriftMs, original.Play.Sync.MaxDriftMs)
	}
	if len(decoded.Play.Stalls) != len(original.Play.Stalls) {
		t.Errorf("Stalls count: got %d, want %d", len(decoded.Play.Stalls), len(original.Play.Stalls))
	}
}

func TestFormatJSON_PushReport(t *testing.T) {
	report := &TopLevelReport{
		Command:    "push",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 3000,
		Pass:       true,
		Push: &PushReport{
			Protocol:   "rtmp",
			Target:     "rtmp://localhost/live/test",
			DurationMs: 3000,
			FramesSent: 90,
			BytesSent:  150000,
		},
	}

	data, err := FormatJSON(report)
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var decoded TopLevelReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.Push == nil {
		t.Fatal("Push is nil after round-trip")
	}
	if decoded.Push.Protocol != "rtmp" {
		t.Errorf("Push.Protocol: got %q, want %q", decoded.Push.Protocol, "rtmp")
	}
	if decoded.Push.FramesSent != 90 {
		t.Errorf("Push.FramesSent: got %d, want %d", decoded.Push.FramesSent, 90)
	}
}

func TestFormatJSON_AuthReport(t *testing.T) {
	report := &TopLevelReport{
		Command:    "auth",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 1000,
		Pass:       true,
		Auth: &AuthReport{
			Total:  2,
			Passed: 2,
			Failed: 0,
			Cases: []CaseResult{
				{Protocol: "rtmp", Action: "push", Credential: "valid", ExpectAllow: true, ActualAllow: true, Pass: true, LatencyMs: 50},
				{Protocol: "rtmp", Action: "push", Credential: "invalid", ExpectAllow: false, ActualAllow: false, Pass: true, LatencyMs: 30},
			},
		},
	}

	data, err := FormatJSON(report)
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var decoded TopLevelReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.Auth == nil {
		t.Fatal("Auth is nil after round-trip")
	}
	if decoded.Auth.Total != 2 {
		t.Errorf("Auth.Total: got %d, want %d", decoded.Auth.Total, 2)
	}
	if len(decoded.Auth.Cases) != 2 {
		t.Fatalf("Auth.Cases count: got %d, want %d", len(decoded.Auth.Cases), 2)
	}
	if decoded.Auth.Cases[0].Protocol != "rtmp" {
		t.Errorf("Auth.Cases[0].Protocol: got %q, want %q", decoded.Auth.Cases[0].Protocol, "rtmp")
	}
}

func TestFormatJSON_ClusterReport(t *testing.T) {
	report := &TopLevelReport{
		Command:    "cluster",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 8000,
		Pass:       true,
		Cluster: &ClusterReport{
			Topology: "origin-edge",
			Nodes: []NodeStatus{
				{Name: "origin", Role: "origin", Healthy: true, Ports: map[string]int{"rtmp": 1935}},
				{Name: "edge-1", Role: "edge", Healthy: true, Ports: map[string]int{"rtmp": 1936}},
			},
			RelayMs: 150,
		},
	}

	data, err := FormatJSON(report)
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var decoded TopLevelReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.Cluster == nil {
		t.Fatal("Cluster is nil after round-trip")
	}
	if decoded.Cluster.Topology != "origin-edge" {
		t.Errorf("Cluster.Topology: got %q, want %q", decoded.Cluster.Topology, "origin-edge")
	}
	if len(decoded.Cluster.Nodes) != 2 {
		t.Fatalf("Cluster.Nodes count: got %d, want %d", len(decoded.Cluster.Nodes), 2)
	}
}

func TestFormatJSON_ErrorDetails(t *testing.T) {
	report := &TopLevelReport{
		Command:    "play",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 500,
		Pass:       false,
		Errors: []ErrorDetail{
			{Code: "CONNECT_FAILED", Message: "connection refused"},
			{Code: "TIMEOUT", Message: "timed out after 5s"},
		},
	}

	data, err := FormatJSON(report)
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var decoded TopLevelReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if len(decoded.Errors) != 2 {
		t.Fatalf("Errors count: got %d, want %d", len(decoded.Errors), 2)
	}
	if decoded.Errors[0].Code != "CONNECT_FAILED" {
		t.Errorf("Errors[0].Code: got %q, want %q", decoded.Errors[0].Code, "CONNECT_FAILED")
	}
}

func TestFormatJSON_OmitsNilSubReports(t *testing.T) {
	report := &TopLevelReport{
		Command:    "play",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 1000,
		Pass:       true,
		Play:       newTestPlayReport(),
	}

	data, err := FormatJSON(report)
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, `"auth"`) {
		t.Error("JSON should not contain auth key when Auth is nil")
	}
	if strings.Contains(jsonStr, `"cluster"`) {
		t.Error("JSON should not contain cluster key when Cluster is nil")
	}
	if strings.Contains(jsonStr, `"push"`) {
		t.Error("JSON should not contain push key when Push is nil")
	}
}

func TestFormatHuman_PlayReport(t *testing.T) {
	report := newTestTopLevelReport()
	output := FormatHuman(report)

	// Verify key sections are present.
	requiredStrings := []string{
		"lf-test: play [PASS]",
		"Video",
		"H264",
		"640x360",
		"29.97",
		"Audio",
		"AAC",
		"44100",
		"AV Sync",
		"12.50 ms",
		"Stalls (1)",
	}
	for _, s := range requiredStrings {
		if !strings.Contains(output, s) {
			t.Errorf("human output missing %q", s)
		}
	}
}

func TestFormatHuman_PushReport(t *testing.T) {
	report := &TopLevelReport{
		Command:    "push",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 3000,
		Pass:       true,
		Push: &PushReport{
			Protocol:   "rtmp",
			Target:     "rtmp://localhost/live/test",
			DurationMs: 3000,
			FramesSent: 90,
			BytesSent:  150000,
		},
	}
	output := FormatHuman(report)

	requiredStrings := []string{
		"lf-test: push [PASS]",
		"Push",
		"rtmp",
		"90",
		"150000",
	}
	for _, s := range requiredStrings {
		if !strings.Contains(output, s) {
			t.Errorf("human output missing %q", s)
		}
	}
}

func TestFormatHuman_FailStatus(t *testing.T) {
	report := &TopLevelReport{
		Command:    "play",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 500,
		Pass:       false,
		Errors: []ErrorDetail{
			{Code: "CONNECT_FAILED", Message: "connection refused"},
		},
	}
	output := FormatHuman(report)

	if !strings.Contains(output, "[FAIL]") {
		t.Error("human output missing [FAIL] status")
	}
	if !strings.Contains(output, "CONNECT_FAILED") {
		t.Error("human output missing error code")
	}
}

func TestFormatHuman_AuthReport(t *testing.T) {
	report := &TopLevelReport{
		Command:    "auth",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 1000,
		Pass:       true,
		Auth: &AuthReport{
			Total:  1,
			Passed: 1,
			Failed: 0,
			Cases: []CaseResult{
				{Protocol: "rtmp", Action: "push", Credential: "valid", ExpectAllow: true, ActualAllow: true, Pass: true, LatencyMs: 50},
			},
		},
	}
	output := FormatHuman(report)

	if !strings.Contains(output, "Auth (1/1 passed)") {
		t.Error("human output missing auth summary")
	}
	if !strings.Contains(output, "[PASS] rtmp/push") {
		t.Error("human output missing auth case details")
	}
}

func TestFormatHuman_ClusterReport(t *testing.T) {
	report := &TopLevelReport{
		Command:    "cluster",
		Timestamp:  time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		DurationMs: 8000,
		Pass:       true,
		Cluster: &ClusterReport{
			Topology: "origin-edge",
			Nodes: []NodeStatus{
				{Name: "origin", Role: "origin", Healthy: true, Ports: map[string]int{"rtmp": 1935}},
			},
			RelayMs: 150,
		},
	}
	output := FormatHuman(report)

	if !strings.Contains(output, "Cluster (origin-edge)") {
		t.Error("human output missing cluster topology")
	}
	if !strings.Contains(output, "origin (origin)") {
		t.Error("human output missing node info")
	}
	if !strings.Contains(output, "150ms") {
		t.Error("human output missing relay latency")
	}
}
