package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/config"
	"gopkg.in/yaml.v3"
)

// ParseDocument parses YAML or JSON into a defaulted and normalized config.
func ParseDocument(data []byte) (*config.Config, error) {
	return parseDocument(data, true)
}

func parseDocument(data []byte, expandEnvironment bool) (*config.Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("configuration document is empty")
	}
	if expandEnvironment {
		data = []byte(os.ExpandEnv(string(data)))
	}
	cfg := config.Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	config.Normalize(cfg)
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func validateConfig(cfg *config.Config) error {
	if err := config.Validate(cfg); err != nil {
		return err
	}
	if cfg.Server.DrainTimeout < 0 {
		return fmt.Errorf("server.drain_timeout must not be negative")
	}
	if err := validateRange("rtsp.rtp_port_range", cfg.RTSP.RTPPortRange); err != nil {
		return err
	}
	if err := validateRange("webrtc.udp_port_range", cfg.WebRTC.UDPPortRange); err != nil {
		return err
	}
	if err := validateRange("gb28181.rtp_port_range", cfg.GB28181.RTPPortRange); err != nil {
		return err
	}
	if cfg.HTTP.LLHLS.Container != "" && cfg.HTTP.LLHLS.Container != "fmp4" && cfg.HTTP.LLHLS.Container != "ts" {
		return fmt.Errorf("http_stream.llhls.container must be fmp4 or ts")
	}
	if cfg.WebRTC.GCC.MinBitrate < 0 || cfg.WebRTC.GCC.InitialBitrate < 0 || cfg.WebRTC.GCC.MaxBitrate < 0 {
		return fmt.Errorf("webrtc.gcc bitrates must not be negative")
	}
	if cfg.WebRTC.GCC.MaxBitrate > 0 && cfg.WebRTC.GCC.MinBitrate > cfg.WebRTC.GCC.MaxBitrate {
		return fmt.Errorf("webrtc.gcc.min_bitrate must not exceed max_bitrate")
	}
	if (cfg.TLS.CertFile == "") != (cfg.TLS.KeyFile == "") {
		return fmt.Errorf("tls.cert_file and tls.key_file must be configured together")
	}
	return nil
}

func validateRange(name string, r []int) error {
	if len(r) == 0 {
		return nil
	}
	if len(r) != 2 || r[0] < 1 || r[1] < r[0] || r[1] > 65535 {
		return fmt.Errorf("%s must contain an ordered range between 1 and 65535", name)
	}
	return nil
}

func normalizedBytes(cfg *config.Config) ([]byte, error) {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized config: %w", err)
	}
	return b, nil
}

func configHash(cfg *config.Config) (string, error) {
	b, err := normalizedBytes(cfg)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func cloneConfig(cfg *config.Config) (*config.Config, error) {
	b, err := normalizedBytes(cfg)
	if err != nil {
		return nil, err
	}
	return parseDocument(b, false)
}

func configMap(cfg *config.Config) (map[string]any, error) {
	b, err := normalizedBytes(cfg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func diffConfigs(oldCfg, newCfg *config.Config) ([]Change, error) {
	if oldCfg == nil {
		return nil, nil
	}
	oldMap, err := configMap(oldCfg)
	if err != nil {
		return nil, err
	}
	newMap, err := configMap(newCfg)
	if err != nil {
		return nil, err
	}
	changes := make([]Change, 0)
	diffMaps("", oldMap, newMap, &changes)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

func diffMaps(prefix string, oldMap, newMap map[string]any, changes *[]Change) {
	keys := make(map[string]struct{}, len(oldMap)+len(newMap))
	for k := range oldMap {
		keys[k] = struct{}{}
	}
	for k := range newMap {
		keys[k] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		ov, ook := oldMap[key]
		nv, nok := newMap[key]
		om, omok := ov.(map[string]any)
		nm, nmok := nv.(map[string]any)
		if omok && nmok {
			diffMaps(path, om, nm, changes)
			continue
		}
		if !ook || !nok || !reflect.DeepEqual(ov, nv) {
			*changes = append(*changes, Change{Path: path, Class: classifyPath(path)})
		}
	}
}

func applyHotChanges(current, desired *config.Config, changes []Change) (*config.Config, error) {
	if current == nil {
		return cloneConfig(desired)
	}
	currentMap, err := configMap(current)
	if err != nil {
		return nil, err
	}
	desiredMap, err := configMap(desired)
	if err != nil {
		return nil, err
	}
	for _, change := range changes {
		if change.Class != ChangeHot {
			continue
		}
		parts := strings.Split(change.Path, ".")
		value, ok := mapPathValue(desiredMap, parts)
		if !ok {
			return nil, fmt.Errorf("hot configuration path %q is missing", change.Path)
		}
		if !setMapPath(currentMap, parts, value) {
			return nil, fmt.Errorf("hot configuration path %q cannot be applied", change.Path)
		}
	}
	data, err := yaml.Marshal(currentMap)
	if err != nil {
		return nil, fmt.Errorf("marshal effective config: %w", err)
	}
	return parseDocument(data, false)
}

func mapPathValue(root map[string]any, parts []string) (any, bool) {
	current := root
	for i, part := range parts {
		value, ok := current[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return value, true
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return nil, false
}

func setMapPath(root map[string]any, parts []string, value any) bool {
	if len(parts) == 0 {
		return false
	}
	current := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return false
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return true
}

func classifyPath(path string) ChangeClass {
	if path == "server.name" || strings.HasPrefix(path, "server.name.") {
		return ChangeImmutable
	}
	switch {
	case path == "server.log_level", path == "server.drain_timeout":
		return ChangeHot
	case strings.HasPrefix(path, "limits."):
		return ChangeHot
	case strings.HasPrefix(path, "stream.simulcast"), path == "stream.ring_buffer_size":
		return ChangeRestart
	case strings.HasPrefix(path, "stream."):
		return ChangeHot
	case path == "auth.enabled":
		return ChangeRestart
	case strings.HasPrefix(path, "auth."):
		return ChangeHot
	case strings.HasPrefix(path, "notify.http.endpoints"):
		return ChangeHot
	case strings.HasPrefix(path, "notify."):
		return ChangeRestart
	case strings.HasPrefix(path, "record."):
		if path == "record.enabled" || path == "record.path" {
			return ChangeRestart
		}
		return ChangeHot
	case strings.HasPrefix(path, "dvr."):
		if path == "dvr.enabled" || path == "dvr.listen" || path == "dvr.path" {
			return ChangeRestart
		}
		return ChangeHot
	case strings.HasPrefix(path, "api.auth."), strings.HasPrefix(path, "api.console."):
		return ChangeHot
	case strings.HasPrefix(path, "api."):
		return ChangeRestart
	case strings.HasPrefix(path, "cluster."):
		if strings.HasPrefix(path, "cluster.forward.") || strings.HasPrefix(path, "cluster.origin.") || strings.HasPrefix(path, "cluster.health_check.") {
			if path == "cluster.forward.enabled" || path == "cluster.origin.enabled" || path == "cluster.health_check.enabled" {
				return ChangeRestart
			}
			return ChangeHot
		}
		return ChangeRestart
	case strings.HasPrefix(path, "http_stream.hls."), strings.HasPrefix(path, "http_stream.dash."), strings.HasPrefix(path, "http_stream.llhls."):
		return ChangeHot
	case strings.HasPrefix(path, "webrtc.gcc."):
		if path == "webrtc.gcc.enabled" {
			return ChangeRestart
		}
		return ChangeHot
	default:
		return ChangeRestart
	}
}

func isZeroTime(t time.Time) bool { return t.IsZero() }
