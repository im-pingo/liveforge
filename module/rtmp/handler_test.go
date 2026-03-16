package rtmp

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func newTestHub() (*core.StreamHub, *core.EventBus) {
	bus := core.NewEventBus()
	cfg := config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        1,
		RingBufferSize:     256,
		NoPublisherTimeout: 5 * time.Second,
	}
	hub := core.NewStreamHub(cfg, bus)
	return hub, bus
}

func TestHandlerConnect(t *testing.T) {
	hub, bus := newTestHub()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	handler := NewHandler(serverConn, hub, bus, 4096)

	errCh := make(chan error, 1)
	go func() {
		errCh <- handler.Handle()
	}()

	// Client sends connect command
	cw := NewChunkWriter(clientConn, DefaultChunkSize)
	cr := NewChunkReader(clientConn, DefaultChunkSize)

	connectPayload, _ := AMF0Encode(
		"connect",
		float64(1),
		map[string]any{"app": "live"},
	)
	err := cw.WriteMessage(3, &Message{
		TypeID:  MsgAMF0Command,
		Length:  uint32(len(connectPayload)),
		Payload: connectPayload,
	})
	if err != nil {
		t.Fatalf("write connect: %v", err)
	}

	// Read responses — should get WindowAckSize, PeerBandwidth, SetChunkSize, _result
	responses := make([]*Message, 0)
	for range 4 {
		msg, err := cr.ReadMessage()
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		responses = append(responses, msg)
		// Update chunk size if SetChunkSize message
		if msg.TypeID == MsgSetChunkSize && len(msg.Payload) >= 4 {
			size := int(msg.Payload[0])<<24 | int(msg.Payload[1])<<16 | int(msg.Payload[2])<<8 | int(msg.Payload[3])
			cr.SetChunkSize(size)
		}
	}

	// Verify _result response
	lastMsg := responses[3]
	if lastMsg.TypeID != MsgAMF0Command {
		t.Fatalf("expected AMF0 command response, got type %d", lastMsg.TypeID)
	}
	vals, err := AMF0Decode(lastMsg.Payload)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if vals[0] != "_result" {
		t.Errorf("expected _result, got %v", vals[0])
	}

	clientConn.Close()
	<-errCh
}

func TestHandlerPublish(t *testing.T) {
	hub, bus := newTestHub()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	handler := NewHandler(serverConn, hub, bus, 4096)

	go handler.Handle()

	cw := NewChunkWriter(clientConn, DefaultChunkSize)
	cr := NewChunkReader(clientConn, DefaultChunkSize)

	// Connect
	connectPayload, _ := AMF0Encode("connect", float64(1), map[string]any{"app": "live"})
	cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(connectPayload)), Payload: connectPayload})

	// Drain connect responses
	for range 4 {
		msg, _ := cr.ReadMessage()
		if msg.TypeID == MsgSetChunkSize && len(msg.Payload) >= 4 {
			size := int(msg.Payload[0])<<24 | int(msg.Payload[1])<<16 | int(msg.Payload[2])<<8 | int(msg.Payload[3])
			cr.SetChunkSize(size)
		}
	}

	// CreateStream
	createPayload, _ := AMF0Encode("createStream", float64(2), nil)
	cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(createPayload)), Payload: createPayload})
	cr.ReadMessage() // _result

	// Publish
	publishPayload, _ := AMF0Encode("publish", float64(0), nil, "teststream", "live")
	cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(publishPayload)), Payload: publishPayload})

	// Read onStatus response
	statusMsg, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("read onStatus: %v", err)
	}
	vals, _ := AMF0Decode(statusMsg.Payload)
	if vals[0] != "onStatus" {
		t.Errorf("expected onStatus, got %v", vals[0])
	}

	// Verify stream was created with a publisher
	stream, ok := hub.Find("live/teststream")
	if !ok {
		t.Fatal("stream not found in hub")
	}
	if stream.State() != core.StreamStatePublishing {
		t.Errorf("expected publishing state, got %v", stream.State())
	}

	clientConn.Close()
}

func TestHandlerMediaMessage(t *testing.T) {
	hub, bus := newTestHub()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	handler := NewHandler(serverConn, hub, bus, 4096)

	go handler.Handle()

	cw := NewChunkWriter(clientConn, DefaultChunkSize)
	cr := NewChunkReader(clientConn, DefaultChunkSize)

	// Connect + CreateStream + Publish
	connectPayload, _ := AMF0Encode("connect", float64(1), map[string]any{"app": "live"})
	cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(connectPayload)), Payload: connectPayload})
	for range 4 {
		msg, _ := cr.ReadMessage()
		if msg.TypeID == MsgSetChunkSize && len(msg.Payload) >= 4 {
			size := int(msg.Payload[0])<<24 | int(msg.Payload[1])<<16 | int(msg.Payload[2])<<8 | int(msg.Payload[3])
			cr.SetChunkSize(size)
		}
	}

	createPayload, _ := AMF0Encode("createStream", float64(2), nil)
	cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(createPayload)), Payload: createPayload})
	cr.ReadMessage()

	publishPayload, _ := AMF0Encode("publish", float64(0), nil, "test", "live")
	cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(publishPayload)), Payload: publishPayload})
	cr.ReadMessage() // onStatus

	// Send video keyframe
	videoData := []byte{
		0x17,       // keyframe + H.264
		0x01,       // AVC NALU
		0x00, 0x00, 0x00, // CTS = 0
		0x65, 0x88, 0x00, 0x01, // NALU data
	}
	cw.WriteMessage(6, &Message{
		TypeID:    MsgVideo,
		Length:    uint32(len(videoData)),
		Timestamp: 0,
		StreamID:  1,
		Payload:   videoData,
	})

	// Give handler time to process
	time.Sleep(50 * time.Millisecond)

	// Verify frame was written to stream
	stream, ok := hub.Find("live/test")
	if !ok {
		t.Fatal("stream not found")
	}
	gop := stream.GOPCache()
	if len(gop) == 0 {
		t.Error("expected GOP cache to have frames")
	}

	clientConn.Close()
}

// drainReader is a helper to read and discard from a connection
func drainReader(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		_, err := r.Read(buf)
		if err != nil {
			return
		}
	}
}

func init() {
	_ = bytes.Compare // ensure bytes import is used
}
