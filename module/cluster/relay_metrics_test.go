package cluster

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRelayMetricsIncrement(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRelayMetricsWithRegistry(reg)

	m.RecordPush("rtmp", 1024, nil)
	m.RecordPush("srt", 512, fmt.Errorf("connection refused"))

	expectedBytes := `
		# HELP cluster_relay_bytes_total Total bytes relayed.
		# TYPE cluster_relay_bytes_total counter
		cluster_relay_bytes_total{direction="forward",protocol="rtmp"} 1024
		cluster_relay_bytes_total{direction="forward",protocol="srt"} 512
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedBytes), "cluster_relay_bytes_total"); err != nil {
		t.Errorf("bytes mismatch: %v", err)
	}

	expectedErr := `
		# HELP cluster_relay_errors_total Total relay errors.
		# TYPE cluster_relay_errors_total counter
		cluster_relay_errors_total{direction="forward",error_type="connection",protocol="srt"} 1
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedErr), "cluster_relay_errors_total"); err != nil {
		t.Errorf("errors mismatch: %v", err)
	}
}

func TestRelayMetricsActive(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRelayMetricsWithRegistry(reg)

	m.SetActive("forward", "rtmp", 2)
	m.SetActive("origin", "srt", 1)

	expected := `
		# HELP cluster_relay_active Number of active relay connections.
		# TYPE cluster_relay_active gauge
		cluster_relay_active{direction="forward",protocol="rtmp"} 2
		cluster_relay_active{direction="origin",protocol="srt"} 1
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "cluster_relay_active"); err != nil {
		t.Errorf("active mismatch: %v", err)
	}
}

func TestRelayMetricsPull(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRelayMetricsWithRegistry(reg)

	m.RecordPull("rtmp", 2048, nil)
	m.RecordPull("srt", 256, fmt.Errorf("timeout"))

	expectedBytes := `
		# HELP cluster_relay_bytes_total Total bytes relayed.
		# TYPE cluster_relay_bytes_total counter
		cluster_relay_bytes_total{direction="origin",protocol="rtmp"} 2048
		cluster_relay_bytes_total{direction="origin",protocol="srt"} 256
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedBytes), "cluster_relay_bytes_total"); err != nil {
		t.Errorf("bytes mismatch: %v", err)
	}

	expectedErr := `
		# HELP cluster_relay_errors_total Total relay errors.
		# TYPE cluster_relay_errors_total counter
		cluster_relay_errors_total{direction="origin",error_type="connection",protocol="srt"} 1
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedErr), "cluster_relay_errors_total"); err != nil {
		t.Errorf("errors mismatch: %v", err)
	}
}

func TestRelayMetricsLatency(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRelayMetricsWithRegistry(reg)

	m.RecordLatency("rtp", 0.005)

	// Verify the histogram has exactly one observation with the correct sum.
	count := testutil.CollectAndCount(m.latency)
	if count != 1 {
		t.Errorf("expected 1 metric series, got %d", count)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "cluster_relay_latency_seconds" {
			for _, met := range f.GetMetric() {
				h := met.GetHistogram()
				if h.GetSampleCount() != 1 {
					t.Errorf("expected 1 sample, got %d", h.GetSampleCount())
				}
				if h.GetSampleSum() != 0.005 {
					t.Errorf("expected sum 0.005, got %v", h.GetSampleSum())
				}
			}
		}
	}
}

func TestRelayMetricsPacketLoss(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRelayMetricsWithRegistry(reg)

	m.RecordPacketLoss("live/test", "forward", 0.02)

	expected := `
		# HELP cluster_rtp_packet_loss_ratio RTP packet loss ratio.
		# TYPE cluster_rtp_packet_loss_ratio gauge
		cluster_rtp_packet_loss_ratio{direction="forward",stream="live/test"} 0.02
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "cluster_rtp_packet_loss_ratio"); err != nil {
		t.Errorf("packet loss mismatch: %v", err)
	}
}
