package rtmp

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

func TestSplitNameParams(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantName   string
		wantParams map[string]string
	}{
		{"no params", "teststream", "teststream", nil},
		{"with params", "teststream?token=abc&key=val", "teststream", map[string]string{"token": "abc", "key": "val"}},
		{"empty query", "teststream?", "teststream", map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, params := splitNameParams(tt.input)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if tt.wantParams == nil {
				if params != nil {
					t.Errorf("params = %v, want nil", params)
				}
			} else {
				for k, v := range tt.wantParams {
					if params[k] != v {
						t.Errorf("params[%q] = %q, want %q", k, params[k], v)
					}
				}
			}
		})
	}
}

func TestParseQueryParams(t *testing.T) {
	tests := []struct {
		query string
		want  map[string]string
	}{
		{"key=val", map[string]string{"key": "val"}},
		{"a=1&b=2&c=3", map[string]string{"a": "1", "b": "2", "c": "3"}},
		{"flag", map[string]string{"flag": ""}},
		{"", map[string]string{}},
		{"a=1&&b=2", map[string]string{"a": "1", "b": "2"}},
	}
	for _, tt := range tests {
		params := parseQueryParams(tt.query)
		for k, v := range tt.want {
			if params[k] != v {
				t.Errorf("parseQueryParams(%q)[%q] = %q, want %q", tt.query, k, params[k], v)
			}
		}
	}
}

func TestMergeParams(t *testing.T) {
	// Both nil
	if result := mergeParams(nil, nil); result != nil {
		t.Errorf("both nil: got %v", result)
	}

	// Override wins
	base := map[string]string{"a": "1", "b": "2"}
	override := map[string]string{"b": "3", "c": "4"}
	result := mergeParams(base, override)
	if result["a"] != "1" || result["b"] != "3" || result["c"] != "4" {
		t.Errorf("merge result = %v", result)
	}

	// Base only
	result = mergeParams(base, nil)
	if result["a"] != "1" || result["b"] != "2" {
		t.Errorf("base only result = %v", result)
	}
}

func TestIsConnClosed(t *testing.T) {
	if isConnClosed(nil) {
		t.Error("nil error should return false")
	}

	if isConnClosed(errors.New("some error")) {
		t.Error("random error should return false")
	}

	closedErr := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: errors.New("use of closed network connection"),
	}
	if !isConnClosed(closedErr) {
		t.Error("closed connection error should return true")
	}
}

func TestParseVideoPayload(t *testing.T) {
	// Valid H.264 keyframe
	data := []byte{
		0x17,                   // keyframe (1) + H.264 (7)
		0x01,                   // AVC NALU
		0x00, 0x00, 0x00,      // CTS = 0
		0x65, 0x88, 0x00, 0x01, // NALU data
	}
	frame := parseVideoPayload(data, 1000)
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Codec != avframe.CodecH264 {
		t.Errorf("Codec = %v, want H264", frame.Codec)
	}
	if frame.FrameType != avframe.FrameTypeKeyframe {
		t.Errorf("FrameType = %v, want Keyframe", frame.FrameType)
	}
	if frame.DTS != 1000 {
		t.Errorf("DTS = %d, want 1000", frame.DTS)
	}

	// Sequence header
	seqData := []byte{
		0x17, // keyframe + H.264
		flvpkg.AVCPacketSequenceHeader,
		0x00, 0x00, 0x00,
		0x01, 0x64, 0x00, 0x28, // SPS data
	}
	frame = parseVideoPayload(seqData, 0)
	if frame == nil {
		t.Fatal("expected non-nil frame for seq header")
	}
	if frame.FrameType != avframe.FrameTypeSequenceHeader {
		t.Errorf("FrameType = %v, want SequenceHeader", frame.FrameType)
	}

	// Interframe
	interData := []byte{
		0x27,                   // interframe (2) + H.264 (7)
		0x01,                   // AVC NALU
		0x00, 0x00, 0x00,      // CTS = 0
		0x41, 0x9A, 0x00, 0x01, // NALU data
	}
	frame = parseVideoPayload(interData, 2000)
	if frame == nil {
		t.Fatal("expected non-nil frame for interframe")
	}
	if frame.FrameType != avframe.FrameTypeInterframe {
		t.Errorf("FrameType = %v, want Interframe", frame.FrameType)
	}

	// Too short
	frame = parseVideoPayload([]byte{0x17, 0x01}, 0)
	if frame != nil {
		t.Error("expected nil for short payload")
	}
}

func TestParseVideoPayloadBFrameCTS(t *testing.T) {
	tests := []struct {
		name    string
		cts     int32 // composition time offset in ms
		dts     int64
		wantPTS int64
	}{
		{"CTS=66ms", 66, 0, 66},
		{"CTS=100ms", 100, 1000, 1100},
		{"CTS=33ms", 33, 2000, 2033},
		{"CTS=0", 0, 500, 500},
		{"CTS=large", 300, 0, 300},
		{"CTS=negative", -33, 100, 67},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode CTS as signed 24-bit big-endian
			ctsBytes := [3]byte{
				byte(tt.cts >> 16),
				byte(tt.cts >> 8),
				byte(tt.cts),
			}
			data := []byte{
				0x27,            // interframe (2) + H.264 (7)
				0x01,            // AVC NALU
				ctsBytes[0], ctsBytes[1], ctsBytes[2],
				0x41, 0x9A, 0x00, 0x01, // NALU data
			}

			frame := parseVideoPayload(data, tt.dts)
			if frame == nil {
				t.Fatal("expected non-nil frame")
			}
			if frame.DTS != tt.dts {
				t.Errorf("DTS = %d, want %d", frame.DTS, tt.dts)
			}
			if frame.PTS != tt.wantPTS {
				t.Errorf("PTS = %d, want %d", frame.PTS, tt.wantPTS)
			}
		})
	}
}

