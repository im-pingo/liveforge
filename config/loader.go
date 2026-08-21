package config

import (
	"context"
	"strings"
	"time"
)

// Load reads and parses a YAML config file, expanding environment variables.
func Load(path string) (*Config, error) {
	doc, err := NewFileSource(path, "").Load(context.Background())
	if err != nil {
		return nil, err
	}
	return doc.Config, nil
}

// defaults returns a Config with sensible default values.
func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Name:     "liveforge",
			LogLevel: "info",
		},
		RTMP: RTMPConfig{
			Listen:    ":1935",
			ChunkSize: 4096,
		},
		RTSP: RTSPConfig{
			Listen:       ":554",
			RTPPortRange: []int{10000, 20000},
		},
		HTTP: HTTPConfig{
			Listen: ":8080",
			CORS:   true,
			LLHLS: LLHLSConfig{
				PartDuration: 0.2,
				SegmentCount: 4,
				Container:    "fmp4",
			},
		},
		WS: WSConfig{
			Listen: ":8080",
			Path:   "/ws/{stream}.{format}",
		},
		WebRTC: WebRTCConfig{
			Listen:       ":8443",
			UDPPortRange: []int{20000, 30000},
			GCC: GCCConfig{
				Enabled:        true,
				InitialBitrate: 2_000_000,
				MinBitrate:     100_000,
				MaxBitrate:     10_000_000,
			},
		},
		SRT: SRTConfig{
			Listen:  ":6000",
			Latency: 120,
		},
		SIP: SIPConfig{
			Listen:    ":5060",
			Transport: []string{"udp", "tcp"},
		},
		Stream: StreamConfig{
			GOPCache:       true,
			GOPCacheNum:    1,
			AudioCacheMs:   1000,
			RingBufferSize: 1024,
			SlowConsumer: SlowConsumerConfig{
				Enabled:          true,
				LagWarnRatio:     0.5,
				LagDropRatio:     0.75,
				LagCriticalRatio: 0.9,
				LagRecoverRatio:  0.5,
				EWMAAlpha:        0.3,
				SendTimeRatio:    2.0,
			},
			Feedback: FeedbackConfig{
				DefaultMode: "auto",
				AutoThresholds: AutoThresholdsConfig{
					PassthroughMax: 1,
					AggregateMax:   5,
				},
			},
		},
		Limits: LimitsConfig{
			RateLimit: RateLimitConfig{
				Rate:  50,
				Burst: 100,
			},
		},
		API: APIConfig{
			Listen:  "127.0.0.1:8090",
			Auth:    APIAuthConfig{Enabled: true},
			Console: ConsoleConfig{Username: "admin", Password: "admin"},
		},
		Metrics: MetricsConfig{
			Listen: ":9090",
			Path:   "/metrics",
		},
		DVR: DVRConfig{
			Listen:          ":8070",
			Path:            "./dvr/{stream_key}",
			Window:          2 * time.Hour,
			SegmentDuration: 6 * time.Second,
			CleanupInterval: 30 * time.Second,
		},
		Cluster: ClusterConfig{
			SRT: ClusterSRTConfig{
				Latency:  120 * time.Millisecond,
				PBKeyLen: 16,
			},
			RTSP: ClusterRTSPConfig{
				Transport: "tcp",
			},
			RTP: ClusterRTPConfig{
				PortRange:     "20000-20100",
				SignalingPath: "/api/relay",
				RTCPInterval:  5 * time.Second,
				Timeout:       15 * time.Second,
			},
		},
	}
}

// normalize canonicalizes config values (e.g. container name aliases).
func normalize(cfg *Config) {
	normalizeAuthRule(&cfg.Auth.Publish)
	normalizeAuthRule(&cfg.Auth.Subscribe)
	if cfg.Auth.Publish.Stage == "" {
		cfg.Auth.Publish.Stage = "post_connect"
	}
	if cfg.Auth.Subscribe.Stage == "" {
		cfg.Auth.Subscribe.Stage = "post_connect"
	}
	switch strings.ToLower(cfg.HTTP.LLHLS.Container) {
	case "mpegts", "mpeg-ts":
		cfg.HTTP.LLHLS.Container = "ts"
	}
}

func normalizeAuthRule(rule *AuthRuleConfig) {
	algorithm := strings.TrimSpace(rule.Token.Algorithm)
	if algorithm == "" || strings.EqualFold(algorithm, "HS256") {
		rule.Token.Algorithm = "HS256"
	}
}
