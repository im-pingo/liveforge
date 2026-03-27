package ts

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
	"github.com/im-pingo/liveforge/pkg/rtp"
)

// Demuxer parses MPEG-TS packets and outputs AVFrames via a callback.
type Demuxer struct {
	pmtPID   uint16
	videoPID uint16
	audioPID uint16

	videoCodec avframe.CodecType
	audioCodec avframe.CodecType

	videoBuf []byte // PES accumulator for video
	audioBuf []byte // PES accumulator for audio

	videoSeqSent bool
	audioSeqSent bool

	remainder []byte // leftover bytes from previous Feed call

	callback func(*avframe.AVFrame)
}

// NewDemuxer creates a new TS demuxer that emits AVFrames to the callback.
func NewDemuxer(callback func(*avframe.AVFrame)) *Demuxer {
	return &Demuxer{callback: callback}
}

// Feed accepts arbitrary-length TS data and processes complete 188-byte packets.
// Leftover bytes are buffered for the next call.
func (d *Demuxer) Feed(data []byte) {
	if len(d.remainder) > 0 {
		data = append(d.remainder, data...)
		d.remainder = nil
	}

	for len(data) >= PacketSize {
		// Find sync byte
		if data[0] != SyncByte {
			// Scan for sync
			idx := -1
			for i := 1; i < len(data); i++ {
				if data[i] == SyncByte {
					idx = i
					break
				}
			}
			if idx < 0 {
				return
			}
			data = data[idx:]
			continue
		}

		if len(data) < PacketSize {
			break
		}

		d.parsePacket(data[:PacketSize])
		data = data[PacketSize:]
	}

	if len(data) > 0 {
		d.remainder = append(d.remainder, data...)
	}
}

// parsePacket processes a single 188-byte TS packet.
func (d *Demuxer) parsePacket(pkt []byte) {
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	pusi := (pkt[1] & 0x40) != 0
	adaptControl := (pkt[3] >> 4) & 0x03

	// Determine payload start offset
	offset := 4
	if adaptControl == 0x02 || adaptControl == 0x03 {
		// Adaptation field present
		if offset >= PacketSize {
			return
		}
		adaptLen := int(pkt[offset])
		offset += 1 + adaptLen
	}
	if adaptControl == 0x01 || adaptControl == 0x03 {
		// Payload present
		if offset >= PacketSize {
			return
		}
		payload := pkt[offset:]

		switch {
		case pid == PIDPat:
			d.parsePAT(payload, pusi)
		case pid == d.pmtPID && d.pmtPID != 0:
			d.parsePMT(payload, pusi)
		case pid == d.videoPID && d.videoPID != 0:
			d.handlePES(&d.videoBuf, pusi, payload)
		case pid == d.audioPID && d.audioPID != 0:
			d.handlePES(&d.audioBuf, pusi, payload)
		}
	}
}

// parsePAT extracts the PMT PID from a PAT section.
func (d *Demuxer) parsePAT(payload []byte, pusi bool) {
	if pusi && len(payload) > 0 {
		pointerField := int(payload[0])
		payload = payload[1+pointerField:]
	}
	// PAT: table_id(1) + section_syntax(2) + transport_stream_id(2) +
	//       version/current(1) + section_number(1) + last_section_number(1) = 8 bytes header
	if len(payload) < 12 {
		return
	}
	sectionLen := int(payload[1]&0x0F)<<8 | int(payload[2])
	if sectionLen < 9 || len(payload) < 3+sectionLen {
		return
	}
	// Program entries start at offset 8, each is 4 bytes, end is sectionLen+3-4 (CRC)
	entryStart := 8
	entryEnd := 3 + sectionLen - 4
	for i := entryStart; i+3 < entryEnd && i+3 < len(payload); i += 4 {
		programNum := uint16(payload[i])<<8 | uint16(payload[i+1])
		if programNum != 0 {
			d.pmtPID = uint16(payload[i+2]&0x1F)<<8 | uint16(payload[i+3])
			break
		}
	}
}

