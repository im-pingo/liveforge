//go:build audiocodec

package core

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func BenchmarkTranscodeReaderFanoutAdmission(b *testing.B) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	b.Cleanup(stream.Close)
	snapshot := stream.StartupSnapshot()
	seed, release, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(avframe.CodecAAC, snapshot)
	if err != nil {
		b.Fatal(err)
	}
	seed.Close()
	release()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		reader, release, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(snapshot.MediaInfo.AudioCodec, snapshot)
		if err != nil {
			b.Fatal(err)
		}
		reader.Close()
		release()
	}
}

// Catches a codec-boundary return closing the producer-owned ring after one
// HTTP-style consumer hands off and releases its shared transform subscription.
func TestTranscodeManagerHTTPHandoffDoesNotCloseSharedAudioReader(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	snapshot := stream.StartupSnapshot()
	httpReader, releaseHTTP, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(avframe.CodecAAC, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	peerReader, releasePeer, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(avframe.CodecAAC, snapshot)
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
		avframe.CodecAAC, baselineStream.StartupSnapshot(),
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
		avframe.CodecAAC, stream.StartupSnapshot(),
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

// Catches treating an unsupported codec observed by the asynchronously
// starting producer as terminal. The shared ring must remain mapped and both
// existing and later readers must receive a subsequent supported epoch.
func TestTranscodeManagerUnsupportedStartupEpochDoesNotPoisonSharedTrack(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG729)
	reader, release, err := stream.TranscodeManager().GetOrCreateAudioReaderAtFromHistory(
		avframe.CodecAAC, stream.StartupSnapshot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reader.Close()
		release()
		stream.RingBuffer().Close()
	})

	deadline := time.Now().Add(time.Second)
	for {
		stream.TranscodeManager().mu.Lock()
		track := stream.TranscodeManager().audioTracks[avframe.CodecAAC]
		closed := track == nil || track.ringBuffer.IsClosed()
		ready := track != nil && track.ringBuffer.WriteCursor() > 0
		stream.TranscodeManager().mu.Unlock()
		if closed {
			t.Fatal("unsupported startup codec closed and poisoned the mapped audio track")
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shared audio producer did not finish asynchronous startup")
		}
		time.Sleep(time.Millisecond)
	}

	writeG711Frames(stream, 1000, 12, bytes.Repeat([]byte{0xff}, 160))
	lateSnapshot := stream.StartupSnapshot()
	lateReader, releaseLate, err := stream.TranscodeManager().GetOrCreateAudioReaderAtFromHistory(
		avframe.CodecAAC, lateSnapshot,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lateReader.Close()
		releaseLate()
	})
	writeDirectAACFrames(stream, 3000, 2)

	for _, frame := range readTranscodedAudioFrames(t, reader, 2) {
		if frame.DTS < 1000 {
			t.Fatalf("existing reader emitted stale DTS %d before the supported epoch", frame.DTS)
		}
	}
	lateFrames := readCurrentEpochAudioWithHeader(t, lateReader, lateSnapshot.audioCodecEpoch, 2)
	for _, frame := range lateFrames {
		if frame.DTS < 3000 {
			t.Fatalf("new reader emitted pre-floor DTS %d before post-snapshot direct AAC", frame.DTS)
		}
	}

	stream.TranscodeManager().mu.Lock()
	track := stream.TranscodeManager().audioTracks[avframe.CodecAAC]
	poisoned := track == nil || track.ringBuffer.IsClosed()
	stream.TranscodeManager().mu.Unlock()
	if poisoned {
		t.Fatal("supported recovery left the mapped audio track closed")
	}
}

// Catches late snapshot readers starting at retained output from an older
// source-codec epoch. The reader's first media must be at the snapshot epoch
// floor, never stale audio with a backward timestamp.
func TestTranscodeManagerLateSnapshotReaderStartsAtCurrentAudioEpoch(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	keeper, releaseKeeper, err := stream.TranscodeManager().GetOrCreateAudioReaderAtFromHistory(
		avframe.CodecAAC, stream.StartupSnapshot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		keeper.Close()
		releaseKeeper()
		stream.RingBuffer().Close()
	})

	writeG711Frames(stream, 20, 16, bytes.Repeat([]byte{0x00}, 160))
	readTranscodedAudioFrames(t, keeper, 3)
	directHeader := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		1000, 1000, []byte{0x12, 0x10},
	)
	directMedia := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		1020, 1020, []byte{0x21, 0x10},
	)
	stream.WriteFrame(directHeader)
	stream.WriteFrame(directMedia)
	readUntilTranscodeFrame(t, keeper, directMedia)
	writeG711Frames(stream, 2000, 12, bytes.Repeat([]byte{0xff}, 160))
	current := readTranscodedAudioFrames(t, keeper, 2)

	snapshot := stream.StartupSnapshot()
	lateReader, releaseLate, err := stream.TranscodeManager().GetOrCreateAudioReaderAtFromHistory(
		avframe.CodecAAC, snapshot,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		lateReader.Close()
		releaseLate()
	}()
	writeDirectAACFrames(stream, 3000, 2)

	first := readTranscodedAudioFrames(t, lateReader, 1)[0]
	if first.AudioCodecEpoch < current[0].AudioCodecEpoch {
		t.Fatalf("late reader emitted retained epoch %d before snapshot epoch %d", first.AudioCodecEpoch, current[0].AudioCodecEpoch)
	}
	if first.DTS < current[0].DTS {
		t.Fatalf("late reader first DTS = %d, want at least current DTS %d", first.DTS, current[0].DTS)
	}
	if first.DTS < 3000 {
		t.Fatalf("late reader emitted pre-floor retained DTS %d", first.DTS)
	}
}

