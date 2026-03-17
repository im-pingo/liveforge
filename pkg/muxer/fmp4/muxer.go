package fmp4

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Muxer generates fragmented MP4 (CMAF/fMP4) output from AVFrames.
type Muxer struct {
	videoCodec     avframe.CodecType
	audioCodec     avframe.CodecType
	sequenceNumber uint32
}

// NewMuxer creates a new FMP4 muxer.
func NewMuxer(videoCodec, audioCodec avframe.CodecType) *Muxer {
	return &Muxer{
		videoCodec: videoCodec,
		audioCodec: audioCodec,
	}
}

// Init generates the ftyp+moov init segment from sequence header frames.
func (m *Muxer) Init(videoSeqHeader, audioSeqHeader *avframe.AVFrame, width, height, sampleRate, channels int) []byte {
	var videoData, audioData []byte
	if videoSeqHeader != nil {
		videoData = videoSeqHeader.Payload
	}
	if audioSeqHeader != nil {
		audioData = audioSeqHeader.Payload
	}
	return BuildInitSegment(m.videoCodec, m.audioCodec, videoData, audioData, width, height, sampleRate, channels)
}

// WriteSegment generates a moof+mdat segment from a GOP or group of frames.
func (m *Muxer) WriteSegment(frames []*avframe.AVFrame) []byte {
	m.sequenceNumber++
	return BuildMediaSegment(frames, m.sequenceNumber)
}