// parsePMT extracts video/audio PIDs and their codec types from a PMT section.
func (d *Demuxer) parsePMT(payload []byte, pusi bool) {
	if pusi && len(payload) > 0 {
		pointerField := int(payload[0])
		payload = payload[1+pointerField:]
	}
	if len(payload) < 12 {
		return
	}
	sectionLen := int(payload[1]&0x0F)<<8 | int(payload[2])
	if sectionLen < 13 || len(payload) < 3+sectionLen {
		return
	}

	// Program info length
	programInfoLen := int(payload[10]&0x0F)<<8 | int(payload[11])
	offset := 12 + programInfoLen
	endOffset := 3 + sectionLen - 4 // exclude CRC

	for offset+4 < endOffset && offset+4 < len(payload) {
		streamType := payload[offset]
		elemPID := uint16(payload[offset+1]&0x1F)<<8 | uint16(payload[offset+2])
		esInfoLen := int(payload[offset+3]&0x0F)<<8 | int(payload[offset+4])
		offset += 5 + esInfoLen

		codec := codecFromStreamType(streamType)
		if codec == 0 {
			continue
		}
		if codec.IsVideo() && d.videoPID == 0 {
			d.videoPID = elemPID
			d.videoCodec = codec
		} else if codec.IsAudio() && d.audioPID == 0 {
			d.audioPID = elemPID
			d.audioCodec = codec
		}
	}
}

// codecFromStreamType maps MPEG-TS stream_type to avframe.CodecType.
func codecFromStreamType(st byte) avframe.CodecType {
	switch st {
	case 0x1B:
		return avframe.CodecH264
	case 0x24:
		return avframe.CodecH265
	case 0x0F:
		return avframe.CodecAAC
	case 0x03, 0x04:
		return avframe.CodecMP3
	default:
		return 0
	}
}

// handlePES accumulates PES data. When a new PES starts (PUSI=1), flush the previous one.
func (d *Demuxer) handlePES(buf *[]byte, pusi bool, payload []byte) {
	if pusi {
		if len(*buf) > 0 {
			d.flushPES(buf)
		}
		*buf = append((*buf)[:0], payload...)
	} else {
		*buf = append(*buf, payload...)
	}
}

// flushPES parses accumulated PES data and emits AVFrame(s).
func (d *Demuxer) flushPES(buf *[]byte) {
	pesData := *buf
	*buf = (*buf)[:0]

	if len(pesData) < 9 {
		return
	}

	// Verify PES start code
	if pesData[0] != 0x00 || pesData[1] != 0x00 || pesData[2] != 0x01 {
		return
	}

	streamID := pesData[3]
	ptsDTSFlags := (pesData[7] >> 6) & 0x03
	headerDataLen := int(pesData[8])

	if len(pesData) < 9+headerDataLen {
		return
	}

	var pts, dts int64
	headerOff := 9

	if ptsDTSFlags >= 2 && headerOff+5 <= 9+headerDataLen {
		pts = readTimestamp(pesData[headerOff:])
		dts = pts
		headerOff += 5
	}
	if ptsDTSFlags == 3 && headerOff+5 <= 9+headerDataLen {
		dts = readTimestamp(pesData[headerOff:])
	}

	// Copy the payload so it is independent of the PES accumulation buffer,
	// which will be reused for the next PES packet.
	raw := pesData[9+headerDataLen:]
	if len(raw) == 0 {
		return
	}
	payload := make([]byte, len(raw))
	copy(payload, raw)

	isVideo := streamID >= 0xE0 && streamID <= 0xEF
	isAudio := streamID >= 0xC0 && streamID <= 0xDF

	if isVideo {
		d.processVideo(payload, dts, pts)
	} else if isAudio {
		d.processAudio(payload, dts, pts)
	}
}

// readTimestamp extracts a 33-bit PTS/DTS from 5 bytes.
func readTimestamp(buf []byte) int64 {
	if len(buf) < 5 {
		return 0
	}
	ts90 := int64(buf[0]&0x0E) << 29
	ts90 |= int64(buf[1]) << 22
	ts90 |= int64(buf[2]&0xFE) << 14
	ts90 |= int64(buf[3]) << 7
	ts90 |= int64(buf[4]&0xFE) >> 1
	// Convert from 90kHz to milliseconds
	return ts90 / 90
}

// processVideo converts Annex-B video payload to internal AVFrame format.
func (d *Demuxer) processVideo(payload []byte, dts, pts int64) {
	switch d.videoCodec {
	case avframe.CodecH264:
		d.processH264(payload, dts, pts)
	case avframe.CodecH265:
		d.processH265(payload, dts, pts)
	default:
		// Pass through for unknown/unsupported video codecs
		d.callback(avframe.NewAVFrame(
			avframe.MediaTypeVideo, d.videoCodec, avframe.FrameTypeInterframe, dts, pts, payload,
		))
	}
}

