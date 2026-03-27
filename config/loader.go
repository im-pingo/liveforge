package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads and parses a YAML config file, expanding environment variables.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	// Expand ${ENV_VAR} patterns
	expanded := os.ExpandEnv(string(data))

	cfg := defaults()
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return cfg, nil
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
			MaxSkipCount:   3,
			Feedback: FeedbackConfig{
				DefaultMode: "auto",
				AutoThresholds: AutoThresholdsConfig{
					PassthroughMax: 1,
					AggregateMax:   5,
				},
			},
		},
		API: APIConfig{
			Listen: ":8090",
		},
	}
}
