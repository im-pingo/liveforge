package h264

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseSPS(t *testing.T) {
	// Minimal SPS for 1920x1080, profile 100 (High), level 4.0
	// This is a real-world SPS NAL unit (without the start code)
	sps := []byte{
		0x67, 0x64, 0x00, 0x28, 0xAC, 0xD9, 0x40, 0x78,
		0x02, 0x27, 0xE5, 0xC0, 0x44, 0x00, 0x00, 0x03,
		0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xF0, 0x3C,
		0x60, 0xC6, 0x58,
	}
	info, err := ParseSPS(sps)
	if err != nil {
		t.Fatalf("ParseSPS error: %v", err)
	}
	if info.Width != 1920 {
		t.Errorf("expected width 1920, got %d", info.Width)
	}
	if info.Height != 1080 {
		t.Errorf("expected height 1080, got %d", info.Height)
	}
	if info.Profile != 100 {
		t.Errorf("expected profile 100, got %d", info.Profile)
	}
}

func TestExtractNALUs(t *testing.T) {
	// Annex-B format with start codes
	data := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x01, 0x02,
		0x00, 0x00, 0x00, 0x01, 0x68, 0x03, 0x04}
	nalus := ExtractNALUs(data)
	if len(nalus) != 2 {
		t.Fatalf("expected 2 NALUs, got %d", len(nalus))
	}
	if nalus[0][0] != 0x67 {
		t.Errorf("first NALU type byte: expected 0x67, got 0x%02x", nalus[0][0])
	}
	if nalus[1][0] != 0x68 {
		t.Errorf("second NALU type byte: expected 0x68, got 0x%02x", nalus[1][0])
	}
}

func TestAVCCToAnnexB(t *testing.T) {
	nalu := []byte{0x65, 0x88, 0x00, 0x01}
	avcc := make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(avcc, uint32(len(nalu)))
	copy(avcc[4:], nalu)

	annexB := AVCCToAnnexB(avcc)
	expected := append([]byte{0x00, 0x00, 0x00, 0x01}, nalu...)
	if !bytes.Equal(annexB, expected) {
		t.Errorf("expected %x, got %x", expected, annexB)
	}
}

func TestAVCCToAnnexBMultiNALU(t *testing.T) {
	nalu1 := []byte{0x67, 0x64, 0x00, 0x28}
	nalu2 := []byte{0x68, 0xEE, 0x38, 0x80}
	var avcc []byte
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(nalu1)))
	avcc = append(avcc, buf...)
	avcc = append(avcc, nalu1...)
	binary.BigEndian.PutUint32(buf, uint32(len(nalu2)))
	avcc = append(avcc, buf...)
	avcc = append(avcc, nalu2...)

	annexB := AVCCToAnnexB(avcc)
	nalus := ExtractNALUs(annexB)
	if len(nalus) != 2 {
		t.Fatalf("expected 2 NALUs, got %d", len(nalus))
	}
}

func TestExtractSPSPPSFromAVCRecord(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x28, 0xAC, 0xD9, 0x40}
	pps := []byte{0x68, 0xEE, 0x38, 0x80}
	// Build minimal AVCDecoderConfigurationRecord
	var record []byte
	record = append(record, 1, 0x64, 0x00, 0x28) // version, profile, compat, level
	record = append(record, 0xFF)                  // lengthSizeMinusOne=3 (0xFF = 6 reserved bits + 11)
	record = append(record, 0xE1)                  // numSPS=1 (0xE1 = 3 reserved + 00001)
	spsLen := make([]byte, 2)
	binary.BigEndian.PutUint16(spsLen, uint16(len(sps)))
	record = append(record, spsLen...)
	record = append(record, sps...)
	record = append(record, 1) // numPPS=1
	ppsLen := make([]byte, 2)
	binary.BigEndian.PutUint16(ppsLen, uint16(len(pps)))
	record = append(record, ppsLen...)
	record = append(record, pps...)

	gotSPS, gotPPS, err := ExtractSPSPPSFromAVCRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSPS, sps) {
		t.Errorf("SPS mismatch")
	}
	if !bytes.Equal(gotPPS, pps) {
		t.Errorf("PPS mismatch")
	}
}
