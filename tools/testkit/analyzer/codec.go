package analyzer

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	"github.com/im-pingo/liveforge/tools/testkit/report"
)

// codecValidator parses sequence headers to extract and validate codec parameters.
type codecValidator struct {
	videoCodec      string
	videoProfile    string
	videoLevel      string
	videoResolution string
	videoMatch      bool
	videoParsed     bool

	audioCodec      string
	audioSampleRate int
	audioChannels   int
	audioMatch      bool
	audioParsed     bool
}

func newCodecValidator() *codecValidator {
	return &codecValidator{}
}

func (c *codecValidator) feed(frame *avframe.AVFrame) {
	if frame.FrameType != avframe.FrameTypeSequenceHeader {
		return
	}

	if frame.MediaType == avframe.MediaTypeVideo {
		c.parseVideo(frame)
	} else if frame.MediaType == avframe.MediaTypeAudio {
		c.parseAudio(frame)
	}
}

func (c *codecValidator) parseVideo(frame *avframe.AVFrame) {
	c.videoCodec = frame.Codec.String()

	if frame.Codec != avframe.CodecH264 {
		c.videoParsed = true
		c.videoMatch = true
		return
	}

	spsData := extractSPSFromPayload(frame.Payload)
	if spsData == nil {
		return
	}

	info, err := h264.ParseSPS(spsData)
	if err != nil {
		return
	}

	c.videoProfile = profileName(info.Profile)
	c.videoLevel = fmt.Sprintf("%.1f", float64(info.Level)/10.0)
	c.videoResolution = fmt.Sprintf("%dx%d", info.Width, info.Height)
	c.videoParsed = true
	c.videoMatch = true
}

func (c *codecValidator) parseAudio(frame *avframe.AVFrame) {
	c.audioCodec = frame.Codec.String()

	if frame.Codec != avframe.CodecAAC {
		c.audioParsed = true
		c.audioMatch = true
		return
	}

	info, err := aac.ParseAudioSpecificConfig(frame.Payload)
	if err != nil {
		return
	}

	c.audioSampleRate = info.SampleRate
	c.audioChannels = info.Channels
	c.audioParsed = true
	c.audioMatch = true
}

// videoReport returns video codec parameters for the report.
func (c *codecValidator) videoReport() (codec, profile, level, resolution string) {
	return c.videoCodec, c.videoProfile, c.videoLevel, c.videoResolution
}

// audioReport returns audio codec parameters for the report.
func (c *codecValidator) audioReport() (codec string, sampleRate, channels int) {
	return c.audioCodec, c.audioSampleRate, c.audioChannels
}

func (c *codecValidator) codecReport() report.CodecReport {
	return report.CodecReport{
		VideoMatch: c.videoMatch,
		AudioMatch: c.audioMatch,
	}
}

// extractSPSFromPayload extracts the SPS NAL unit from Annex-B formatted data.
// If the payload is already a raw SPS (starts with 0x67), return it directly.
func extractSPSFromPayload(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}

	// Try Annex-B extraction first.
	nalus := h264.ExtractNALUs(data)
	for _, nalu := range nalus {
		if len(nalu) > 0 && (nalu[0]&0x1F) == h264.NALTypeSPS {
			return nalu
		}
	}

	// If no start codes found, check if the data itself is a raw SPS.
	if len(data) > 0 && (data[0]&0x1F) == h264.NALTypeSPS {
		return data
	}

	return nil
}

// profileName maps H.264 profile_idc values to human-readable names.
func profileName(profileIDC int) string {
	switch profileIDC {
	case 66:
		return "Baseline"
	case 77:
		return "Main"
	case 88:
		return "Extended"
	case 100:
		return "High"
	case 110:
		return "High 10"
	case 122:
		return "High 4:2:2"
	case 244:
		return "High 4:4:4 Predictive"
	default:
		return fmt.Sprintf("Profile(%d)", profileIDC)
	}
}
