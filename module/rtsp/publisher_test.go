package rtsp

import (
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
)

func TestRTSPPublisherInterface(t *testing.T) {
	// Verify compile-time interface compliance
	var _ core.Publisher = (*RTSPPublisher)(nil)
}

func TestRTSPPublisherIDAndMediaInfo(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	// Pass nil stream — just testing metadata methods
	pub := &RTSPPublisher{
		id:            "test-pub",
		mediaInfo:     info,
		depacketizers: make(map[uint8]pkgrtp.Depacketizer),
	}
	if pub.ID() != "test-pub" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo().VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v", pub.MediaInfo().VideoCodec)
	}
}

func TestRTSPPublisherClose(t *testing.T) {
	pub := &RTSPPublisher{
		depacketizers: make(map[uint8]pkgrtp.Depacketizer),
	}
	if err := pub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After close, FeedRTP should silently return
	pkt := &pionrtp.Packet{Header: pionrtp.Header{PayloadType: 96}}
	if err := pub.FeedRTP(pkt); err != nil {
		t.Fatalf("FeedRTP after close: %v", err)
	}
}

func TestRTSPPublisherUnknownPT(t *testing.T) {
	pub := &RTSPPublisher{
		depacketizers: make(map[uint8]pkgrtp.Depacketizer),
	}
	// Unknown payload type should be silently skipped
	pkt := &pionrtp.Packet{Header: pionrtp.Header{PayloadType: 200}, Payload: []byte{0x01}}
	if err := pub.FeedRTP(pkt); err != nil {
		t.Fatalf("FeedRTP unknown PT: %v", err)
	}
}
