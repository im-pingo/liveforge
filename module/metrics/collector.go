package metrics

import (
	"github.com/im-pingo/liveforge/core"
	"github.com/prometheus/client_golang/prometheus"
)

// Collector implements prometheus.Collector and gathers LiveForge metrics.
type Collector struct {
	server *core.Server

	// Server-level gauges
	streamCount     *prometheus.Desc
	connectionCount *prometheus.Desc
	uptimeSeconds   *prometheus.Desc
	configFailures  *prometheus.Desc
	configPending   *prometheus.Desc
	configCallbacks *prometheus.Desc
	configDropped   *prometheus.Desc
	configChanges   *prometheus.Desc
	apiAuthFailures *prometheus.Desc
	apiRBACFailures *prometheus.Desc
	apiRateDenials  *prometheus.Desc
	apiAuditEvents  *prometheus.Desc

	// Per-stream gauges (labels: stream_key)
	streamBytesIn     *prometheus.Desc
	streamVideoFrames *prometheus.Desc
	streamAudioFrames *prometheus.Desc
	streamBitrateKbps *prometheus.Desc
	streamFPS         *prometheus.Desc
	streamUptimeSec   *prometheus.Desc
	streamSubscribers *prometheus.Desc
	streamGOPFrames   *prometheus.Desc
}

// NewCollector creates a Collector that scrapes metrics from the server.
func NewCollector(s *core.Server) *Collector {
	ns := "liveforge"
	return &Collector{
		server: s,

		streamCount: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "server", "streams_active"),
			"Number of active streams.",
			nil, nil,
		),
		connectionCount: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "server", "connections_active"),
			"Number of active connections.",
			nil, nil,
		),
		uptimeSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "server", "uptime_seconds"),
			"Server uptime in seconds.",
			nil, nil,
		),
		configFailures: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "config", "consecutive_failures"),
			"Consecutive runtime configuration refresh failures.", nil, nil,
		),
		configPending: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "config", "pending_restart"),
			"Number of configuration paths waiting for a process restart.", nil, nil,
		),
		configCallbacks: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "config", "callback_failures"),
			"Total runtime configuration callback failures.", nil, nil,
		),
		configDropped: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "config", "callbacks_dropped"),
			"Total runtime configuration callbacks dropped because the callback queue was full.", nil, nil,
		),
		configChanges: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "config", "changes_total"),
			"Total runtime configuration changes by terminal result.", []string{"result"}, nil,
		),
		apiAuthFailures: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "api", "authentication_failures_total"),
			"Total management API authentication failures.", nil, nil,
		),
		apiRBACFailures: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "api", "authorization_failures_total"),
			"Total management API authorization failures.", nil, nil,
		),
		apiRateDenials: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "api", "rate_limit_denials_total"),
			"Total management API requests rejected by rate limiting.", nil, nil,
		),
		apiAuditEvents: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "api", "audit_events_total"),
			"Total management audit events emitted.", nil, nil,
		),

		streamBytesIn: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "bytes_in_total"),
			"Total bytes received for a stream.",
			[]string{"stream_key"}, nil,
		),
		streamVideoFrames: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "video_frames_total"),
			"Total video frames received for a stream.",
			[]string{"stream_key"}, nil,
		),
		streamAudioFrames: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "audio_frames_total"),
			"Total audio frames received for a stream.",
			[]string{"stream_key"}, nil,
		),
		streamBitrateKbps: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "bitrate_kbps"),
			"Current bitrate in kbps for a stream.",
			[]string{"stream_key"}, nil,
		),
		streamFPS: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "fps"),
			"Current FPS for a stream.",
			[]string{"stream_key"}, nil,
		),
		streamUptimeSec: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "uptime_seconds"),
			"Stream uptime in seconds.",
			[]string{"stream_key"}, nil,
		),
		streamSubscribers: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "subscribers"),
			"Number of subscribers per stream and protocol.",
			[]string{"stream_key", "protocol"}, nil,
		),
		streamGOPFrames: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "stream", "gop_cache_frames"),
			"Number of frames in the GOP cache.",
			[]string{"stream_key"}, nil,
		),
	}
}

