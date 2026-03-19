package sdp

import (
	"fmt"
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

	// Add fmtp for AAC
	if codec == avframe.CodecAAC {
		fmtpVal := fmt.Sprintf("%d profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3", rtpInfo.PT)
		md.Attributes = append(md.Attributes, Attribute{Key: "fmtp", Value: fmtpVal})
	}

	// Control attribute for per-track addressing
	md.Attributes = append(md.Attributes, Attribute{Key: "control", Value: fmt.Sprintf("trackID=%d", trackIdx)})

	return md
}
