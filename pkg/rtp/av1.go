package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// AV1Packetizer packetizes AV1 OBU data into RTP packets.
// Uses a simplified aggregation header per draft-ietf-avt-rtp-av1.
type AV1Packetizer struct{}

// Packetize splits AV1 OBU data into one or more RTP packets.
// Each packet carries a 1-byte aggregation header followed by OBU data.
//
// Single OBU that fits in MTU: header = 0x10 (W=1), Marker=true.
// Fragmented: first fragment header = 0x00 (Z=0), continuations = 0x80 (Z=1).
// Last fragment gets Marker=true.
func (p *AV1Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	data := frame.Payload
	if len(data) == 0 {
		return nil, fmt.Errorf("empty AV1 frame payload")
	}
	if mtu < 2 {
		return nil, fmt.Errorf("MTU too small: %d", mtu)
	}

	maxChunk := mtu - 1 // 1 byte for aggregation header

	// Single packet case.
	if len(data) <= maxChunk {
		payload := make([]byte, 1+len(data))
		payload[0] = 0x10 // W=1
		copy(payload[1:], data)
		pkt := &pionrtp.Packet{
			Header:  pionrtp.Header{Marker: true},
			Payload: payload,
		}
		return []*pionrtp.Packet{pkt}, nil
	}

	// Fragmentation.
	var pkts []*pionrtp.Packet
	offset := 0
	for offset < len(data) {
		end := offset + maxChunk
		if end > len(data) {
			end = len(data)
		}

		isFirst := offset == 0
		isLast := end == len(data)

		var header byte
		if isFirst {
			header = 0x00 // Z=0
		} else {
			header = 0x80 // Z=1 (continuation)
		}

		payload := make([]byte, 1+end-offset)
		payload[0] = header
		copy(payload[1:], data[offset:end])

		pkt := &pionrtp.Packet{
			Header:  pionrtp.Header{Marker: isLast},
			Payload: payload,
		}
		pkts = append(pkts, pkt)
		offset = end
	}

	return pkts, nil
}

// AV1Depacketizer reassembles RTP packets into AV1 frames.
type AV1Depacketizer struct {
	buf []byte
}

// Depacketize processes one RTP packet. It accumulates fragments and returns
// a completed AVFrame when the marker bit is set. Returns (nil, nil) for
// intermediate fragments.
func (d *AV1Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	payload := pkt.Payload
	if len(payload) < 2 {
		return nil, fmt.Errorf("AV1 RTP payload too short")
	}

	header := payload[0]
	zBit := header & 0x80

	if zBit == 0 {
		// Start of a new OBU / frame.
		d.buf = make([]byte, 0, len(payload)*4)
		d.buf = append(d.buf, payload[1:]...)
	} else {
		// Continuation fragment.
		d.buf = append(d.buf, payload[1:]...)
	}

	if pkt.Marker {
		data := d.buf
		d.buf = nil
		// AV1 keyframe detection: parse OBUs in the reassembled frame.
		// A keyframe contains an OBU_FRAME or OBU_FRAME_HEADER with
		// frame_type == KEY_FRAME (0). We scan for the first frame header OBU.
		ft := classifyAV1Frame(data)
		return avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecAV1,
			ft,
			0, 0,
			data,
		), nil
	}

	return nil, nil
}

// classifyAV1Frame scans OBUs in the frame data to detect keyframes.
// AV1 OBU format: obu_header (1-2 bytes) + optional size LEB128 + payload.
// A keyframe has an OBU_SEQUENCE_HEADER (type 1) typically preceding the
// OBU_FRAME (type 6) or OBU_FRAME_HEADER (type 3) with frame_type == KEY_FRAME.
// For simplicity, we treat any frame containing an OBU_SEQUENCE_HEADER as a keyframe,
// since encoders emit it at the start of each key picture.
func classifyAV1Frame(data []byte) avframe.FrameType {
	offset := 0
	for offset < len(data) {
		if offset >= len(data) {
			break
		}
		header := data[offset]
		obuType := (header >> 3) & 0x0F
		hasExtension := (header & 0x04) != 0
		hasSizeField := (header & 0x02) != 0
		offset++ // past obu_header

		if hasExtension {
			if offset >= len(data) {
				break
			}
			offset++ // skip extension byte
		}

		if obuType == 1 { // OBU_SEQUENCE_HEADER
			return avframe.FrameTypeKeyframe
		}

		if hasSizeField {
			// Read LEB128 size
			obuSize := 0
			shift := 0
			for offset < len(data) {
				b := data[offset]
				offset++
				obuSize |= int(b&0x7F) << shift
				if (b & 0x80) == 0 {
					break
				}
				shift += 7
				if shift > 28 {
					return avframe.FrameTypeInterframe // corrupt
				}
			}
			offset += obuSize
		} else {
			// No size field — remaining data is the OBU payload.
			break
		}
	}
	return avframe.FrameTypeInterframe
}
