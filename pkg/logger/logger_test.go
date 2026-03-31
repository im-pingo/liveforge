package logger

import (
	"log/slog"
	"testing"
)

func TestInitSetsLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}

	for _, tt := range tests {
		Init(tt.input)
		if !slog.Default().Enabled(nil, tt.want) {
			t.Errorf("Init(%q): expected level %v to be enabled", tt.input, tt.want)
		}
	}
}
