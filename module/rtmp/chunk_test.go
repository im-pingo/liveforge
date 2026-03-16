package rtmp

import (
	"bytes"
	"testing"
)

func TestChunkWriteReadSmall(t *testing.T) {
	// Small message that fits in one chunk
	msg := &Message{
		TypeID:    MsgAMF0Command,
		Length:    10,
		Timestamp: 100,
		StreamID:  1,
		Payload:   []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	}

	var buf bytes.Buffer
	cw := NewChunkWriter(&buf, DefaultChunkSize)
	if err := cw.WriteMessage(3, msg); err != nil {
		t.Fatalf("WriteMessage error: %v", err)
	}

	cr := NewChunkReader(&buf, DefaultChunkSize)
	got, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage error: %v", err)
	}

	if got.TypeID != msg.TypeID {
		t.Errorf("TypeID: got %d, want %d", got.TypeID, msg.TypeID)
	}
	if got.Timestamp != msg.Timestamp {
		t.Errorf("Timestamp: got %d, want %d", got.Timestamp, msg.Timestamp)
	}
	if !bytes.Equal(got.Payload, msg.Payload) {
		t.Errorf("Payload mismatch")
	}
}

func TestChunkWriteReadLarge(t *testing.T) {
	// Message larger than chunk size — forces multi-chunk splitting
	payload := make([]byte, 300)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	msg := &Message{
		TypeID:    MsgVideo,
		Length:    uint32(len(payload)),
		Timestamp: 1000,
		StreamID:  1,
		Payload:   payload,
	}

	var buf bytes.Buffer
	cw := NewChunkWriter(&buf, 128) // chunk size 128
	if err := cw.WriteMessage(6, msg); err != nil {
		t.Fatalf("WriteMessage error: %v", err)
	}

	cr := NewChunkReader(&buf, 128)
	got, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage error: %v", err)
	}

	if got.TypeID != msg.TypeID {
		t.Errorf("TypeID: got %d, want %d", got.TypeID, msg.TypeID)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("Payload mismatch: got %d bytes, want %d", len(got.Payload), len(payload))
	}
}

func TestChunkSetChunkSize(t *testing.T) {
	// Test with custom chunk size
	payload := make([]byte, 500)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	msg := &Message{
		TypeID:    MsgAudio,
		Length:    uint32(len(payload)),
		Timestamp: 2000,
		StreamID:  1,
		Payload:   payload,
	}

	var buf bytes.Buffer
	cw := NewChunkWriter(&buf, 256) // larger chunks
	if err := cw.WriteMessage(4, msg); err != nil {
		t.Fatalf("WriteMessage error: %v", err)
	}

	cr := NewChunkReader(&buf, 256)
	got, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage error: %v", err)
	}

	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("Payload mismatch")
	}
}
