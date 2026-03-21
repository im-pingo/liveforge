package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

func TestH264PacketizeSingleNAL(t *testing.T) {
	nalData := make([]byte, 100)
	nalData[0] = 0x65 // IDR slice NAL type 5
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)
	p := &H264Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(pkts))
	}
	if !pkts[0].Marker {
		t.Error("expected marker bit on single NAL")
	}
	if !bytes.Equal(pkts[0].Payload, nalData) {
		t.Error("payload mismatch")
	}
}

func TestH264PacketizeFUA(t *testing.T) {
	nalData := make([]byte, 3000)
	nalData[0] = 0x65
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)
	p := &H264Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected >=3 FU-A packets, got %d", len(pkts))
	}
	// First FU-A has S bit
	if pkts[0].Payload[1]&0x80 == 0 {
		t.Error("first FU-A missing S bit")
	}
	// Last has marker
	if !pkts[len(pkts)-1].Marker {
		t.Error("last FU-A missing marker bit")
	}
	// Middle packets: no S or E bit
	for i := 1; i < len(pkts)-1; i++ {
		if pkts[i].Payload[1]&0xC0 != 0 {
			t.Errorf("middle packet %d has S or E bit set", i)
		}
	}
}

func TestH264PacketizeEmptyPayload(t *testing.T) {
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nil)
	p := &H264Packetizer{}
	_, err := p.Packetize(frame, 1400)
	if err == nil {
		t.Fatal("expected error for empty payload")
	}
}

func TestH264DepacketizeSingleNAL(t *testing.T) {
	d := &H264Depacketizer{}
	// Simulate single NAL packet (type 5 = IDR)
	payload := make([]byte, 100)
	payload[0] = 0x65 // NAL type 5
	pkt := &pionrtp.Packet{Header: pionrtp.Header{Marker: true}, Payload: payload}
	f, err := d.Depacketize(pkt)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if f == nil {
		t.Fatal("expected frame for single NAL")
	}
	// Depacketizer now outputs AVCC format: [4-byte length] + NAL
	expected := make([]byte, 4+len(payload))
	expected[0] = 0
	expected[1] = 0
	expected[2] = 0
	expected[3] = byte(len(payload))
	copy(expected[4:], payload)
	if !bytes.Equal(f.Payload, expected) {
		t.Errorf("payload mismatch: got %x..., want %x...", f.Payload[:min(8, len(f.Payload))], expected[:8])
	}
	if f.FrameType != avframe.FrameTypeKeyframe {
		t.Errorf("expected Keyframe, got %d", f.FrameType)
	}
}

func TestH264DepacketizeRoundTrip(t *testing.T) {
	nalData := make([]byte, 3000)
	for i := range nalData {
		nalData[i] = byte(i % 256)
	}
	nalData[0] = 0x65 // IDR NAL header
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)

	p := &H264Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &H264Depacketizer{}
	var result *avframe.AVFrame
	for _, pkt := range pkts {
		f, err := d.Depacketize(pkt)
		if err != nil {
			t.Fatalf("Depacketize: %v", err)
		}
		if f != nil {
			result = f
		}
	}
	if result == nil {
		t.Fatal("no frame reassembled")
	}
	// Depacketizer outputs AVCC: [4-byte length] + NAL
	expected := make([]byte, 4+len(nalData))
	expected[0] = byte(len(nalData) >> 24)
	expected[1] = byte(len(nalData) >> 16)
	expected[2] = byte(len(nalData) >> 8)
	expected[3] = byte(len(nalData))
	copy(expected[4:], nalData)
	if len(result.Payload) != len(expected) {
		t.Errorf("payload len = %d, want %d", len(result.Payload), len(expected))
	}
	if !bytes.Equal(result.Payload, expected) {
		t.Error("payload content mismatch after round-trip")
	}
}

func TestH264DepacketizeSTAPA(t *testing.T) {
	// Construct a STAP-A with SPS + PPS
	sps := []byte{0x67, 0x42, 0xC0, 0x0D} // NAL type 7
	pps := []byte{0x68, 0xCB, 0x83}        // NAL type 8

	// STAP-A format: [NAL header=24] [2-byte len][SPS] [2-byte len][PPS]
	payload := []byte{0x18} // NAL type 24
	payload = append(payload, byte(len(sps)>>8), byte(len(sps)))
	payload = append(payload, sps...)
	payload = append(payload, byte(len(pps)>>8), byte(len(pps)))
	payload = append(payload, pps...)

	d := &H264Depacketizer{}
	pkt := &pionrtp.Packet{Header: pionrtp.Header{Marker: true}, Payload: payload}
	f, err := d.Depacketize(pkt)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if f == nil {
		t.Fatal("expected frame for STAP-A")
	}
	if f.FrameType != avframe.FrameTypeSequenceHeader {
		t.Errorf("expected SequenceHeader, got %d", f.FrameType)
	}
	// Should be AVCDecoderConfigurationRecord format
	if len(f.Payload) < 8 || f.Payload[0] != 1 {
		t.Errorf("expected AVCDecoderConfigurationRecord, got first byte: %x", f.Payload[0])
	}
}

