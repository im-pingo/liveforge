package webrtc

import (
	"fmt"
	"sort"
	"strings"

	"github.com/im-pingo/liveforge/config"
	"github.com/pion/sdp/v3"
)

type whipSimulcastPlan struct {
	layers       []config.LayerConfig // descending advisory bitrate, then RID
	pause        bool
	expectsAudio bool
}

// Admit only the bounded RID form implemented by the ingest family. In
// particular, SDP alternatives and initially paused encodings are not aliases.
func parseWHIPSimulcastOffer(offer string, cfg config.SimulcastConfig) (*whipSimulcastPlan, error) {
	var parsed sdp.SessionDescription
	if err := parsed.UnmarshalString(offer); err != nil {
		return nil, fmt.Errorf("invalid SDP: %w", err)
	}
	var plan *whipSimulcastPlan
	var videoLines, audioLines int
	for _, attr := range parsed.Attributes {
		if attr.Key == "rid" || attr.Key == "simulcast" || attr.Key == "ssrc-group" && strings.HasPrefix(attr.Value, "SIM ") {
			return nil, fmt.Errorf("simulcast attributes must be media-level")
		}
	}
	for _, media := range parsed.MediaDescriptions {
		sends := mediaSendsToWHIP(&parsed, media)
		if sends {
			switch media.MediaName.Media {
			case "video":
				videoLines++
			case "audio":
				audioLines++
			}
		}
		rids := make(map[string]bool)
		var simulcast string
		hasSimulcast := false
		for _, attr := range media.Attributes {
			switch attr.Key {
			case "ssrc-group":
				if fields := strings.Fields(attr.Value); len(fields) > 0 && fields[0] == "SIM" {
					return nil, fmt.Errorf("legacy SSRC simulcast is unsupported")
				}
			case "rid":
				fields := strings.Fields(attr.Value)
				if len(fields) < 2 || fields[1] != "send" || !config.ValidSimulcastRID(fields[0]) || rids[fields[0]] {
					return nil, fmt.Errorf("invalid or duplicate sending RID")
				}
				rids[fields[0]] = true
			case "simulcast":
				if hasSimulcast {
					return nil, fmt.Errorf("duplicate simulcast attribute")
				}
				hasSimulcast = true
				simulcast = attr.Value
			}
		}
		if len(rids) == 0 && !hasSimulcast {
			continue
		}
		if !cfg.Enabled {
			return nil, fmt.Errorf("simulcast is disabled")
		}
		if err := config.ValidateSimulcastConfig(cfg); err != nil {
			return nil, err
		}
		if plan != nil || media.MediaName.Media != "video" || !sends {
			return nil, fmt.Errorf("simulcast requires one active video m-line")
		}
		fields := strings.Fields(simulcast)
		if len(fields) != 2 || fields[0] != "send" || len(rids) == 0 || len(rids) > config.MaxSimulcastLayers {
			return nil, fmt.Errorf("simulcast requires 1 to 3 declared sending RIDs")
		}
		configured := make(map[string]config.LayerConfig, len(cfg.Layers))
		for _, layer := range cfg.Layers {
			configured[layer.RID] = layer
		}
		plan = &whipSimulcastPlan{pause: cfg.AutoPauseLayer}
		seen := make(map[string]bool)
		for _, rid := range strings.Split(fields[1], ";") {
			layer, ok := configured[rid]
			if !ok || !rids[rid] || seen[rid] {
				return nil, fmt.Errorf("simulcast RID is undeclared, unconfigured, duplicated, paused, or an alternative")
			}
			seen[rid] = true
			plan.layers = append(plan.layers, layer)
		}
		if len(seen) != len(rids) {
			return nil, fmt.Errorf("every sending RID must appear in simulcast")
		}
		sort.Slice(plan.layers, func(i, j int) bool {
			if plan.layers[i].MaxBitrate != plan.layers[j].MaxBitrate {
				return plan.layers[i].MaxBitrate > plan.layers[j].MaxBitrate
			}
			return plan.layers[i].RID < plan.layers[j].RID
		})
	}
	if plan != nil {
		if videoLines != 1 || audioLines > 1 {
			return nil, fmt.Errorf("simulcast supports one sending video and at most one shared audio m-line")
		}
		plan.expectsAudio = audioLines == 1
	}
	return plan, nil
}

func mediaSendsToWHIP(session *sdp.SessionDescription, media *sdp.MediaDescription) bool {
	if media.MediaName.Port.Value == 0 {
		return false
	}
	direction := "sendrecv"
	for _, attributes := range [][]sdp.Attribute{session.Attributes, media.Attributes} {
		for _, attr := range attributes {
			switch attr.Key {
			case "sendrecv", "sendonly", "recvonly", "inactive":
				direction = attr.Key
			}
		}
	}
	return direction == "sendrecv" || direction == "sendonly"
}
