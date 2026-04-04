package gb28181

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestBuildPTZCmd(t *testing.T) {
	tests := []struct {
		name      string
		direction PTZDirection
		zoom      PTZZoom
		hSpeed    uint8
		vSpeed    uint8
	}{
		{"stop", PTZStop, PTZZoomStop, 0, 0},
		{"up", PTZUp, PTZZoomStop, 0, 0x1F},
		{"down", PTZDown, PTZZoomStop, 0, 0x1F},
		{"left", PTZLeft, PTZZoomStop, 0x1F, 0},
		{"right", PTZRight, PTZZoomStop, 0x1F, 0},
		{"zoom_in", PTZStop, PTZZoomIn, 0, 0},
		{"zoom_out", PTZStop, PTZZoomOut, 0, 0},
		{"up_left", PTZUpLeft, PTZZoomStop, 0x1F, 0x1F},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := BuildPTZCmd(tt.direction, tt.zoom, tt.hSpeed, tt.vSpeed)

			// Should be 16 hex chars (8 bytes)
			if len(cmd) != 16 {
				t.Fatalf("cmd length = %d, want 16", len(cmd))
			}

			// Should be valid hex
			bytes, err := hex.DecodeString(cmd)
			if err != nil {
				t.Fatalf("invalid hex: %v", err)
			}

			// First two bytes should be A5 0F
			if bytes[0] != 0xA5 || bytes[1] != 0x0F {
				t.Errorf("prefix = %02X%02X, want A50F", bytes[0], bytes[1])
			}

			// Verify checksum
			var sum byte
			for i := 0; i < 7; i++ {
				sum += bytes[i]
			}
			if bytes[7] != sum {
				t.Errorf("checksum = %02X, want %02X", bytes[7], sum)
			}
		})
	}
}

func TestBuildPTZStopCmd(t *testing.T) {
	cmd := BuildPTZStopCmd()
	if !strings.HasPrefix(cmd, "A50F") {
		t.Errorf("stop cmd = %q, want A50F prefix", cmd)
	}

	bytes, _ := hex.DecodeString(cmd)
	if bytes[3] != 0 || bytes[4] != 0 || bytes[5] != 0 || bytes[6] != 0 {
		t.Errorf("stop cmd should have zero direction/zoom/speed")
	}
}
