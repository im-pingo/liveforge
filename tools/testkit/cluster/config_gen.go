package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/im-pingo/liveforge/config"
	"gopkg.in/yaml.v3"
)

// AllocatedPorts holds the auto-allocated ports for a node.
// Keys are protocol names: "rtmp", "srt", "http_stream", "rtsp", "webrtc", "api".
type AllocatedPorts struct {
	Ports map[string]int
}

// GenerateNodeConfig builds a config.Config for a single cluster node.
// It sets protocol listeners based on node.Protocols, always enables the API
// for health checks, and wires up cluster forward/origin settings based on
// the topology links.
func GenerateNodeConfig(node NodeConfig, ports AllocatedPorts, links []Link, allPorts map[string]AllocatedPorts) *config.Config {
	cfg := baseConfig(node.Name)

	// Enable requested protocols.
	for _, proto := range node.Protocols {
		enableProtocol(cfg, proto, ports)
	}

	// Always enable API for health checks.
	if p, ok := ports.Ports["api"]; ok {
		cfg.API.Enabled = true
		cfg.API.Listen = fmt.Sprintf("127.0.0.1:%d", p)
	}

	// Configure cluster relay based on role and links.
	configureCluster(cfg, node, links, allPorts)

	return cfg
}

// WriteConfig serializes a Config to a YAML file in the specified directory.
// It returns the full path to the written file.
func WriteConfig(cfg *config.Config, dir, filename string) (string, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}

	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write config to %s: %w", path, err)
	}

	return path, nil
}

// baseConfig returns a minimal Config with sensible defaults.
func baseConfig(name string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Name:         name,
			LogLevel:     "warn",
			DrainTimeout: 5 * time.Second,
		},
		Stream: config.StreamConfig{
			GOPCache:           true,
			GOPCacheNum:        1,
			AudioCacheMs:       1000,
			RingBufferSize:     1024,
			IdleTimeout:        30 * time.Second,
			NoPublisherTimeout: 15 * time.Second,
		},
	}
}

// enableProtocol enables a specific protocol on the config using the allocated port.
func enableProtocol(cfg *config.Config, proto string, ports AllocatedPorts) {
	p, ok := ports.Ports[proto]
	if !ok {
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", p)

	switch proto {
	case "rtmp":
		cfg.RTMP.Enabled = true
		cfg.RTMP.Listen = addr
		cfg.RTMP.ChunkSize = 4096
	case "srt":
		cfg.SRT.Enabled = true
		cfg.SRT.Listen = addr
		cfg.SRT.Latency = 120
	case "rtsp":
		cfg.RTSP.Enabled = true
		cfg.RTSP.Listen = addr
	case "http_stream":
		cfg.HTTP.Enabled = true
		cfg.HTTP.Listen = addr
		cfg.HTTP.CORS = true
		cfg.HTTP.HLS.SegmentDuration = 6
		cfg.HTTP.HLS.PlaylistSize = 5
	case "webrtc":
		cfg.WebRTC.Enabled = true
		cfg.WebRTC.Listen = addr
		cfg.WebRTC.ICELite = true
	}
}

// configureCluster sets up forward and origin settings based on the node's role
// and the topology links.
func configureCluster(cfg *config.Config, node NodeConfig, links []Link, allPorts map[string]AllocatedPorts) {
	// Find outgoing links (this node forwards to downstream).
	var forwardTargets []string
	for _, link := range links {
		if link.From != node.Name {
			continue
		}
		targetPorts, ok := allPorts[link.To]
		if !ok {
			continue
		}
		targetPort, ok := targetPorts.Ports[link.Protocol]
		if !ok {
			continue
		}
		// Forward target includes app path (e.g. "/live").
		target := buildRelayURL(link.Protocol, targetPort, true)
		forwardTargets = append(forwardTargets, target)
	}

	// Find incoming links (this node pulls from upstream).
	var originServers []string
	for _, link := range links {
		if link.To != node.Name {
			continue
		}
		sourcePorts, ok := allPorts[link.From]
		if !ok {
			continue
		}
		sourcePort, ok := sourcePorts.Ports[link.Protocol]
		if !ok {
			continue
		}
		// Origin server does NOT include app path.
		server := buildRelayURL(link.Protocol, sourcePort, false)
		originServers = append(originServers, server)
	}

	if len(forwardTargets) > 0 {
		cfg.Cluster.Forward.Enabled = true
		cfg.Cluster.Forward.Targets = forwardTargets
		cfg.Cluster.Forward.RetryMax = 3
		cfg.Cluster.Forward.RetryInterval = 2 * time.Second
	}

	if len(originServers) > 0 {
		cfg.Cluster.Origin.Enabled = true
		cfg.Cluster.Origin.Servers = originServers
		cfg.Cluster.Origin.IdleTimeout = 30 * time.Second
		cfg.Cluster.Origin.RetryMax = 3
		cfg.Cluster.Origin.RetryDelay = 2 * time.Second
	}
}

// buildRelayURL constructs a protocol URL for relay.
// If includeApp is true, the app path "/live" is appended (for forward targets).
// If includeApp is false, only the scheme and host:port are included (for origin servers).
func buildRelayURL(proto string, port int, includeApp bool) string {
	scheme := protocolScheme(proto)
	base := fmt.Sprintf("%s://127.0.0.1:%d", scheme, port)
	if includeApp {
		return base + "/live"
	}
	return base
}

// protocolScheme returns the URL scheme for a given protocol name.
func protocolScheme(proto string) string {
	switch proto {
	case "rtmp":
		return "rtmp"
	case "srt":
		return "srt"
	case "rtsp":
		return "rtsp"
	default:
		return proto
	}
}
