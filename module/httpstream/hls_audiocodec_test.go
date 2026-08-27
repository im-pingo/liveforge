//go:build audiocodec

package httpstream

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

func TestHLSManagerTranscodesCachedOpusIntoFirstSegment(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecOpus)
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))

	encoder := audiocodec.NewFFmpegEncoder("libopus", 48000, 2)
	defer encoder.Close()
	var opusFrames [][]byte
	for range 8 {
		payload, err := encoder.Encode(&audiocodec.PCMFrame{
			Samples:    make([]int16, 960*2),
			SampleRate: 48000,
			Channels:   2,
		})
		if err != nil {
			t.Fatalf("encode Opus: %v", err)
		}
		opusFrames = append(opusFrames, payload)
	}
	for i, payload := range opusFrames {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
			int64(i*20), int64(i*20), payload,
		))
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			int64(i*20), int64(i*20), []byte{0, 0, 0, 2, 0x41, byte(i)},
		))
	}

	mgr := NewHLSManager("live/hls-opus-first-segment", "/live/hls-opus-first-segment", 1, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
		<-done
	})

	time.Sleep(20 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		1000, 1000, []byte{0, 0, 0, 2, 0x65, 0x7f},
	))

	deadline := time.Now().Add(3 * time.Second)
	for mgr.SegmentCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	segment, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("HLS did not produce its first segment")
	}
	audioFrames := 0
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.MediaType.IsAudio() && frame.Codec == avframe.CodecAAC {
			audioFrames++
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	if audioFrames == 0 {
		t.Fatal("first HLS segment has no transcoded AAC audio from cached Opus")
	}
}

// Catches a codec-boundary return closing the combined transcode reader before
// it can pass later direct video frames to a segmenter.
func TestHLSManagerCodecSwitchKeepsCombinedVideoReaderPlaying(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecG711U)
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))

	mgr := NewHLSManager("live/hls-codec-switch", "/live/hls-codec-switch", 1, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})

	time.Sleep(20 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		100, 100, []byte{0x12, 0x10},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		120, 120, []byte{0x21, 0x10},
	))
	for _, dts := range []int64{1000, 2000} {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			dts, dts, []byte{0, 0, 0, 2, 0x65, byte(dts / 1000)},
		))
	}

	deadline := time.Now().Add(2 * time.Second)
	for mgr.SegmentCount() < 2 && time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("HLS combined transform reader stopped at the AAC codec boundary with %d segments", mgr.SegmentCount())
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if mgr.SegmentCount() < 2 {
		t.Fatalf("HLS segment count = %d, want 2 after post-switch video keyframes", mgr.SegmentCount())
	}
	segment, ok := mgr.GetSegment(1)
	if !ok {
		t.Fatal("HLS did not retain the post-switch video segment")
	}
	videoFrames := 0
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.MediaType.IsVideo() {
			videoFrames++
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	if videoFrames == 0 {
		t.Fatal("post-switch HLS segment contains no direct video")
	}
}
