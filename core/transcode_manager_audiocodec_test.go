//go:build audiocodec

package core

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Catches a codec-boundary return closing the producer-owned ring after one
// HTTP-style consumer hands off and releases its shared transform subscription.
func TestTranscodeManagerHTTPHandoffDoesNotCloseSharedAudioReader(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	start := stream.RingBuffer().WriteCursor()
	httpReader, releaseHTTP, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(avframe.CodecAAC, start)
	if err != nil {
		t.Fatal(err)
	}
	peerReader, releasePeer, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(avframe.CodecAAC, start)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		httpReader.Close()
		peerReader.Close()
		releaseHTTP()
		releasePeer()
		stream.RingBuffer().Close()
	})

	// The HTTP worker changes owner at this header and releases only its reader.
	releaseHTTP()
	httpReader.Close()
	directHeader := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		100, 100, []byte{0x12, 0x10},
	)
	directMedia := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		120, 120, []byte{0x21, 0x10},
	)
	stream.WriteFrame(directHeader)
	stream.WriteFrame(directMedia)

	deadline := time.After(time.Second)
	for {
		result := make(chan struct {
			frame *avframe.AVFrame
			ok    bool
		}, 1)
		go func() {
			frame, ok := peerReader.Read()
			result <- struct {
				frame *avframe.AVFrame
				ok    bool
			}{frame: frame, ok: ok}
		}()
		select {
		case got := <-result:
			if !got.ok {
				t.Fatal("shared transform producer closed the peer reader at the AAC handoff")
			}
			if got.frame == directMedia {
				return
			}
		case <-deadline:
			peerReader.Close()
			t.Fatal("peer reader did not receive direct AAC after the HTTP handoff")
		}
	}
}

// Catches preserving decoder, encoder, timestamp tracker, resampler, or PCM
// buffering across G.711 -> direct AAC -> G.711 source-codec epochs. The
// resumed transformed epoch must behave like a fresh pipeline.
func TestTranscodeManagerRebuildsPipelineForReturningSourceCodec(t *testing.T) {
	const (
		directDTS  = int64(1020)
		reverseDTS = int64(2000)
		wantFrames = 3
	)
	reversePayload := bytes.Repeat([]byte{0xff}, 160)

	baselineStream := newTranscodeTestStream(avframe.CodecG711U)
	baselineReader, releaseBaseline, err := baselineStream.TranscodeManager().GetOrCreateAudioReaderAt(
		avframe.CodecAAC, baselineStream.RingBuffer().WriteCursor(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		baselineReader.Close()
		releaseBaseline()
		baselineStream.RingBuffer().Close()
	})
	writeG711Frames(baselineStream, reverseDTS, 12, reversePayload)
	baseline := readTranscodedAudioFrames(t, baselineReader, wantFrames)

	stream := newTranscodeTestStream(avframe.CodecG711U)
	reader, release, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(
		avframe.CodecAAC, stream.RingBuffer().WriteCursor(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reader.Close()
		release()
		stream.RingBuffer().Close()
	})

	// Two frames produce one AAC access unit while deliberately leaving PCM
	// buffered in the first codec epoch.
	writeG711Frames(stream, 20, 2, bytes.Repeat([]byte{0x00}, 160))
	readTranscodedAudioFrames(t, reader, 1)
	directHeader := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		1000, 1000, []byte{0x12, 0x10},
	)
	directMedia := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		directDTS, directDTS, []byte{0x21, 0x10},
	)
	stream.WriteFrame(directHeader)
	stream.WriteFrame(directMedia)
	readUntilTranscodeFrame(t, reader, directMedia)

	writeG711Frames(stream, reverseDTS, 12, reversePayload)
	resumed := readTranscodedAudioFrames(t, reader, wantFrames)
	lastDTS := directDTS
	for i, frame := range resumed {
		if frame.DTS < reverseDTS || frame.DTS <= lastDTS {
			t.Fatalf("resumed output DTS[%d] = %d after %d, want monotonic DTS anchored at %d", i, frame.DTS, lastDTS, reverseDTS)
		}
		lastDTS = frame.DTS
		if !bytes.Equal(frame.Payload, baseline[i].Payload) {
			t.Fatalf("resumed output payload[%d] differs from a fresh codec epoch; stale PCM or encoder state crossed the AAC epoch", i)
		}
	}
}

func writeG711Frames(stream *Stream, startDTS int64, count int, payload []byte) {
	for i := range count {
		dts := startDTS + int64(i*20)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
			dts, dts, append([]byte(nil), payload...),
		))
	}
}

func readTranscodedAudioFrames(t *testing.T, reader interface {
	ReadContext(context.Context) (*avframe.AVFrame, bool)
}, count int) []*avframe.AVFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	frames := make([]*avframe.AVFrame, 0, count)
	for len(frames) < count {
		frame, ok := reader.ReadContext(ctx)
		if !ok {
			t.Fatalf("transcode reader closed with %d/%d audio frames", len(frames), count)
		}
		if frame != nil && frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			frames = append(frames, frame)
		}
	}
	return frames
}

func readUntilTranscodeFrame(t *testing.T, reader interface {
	ReadContext(context.Context) (*avframe.AVFrame, bool)
}, want *avframe.AVFrame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		frame, ok := reader.ReadContext(ctx)
		if !ok {
			t.Fatal("transcode reader closed before direct AAC passthrough")
		}
		if frame == want {
			return
		}
	}
}
