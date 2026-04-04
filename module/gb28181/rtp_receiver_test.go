package gb28181

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	pionrtp "github.com/pion/rtp/v2"
)

func TestReorderBufferInOrder(t *testing.T) {
	buf := newReorderBuffer(50)
	var emitted []uint16

	emit := func(p *pionrtp.Packet) {
		emitted = append(emitted, p.SequenceNumber)
	}

	for i := uint16(0); i < 10; i++ {
		buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: i}}, emit)
	}

	if len(emitted) != 10 {
		t.Fatalf("emitted %d packets, want 10", len(emitted))
	}
	for i, seq := range emitted {
		if seq != uint16(i) {
			t.Errorf("emitted[%d] = %d, want %d", i, seq, i)
		}
	}
}

func TestReorderBufferOutOfOrder(t *testing.T) {
	buf := newReorderBuffer(50)
	var emitted []uint16

	emit := func(p *pionrtp.Packet) {
		emitted = append(emitted, p.SequenceNumber)
	}

	// Send packets: 0, 2, 1, 3
	buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: 0}}, emit)
	buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: 2}}, emit)
	buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: 1}}, emit)
	buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: 3}}, emit)

	want := []uint16{0, 1, 2, 3}
	if len(emitted) != len(want) {
		t.Fatalf("emitted %d packets, want %d", len(emitted), len(want))
	}
	for i, seq := range emitted {
		if seq != want[i] {
			t.Errorf("emitted[%d] = %d, want %d", i, seq, want[i])
		}
	}
}

func TestReorderBufferGapSkipOnMaxDelay(t *testing.T) {
	buf := newReorderBuffer(5)
	var emitted []uint16

	emit := func(p *pionrtp.Packet) {
		emitted = append(emitted, p.SequenceNumber)
	}

	// Seq 0 arrives
	buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: 0}}, emit)
	// Seq 1 is lost, then fill buffer beyond maxDelay to trigger skip
	for i := uint16(2); i <= 8; i++ {
		buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: i}}, emit)
	}

	// Should have emitted seq 0, then after buffer overflow, skipped to seq 2 and flushed
	if len(emitted) < 2 {
		t.Fatalf("emitted %d packets, want >= 2", len(emitted))
	}
	if emitted[0] != 0 {
		t.Errorf("first emitted = %d, want 0", emitted[0])
	}
	// Verify remaining packets arrived in order after gap skip
	for i := 1; i < len(emitted)-1; i++ {
		if emitted[i+1] <= emitted[i] {
			t.Errorf("not in order: emitted[%d]=%d, emitted[%d]=%d", i, emitted[i], i+1, emitted[i+1])
		}
	}
}

func TestReorderBufferWraparound(t *testing.T) {
	buf := newReorderBuffer(50)
	var emitted []uint16

	emit := func(p *pionrtp.Packet) {
		emitted = append(emitted, p.SequenceNumber)
	}

	// Start near uint16 max
	start := uint16(65533)
	for i := 0; i < 6; i++ {
		seq := start + uint16(i) // wraps around at 65535
		buf.push(&pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: seq}}, emit)
	}

	if len(emitted) != 6 {
		t.Fatalf("emitted %d packets, want 6", len(emitted))
	}
}

func TestReadTCPRTPPacket(t *testing.T) {
	// Build an RTP packet
	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 42,
			Timestamp:      12345,
			SSRC:           99,
		},
		Payload: []byte{0x01, 0x02, 0x03, 0x04},
	}

	rtpData, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Prefix with 2-byte length (RFC 4571)
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(len(rtpData)))
	buf.Write(rtpData)

	// Use a pipe to simulate TCP connection
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		client.Write(buf.Bytes())
	}()

	got, err := ReadTCPRTPPacket(server)
	if err != nil {
		t.Fatalf("ReadTCPRTPPacket: %v", err)
	}

	if got.SequenceNumber != 42 {
		t.Errorf("SequenceNumber = %d, want 42", got.SequenceNumber)
	}
	if got.SSRC != 99 {
		t.Errorf("SSRC = %d, want 99", got.SSRC)
	}
	if len(got.Payload) != 4 {
		t.Errorf("payload len = %d, want 4", len(got.Payload))
	}
}

func TestReadTCPRTPPacketInvalidLength(t *testing.T) {
	// Length = 5 (less than 12 byte minimum RTP header)
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(5))
	buf.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		client.Write(buf.Bytes())
	}()

	_, err := ReadTCPRTPPacket(server)
	if err == nil {
		t.Error("expected error for invalid packet length")
	}
}

func TestSeqDiff(t *testing.T) {
	tests := []struct {
		a, b uint16
		want uint16
	}{
		{5, 3, 2},
		{0, 65535, 1},   // wraparound: 0 - 65535 = 1
		{100, 100, 0},
	}

	for _, tt := range tests {
		got := seqDiff(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("seqDiff(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
