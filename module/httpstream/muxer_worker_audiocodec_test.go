//go:build audiocodec

package httpstream

import (
	"bytes"
	"context"
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

			// Catches treating the transcoder-generated startup header as a
			// direct-source handoff marker: real transformed media must flow before
			// the source publishes its first AAC header.
			for i := range 20 {
				dts := int64(20 + i*20)
				stream.WriteFrame(avframe.NewAVFrame(
					avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
					dts, dts, bytes.Repeat([]byte{0xff}, 160),
				))
			}
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				500, 500, []byte{0, 0, 0, 2, 0x41, 0x55},
			))
			if format == "fmp4" {
				time.Sleep(10 * time.Millisecond)
				stream.WriteFrame(avframe.NewAVFrame(
					avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
					700, 700, []byte{0, 0, 0, 2, 0x41, 0x57},
				))
			}
			packets := waitForMuxerAudioAt(t, format, initData, reader, startupPackets, func(frame *avframe.AVFrame) bool {
				return frame.DTS < 1000
			})
			preSwitchPacketCount := len(packets)

			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
				1000, 1000, []byte{0x12, 0x10},
			))
			for i, payload := range directAAC {
				dts := int64(1020 + i*23)
				stream.WriteFrame(avframe.NewAVFrame(
					avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
					dts, dts, payload,
				))
			}
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				1500, 1500, videoPayload,
			))
			stream.RingBuffer().Close()
			select {
			case <-workerDone:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s muxer did not stop after source closed", format)
			}

			for {
				packet, ok := reader.TryRead()
				if !ok {
					break
				}
				packets = append(packets, packet)
			}
			frames := demuxMuxerWorkerOutput(t, format, initData, packets)
			assertTranscodedThenSingleDirectAACPath(t, frames, directAAC, 1000, 1500, videoPayload)
			if format == "ts" {
				assertLateAACAnnouncementPrecedesMedia(t, packets[preSwitchPacketCount:], directAAC)
			}
		})
	}
}

// Catches direct-AAC startup choosing a permanent passthrough reader. A worker
// that starts direct must already follow the shared target track so a later
// G.711 epoch can produce AAC without losing its first frame or direct video.
func TestSharedMuxersSwitchFromDirectAACToTransformedAudio(t *testing.T) {
	directAAC := encodeMuxerWorkerAACFrames(t, 4)
	videoPayload := []byte{0, 0, 0, 2, 0x41, 0x5a}

	for _, format := range []string{"flv", "ts", "fmp4"} {
		t.Run(format, func(t *testing.T) {
			stream := core.NewStream("live/"+format+"-direct-start", config.StreamConfig{
				GOPCache: true, GOPCacheNum: 1, RingBufferSize: 256,
			}, config.LimitsConfig{}, core.NewEventBus())
			if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
				VideoCodec:          avframe.CodecH264,
				AudioCodec:          avframe.CodecAAC,
				AudioSequenceHeader: []byte{0x12, 0x10},
				SampleRate:          44100,
				Channels:            2,
			}}); err != nil {
				t.Fatal(err)
			}
			core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))
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
			var packets [][]byte
			if format == "ts" {
				packets = append(packets, readMuxerPacket(t, reader))
			} else {
				initData = waitForMuxerInit(t, inst)
			}

			for i, payload := range directAAC {
				dts := int64(20 + i*23)
				stream.WriteFrame(avframe.NewAVFrame(
					avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
					dts, dts, payload,
				))
			}
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				500, 500, []byte{0, 0, 0, 2, 0x41, 0x05},
			))
			writeG711MuxerFrames(stream, 1000, 20, 0xff)
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				1500, 1500, videoPayload,
			))
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				1700, 1700, []byte{0, 0, 0, 2, 0x41, 0x17},
			))
			stream.RingBuffer().Close()
			select {
			case <-workerDone:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s muxer did not stop after source closed", format)
			}
			for {
				packet, ok := reader.TryRead()
				if !ok {
					break
				}
				packets = append(packets, packet)
			}

			frames := demuxMuxerWorkerOutput(t, format, initData, packets)
			assertDirectThenTransformedMuxerAudio(t, frames, directAAC, videoPayload)
			if format == "ts" {
				seenVideoPMT := false
				seenAACPMT := false
				for _, packet := range packets {
					seenVideoPMT = seenVideoPMT || tsOutputDeclaresStreamType(packet, 0x1b)
					seenAACPMT = seenAACPMT || tsOutputDeclaresStreamType(packet, 0x0f)
				}
				if !seenVideoPMT || !seenAACPMT {
					t.Fatalf("TS PAT/PMT announced H.264/AAC = %v/%v, want both", seenVideoPMT, seenAACPMT)
				}
			}
		})
	}
}

