package rtmp

import (
	"bytes"
	"encoding/binary"
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

func TestChunkReadFmt3NewMessageReusesTimestampDelta(t *testing.T) {
	wire := []byte{
		byte(chunkFmt0<<6 | 4),
		0, 0, 100,
		0, 0, 1,
		MsgAudio,
		1, 0, 0, 0,
		0xA0,
		byte(chunkFmt2<<6 | 4),
		0, 0, 23,
		0xB0,
		byte(chunkFmt3<<6 | 4),
		0xC0,
	}

	reader := NewChunkReader(bytes.NewReader(wire), DefaultChunkSize)
	for i, want := range []struct {
		timestamp uint32
		payload   byte
	}{
		{timestamp: 100, payload: 0xA0},
		{timestamp: 123, payload: 0xB0},
		{timestamp: 146, payload: 0xC0},
	} {
		got, err := reader.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got.Timestamp != want.timestamp {
			t.Errorf("message %d timestamp = %d, want %d", i, got.Timestamp, want.timestamp)
		}
		if !bytes.Equal(got.Payload, []byte{want.payload}) {
			t.Errorf("message %d payload = %x, want %x", i, got.Payload, want.payload)
		}
	}
}

func TestChunkReadFmt3AfterFmt0UsesZeroTimestampDelta(t *testing.T) {
	wire := []byte{
		byte(chunkFmt0<<6 | 4),
		0, 0, 100,
		0, 0, 1,
		MsgAudio,
		1, 0, 0, 0,
		0xA0,
		byte(chunkFmt3<<6 | 4),
		0xB0,
	}

	reader := NewChunkReader(bytes.NewReader(wire), DefaultChunkSize)
	for i, want := range []struct {
		timestamp uint32
		payload   byte
	}{
		{timestamp: 100, payload: 0xA0},
		{timestamp: 100, payload: 0xB0},
	} {
		got, err := reader.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got.Timestamp != want.timestamp {
			t.Errorf("message %d timestamp = %d, want %d", i, got.Timestamp, want.timestamp)
		}
		if !bytes.Equal(got.Payload, []byte{want.payload}) {
			t.Errorf("message %d payload = %x, want %x", i, got.Payload, want.payload)
		}
	}
}

func TestChunkReadFmt3ContinuationDoesNotAdvanceTimestamp(t *testing.T) {
	wire := []byte{
		byte(chunkFmt0<<6 | 4),
		0, 0, 100,
		0, 0, 4,
		MsgAudio,
		1, 0, 0, 0,
		0x10, 0x11,
		byte(chunkFmt3<<6 | 4),
		0x12, 0x13,
		byte(chunkFmt2<<6 | 4),
		0, 0, 23,
		0x20, 0x21,
		byte(chunkFmt3<<6 | 4),
		0x22, 0x23,
		byte(chunkFmt3<<6 | 4),
		0x30, 0x31,
		byte(chunkFmt3<<6 | 4),
		0x32, 0x33,
	}

	reader := NewChunkReader(bytes.NewReader(wire), 2)
	for i, want := range []struct {
		timestamp uint32
		payload   []byte
	}{
		{timestamp: 100, payload: []byte{0x10, 0x11, 0x12, 0x13}},
		{timestamp: 123, payload: []byte{0x20, 0x21, 0x22, 0x23}},
		{timestamp: 146, payload: []byte{0x30, 0x31, 0x32, 0x33}},
	} {
		got, err := reader.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got.Timestamp != want.timestamp {
			t.Errorf("message %d timestamp = %d, want %d", i, got.Timestamp, want.timestamp)
		}
		if !bytes.Equal(got.Payload, want.payload) {
			t.Errorf("message %d payload = %x, want %x", i, got.Payload, want.payload)
		}
	}
}

func TestChunkReadFmt3ContinuationConsumesExtendedTimestamp(t *testing.T) {
	const timestamp = uint32(0x01000000)

	wire := []byte{
		byte(chunkFmt0<<6 | 6),
		0xFF, 0xFF, 0xFF,
		0, 0, 4,
		MsgVideo,
		1, 0, 0, 0,
	}
	wire = binary.BigEndian.AppendUint32(wire, timestamp)
	wire = append(wire, 0x01, 0x02, byte(chunkFmt3<<6|6))
	wire = binary.BigEndian.AppendUint32(wire, timestamp)
	wire = append(wire, 0x03, 0x04)

	got, err := NewChunkReader(bytes.NewReader(wire), 2).ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got.Timestamp != timestamp {
		t.Errorf("timestamp = %d, want %d", got.Timestamp, timestamp)
	}
	if !bytes.Equal(got.Payload, []byte{0x01, 0x02, 0x03, 0x04}) {
		t.Errorf("payload = %x, want 01020304", got.Payload)
	}
}

func TestChunkReadFmt3AfterFmt0ExtendedTimestampKeepsAbsoluteTime(t *testing.T) {
	const timestamp = uint32(0x01000000)

	wire := []byte{
		byte(chunkFmt0<<6 | 6),
		0xFF, 0xFF, 0xFF,
		0, 0, 1,
		MsgVideo,
		1, 0, 0, 0,
	}
	wire = binary.BigEndian.AppendUint32(wire, timestamp)
	wire = append(wire, 0x01, byte(chunkFmt3<<6|6))
	wire = binary.BigEndian.AppendUint32(wire, timestamp)
	wire = append(wire, 0x02)

	reader := NewChunkReader(bytes.NewReader(wire), DefaultChunkSize)
	for i, want := range []struct {
		timestamp uint32
		payload   byte
	}{
		{timestamp: timestamp, payload: 0x01},
		{timestamp: timestamp, payload: 0x02},
	} {
		got, err := reader.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got.Timestamp != want.timestamp {
			t.Errorf("message %d timestamp = %d, want %d", i, got.Timestamp, want.timestamp)
		}
		if !bytes.Equal(got.Payload, []byte{want.payload}) {
			t.Errorf("message %d payload = %x, want %x", i, got.Payload, want.payload)
		}
	}
}

func TestChunkReadCompressedHeaderRequiresPreviousChunk(t *testing.T) {
	reader := NewChunkReader(bytes.NewReader([]byte{byte(chunkFmt3<<6 | 4)}), DefaultChunkSize)
	if _, err := reader.ReadMessage(); err == nil {
		t.Fatal("expected protocol error for fmt 3 without previous chunk")
	}
}

func TestChunkReadFmt3NewMessageUsesExtendedTimestampDelta(t *testing.T) {
	const delta = uint32(0x01000000)

	wire := []byte{
		byte(chunkFmt0<<6 | 4),
		0, 0, 100,
		0, 0, 1,
		MsgAudio,
		1, 0, 0, 0,
		0xA0,
		byte(chunkFmt2<<6 | 4),
		0xFF, 0xFF, 0xFF,
	}
	wire = binary.BigEndian.AppendUint32(wire, delta)
	wire = append(wire, 0xB0, byte(chunkFmt3<<6|4))
	wire = binary.BigEndian.AppendUint32(wire, delta)
	wire = append(wire, 0xC0)

	reader := NewChunkReader(bytes.NewReader(wire), DefaultChunkSize)
	for i, want := range []uint32{100, 100 + delta, 100 + 2*delta} {
		got, err := reader.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got.Timestamp != want {
			t.Errorf("message %d timestamp = %d, want %d", i, got.Timestamp, want)
		}
	}
}

func TestChunkWriteExtendedTimestampOnContinuations(t *testing.T) {
	const timestamp = uint32(0x01000000)

	msg := &Message{
		TypeID:    MsgVideo,
		Length:    5,
		Timestamp: timestamp,
		StreamID:  1,
		Payload:   []byte{0x01, 0x02, 0x03, 0x04, 0x05},
	}

	var buf bytes.Buffer
	if err := NewChunkWriter(&buf, 2).WriteMessage(6, msg); err != nil {
		t.Fatal(err)
	}

	wire := buf.Bytes()
	if len(wire) != 31 {
		t.Fatalf("wire length = %d, want 31", len(wire))
	}
	for i, offset := range []int{12, 19, 26} {
		if got := binary.BigEndian.Uint32(wire[offset : offset+4]); got != timestamp {
			t.Errorf("extended timestamp %d = %d, want %d", i, got, timestamp)
		}
	}
	if wire[18] != byte(chunkFmt3<<6|6) || wire[25] != byte(chunkFmt3<<6|6) {
		t.Errorf("continuation headers missing: %x", wire)
	}
}
