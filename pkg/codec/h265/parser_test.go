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

func TestExtractVPSSPSPPSFromHVCRecordTooShort(t *testing.T) {
	_, _, _, err := ExtractVPSSPSPPSFromHVCRecord([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short record")
	}
}
