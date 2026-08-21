package rtmp

import (
	"encoding/binary"
	"strings"

	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

func parseVideoPayload(data []byte, dts int64) *avframe.AVFrame {
	frame, _ := parseVideoPayloadWithError(data, dts)
	return frame
}

func parseAudioPayload(data []byte, dts int64) *avframe.AVFrame {
	frame, _ := parseAudioPayloadWithError(data, dts)
	return frame
}

func parseVideoPayloadWithError(data []byte, dts int64) (*avframe.AVFrame, error) {
	return flvpkg.ParseVideoPayload(data, dts)
}

func parseAudioPayloadWithError(data []byte, dts int64) (*avframe.AVFrame, error) {
	return flvpkg.ParseAudioPayload(data, dts)
}

// splitNameParams splits "test?token=xxx&key=val" into ("test", {"token":"xxx","key":"val"}).
func splitNameParams(name string) (string, map[string]string) {
	parts := strings.SplitN(name, "?", 2)
	if len(parts) == 1 {
		return name, nil
	}
	return parts[0], parseQueryParams(parts[1])
}

// parseQueryParams parses "key=val&key2=val2" into a map.
func parseQueryParams(query string) map[string]string {
	params := make(map[string]string)
	for _, pair := range strings.Split(query, "&") {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			params[kv[0]] = kv[1]
		} else {
			params[kv[0]] = ""
		}
	}
	return params
}

// mergeParams merges base and override params, with override taking precedence.
func mergeParams(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	result := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		result[k] = v
	}
	return result
}

// Protocol messages

func (h *Handler) sendStreamBegin(streamID uint32) error {
	payload := make([]byte, 6)
	binary.BigEndian.PutUint16(payload[0:2], 0) // StreamBegin event type
	binary.BigEndian.PutUint32(payload[2:6], streamID)
	return h.cw.WriteMessage(2, &Message{TypeID: MsgUserControl, Length: 6, Payload: payload})
}

func (h *Handler) sendSetWindowAckSize(size uint32) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, size)
	return h.cw.WriteMessage(2, &Message{TypeID: MsgWindowAckSize, Length: 4, Payload: payload})
}

func (h *Handler) sendSetPeerBandwidth(size uint32, limitType byte) error {
	payload := make([]byte, 5)
	binary.BigEndian.PutUint32(payload, size)
	payload[4] = limitType
	return h.cw.WriteMessage(2, &Message{TypeID: MsgSetPeerBandwidth, Length: 5, Payload: payload})
}

func (h *Handler) sendSetChunkSize(size int) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(size))
	if err := h.cw.WriteMessage(2, &Message{TypeID: MsgSetChunkSize, Length: 4, Payload: payload}); err != nil {
		return err
	}
	h.cw.SetChunkSize(size)
	return nil
}

func (h *Handler) sendConnectResult(txID float64) error {
	properties := map[string]any{
		"fmsVer":       "FMS/3,5,7,7009",
		"capabilities": float64(31),
	}
	for key, value := range ServerCapabilitiesObject() {
		properties[key] = value
	}

	payload, err := AMF0Encode(
		"_result",
		txID,
		properties,
		map[string]any{
			"level":       "status",
			"code":        "NetConnection.Connect.Success",
			"description": "Connection succeeded",
		},
	)
	if err != nil {
		return err
	}
	return h.cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(payload)), Payload: payload})
}

func (h *Handler) sendOnStatus(level, code, description string) error {
	payload, err := AMF0Encode(
		"onStatus",
		float64(0),
		nil,
		map[string]any{
			"level":       level,
			"code":        code,
			"description": description,
		},
	)
	if err != nil {
		return err
	}
	return h.cw.WriteMessage(5, &Message{TypeID: MsgAMF0Command, Length: uint32(len(payload)), Payload: payload, StreamID: 1})
}
