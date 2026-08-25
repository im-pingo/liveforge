package rtp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
	pionrtp "github.com/pion/rtp/v2"
)

func TestH265PacketizeSingleNAL(t *testing.T) {
	// Build a 100-byte NAL with a valid 2-byte header.
	// NAL type 19 (IDR_W_RADL): first byte = (19 << 1) = 0x26, second byte = 0x01 (TID=1).
	nal := make([]byte, 100)
	nal[0] = 0x26
	nal[1] = 0x01
	for i := 2; i < len(nal); i++ {
		nal[i] = byte(i)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH265,
		Payload:   nal,
	}

	p := &H265Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize failed: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(pkts))
	}
	if !pkts[0].Header.Marker {
		t.Error("expected Marker=true on single NAL packet")
	}
	if !bytes.Equal(pkts[0].Payload, nal) {
		t.Error("payload does not match original NAL")
	}
}

func TestH265PacketizeFU(t *testing.T) {
	// Build a 3000-byte NAL (exceeds DefaultMTU of 1400).
	nal := make([]byte, 3000)
	nal[0] = 0x26 // NAL type 19
	nal[1] = 0x01 // TID=1
	for i := 2; i < len(nal); i++ {
		nal[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH265,
		Payload:   nal,
	}

	p := &H265Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize failed: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected at least 3 FU packets, got %d", len(pkts))
	}

	// Check first packet has S bit set.
	fuHeader := pkts[0].Payload[2]
	if fuHeader&0x80 == 0 {
		t.Error("expected S bit set on first FU packet")
	}
	if fuHeader&0x40 != 0 {
		t.Error("expected E bit clear on first FU packet")
	}
	if pkts[0].Header.Marker {
		t.Error("expected Marker=false on first FU packet")
	}

	// Check last packet has E bit set and Marker=true.
	last := pkts[len(pkts)-1]
	lastFU := last.Payload[2]
	if lastFU&0x40 == 0 {
		t.Error("expected E bit set on last FU packet")
	}
	if lastFU&0x80 != 0 {
		t.Error("expected S bit clear on last FU packet")
	}
	if !last.Header.Marker {
		t.Error("expected Marker=true on last FU packet")
	}

	// Check middle packets have neither S nor E bit.
	for i := 1; i < len(pkts)-1; i++ {
		mid := pkts[i].Payload[2]
		if mid&0x80 != 0 || mid&0x40 != 0 {
			t.Errorf("packet %d: expected neither S nor E bit set, got 0x%02x", i, mid)
		}
		if pkts[i].Header.Marker {
			t.Errorf("packet %d: expected Marker=false", i)
		}
	}

	// Verify NAL type in PayloadHdr.
	for _, pkt := range pkts {
		nalTypeInHdr := (pkt.Payload[0] >> 1) & 0x3F
		if nalTypeInHdr != h265NALTypeFU {
			t.Errorf("expected NAL type %d in PayloadHdr, got %d", h265NALTypeFU, nalTypeInHdr)
		}
	}
}

func TestH265DepacketizeRoundTrip(t *testing.T) {
	// Build a 3000-byte NAL that will be fragmented.
	nal := make([]byte, 3000)
	nal[0] = 0x26 // NAL type 19
	nal[1] = 0x01 // TID=1
	for i := 2; i < len(nal); i++ {
		nal[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH265,
		Payload:   nal,
	}

	p := &H265Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize failed: %v", err)
	}

	d := &H265Depacketizer{}
	var result *avframe.AVFrame
	for i, pkt := range pkts {
		f, err := d.Depacketize(pkt)
		if err != nil {
			t.Fatalf("Depacketize failed on packet %d: %v", i, err)
		}
		if i < len(pkts)-1 {
			if f != nil {
				t.Fatalf("expected nil frame for intermediate packet %d", i)
			}
		} else {
			if f == nil {
				t.Fatal("expected non-nil frame for last packet")
			}
			result = f
		}
	}

	if len(result.Payload) != len(nal)+4 || binary.BigEndian.Uint32(result.Payload[:4]) != uint32(len(nal)) || !bytes.Equal(result.Payload[4:], nal) {
		t.Errorf("round-trip payload is not normalized HVCC: got %x", result.Payload)
	}
	if result.Codec != avframe.CodecH265 {
		t.Errorf("expected codec H265, got %v", result.Codec)
	}
	if result.FrameType != avframe.FrameTypeKeyframe {
		t.Errorf("expected keyframe, got %v", result.FrameType)
	}
}

