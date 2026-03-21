package rtsp

import (
	"encoding/base64"
	"strings"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

// encodingNameToCodec maps SDP encoding names to internal codec types.
var encodingNameToCodec = map[string]avframe.CodecType{
	"H264":           avframe.CodecH264,
	"H265":           avframe.CodecH265,
	"VP8":            avframe.CodecVP8,
	"VP9":            avframe.CodecVP9,
	"AV1":            avframe.CodecAV1,
	"MPEG4-GENERIC":  avframe.CodecAAC,
	"MP4A-LATM":      avframe.CodecAAC,
	"OPUS":           avframe.CodecOpus,
	"MPA":            avframe.CodecMP3,
	"PCMU":           avframe.CodecG711U,
	"PCMA":           avframe.CodecG711A,
	"G722":           avframe.CodecG722,
	"G729":           avframe.CodecG729,
	"SPEEX":          avframe.CodecSpeex,
}

// sdpToMediaInfo extracts MediaInfo from a parsed SDP SessionDescription.
func sdpToMediaInfo(sd *sdp.SessionDescription) *avframe.MediaInfo {
	info, _ := sdpToMediaInfoWithPT(sd)
	return info
}

// PTMap maps RTP payload types to codec types, as declared in the SDP.
type PTMap map[uint8]avframe.CodecType

// sdpToMediaInfoWithPT extracts MediaInfo and a PT-to-codec mapping from SDP.
func sdpToMediaInfoWithPT(sd *sdp.SessionDescription) (*avframe.MediaInfo, PTMap) {
	info := &avframe.MediaInfo{}
	ptMap := make(PTMap)

	for _, md := range sd.Media {
		if len(md.Formats) == 0 {
			continue
		}
		pt := md.Formats[0]
		rtpMap := md.RTPMap(pt)
		if rtpMap == nil {
			continue
		}

		codec, ok := encodingNameToCodec[strings.ToUpper(rtpMap.EncodingName)]
		if !ok {
			continue
		}

		// Record the SDP-declared PT for this codec.
		if pt >= 0 && pt <= 127 {
			ptMap[uint8(pt)] = codec
		}

		switch md.Type {
		case "video":
			if info.VideoCodec == 0 {
				info.VideoCodec = codec
				// Extract sprop-parameter-sets for H.264 sequence header
				if codec == avframe.CodecH264 {
					fmtp := md.FMTP(pt)
					if seqHeader := parseSPropParameterSets(fmtp); len(seqHeader) > 0 {
						info.VideoSequenceHeader = seqHeader
					}
				}
			}
		case "audio":
			if info.AudioCodec == 0 {
				info.AudioCodec = codec
				info.SampleRate = rtpMap.ClockRate
				info.Channels = rtpMap.Channels
				if info.Channels == 0 {
					info.Channels = 1
				}
			}
		}
	}

	return info, ptMap
}

// parseSPropParameterSets extracts SPS/PPS from the fmtp sprop-parameter-sets
// value and returns them as an AVCDecoderConfigurationRecord (matching RTMP/FLV format).
func parseSPropParameterSets(fmtp string) []byte {
	// Find sprop-parameter-sets in fmtp string
	// Format: "packetization-mode=1; sprop-parameter-sets=base64sps,base64pps; ..."
	for _, part := range strings.Split(fmtp, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "sprop-parameter-sets=") {
			val := part[len("sprop-parameter-sets="):]
			params := strings.Split(val, ",")
			var sps, pps []byte
			for _, p := range params {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				data, err := base64.StdEncoding.DecodeString(p)
				if err != nil || len(data) == 0 {
					continue
				}
				nalType := data[0] & 0x1F
				if nalType == 7 && sps == nil {
					sps = data
				} else if nalType == 8 && pps == nil {
					pps = data
				}
			}
			if sps != nil && pps != nil {
				return pkgrtp.BuildAVCDecoderConfig(
					append(append([]byte{0, 0, 0, 1}, sps...), append([]byte{0, 0, 0, 1}, pps...)...),
				)
			}
			return nil
		}
	}
	return nil
}

// extractTrackID extracts the trackID from an RTSP URL.
// e.g., "rtsp://host/live/test/trackID=0" -> 0, true
// Returns -1, false if no trackID is found.
func extractTrackID(rawURL string) (int, bool) {
	idx := strings.Index(rawURL, "trackID=")
	if idx < 0 {
		return -1, false
	}
	s := rawURL[idx+len("trackID="):]
	// Parse simple integer
	n := 0
	found := false
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			found = true
		} else {
			break
		}
	}
	if !found {
		return -1, false
	}
	return n, true
}