func TestAnnexBToAVCC(t *testing.T) {
	nal := []byte{0x65, 0x01, 0x02, 0x03}
	annexB := append([]byte{0x00, 0x00, 0x00, 0x01}, nal...)

	result := AnnexBToAVCC(annexB)
	expected := make([]byte, 4+len(nal))
	expected[3] = byte(len(nal))
	copy(expected[4:], nal)
	if !bytes.Equal(result, expected) {
		t.Errorf("AnnexBToAVCC failed: got %x, want %x", result, expected)
	}
}

func TestBuildAVCDecoderConfig(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xC0, 0x0D}
	pps := []byte{0x68, 0xCB, 0x83}
	annexB := []byte{0x00, 0x00, 0x00, 0x01}
	annexB = append(annexB, sps...)
	annexB = append(annexB, 0x00, 0x00, 0x00, 0x01)
	annexB = append(annexB, pps...)

	config := BuildAVCDecoderConfig(annexB)
	if config == nil {
		t.Fatal("expected non-nil config")
	}
	if config[0] != 1 {
		t.Errorf("expected version=1, got %d", config[0])
	}
	if config[1] != 0x42 {
		t.Errorf("expected profile=0x42, got 0x%x", config[1])
	}

	// Verify round-trip: config → Annex-B → config
	annexBResult := ToAnnexB(config, true)
	if !bytes.Equal(annexBResult, annexB) {
		t.Errorf("round-trip failed:\n  got  %x\n  want %x", annexBResult, annexB)
	}
}

func TestExtractSPSPPS(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xC0, 0x0D}
	pps := []byte{0x68, 0xCB, 0x83}
	annexB := append(append([]byte{0, 0, 0, 1}, sps...), append([]byte{0, 0, 0, 1}, pps...)...)
	config := BuildAVCDecoderConfig(annexB)

	gotSPS, gotPPS := ExtractSPSPPS(config)
	if !bytes.Equal(gotSPS, sps) {
		t.Errorf("SPS mismatch: got %x, want %x", gotSPS, sps)
	}
	if !bytes.Equal(gotPPS, pps) {
		t.Errorf("PPS mismatch: got %x, want %x", gotPPS, pps)
	}
}

func TestToAnnexBAVCC(t *testing.T) {
	// AVCC format: 4-byte length prefix + NAL
	nal := []byte{0x65, 0x01, 0x02, 0x03}
	avcc := make([]byte, 4+len(nal))
	avcc[0] = 0
	avcc[1] = 0
	avcc[2] = 0
	avcc[3] = byte(len(nal))
	copy(avcc[4:], nal)

	result := ToAnnexB(avcc, false)
	expected := append([]byte{0x00, 0x00, 0x00, 0x01}, nal...)
	if !bytes.Equal(result, expected) {
		t.Errorf("AVCC→AnnexB failed: got %x, want %x", result, expected)
	}
}

func TestToAnnexBAVCDecoderConfig(t *testing.T) {
	// Minimal AVCDecoderConfigurationRecord
	sps := []byte{0x67, 0x42, 0xC0, 0x0D}
	pps := []byte{0x68, 0xCB, 0x83}
	config := []byte{
		1,    // version
		0x42, // profile
		0xC0, // compat
		0x0D, // level
		0xFF, // lengthSizeMinusOne | reserved
		0xE1, // numSPS=1 | reserved
	}
	config = append(config, byte(len(sps)>>8), byte(len(sps)))
	config = append(config, sps...)
	config = append(config, 1) // numPPS=1
	config = append(config, byte(len(pps)>>8), byte(len(pps)))
	config = append(config, pps...)

	result := ToAnnexB(config, true)
	expected := []byte{0x00, 0x00, 0x00, 0x01}
	expected = append(expected, sps...)
	expected = append(expected, 0x00, 0x00, 0x00, 0x01)
	expected = append(expected, pps...)
	if !bytes.Equal(result, expected) {
		t.Errorf("AVCDecoderConfig→AnnexB failed:\n  got  %x\n  want %x", result, expected)
	}
}
