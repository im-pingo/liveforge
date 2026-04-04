package gb28181

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
)

// buildTestPSData creates valid PS-packed data from a H.264 keyframe with Annex-B NALUs.
func buildTestPSData(t *testing.T) []byte {
	t.Helper()
	muxer := ps.NewMuxer()

	// SPS + PPS + IDR in Annex-B format
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	sps := []byte{0x67, 0x42, 0x00, 0x1E, 0xAB, 0x40, 0x50}
	pps := []byte{0x68, 0xCE, 0x38, 0x80}
	idr := make([]byte, 100)
	idr[0] = 0x65
	for i := 1; i < len(idr); i++ {
		idr[i] = byte(i)
	}

	var payload []byte
	payload = append(payload, startCode...)
	payload = append(payload, sps...)
	payload = append(payload, startCode...)
	payload = append(payload, pps...)
	payload = append(payload, startCode...)
	payload = append(payload, idr...)

	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, payload)
	psData, err := muxer.Pack(frame)
	if err != nil {
		t.Fatalf("muxer.Pack: %v", err)
	}
	return psData
}

func TestPublisherFeedRTPSinglePacketWithMarker(t *testing.T) {
	psData := buildTestPSData(t)

	var received []*avframe.AVFrame
	pub := NewPublisher("test-pub", func(f *avframe.AVFrame) {
		received = append(received, f)
	})

	// Single RTP packet with marker = complete PS pack
	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 1,
			Timestamp:      90000,
			SSRC:           12345,
			Marker:         true,
		},
		Payload: psData,
	}

	pub.FeedRTP(pkt)

	if len(received) == 0 {
		t.Fatal("no frames emitted")
	}
	if received[0].MediaType != avframe.MediaTypeVideo {
		t.Errorf("MediaType = %v, want video", received[0].MediaType)
	}
}

func TestPublisherFeedRTPFragmented(t *testing.T) {
	psData := buildTestPSData(t)

	var received []*avframe.AVFrame
	pub := NewPublisher("test-frag", func(f *avframe.AVFrame) {
		received = append(received, f)
	})

	// Fragment into two RTP packets
	mid := len(psData) / 2

	pkt1 := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 1,
			Timestamp:      90000,
			SSRC:           12345,
			Marker:         false, // not last
		},
		Payload: psData[:mid],
	}

	pkt2 := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 2,
			Timestamp:      90000,
			SSRC:           12345,
			Marker:         true, // last fragment
		},
		Payload: psData[mid:],
	}

	pub.FeedRTP(pkt1)
	if len(received) != 0 {
		t.Error("should not emit frames before marker")
	}

	pub.FeedRTP(pkt2)
	if len(received) == 0 {
		t.Fatal("no frames emitted after marker")
	}
}

func TestPublisherFeedRTPAfterClose(t *testing.T) {
	var received []*avframe.AVFrame
	pub := NewPublisher("test-close", func(f *avframe.AVFrame) {
		received = append(received, f)
	})

	pub.Close()

	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version: 2,
			Marker:  true,
		},
		Payload: []byte{0x00, 0x00, 0x01, 0xBA, 0x00},
	}

	pub.FeedRTP(pkt)

	if len(received) != 0 {
		t.Error("should not emit frames after close")
	}
}

func TestPublisherBufferReset(t *testing.T) {
	// Verify buffer is reset after each PS pack processing
	psData := buildTestPSData(t)

	count := 0
	pub := NewPublisher("test-reset", func(f *avframe.AVFrame) {
		count++
	})

	// Send two complete packs in sequence
	for i := 0; i < 2; i++ {
		pkt := &pionrtp.Packet{
			Header: pionrtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: uint16(i),
				Timestamp:      uint32(90000 * (i + 1)),
				SSRC:           12345,
				Marker:         true,
			},
			Payload: psData,
		}
		pub.FeedRTP(pkt)
	}

	if count < 2 {
		t.Errorf("expected at least 2 frame emissions, got %d", count)
	}
}

func TestPublisherID(t *testing.T) {
	pub := NewPublisher("my-id", nil)
	if pub.ID() != "my-id" {
		t.Errorf("ID() = %q, want my-id", pub.ID())
	}
}
