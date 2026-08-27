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
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
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

func TestSharedMuxersSwitchFromTranscodedToDirectAACOnce(t *testing.T) {
	directAAC := encodeMuxerWorkerAACFrames(t, 12)
	videoPayload := []byte{0, 0, 0, 2, 0x41, 0x7e}

	for _, format := range []string{"flv", "ts", "fmp4"} {
		t.Run(format, func(t *testing.T) {
			stream := core.NewStream("live/"+format+"-audio-owner", config.StreamConfig{
				GOPCache: true, GOPCacheNum: 1, RingBufferSize: 128,
			}, config.LimitsConfig{}, core.NewEventBus())
			if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
				VideoCodec: avframe.CodecH264,
				AudioCodec: avframe.CodecG711U,
				SampleRate: 8000,
				Channels:   1,
			}}); err != nil {
				t.Fatal(err)
			}
			core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 128))
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
				0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
			))
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
				0, 0, []byte{0, 0, 0, 2, 0x65, 0x01},
			))

			inst, reader := newMuxerWorkerInstance(stream)
			workerDone := make(chan struct{})
			go func() {
				switch format {
				case "flv":
					new(Module).runFLVMuxer(inst, stream)
				case "ts":
					new(Module).runTSMuxer(inst, stream)
				case "fmp4":
					new(Module).runFMP4Muxer(inst, stream)
				}
				close(workerDone)
			}()

			var initData []byte
			var startupPackets [][]byte
			if format == "ts" {
				startupPackets = append(startupPackets, readMuxerPacket(t, reader))
			} else {
				initData = waitForMuxerInit(t, inst)
			}

			// Activate the G.711-to-AAC reader without supplying enough source
			// samples to produce an encoded AAC access unit.
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
				20, 20, []byte{0xff},
			))
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
				40, 40, []byte{0x12, 0x10},
			))
			for i, payload := range directAAC {
				dts := int64(60 + i*23)
				stream.WriteFrame(avframe.NewAVFrame(
					avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
					dts, dts, payload,
				))
			}
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				500, 500, videoPayload,
			))
			stream.RingBuffer().Close()
			select {
			case <-workerDone:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s muxer did not stop after source closed", format)
			}

			packets := startupPackets
			for {
				packet, ok := reader.TryRead()
				if !ok {
					break
				}
				packets = append(packets, packet)
			}
			frames := demuxMuxerWorkerOutput(t, format, initData, packets)
			assertSingleDirectAACPath(t, frames, directAAC, videoPayload)
			if format == "ts" {
				assertLateAACAnnouncementPrecedesMedia(t, packets[len(startupPackets):])
			}
		})
	}
}

func encodeMuxerWorkerAACFrames(t *testing.T, count int) [][]byte {
	t.Helper()
	encoder := audiocodec.NewFFmpegEncoder("aac", 44100, 2)
	defer encoder.Close()

	frames := make([][]byte, 0, count)
	for frameIndex := range count {
		samples := make([]int16, 1024*2)
		for i := range samples {
			samples[i] = int16((i*37+frameIndex*211)%20000 - 10000)
		}
		payload, err := encoder.Encode(&audiocodec.PCMFrame{
			Samples: samples, SampleRate: 44100, Channels: 2,
		})
		if err != nil {
			t.Fatalf("encode direct AAC frame %d: %v", frameIndex, err)
		}
		if len(payload) == 0 {
			t.Fatalf("encode direct AAC frame %d returned no payload", frameIndex)
		}
		frames = append(frames, payload)
	}
	return frames
}

func demuxMuxerWorkerOutput(t *testing.T, format string, initData []byte, packets [][]byte) []*avframe.AVFrame {
	t.Helper()
	var frames []*avframe.AVFrame
	switch format {
	case "flv":
		output := append([]byte(nil), initData...)
		for _, packet := range packets {
			output = append(output, packet...)
		}
		demuxer := flv.NewDemuxer(bytes.NewReader(output))
		for {
			frame, err := demuxer.ReadTag()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("demux FLV output: %v", err)
			}
			frames = append(frames, frame)
		}
	case "ts":
		demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
			frames = append(frames, frame)
		})
		for _, packet := range packets {
			demuxer.Feed(packet)
		}
		demuxer.Flush()
	case "fmp4":
		demuxer, err := fmp4.NewDemuxer(initData)
		if err != nil {
			t.Fatalf("create fMP4 demuxer: %v", err)
		}
		for _, packet := range packets {
			parsed, err := demuxer.Parse(packet)
			if err != nil {
				t.Fatalf("demux fMP4 output: %v", err)
			}
			frames = append(frames, parsed...)
		}
	}
	return frames
}

func assertSingleDirectAACPath(t *testing.T, frames []*avframe.AVFrame, directAAC [][]byte, videoPayload []byte) {
	t.Helper()
	want := make(map[string]int, len(directAAC))
	for _, payload := range directAAC {
		want[string(payload)]++
	}
	got := make(map[string]int, len(directAAC))
	var audioMedia, liveVideo int
	for _, frame := range frames {
		if frame == nil || frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if frame.MediaType.IsAudio() {
			audioMedia++
			got[string(frame.Payload)]++
		}
		if frame.MediaType.IsVideo() && frame.DTS == 500 {
			liveVideo++
		}
	}
	if audioMedia != len(directAAC) {
		t.Fatalf("audio media frame count = %d, want %d direct AAC frames only", audioMedia, len(directAAC))
	}
	for payload, count := range want {
		if got[payload] != count {
			t.Fatalf("direct AAC payload occurrence count = %d, want %d", got[payload], count)
		}
	}
	if liveVideo != 1 {
		t.Fatalf("direct live video count = %d, want 1 (payload %x)", liveVideo, videoPayload)
	}
}

func assertLateAACAnnouncementPrecedesMedia(t *testing.T, packets [][]byte) {
	t.Helper()
	seenAACPMT := false
	for _, output := range packets {
		if tsOutputDeclaresStreamType(output, 0x0f) {
			seenAACPMT = true
		}
		if tsOutputContainsPID(output, ts.PIDAudio) {
			if !seenAACPMT {
				t.Fatal("TS emitted AAC media before the post-transition AAC PMT")
			}
			return
		}
	}
	t.Fatal("TS emitted no post-transition AAC media")
}
