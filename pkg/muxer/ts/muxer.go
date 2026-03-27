package ts

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
)

// Muxer converts AVFrames into MPEG-TS packets.
type Muxer struct {
	videoCodec     avframe.CodecType
	audioCodec     avframe.CodecType
	videoSeqHeader []byte // Raw SPS/PPS for H.264 or VPS/SPS/PPS for H.265 in Annex-B
	audioSeqHeader []byte // AudioSpecificConfig for AAC
	aacInfo        *aac.AACInfo

	videoContinuity uint8
	audioContinuity uint8
	patContinuity   uint8
	pmtContinuity   uint8
	lastPCR         int64

	pat []byte
	pmt []byte
}

// NewMuxer creates a TS muxer. videoSeqHeader/audioSeqHeader are the raw codec config data
// (e.g., AVCDecoderConfigurationRecord for H.264, AudioSpecificConfig for AAC).
func NewMuxer(videoCodec, audioCodec avframe.CodecType, videoSeqHeader, audioSeqHeader []byte) *Muxer {
	m := &Muxer{
		videoCodec:     videoCodec,
		audioCodec:     audioCodec,
		lastPCR:        -1,
	}

	// Parse video sequence header into Annex-B format for prepending on keyframes
	switch videoCodec {
	case avframe.CodecH264:
		if len(videoSeqHeader) > 5 {
			sps, pps, err := h264.ExtractSPSPPSFromAVCRecord(videoSeqHeader)
			if err == nil {
				startCode := []byte{0x00, 0x00, 0x00, 0x01}
				m.videoSeqHeader = append(m.videoSeqHeader, startCode...)
				m.videoSeqHeader = append(m.videoSeqHeader, sps...)
				m.videoSeqHeader = append(m.videoSeqHeader, startCode...)
				m.videoSeqHeader = append(m.videoSeqHeader, pps...)
			}
		}
	case avframe.CodecH265:
		if len(videoSeqHeader) > 22 {
			vps, sps, pps, err := h265.ExtractVPSSPSPPSFromHVCRecord(videoSeqHeader)
			if err == nil {
				startCode := []byte{0x00, 0x00, 0x00, 0x01}
				if vps != nil {
					m.videoSeqHeader = append(m.videoSeqHeader, startCode...)
					m.videoSeqHeader = append(m.videoSeqHeader, vps...)
				}
				if sps != nil {
					m.videoSeqHeader = append(m.videoSeqHeader, startCode...)
					m.videoSeqHeader = append(m.videoSeqHeader, sps...)
				}
				if pps != nil {
					m.videoSeqHeader = append(m.videoSeqHeader, startCode...)
					m.videoSeqHeader = append(m.videoSeqHeader, pps...)
				}
			}
		}
	case avframe.CodecAV1:
		// AV1 OBUs are passed through directly
		m.videoSeqHeader = nil
	}

	// Parse audio sequence header
	if audioCodec == avframe.CodecAAC && len(audioSeqHeader) >= 2 {
		info, err := aac.ParseAudioSpecificConfig(audioSeqHeader)
		if err == nil {
			m.aacInfo = info
		}
		m.audioSeqHeader = audioSeqHeader
	}

	// Pre-build PAT/PMT
	videoStreamType := StreamType(videoCodec)
	audioStreamType := StreamType(audioCodec)
	m.pat = BuildPAT(m.patContinuity)
	m.patContinuity = (m.patContinuity + 1) & 0x0F
	m.pmt = BuildPMT(videoStreamType, audioStreamType, m.pmtContinuity)
	m.pmtContinuity = (m.pmtContinuity + 1) & 0x0F

	return m
}

// WriteFrame converts an AVFrame to TS packets.
// Returns []byte of N*188 bytes. Returns nil for sequence header frames.
func (m *Muxer) WriteFrame(frame *avframe.AVFrame) []byte {
	// Skip sequence header frames (already parsed in constructor)
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		return nil
	}

	if frame.MediaType.IsVideo() {
		return m.writeVideoFrame(frame)
	}
	if frame.MediaType.IsAudio() {
		return m.writeAudioFrame(frame)
	}
	return nil
}

