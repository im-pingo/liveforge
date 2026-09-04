package core

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

const productionIngressFixtureDurationMillis int64 = 64_000

type productionIngressBenchmarkFrame struct {
	frame *avframe.AVFrame
	dts   int64
}

func BenchmarkStreamIngressProductionStablePublisher(b *testing.B) {
	stream, publisher, frames, wantPayloads, startCursor := newProductionIngressBenchmark(b)
	frameIndex := 4 // The first four frames form the validated startup GOP.
	var cycleOffset int64

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		fixture := frames[frameIndex]
		fixture.frame.DTS = fixture.dts + cycleOffset
		fixture.frame.PTS = fixture.dts + cycleOffset
		if !stream.WriteFrameForPublisher(publisher, fixture.frame) {
			b.Fatal("production ingress rejected the active publisher frame")
		}
		frameIndex++
		if frameIndex == len(frames) {
			frameIndex = 0
			cycleOffset += productionIngressFixtureDurationMillis
		}
	}
	b.StopTimer()

	if advanced := stream.RingBuffer().WriteCursor() - startCursor; advanced != int64(b.N) {
		b.Fatalf("production ingress advanced ring by %d frames, want %d", advanced, b.N)
	}
	snapshot := stream.StartupSnapshot()
	if !snapshot.Ready || len(snapshot.ReplayFrames) == 0 ||
		snapshot.ReplayFrames[0].FrameType != avframe.FrameTypeKeyframe ||
		len(snapshot.ReplayFrames) > 300 {
		b.Fatalf("production ingress left invalid bounded GOP state: ready=%v replay=%d", snapshot.Ready, len(snapshot.ReplayFrames))
	}
	if !bytes.Equal(frames[0].frame.Payload, wantPayloads[0]) ||
		!bytes.Equal(frames[1].frame.Payload, wantPayloads[1]) ||
		!bytes.Equal(frames[3].frame.Payload, wantPayloads[2]) {
		b.Fatal("production ingress mutated a fixture payload")
	}
}

func BenchmarkStreamIngressWithBitrateLimit(b *testing.B) {
	stream, publisher, frames, _, _ := newProductionIngressBenchmark(b)
	stream.limits.MaxBitratePerStream = 1 << 30
	frameIndex := 4
	var cycleOffset int64

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		fixture := frames[frameIndex]
		fixture.frame.DTS = fixture.dts + cycleOffset
		fixture.frame.PTS = fixture.dts + cycleOffset
		if !stream.WriteFrameForPublisher(publisher, fixture.frame) {
			b.Fatal("bitrate-limited ingress rejected the benchmark fixture")
		}
		frameIndex++
		if frameIndex == len(frames) {
			frameIndex = 0
			cycleOffset += productionIngressFixtureDurationMillis
		}
	}
}

func BenchmarkStreamIngressMatrix(b *testing.B) {
	for _, streamCount := range []int{1, 8, 32} {
		for _, readersPerStream := range []int{0, 4, 16} {
			b.Run(fmt.Sprintf("streams=%d/readers=%d", streamCount, readersPerStream), func(b *testing.B) {
				streams := make([]*Stream, streamCount)
				publishers := make([]Publisher, streamCount)
				frames := make([][]*avframe.AVFrame, streamCount)
				for streamIndex := range streams {
					cfg := config.StreamConfig{GOPCache: true, GOPCacheNum: 1, GOPCacheMaxFrames: 300, RingBufferSize: 4096}
					streams[streamIndex] = NewStream(fmt.Sprintf("bench/matrix/%d", streamIndex), cfg, config.LimitsConfig{}, NewEventBus())
					pub := &testPublisher{id: fmt.Sprintf("matrix-publisher-%d", streamIndex), info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, VideoSequenceHeader: []byte{1, 2, 3}}}
					publishers[streamIndex] = pub
					if err := streams[streamIndex].SetPublisher(pub); err != nil {
						b.Fatal(err)
					}
					frames[streamIndex] = []*avframe.AVFrame{
						avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 1, 2, 3}),
						avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41, 4, 5, 6}),
					}
				}

				ctx, cancel := context.WithCancel(context.Background())
				var readers sync.WaitGroup
				for _, stream := range streams {
					for range readersPerStream {
						reader := stream.RingBuffer().NewReader()
						readers.Add(1)
						go func() {
							defer readers.Done()
							for {
								if _, ok := reader.ReadContext(ctx); !ok {
									return
								}
							}
						}()
					}
				}

				b.ReportAllocs()
				b.ResetTimer()
				var writers sync.WaitGroup
				var rejected atomic.Int64
				for streamIndex := range streams {
					writers.Add(1)
					go func(index int) {
						defer writers.Done()
						for n := 0; n < b.N; n++ {
							if !streams[index].WriteFrameForPublisher(publishers[index], frames[index][n%len(frames[index])]) {
								rejected.Add(1)
							}
						}
					}(streamIndex)
				}
				writers.Wait()
				b.StopTimer()
				cancel()
				readers.Wait()
				for _, stream := range streams {
					stream.Close()
				}
				if got := rejected.Load(); got != 0 {
					b.Fatalf("ingress rejected %d frames", got)
				}
				for index, stream := range streams {
					if got := stream.RingBuffer().WriteCursor(); got != int64(b.N) {
						b.Fatalf("stream %d cursor = %d, want %d", index, got, b.N)
					}
				}
			})
		}
	}
}

