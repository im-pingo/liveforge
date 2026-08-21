package rtsp

import (
	"encoding/base64"
	"net/url"
	"strings"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

// encodingNameToCodec maps SDP encoding names to internal codec types.
var encodingNameToCodec = map[string]avframe.CodecType{
	"H264":          avframe.CodecH264,
	"H265":          avframe.CodecH265,
	"VP8":           avframe.CodecVP8,
	"VP9":           avframe.CodecVP9,
	"AV1":           avframe.CodecAV1,
	"MPEG4-GENERIC": avframe.CodecAAC,
	"MP4A-LATM":     avframe.CodecAAC,
	"OPUS":          avframe.CodecOpus,
	"MPA":           avframe.CodecMP3,
	"PCMU":          avframe.CodecG711U,
	"PCMA":          avframe.CodecG711A,
	"G722":          avframe.CodecG722,
	"G729":          avframe.CodecG729,
	"SPEEX":         avframe.CodecSpeex,
}

// sdpToMediaInfo extracts MediaInfo from a parsed SDP SessionDescription.
func sdpToMediaInfo(sd *sdp.SessionDescription) *avframe.MediaInfo {
	info, _ := sdpToMediaInfoWithPT(sd)
	return info
}

// RTPTrackInfo is the SDP-declared codec and RTP timestamp clock for a payload type.
type RTPTrackInfo struct {
	Codec     avframe.CodecType
	ClockRate uint32
}

// PTMap maps RTP payload types to codecs. It is retained for constructor compatibility.
type PTMap map[uint8]avframe.CodecType

// RTPTrackMap maps RTP payload types to their SDP-declared track metadata.
type RTPTrackMap map[uint8]RTPTrackInfo

// RTPTrackDescription preserves one SDP media line's identity. Payload types
// are scoped to a media line and may be reused by another track.
type RTPTrackDescription struct {
	TrackID     int
	Control     string
	PayloadType uint8
	Info        RTPTrackInfo
}

// sdpToTrackDescriptions extracts codec and control metadata for each media
// line. A trackID in the control attribute is preferred; the media index is
// the fallback identity used by generated SDP.
func sdpToTrackDescriptions(sd *sdp.SessionDescription) []RTPTrackDescription {
	if sd == nil {
		return nil
	}
	descriptions := make([]RTPTrackDescription, 0, len(sd.Media))
	for index, md := range sd.Media {
		if md == nil || len(md.Formats) == 0 {
			continue
		}
		pt := md.Formats[0]
		if pt < 0 || pt > 127 {
			continue
		}
		rtpMap := md.RTPMap(pt)
		if rtpMap == nil {
			continue
		}
		codec, ok := encodingNameToCodec[strings.ToUpper(rtpMap.EncodingName)]
		if !ok {
			continue
		}
		clockRate := uint32(0)
		if rtpMap.ClockRate > 0 {
			clockRate = uint32(rtpMap.ClockRate)
		}
		control := md.Control()
		trackID := index
		if id, ok := extractTrackID(control); ok {
			trackID = id
		}
		descriptions = append(descriptions, RTPTrackDescription{
			TrackID:     trackID,
			Control:     control,
			PayloadType: uint8(pt),
			Info:        RTPTrackInfo{Codec: codec, ClockRate: clockRate},
		})
	}
	return descriptions
}

// sdpToMediaInfoWithPT extracts MediaInfo and a PT-to-codec mapping from SDP.
func sdpToMediaInfoWithPT(sd *sdp.SessionDescription) (*avframe.MediaInfo, RTPTrackMap) {
	info := &avframe.MediaInfo{}
	ptMap := make(RTPTrackMap)

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
			trackInfo := RTPTrackInfo{Codec: codec}
			if rtpMap.ClockRate > 0 {
				trackInfo.ClockRate = uint32(rtpMap.ClockRate)
			}
			ptMap[uint8(pt)] = trackInfo
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
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Path != "" {
		rawURL = parsed.Path
	}
	rawURL = strings.Trim(rawURL, "/")
	if idx := strings.LastIndexByte(rawURL, '/'); idx >= 0 {
		rawURL = rawURL[idx+1:]
	}
	if !strings.HasPrefix(rawURL, "trackID=") {
		return -1, false
	}
	s := rawURL[len("trackID="):]
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
