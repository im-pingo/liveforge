package rtp

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestNewPacketizerH264(t *testing.T) {
	p, err := NewPacketizer(avframe.CodecH264)
	if err != nil {
		t.Fatalf("NewPacketizer: %v", err)
	}
	if p == nil {
		t.Fatal("packetizer is nil")
	}
}

func TestNewDepacketizerH264(t *testing.T) {
	d, err := NewDepacketizer(avframe.CodecH264)
	if err != nil {
		t.Fatalf("NewDepacketizer: %v", err)
	}
	if d == nil {
		t.Fatal("depacketizer is nil")
	}
}

func TestNewPacketizerUnknown(t *testing.T) {
	_, err := NewPacketizer(avframe.CodecType(255))
	if err == nil {
		t.Fatal("expected error for unknown codec")
	}
}