func newProductionIngressBenchmark(b *testing.B) (*Stream, Publisher, []productionIngressBenchmarkFrame, [3][]byte, int64) {
	b.Helper()
	cfg := config.StreamConfig{
		GOPCache:            true,
		GOPCacheNum:         2,
		GOPCacheMaxFrames:   300,
		GOPCacheMaxDuration: 10 * time.Second,
		GOPCacheMaxBytes:    32 * 1024 * 1024,
		RingBufferSize:      4096,
	}
	stream := NewStream("bench/production-ingress", cfg, config.LimitsConfig{}, NewEventBus())
	b.Cleanup(stream.Close)
	publisher := &testPublisher{id: "production-benchmark-publisher", info: &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		AudioCodec:          avframe.CodecG711A,
		SampleRate:          8000,
		Channels:            1,
		VideoSequenceHeader: []byte{1, 0x42, 0, 0x1f, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68},
	}}
	if err := stream.SetPublisher(publisher); err != nil {
		b.Fatal(err)
	}

	frames, wantPayloads := productionIngressFrames()
	for _, fixture := range frames[:4] {
		if !stream.WriteFrameForPublisher(publisher, fixture.frame) {
			b.Fatal("production ingress fixture rejected its preflight frame")
		}
	}
	snapshot := stream.StartupSnapshot()
	if !snapshot.Ready || len(snapshot.ReplayFrames) != 4 ||
		!snapshot.ReplayFrames[0].MediaType.IsVideo() ||
		!snapshot.ReplayFrames[1].MediaType.IsAudio() ||
		!snapshot.ReplayFrames[2].MediaType.IsAudio() ||
		!snapshot.ReplayFrames[3].MediaType.IsVideo() {
		b.Fatalf("production ingress preflight did not create an interleaved playable GOP: ready=%v replay=%d", snapshot.Ready, len(snapshot.ReplayFrames))
	}
	return stream, publisher, frames, wantPayloads, stream.RingBuffer().WriteCursor()
}

func productionIngressFrames() ([]productionIngressBenchmarkFrame, [3][]byte) {
	keyPayload := make([]byte, 1200)
	keyPayload[0] = 0x65
	interPayload := make([]byte, 1200)
	interPayload[0] = 0x41
	audioPayload := make([]byte, 160)
	for index := range audioPayload {
		audioPayload[index] = byte(index)
	}

	frames := make([]productionIngressBenchmarkFrame, 0, 4800)
	for timestamp := int64(0); timestamp < productionIngressFixtureDurationMillis; timestamp += 20 {
		if timestamp%40 == 0 {
			frameType := avframe.FrameTypeInterframe
			payload := interPayload
			if timestamp%1000 == 0 {
				frameType = avframe.FrameTypeKeyframe
				payload = keyPayload
			}
			frames = append(frames, productionIngressBenchmarkFrame{
				frame: avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, frameType, timestamp, timestamp, payload),
				dts:   timestamp,
			})
		}
		frames = append(frames, productionIngressBenchmarkFrame{
			frame: avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe, timestamp, timestamp, audioPayload),
			dts:   timestamp,
		})
	}

	wantPayloads := [3][]byte{
		append([]byte(nil), keyPayload...),
		append([]byte(nil), audioPayload...),
		append([]byte(nil), interPayload...),
	}
	return frames, wantPayloads
}
