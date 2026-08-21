package rtsp

import (
	"bytes"
	"math"
	"sync"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
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
		id:        "test-pub",
		mediaInfo: info,
	}
	if pub.ID() != "test-pub" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo().VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v", pub.MediaInfo().VideoCodec)
	}
}

func TestRTSPPublisherClose(t *testing.T) {
	pub := &RTSPPublisher{}
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
	pub := &RTSPPublisher{}
	// Unknown payload type should be silently skipped
	pkt := &pionrtp.Packet{Header: pionrtp.Header{PayloadType: 200}, Payload: []byte{0x01}}
	if err := pub.FeedRTP(pkt); err != nil {
		t.Fatalf("FeedRTP unknown PT: %v", err)
	}
}

func TestRTSPPublisherInterleavedTracksKeepIndependentPayloadsAndClocks(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecOpus,
		SampleRate: 48000,
	}
	pub, stream := newPublisherTestHarness(t, info, RTPTrackMap{
		96:  {Codec: avframe.CodecH264, ClockRate: 90000},
		111: {Codec: avframe.CodecOpus, ClockRate: 48000},
	})

	packets := []*pionrtp.Packet{
		newRTPPacket(96, 1001, 1, 900000, false, []byte{0x65, 0x01, 0x11}),
		newRTPPacket(111, 2002, 1, 480000, false, []byte{0xf8, 0x10}),
		newRTPPacket(96, 1001, 2, 900000, true, []byte{0x41, 0x02, 0x12}),
		newRTPPacket(96, 1001, 3, 909000, false, []byte{0x41, 0x03, 0x13}),
		newRTPPacket(111, 2002, 2, 484800, false, []byte{0xf8, 0x20}),
		newRTPPacket(96, 1001, 4, 909000, true, []byte{0x41, 0x04, 0x14}),
	}
	for _, pkt := range packets {
		if err := pub.FeedRTP(pkt); err != nil {
			t.Fatalf("FeedRTP: %v", err)
		}
	}

	frames := readPublisherFrames(t, stream, 4)
	want := []struct {
		media avframe.MediaType
		codec avframe.CodecType
		dts   int64
		data  []byte
	}{
		{avframe.MediaTypeAudio, avframe.CodecOpus, 0, []byte{0xf8, 0x10}},
		{avframe.MediaTypeVideo, avframe.CodecH264, 0, avccPayload([]byte{0x65, 0x01, 0x11}, []byte{0x41, 0x02, 0x12})},
		{avframe.MediaTypeAudio, avframe.CodecOpus, 100, []byte{0xf8, 0x20}},
		{avframe.MediaTypeVideo, avframe.CodecH264, 100, avccPayload([]byte{0x41, 0x03, 0x13}, []byte{0x41, 0x04, 0x14})},
	}
	for i, frame := range frames {
		if frame.MediaType != want[i].media || frame.Codec != want[i].codec {
			t.Errorf("frame %d type = (%v, %v), want (%v, %v)", i, frame.MediaType, frame.Codec, want[i].media, want[i].codec)
		}
		if frame.DTS != want[i].dts || frame.PTS != want[i].dts {
			t.Errorf("frame %d timestamps = (%d, %d), want (%d, %d)", i, frame.DTS, frame.PTS, want[i].dts, want[i].dts)
		}
		if !bytes.Equal(frame.Payload, want[i].data) {
			t.Errorf("frame %d payload = %x, want %x", i, frame.Payload, want[i].data)
		}
	}
}

