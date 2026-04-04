// Package play provides protocol-specific media play (subscribe) clients for
// integration testing. Each protocol implements the Player interface, which
// connects to a server, reads media frames, and delivers them to a
// FrameCallback for analysis.
package play

import (
	"context"
	"fmt"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// PlayConfig describes the target and constraints for a play session.
type PlayConfig struct {
	Protocol string        // e.g. "rtmp", "rtsp", "srt"
	URL      string        // e.g. "rtmp://127.0.0.1:1935/live/test"
	Duration time.Duration // maximum play duration; 0 = until server closes
	Token    string        // optional auth token
}

// FrameCallback is called for each received media frame.
type FrameCallback func(frame *avframe.AVFrame)

// Player subscribes to a media stream and delivers frames via a callback.
type Player interface {
	Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error
}

// NewPlayer returns a Player for the given protocol.
// Supported protocols: "rtmp".
func NewPlayer(protocol string) (Player, error) {
	switch protocol {
	case "rtmp":
		return &rtmpPlayer{}, nil
	default:
		return nil, fmt.Errorf("unsupported play protocol: %q", protocol)
	}
}