func TestTranscodeManagerGenerationEndFinalizesPartialAACAndDelayedPackets(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	defer stream.Close()
	tm := stream.TranscodeManager()
	snapshot := stream.StartupSnapshot()
	reader, release, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer reader.Close()

	const firstDTS = int64(1000)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
		firstDTS, firstDTS, bytes.Repeat([]byte{0xff}, 160),
	))
	endCursor := stream.RingBuffer().WriteCursor()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 3*time.Second)
	if !tm.WaitForSourceCursor(avframe.CodecAAC, endCursor, waitCtx) {
		cancelWait()
		t.Fatal("transcoder did not consume the partial source frame")
	}
	cancelWait()

	var mediaBeforeEnd int
	for {
		frame, ok := reader.TryRead()
		if !ok {
			break
		}
		if frame != nil && frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			mediaBeforeEnd++
		}
	}
	if mediaBeforeEnd != 0 {
		t.Fatalf("partial PCM produced %d AAC packets before generation end, want 0", mediaBeforeEnd)
	}

	tm.mu.Lock()
	track := tm.tracks[avframe.CodecAAC]
	tm.mu.Unlock()
	if track == nil {
		t.Fatal("AAC track disappeared before generation retirement")
	}
	stream.RemovePublisher()
	if err := stream.SetPublisher(&testPublisher{
		id:   "replacement-after-audio-tail",
		info: &avframe.MediaInfo{AudioCodec: avframe.CodecG711U},
	}); err != nil {
		t.Fatalf("install replacement publisher: %v", err)
	}

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRead()
	var tail []*avframe.AVFrame
	for {
		frame, ok := reader.ReadContext(readCtx)
		if !ok {
			break
		}
		if frame != nil && frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			tail = append(tail, frame)
		}
	}
	if err := readCtx.Err(); err != nil {
		t.Fatalf("transcoded generation tail did not close: %v", err)
	}
	if len(tail) == 0 {
		t.Fatal("generation tail produced no AAC packets")
	}
	for i, frame := range tail {
		if i > 0 && frame.DTS <= tail[i-1].DTS {
			t.Fatalf("generation tail DTS[%d] = %d after %d, want strictly increasing", i, frame.DTS, tail[i-1].DTS)
		}
		if frame.AudioCodecEpoch != snapshot.audioCodecEpoch {
			t.Fatalf("generation tail epoch[%d] = %d, want %d", i, frame.AudioCodecEpoch, snapshot.audioCodecEpoch)
		}
		if frame.AudioProvenance != avframe.FrameProvenanceTranscoded {
			t.Fatalf("generation tail provenance[%d] = %d, want transcoded", i, frame.AudioProvenance)
		}
	}
	if !track.ringBuffer.IsClosed() {
		t.Fatal("transcode output ring remained open after generation tail")
	}
	if got := track.sourceCursor.Load(); got != endCursor {
		t.Fatalf("transcode source cursor = %d, want generation end %d", got, endCursor)
	}
	closedCursor := track.ringBuffer.WriteCursor()
	time.Sleep(20 * time.Millisecond)
	if got := track.ringBuffer.WriteCursor(); got != closedCursor {
		t.Fatalf("transcode output advanced from %d to %d after ring close", closedCursor, got)
	}
}

func TestTranscodeManagerLastConsumerCancellationDiscardsPartialTail(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	defer stream.Close()
	tm := stream.TranscodeManager()
	reader, release, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, stream.StartupSnapshot())
	if err != nil {
		t.Fatal(err)
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
		1000, 1000, bytes.Repeat([]byte{0xff}, 160),
	))
	sourceCursor := stream.RingBuffer().WriteCursor()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 3*time.Second)
	if !tm.WaitForSourceCursor(avframe.CodecAAC, sourceCursor, waitCtx) {
		cancelWait()
		t.Fatal("transcoder did not consume the partial source frame")
	}
	cancelWait()

	tm.mu.Lock()
	track := tm.tracks[avframe.CodecAAC]
	tm.mu.Unlock()
	if track == nil {
		t.Fatal("AAC track disappeared before consumer cancellation")
	}
	beforeCancel := track.ringBuffer.WriteCursor()
	release()
	reader.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !track.ringBuffer.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !track.ringBuffer.IsClosed() {
		t.Fatal("transcode output ring remained open after last consumer cancellation")
	}
	if got := track.ringBuffer.WriteCursor(); got != beforeCancel {
		t.Fatalf("last-consumer cancellation published %d unowned tail frames", got-beforeCancel)
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

func writeDirectAACFrames(stream *Stream, startDTS int64, count int) {
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		startDTS, startDTS, []byte{0x12, 0x10},
	))
	for i := range count {
		dts := startDTS + int64((i+1)*20)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
			dts, dts, []byte{0x21, byte(i)},
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

func readCurrentEpochAudioWithHeader(t *testing.T, reader interface {
	ReadContext(context.Context) (*avframe.AVFrame, bool)
}, epoch uint64, count int) []*avframe.AVFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	frames := make([]*avframe.AVFrame, 0, count)
	var headerEpoch uint64
	for len(frames) < count {
		frame, ok := reader.ReadContext(ctx)
		if !ok {
			t.Fatalf("current-epoch reader closed with %d/%d audio frames", len(frames), count)
		}
		if frame == nil || !frame.MediaType.IsAudio() {
			continue
		}
		if frame.AudioCodecEpoch < epoch {
			t.Fatalf("reader emitted audio epoch %d below floor %d", frame.AudioCodecEpoch, epoch)
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			headerEpoch = frame.AudioCodecEpoch
			continue
		}
		if headerEpoch != frame.AudioCodecEpoch {
			t.Fatalf("reader emitted epoch %d media after target sequence header epoch %d", frame.AudioCodecEpoch, headerEpoch)
		}
		frames = append(frames, frame)
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
