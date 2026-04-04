package ps

import (
	"encoding/binary"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	"github.com/im-pingo/liveforge/pkg/rtp"
)

// Demuxer parses MPEG-PS packs and extracts AVFrames.
type Demuxer struct {
	videoCodec avframe.CodecType
	audioCodec avframe.CodecType

	videoSeqSent bool
	audioSeqSent bool
}

// NewDemuxer creates a new PS demuxer.
func NewDemuxer() *Demuxer {
	return &Demuxer{}
}

// Feed parses a complete PS pack (one or more PES packets) and returns frames.
// The input should be a complete PS pack starting with pack header (00 00 01 BA).
func (d *Demuxer) Feed(data []byte) ([]*avframe.AVFrame, error) {
	if len(data) < 14 {
		return nil, fmt.Errorf("ps: data too short (%d bytes)", len(data))
	}

	var frames []*avframe.AVFrame
	offset := 0

	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}

		startCode := binary.BigEndian.Uint32(data[offset:])

		switch {
		case startCode == PackHeaderStartCode:
			n, err := d.skipPackHeader(data[offset:])
			if err != nil {
				return frames, err
			}
			offset += n

		case startCode == SystemHeaderStartCode:
			n, err := d.skipSystemHeader(data[offset:])
			if err != nil {
				return frames, err
			}
			offset += n

		case startCode == ProgramStreamMapStartCode:
			n, err := d.parsePSM(data[offset:])
			if err != nil {
				return frames, err
			}
			offset += n

		case isPESStartCode(startCode):
			pesFrames, n, err := d.parsePES(data[offset:])
			if err != nil {
				return frames, err
			}
			frames = append(frames, pesFrames...)
			offset += n

		default:
			// Skip unknown byte
			offset++
		}
	}

	return frames, nil
}

// VideoCodec returns the detected video codec.
func (d *Demuxer) VideoCodec() avframe.CodecType { return d.videoCodec }

// AudioCodec returns the detected audio codec.
func (d *Demuxer) AudioCodec() avframe.CodecType { return d.audioCodec }

// skipPackHeader skips a PS pack header and returns bytes consumed.
func (d *Demuxer) skipPackHeader(data []byte) (int, error) {
	if len(data) < 14 {
		return 0, fmt.Errorf("ps: pack header too short")
	}
	// MPEG-2 PS: 4 start code + 9 fixed + stuffing
	stuffLen := int(data[13] & 0x07)
	total := 14 + stuffLen
	if len(data) < total {
		return 0, fmt.Errorf("ps: pack header stuffing overflow")
	}
	return total, nil
}

// skipSystemHeader skips a system header.
func (d *Demuxer) skipSystemHeader(data []byte) (int, error) {
	if len(data) < 6 {
		return 0, fmt.Errorf("ps: system header too short")
	}
	headerLen := int(binary.BigEndian.Uint16(data[4:6]))
	total := 6 + headerLen
	if len(data) < total {
		return 0, fmt.Errorf("ps: system header length overflow")
	}
	return total, nil
}

// parsePSM parses a Program Stream Map to detect codecs.
func (d *Demuxer) parsePSM(data []byte) (int, error) {
	if len(data) < 6 {
		return 0, fmt.Errorf("ps: PSM too short")
	}
	psmLen := int(binary.BigEndian.Uint16(data[4:6]))
	total := 6 + psmLen
	if len(data) < total {
		return 0, fmt.Errorf("ps: PSM length overflow")
	}

	if psmLen < 6 {
		return total, nil
	}

	// Skip current_next_indicator(1) + reserved(1) + program_stream_info_length(2)
	offset := 8
	if offset+2 > len(data) {
		return total, nil
	}
	progInfoLen := int(binary.BigEndian.Uint16(data[offset:]))
	offset += 2 + progInfoLen

	if offset+2 > len(data) {
		return total, nil
	}
	esMapLen := int(binary.BigEndian.Uint16(data[offset:]))
	offset += 2

	end := offset + esMapLen
	if end > total {
		end = total
	}

	for offset+4 <= end {
		streamType := data[offset]
		streamID := data[offset+1]
		esInfoLen := int(binary.BigEndian.Uint16(data[offset+2:]))
		offset += 4 + esInfoLen

		codec := CodecFromStreamType(streamType)
		if codec == 0 {
			continue
		}
		if codec.IsVideo() && streamID >= 0xE0 && streamID <= 0xEF {
			d.videoCodec = codec
		} else if codec.IsAudio() {
			d.audioCodec = codec
		}
	}

	return total, nil
}