func TestParseAudioPayload(t *testing.T) {
	// AAC sequence header
	seqData := []byte{
		0xAF, // AAC (10) + config
		flvpkg.AACPacketSequenceHeader,
		0x12, 0x10, // AAC-LC, 44100 Hz, stereo
	}
	frame := parseAudioPayload(seqData, 0)
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.FrameType != avframe.FrameTypeSequenceHeader {
		t.Errorf("FrameType = %v, want SequenceHeader", frame.FrameType)
	}
	if frame.Codec != avframe.CodecAAC {
		t.Errorf("Codec = %v, want AAC", frame.Codec)
	}

	// AAC raw data
	rawData := []byte{
		0xAF, // AAC
		0x01, // Raw
		0xDE, 0xAD,
	}
	frame = parseAudioPayload(rawData, 1000)
	if frame == nil {
		t.Fatal("expected non-nil frame for raw audio")
	}
	if frame.FrameType != avframe.FrameTypeInterframe {
		t.Errorf("FrameType = %v, want Interframe", frame.FrameType)
	}

	// Too short
	frame = parseAudioPayload([]byte{0xAF}, 0)
	if frame != nil {
		t.Error("expected nil for short payload")
	}
}

func TestPublisherIDAndMediaInfo(t *testing.T) {
	conn, _ := net.Pipe()
	defer conn.Close()

	pub := NewPublisher("live/test", conn)
	if pub.ID() != "rtmp-pub-live/test" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo() == nil {
		t.Error("expected non-nil MediaInfo")
	}

	// SetMediaInfo
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	pub.SetMediaInfo(info)
	if pub.MediaInfo().VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v", pub.MediaInfo().VideoCodec)
	}
}

func TestPublisherClose(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	pub := NewPublisher("live/test", serverConn)
	if err := pub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSubscriberIDAndOptions(t *testing.T) {
	conn, _ := net.Pipe()
	defer conn.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")
	cw := NewChunkWriter(conn, DefaultChunkSize)
	sub := NewSubscriber("live/test", conn, cw, stream, nil)

	if sub.ID() != "rtmp-sub-live/test" {
		t.Errorf("ID = %q", sub.ID())
	}
	if sub.Options().StartMode != core.StartModeGOP {
		t.Errorf("StartMode = %v", sub.Options().StartMode)
	}
}

func TestSubscriberClose(t *testing.T) {
	conn, _ := net.Pipe()
	defer conn.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")
	cw := NewChunkWriter(conn, DefaultChunkSize)
	sub := NewSubscriber("live/test", conn, cw, stream, nil)

	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should not panic
	if err := sub.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestModuleNameAndHooks(t *testing.T) {
	m := NewModule()
	if m.Name() != "rtmp" {
		t.Errorf("Name = %q", m.Name())
	}
	if hooks := m.Hooks(); hooks != nil {
		t.Errorf("Hooks = %v, want nil", hooks)
	}
}

func TestModuleInitAndClose(t *testing.T) {
	cfg := &config.Config{}
	cfg.RTMP.Enabled = true
	cfg.RTMP.Listen = "127.0.0.1:0"
	cfg.RTMP.ChunkSize = 4096
	cfg.Stream.RingBufferSize = 256

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)
	if err := srv.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer srv.Shutdown()

	// Module should have a listener
	if m.Hub() == nil {
		t.Error("Hub should not be nil")
	}

	// Try connecting (handshake will fail since we won't do full RTMP, that's OK)
	conn, err := net.DialTimeout("tcp", m.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
}

func TestHandlerConnectWithAppParams(t *testing.T) {
	hub, bus := newTestHub()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	handler := NewHandler(serverConn, hub, bus, 4096, nil)

	errCh := make(chan error, 1)
	go func() {
		errCh <- handler.Handle()
	}()

	cw := NewChunkWriter(clientConn, DefaultChunkSize)
	cr := NewChunkReader(clientConn, DefaultChunkSize)

	// Connect with app query params
	connectPayload, _ := AMF0Encode(
		"connect",
		float64(1),
		map[string]any{"app": "live?token=abc123"},
	)
	cw.WriteMessage(3, &Message{
		TypeID:  MsgAMF0Command,
		Length:  uint32(len(connectPayload)),
		Payload: connectPayload,
	})

	// Drain connect responses
	for range 4 {
		msg, _ := cr.ReadMessage()
		if msg.TypeID == MsgSetChunkSize && len(msg.Payload) >= 4 {
			size := int(msg.Payload[0])<<24 | int(msg.Payload[1])<<16 | int(msg.Payload[2])<<8 | int(msg.Payload[3])
			cr.SetChunkSize(size)
		}
	}

	// Verify app was parsed correctly
	if handler.app != "live" {
		t.Errorf("app = %q, want %q", handler.app, "live")
	}
	if handler.appParams["token"] != "abc123" {
		t.Errorf("appParams[token] = %q, want %q", handler.appParams["token"], "abc123")
	}

	clientConn.Close()
	<-errCh
}
