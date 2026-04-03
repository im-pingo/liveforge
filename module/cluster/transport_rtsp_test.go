package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

func TestRTSPTransportScheme(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	tr := NewRTSPTransport(cfg)
	if tr.Scheme() != "rtsp" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "rtsp")
	}
}

func TestRTSPTransportPushBadURL(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	tr := NewRTSPTransport(cfg)
	defer tr.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	ctx := context.Background()
	err := tr.Push(ctx, "rtsp://127.0.0.1:19999/live/test", stream)
	if err == nil {
		t.Error("expected error for connection to non-existent server")
	}
}

func TestRTSPTransportPullBadURL(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	tr := NewRTSPTransport(cfg)
	defer tr.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	ctx := context.Background()
	err := tr.Pull(ctx, "rtsp://127.0.0.1:19999/live/test", stream)
	if err == nil {
		t.Error("expected error for connection to non-existent server")
	}
}

func TestRTSPTransportInterfaceCompliance(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	var _ RelayTransport = NewRTSPTransport(cfg)
}

func TestWriteReadInterleaved(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{0x80, 0x60, 0x00, 0x01, 0x00, 0x00, 0x00, 0xA0}

	if err := writeInterleaved(&buf, 0, payload); err != nil {
		t.Fatalf("writeInterleaved: %v", err)
	}

	// Verify framing: '$' + channel + 2-byte length + payload
	data := buf.Bytes()
	if data[0] != '$' {
		t.Errorf("marker = 0x%02x, want '$'", data[0])
	}
	if data[1] != 0 {
		t.Errorf("channel = %d, want 0", data[1])
	}
	length := binary.BigEndian.Uint16(data[2:4])
	if int(length) != len(payload) {
		t.Errorf("length = %d, want %d", length, len(payload))
	}

	// Read back
	br := bufio.NewReader(&buf)
	// Reset buffer with same data
	buf.Reset()
	buf.Write(data)
	br = bufio.NewReader(&buf)

	ch, readData, err := readInterleaved(br)
	if err != nil {
		t.Fatalf("readInterleaved: %v", err)
	}
	if ch != 0 {
		t.Errorf("channel = %d, want 0", ch)
	}
	if !bytes.Equal(readData, payload) {
		t.Errorf("data mismatch: got %x, want %x", readData, payload)
	}
}

func TestReadInterleavedBadMarker(t *testing.T) {
	buf := bytes.NewBuffer([]byte{0x00, 0x00, 0x00, 0x04, 0x01, 0x02, 0x03, 0x04})
	br := bufio.NewReader(buf)
	_, _, err := readInterleaved(br)
	if err == nil {
		t.Error("expected error for bad marker byte")
	}
}

func TestEncodingNameToCodec(t *testing.T) {
	tests := []struct {
		name  string
		codec avframe.CodecType
	}{
		{"H264", avframe.CodecH264},
		{"h264", avframe.CodecH264},
		{"H265", avframe.CodecH265},
		{"OPUS", avframe.CodecOpus},
		{"PCMU", avframe.CodecG711U},
		{"PCMA", avframe.CodecG711A},
		{"MPEG4-GENERIC", avframe.CodecAAC},
		{"unknown", 0},
	}
	for _, tt := range tests {
		got := encodingNameToCodec(tt.name)
		if got != tt.codec {
			t.Errorf("encodingNameToCodec(%q) = %v, want %v", tt.name, got, tt.codec)
		}
	}
}

func TestSdpToMediaInfo(t *testing.T) {
	mi := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
		SampleRate: 44100,
	}
	sd := sdp.BuildFromMediaInfo(mi, "rtsp://localhost/live/test", "0.0.0.0")

	parsed := sdpToMediaInfo(sd)
	if parsed.VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v, want H264", parsed.VideoCodec)
	}
	if parsed.AudioCodec != avframe.CodecAAC {
		t.Errorf("AudioCodec = %v, want AAC", parsed.AudioCodec)
	}
}

func TestBuildPTMap(t *testing.T) {
	mi := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecOpus,
		SampleRate: 48000,
	}
	sd := sdp.BuildFromMediaInfo(mi, "rtsp://localhost/live/test", "0.0.0.0")

	ptMap := buildPTMap(sd)
	if len(ptMap) == 0 {
		t.Fatal("ptMap is empty")
	}

	// Verify that at least one entry maps to H264 and one to Opus
	var hasH264, hasOpus bool
	for _, codec := range ptMap {
		if codec == avframe.CodecH264 {
			hasH264 = true
		}
		if codec == avframe.CodecOpus {
			hasOpus = true
		}
	}
	if !hasH264 {
		t.Error("ptMap missing H264")
	}
	if !hasOpus {
		t.Error("ptMap missing Opus")
	}
}

func TestDefaultClusterRTSPConfig(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	if cfg.Transport != "tcp" {
		t.Errorf("Transport = %q, want %q", cfg.Transport, "tcp")
	}
}

func TestRTSPTransportClose(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	tr := NewRTSPTransport(cfg)
	if err := tr.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}
