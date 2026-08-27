//go:build audiocodec

package httpstream

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
)

func TestFLVMuxerTranscodeKeepsDirectVideoAtLiveCursor(t *testing.T) {
	stream := core.NewStream("live/flv-transcode-cursor", config.StreamConfig{
		GOPCache: true, GOPCacheNum: 1, RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711U,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 32))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{1, 0x64},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		4000, 4000, []byte{0, 0, 0, 2, 0x65, 1},
	))

	inst, reader := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFLVMuxer(inst, stream)
		close(workerDone)
	}()
	initData := waitForMuxerInit(t, inst)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		5000, 5000, []byte{0, 0, 0, 2, 0x41, 2},
	))
	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FLV muxer did not stop after source closed")
	}

	output := append(initData, collectMuxerOutput(reader)...)
	demuxer := flv.NewDemuxer(bytes.NewReader(output))
	videoHeaderCount := 0
	audioHeaderCount := 0
	cachedVideoCount := 0
	liveVideoCount := 0
	for {
		frame, err := demuxer.ReadTag()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("demux FLV output: %v", err)
		}
		if frame == nil {
			continue
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if frame.MediaType.IsVideo() {
				videoHeaderCount++
			} else if frame.MediaType.IsAudio() {
				audioHeaderCount++
			}
		}
		if frame.MediaType.IsVideo() && frame.DTS == 4000 {
			cachedVideoCount++
		}
		if frame.MediaType.IsVideo() && frame.DTS == 5000 {
			liveVideoCount++
		}
	}
	if videoHeaderCount != 1 || audioHeaderCount != 1 || cachedVideoCount != 1 || liveVideoCount != 1 {
		t.Fatalf("FLV startup counts video-header/audio-header/cached/live = %d/%d/%d/%d, want 1/1/1/1",
			videoHeaderCount, audioHeaderCount, cachedVideoCount, liveVideoCount)
	}
}
