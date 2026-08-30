package httpstream

import "github.com/im-pingo/liveforge/pkg/avframe"

type segmentMediaClock struct {
	hasVideo        bool
	audioSampleRate int
	lastVideo       *avframe.AVFrame
	lastAudio       *avframe.AVFrame
}

func newSegmentMediaClock(hasVideo bool, audioSampleRate int) segmentMediaClock {
	return segmentMediaClock{hasVideo: hasVideo, audioSampleRate: audioSampleRate}
}

func (c *segmentMediaClock) Observe(frame *avframe.AVFrame) {
	if frame == nil {
		return
	}
	if frame.MediaType.IsVideo() {
		c.lastVideo = frame
	} else if frame.MediaType.IsAudio() {
		c.lastAudio = frame
	}
}

func (c *segmentMediaClock) Reset() {
	c.lastVideo = nil
	c.lastAudio = nil
}

func (c *segmentMediaClock) EndDTS(startDTS int64) int64 {
	last := c.lastAudio
	if c.hasVideo && c.lastVideo != nil {
		last = c.lastVideo
	}
	if last == nil {
		return startDTS
	}
	endDTS := last.DTS + frameDurationMilliseconds(last, c.audioSampleRate)
	if endDTS <= startDTS {
		return startDTS + 1
	}
	return endDTS
}

func frameDurationMilliseconds(frame *avframe.AVFrame, sampleRate int) int64 {
	if frame == nil {
		return 1
	}
	if frame.MediaType.IsVideo() {
		return 33
	}

	samples := int64(0)
	switch frame.Codec {
	case avframe.CodecAAC:
		samples = 1024
		if sampleRate <= 0 {
			sampleRate = 44100
		}
	case avframe.CodecMP3:
		samples = 1152
		if sampleRate <= 0 {
			sampleRate = 44100
		}
	case avframe.CodecOpus:
		if opusSamples, ok := opusPacketDurationSamples48k(frame.Payload); ok {
			samples = int64(opusSamples)
		}
		sampleRate = 48000
	}
	if samples > 0 && sampleRate > 0 {
		duration := (samples*1000 + int64(sampleRate)/2) / int64(sampleRate)
		if duration > 0 {
			return duration
		}
	}
	return 20
}

func opusPacketDurationSamples48k(payload []byte) (uint32, bool) {
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
