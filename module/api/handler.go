package api

import (
	"encoding/json"
	"net/http"

	"github.com/im-pingo/liveforge/core"
)

// StreamInfo represents a single stream in the API response.
type StreamInfo struct {
	Key         string         `json:"key"`
	State       string         `json:"state"`
	Publisher   string         `json:"publisher"`
	VideoCodec  string         `json:"video_codec"`
	AudioCodec  string         `json:"audio_codec"`
	GOPCacheLen int            `json:"gop_cache_len"`
	Subscribers map[string]int `json:"subscribers"`
}

// StreamsResponse is the top-level JSON response for GET /api/v1/streams.
type StreamsResponse struct {
	Streams []StreamInfo `json:"streams"`
}

func (m *Module) handleStreams(w http.ResponseWriter, r *http.Request) {
	resp := buildStreamsResponse(m.server.StreamHub())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func buildStreamsResponse(hub *core.StreamHub) StreamsResponse {
	keys := hub.Keys()
	streams := make([]StreamInfo, 0, len(keys))

	for _, key := range keys {
		stream, ok := hub.Find(key)
		if !ok {
			continue
		}

		state := stream.State()
		// Skip streams that are being destroyed — they are effectively dead.
		if state == core.StreamStateDestroying {
			continue
		}

		// Merge muxer-level (flv/ts/mp4) and protocol-level (rtmp) subscriber counts.
		subs := stream.MuxerManager().Formats()
		for proto, count := range stream.Subscribers() {
			subs[proto] = count
		}

		info := StreamInfo{
			Key:         stream.Key(),
			State:       state.String(),
			GOPCacheLen: stream.GOPCacheLen(),
			Subscribers: subs,
		}

		if pub := stream.Publisher(); pub != nil {
			info.Publisher = pub.ID()
			if mi := pub.MediaInfo(); mi != nil {
				info.VideoCodec = mi.VideoCodec.String()
				info.AudioCodec = mi.AudioCodec.String()
			}
		}

		streams = append(streams, info)
	}

	return StreamsResponse{Streams: streams}
}