func (m *Muxer) writeVideoFrame(frame *avframe.AVFrame) []byte {
	var result []byte

	// Prepend PAT+PMT before keyframes
	if frame.FrameType.IsKeyframe() {
		result = append(result, m.rebuildPATandPMT()...)
	}

	// Build video payload
	var payload []byte

	switch m.videoCodec {
	case avframe.CodecH264:
		annexB := h264.AVCCToAnnexB(frame.Payload)
		if frame.FrameType.IsKeyframe() && m.videoSeqHeader != nil {
			// Prepend SPS/PPS before keyframe
			payload = append(payload, m.videoSeqHeader...)
		}
		payload = append(payload, annexB...)
	case avframe.CodecH265:
		annexB := h265.HVCCToAnnexB(frame.Payload)
		if frame.FrameType.IsKeyframe() && m.videoSeqHeader != nil {
			payload = append(payload, m.videoSeqHeader...)
		}
		payload = append(payload, annexB...)
	case avframe.CodecAV1:
		// AV1 OBUs passed through directly
		payload = frame.Payload
	default:
		payload = frame.Payload
	}

	// Build PES
	pesHeader := BuildPESHeader(0xE0, frame.PTS, frame.DTS, len(payload))
	pesData := append(pesHeader, payload...)

	// Build packetization options: embed PCR in first TS packet if needed,
	// set random_access_indicator for keyframes. This avoids separate
	// PCR-only packets that cause continuity counter issues with demuxers.
	opts := &PESPacketizeOptions{
		RandomAccess: frame.FrameType.IsKeyframe(),
		PCR:          -1, // no PCR by default
	}
	if m.shouldInsertPCR(frame.DTS) {
		opts.PCR = frame.DTS
		m.lastPCR = frame.DTS
	}

	// Packetize into TS
	tsPackets := PacketizePES(PIDVideo, pesData, &m.videoContinuity, opts)
	result = append(result, tsPackets...)

	return result
}

func (m *Muxer) writeAudioFrame(frame *avframe.AVFrame) []byte {
	var payload []byte

	switch m.audioCodec {
	case avframe.CodecAAC:
		if m.aacInfo != nil {
			// Prepend ADTS header
			adts := aac.BuildADTSHeader(m.aacInfo, len(frame.Payload))
			payload = append(payload, adts...)
		}
		payload = append(payload, frame.Payload...)
	case avframe.CodecMP3:
		// MP3 is self-delimiting, pass through
		payload = frame.Payload
	case avframe.CodecOpus:
		payload = frame.Payload
	default:
		payload = frame.Payload
	}

	// Build PES (audio uses PTS only, no DTS)
	pesHeader := BuildPESHeader(0xC0, frame.PTS, frame.PTS, len(payload))
	pesData := append(pesHeader, payload...)

	tsPackets := PacketizePES(PIDAudio, pesData, &m.audioContinuity, nil)
	return tsPackets
}

func (m *Muxer) shouldInsertPCR(dts int64) bool {
	if m.lastPCR < 0 {
		return true
	}
	return dts-m.lastPCR >= MaxPCRInterval
}


// WritePATAndPMT generates fresh PAT and PMT packets.
// Used by LL-HLS to insert PAT/PMT at partial segment boundaries.
func (m *Muxer) WritePATAndPMT() []byte {
	return m.rebuildPATandPMT()
}

func (m *Muxer) rebuildPATandPMT() []byte {
	pat := BuildPAT(m.patContinuity)
	m.patContinuity = (m.patContinuity + 1) & 0x0F
	pmt := BuildPMT(StreamType(m.videoCodec), StreamType(m.audioCodec), m.pmtContinuity)
	m.pmtContinuity = (m.pmtContinuity + 1) & 0x0F
	return append(pat, pmt...)
}
