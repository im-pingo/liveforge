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