// Describe sends metric descriptors to the channel.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.streamCount
	ch <- c.connectionCount
	ch <- c.uptimeSeconds
	ch <- c.configFailures
	ch <- c.configPending
	ch <- c.configCallbacks
	ch <- c.configDropped
	ch <- c.configChanges
	ch <- c.apiAuthFailures
	ch <- c.apiRBACFailures
	ch <- c.apiRateDenials
	ch <- c.apiAuditEvents
	ch <- c.streamBytesIn
	ch <- c.streamVideoFrames
	ch <- c.streamAudioFrames
	ch <- c.streamBitrateKbps
	ch <- c.streamFPS
	ch <- c.streamUptimeSec
	ch <- c.streamSubscribers
	ch <- c.streamGOPFrames
}

// Collect gathers current metrics from the server and streams.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	hub := c.server.StreamHub()

	// Server-level metrics
	ch <- prometheus.MustNewConstMetric(c.streamCount, prometheus.GaugeValue, float64(hub.Count()))
	ch <- prometheus.MustNewConstMetric(c.connectionCount, prometheus.GaugeValue, float64(c.server.ConnectionCount()))
	ch <- prometheus.MustNewConstMetric(c.uptimeSeconds, prometheus.GaugeValue, c.server.UptimeSeconds())
	if manager := c.server.ConfigManager(); manager != nil {
		status := manager.Status()
		ch <- prometheus.MustNewConstMetric(c.configFailures, prometheus.GaugeValue, float64(status.ConsecutiveFailures))
		ch <- prometheus.MustNewConstMetric(c.configPending, prometheus.GaugeValue, float64(len(status.PendingRestart)))
		ch <- prometheus.MustNewConstMetric(c.configCallbacks, prometheus.CounterValue, float64(status.CallbackFailures))
		ch <- prometheus.MustNewConstMetric(c.configDropped, prometheus.CounterValue, float64(status.DroppedCallbacks))
		ch <- prometheus.MustNewConstMetric(c.configChanges, prometheus.CounterValue, float64(status.ConfigChangesAccepted), "accepted")
		ch <- prometheus.MustNewConstMetric(c.configChanges, prometheus.CounterValue, float64(status.ConfigChangesRejected), "rejected")
		ch <- prometheus.MustNewConstMetric(c.configChanges, prometheus.CounterValue, float64(status.ConfigChangesApplicationFailed), "application_failed")
	}
	if module := c.server.ModuleByName("api"); module != nil {
		if provider, ok := module.(interface{ SecurityMetricValues() map[string]float64 }); ok {
			values := provider.SecurityMetricValues()
			ch <- prometheus.MustNewConstMetric(c.apiAuthFailures, prometheus.CounterValue, values["authentication_failures"])
			ch <- prometheus.MustNewConstMetric(c.apiRBACFailures, prometheus.CounterValue, values["authorization_failures"])
			ch <- prometheus.MustNewConstMetric(c.apiRateDenials, prometheus.CounterValue, values["rate_limit_denials"])
			ch <- prometheus.MustNewConstMetric(c.apiAuditEvents, prometheus.CounterValue, values["audit_events"])
		}
	}

	// Per-stream metrics
	for _, key := range hub.Keys() {
		stream, ok := hub.Find(key)
		if !ok || stream.State() == core.StreamStateDestroying {
			continue
		}

		stats := stream.Stats()
		ch <- prometheus.MustNewConstMetric(c.streamBytesIn, prometheus.CounterValue, float64(stats.BytesIn), key)
		ch <- prometheus.MustNewConstMetric(c.streamVideoFrames, prometheus.CounterValue, float64(stats.VideoFrames), key)
		ch <- prometheus.MustNewConstMetric(c.streamAudioFrames, prometheus.CounterValue, float64(stats.AudioFrames), key)
		ch <- prometheus.MustNewConstMetric(c.streamBitrateKbps, prometheus.GaugeValue, float64(stats.BitrateKbps), key)
		ch <- prometheus.MustNewConstMetric(c.streamFPS, prometheus.GaugeValue, stats.FPS, key)
		ch <- prometheus.MustNewConstMetric(c.streamUptimeSec, prometheus.GaugeValue, stats.Uptime.Seconds(), key)

		gopDetail := stream.GOPCacheDetail()
		ch <- prometheus.MustNewConstMetric(c.streamGOPFrames, prometheus.GaugeValue, float64(gopDetail.TotalFrames), key)

		// Subscriber counts per protocol
		for proto, count := range stream.Subscribers() {
			ch <- prometheus.MustNewConstMetric(c.streamSubscribers, prometheus.GaugeValue, float64(count), key, proto)
		}
		for proto, count := range stream.MuxerManager().Formats() {
			ch <- prometheus.MustNewConstMetric(c.streamSubscribers, prometheus.GaugeValue, float64(count), key, proto)
		}
	}
}
