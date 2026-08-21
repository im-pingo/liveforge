package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type ReloadClass string

const (
	ReloadHot        ReloadClass = "hot"
	ReloadNewSession ReloadClass = "new_session"
	ReloadRestart    ReloadClass = "restart"
)

type ChangeSet map[string]ReloadClass

func (c ChangeSet) Class(path string) ReloadClass { return c[path] }

func (c ChangeSet) Paths(class ReloadClass) []string {
	paths := make([]string, 0)
	for path, got := range c {
		if got == class {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

var restartPaths = map[string]struct{}{
	"tls": {}, "rtmp.enabled": {}, "rtmp.listen": {}, "rtmp.tls": {},
	"rtsp.enabled": {}, "rtsp.listen": {}, "rtsp.rtp_port_range": {}, "rtsp.tls": {},
	"http_stream.enabled": {}, "http_stream.listen": {}, "http_stream.tls": {},
	"websocket.enabled": {}, "websocket.listen": {}, "websocket.path": {},
	"webrtc.enabled": {}, "webrtc.listen": {}, "webrtc.udp_port_range": {}, "webrtc.tls": {},
	"srt.enabled": {}, "srt.listen": {}, "sip.enabled": {}, "sip.listen": {}, "sip.transport": {},
	"gb28181.enabled": {}, "gb28181.rtp_port_range": {},
	"api.enabled": {}, "api.listen": {}, "api.tls": {},
	"metrics.enabled": {}, "metrics.listen": {}, "metrics.path": {}, "dvr.listen": {},
}

func classifyPath(path string) ReloadClass {
	if _, ok := restartPaths[path]; ok {
		return ReloadRestart
	}
	for restart := range restartPaths {
		if strings.HasPrefix(path, restart+".") {
			return ReloadRestart
		}
	}
	for _, prefix := range []string{"stream.", "record.", "dvr.", "audio_codec."} {
		if strings.HasPrefix(path, prefix) {
			return ReloadNewSession
		}
	}
	return ReloadHot
}

func diffConfigs(oldCfg, newCfg *Config) (ChangeSet, error) {
	oldMap, err := configMap(oldCfg)
	if err != nil {
		return nil, err
	}
	newMap, err := configMap(newCfg)
	if err != nil {
		return nil, err
	}
	changes := ChangeSet{}
	diffMap("", oldMap, newMap, changes)
	return changes, nil
}

func diffMap(prefix string, oldMap, newMap map[string]any, changes ChangeSet) {
	keys := map[string]struct{}{}
	for key := range oldMap {
		keys[key] = struct{}{}
	}
	for key := range newMap {
		keys[key] = struct{}{}
	}
	for key := range keys {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		oldValue, oldOK := oldMap[key]
		newValue, newOK := newMap[key]
		oldNested, oldMapOK := oldValue.(map[string]any)
		newNested, newMapOK := newValue.(map[string]any)
		if oldOK && newOK && oldMapOK && newMapOK {
			diffMap(path, oldNested, newNested, changes)
			continue
		}
		if fmt.Sprint(oldValue) != fmt.Sprint(newValue) || oldOK != newOK {
			changes[path] = classifyPath(path)
		}
	}
}

func buildEffective(previous, desired *Config, changes ChangeSet) (*Config, error) {
	previousMap, err := configMap(previous)
	if err != nil {
		return nil, err
	}
	effectiveMap, err := configMap(desired)
	if err != nil {
		return nil, err
	}
	for _, path := range changes.Paths(ReloadRestart) {
		value, ok := getPath(previousMap, strings.Split(path, "."))
		if ok {
			setPath(effectiveMap, strings.Split(path, "."), value)
		}
	}
	effective, _, err := decodeConfigMap(effectiveMap)
	return effective, err
}

func configMap(cfg *Config) (map[string]any, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return decodeYAMLMap(data)
}

func getPath(value map[string]any, parts []string) (any, bool) {
	current := value
	for i, part := range parts {
		got, ok := current[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return got, true
		}
		current, ok = got.(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return nil, false
}

func setPath(value map[string]any, parts []string, leaf any) {
	current := value
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = leaf
}
