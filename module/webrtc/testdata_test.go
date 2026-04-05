package webrtc

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// vp8Frame represents a single VP8 frame extracted from an IVF file.
type vp8Frame struct {
	payload    []byte
	isKeyframe bool
	timestamp  uint64
}

// loadVP8TestFixture reads an IVF file containing VP8 frames and returns
// the raw frame payloads ready for feeding into the WebRTC pipeline.
func loadVP8TestFixture(t *testing.T, path string) []vp8Frame {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read VP8 fixture: %v", err)
	}

	// IVF header: 32 bytes.
	if len(data) < 32 {
		t.Fatal("IVF file too short for header")
	}
	if string(data[0:4]) != "DKIF" {
		t.Fatalf("not an IVF file: magic=%q", string(data[0:4]))
	}
	codec := string(data[8:12])
	if codec != "VP80" {
		t.Fatalf("expected VP80 codec, got %q", codec)
	}
	headerSize := binary.LittleEndian.Uint16(data[6:8])
	numFrames := binary.LittleEndian.Uint32(data[24:28])
	t.Logf("IVF: codec=%s, frames=%d, headerSize=%d", codec, numFrames, headerSize)

	// Parse frame data.
	var frames []vp8Frame
	offset := int(headerSize)
	for offset+12 <= len(data) {
		frameSize := binary.LittleEndian.Uint32(data[offset : offset+4])
		timestamp := binary.LittleEndian.Uint64(data[offset+4 : offset+12])
		offset += 12

		if offset+int(frameSize) > len(data) {
			break
		}
		payload := make([]byte, frameSize)
		copy(payload, data[offset:offset+int(frameSize)])
		offset += int(frameSize)

		// VP8 keyframe detection: first byte bit 0 == 0 means keyframe.
		isKey := len(payload) > 0 && (payload[0]&0x01) == 0

		frames = append(frames, vp8Frame{
			payload:    payload,
			isKeyframe: isKey,
			timestamp:  timestamp,
		})
	}

	if len(frames) == 0 {
		t.Fatal("no frames found in IVF file")
	}

	keyCount := 0
	for _, f := range frames {
		if f.isKeyframe {
			keyCount++
		}
	}
	t.Logf("Loaded VP8 fixture: %d frames (%d keyframes, %d bytes)",
		len(frames), keyCount, len(data))
	return frames
}

// expandVP8Frames generates a longer frame sequence by repeating the source
// frames cyclically, inserting keyframes at GOP boundaries.
func expandVP8Frames(source []vp8Frame, totalFrames int, frameDurMs int64, gopInterval int) []avframe.AVFrame {
	result := make([]avframe.AVFrame, totalFrames)
	// Find first keyframe and first interframe in source.
	var keyPayload, interPayload []byte
	for _, f := range source {
		if f.isKeyframe && keyPayload == nil {
			keyPayload = f.payload
		}
		if !f.isKeyframe && interPayload == nil {
			interPayload = f.payload
		}
	}
	if keyPayload == nil {
		panic("no keyframe in VP8 source")
	}
	if interPayload == nil {
		interPayload = keyPayload // fallback: all keyframes
	}

	for i := 0; i < totalFrames; i++ {
		dts := int64(i) * frameDurMs
		isKey := (i % gopInterval) == 0
		ft := avframe.FrameTypeInterframe
		payload := interPayload
		if isKey {
			ft = avframe.FrameTypeKeyframe
			payload = keyPayload
		}
		result[i] = avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecVP8,
			FrameType: ft,
			Payload:   payload,
			DTS:       dts,
			PTS:       dts,
		}
	}
	return result
}

func init() {
	// Suppress "unused" warnings for format import.
	_ = fmt.Sprintf
}

// h264AccessUnit represents one H.264 access unit (frame) as AVCC-formatted NALUs.
type h264AccessUnit struct {
	avccPayload []byte
	isKeyframe  bool
}

