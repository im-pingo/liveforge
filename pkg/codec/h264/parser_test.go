package h264

import "testing"

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
