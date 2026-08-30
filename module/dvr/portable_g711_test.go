//go:build !audiocodec

package dvr

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

func TestDVRPortableG711FallbackPublishesVideoOnlyTS(t *testing.T) {
	stream := core.NewStream("live/portable-g711", config.StreamConfig{
		GOPCache:          true,
		GOPCacheNum:       1,
		GOPCacheMaxFrames: 32,
		RingBufferSize:    128,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	publisher := &dvrStaleHistoryPublisher{
		id: "portable-g711-publisher",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecG711A,
		},
	}
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeSequenceHeader,
		0,
		0,
		dvrTestAVCConfig(),
	))

	session, err := NewSession("live/portable-g711", stream, config.DVRConfig{
		Path:            filepath.Join(t.TempDir(), "{stream_key}"),
		SegmentDuration: 100 * time.Millisecond,
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()
	for i := int64(0); i < 5; i++ {
		dts := i * 100
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			avframe.FrameTypeKeyframe,
			dts,
			dts,
			portableAVCCNAL(0x65, byte(i)),
		))
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio,
			avframe.CodecG711A,
			avframe.FrameTypeInterframe,
			dts,
			dts,
			[]byte{0x7e, 0x11, 0x22, byte(i)},
		))
	}

	deadline := time.Now().Add(2 * time.Second)
	for session.Index().Len() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	session.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("portable DVR session did not stop")
	}

	videoFrames := 0
	audioFrames := 0
	for _, segment := range session.Index().Segments() {
		data, err := os.ReadFile(segment.DiskPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Fatalf("portable DVR published an empty segment: %#v", segment)
		}
		demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
			if frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader {
				videoFrames++
			}
			if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
				audioFrames++
			}
		})
		demuxer.Feed(data)
		demuxer.Flush()
	}
	if videoFrames == 0 {
		t.Fatal("portable DVR did not publish a demuxable video frame")
	}
	if audioFrames != 0 {
		t.Fatalf("portable DVR published %d audio frames without audiocodec", audioFrames)
	}
}

func portableAVCCNAL(nalType, marker byte) []byte {
	nal := []byte{nalType, 0x88, 0x84, marker}
	payload := make([]byte, 4+len(nal))
	binary.BigEndian.PutUint32(payload, uint32(len(nal)))
	copy(payload[4:], nal)
	return payload
}