// loadH264TestFixture reads a raw Annex-B H.264 file and returns access units
// in AVCC format suitable for feeding into the WebRTC pipeline.
//
// It also returns the SPS and PPS NALUs wrapped in an AVCDecoderConfigurationRecord.
func loadH264TestFixture(t *testing.T, path string) (seqHeader []byte, frames []h264AccessUnit) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read H.264 fixture: %v", err)
	}

	// Parse Annex-B NALUs (split on 0x00000001 or 0x000001 start codes).
	nalus := parseAnnexBNALUs(data)
	if len(nalus) == 0 {
		t.Fatal("no NALUs found in H.264 fixture")
	}

	var sps, pps []byte
	var currentAU [][]byte
	var currentIsKey bool

	flushAU := func() {
		if len(currentAU) == 0 {
			return
		}
		// Convert to AVCC: each NALU prefixed with 4-byte length.
		var avcc []byte
		for _, nalu := range currentAU {
			lenBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(lenBuf, uint32(len(nalu)))
			avcc = append(avcc, lenBuf...)
			avcc = append(avcc, nalu...)
		}
		frames = append(frames, h264AccessUnit{
			avccPayload: avcc,
			isKeyframe:  currentIsKey,
		})
		currentAU = nil
		currentIsKey = false
	}

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		nalType := nalu[0] & 0x1F

		switch nalType {
		case 7: // SPS
			sps = nalu
		case 8: // PPS
			pps = nalu
		case 5: // IDR slice
			// IDR starts a new access unit.
			flushAU()
			currentAU = append(currentAU, nalu)
			currentIsKey = true
		case 1: // Non-IDR slice
			// Non-IDR starts a new access unit.
			flushAU()
			currentAU = append(currentAU, nalu)
		case 6: // SEI — belongs to current AU
			currentAU = append(currentAU, nalu)
		default:
			// Other NALUs (AUD, etc.) — include in current AU.
			currentAU = append(currentAU, nalu)
		}
	}
	flushAU()

	if sps == nil || pps == nil {
		t.Fatal("H.264 fixture missing SPS or PPS")
	}

	// Build AVCDecoderConfigurationRecord.
	seqHeader = buildTestAVCConfigPayload(sps, pps)

	t.Logf("Loaded H.264 fixture: %d frames (%d bytes), SPS=%d PPS=%d",
		len(frames), len(data), len(sps), len(pps))
	return
}

// parseAnnexBNALUs splits an Annex-B byte stream into individual NALUs.
func parseAnnexBNALUs(data []byte) [][]byte {
	var nalus [][]byte
	i := 0
	for i < len(data) {
		// Find start code (0x00000001 or 0x000001).
		scLen := 0
		if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			scLen = 4
		} else if i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			scLen = 3
		} else {
			i++
			continue
		}

		naluStart := i + scLen
		// Find next start code or end of data.
		j := naluStart
		for j < len(data) {
			if j+4 <= len(data) && data[j] == 0 && data[j+1] == 0 && data[j+2] == 0 && data[j+3] == 1 {
				break
			}
			if j+3 <= len(data) && data[j] == 0 && data[j+1] == 0 && data[j+2] == 1 {
				break
			}
			j++
		}

		if j > naluStart {
			nalus = append(nalus, data[naluStart:j])
		}
		i = j
	}
	return nalus
}

// expandH264Frames generates a longer frame sequence by repeating the
// source frames cyclically, assigning sequential DTS/PTS values.
func expandH264Frames(source []h264AccessUnit, totalFrames int, frameDurMs int64) []avframe.AVFrame {
	result := make([]avframe.AVFrame, totalFrames)
	for i := 0; i < totalFrames; i++ {
		src := source[i%len(source)]
		ft := avframe.FrameTypeInterframe
		if src.isKeyframe || i == 0 {
			ft = avframe.FrameTypeKeyframe
		}
		dts := int64(i) * frameDurMs
		result[i] = avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			FrameType: ft,
			Payload:   src.avccPayload,
			DTS:       dts,
			PTS:       dts,
		}
	}
	return result
}