func TestH265DepacketizeSingleNAL(t *testing.T) {
	// Single NAL packet depacketization.
	nal := []byte{0x26, 0x01, 0xAA, 0xBB, 0xCC}
	pkt := &pionrtp.Packet{
		Header:  pionrtp.Header{Marker: true},
		Payload: nal,
	}

	d := &H265Depacketizer{}
	f, err := d.Depacketize(pkt)
	if err != nil {
		t.Fatalf("Depacketize failed: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil frame")
	}
	if len(f.Payload) != len(nal)+4 || binary.BigEndian.Uint32(f.Payload[:4]) != uint32(len(nal)) || !bytes.Equal(f.Payload[4:], nal) {
		t.Errorf("payload is not normalized HVCC: %x", f.Payload)
	}
	if f.FrameType != avframe.FrameTypeKeyframe {
		t.Errorf("frame type = %v, want keyframe", f.FrameType)
	}
}

func TestH265DepacketizerClassifiesInterframe(t *testing.T) {
	nal := []byte{0x02, 0x01, 0xAA, 0xBB} // TRAIL_R, NAL type 1
	d := &H265Depacketizer{}
	f, err := d.Depacketize(&pionrtp.Packet{Payload: nal})
	if err != nil {
		t.Fatalf("Depacketize failed: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil frame")
	}
	if f.FrameType != avframe.FrameTypeInterframe {
		t.Errorf("frame type = %v, want interframe", f.FrameType)
	}
	if len(f.Payload) != len(nal)+4 || binary.BigEndian.Uint32(f.Payload[:4]) != uint32(len(nal)) || !bytes.Equal(f.Payload[4:], nal) {
		t.Errorf("payload is not normalized HVCC: %x", f.Payload)
	}
}

func TestH265DepacketizerBuildsSequenceHeaderFromAggregationPacket(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0C, 0x01}
	sps := mustDecodeH265TestHex(t, "420103016000000300b0000003000003005a0000a0050201e162023b914842e7e13d0bea1bd50feaa08f554a6a02020207f08041")
	pps := []byte{0x44, 0x01, 0xC0, 0xF7}
	payload := []byte{byte(48 << 1), 0x01} // RFC 7798 aggregation packet
	for _, nal := range [][]byte{vps, sps, pps} {
		payload = append(payload, byte(len(nal)>>8), byte(len(nal)))
		payload = append(payload, nal...)
	}

	d := &H265Depacketizer{}
	f, err := d.Depacketize(&pionrtp.Packet{Payload: payload})
	if err != nil {
		t.Fatalf("Depacketize failed: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil sequence header")
	}
	if f.FrameType != avframe.FrameTypeSequenceHeader {
		t.Fatalf("frame type = %v, want sequence header", f.FrameType)
	}
	gotVPS, gotSPS, gotPPS, err := h265.ExtractVPSSPSPPSFromHVCRecord(f.Payload)
	if err != nil {
		t.Fatalf("parse HEVCDecoderConfigurationRecord: %v", err)
	}
	if !bytes.Equal(gotVPS, vps) || !bytes.Equal(gotSPS, sps) || !bytes.Equal(gotPPS, pps) {
		t.Fatalf("sequence header parameter sets = %x/%x/%x, want %x/%x/%x", gotVPS, gotSPS, gotPPS, vps, sps, pps)
	}
}

