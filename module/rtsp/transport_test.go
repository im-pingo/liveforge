package rtsp

import (
	"bufio"
	"bytes"
	"testing"
)

func TestWriteReadInterleaved(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{0x80, 0x60, 0x00, 0x01} // minimal RTP header
	err := WriteInterleaved(&buf, 0, payload)
	if err != nil {
		t.Fatalf("WriteInterleaved: %v", err)
	}
	// Verify raw bytes: $ + channel(0) + length(0,4) + payload
	raw := buf.Bytes()
	if raw[0] != '$' {
		t.Errorf("marker = 0x%02x", raw[0])
	}
	if raw[1] != 0 {
		t.Errorf("channel = %d", raw[1])
	}
	if raw[2] != 0 || raw[3] != 4 {
		t.Errorf("length = %d %d", raw[2], raw[3])
	}

	// Read back
	channel, data, err := ReadInterleaved(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadInterleaved: %v", err)
	}
	if channel != 0 {
		t.Errorf("channel = %d", channel)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("data mismatch")
	}
}

func TestInterleavedDifferentChannels(t *testing.T) {
	var buf bytes.Buffer
	WriteInterleaved(&buf, 2, []byte{0xAA, 0xBB})
	WriteInterleaved(&buf, 3, []byte{0xCC})

	r := bufio.NewReader(&buf)
	ch1, d1, err := ReadInterleaved(r)
	if err != nil {
		t.Fatalf("Read 1: %v", err)
	}
	if ch1 != 2 || len(d1) != 2 {
		t.Errorf("frame 1: ch=%d len=%d", ch1, len(d1))
	}

	ch2, d2, err := ReadInterleaved(r)
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	if ch2 != 3 || len(d2) != 1 {
		t.Errorf("frame 2: ch=%d len=%d", ch2, len(d2))
	}
}

func TestPortManagerAllocateRelease(t *testing.T) {
	pm := NewPortManager(10000, 10010)
	rtp1, rtcp1, err := pm.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if rtp1%2 != 0 {
		t.Errorf("RTP port not even: %d", rtp1)
	}
	if rtcp1 != rtp1+1 {
		t.Errorf("RTCP port = %d, want %d", rtcp1, rtp1+1)
	}

	// Allocate again, should get different ports
	rtp2, _, err := pm.Allocate()
	if err != nil {
		t.Fatalf("Allocate 2: %v", err)
	}
	if rtp2 == rtp1 {
		t.Error("got same port twice")
	}

	// Release and re-allocate
	pm.Release(rtp1)
	rtp3, _, err := pm.Allocate()
	if err != nil {
		t.Fatalf("Allocate 3: %v", err)
	}
	if rtp3 != rtp1 {
		t.Errorf("expected reuse of released port %d, got %d", rtp1, rtp3)
	}
}

func TestPortManagerExhausted(t *testing.T) {
	pm := NewPortManager(10000, 10004) // only 2 pairs available
	_, _, _ = pm.Allocate()
	_, _, _ = pm.Allocate()
	_, _, err := pm.Allocate()
	if err == nil {
		t.Fatal("expected port exhaustion error")
	}
}
