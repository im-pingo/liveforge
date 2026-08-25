package sipgateway

import "github.com/prometheus/client_golang/prometheus"

var (
	activeCallsDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "active_calls"),
		"Number of active SIP gateway calls.", []string{"direction"}, nil,
	)
	callsStartedDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "calls_started_total"),
		"Total SIP gateway calls established.", nil, nil,
	)
	callsEndedDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "calls_ended_total"),
		"Total established SIP gateway calls ended.", nil, nil,
	)
	setupFailuresDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "setup_failures_total"),
		"Total SIP gateway call setup failures.", nil, nil,
	)
	codecFailuresDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "codec_failures_total"),
		"Total SIP gateway calls rejected due to codec mismatch.", nil, nil,
	)
	duplicateCallIDsDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "duplicate_call_ids_total"),
		"Total duplicate SIP Call-IDs rejected.", nil, nil,
	)
	portExhaustionsDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "port_exhaustions_total"),
		"Total SIP gateway setups rejected due to RTP port exhaustion.", nil, nil,
	)
	capacityRejectionsDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "capacity_rejections_total"),
		"Total SIP gateway setups rejected at the call limit.", nil, nil,
	)
	networkFailuresDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "network_failures_total"),
		"Total SIP gateway calls ended by RTP network failure or timeout.", nil, nil,
	)
	rtpPacketsDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "rtp_packets_total"),
		"Total SIP gateway RTP packets.", []string{"direction"}, nil,
	)
	rtpBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName("liveforge", "sipgateway", "rtp_bytes_total"),
		"Total SIP gateway RTP bytes including RTP headers.", []string{"direction"}, nil,
	)
)

// Describe implements prometheus.Collector.
func (gw *Gateway) Describe(ch chan<- *prometheus.Desc) {
	ch <- activeCallsDesc
	ch <- callsStartedDesc
	ch <- callsEndedDesc
	ch <- setupFailuresDesc
	ch <- codecFailuresDesc
	ch <- duplicateCallIDsDesc
	ch <- portExhaustionsDesc
	ch <- capacityRejectionsDesc
	ch <- networkFailuresDesc
	ch <- rtpPacketsDesc
	ch <- rtpBytesDesc
}

// Collect implements prometheus.Collector with a fixed set of bounded labels.
func (gw *Gateway) Collect(ch chan<- prometheus.Metric) {
	snapshot := gw.Metrics()
	ch <- prometheus.MustNewConstMetric(activeCallsDesc, prometheus.GaugeValue, float64(snapshot.ActiveInbound), "inbound")
	ch <- prometheus.MustNewConstMetric(activeCallsDesc, prometheus.GaugeValue, float64(snapshot.ActiveOutbound), "outbound")
	ch <- prometheus.MustNewConstMetric(callsStartedDesc, prometheus.CounterValue, float64(snapshot.CallsStarted))
	ch <- prometheus.MustNewConstMetric(callsEndedDesc, prometheus.CounterValue, float64(snapshot.CallsEnded))
	ch <- prometheus.MustNewConstMetric(setupFailuresDesc, prometheus.CounterValue, float64(snapshot.SetupFailures))
	ch <- prometheus.MustNewConstMetric(codecFailuresDesc, prometheus.CounterValue, float64(snapshot.CodecFailures))
	ch <- prometheus.MustNewConstMetric(duplicateCallIDsDesc, prometheus.CounterValue, float64(snapshot.DuplicateCallIDs))
	ch <- prometheus.MustNewConstMetric(portExhaustionsDesc, prometheus.CounterValue, float64(snapshot.PortExhaustions))
	ch <- prometheus.MustNewConstMetric(capacityRejectionsDesc, prometheus.CounterValue, float64(snapshot.CapacityRejections))
	ch <- prometheus.MustNewConstMetric(networkFailuresDesc, prometheus.CounterValue, float64(snapshot.NetworkFailures))
	ch <- prometheus.MustNewConstMetric(rtpPacketsDesc, prometheus.CounterValue, float64(snapshot.RTPPacketsSent), "sent")
	ch <- prometheus.MustNewConstMetric(rtpPacketsDesc, prometheus.CounterValue, float64(snapshot.RTPPacketsRecv), "received")
	ch <- prometheus.MustNewConstMetric(rtpBytesDesc, prometheus.CounterValue, float64(snapshot.RTPBytesSent), "sent")
	ch <- prometheus.MustNewConstMetric(rtpBytesDesc, prometheus.CounterValue, float64(snapshot.RTPBytesRecv), "received")
}

var _ prometheus.Collector = (*Gateway)(nil)
