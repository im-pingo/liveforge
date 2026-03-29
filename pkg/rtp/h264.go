package rtp

import (
	"encoding/binary"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pioncodecs "github.com/pion/rtp/v2/codecs"
	pionrtp "github.com/pion/rtp/v2"
)

// H264Packetizer packetizes H.264 NAL units into RTP packets.
// Delegates to pion's H264Payloader for correct Single NAL / FU-A handling.
type H264Packetizer struct{}

// Packetize splits an AVFrame into RTP packets using pion's H264Payloader.
// Accepts payloads in Annex-B, AVCC, or AVCDecoderConfigurationRecord format.
func (p *H264Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	if mtu < 3 {
		return nil, fmt.Errorf("MTU too small: %d", mtu)
	}

	annexB := ToAnnexB(frame.Payload, frame.FrameType == avframe.FrameTypeSequenceHeader)

	payloader := &pioncodecs.H264Payloader{}
	payloads := payloader.Payload(mtu-12, annexB) // -12 for RTP header overhead
	if len(payloads) == 0 {
		return nil, nil
	}

	pkts := make([]*pionrtp.Packet, len(payloads))
	for i, pp := range payloads {
		pkts[i] = &pionrtp.Packet{
			Header: pionrtp.Header{
				Marker: i == len(payloads)-1,
			},
			Payload: pp,
		}
	}
	return pkts, nil
}

// ToAnnexB converts payload to Annex-B format if it isn't already.
// Handles: Annex-B (pass-through), AVCC (4-byte length prefix), AVCDecoderConfigurationRecord.
func ToAnnexB(data []byte, isSequenceHeader bool) []byte {
	if len(data) == 0 {
		return data
	}

	// AVCDecoderConfigurationRecord: version=1 at byte 0
	if isSequenceHeader && len(data) >= 8 && data[0] == 1 {
		return avcDecoderConfigToAnnexB(data)
	}

	// Try AVCC first (4-byte big-endian length + NAL). AVCC lengths in the
	// range 256–511 produce a prefix 00 00 01 XX which is a false positive
	// for the 3-byte Annex-B start code, so AVCC must be checked first.
	if result := avccToAnnexB(data); result != nil {
		return result
	}

	// Check if already Annex-B (starts with 00 00 00 01 or 00 00 01)
	if hasAnnexBStartCode(data) {
		return data
	}

	// Fallback: treat as single raw NAL, prepend start code
	result := make([]byte, 0, 4+len(data))
	result = append(result, 0x00, 0x00, 0x00, 0x01)
	result = append(result, data...)
	return result
}

// hasAnnexBStartCode checks if data begins with Annex-B start code.
func hasAnnexBStartCode(data []byte) bool {
	if len(data) >= 4 && data[0] == 0 && data[1] == 0 && data[2] == 0 && data[3] == 1 {
		return true
	}
	if len(data) >= 3 && data[0] == 0 && data[1] == 0 && data[2] == 1 {
		return true
	}
	return false
}

// avccToAnnexB converts AVCC format (4-byte length prefix + NAL) to Annex-B.
// Returns nil if the data doesn't look like valid AVCC.
func avccToAnnexB(data []byte) []byte {
	// Validate: walk the data checking that lengths are consistent
	offset := 0
	for offset+4 <= len(data) {
		nalLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if nalLen <= 0 || offset+nalLen > len(data) {
			return nil
		}
		offset += nalLen
	}
	if offset != len(data) {
		return nil
	}

	// Convert
	result := make([]byte, 0, len(data))
	offset = 0
	for offset+4 <= len(data) {
		nalLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
		result = append(result, 0x00, 0x00, 0x00, 0x01)
		result = append(result, data[offset:offset+nalLen]...)
		offset += nalLen
	}
	return result
}

