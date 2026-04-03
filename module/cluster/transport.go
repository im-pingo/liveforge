// module/cluster/transport.go
package cluster

import (
	"context"
	"errors"

	"github.com/im-pingo/liveforge/core"
)

// ErrCodecMismatch is returned when remote node rejects all offered codecs.
// This error is non-retryable.
var ErrCodecMismatch = errors.New("codec mismatch: remote rejected all offered codecs")

// RelayTransport is the plugin interface for cluster relay protocols.
// Each protocol (RTMP, SRT, RTSP, RTP) implements this interface and
// registers with the TransportRegistry.
type RelayTransport interface {
	// Scheme returns the URL scheme this transport handles ("rtmp", "srt", "rtsp", "rtp").
	Scheme() string

	// Push connects to a remote node and pushes frames from a local stream.
	// Returns nil on normal termination (stream ended, context cancelled).
	// Returns error on abnormal disconnection (network error, protocol error).
	// Callers use errors.Is(err, ErrCodecMismatch) to detect non-retryable errors.
	Push(ctx context.Context, targetURL string, stream *core.Stream) error

	// Pull connects to a remote node and pulls frames into a local stream.
	// stream.WriteFrame() returning false (bitrate-limited) is silently dropped.
	// Returns nil on normal termination, error on abnormal disconnection.
	Pull(ctx context.Context, sourceURL string, stream *core.Stream) error

	// Close releases any resources held by this transport.
	Close() error
}
