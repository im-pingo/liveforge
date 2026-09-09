package config

import "testing"

func TestValidateSimulcastConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   SimulcastConfig
		valid bool
	}{
		{"disabled", SimulcastConfig{}, true},
		{"enabled empty", SimulcastConfig{Enabled: true}, false},
		{"valid", SimulcastConfig{Enabled: true, Layers: []LayerConfig{{RID: "h", MaxBitrate: 2500000}, {RID: "l", MaxBitrate: 500000}}}, true},
		{"duplicate", SimulcastConfig{Layers: []LayerConfig{{RID: "h", MaxBitrate: 1}, {RID: "h", MaxBitrate: 2}}}, false},
		{"unbounded", SimulcastConfig{Layers: []LayerConfig{{RID: "a", MaxBitrate: 1}, {RID: "b", MaxBitrate: 1}, {RID: "c", MaxBitrate: 1}, {RID: "d", MaxBitrate: 1}}}, false},
		{"reserved", SimulcastConfig{Layers: []LayerConfig{{RID: "auto", MaxBitrate: 1}}}, false},
		{"bad rid", SimulcastConfig{Layers: []LayerConfig{{RID: "a/b", MaxBitrate: 1}}}, false},
		{"long rid", SimulcastConfig{Layers: []LayerConfig{{RID: "abcdefghijklmnopq", MaxBitrate: 1}}}, false},
		{"zero bitrate", SimulcastConfig{Layers: []LayerConfig{{RID: "h"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSimulcastConfig(tc.cfg); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
}
