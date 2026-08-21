package rtmp

import (
	"strconv"

	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

// Capability flags describe what a peer can do with a codec payload.
const (
	CanDecode  uint32 = 0x01
	CanEncode  uint32 = 0x02
	CanForward uint32 = 0x04
)

var supportedVideoFourCC = []string{
	"vp08",
	"vp09",
	"av01",
	"avc1",
	"hvc1",
}

var supportedAudioFourCC = []string{
	"mp4a",
	"Opus",
	".mp3",
}

// PeerCapabilities is the E-RTMP capability snapshot for one connection.
// The maps contain only valid numeric capability entries received from AMF0.
type PeerCapabilities struct {
	FourCCList         []string
	VideoFourCCInfoMap map[string]uint32
	AudioFourCCInfoMap map[string]uint32
	CapsEx             uint32

	fourCCListPresent bool
	videoMapPresent   bool
	audioMapPresent   bool
	capsExPresent     bool
}

// ParsePeerCapabilities converts the optional connect properties into a
// tolerant, connection-local capability snapshot.
func ParsePeerCapabilities(obj map[string]any) PeerCapabilities {
	var caps PeerCapabilities
	if obj == nil {
		return caps
	}

	if value, ok := obj["fourCcList"]; ok {
		if list, valid := parseFourCCList(value); valid {
			caps.FourCCList = list
			caps.fourCCListPresent = true
		}
	}
	if value, ok := obj["videoFourCcInfoMap"]; ok {
		if info, valid := parseCapabilityMap(value); valid {
			caps.VideoFourCCInfoMap = info
			caps.videoMapPresent = true
		}
	}
	if value, ok := obj["audioFourCcInfoMap"]; ok {
		if info, valid := parseCapabilityMap(value); valid {
			caps.AudioFourCCInfoMap = info
			caps.audioMapPresent = true
		}
	}
	if value, ok := obj["capsEx"]; ok {
		if number, valid := capabilityNumber(value); valid {
			caps.CapsEx = number
			caps.capsExPresent = true
		}
	}

	return caps
}

// Enhanced reports whether the peer supplied at least one valid E-RTMP
// capability property.
func (c PeerCapabilities) Enhanced() bool {
	return c.fourCCListPresent || c.videoMapPresent || c.audioMapPresent || c.capsExPresent
}

// SupportsVideo reports whether the peer can receive the given video codec.
func (c PeerCapabilities) SupportsVideo(codec avframe.CodecType) bool {
	fourCC := flvpkg.VideoFourCC(codec)
	if fourCC == "" {
		return false
	}
	if c.videoMapPresent {
		return capabilityMapSupports(c.VideoFourCCInfoMap, fourCC)
	}
	return c.listSupports(fourCC)
}

// SupportsAudio reports whether the peer can receive the given audio codec.
func (c PeerCapabilities) SupportsAudio(codec avframe.CodecType) bool {
	fourCC := flvpkg.AudioFourCC(codec)
	if fourCC == "" {
		return false
	}
	if c.audioMapPresent {
		return capabilityMapSupports(c.AudioFourCCInfoMap, fourCC)
	}
	return c.listSupports(fourCC)
}

func (c PeerCapabilities) listSupports(fourCC string) bool {
	if !c.fourCCListPresent {
		return false
	}
	for _, supported := range c.FourCCList {
		if supported == "*" || supported == fourCC {
			return true
		}
	}
	return false
}

func capabilityMapSupports(info map[string]uint32, fourCC string) bool {
	// E-RTMP defines the wildcard as the map-wide default. It takes
	// precedence over a codec-specific entry when both are present.
	if flags, ok := info["*"]; ok {
		return flags&(CanDecode|CanForward) != 0
	}
	flags, ok := info[fourCC]
	return ok && flags&(CanDecode|CanForward) != 0
}

// ServerCapabilitiesObject returns the capability properties advertised by
// LiveForge in a connect result.
func ServerCapabilitiesObject() map[string]any {
	return capabilitiesObject(CanForward)
}

// ClientCapabilitiesObject returns capabilities for the bundled RTMP clients.
func ClientCapabilitiesObject() map[string]any {
	return capabilitiesObject(CanDecode | CanForward)
}

func capabilitiesObject(flags uint32) map[string]any {
	list := make([]any, 0, len(supportedVideoFourCC)+len(supportedAudioFourCC))
	for _, fourCC := range supportedVideoFourCC {
		list = append(list, fourCC)
	}
	for _, fourCC := range supportedAudioFourCC {
		list = append(list, fourCC)
	}

	videoInfo := make(map[string]any, len(supportedVideoFourCC))
	for _, fourCC := range supportedVideoFourCC {
		videoInfo[fourCC] = float64(flags)
	}
	audioInfo := make(map[string]any, len(supportedAudioFourCC))
	for _, fourCC := range supportedAudioFourCC {
		audioInfo[fourCC] = float64(flags)
	}

	return map[string]any{
		"fourCcList":         list,
		"videoFourCcInfoMap": videoInfo,
		"audioFourCcInfoMap": audioInfo,
		"capsEx":             float64(0),
	}
}

func parseFourCCList(value any) ([]string, bool) {
	var values []any
	switch list := value.(type) {
	case []any:
		values = list
	case []string:
		result := append([]string(nil), list...)
		return result, true
	default:
		return nil, false
	}

	result := make([]string, 0, len(values))
	for _, value := range values {
		fourCC, ok := value.(string)
		if !ok {
			return nil, false
		}
		result = append(result, fourCC)
	}
	return result, true
}

func parseCapabilityMap(value any) (map[string]uint32, bool) {
	result := make(map[string]uint32)
	switch info := value.(type) {
	case map[string]any:
		for fourCC, value := range info {
			if flags, ok := capabilityNumber(value); ok {
				result[fourCC] = flags
				continue
			}
			return nil, false
		}
		return result, true
	case map[string]float64:
		for fourCC, value := range info {
			if flags, ok := capabilityNumber(value); ok {
				result[fourCC] = flags
				continue
			}
			return nil, false
		}
		return result, true
	case map[string]uint32:
		for fourCC, value := range info {
			result[fourCC] = value
		}
		return result, true
	default:
		return nil, false
	}
}

func capabilityNumber(value any) (uint32, bool) {
	var number uint64
	switch value := value.(type) {
	case uint8:
		number = uint64(value)
	case uint16:
		number = uint64(value)
	case uint32:
		number = uint64(value)
	case uint64:
		number = value
	case int:
		if value < 0 {
			return 0, false
		}
		number = uint64(value)
	case int8:
		if value < 0 {
			return 0, false
		}
		number = uint64(value)
	case int16:
		if value < 0 {
			return 0, false
		}
		number = uint64(value)
	case int32:
		if value < 0 {
			return 0, false
		}
		number = uint64(value)
	case int64:
		if value < 0 {
			return 0, false
		}
		number = uint64(value)
	case float32:
		if value < 0 || float32(uint32(value)) != value {
			return 0, false
		}
		number = uint64(value)
	case float64:
		if value < 0 || value != float64(uint64(value)) {
			return 0, false
		}
		number = uint64(value)
	case string:
		parsed, err := strconv.ParseUint(value, 0, 32)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if number > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(number), true
}