// parsePES parses a PES packet and returns frames and bytes consumed.
func (d *Demuxer) parsePES(data []byte) ([]*avframe.AVFrame, int, error) {
	if len(data) < 9 {
		return nil, 0, fmt.Errorf("ps: PES too short")
	}

	streamID := data[3]
	pesLen := int(binary.BigEndian.Uint16(data[4:6]))
	total := 6 + pesLen
	if pesLen == 0 {
		// Unbounded PES — consume rest of data
		total = len(data)
	}
	if len(data) < total {
		total = len(data)
	}

	// Skip padding/navigation streams
	if streamID == 0xBE || streamID == 0xBF {
		return nil, total, nil
	}

	ptsDtsFlags := (data[7] >> 6) & 0x03
	pesHeaderLen := int(data[8])
	payloadStart := 9 + pesHeaderLen
	if payloadStart > total {
		return nil, total, nil
	}

	var pts, dts int64
	if ptsDtsFlags >= 2 && len(data) >= 14 {
		pts = parsePESTimestamp(data[9:14])
		dts = pts
	}
	if ptsDtsFlags == 3 && len(data) >= 19 {
		dts = parsePESTimestamp(data[14:19])
	}

	// Convert 90kHz timestamps to milliseconds
	ptsMs := pts / 90
	dtsMs := dts / 90

	payload := data[payloadStart:total]
	if len(payload) == 0 {
		return nil, total, nil
	}

	// Determine media type from stream ID
	if streamID >= 0xE0 && streamID <= 0xEF {
		return d.handleVideoPayload(payload, dtsMs, ptsMs), total, nil
	}
	if streamID >= 0xC0 && streamID <= 0xDF {
		return d.handleAudioPayload(payload, dtsMs, ptsMs), total, nil
	}

	return nil, total, nil
}

// handleVideoPayload processes video PES payload and detects codec/keyframes.
// Payload arrives in Annex-B format from PS; we convert to AVCC to match
// the internal convention used by RTMP/FLV/HLS muxers.
// May return two frames on the first keyframe: sequence header + keyframe.
func (d *Demuxer) handleVideoPayload(payload []byte, dts, pts int64) []*avframe.AVFrame {
	// Auto-detect codec from NAL unit types if not yet known
	if d.videoCodec == 0 {
		d.videoCodec = detectVideoCodec(payload)
	}
	if d.videoCodec == 0 {
		return nil
	}

	isKey := isVideoKeyframe(d.videoCodec, payload)

	var frames []*avframe.AVFrame

	// Emit sequence header on first keyframe (AVCDecoderConfigurationRecord)
	if isKey && !d.videoSeqSent {
		config := rtp.BuildAVCDecoderConfig(payload)
		if config != nil {
			d.videoSeqSent = true
			frames = append(frames, avframe.NewAVFrame(
				avframe.MediaTypeVideo, d.videoCodec,
				avframe.FrameTypeSequenceHeader,
				dts, pts, config,
			))
		}
	}

	// Convert Annex-B to AVCC (4-byte length prefix per NAL)
	avcc := rtp.AnnexBToAVCC(payload)
	if len(avcc) == 0 {
		return frames
	}

	frameType := avframe.FrameTypeInterframe
	if isKey {
		frameType = avframe.FrameTypeKeyframe
	}

	frames = append(frames, avframe.NewAVFrame(
		avframe.MediaTypeVideo, d.videoCodec,
		frameType, dts, pts,
		avcc,
	))
	return frames
}

// handleAudioPayload processes audio PES payload.
// For AAC, ADTS headers are detected and stripped; an AudioSpecificConfig
// sequence header is emitted before the first raw AAC frame.
func (d *Demuxer) handleAudioPayload(payload []byte, dts, pts int64) []*avframe.AVFrame {
	if len(payload) == 0 {
		return nil
	}

	// Auto-detect codec from payload if not yet known (PSM may be absent)
	if d.audioCodec == 0 {
		d.audioCodec = detectAudioCodec(payload)
	}
	if d.audioCodec == 0 {
		return nil
	}

	if d.audioCodec == avframe.CodecAAC {
		return d.handleAACPayload(payload, dts, pts)
	}

	return []*avframe.AVFrame{avframe.NewAVFrame(
		avframe.MediaTypeAudio, d.audioCodec,
		avframe.FrameTypeKeyframe, dts, pts,
		payload,
	)}
}