func TestRTSPPublisherPreservesDuplicatePayloadTypeTrackIdentity(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecOpus}
	stream := core.NewStream("live/duplicate-pt", config.StreamConfig{RingBufferSize: 64}, config.LimitsConfig{}, core.NewEventBus())
	pub, err := NewRTSPPublisherWithDescriptions("duplicate-pt", info, stream, []RTPTrackDescription{
		{TrackID: 0, Control: "trackID=0", PayloadType: 96, Info: RTPTrackInfo{Codec: avframe.CodecH264, ClockRate: 90000}},
		{TrackID: 1, Control: "trackID=1", PayloadType: 96, Info: RTPTrackInfo{Codec: avframe.CodecOpus, ClockRate: 48000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pub.trackMap[0].Codec; got != avframe.CodecH264 {
		t.Fatalf("track 0 codec = %v, want H264", got)
	}
	if got := pub.trackMap[1].Codec; got != avframe.CodecOpus {
		t.Fatalf("track 1 codec = %v, want Opus", got)
	}
}

func TestRTSPPublisherSamePTDifferentSSRCsKeepIndependentDepacketizers(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	pub, stream := newPublisherTestHarness(t, info, RTPTrackMap{
		96: {Codec: avframe.CodecH264, ClockRate: 90000},
	})

	packets := []*pionrtp.Packet{
		newRTPPacket(96, 1001, 1, 90000, false, []byte{0x7c, 0x85, 0xa1}),
		newRTPPacket(96, 2002, 1, 180000, false, []byte{0x7c, 0x85, 0xc1}),
		newRTPPacket(96, 1001, 2, 90000, true, []byte{0x7c, 0x45, 0xb1}),
		newRTPPacket(96, 2002, 2, 180000, true, []byte{0x7c, 0x45, 0xd1}),
	}
	for _, pkt := range packets {
		if err := pub.FeedRTP(pkt); err != nil {
			t.Fatalf("FeedRTP: %v", err)
		}
	}

	frames := readPublisherFrames(t, stream, 2)
	wantPayloads := [][]byte{
		avccPayload([]byte{0x65, 0xa1, 0xb1}),
		avccPayload([]byte{0x65, 0xc1, 0xd1}),
	}
	for i, frame := range frames {
		if frame.Codec != avframe.CodecH264 || !bytes.Equal(frame.Payload, wantPayloads[i]) {
			t.Errorf("frame %d = codec %v payload %x, want H264 %x", i, frame.Codec, frame.Payload, wantPayloads[i])
		}
	}
}

func TestRTSPPublisherConcurrentTracks(t *testing.T) {
	info := &avframe.MediaInfo{AudioCodec: avframe.CodecOpus, SampleRate: 48000}
	pub, stream := newPublisherTestHarness(t, info, RTPTrackMap{
		0:   {Codec: avframe.CodecG711U, ClockRate: 8000},
		111: {Codec: avframe.CodecOpus, ClockRate: 48000},
	})

	const packetsPerTrack = 200
	start := make(chan struct{})
	var wg sync.WaitGroup
	feed := func(pt uint8, ssrc uint32, payloadPrefix byte, timestampStep uint32) {
		defer wg.Done()
		<-start
		for i := 0; i < packetsPerTrack; i++ {
			pkt := newRTPPacket(pt, ssrc, uint16(i), uint32(i)*timestampStep, true, []byte{payloadPrefix, byte(i)})
			if err := pub.FeedRTP(pkt); err != nil {
				t.Errorf("FeedRTP PT %d: %v", pt, err)
				return
			}
		}
	}

	wg.Add(2)
	go feed(111, 1001, 0xa1, 960)
	go feed(0, 2002, 0xb2, 160)
	close(start)
	wg.Wait()

	frames := readPublisherFrames(t, stream, packetsPerTrack*2)
	counts := map[avframe.CodecType]int{}
	for i, frame := range frames {
		counts[frame.Codec]++
		if len(frame.Payload) != 2 {
			t.Errorf("frame %d payload length = %d, want 2", i, len(frame.Payload))
			continue
		}
		switch frame.Codec {
		case avframe.CodecOpus:
			if frame.Payload[0] != 0xa1 {
				t.Errorf("Opus frame %d payload = %x", i, frame.Payload)
			}
		case avframe.CodecG711U:
			if frame.Payload[0] != 0xb2 {
				t.Errorf("G711 frame %d payload = %x", i, frame.Payload)
			}
		default:
			t.Errorf("frame %d unexpected codec %v", i, frame.Codec)
		}
	}
	if counts[avframe.CodecOpus] != packetsPerTrack || counts[avframe.CodecG711U] != packetsPerTrack {
		t.Errorf("frame counts = %v, want %d of each codec", counts, packetsPerTrack)
	}
}

func TestRTSPPublisherConcurrentSameTrack(t *testing.T) {
	info := &avframe.MediaInfo{AudioCodec: avframe.CodecOpus, SampleRate: 48000}
	pub, stream := newPublisherTestHarness(t, info, RTPTrackMap{
		111: {Codec: avframe.CodecOpus, ClockRate: 48000},
	})

	const packetsPerWorker = 200
	start := make(chan struct{})
	var wg sync.WaitGroup
	feed := func(prefix byte, sequenceBase uint16) {
		defer wg.Done()
		<-start
		for i := 0; i < packetsPerWorker; i++ {
			pkt := newRTPPacket(111, 3003, sequenceBase+uint16(i), uint32(sequenceBase+uint16(i))*960, false, []byte{prefix, byte(i)})
			if err := pub.FeedRTP(pkt); err != nil {
				t.Errorf("FeedRTP: %v", err)
				return
			}
		}
	}

	wg.Add(2)
	go feed(0xc3, 0)
	go feed(0xd4, packetsPerWorker)
	close(start)
	wg.Wait()

	frames := readPublisherFrames(t, stream, packetsPerWorker*2)
	counts := map[byte]int{}
	for i, frame := range frames {
		if frame.Codec != avframe.CodecOpus || len(frame.Payload) != 2 {
			t.Errorf("frame %d = codec %v payload %x", i, frame.Codec, frame.Payload)
			continue
		}
		counts[frame.Payload[0]]++
	}
	if counts[0xc3] != packetsPerWorker || counts[0xd4] != packetsPerWorker {
		t.Errorf("payload prefix counts = %v, want %d each", counts, packetsPerWorker)
	}
}

func TestRTSPPublisherTimestampWrap(t *testing.T) {
	info := &avframe.MediaInfo{AudioCodec: avframe.CodecOpus, SampleRate: 48000}
	pub, stream := newPublisherTestHarness(t, info, RTPTrackMap{
		111: {Codec: avframe.CodecOpus, ClockRate: 48000},
	})

	base := uint32(math.MaxUint32 - 4799)
	packets := []*pionrtp.Packet{
		newRTPPacket(111, 1001, 1, base, true, []byte{0xf8, 0x01}),
		newRTPPacket(111, 1001, 2, 0, true, []byte{0xf8, 0x02}),
	}
	for _, pkt := range packets {
		if err := pub.FeedRTP(pkt); err != nil {
			t.Fatalf("FeedRTP: %v", err)
		}
	}

	frames := readPublisherFrames(t, stream, 2)
	if frames[0].DTS != 0 || frames[1].DTS != 100 {
		t.Fatalf("DTS values = [%d, %d], want [0, 100]", frames[0].DTS, frames[1].DTS)
	}
}

func TestRTSPPublisherBoundsSSRCTrackStates(t *testing.T) {
	info := &avframe.MediaInfo{AudioCodec: avframe.CodecOpus, SampleRate: 48000}
	pub, _ := newPublisherTestHarness(t, info, RTPTrackMap{
		111: {Codec: avframe.CodecOpus, ClockRate: 48000},
	})

	for i := 0; i < 64; i++ {
		pkt := newRTPPacket(111, uint32(i+1), 1, 0, true, []byte{0xf8, byte(i)})
		if err := pub.FeedRTP(pkt); err != nil {
			t.Fatalf("FeedRTP SSRC %d: %v", pkt.SSRC, err)
		}
	}

	pub.mu.Lock()
	stateCount := len(pub.trackStates)
	pub.mu.Unlock()
	if stateCount > 16 {
		t.Fatalf("track state count = %d, want at most 16", stateCount)
	}
}

func newPublisherTestHarness(t *testing.T, info *avframe.MediaInfo, ptMap RTPTrackMap) (*RTSPPublisher, *core.Stream) {
	t.Helper()
	stream := core.NewStream(
		"live/rtsp-publisher-test",
		config.StreamConfig{RingBufferSize: 1024},
		config.LimitsConfig{},
		core.NewEventBus(),
	)
	pub, err := NewRTSPPublisherWithTracks("test-publisher", info, stream, ptMap)
	if err != nil {
		t.Fatalf("NewRTSPPublisher: %v", err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	return pub, stream
}

func newRTPPacket(pt uint8, ssrc uint32, sequence uint16, timestamp uint32, marker bool, payload []byte) *pionrtp.Packet {
	return &pionrtp.Packet{
		Header: pionrtp.Header{
			PayloadType:    pt,
			SSRC:           ssrc,
			SequenceNumber: sequence,
			Timestamp:      timestamp,
			Marker:         marker,
		},
		Payload: payload,
	}
}

func readPublisherFrames(t *testing.T, stream *core.Stream, count int) []*avframe.AVFrame {
	t.Helper()
	reader := stream.RingBuffer().NewReader()
	frames := make([]*avframe.AVFrame, 0, count)
	for len(frames) < count {
		frame, ok := reader.TryRead()
		if !ok {
			t.Fatalf("read %d frames, want %d", len(frames), count)
		}
		frames = append(frames, frame)
	}
	if frame, ok := reader.TryRead(); ok {
		t.Fatalf("unexpected extra frame: codec=%v payload=%x", frame.Codec, frame.Payload)
	}
	return frames
}

func avccPayload(nals ...[]byte) []byte {
	var payload []byte
	for _, nal := range nals {
		length := len(nal)
		payload = append(payload, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
		payload = append(payload, nal...)
	}
	return payload
}
