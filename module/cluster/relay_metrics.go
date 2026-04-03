// module/cluster/relay_metrics.go
package cluster

import (
	"github.com/prometheus/client_golang/prometheus"
)

// RelayMetrics holds Prometheus metrics for cluster relay transports.
type RelayMetrics struct {
	active     *prometheus.GaugeVec
	errors     *prometheus.CounterVec
	bytesTotal *prometheus.CounterVec
	latency    *prometheus.HistogramVec
	packetLoss *prometheus.GaugeVec
	registry   *prometheus.Registry
}

// NewRelayMetrics creates relay metrics registered with the default registerer.
func NewRelayMetrics() *RelayMetrics {
	m := newRelayMetrics()
	prometheus.MustRegister(m.active, m.errors, m.bytesTotal, m.latency, m.packetLoss)
	return m
}

// NewRelayMetricsWithRegistry creates relay metrics registered with a custom
// registry, which is useful for test isolation.
func NewRelayMetricsWithRegistry(reg *prometheus.Registry) *RelayMetrics {
	m := newRelayMetrics()
	m.registry = reg
	reg.MustRegister(m.active, m.errors, m.bytesTotal, m.latency, m.packetLoss)
	return m
}

func newRelayMetrics() *RelayMetrics {
	return &RelayMetrics{
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cluster_relay_active",
			Help: "Number of active relay connections.",
		}, []string{"direction", "protocol"}),

		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cluster_relay_errors_total",
			Help: "Total relay errors.",
		}, []string{"direction", "protocol", "error_type"}),

		bytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cluster_relay_bytes_total",
			Help: "Total bytes relayed.",
		}, []string{"direction", "protocol"}),

		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cluster_relay_latency_seconds",
			Help:    "Relay latency in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"protocol"}),

		packetLoss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cluster_rtp_packet_loss_ratio",
			Help: "RTP packet loss ratio.",
		}, []string{"stream", "direction"}),
	}
}

// SetActive sets the number of active relay connections for a given direction
// and protocol.
func (m *RelayMetrics) SetActive(direction, protocol string, count float64) {
	m.active.WithLabelValues(direction, protocol).Set(count)
}

// RecordPush records bytes forwarded and any errors for a push (forward)
// operation.
func (m *RelayMetrics) RecordPush(protocol string, bytes int64, err error) {
	m.bytesTotal.WithLabelValues("forward", protocol).Add(float64(bytes))
	if err != nil {
		m.errors.WithLabelValues("forward", protocol, "connection").Inc()
	}
}

// RecordPull records bytes received and any errors for a pull (origin)
// operation.
func (m *RelayMetrics) RecordPull(protocol string, bytes int64, err error) {
	m.bytesTotal.WithLabelValues("origin", protocol).Add(float64(bytes))
	if err != nil {
		m.errors.WithLabelValues("origin", protocol, "connection").Inc()
	}
}

// RecordLatency records a relay latency observation for the given protocol.
func (m *RelayMetrics) RecordLatency(protocol string, seconds float64) {
	m.latency.WithLabelValues(protocol).Observe(seconds)
}

// RecordPacketLoss records the packet loss ratio for a stream and direction.
func (m *RelayMetrics) RecordPacketLoss(stream, direction string, ratio float64) {
	m.packetLoss.WithLabelValues(stream, direction).Set(ratio)
}