// handleAACPayload parses ADTS-wrapped AAC and emits sequence header + raw frames.
func (d *Demuxer) handleAACPayload(payload []byte, dts, pts int64) []*avframe.AVFrame {
	var frames []*avframe.AVFrame
	offset := 0

	for offset+7 <= len(payload) {
		// Verify ADTS sync word
		if payload[offset] != 0xFF || (payload[offset+1]&0xF0) != 0xF0 {
			offset++
			continue
		}

		// ADTS frame length (13 bits)
		frameLen := (int(payload[offset+3]&0x03) << 11) |
			(int(payload[offset+4]) << 3) |
			(int(payload[offset+5]) >> 5)
		if frameLen < 7 || offset+frameLen > len(payload) {
			break
		}

		// Emit AudioSpecificConfig sequence header on first AAC frame
		if !d.audioSeqSent {
			asc := adtsToAudioSpecificConfig(payload[offset:])
			if asc != nil {
				d.audioSeqSent = true
				frames = append(frames, avframe.NewAVFrame(
					avframe.MediaTypeAudio, avframe.CodecAAC,
					avframe.FrameTypeSequenceHeader, dts, pts, asc,
				))
			}
		}

		// Strip ADTS header (7 or 9 bytes) and emit raw AAC
		headerLen := 7
		if (payload[offset+1] & 0x01) == 0 { // protection_absent=0 → CRC present
			headerLen = 9
		}
		if headerLen < frameLen {
			raw := payload[offset+headerLen : offset+frameLen]
			frames = append(frames, avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecAAC,
				avframe.FrameTypeKeyframe, dts, pts, raw,
			))
		}

		offset += frameLen
	}
	return frames
}

// detectAudioCodec detects the audio codec from raw payload bytes.
func detectAudioCodec(data []byte) avframe.CodecType {
	if len(data) >= 2 && data[0] == 0xFF && (data[1]&0xF0) == 0xF0 {
		return avframe.CodecAAC // ADTS sync word
	}
	// Fallback: assume G.711A for GB28181 when no ADTS detected
	return avframe.CodecG711A
}

// adtsToAudioSpecificConfig builds a 2-byte AudioSpecificConfig from an ADTS header.
// Layout: 5 bits objectType + 4 bits freqIndex + 4 bits channelConfig + padding
func adtsToAudioSpecificConfig(adts []byte) []byte {
	if len(adts) < 4 {
		return nil
	}
	// ADTS byte 2: profile(2) + sampling_freq_index(4) + private(1) + channel_config(1 bit)
	// ADTS byte 3: channel_config(2 bits) + ...
	objectType := ((adts[2] >> 6) & 0x03) + 1 // ADTS profile is objectType-1
	freqIndex := (adts[2] >> 2) & 0x0F
	channelConfig := ((adts[2] & 0x01) << 2) | ((adts[3] >> 6) & 0x03)

	// AudioSpecificConfig: objectType(5) + freqIndex(4) + channelConfig(4) + 0(3)
	asc := make([]byte, 2)
	asc[0] = (objectType << 3) | (freqIndex >> 1)
	asc[1] = (freqIndex << 7) | (channelConfig << 3)
	return asc
}

// detectVideoCodec attempts to identify the video codec from Annex-B NAL units.
func detectVideoCodec(data []byte) avframe.CodecType {
	nalus := h264.ExtractNALUs(data)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		// H.264 NAL type is in lower 5 bits of first byte
		h264Type := nalu[0] & 0x1F
		if h264Type >= 1 && h264Type <= 12 {
			return avframe.CodecH264
		}
		// H.265 NAL type is in bits 1-6 of first byte (shifted right by 1)
		h265Type := (nalu[0] >> 1) & 0x3F
		// Check for H.265-specific types (VPS=32, SPS=33, PPS=34)
		if h265Type >= 32 && h265Type <= 34 {
			return avframe.CodecH265
		}
		// H.265 IRAP types (BLA, IDR, CRA)
		if h265Type >= 16 && h265Type <= 23 {
			return avframe.CodecH265
		}
	}
	return 0
}

// isVideoKeyframe checks if the payload contains a keyframe.
func isVideoKeyframe(codec avframe.CodecType, data []byte) bool {
	nalus := h264.ExtractNALUs(data)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch codec {
		case avframe.CodecH264:
			nalType := nalu[0] & 0x1F
			if nalType == 5 { // IDR
				return true
			}
		case avframe.CodecH265:
			nalType := (nalu[0] >> 1) & 0x3F
			if nalType >= 16 && nalType <= 21 { // BLA, IDR, CRA
				return true
			}
		}
	}
	return false
}


// parsePESTimestamp parses a 5-byte PES timestamp field.
func parsePESTimestamp(data []byte) int64 {
	if len(data) < 5 {
		return 0
	}
	ts := int64(data[0]>>1&0x07) << 30
	ts |= int64(binary.BigEndian.Uint16(data[1:3])>>1) << 15
	ts |= int64(binary.BigEndian.Uint16(data[3:5]) >> 1)
	return ts
}

// isPESStartCode checks if a 4-byte value is a valid PES start code (00 00 01 xx).
func isPESStartCode(code uint32) bool {
	return (code >> 8) == 0x000001
}