// Catches making HTTP audio ownership one-way or closing a worker's shared
// transform subscription at the direct AAC epoch. All three real muxer workers
// share one producer and must independently reacquire transformed output when
// the same publisher returns to G.711.
func TestSharedMuxerWorkersReturnToTransformedAudioPerCodecEpoch(t *testing.T) {
	stream := core.NewStream("live/shared-bidirectional-audio-owner", config.StreamConfig{
		GOPCache: true, GOPCacheNum: 1, RingBufferSize: 512,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711U,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 8))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 2, 0x65, 0x01},
	))

	// Keep a peer reader open and age the generated startup header out of the
	// small transform ring. This isolates the reverse-owner mutation from the
	// separate generated-header provenance regression above.
	keeper, releaseKeeper, err := stream.TranscodeManager().GetOrCreateAudioReaderAtFromHistory(
		avframe.CodecAAC, stream.StartupSnapshot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		keeper.Close()
		releaseKeeper()
	})
	for i := range 28 {
		dts := int64(20 + i*20)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
			dts, dts, bytes.Repeat([]byte{0x00}, 160),
		))
	}
	readAudioFramesAtOrAfter(t, keeper, 9, 0)

	type runningMuxer struct {
		format   string
		inst     *core.MuxerInstance
		reader   *core.SharedBufferReader
		initData []byte
		packets  [][]byte
		done     chan struct{}
	}
	workers := make([]*runningMuxer, 0, 3)
	for _, format := range []string{"flv", "ts", "fmp4"} {
		inst, reader := newMuxerWorkerInstance(stream)
		worker := &runningMuxer{format: format, inst: inst, reader: reader, done: make(chan struct{})}
		workers = append(workers, worker)
		go func() {
			switch worker.format {
			case "flv":
				new(Module).runFLVMuxer(worker.inst, stream)
			case "ts":
				new(Module).runTSMuxer(worker.inst, stream)
			case "fmp4":
				new(Module).runFMP4Muxer(worker.inst, stream)
			}
			close(worker.done)
		}()
		if format == "ts" {
			worker.packets = append(worker.packets, readMuxerPacket(t, reader))
		} else {
			worker.initData = waitForMuxerInit(t, inst)
		}
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		800, 800, []byte{0, 0, 0, 2, 0x41, 0x08},
	))
	time.Sleep(10 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		900, 900, []byte{0, 0, 0, 2, 0x41, 0x09},
	))
	for _, worker := range workers {
		worker.packets = waitForMuxerAudioAt(t, worker.format, worker.initData, worker.reader, worker.packets, func(frame *avframe.AVFrame) bool {
			return frame.DTS < 1000
		})
	}

	directAAC := encodeMuxerWorkerAACFrames(t, 4)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		1000, 1000, []byte{0x12, 0x10},
	))
	for i, payload := range directAAC {
		dts := int64(1020 + i*23)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
			dts, dts, payload,
		))
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		1500, 1500, []byte{0, 0, 0, 2, 0x41, 0x15},
	))
	for _, worker := range workers {
		worker.packets = waitForMuxerPayloads(t, worker.format, worker.initData, worker.reader, worker.packets, directAAC)
	}

	for i := range 20 {
		dts := int64(2000 + i*20)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
			dts, dts, bytes.Repeat([]byte{0xff}, 160),
		))
	}
	readAudioFramesAtOrAfter(t, keeper, 1, 2000)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		2500, 2500, []byte{0, 0, 0, 2, 0x41, 0x25},
	))
	time.Sleep(10 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		2700, 2700, []byte{0, 0, 0, 2, 0x41, 0x27},
	))
	for _, worker := range workers {
		worker.packets = waitForMuxerAudioAt(t, worker.format, worker.initData, worker.reader, worker.packets, func(frame *avframe.AVFrame) bool {
			return frame.DTS >= 2000
		})
	}
	stream.RingBuffer().Close()
	for _, worker := range workers {
		select {
		case <-worker.done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s muxer did not stop after source closed", worker.format)
		}
		for {
			packet, ok := worker.reader.TryRead()
			if !ok {
				break
			}
			worker.packets = append(worker.packets, packet)
		}
		frames := demuxMuxerWorkerOutput(t, worker.format, worker.initData, worker.packets)
		assertBidirectionalMuxerAudio(t, frames, directAAC)
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

func writeG711MuxerFrames(stream *core.Stream, startDTS int64, count int, sample byte) {
	for i := range count {
		dts := startDTS + int64(i*20)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
			dts, dts, bytes.Repeat([]byte{sample}, 160),
		))
	}
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

