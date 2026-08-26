// module/cluster/transport.go
package cluster

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/prometheus/client_golang/prometheus"
)

// ErrCodecMismatch is returned when remote node rejects all offered codecs.
// This error is non-retryable.
var ErrCodecMismatch = errors.New("codec mismatch: remote rejected all offered codecs")

type relayObservationContextKey struct{}

type relayObservation struct {
	started      time.Time
	metrics      *RelayMetrics
	bytesTotal   prometheus.Counter
	direction    string
	protocol     string
	connected    atomic.Bool
	firstFlush   atomic.Bool
	pendingBytes atomic.Int64
}

const relayMetricsFlushBytes int64 = 64 * 1024

func observeRelay(ctx context.Context, metrics *RelayMetrics, direction, protocol string) context.Context {
	observation := &relayObservation{
		started:    time.Now(),
		metrics:    metrics,
		bytesTotal: metrics.bytesCounter(direction, protocol),
		direction:  direction,
		protocol:   protocol,
	}
	return context.WithValue(ctx, relayObservationContextKey{}, observation)
}

func recordRelayBytes(ctx context.Context, count int64) {
	if count <= 0 {
		return
	}
	if observation, ok := ctx.Value(relayObservationContextKey{}).(*relayObservation); ok {
		observation.recordBytes(count)
	}
}

func (o *relayObservation) recordBytes(count int64) {
	if o == nil || o.bytesTotal == nil || count <= 0 {
		return
	}
	if o.firstFlush.CompareAndSwap(false, true) {
		o.bytesTotal.Add(float64(count))
		return
	}
	if o.pendingBytes.Add(count) >= relayMetricsFlushBytes {
		o.flushBytes()
	}
}

func (o *relayObservation) flushBytes() {
	if o == nil || o.bytesTotal == nil {
		return
	}
	if pending := o.pendingBytes.Swap(0); pending > 0 {
		o.bytesTotal.Add(float64(pending))
	}
}

func flushRelayBytes(ctx context.Context) {
	if observation, ok := ctx.Value(relayObservationContextKey{}).(*relayObservation); ok {
		observation.flushBytes()
	}
}

func markRelayConnected(ctx context.Context) {
	observation, ok := ctx.Value(relayObservationContextKey{}).(*relayObservation)
	if !ok || !observation.connected.CompareAndSwap(false, true) {
		return
	}
	observation.metrics.RecordLatency(observation.protocol, time.Since(observation.started).Seconds())
}

func closeOnContextDone(ctx context.Context, closer io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = closer.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

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
