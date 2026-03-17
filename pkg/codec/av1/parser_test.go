package av1

import "testing"

func TestParseOBUHeader(t *testing.T) {
	// OBU type=1 (sequence_header), has_size=1: 0b00001010 = 0x0A
	header := []byte{0x0A}
	obuType, hasSize, err := ParseOBUHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if obuType != OBUSequenceHeader {
		t.Errorf("expected type %d, got %d", OBUSequenceHeader, obuType)
	}
	if !hasSize {
		t.Error("expected has_size=true")
	}
}

func TestParseOBUHeaderFrame(t *testing.T) {
	// OBU type=6 (frame), has_size=1: 0b00110010 = 0x32
	header := []byte{0x32}
	obuType, _, err := ParseOBUHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if obuType != OBUFrame {
		t.Errorf("expected type %d, got %d", OBUFrame, obuType)
	}
}

func TestParseOBUHeaderForbidden(t *testing.T) {
	_, _, err := ParseOBUHeader([]byte{0x80})
	if err == nil {
		t.Error("expected error for forbidden bit")
	}
}

func TestParseOBUHeaderTooShort(t *testing.T) {
	_, _, err := ParseOBUHeader([]byte{})
	if err == nil {
		t.Error("expected error for empty data")
	}
}

func TestBuildAV1CodecConfigRecord(t *testing.T) {
	seqHeader := []byte{0x0A, 0x05, 0x00, 0x00, 0x00, 0x24, 0xF8}
	record := BuildAV1CodecConfigRecord(seqHeader)
	if len(record) != 4+len(seqHeader) {
		t.Fatalf("expected %d bytes, got %d", 4+len(seqHeader), len(record))
	}
	if record[0]&0x80 != 0x80 {
		t.Error("marker bit not set")
	}
}
