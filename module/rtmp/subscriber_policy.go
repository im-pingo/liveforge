package rtmp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

type outputPolicy struct {
	videoMode      flvpkg.EncodingMode
	audioMode      flvpkg.EncodingMode
	transcodeAudio bool
}

func chooseOutputPolicy(info *avframe.MediaInfo, caps PeerCapabilities) (outputPolicy, error) {
	policy := outputPolicy{
		videoMode: flvpkg.EncodingClassic,
		audioMode: flvpkg.EncodingClassic,
	}
	if info == nil {
		return policy, nil
	}

	if info.VideoCodec != 0 {
		if caps.SupportsVideo(info.VideoCodec) {
			policy.videoMode = flvpkg.EncodingEnhanced
		} else if info.VideoCodec != avframe.CodecH264 {
			return outputPolicy{}, fmt.Errorf("rtmp: peer does not support video codec %s", info.VideoCodec)
		}
	}

	if info.AudioCodec != 0 {
		if caps.SupportsAudio(info.AudioCodec) {
			policy.audioMode = flvpkg.EncodingEnhanced
		} else {
			switch info.AudioCodec {
			case avframe.CodecAAC, avframe.CodecMP3:
				// Classic FLV can carry these codecs without negotiation.
			default:
				policy.transcodeAudio = true
			}
		}
	}

	return policy, nil
}