func TestH265DepacketizerRefreshesSequenceHeaderAsParameterSetsArrive(t *testing.T) {
	d := &H265Depacketizer{}
	vps := []byte{0x40, 0x01, 0x0C, 0x01}
	sps := mustDecodeH265TestHex(t, "420103016000000300b0000003000003005a0000a0050201e162023b914842e7e13d0bea1bd50feaa08f554a6a02020207f08041")
	pps := []byte{0x44, 0x01, 0xC0, 0xF7}

	if f, err := d.Depacketize(&pionrtp.Packet{Payload: vps}); err != nil || f != nil {
		t.Fatalf("VPS before SPS = (%v, %v), want no frame", f, err)
	}
	if f, err := d.Depacketize(&pionrtp.Packet{Payload: sps}); err != nil || f == nil || f.FrameType != avframe.FrameTypeSequenceHeader {
		t.Fatalf("SPS result = (%v, %v), want sequence header", f, err)
	}
	f, err := d.Depacketize(&pionrtp.Packet{Payload: pps})
	if err != nil {
		t.Fatalf("PPS depacketize failed: %v", err)
	}
	if f == nil || f.FrameType != avframe.FrameTypeSequenceHeader {
		t.Fatalf("PPS result = %v, want refreshed sequence header", f)
	}
	gotVPS, gotSPS, gotPPS, err := h265.ExtractVPSSPSPPSFromHVCRecord(f.Payload)
	if err != nil {
		t.Fatalf("parse HEVCDecoderConfigurationRecord: %v", err)
	}
	if !bytes.Equal(gotVPS, vps) || !bytes.Equal(gotSPS, sps) || !bytes.Equal(gotPPS, pps) {
		t.Fatalf("refreshed parameter sets = %x/%x/%x, want %x/%x/%x", gotVPS, gotSPS, gotPPS, vps, sps, pps)
	}
}

func TestH265DepacketizerReturnsSequenceHeaderAndMediaFromMixedAggregationPacket(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0C, 0x01}
	sps := mustDecodeH265TestHex(t, "420103016000000300b0000003000003005a0000a0050201e162023b914842e7e13d0bea1bd50feaa08f554a6a02020207f08041")
	pps := []byte{0x44, 0x01, 0xC0, 0xF7}
	aud := []byte{0x46, 0x01, 0x10}
	sei := []byte{0x4E, 0x01, 0x80}
	idr := []byte{0x26, 0x01, 0xAA, 0xBB}
	payload := []byte{byte(h265NALTypeAP << 1), 0x01}
	for _, nal := range [][]byte{vps, sps, pps, aud, sei, idr} {
		payload = append(payload, byte(len(nal)>>8), byte(len(nal)))
		payload = append(payload, nal...)
	}

	d := &H265Depacketizer{}
	multi, ok := any(d).(interface {
		DepacketizeFrames(*pionrtp.Packet) ([]*avframe.AVFrame, error)
	})
	if !ok {
		t.Fatal("H.265 depacketizer does not support multiple frames from one RTP packet")
	}
	frames, err := multi.DepacketizeFrames(&pionrtp.Packet{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("mixed H.265 AP produced %d frames, want sequence header and media", len(frames))
	}
	if frames[0].FrameType != avframe.FrameTypeSequenceHeader {
		t.Fatalf("first frame type = %v, want sequence header", frames[0].FrameType)
	}
	if frames[1].FrameType != avframe.FrameTypeKeyframe {
		t.Fatalf("second frame type = %v, want keyframe", frames[1].FrameType)
	}
	gotVPS, gotSPS, gotPPS, err := h265.ExtractVPSSPSPPSFromHVCRecord(frames[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotVPS, vps) || !bytes.Equal(gotSPS, sps) || !bytes.Equal(gotPPS, pps) {
		t.Fatal("mixed H.265 AP sequence header lost parameter sets")
	}
	if !bytes.Contains(frames[1].Payload, idr) {
		t.Fatal("mixed H.265 AP media frame lost the IDR NAL")
	}
}

func TestVideoToAnnexBH265DecoderConfig(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0c, 0x01}
	sps := []byte{0x42, 0x01, 0x01, 0x60}
	pps := []byte{0x44, 0x01, 0xc0, 0xf7}

	config := make([]byte, 22)
	config[0] = 1 // HEVCDecoderConfigurationRecord configurationVersion
	config = append(config, 3)
	for _, nal := range []struct {
		typ  byte
		data []byte
	}{
		{h265.NALTypeVPS, vps},
		{h265.NALTypeSPS, sps},
		{h265.NALTypePPS, pps},
	} {
		config = append(config, nal.typ, 0, 1, 0, byte(len(nal.data)))
		config = append(config, nal.data...)
	}

	want := []byte{
		0, 0, 0, 1, 0x40, 0x01, 0x0c, 0x01,
		0, 0, 0, 1, 0x42, 0x01, 0x01, 0x60,
		0, 0, 0, 1, 0x44, 0x01, 0xc0, 0xf7,
	}
	got := VideoToAnnexB(avframe.CodecH265, config, true)
	if !bytes.Equal(got, want) {
		t.Fatalf("H.265 decoder config to Annex-B = %x, want %x", got, want)
	}
}

func mustDecodeH265TestHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
