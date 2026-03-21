package sdp

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// codecRTPInfo maps CodecType to (encoding name, clock rate, default PT).
// A clock rate of 0 means "use MediaInfo.SampleRate".
var codecRTPInfo = map[avframe.CodecType]struct {
	Name      string
	ClockRate int
	PT        int
}{
	avframe.CodecH264:  {"H264", 90000, 96},
	avframe.CodecH265:  {"H265", 90000, 97},
	avframe.CodecVP8:   {"VP8", 90000, 98},
	avframe.CodecVP9:   {"VP9", 90000, 99},
	avframe.CodecAV1:   {"AV1", 90000, 100},
	avframe.CodecAAC:   {"MPEG4-GENERIC", 0, 101},
	avframe.CodecOpus:  {"opus", 48000, 111},
	avframe.CodecMP3:   {"MPA", 90000, 14},
	avframe.CodecG711U: {"PCMU", 8000, 0},
	avframe.CodecG711A: {"PCMA", 8000, 8},
	avframe.CodecG722:  {"G722", 8000, 9},
	avframe.CodecG729:  {"G729", 8000, 18},
	avframe.CodecSpeex: {"speex", 0, 102},
}

// BuildFromMediaInfo generates an SDP SessionDescription from stream MediaInfo.
// baseURL is the RTSP URL used for session-level control.
// serverAddr is the server IP address used in the o= and c= lines.
func BuildFromMediaInfo(info *avframe.MediaInfo, baseURL string, serverAddr string) *SessionDescription {
	sessionID := fmt.Sprintf("%d", time.Now().UnixNano())

	sd := &SessionDescription{
		Version: 0,
		Origin: Origin{
			Username:       "-",
			SessionID:      sessionID,
			SessionVersion: "1",
			NetType:        "IN",
			AddrType:       "IP4",
			Address:        serverAddr,
		},
		Name: "LiveForge Stream",
		Connection: &Connection{
			NetType:  "IN",
			AddrType: "IP4",
			Address:  "0.0.0.0",
		},
		Timing: Timing{Start: 0, Stop: 0},
		Attributes: []Attribute{
			{Key: "tool", Value: "liveforge"},
			{Key: "type", Value: "broadcast"},
			{Key: "control", Value: baseURL},
		},
	}

	trackIdx := 0

	if info.HasVideo() {
		md := buildMediaDesc("video", info.VideoCodec, 0, 0, trackIdx)
		// Add sprop-parameter-sets for H.264 if VideoSequenceHeader is available
		if info.VideoCodec == avframe.CodecH264 && len(info.VideoSequenceHeader) > 0 {
			sprop := buildSPropParameterSets(info.VideoSequenceHeader)
			if sprop != "" {
				// Update the fmtp line to include sprop-parameter-sets
				for i, a := range md.Attributes {
					if a.Key == "fmtp" {
						md.Attributes[i].Value += "; sprop-parameter-sets=" + sprop
						break
					}
				}
			}
		}
		sd.Media = append(sd.Media, md)
		trackIdx++
	}

	if info.HasAudio() {
		md := buildMediaDesc("audio", info.AudioCodec, info.SampleRate, info.Channels, trackIdx)
		sd.Media = append(sd.Media, md)
	}

	return sd
}

