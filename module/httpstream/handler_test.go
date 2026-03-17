package httpstream

import "testing"

func TestParseStreamPath(t *testing.T) {
	tests := []struct {
		path   string
		app    string
		key    string
		format string
		ok     bool
	}{
		{"/live/test.flv", "live", "test", "flv", true},
		{"/live/test.ts", "live", "test", "ts", true},
		{"/app/stream.mp4", "app", "stream", "mp4", true},
		{"/live/multi/part.flv", "live", "multi/part", "flv", true},
		{"/noext", "", "", "", false},
		{"/", "", "", "", false},
		{"/.flv", "", "", "", false},
	}
	for _, tt := range tests {
		app, key, format, ok := parseStreamPath(tt.path)
		if ok != tt.ok || app != tt.app || key != tt.key || format != tt.format {
			t.Errorf("parseStreamPath(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tt.path, app, key, format, ok, tt.app, tt.key, tt.format, tt.ok)
		}
	}
}
