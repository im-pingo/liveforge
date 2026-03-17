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

	// Insert PCR if needed
	if m.shouldInsertPCR(frame.DTS) {
		result = append(result, m.buildPCRPacket(frame.DTS)...)
		m.lastPCR = frame.DTS
	}

	// Packetize into TS
	tsPackets := PacketizePES(PIDVideo, pesData, &m.videoContinuity, frame.FrameType.IsKeyframe())
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

	tsPackets := PacketizePES(PIDAudio, pesData, &m.audioContinuity, false)
	return tsPackets
}

func (m *Muxer) shouldInsertPCR(dts int64) bool {
	if m.lastPCR < 0 {
		return true
	}
	return dts-m.lastPCR >= MaxPCRInterval
}

func (m *Muxer) buildPCRPacket(dts int64) []byte {
	pkt := make([]byte, PacketSize)
	pkt[0] = SyncByte
	pkt[1] = byte((PIDPCR >> 8) & 0x1F)
	pkt[2] = byte(PIDPCR & 0xFF)
	pkt[3] = 0x20 | (m.videoContinuity & 0x0F) // adaptation only, no payload

	// Adaptation field
	pkt[4] = PacketSize - 5 // adaptation_field_length
	pkt[5] = 0x10           // PCR flag

	// PCR: 33-bit base (90kHz) + 6 reserved + 9-bit extension
	pcr90 := dts * 90
	pkt[6] = byte(pcr90 >> 25)
	pkt[7] = byte(pcr90 >> 17)
	pkt[8] = byte(pcr90 >> 9)
	pkt[9] = byte(pcr90 >> 1)
	pkt[10] = byte((pcr90&1)<<7) | 0x7E // 6 reserved bits set to 1
	pkt[11] = 0x00                       // extension = 0

	// Fill rest with stuffing
	for i := 12; i < PacketSize; i++ {
		pkt[i] = 0xFF
	}

	return pkt
}

func (m *Muxer) rebuildPATandPMT() []byte {
	pat := BuildPAT(m.patContinuity)
	m.patContinuity = (m.patContinuity + 1) & 0x0F
	pmt := BuildPMT(StreamType(m.videoCodec), StreamType(m.audioCodec), m.pmtContinuity)
	m.pmtContinuity = (m.pmtContinuity + 1) & 0x0F
	return append(pat, pmt...)
}