// avcDecoderConfigToAnnexB extracts SPS/PPS from AVCDecoderConfigurationRecord
// and returns Annex-B formatted data.
func avcDecoderConfigToAnnexB(data []byte) []byte {
	if len(data) < 8 {
		return data
	}
	var result []byte
	offset := 5

	// SPS
	if offset >= len(data) {
		return result
	}
	numSPS := int(data[offset] & 0x1F)
	offset++
	for range numSPS {
		if offset+2 > len(data) {
			return result
		}
		spsLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+spsLen > len(data) {
			return result
		}
		result = append(result, 0x00, 0x00, 0x00, 0x01)
		result = append(result, data[offset:offset+spsLen]...)
		offset += spsLen
	}

	// PPS
	if offset >= len(data) {
		return result
	}
	numPPS := int(data[offset])
	offset++
	for range numPPS {
		if offset+2 > len(data) {
			return result
		}
		ppsLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+ppsLen > len(data) {
			return result
		}
		result = append(result, 0x00, 0x00, 0x00, 0x01)
		result = append(result, data[offset:offset+ppsLen]...)
		offset += ppsLen
	}
	return result
}

// AnnexBToAVCC converts Annex-B format (start codes) to AVCC format (4-byte length prefix).
// This is needed because the internal AVFrame format uses AVCC to match RTMP/FLV conventions.
func AnnexBToAVCC(data []byte) []byte {
	var result []byte
	forEachNAL(data, func(nal []byte) {
		if len(nal) == 0 {
			return
		}
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(nal)))
		result = append(result, lenBuf[:]...)
		result = append(result, nal...)
	})
	return result
}

// BuildAVCDecoderConfig builds an AVCDecoderConfigurationRecord from Annex-B data
// containing SPS and PPS NAL units. Returns nil if SPS or PPS not found.
func BuildAVCDecoderConfig(annexB []byte) []byte {
	var sps, pps []byte
	forEachNAL(annexB, func(nal []byte) {
		if len(nal) == 0 {
			return
		}
		nalType := nal[0] & 0x1F
		if nalType == 7 && sps == nil {
			sps = nal
		} else if nalType == 8 && pps == nil {
			pps = nal
		}
	})
	if sps == nil || pps == nil {
		return nil
	}
	return buildAVCDecoderConfigFromNALs(sps, pps)
}

// buildAVCDecoderConfigFromNALs builds AVCDecoderConfigurationRecord from raw SPS/PPS bytes.
func buildAVCDecoderConfigFromNALs(sps, pps []byte) []byte {
	if len(sps) < 4 {
		return nil
	}
	// AVCDecoderConfigurationRecord
	config := make([]byte, 0, 11+len(sps)+len(pps))
	config = append(config, 1)      // version
	config = append(config, sps[1]) // profile
	config = append(config, sps[2]) // profile compatibility
	config = append(config, sps[3]) // level
	config = append(config, 0xFF)   // lengthSizeMinusOne=3 | reserved 6 bits
	config = append(config, 0xE1)   // numSPS=1 | reserved 3 bits
	// SPS length + data
	config = append(config, byte(len(sps)>>8), byte(len(sps)))
	config = append(config, sps...)
	// PPS count + length + data
	config = append(config, 1) // numPPS=1
	config = append(config, byte(len(pps)>>8), byte(len(pps)))
	config = append(config, pps...)
	return config
}

// ExtractSPSPPS extracts raw SPS and PPS NAL bytes from an AVCDecoderConfigurationRecord.
func ExtractSPSPPS(config []byte) (sps, pps []byte) {
	if len(config) < 8 || config[0] != 1 {
		return nil, nil
	}
	offset := 5
	numSPS := int(config[offset] & 0x1F)
	offset++
	for range numSPS {
		if offset+2 > len(config) {
			return
		}
		spsLen := int(binary.BigEndian.Uint16(config[offset : offset+2]))
		offset += 2
		if offset+spsLen > len(config) {
			return
		}
		if sps == nil {
			sps = config[offset : offset+spsLen]
		}
		offset += spsLen
	}
	if offset >= len(config) {
		return
	}
	numPPS := int(config[offset])
	offset++
	for range numPPS {
		if offset+2 > len(config) {
			return
		}
		ppsLen := int(binary.BigEndian.Uint16(config[offset : offset+2]))
		offset += 2
		if offset+ppsLen > len(config) {
			return
		}
		if pps == nil {
			pps = config[offset : offset+ppsLen]
		}
		offset += ppsLen
	}
	return
}

