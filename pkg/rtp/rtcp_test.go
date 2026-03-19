package rtp

import "testing"

func TestBuildParseSR(t *testing.T) {
	data := BuildSR(0x12345678, 0xAABBCCDD00112233, 90000, 100, 50000)
	sr, err := ParseSR(data)
	if err != nil {
		t.Fatalf("ParseSR: %v", err)
	}
	if sr.SSRC != 0x12345678 {
		t.Errorf("SSRC = 0x%x", sr.SSRC)
	}
	if sr.NTPTime != 0xAABBCCDD00112233 {
		t.Errorf("NTPTime = 0x%x", sr.NTPTime)
	}
	if sr.RTPTime != 90000 {
		t.Errorf("RTPTime = %d", sr.RTPTime)
	}
	if sr.PacketCount != 100 {
		t.Errorf("PacketCount = %d", sr.PacketCount)
	}
	if sr.OctetCount != 50000 {
		t.Errorf("OctetCount = %d", sr.OctetCount)
	}
}

func TestBuildParseRR(t *testing.T) {
	reports := []ReceptionReport{
		{SSRC: 0xAABBCCDD, FractionLost: 10, TotalLost: 5, HighestSeq: 1000, Jitter: 20},
	}
	data := BuildRR(0x12345678, reports)
	rr, err := ParseRR(data)
	if err != nil {
		t.Fatalf("ParseRR: %v", err)
	}
	if rr.SSRC != 0x12345678 {
		t.Errorf("SSRC = 0x%x", rr.SSRC)
	}
	if len(rr.Reports) != 1 {
		t.Fatalf("Reports count = %d", len(rr.Reports))
	}
	if rr.Reports[0].SSRC != 0xAABBCCDD {
		t.Errorf("Report SSRC = 0x%x", rr.Reports[0].SSRC)
	}
}
