// Package push provides protocol-specific media push (publish) clients for
// integration testing. Each protocol implements the Pusher interface, which
// reads frames from a source.Source, publishes them to a target server, and
// returns a report.PushReport with statistics.
package push

import (
	"context"
	"fmt"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

// PushConfig describes the target and constraints for a push session.
type PushConfig struct {
	Protocol string        // e.g. "rtmp", "rtsp", "srt"
	Target   string        // e.g. "rtmp://127.0.0.1:1935/live/test"
	Duration time.Duration // maximum push duration; 0 = until source exhausted
	Token    string        // optional auth token
}

// Pusher publishes media frames to a remote server.
type Pusher interface {
	Push(ctx context.Context, src source.Source, cfg PushConfig) (*report.PushReport, error)
}

// NewPusher returns a Pusher for the given protocol.
// Supported protocols: "rtmp", "rtsp".
func NewPusher(protocol string) (Pusher, error) {
	switch protocol {
	case "rtmp":
		return &rtmpPusher{}, nil
	case "rtsp":
		return &rtspPusher{}, nil
	default:
		return nil, fmt.Errorf("unsupported push protocol: %q", protocol)
	}
}
