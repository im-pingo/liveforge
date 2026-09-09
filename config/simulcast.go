package config

import "fmt"

const MaxSimulcastLayers = 3

// ValidSimulcastRID accepts short RTP identifiers, excluding selector aliases.
func ValidSimulcastRID(rid string) bool {
	if len(rid) == 0 || len(rid) > 16 || rid == "auto" || rid == "high" || rid == "low" {
		return false
	}
	for _, c := range []byte(rid) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func ValidateSimulcastConfig(cfg SimulcastConfig) error {
	if len(cfg.Layers) > MaxSimulcastLayers || cfg.Enabled && len(cfg.Layers) == 0 {
		return fmt.Errorf("stream.simulcast.layers must contain 1 to %d layers when enabled", MaxSimulcastLayers)
	}
	seen := make(map[string]bool, len(cfg.Layers))
	for _, layer := range cfg.Layers {
		if !ValidSimulcastRID(layer.RID) || seen[layer.RID] {
			return fmt.Errorf("stream.simulcast.layers requires unique 1 to 16 character ASCII RIDs, excluding auto/high/low")
		}
		if layer.MaxBitrate <= 0 {
			return fmt.Errorf("stream.simulcast.layers.max_bitrate must be positive (ranking metadata in bits per second)")
		}
		seen[layer.RID] = true
	}
	return nil
}