// H264Depacketizer reassembles RTP packets into H.264 NAL units.
// Delegates to pion's H264Packet for correct Single NAL / STAP-A / FU-A handling.
// Output is in AVCC format (4-byte length prefix) to match RTMP/FLV conventions.
type H264Depacketizer struct {
	pkt pioncodecs.H264Packet // pion's stateful depacketizer (tracks FU-A buffer)
}

// Depacketize processes one RTP packet and returns an AVFrame when a complete
// NAL unit (or set of NALs) is available. Returns (nil, nil) for intermediate
// FU-A fragments. Output payload is in AVCC format.
func (d *H264Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if len(pkt.Payload) == 0 {
		return nil, fmt.Errorf("empty RTP payload")
	}

	data, err := d.pkt.Unmarshal(pkt.Payload)
	if err != nil {
		return nil, err
	}
	// pion returns empty slice for intermediate FU-A fragments
	if len(data) == 0 {
		return nil, nil
	}

	// data is in Annex-B format from pion — classify first, then convert to AVCC
	frameType := classifyAnnexB(data)

	// For SequenceHeader (SPS/PPS only), build AVCDecoderConfigurationRecord
	if frameType == avframe.FrameTypeSequenceHeader {
		config := BuildAVCDecoderConfig(data)
		if config == nil {
			return nil, nil
		}
		return avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			frameType,
			0, 0,
			config,
		), nil
	}

	// For regular frames, convert Annex-B to AVCC (4-byte length prefix)
	avcc := AnnexBToAVCC(data)
	return avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		frameType,
		0, 0,
		avcc,
	), nil
}

// classifyAnnexB scans Annex-B data for NAL types and returns the frame type.
// Priority: Keyframe > SequenceHeader > Interframe
func classifyAnnexB(data []byte) avframe.FrameType {
	best := avframe.FrameTypeInterframe
	hasSlice := false

	forEachNAL(data, func(nal []byte) {
		if len(nal) == 0 {
			return
		}
		nalType := nal[0] & 0x1F
		switch nalType {
		case 5: // IDR
			best = avframe.FrameTypeKeyframe
			hasSlice = true
		case 1: // non-IDR slice
			hasSlice = true
		case 7, 8: // SPS, PPS
			if best != avframe.FrameTypeKeyframe {
				best = avframe.FrameTypeSequenceHeader
			}
		}
	})

	// If SPS/PPS came with slice data (e.g., SPS+PPS+IDR in one access unit),
	// the slice type takes precedence
	if hasSlice && best == avframe.FrameTypeSequenceHeader {
		best = avframe.FrameTypeKeyframe
	}

	return best
}

// forEachNAL calls fn for each NAL unit in Annex-B data.
// If no start codes are found, calls fn with the entire data.
func forEachNAL(data []byte, fn func([]byte)) {
	// Use the same start code detection as pion's emitNalus
	nextInd := func(nalu []byte, start int) (indStart int, indLen int) {
		zeroCount := 0
		for i, b := range nalu[start:] {
			if b == 0 {
				zeroCount++
				continue
			} else if b == 1 && zeroCount >= 2 {
				return start + i - zeroCount, zeroCount + 1
			}
			zeroCount = 0
		}
		return -1, -1
	}

	nextIndStart, nextIndLen := nextInd(data, 0)
	if nextIndStart == -1 {
		fn(data)
		return
	}
	for nextIndStart != -1 {
		prevStart := nextIndStart + nextIndLen
		nextIndStart, nextIndLen = nextInd(data, prevStart)
		if nextIndStart != -1 {
			fn(data[prevStart:nextIndStart])
		} else {
			fn(data[prevStart:])
		}
	}
}