// processH264 handles H.264 Annex-B → AVCC conversion and sequence header detection.
func (d *Demuxer) processH264(payload []byte, dts, pts int64) {
	nalus := h264.ExtractNALUs(payload)
	if len(nalus) == 0 {
		return
	}

	// Check for SPS/PPS and keyframe
	hasSPS := false
	isKeyframe := false
	for _, nal := range nalus {
		if len(nal) == 0 {
			continue
		}
		nalType := nal[0] & 0x1F
		if nalType == h264.NALTypeSPS {
			hasSPS = true
		}
		if nalType == h264.NALTypeIDR {
			isKeyframe = true
		}
	}

	// Emit sequence header if SPS detected and not yet sent
	if hasSPS && !d.videoSeqSent {
		config := rtp.BuildAVCDecoderConfig(payload)
		if config != nil {
			d.videoSeqSent = true
			d.callback(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264,
				avframe.FrameTypeSequenceHeader, dts, pts, config,
			))
		}
	}

	// Convert Annex-B to AVCC
	avcc := rtp.AnnexBToAVCC(payload)
	if len(avcc) == 0 {
		return
	}

	frameType := avframe.FrameTypeInterframe
	if isKeyframe {
		frameType = avframe.FrameTypeKeyframe
	}

	d.callback(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, frameType, dts, pts, avcc,
	))
}

// processH265 handles H.265 Annex-B → HVCC conversion and sequence header detection.
func (d *Demuxer) processH265(payload []byte, dts, pts int64) {
	nalus := h265ExtractNALUs(payload)
	if len(nalus) == 0 {
		return
	}

	hasSPS := false
	isKeyframe := false
	for _, nal := range nalus {
		if len(nal) < 2 {
			continue
		}
		nalType := (nal[0] >> 1) & 0x3F
		if nalType == h265.NALTypeSPS {
			hasSPS = true
		}
		if nalType == h265.NALTypeIDRWLP || nalType == h265.NALTypeIDRNLP {
			isKeyframe = true
		}
	}

	if hasSPS && !d.videoSeqSent {
		config := h265.BuildHVCCDecoderConfig(payload)
		if config != nil {
			d.videoSeqSent = true
			d.callback(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH265,
				avframe.FrameTypeSequenceHeader, dts, pts, config,
			))
		}
	}

	hvcc := h265.AnnexBToHVCC(payload)
	if len(hvcc) == 0 {
		return
	}

	frameType := avframe.FrameTypeInterframe
	if isKeyframe {
		frameType = avframe.FrameTypeKeyframe
	}

	d.callback(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, frameType, dts, pts, hvcc,
	))
}

// h265ExtractNALUs splits Annex-B into individual NALs (same as H.264 logic).
func h265ExtractNALUs(data []byte) [][]byte {
	return h264.ExtractNALUs(data)
}

// processAudio handles audio payload conversion.
func (d *Demuxer) processAudio(payload []byte, dts, pts int64) {
	switch d.audioCodec {
	case avframe.CodecAAC:
		d.processAAC(payload, dts, pts)
	default:
		// MP3, Opus, etc. — pass through
		d.callback(avframe.NewAVFrame(
			avframe.MediaTypeAudio, d.audioCodec, avframe.FrameTypeInterframe, dts, pts, payload,
		))
	}
}

// processAAC strips ADTS headers and emits raw AAC frames.
func (d *Demuxer) processAAC(payload []byte, dts, pts int64) {
	// ADTS frames can be concatenated in a single PES
	offset := 0
	for offset < len(payload) {
		remaining := payload[offset:]
		info, headerLen, err := aac.ParseADTSHeader(remaining)
		if err != nil {
			break
		}

		// Extract frame length from ADTS header
		frameLen := int(remaining[3]&0x03)<<11 | int(remaining[4])<<3 | int(remaining[5]>>5)
		if frameLen < headerLen || offset+frameLen > len(payload) {
			break
		}

		rawAAC := remaining[headerLen:frameLen]

		// Emit sequence header on first AAC frame
		if !d.audioSeqSent {
			d.audioSeqSent = true
			asc := aac.BuildAudioSpecificConfig(info)
			d.callback(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecAAC,
				avframe.FrameTypeSequenceHeader, dts, pts, asc,
			))
		}

		d.callback(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC,
			avframe.FrameTypeInterframe, dts, pts, rawAAC,
		))

		offset += frameLen
	}
}

// Flush processes any remaining buffered PES data. Call this when the
// input stream ends to emit the last pending frames.
func (d *Demuxer) Flush() {
	if len(d.videoBuf) > 0 {
		d.flushPES(&d.videoBuf)
	}
	if len(d.audioBuf) > 0 {
		d.flushPES(&d.audioBuf)
	}
}

// CodecFromStreamType is the exported version for testing.
func CodecFromStreamType(st byte) avframe.CodecType {
	return codecFromStreamType(st)
}

// ReadTimestamp is the exported version for testing.
func ReadTimestamp(buf []byte) int64 {
	return readTimestamp(buf)
}
