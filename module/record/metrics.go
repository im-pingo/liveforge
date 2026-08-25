package record

import "github.com/prometheus/client_golang/prometheus"

type prometheusCollector struct {
	module        *Module
	active        *prometheus.Desc
	completed     *prometheus.Desc
	failed        *prometheus.Desc
	retries       *prometheus.Desc
	writeFailures *prometheus.Desc
	deleted       *prometheus.Desc
	bytesWritten  *prometheus.Desc
	storageErrors *prometheus.Desc
}

func newPrometheusCollector(module *Module) *prometheusCollector {
	return &prometheusCollector{
		module:        module,
		active:        prometheus.NewDesc("liveforge_record_sessions_active", "Current active recording sessions.", nil, nil),
		completed:     prometheus.NewDesc("liveforge_record_files_completed_total", "Total recordings completed successfully.", nil, nil),
		failed:        prometheus.NewDesc("liveforge_record_files_failed_total", "Total recordings preserved in failed state.", nil, nil),
		retries:       prometheus.NewDesc("liveforge_record_write_retries_total", "Total recording write retries.", nil, nil),
		writeFailures: prometheus.NewDesc("liveforge_record_write_failures_total", "Total recording write failures.", nil, nil),
		deleted:       prometheus.NewDesc("liveforge_record_files_deleted_total", "Total recordings deleted through the management provider.", nil, nil),
		bytesWritten:  prometheus.NewDesc("liveforge_record_bytes_written_total", "Total bytes written to recordings.", nil, nil),
		storageErrors: prometheus.NewDesc("liveforge_record_storage_errors_total", "Total recording storage errors.", nil, nil),
	}
}

func (c *prometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.active
	ch <- c.completed
	ch <- c.failed
	ch <- c.retries
	ch <- c.writeFailures
	ch <- c.deleted
	ch <- c.bytesWritten
	ch <- c.storageErrors
}

func (c *prometheusCollector) Collect(ch chan<- prometheus.Metric) {
	c.module.mu.Lock()
	active := len(c.module.sessions)
	c.module.mu.Unlock()
	metrics := c.module.metrics.Snapshot()
	ch <- prometheus.MustNewConstMetric(c.active, prometheus.GaugeValue, float64(active))
	ch <- prometheus.MustNewConstMetric(c.completed, prometheus.CounterValue, float64(metrics.FilesCompleted))
	ch <- prometheus.MustNewConstMetric(c.failed, prometheus.CounterValue, float64(metrics.FilesFailed))
	ch <- prometheus.MustNewConstMetric(c.retries, prometheus.CounterValue, float64(metrics.WriteRetries))
	ch <- prometheus.MustNewConstMetric(c.writeFailures, prometheus.CounterValue, float64(metrics.WriteFailures))
	ch <- prometheus.MustNewConstMetric(c.deleted, prometheus.CounterValue, float64(metrics.FilesDeleted))
	ch <- prometheus.MustNewConstMetric(c.bytesWritten, prometheus.CounterValue, float64(metrics.BytesWritten))
	ch <- prometheus.MustNewConstMetric(c.storageErrors, prometheus.CounterValue, float64(metrics.StorageErrors))
}

// PrometheusCollectors exposes fixed-label recording metrics to the metrics module.
func (m *Module) PrometheusCollectors() []prometheus.Collector {
	return []prometheus.Collector{newPrometheusCollector(m)}
}

var _ RecordingProvider = (*Module)(nil)
