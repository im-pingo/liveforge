package push

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type whipAudioProcessor interface {
	Process(*avframe.AVFrame) ([][]byte, error)
	Close()
}

type whipOpusPassthrough struct{}

func (*whipOpusPassthrough) Process(frame *avframe.AVFrame) ([][]byte, error) {
	if frame == nil || frame.FrameType == avframe.FrameTypeSequenceHeader || len(frame.Payload) == 0 {
		return nil, nil
	}
	return [][]byte{frame.Payload}, nil
}

func (*whipOpusPassthrough) Close() {}

func newWHIPAudioProcessor(codec avframe.CodecType) (whipAudioProcessor, error) {
	if codec == 0 {
		return nil, nil
	}
	if codec == avframe.CodecOpus {
		return &whipOpusPassthrough{}, nil
	}
	return newWHIPAudioTranscoder(codec)
}

func whipOpusPacketDurationSamples(payload []byte) (uint32, bool) {
	if len(payload) == 0 {
		return 0, false
	}

	config := payload[0] >> 3
	var samplesPerFrame uint32
	switch {
	case config < 12:
		samplesPerFrame = [...]uint32{480, 960, 1920, 2880}[config&0x03]
	case config < 16:
		samplesPerFrame = 480 << (config & 0x01)
	default:
		samplesPerFrame = 120 << (config & 0x03)
	}

	var frameCount uint32
	switch payload[0] & 0x03 {
	case 0:
		frameCount = 1
	case 1, 2:
		frameCount = 2
	case 3:
		if len(payload) < 2 {
			return 0, false
		}
		frameCount = uint32(payload[1] & 0x3f)
		if frameCount == 0 {
			return 0, false
		}
	}

	duration := samplesPerFrame * frameCount
	if duration == 0 || duration > 5760 {
		return 0, false
	}
	return duration, true
}
