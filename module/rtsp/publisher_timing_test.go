package rtsp

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRTSPPublisherIndependentAudioVideoClocks(t *testing.T) {
	stream := core.NewStream("live/track-clocks", config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	pub, err := NewRTSPPublisher("track-clocks", &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecOpus}, stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	reader := stream.RingBuffer().NewReader()
	for _, tc := range []struct {
		pt   uint8
		ts   uint32
		want int64
	}{
		{96, 900000, 0}, {111, 5000, 0}, {96, 903000, 33}, {111, 5960, 20},
	} {
		pkt := rtspVideoPacket(1, tc.ts, true, 0x65, 1)
		pkt.PayloadType = tc.pt
		if tc.want != 0 {
			pkt.SequenceNumber = 2
		}
		if err := pub.FeedRTP(pkt); err != nil {
			t.Fatal(err)
		}
		frame, ok := reader.TryRead()
		if !ok || frame.DTS != tc.want || frame.PTS != tc.want {
			t.Fatalf("PT %d clock: frame=%v want DTS/PTS=%d", tc.pt, frame, tc.want)
		}
	}
	// Audio Marker indicates a talkspurt, not the end of each audio sample.
	pkt := rtspVideoPacket(3, 6920, false, 0xf8, 0xff, 0xfe)
	pkt.PayloadType = 111
	if err := pub.FeedRTP(pkt); err != nil {
		t.Fatal(err)
	}
	if frame, ok := reader.TryRead(); !ok || frame.MediaType != avframe.MediaTypeAudio || frame.DTS != 40 {
		t.Fatalf("unmarked audio sample was not delivered at 40ms: %v", frame)
	}
}
