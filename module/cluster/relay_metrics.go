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
}

// NewRelayMetrics creates relay metrics registered with the default registerer.
func NewRelayMetrics() *RelayMetrics {
	m := newRelayMetrics()
	prometheus.MustRegister(m)
	return m
}

// NewRelayMetricsWithRegistry creates relay metrics registered with a custom
// registry, which is useful for test isolation.
func NewRelayMetricsWithRegistry(reg *prometheus.Registry) *RelayMetrics {
	m := newRelayMetrics()
	reg.MustRegister(m)
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
		}, []string{"direction"}),
	}
}

// Describe implements prometheus.Collector.
func (m *RelayMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.active.Describe(ch)
	m.errors.Describe(ch)
	m.bytesTotal.Describe(ch)
	m.latency.Describe(ch)
	m.packetLoss.Describe(ch)
}

// Collect implements prometheus.Collector.
func (m *RelayMetrics) Collect(ch chan<- prometheus.Metric) {
	m.active.Collect(ch)
	m.errors.Collect(ch)
	m.bytesTotal.Collect(ch)
	m.latency.Collect(ch)
	m.packetLoss.Collect(ch)
}

// SetActive sets the number of active relay connections for a given direction
// and protocol.
func (m *RelayMetrics) SetActive(direction, protocol string, count float64) {
	m.active.WithLabelValues(direction, protocol).Set(count)
}

// RelayStarted increments the number of active relay attempts.
func (m *RelayMetrics) RelayStarted(direction, protocol string) {
	if m == nil {
		return
	}
	m.active.WithLabelValues(direction, protocol).Inc()
}

// RelayStopped decrements the number of active relay attempts.
func (m *RelayMetrics) RelayStopped(direction, protocol string) {
	if m == nil {
		return
	}
	m.active.WithLabelValues(direction, protocol).Dec()
}

// RecordPush records bytes forwarded and any errors for a push (forward)
// operation.
func (m *RelayMetrics) RecordPush(protocol string, bytes int64, err error) {
	if m == nil {
		return
	}
	m.bytesTotal.WithLabelValues("forward", protocol).Add(float64(bytes))
	if err != nil {
		m.errors.WithLabelValues("forward", protocol, "connection").Inc()
	}
}

// RecordPull records bytes received and any errors for a pull (origin)
// operation.
func (m *RelayMetrics) RecordPull(protocol string, bytes int64, err error) {
	if m == nil {
		return
	}
	m.bytesTotal.WithLabelValues("origin", protocol).Add(float64(bytes))
	if err != nil {
		m.errors.WithLabelValues("origin", protocol, "connection").Inc()
	}
}

// RecordLatency records a relay latency observation for the given protocol.
func (m *RelayMetrics) RecordLatency(protocol string, seconds float64) {
	if m == nil {
		return
	}
	m.latency.WithLabelValues(protocol).Observe(seconds)
}

// RecordPacketLoss records the latest packet loss ratio for a direction. The
// stream argument is intentionally excluded from labels to bound cardinality.
func (m *RelayMetrics) RecordPacketLoss(_ string, direction string, ratio float64) {
	if m == nil {
		return
	}
	m.packetLoss.WithLabelValues(direction).Set(ratio)
}
