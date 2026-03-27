package h265

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestHVCCToAnnexB(t *testing.T) {
	nalu := []byte{0x40, 0x01, 0x0C, 0x01} // VPS
	hvcc := make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(hvcc, uint32(len(nalu)))
	copy(hvcc[4:], nalu)

	annexB := HVCCToAnnexB(hvcc)
	expected := append([]byte{0x00, 0x00, 0x00, 0x01}, nalu...)
	if !bytes.Equal(annexB, expected) {
		t.Errorf("expected %x, got %x", expected, annexB)
	}
}

func TestHVCCToAnnexBMultiNALU(t *testing.T) {
	nalu1 := []byte{0x40, 0x01, 0x0C, 0x01}
	nalu2 := []byte{0x42, 0x01, 0x01, 0x01}
	var hvcc []byte
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(nalu1)))
	hvcc = append(hvcc, buf...)
	hvcc = append(hvcc, nalu1...)
	binary.BigEndian.PutUint32(buf, uint32(len(nalu2)))
	hvcc = append(hvcc, buf...)
	hvcc = append(hvcc, nalu2...)

	annexB := HVCCToAnnexB(hvcc)
	// Verify two start codes present
	count := bytes.Count(annexB, []byte{0x00, 0x00, 0x00, 0x01})
	if count != 2 {
		t.Errorf("expected 2 start codes, got %d", count)
	}
}

func TestExtractVPSSPSPPSFromHVCRecord(t *testing.T) {
	vpsNALU := []byte{0x40, 0x01, 0x0C, 0x01, 0xFF}
	spsNALU := []byte{0x42, 0x01, 0x01, 0x01, 0x60}
	ppsNALU := []byte{0x44, 0x01, 0xC0, 0xF7}

	// Build HEVCDecoderConfigurationRecord
	var record []byte
	// 22 bytes of fixed config (simplified)
	fixed := make([]byte, 22)
	fixed[0] = 1 // configurationVersion
	record = append(record, fixed...)
	record = append(record, 3) // numOfArrays

	// VPS array
	record = append(record, NALTypeVPS) // array_completeness=0, nal_unit_type=32
	nalCount := make([]byte, 2)
	binary.BigEndian.PutUint16(nalCount, 1)
	record = append(record, nalCount...)
	nalLen := make([]byte, 2)
	binary.BigEndian.PutUint16(nalLen, uint16(len(vpsNALU)))
	record = append(record, nalLen...)
	record = append(record, vpsNALU...)

	// SPS array
	record = append(record, NALTypeSPS)
	binary.BigEndian.PutUint16(nalCount, 1)
	record = append(record, nalCount...)
	binary.BigEndian.PutUint16(nalLen, uint16(len(spsNALU)))
	record = append(record, nalLen...)
	record = append(record, spsNALU...)

	// PPS array
	record = append(record, NALTypePPS)
	binary.BigEndian.PutUint16(nalCount, 1)
	record = append(record, nalCount...)
	binary.BigEndian.PutUint16(nalLen, uint16(len(ppsNALU)))
	record = append(record, nalLen...)
	record = append(record, ppsNALU...)

	vps, sps, pps, err := ExtractVPSSPSPPSFromHVCRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vps, vpsNALU) {
		t.Error("VPS mismatch")
	}
	if !bytes.Equal(sps, spsNALU) {
		t.Error("SPS mismatch")
	}
	if !bytes.Equal(pps, ppsNALU) {
		t.Error("PPS mismatch")
	}
}

func TestAnnexBToHVCC(t *testing.T) {
	// Build Annex-B with one NAL
	nal := []byte{0x40, 0x01, 0x0C, 0x01}
	annexB := append([]byte{0x00, 0x00, 0x00, 0x01}, nal...)

	hvcc := AnnexBToHVCC(annexB)
	// Expect 4-byte length + nal
	if len(hvcc) != 4+len(nal) {
		t.Fatalf("expected %d bytes, got %d", 4+len(nal), len(hvcc))
	}
	naluLen := int(binary.BigEndian.Uint32(hvcc[:4]))
	if naluLen != len(nal) {
		t.Errorf("expected NAL length %d, got %d", len(nal), naluLen)
	}
	if !bytes.Equal(hvcc[4:], nal) {
		t.Error("NAL data mismatch")
	}
}

func TestAnnexBToHVCCMultiNAL(t *testing.T) {
	nal1 := []byte{0x40, 0x01, 0x0C, 0x01} // VPS-like
	nal2 := []byte{0x42, 0x01, 0x01, 0x01} // SPS-like
	var annexB []byte
	annexB = append(annexB, 0x00, 0x00, 0x00, 0x01)
	annexB = append(annexB, nal1...)
	annexB = append(annexB, 0x00, 0x00, 0x00, 0x01)
	annexB = append(annexB, nal2...)

	hvcc := AnnexBToHVCC(annexB)
	expected := 2*(4) + len(nal1) + len(nal2)
	if len(hvcc) != expected {
		t.Fatalf("expected %d bytes, got %d", expected, len(hvcc))
	}
}

func TestBuildHVCCDecoderConfig(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0C, 0x01, 0xFF}
	sps := []byte{0x42, 0x01, 0x01, 0x01, 0x60}
	pps := []byte{0x44, 0x01, 0xC0, 0xF7}

	var annexB []byte
	sc := []byte{0x00, 0x00, 0x00, 0x01}
	annexB = append(annexB, sc...)
	annexB = append(annexB, vps...)
	annexB = append(annexB, sc...)
	annexB = append(annexB, sps...)
	annexB = append(annexB, sc...)
	annexB = append(annexB, pps...)

	config := BuildHVCCDecoderConfig(annexB)
	if config == nil {
		t.Fatal("expected non-nil config")
	}

	// Round-trip: extract from the config we just built
	gotVPS, gotSPS, gotPPS, err := ExtractVPSSPSPPSFromHVCRecord(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotVPS, vps) {
		t.Error("VPS round-trip mismatch")
	}
	if !bytes.Equal(gotSPS, sps) {
		t.Error("SPS round-trip mismatch")
	}
	if !bytes.Equal(gotPPS, pps) {
		t.Error("PPS round-trip mismatch")
	}
}

func TestBuildHVCCDecoderConfigNoSPS(t *testing.T) {
	// Only VPS, no SPS
	vps := []byte{0x40, 0x01, 0x0C, 0x01}
	annexB := append([]byte{0x00, 0x00, 0x00, 0x01}, vps...)
	config := BuildHVCCDecoderConfig(annexB)
	if config != nil {
		t.Error("expected nil config without SPS")
	}
}

func TestExtractVPSSPSPPSFromHVCRecordTooShort(t *testing.T) {
	_, _, _, err := ExtractVPSSPSPPSFromHVCRecord([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short record")
	}
}
