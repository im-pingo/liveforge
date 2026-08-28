package fmp4

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
)

// Muxer generates fragmented MP4 (CMAF/fMP4) output from AVFrames.
type Muxer struct {
	videoCodec      avframe.CodecType
	audioCodec      avframe.CodecType
	audioSampleRate int
	sequenceNumber  uint32
}

// NewMuxer creates a new FMP4 muxer.
func NewMuxer(videoCodec, audioCodec avframe.CodecType) *Muxer {
	return &Muxer{
		videoCodec: videoCodec,
		audioCodec: audioCodec,
	}
}

// Init generates the ftyp+moov init segment from sequence header frames.
// It also records audioSampleRate for use in subsequent WriteSegment calls.
func (m *Muxer) Init(videoSeqHeader, audioSeqHeader *avframe.AVFrame, width, height, sampleRate, channels int) []byte {
	var videoData, audioData []byte
	if videoSeqHeader != nil {
		videoData = videoSeqHeader.Payload
	}
	if audioSeqHeader != nil {
		audioData = audioSeqHeader.Payload
	}
	sampleRate, channels = resolveAudioConfig(m.audioCodec, audioData, sampleRate, channels)
	m.audioSampleRate = sampleRate
	if width <= 0 || height <= 0 {
		if derivedWidth, derivedHeight := ParseVideoDimensions(m.videoCodec, videoData); derivedWidth > 0 && derivedHeight > 0 {
			width, height = derivedWidth, derivedHeight
		}
	}
	return BuildInitSegment(m.videoCodec, m.audioCodec, videoData, audioData, width, height, sampleRate, channels)
}

func resolveAudioConfig(codec avframe.CodecType, audioData []byte, sampleRate, channels int) (int, int) {
	if sampleRate <= 0 {
		switch codec {
		case avframe.CodecAAC:
			if info, err := aac.ParseAudioSpecificConfig(audioData); err == nil && info.SampleRate > 0 {
				sampleRate = info.SampleRate
			}
		case avframe.CodecOpus:
			sampleRate = 48000
		case avframe.CodecMP3:
			sampleRate = 44100
		}
		if sampleRate <= 0 {
			sampleRate = timescaleAudio
		}
	}
	if channels <= 0 {
		if codec == avframe.CodecAAC {
			if info, err := aac.ParseAudioSpecificConfig(audioData); err == nil && info.Channels > 0 {
				channels = info.Channels
			}
		}
		if channels <= 0 {
			channels = 2
		}
	}
	return sampleRate, channels
}

// ParseVideoDimensions extracts display dimensions from a codec configuration record.
func ParseVideoDimensions(codec avframe.CodecType, config []byte) (width, height int) {
	switch codec {
	case avframe.CodecH264:
		return ParseAVCCDimensions(config)
	case avframe.CodecH265:
		return h265.ParseHVCCDimensions(config)
	default:
		return 0, 0
	}
}

// WriteSegment generates a moof+mdat segment from a GOP or group of frames.
func (m *Muxer) WriteSegment(frames []*avframe.AVFrame) []byte {
	m.sequenceNumber++
	return BuildMediaSegment(frames, m.sequenceNumber, uint32(m.audioSampleRate))
}

// WriteSegmentUntil generates a segment whose final video sample ends at endDTS.
func (m *Muxer) WriteSegmentUntil(frames []*avframe.AVFrame, endDTS int64) []byte {
	m.sequenceNumber++
	return buildMediaSegment(frames, m.sequenceNumber, uint32(m.audioSampleRate), endDTS)
}