func waitForMuxerAudioAt(
	t *testing.T,
	format string,
	initData []byte,
	reader *core.SharedBufferReader,
	packets [][]byte,
	match func(*avframe.AVFrame) bool,
) [][]byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for {
			packet, ok := reader.TryRead()
			if !ok {
				break
			}
			packets = append(packets, packet)
		}
		for _, frame := range demuxMuxerWorkerOutput(t, format, initData, packets) {
			if frame != nil && frame.MediaType.IsAudio() &&
				frame.FrameType != avframe.FrameTypeSequenceHeader && match(frame) {
				return packets
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s muxer emitted no matching audio media before timeout", format)
	return nil
}

func waitForMuxerPayloads(
	t *testing.T,
	format string,
	initData []byte,
	reader *core.SharedBufferReader,
	packets [][]byte,
	wantPayloads [][]byte,
) [][]byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for {
			packet, ok := reader.TryRead()
			if !ok {
				break
			}
			packets = append(packets, packet)
		}
		seen := make(map[string]int, len(wantPayloads))
		for _, frame := range demuxMuxerWorkerOutput(t, format, initData, packets) {
			if frame != nil && frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
				seen[string(frame.Payload)]++
			}
		}
		allSeen := true
		for _, payload := range wantPayloads {
			if seen[string(payload)] != 1 {
				allSeen = false
				break
			}
		}
		if allSeen {
			return packets
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s muxer did not emit all direct AAC payloads exactly once", format)
	return nil
}

func readAudioFramesAtOrAfter(t *testing.T, reader interface {
	ReadContext(context.Context) (*avframe.AVFrame, bool)
}, count int, minDTS int64) []*avframe.AVFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	frames := make([]*avframe.AVFrame, 0, count)
	for len(frames) < count {
		frame, ok := reader.ReadContext(ctx)
		if !ok {
			t.Fatalf("transform reader closed with %d/%d matching audio frames", len(frames), count)
		}
		if frame != nil && frame.MediaType.IsAudio() &&
			frame.FrameType != avframe.FrameTypeSequenceHeader && frame.DTS >= minDTS {
			frames = append(frames, frame)
		}
	}
	return frames
}

