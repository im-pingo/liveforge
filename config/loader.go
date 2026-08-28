package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
	if err := ValidateRemovedSettings([]byte(expanded)); err != nil {
		return nil, err
	}

	cfg := defaults()
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	normalize(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// ValidateRemovedSettings rejects configuration keys that are no longer
// supported while leaving unrelated unknown fields compatible.
func ValidateRemovedSettings(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if len(document.Content) == 0 {
		return nil
	}

	path := []string{"stream", "audio_cache_ms"}
	if yamlMappingContainsPath(document.Content[0], path, make(map[yamlTraversalState]bool)) {
		return errors.New("stream.audio_cache_ms has been removed; audio is interleaved in the GOP cache")
	}
	return nil
}

type yamlTraversalState struct {
	node      *yaml.Node
	pathDepth int
}

func yamlMappingContainsPath(node *yaml.Node, path []string, active map[yamlTraversalState]bool) bool {
	return yamlMappingContainsPathMemo(node, path, active, make(map[yamlTraversalState]bool))
}

func yamlMappingContainsPathMemo(
	node *yaml.Node,
	path []string,
	active map[yamlTraversalState]bool,
	completed map[yamlTraversalState]bool,
) (found bool) {
	node = dereferenceYAMLAlias(node)
	if node == nil || node.Kind != yaml.MappingNode || len(path) == 0 {
		return false
	}

	state := yamlTraversalState{node: node, pathDepth: len(path)}
	if found, ok := completed[state]; ok {
		return found
	}
	if active[state] {
		return false
	}
	active[state] = true
	defer delete(active, state)
	defer func() {
		completed[state] = found
	}()

	for i := 0; i+1 < len(node.Content); i += 2 {
		key := dereferenceYAMLAlias(node.Content[i])
		if key != nil && key.Kind == yaml.ScalarNode && key.Value == path[0] {
			if len(path) == 1 || yamlMappingContainsPathMemo(node.Content[i+1], path[1:], active, completed) {
				return true
			}
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if isYAMLMergeKey(node.Content[i]) && yamlMergeContainsPath(node.Content[i+1], path, active, completed) {
			return true
		}
	}
	return false
}

func yamlMergeContainsPath(
	node *yaml.Node,
	path []string,
	active map[yamlTraversalState]bool,
	completed map[yamlTraversalState]bool,
) (found bool) {
	node = dereferenceYAMLAlias(node)
	if node == nil {
		return false
	}
	if node.Kind == yaml.MappingNode {
		return yamlMappingContainsPathMemo(node, path, active, completed)
	}
	if node.Kind != yaml.SequenceNode {
		return false
	}

	state := yamlTraversalState{node: node, pathDepth: len(path)}
	if found, ok := completed[state]; ok {
		return found
	}
	if active[state] {
		return false
	}
	active[state] = true
	defer delete(active, state)
	defer func() {
		completed[state] = found
	}()
	for _, child := range node.Content {
		if yamlMergeContainsPath(child, path, active, completed) {
			return true
		}
	}
	return false
}

func isYAMLMergeKey(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == "<<" &&
		(node.Tag == "" || node.Tag == "!" || node.ShortTag() == "!!merge")
}

func dereferenceYAMLAlias(node *yaml.Node) *yaml.Node {
	seen := make(map[*yaml.Node]bool)
	for node != nil && node.Kind == yaml.AliasNode {
		if seen[node] {
			return nil
		}
		seen[node] = true
		node = node.Alias
	}
	return node
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
				PartDuration:    0.2,
				SegmentDuration: 1.0,
				SegmentCount:    4,
				Container:       "fmp4",
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
			Listen: ":8090",
			Audit:  AuditConfig{MaxEntries: 1000},
		},
		Metrics: MetricsConfig{
			Listen: ":9090",
			Path:   "/metrics",
		},
		Runtime: RuntimeConfig{
			Source:       "file",
			PollInterval: 30 * time.Second,
			LoadTimeout:  10 * time.Second,
		},
		Record: RecordConfig{Format: "fmp4"},
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

// Defaults returns a new configuration populated with LiveForge defaults.
// Callers may modify the returned value without affecting any other config.
func Defaults() *Config {
	return defaults()
}

// normalize canonicalizes config values (e.g. container name aliases).
func normalize(cfg *Config) {
	switch strings.ToLower(cfg.HTTP.LLHLS.Container) {
	case "mpegts", "mpeg-ts":
		cfg.HTTP.LLHLS.Container = "ts"
	}
	if cfg.API.Auth.BearerToken == "" && cfg.Auth.API.BearerToken != "" {
		cfg.API.Auth.BearerToken = cfg.Auth.API.BearerToken
	}
}

// Normalize canonicalizes config values after unmarshalling.
func Normalize(cfg *Config) {
	if cfg != nil {
		normalize(cfg)
	}
}