// buildMediaDesc creates a MediaDescription for a single track.
func buildMediaDesc(mediaType string, codec avframe.CodecType, sampleRate, channels, trackIdx int) *MediaDescription {
	rtpInfo, ok := codecRTPInfo[codec]
	if !ok {
		return &MediaDescription{
			Type:    mediaType,
			Port:    0,
			Proto:   "RTP/AVP",
			Formats: []int{96},
		}
	}

	clockRate := rtpInfo.ClockRate
	if clockRate == 0 {
		clockRate = sampleRate
	}

	md := &MediaDescription{
		Type:    mediaType,
		Port:    0,
		Proto:   "RTP/AVP",
		Formats: []int{rtpInfo.PT},
	}

	// Build rtpmap value: "<pt> <name>/<clock>[/<channels>]"
	rtpmapVal := fmt.Sprintf("%d %s/%d", rtpInfo.PT, rtpInfo.Name, clockRate)
	if codec == avframe.CodecOpus && channels > 0 {
		rtpmapVal = fmt.Sprintf("%d %s/%d/%d", rtpInfo.PT, rtpInfo.Name, clockRate, channels)
	}
	md.Attributes = append(md.Attributes, Attribute{Key: "rtpmap", Value: rtpmapVal})

	// Add fmtp for H264 (packetization-mode=1 enables FU-A and STAP-A)
	if codec == avframe.CodecH264 {
		fmtpVal := fmt.Sprintf("%d packetization-mode=1", rtpInfo.PT)
		md.Attributes = append(md.Attributes, Attribute{Key: "fmtp", Value: fmtpVal})
	}

	// Add fmtp for AAC
	if codec == avframe.CodecAAC {
		fmtpVal := fmt.Sprintf("%d profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3", rtpInfo.PT)
		md.Attributes = append(md.Attributes, Attribute{Key: "fmtp", Value: fmtpVal})
	}

	// Control attribute for per-track addressing
	md.Attributes = append(md.Attributes, Attribute{Key: "control", Value: fmt.Sprintf("trackID=%d", trackIdx)})

	return md
}

// buildSPropParameterSets converts a VideoSequenceHeader (AVCDecoderConfigurationRecord
// or Annex-B) into a comma-separated base64 string for SDP sprop-parameter-sets.
func buildSPropParameterSets(seqHeader []byte) string {
	if len(seqHeader) == 0 {
		return ""
	}

	// Try AVCDecoderConfigurationRecord first (version byte = 1)
	if len(seqHeader) >= 8 && seqHeader[0] == 1 {
		return buildSPropFromAVCConfig(seqHeader)
	}

	// Fallback: Annex-B format
	nals := splitAnnexBNALs(seqHeader)
	var parts []string
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		nalType := nal[0] & 0x1F
		if nalType == 7 || nalType == 8 {
			parts = append(parts, base64.StdEncoding.EncodeToString(nal))
		}
	}
	return strings.Join(parts, ",")
}

// buildSPropFromAVCConfig extracts SPS/PPS from AVCDecoderConfigurationRecord
// and returns base64-encoded sprop-parameter-sets string.
func buildSPropFromAVCConfig(config []byte) string {
	var parts []string
	offset := 5
	if offset >= len(config) {
		return ""
	}
	// SPS
	numSPS := int(config[offset] & 0x1F)
	offset++
	for range numSPS {
		if offset+2 > len(config) {
			break
		}
		spsLen := int(config[offset])<<8 | int(config[offset+1])
		offset += 2
		if offset+spsLen > len(config) {
			break
		}
		parts = append(parts, base64.StdEncoding.EncodeToString(config[offset:offset+spsLen]))
		offset += spsLen
	}
	if offset >= len(config) {
		return strings.Join(parts, ",")
	}
	// PPS
	numPPS := int(config[offset])
	offset++
	for range numPPS {
		if offset+2 > len(config) {
			break
		}
		ppsLen := int(config[offset])<<8 | int(config[offset+1])
		offset += 2
		if offset+ppsLen > len(config) {
			break
		}
		parts = append(parts, base64.StdEncoding.EncodeToString(config[offset:offset+ppsLen]))
		offset += ppsLen
	}
	return strings.Join(parts, ",")
}

// splitAnnexBNALs splits Annex-B byte stream into individual NAL units.
func splitAnnexBNALs(data []byte) [][]byte {
	var positions []int
	i := 0
	for i < len(data) {
		if i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				positions = append(positions, i)
				i += 3
				continue
			}
			if i+4 <= len(data) && data[i+2] == 0 && data[i+3] == 1 {
				positions = append(positions, i)
				i += 4
				continue
			}
		}
		i++
	}
	if len(positions) == 0 {
		return [][]byte{data}
	}
	var nals [][]byte
	for j, pos := range positions {
		nalStart := pos + 3
		if pos+3 < len(data) && data[pos+2] == 0 {
			nalStart = pos + 4
		}
		var nalEnd int
		if j+1 < len(positions) {
			nalEnd = positions[j+1]
		} else {
			nalEnd = len(data)
		}
		if nalStart < nalEnd {
			nals = append(nals, data[nalStart:nalEnd])
		}
	}
	return nals
}