func assertTranscodedThenSingleDirectAACPath(
	t *testing.T,
	frames []*avframe.AVFrame,
	directAAC [][]byte,
	switchDTS, videoDTS int64,
	videoPayload []byte,
) {
	t.Helper()
	want := make(map[string]int, len(directAAC))
	for _, payload := range directAAC {
		want[string(payload)]++
	}
	got := make(map[string]int, len(directAAC))
	var preSwitchAudio, postSwitchAudio, liveVideo int
	for _, frame := range frames {
		if frame == nil || frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if frame.MediaType.IsAudio() {
			if frame.DTS < switchDTS {
				preSwitchAudio++
			} else {
				postSwitchAudio++
			}
			got[string(frame.Payload)]++
		}
		if frame.MediaType.IsVideo() && frame.DTS == videoDTS {
			liveVideo++
		}
	}
	if preSwitchAudio == 0 {
		t.Fatal("muxer emitted no transformed AAC media before the direct AAC switch")
	}
	if postSwitchAudio != len(directAAC) {
		t.Fatalf("post-switch audio media frame count = %d, want %d direct AAC frames only", postSwitchAudio, len(directAAC))
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

func assertBidirectionalMuxerAudio(t *testing.T, frames []*avframe.AVFrame, directAAC [][]byte) {
	t.Helper()
	directCounts := make(map[string]int, len(directAAC))
	postReverseDTS := make(map[int64]int)
	var preSwitchAudio, postReverseAudio, postReverseVideo int
	for _, frame := range frames {
		if frame == nil || frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if frame.MediaType.IsAudio() {
			if frame.DTS < 1000 {
				preSwitchAudio++
			}
			if frame.DTS >= 2000 {
				postReverseAudio++
				postReverseDTS[frame.DTS]++
			}
			directCounts[string(frame.Payload)]++
		}
		if frame.MediaType.IsVideo() && frame.DTS == 2500 {
			postReverseVideo++
		}
	}
	if preSwitchAudio == 0 || postReverseAudio == 0 {
		t.Fatalf("transformed audio before/after direct epoch = %d/%d, want both non-zero", preSwitchAudio, postReverseAudio)
	}
	for _, payload := range directAAC {
		if got := directCounts[string(payload)]; got != 1 {
			t.Fatalf("direct AAC payload occurrence count = %d, want 1", got)
		}
	}
	for dts, count := range postReverseDTS {
		if count != 1 {
			t.Fatalf("post-reverse transformed AAC DTS %d occurred %d times, want once", dts, count)
		}
	}
	if postReverseVideo != 1 {
		t.Fatalf("post-reverse direct video count = %d, want 1", postReverseVideo)
	}
}

func assertDirectThenTransformedMuxerAudio(
	t *testing.T,
	frames []*avframe.AVFrame,
	directAAC [][]byte,
	videoPayload []byte,
) {
	t.Helper()
	directCounts := make(map[string]int, len(directAAC))
	var transformedAudio, liveVideo int
	lastTransformedDTS := int64(-1)
	for _, frame := range frames {
		if frame == nil || frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if frame.MediaType.IsAudio() {
			directCounts[string(frame.Payload)]++
			if frame.DTS >= 1000 {
				if frame.DTS <= lastTransformedDTS {
					t.Fatalf("post-switch transformed DTS moved backward or duplicated: %d after %d", frame.DTS, lastTransformedDTS)
				}
				lastTransformedDTS = frame.DTS
				transformedAudio++
			}
		}
		if frame.MediaType.IsVideo() && frame.DTS == 1500 && bytes.Equal(frame.Payload, videoPayload) {
			liveVideo++
		}
	}
	for _, payload := range directAAC {
		if got := directCounts[string(payload)]; got != 1 {
			t.Fatalf("direct-start AAC payload occurrence count = %d, want 1", got)
		}
	}
	if transformedAudio == 0 {
		t.Fatal("direct-start muxer emitted no transformed AAC after the G.711 epoch")
	}
	if liveVideo != 1 {
		t.Fatalf("direct video occurrence count after audio transition = %d, want 1", liveVideo)
	}
}

func assertLateAACAnnouncementPrecedesMedia(t *testing.T, packets [][]byte, directAAC [][]byte) {
	t.Helper()
	seenAACPMT := false
	for _, output := range packets {
		if tsOutputDeclaresStreamType(output, 0x0f) {
			seenAACPMT = true
		}
		for _, payload := range directAAC {
			if bytes.Contains(output, payload) {
				if !seenAACPMT {
					t.Fatal("TS emitted direct AAC media before the post-transition AAC PMT")
				}
				return
			}
		}
	}
	t.Fatal("TS emitted no direct post-transition AAC media")
}
