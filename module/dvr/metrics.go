package dvr

import "github.com/prometheus/client_golang/prometheus"

type prometheusCollector struct {
	module          *Module
	active          *prometheus.Desc
	segmentsWritten *prometheus.Desc
	segmentBytes    *prometheus.Desc
	writeRetries    *prometheus.Desc
	writeFailures   *prometheus.Desc
	cleanupDeleted  *prometheus.Desc
	cleanupBytes    *prometheus.Desc
	cleanupFailures *prometheus.Desc
}

func newPrometheusCollector(module *Module) *prometheusCollector {
	return &prometheusCollector{
		module:          module,
		active:          prometheus.NewDesc("liveforge_dvr_sessions_active", "Current live DVR sessions.", nil, nil),
		segmentsWritten: prometheus.NewDesc("liveforge_dvr_segments_written_total", "Total DVR segments finalized successfully.", nil, nil),
		segmentBytes:    prometheus.NewDesc("liveforge_dvr_segment_bytes_total", "Total bytes finalized into DVR segments.", nil, nil),
		writeRetries:    prometheus.NewDesc("liveforge_dvr_write_retries_total", "Total DVR segment write retries.", nil, nil),
		writeFailures:   prometheus.NewDesc("liveforge_dvr_write_failures_total", "Total DVR segment write failures.", nil, nil),
		cleanupDeleted:  prometheus.NewDesc("liveforge_dvr_cleanup_deleted_total", "Total expired DVR segments deleted.", nil, nil),
		cleanupBytes:    prometheus.NewDesc("liveforge_dvr_cleanup_bytes_total", "Total bytes removed by DVR cleanup.", nil, nil),
		cleanupFailures: prometheus.NewDesc("liveforge_dvr_cleanup_failures_total", "Total DVR segment cleanup failures.", nil, nil),
	}
}

func (c *prometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.active
	ch <- c.segmentsWritten
	ch <- c.segmentBytes
	ch <- c.writeRetries
	ch <- c.writeFailures
	ch <- c.cleanupDeleted
	ch <- c.cleanupBytes
	ch <- c.cleanupFailures
}

func (c *prometheusCollector) Collect(ch chan<- prometheus.Metric) {
	c.module.mu.Lock()
	active := 0
	for _, session := range c.module.sessions {
		if session.IsLive() {
			active++
		}
	}
	c.module.mu.Unlock()
	metrics := c.module.metrics.Snapshot()
	ch <- prometheus.MustNewConstMetric(c.active, prometheus.GaugeValue, float64(active))
	ch <- prometheus.MustNewConstMetric(c.segmentsWritten, prometheus.CounterValue, float64(metrics.SegmentsWritten))
	ch <- prometheus.MustNewConstMetric(c.segmentBytes, prometheus.CounterValue, float64(metrics.SegmentBytes))
	ch <- prometheus.MustNewConstMetric(c.writeRetries, prometheus.CounterValue, float64(metrics.WriteRetries))
	ch <- prometheus.MustNewConstMetric(c.writeFailures, prometheus.CounterValue, float64(metrics.WriteFailures))
	ch <- prometheus.MustNewConstMetric(c.cleanupDeleted, prometheus.CounterValue, float64(metrics.CleanupDeleted))
	ch <- prometheus.MustNewConstMetric(c.cleanupBytes, prometheus.CounterValue, float64(metrics.CleanupBytes))
	ch <- prometheus.MustNewConstMetric(c.cleanupFailures, prometheus.CounterValue, float64(metrics.CleanupFailures))
}

// PrometheusCollectors exposes fixed-label DVR metrics to the metrics module.
func (m *Module) PrometheusCollectors() []prometheus.Collector {
	return []prometheus.Collector{newPrometheusCollector(m)}
}

var _ DVRStatusProvider = (*Module)(nil)
