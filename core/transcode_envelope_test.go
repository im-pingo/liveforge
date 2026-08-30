package core

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// Catches replacing source-cursor identity with output order/DTS or copying an
// AVFrame/payload merely to attach attribution metadata.
func TestTranscodeEnvelopePreservesDirectFrameIdentityAndSourceSpan(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	defer stream.Close()
	tm := stream.TranscodeManager()
	reader, release, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecG711A, stream.StartupSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer release()

	videoPayload := []byte{0x01, 0x02, 0x03}
	video := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		9000, 9000, videoPayload,
	)
	audioPayload := []byte{0x11, 0x22, 0x33}
	audio := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe,
		9020, 9020, audioPayload,
	)
	stream.WriteFrame(video)
	stream.WriteFrame(audio)

	endCursor := stream.RingBuffer().WriteCursor()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !tm.WaitForSourceCursor(avframe.CodecG711A, endCursor, ctx) {
		t.Fatal("transcode producer did not consume direct source frames")
	}

	tm.mu.Lock()
	track := tm.tracks[avframe.CodecG711A]
	tm.mu.Unlock()
	if track == nil {
		t.Fatal("combined transcode track disappeared")
	}
	outputReader := track.ringBuffer.NewReader()
	defer outputReader.Close()

	wants := []struct {
		frame *avframe.AVFrame
		span  audiocodec.SourceSpan
	}{
		{frame: video, span: audiocodec.SourceSpan{Begin: 0, End: 1}},
		{frame: audio, span: audiocodec.SourceSpan{Begin: 1, End: 2}},
	}
	for i, want := range wants {
		output, ok := outputReader.TryRead()
		if !ok {
			t.Fatalf("internal output ended at record %d", i)
		}
		if output.frame != want.frame {
			t.Fatalf("record %d frame pointer = %p, want %p", i, output.frame, want.frame)
		}
		if output.sourceSpan != want.span {
			t.Fatalf("record %d source span = %+v, want %+v", i, output.sourceSpan, want.span)
		}
		if len(output.frame.Payload) == 0 || &output.frame.Payload[0] != &want.frame.Payload[0] {
			t.Fatalf("record %d payload backing storage was copied", i)
		}
	}
}

// Catches filtering with SourceSpan.End > floor: the crossing [3,5) packet
// contains pre-floor samples and must not reach the late reader.
func TestTranscodeSourceFloorUsesSpanBeginAndPreservesExistingReader(t *testing.T) {
	tm := &TranscodeManager{bufSize: 16}
	track := &TranscodedTrack{
		targetCodec: avframe.CodecAAC,
		ringBuffer:  util.NewRingBuffer[transcodeOutput](16),
	}
	existing, stopExisting := tm.newTrackReader(track, true, 0, 0)
	defer stopExisting()

	staleHeader := transcodeHeaderOutput(6, []byte{0x06})
	currentHeader := transcodeHeaderOutput(7, []byte{0x07})
	old := transcodeMediaOutput(7, 20, []byte{0x20}, audiocodec.SourceSpan{Begin: 2, End: 3})
	crossing := transcodeMediaOutput(7, 30, []byte{0x30}, audiocodec.SourceSpan{Begin: 3, End: 5})
	accepted := transcodeMediaOutput(7, 40, []byte{0x40}, audiocodec.SourceSpan{Begin: 4, End: 5})
	track.ringBuffer.Write(staleHeader)
	track.ringBuffer.Write(currentHeader)
	track.ringBuffer.Write(old)
	track.ringBuffer.Write(crossing)

	late, stopLate := tm.newTrackReader(track, true, 7, 4)
	defer stopLate()
	track.ringBuffer.Write(accepted)

	for i, want := range []*avframe.AVFrame{
		staleHeader.frame, currentHeader.frame, old.frame, crossing.frame, accepted.frame,
	} {
		if got := readTranscodeFrame(t, existing); got != want {
			t.Fatalf("existing reader record %d = %p, want %p", i, got, want)
		}
	}
	if got := readTranscodeFrame(t, late); got != currentHeader.frame {
		t.Fatalf("late reader first record = %p, want current header %p", got, currentHeader.frame)
	}
	if got := readTranscodeFrame(t, late); got != accepted.frame {
		t.Fatalf("late reader first media = %p, want accepted media %p", got, accepted.frame)
	}
}

// Catches relying on the generated header still being retained in the output
// ring when a late reader accepts its first same-epoch payload.
func TestTranscodeSourceFloorReplaysCurrentHeaderWhenRingHistoryLostIt(t *testing.T) {
	tm := &TranscodeManager{bufSize: 4}
	track := &TranscodedTrack{
		targetCodec: avframe.CodecAAC,
		ringBuffer:  util.NewRingBuffer[transcodeOutput](2),
	}
	header := transcodeHeaderOutput(7, []byte{0x07})
	track.cacheSequenceHeader(header)
	track.ringBuffer.Write(header)
	track.ringBuffer.Write(transcodeMediaOutput(
		7, 30, []byte{0x30}, audiocodec.SourceSpan{Begin: 3, End: 4},
	))
	accepted := transcodeMediaOutput(
		7, 40, []byte{0x40}, audiocodec.SourceSpan{Begin: 4, End: 5},
	)
	track.ringBuffer.Write(accepted)

	late, stopLate := tm.newTrackReader(track, true, 7, 4)
	defer stopLate()
	if got := readTranscodeFrame(t, late); got != header.frame {
		t.Fatalf("late reader first record = %p, want cached current header %p", got, header.frame)
	}
	if got := readTranscodeFrame(t, late); got != accepted.frame {
		t.Fatalf("late reader first media = %p, want accepted payload %p", got, accepted.frame)
	}
}

// Catches replacing a matching retained header when the producer advances its
// cache before a lagging bridge handles same-epoch media still in output history.
func TestTranscodeSourceFloorReplaysMatchingHeaderAfterCacheAdvances(t *testing.T) {
	tm := &TranscodeManager{bufSize: 4}
	track := &TranscodedTrack{
		targetCodec: avframe.CodecAAC,
		ringBuffer:  util.NewRingBuffer[transcodeOutput](2),
	}
	header7 := transcodeHeaderOutput(7, []byte{0x07})
	track.cacheSequenceHeader(header7)
	track.ringBuffer.Write(header7)
	track.ringBuffer.Write(transcodeMediaOutput(
		7, 30, []byte{0x30}, audiocodec.SourceSpan{Begin: 3, End: 4},
	))
	payload7 := transcodeMediaOutput(
		7, 40, []byte{0x40}, audiocodec.SourceSpan{Begin: 4, End: 5},
	)
	track.ringBuffer.Write(payload7)
	track.cacheSequenceHeader(transcodeHeaderOutput(8, []byte{0x08}))

	late, stopLate := tm.newTrackReader(track, true, 7, 4)
	defer stopLate()
	if got := readTranscodeFrame(t, late); got != header7.frame {
		t.Fatalf("late reader first record epoch = %d, want matching header epoch 7", got.AudioCodecEpoch)
	}
	if got := readTranscodeFrame(t, late); got != payload7.frame {
		t.Fatalf("late reader first media epoch = %d, want retained payload epoch 7", got.AudioCodecEpoch)
	}
}

func transcodeHeaderOutput(epoch uint64, payload []byte) transcodeOutput {
	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		0, 0, payload,
	)
	frame.AudioCodecEpoch = epoch
	frame.AudioProvenance = avframe.FrameProvenanceTranscoded
	return transcodeOutput{
		frame: frame, kind: transcodeOutputSequenceHeader, audioEpoch: epoch,
	}
}

func transcodeMediaOutput(epoch uint64, dts int64, payload []byte, span audiocodec.SourceSpan) transcodeOutput {
	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		dts, dts, payload,
	)
	frame.AudioCodecEpoch = epoch
	frame.AudioProvenance = avframe.FrameProvenanceTranscoded
	return transcodeOutput{
		frame: frame, sourceSpan: span, kind: transcodeOutputMedia, audioEpoch: epoch,
	}
}

func readTranscodeFrame(t *testing.T, reader *util.RingReader[*avframe.AVFrame]) *avframe.AVFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	frame, ok := reader.ReadContext(ctx)
	if !ok {
		t.Fatal("transcode reader closed before expected frame")
	}
	return frame
}

type provenanceEncoder struct {
	spans []audiocodec.SourceSpan
}

type stereoProvenanceEncoder struct {
	encoded [][]int16
	spans   []audiocodec.SourceSpan
}

type terminalAttributionDecoder struct {
	calls int
}

func (d *terminalAttributionDecoder) SetExtradata([]byte) {}
func (d *terminalAttributionDecoder) Decode([]byte) (*audiocodec.PCMFrame, error) {
	d.calls++
	return &audiocodec.PCMFrame{
		Samples: []int16{int16(d.calls)}, SampleRate: 4000, Channels: 1, // #nosec G115 -- decoder call count is a bounded test fixture.
	}, nil
}
func (d *terminalAttributionDecoder) SampleRate() int { return 4000 }
func (d *terminalAttributionDecoder) Channels() int   { return 1 }
func (d *terminalAttributionDecoder) Close()          {}

type terminalAttributedResampler struct {
	calls                int
	attributedDrainCalls int
	legacyCalls          int
	legacyDrainCalls     int
}

func (r *terminalAttributedResampler) Resample(*audiocodec.PCMFrame) *audiocodec.PCMFrame {
	r.legacyCalls++
	return nil
}

func (r *terminalAttributedResampler) ResampleAttributed(_ *audiocodec.PCMFrame, span audiocodec.SourceSpan) (*audiocodec.AttributedPCMFrame, error) {
	r.calls++
	if r.calls == 1 {
		return &audiocodec.AttributedPCMFrame{}, nil
	}
	return &audiocodec.AttributedPCMFrame{
		PCMFrame:   audiocodec.PCMFrame{Samples: []int16{1, 2, 3}, SampleRate: 8000, Channels: 1},
		SourceSpan: audiocodec.SourceSpan{Begin: 20, End: span.End},
	}, nil
}

func (r *terminalAttributedResampler) Drain() *audiocodec.PCMFrame {
	r.legacyDrainCalls++
	return nil
}

func (r *terminalAttributedResampler) DrainAttributed() (*audiocodec.AttributedPCMFrame, error) {
	r.attributedDrainCalls++
	return &audiocodec.AttributedPCMFrame{
		PCMFrame:   audiocodec.PCMFrame{Samples: []int16{4, 5}, SampleRate: 8000, Channels: 1},
		SourceSpan: audiocodec.SourceSpan{Begin: 21, End: 22},
	}, nil
}

func (r *terminalAttributedResampler) Close() {}

type terminalAttributedEncoder struct {
	encoded              [][]int16
	spans                []audiocodec.SourceSpan
	attributedDrainCalls int
	legacyCalls          int
	legacyDrainCalls     int
}

func (e *terminalAttributedEncoder) Encode(*audiocodec.PCMFrame) ([]byte, error) {
	e.legacyCalls++
	return nil, nil
}

func (e *terminalAttributedEncoder) EncodeAttributed(pcm *audiocodec.PCMFrame, span audiocodec.SourceSpan) ([]audiocodec.AttributedPacket, error) {
	e.encoded = append(e.encoded, append([]int16(nil), pcm.Samples...))
	e.spans = append(e.spans, span)
	return []audiocodec.AttributedPacket{{Payload: []byte{byte(0xa0 + len(e.encoded))}, SourceSpan: span}}, nil // #nosec G115 -- encoded fixture count stays below one byte.
}

func (e *terminalAttributedEncoder) Drain() ([][]byte, error) {
	e.legacyDrainCalls++
	return nil, nil
}

func (e *terminalAttributedEncoder) DrainAttributed() ([]audiocodec.AttributedPacket, error) {
	e.attributedDrainCalls++
	return []audiocodec.AttributedPacket{
		{Payload: []byte{0xd1}, SourceSpan: audiocodec.SourceSpan{Begin: 20, End: 22}},
		{Payload: []byte{0xd2}, SourceSpan: audiocodec.SourceSpan{Begin: 21, End: 22}},
	}, nil
}

func (e *terminalAttributedEncoder) SampleRate() int { return 8000 }
func (e *terminalAttributedEncoder) Channels() int   { return 1 }
func (e *terminalAttributedEncoder) FrameSize() int  { return 4 }
func (e *terminalAttributedEncoder) Close()          {}

func (e *provenanceEncoder) Encode(*audiocodec.PCMFrame) ([]byte, error) {
	return []byte{0xee}, nil
}

func (e *provenanceEncoder) EncodeAttributed(_ *audiocodec.PCMFrame, span audiocodec.SourceSpan) ([]audiocodec.AttributedPacket, error) {
	e.spans = append(e.spans, span)
	return []audiocodec.AttributedPacket{{Payload: []byte{byte(len(e.spans))}, SourceSpan: span}}, nil // #nosec G115 -- provenance fixture count stays below one byte.
}

func (e *provenanceEncoder) SampleRate() int { return 8000 }
func (e *provenanceEncoder) Channels() int   { return 1 }
func (e *provenanceEncoder) FrameSize() int  { return 4 }
func (e *provenanceEncoder) Close()          {}

func (e *stereoProvenanceEncoder) Encode(*audiocodec.PCMFrame) ([]byte, error) {
	return nil, nil
}

func (e *stereoProvenanceEncoder) EncodeAttributed(pcm *audiocodec.PCMFrame, span audiocodec.SourceSpan) ([]audiocodec.AttributedPacket, error) {
	e.encoded = append(e.encoded, append([]int16(nil), pcm.Samples...))
	e.spans = append(e.spans, span)
	return []audiocodec.AttributedPacket{{Payload: []byte{byte(len(e.spans))}, SourceSpan: span}}, nil // #nosec G115 -- provenance fixture count stays below one byte.
}

func (*stereoProvenanceEncoder) SampleRate() int { return 8000 }
func (*stereoProvenanceEncoder) Channels() int   { return 2 }
func (*stereoProvenanceEncoder) FrameSize() int  { return 4 }
func (*stereoProvenanceEncoder) Close()          {}

// Catches attributing fixed encoder frames to only the newest PCM chunk or
// retaining an already-consumed old span in every later output.
func TestTranscodePCMProvenanceConsumesOnlyFrameContributors(t *testing.T) {
	track := &TranscodedTrack{
		targetCodec: avframe.CodecAAC,
		ringBuffer:  util.NewRingBuffer[transcodeOutput](8),
	}
	encoder := &provenanceEncoder{}
	pipeline := &audioTranscodePipeline{
		track: track, sourceEpoch: 3, encoder: encoder,
	}
	pipeline.ts.Init(0, encoder.SampleRate())
	pipeline.tsInited = true
	pipeline.encodePCM(
		&audiocodec.PCMFrame{Samples: []int16{1, 2, 3}, SampleRate: 8000, Channels: 1},
		audiocodec.SourceSpan{Begin: 10, End: 11},
	)
	pipeline.encodePCM(
		&audiocodec.PCMFrame{Samples: []int16{4, 5, 6}, SampleRate: 8000, Channels: 1},
		audiocodec.SourceSpan{Begin: 11, End: 12},
	)
	pipeline.encodePCM(
		&audiocodec.PCMFrame{Samples: []int16{7, 8}, SampleRate: 8000, Channels: 1},
		audiocodec.SourceSpan{Begin: 12, End: 13},
	)

	wantSpans := []audiocodec.SourceSpan{
		{Begin: 10, End: 12},
		{Begin: 11, End: 13},
	}
	if len(encoder.spans) != len(wantSpans) {
		t.Fatalf("attributed encoder calls = %d, want %d", len(encoder.spans), len(wantSpans))
	}
	outputReader := track.ringBuffer.NewReader()
	defer outputReader.Close()
	for i, want := range wantSpans {
		if encoder.spans[i] != want {
			t.Fatalf("encoder span %d = %+v, want %+v", i, encoder.spans[i], want)
		}
		output, ok := outputReader.TryRead()
		if !ok {
			t.Fatalf("encoded output ended at packet %d", i)
		}
		if output.sourceSpan != want {
			t.Fatalf("output span %d = %+v, want %+v", i, output.sourceSpan, want)
		}
	}
}

// Catches counting interleaved stereo samples as samples per channel. Each
// encoder frame crosses contributor boundaries and leaves a partial segment
// that must age into exactly one later frame.
func TestTranscodeStereoPCMProvenanceUsesSamplesPerChannel(t *testing.T) {
	track := &TranscodedTrack{
		targetCodec: avframe.CodecAAC,
		ringBuffer:  util.NewRingBuffer[transcodeOutput](8),
	}
	encoder := &stereoProvenanceEncoder{}
	pipeline := &audioTranscodePipeline{track: track, sourceEpoch: 4, encoder: encoder}
	pipeline.ts.Init(0, encoder.SampleRate())
	pipeline.tsInited = true
	chunks := []struct {
		samples []int16
		span    audiocodec.SourceSpan
	}{
		{samples: []int16{1, 101, 2, 102, 3, 103}, span: audiocodec.SourceSpan{Begin: 30, End: 31}},
		{samples: []int16{4, 104, 5, 105, 6, 106}, span: audiocodec.SourceSpan{Begin: 31, End: 32}},
		{samples: []int16{7, 107, 8, 108, 9, 109, 10, 110}, span: audiocodec.SourceSpan{Begin: 32, End: 33}},
		{samples: []int16{11, 111, 12, 112}, span: audiocodec.SourceSpan{Begin: 33, End: 34}},
	}
	for _, chunk := range chunks {
		pipeline.encodePCM(&audiocodec.PCMFrame{
			Samples: chunk.samples, SampleRate: 8000, Channels: 2,
		}, chunk.span)
	}

	wantPCM := [][]int16{
		{1, 101, 2, 102, 3, 103, 4, 104},
		{5, 105, 6, 106, 7, 107, 8, 108},
		{9, 109, 10, 110, 11, 111, 12, 112},
	}
	wantSpans := []audiocodec.SourceSpan{
		{Begin: 30, End: 32},
		{Begin: 31, End: 33},
		{Begin: 32, End: 34},
	}
	if len(encoder.spans) != len(wantSpans) {
		t.Fatalf("stereo attributed encoder calls = %d, want %d", len(encoder.spans), len(wantSpans))
	}
	outputReader := track.ringBuffer.NewReader()
	defer outputReader.Close()
	for i, want := range wantSpans {
		if !slices.Equal(encoder.encoded[i], wantPCM[i]) {
			t.Fatalf("stereo PCM %d = %v, want %v", i, encoder.encoded[i], wantPCM[i])
		}
		if encoder.spans[i] != want {
			t.Fatalf("stereo encoder span %d = %+v, want %+v", i, encoder.spans[i], want)
		}
		output, ok := outputReader.TryRead()
		if !ok {
			t.Fatalf("stereo output ended at packet %d", i)
		}
		if output.sourceSpan != want {
			t.Fatalf("stereo output span %d = %+v, want %+v", i, output.sourceSpan, want)
		}
	}
}

// Catches dropping zero-output resampler provenance or assigning zero/newest-
// only attribution to padded PCM and later encoder-drain packets.
func TestTranscodeAttributedTerminalDrainPreservesSpansAndContentOnce(t *testing.T) {
	track := &TranscodedTrack{
		targetCodec: avframe.CodecAAC,
		ringBuffer:  util.NewRingBuffer[transcodeOutput](8),
	}
	decoder := &terminalAttributionDecoder{}
	resampler := &terminalAttributedResampler{}
	encoder := &terminalAttributedEncoder{}
	pipeline := &audioTranscodePipeline{
		track: track, sourceEpoch: 9, decoder: decoder, encoder: encoder,
		resampler: resampler, resampled: true,
	}
	for i, span := range []audiocodec.SourceSpan{{Begin: 20, End: 21}, {Begin: 21, End: 22}} {
		pipeline.encode(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
			int64(1000+i*20), int64(1000+i*20), []byte{byte(i)},
		), span)
	}
	pipeline.finalize()
	pipeline.finalize()

	if resampler.legacyCalls != 0 || resampler.legacyDrainCalls != 0 {
		t.Fatalf("legacy resampler calls = %d/%d, want 0/0", resampler.legacyCalls, resampler.legacyDrainCalls)
	}
	if resampler.calls != 2 || resampler.attributedDrainCalls != 1 {
		t.Fatalf("attributed resampler calls/drain = %d/%d, want 2/1", resampler.calls, resampler.attributedDrainCalls)
	}
	if encoder.legacyCalls != 0 || encoder.legacyDrainCalls != 0 {
		t.Fatalf("legacy encoder calls = %d/%d, want 0/0", encoder.legacyCalls, encoder.legacyDrainCalls)
	}
	if encoder.attributedDrainCalls != 1 {
		t.Fatalf("attributed encoder drain calls = %d, want 1", encoder.attributedDrainCalls)
	}
	wantPCM := [][]int16{{1, 2, 3, 4}, {5, 0, 0, 0}}
	wantEncodeSpans := []audiocodec.SourceSpan{{Begin: 20, End: 22}, {Begin: 21, End: 22}}
	for i := range wantPCM {
		if !slices.Equal(encoder.encoded[i], wantPCM[i]) {
			t.Fatalf("encoded PCM %d = %v, want %v", i, encoder.encoded[i], wantPCM[i])
		}
		if encoder.spans[i] != wantEncodeSpans[i] {
			t.Fatalf("encoded span %d = %+v, want %+v", i, encoder.spans[i], wantEncodeSpans[i])
		}
	}

	wantPayloads := [][]byte{{0xa1}, {0xa2}, {0xd1}, {0xd2}}
	wantOutputSpans := []audiocodec.SourceSpan{
		{Begin: 20, End: 22}, {Begin: 21, End: 22},
		{Begin: 20, End: 22}, {Begin: 21, End: 22},
	}
	outputReader := track.ringBuffer.NewReader()
	defer outputReader.Close()
	for i := range wantPayloads {
		output, ok := outputReader.TryRead()
		if !ok {
			t.Fatalf("terminal output ended at packet %d", i)
		}
		if !bytes.Equal(output.frame.Payload, wantPayloads[i]) || output.sourceSpan != wantOutputSpans[i] {
			t.Fatalf("packet %d payload/span = %x/%+v, want %x/%+v", i, output.frame.Payload, output.sourceSpan, wantPayloads[i], wantOutputSpans[i])
		}
	}
	if _, ok := outputReader.TryRead(); ok {
		t.Fatal("terminal media was published more than once")
	}
}

// Catches calling attributed drain before any PCM submission, which has no
// valid source span and makes the reviewed encoder fail with ErrInvalidSourceSpan.
func TestTranscodeAttributedEncoderWithoutSubmissionDoesNotDrain(t *testing.T) {
	encoder := &terminalAttributedEncoder{}
	pipeline := &audioTranscodePipeline{encoder: encoder}
	pipeline.drainEncoder(false)
	if encoder.attributedDrainCalls != 0 || encoder.legacyDrainCalls != 0 {
		t.Fatalf("drain calls without submission = %d/%d, want 0/0", encoder.attributedDrainCalls, encoder.legacyDrainCalls)
	}
}

type overwritePendingDecoder struct{}

func (*overwritePendingDecoder) SetExtradata([]byte) {}
func (*overwritePendingDecoder) Decode([]byte) (*audiocodec.PCMFrame, error) {
	return &audiocodec.PCMFrame{Samples: []int16{1, 2, 3}, SampleRate: 8000, Channels: 1}, nil
}
func (*overwritePendingDecoder) SampleRate() int { return 8000 }
func (*overwritePendingDecoder) Channels() int   { return 1 }
func (*overwritePendingDecoder) Close()          {}

type overwriteDelayedEncoder struct {
	entered       chan struct{}
	release       chan struct{}
	enteredOnce   sync.Once
	encodeCalls   int
	drainCalls    int
	legacyCalls   int
	delayedSource audiocodec.SourceSpan
}

func (e *overwriteDelayedEncoder) Encode(*audiocodec.PCMFrame) ([]byte, error) {
	e.legacyCalls++
	return nil, nil
}

func (e *overwriteDelayedEncoder) EncodeAttributed(_ *audiocodec.PCMFrame, span audiocodec.SourceSpan) ([]audiocodec.AttributedPacket, error) {
	e.encodeCalls++
	if e.encodeCalls == 1 {
		e.delayedSource = span
		e.enteredOnce.Do(func() { close(e.entered) })
		<-e.release
		return nil, nil
	}
	return []audiocodec.AttributedPacket{{Payload: []byte{0xf1}, SourceSpan: span}}, nil
}

func (e *overwriteDelayedEncoder) Drain() ([][]byte, error) {
	e.legacyCalls++
	return nil, nil
}

func (e *overwriteDelayedEncoder) DrainAttributed() ([]audiocodec.AttributedPacket, error) {
	e.drainCalls++
	return []audiocodec.AttributedPacket{{Payload: []byte{0xd1}, SourceSpan: e.delayedSource}}, nil
}

func (*overwriteDelayedEncoder) SampleRate() int { return 8000 }
func (*overwriteDelayedEncoder) Channels() int   { return 1 }
func (*overwriteDelayedEncoder) FrameSize() int  { return 4 }
func (*overwriteDelayedEncoder) Close()          {}

// Catches ignoring RingReadResult.Overwritten, finalizing pending PCM or
// delayed encoder output as a clean tail, or bleeding termination into a
// replacement generation.
func TestTranscodeProducerSourceOverwriteTerminatesWithExactCause(t *testing.T) {
	const (
		overwriteSourceCodec avframe.CodecType = 240
		overwriteTargetCodec avframe.CodecType = 241
	)
	stream := NewStream(
		"transcode-overwrite",
		config.StreamConfig{RingBufferSize: 2},
		config.LimitsConfig{},
		NewEventBus(),
	)
	if err := stream.SetPublisher(&testPublisher{
		id: "overwrite-source", info: &avframe.MediaInfo{AudioCodec: overwriteSourceCodec},
	}); err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	snapshot := stream.StartupSnapshot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	track := &TranscodedTrack{
		targetCodec:        overwriteTargetCodec,
		ringBuffer:         util.NewRingBuffer[transcodeOutput](8),
		sourceStart:        0,
		sourceAdvance:      make(chan struct{}),
		generationDone:     snapshot.GenerationDone,
		generationBoundary: snapshot.generationBoundary,
	}
	track.sourceCursor.Store(0)
	registry := audiocodec.Global()
	encoder := &overwriteDelayedEncoder{entered: make(chan struct{}), release: make(chan struct{})}
	registry.RegisterDecoder(overwriteSourceCodec, func() audiocodec.Decoder { return &overwritePendingDecoder{} })
	registry.RegisterEncoder(overwriteTargetCodec, func() audiocodec.Encoder { return encoder })
	tm := NewTranscodeManager(stream, registry, 8)
	done := make(chan struct{})
	var releaseEncoder sync.Once
	unblockEncoder := func() { releaseEncoder.Do(func() { close(encoder.release) }) }
	defer unblockEncoder()

	go func() {
		defer close(done)
		tm.transcodeLoop(ctx, track, 0, 0, false)
	}()
	outputReader := track.ringBuffer.NewReader()
	defer outputReader.Close()
	for i := 0; i < 2; i++ {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, overwriteSourceCodec, avframe.FrameTypeInterframe,
			int64(i*20), int64(i*20), []byte{byte(i)},
		))
	}
	select {
	case <-encoder.entered:
	case <-time.After(time.Second):
		t.Fatal("producer did not reach the deterministic delayed-encoder barrier")
	}
	for i := 2; i < 6; i++ {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, overwriteSourceCodec, avframe.FrameTypeInterframe,
			int64(i*20), int64(i*20), []byte{byte(i)},
		))
	}
	stream.RemovePublisher()
	unblockEncoder()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("overwritten transcode producer did not terminate")
	}
	var overwrite *transcodeSourceOverwriteError
	cause := track.terminationCause()
	if !errors.As(cause, &overwrite) {
		t.Fatalf("termination cause = %T %v, want typed source overwrite", cause, cause)
	}
	if overwrite.Overwritten != 2 {
		t.Fatalf("overwritten count = %d, want 2", overwrite.Overwritten)
	}
	if errors.Is(cause, errTranscodeGenerationComplete) {
		t.Fatal("source overwrite was reported as clean generation completion")
	}
	if output, ok := outputReader.TryRead(); ok {
		t.Fatalf("overwrite published padded/drained tail payload %x with span %+v", output.frame.Payload, output.sourceSpan)
	}
	if encoder.encodeCalls != 1 || encoder.drainCalls != 1 || encoder.legacyCalls != 0 {
		t.Fatalf("overwrite encoder calls encode/drain/legacy = %d/%d/%d, want 1/1/0", encoder.encodeCalls, encoder.drainCalls, encoder.legacyCalls)
	}

	if err := stream.SetPublisher(&testPublisher{
		id: "overwrite-replacement", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatalf("install replacement publisher: %v", err)
	}
	replacementSnapshot := stream.StartupSnapshot()
	replacement := &TranscodedTrack{
		targetCodec:        overwriteTargetCodec,
		ringBuffer:         util.NewRingBuffer[transcodeOutput](8),
		sourceStart:        replacementSnapshot.SourceCursor,
		sourceAdvance:      make(chan struct{}),
		generationDone:     replacementSnapshot.GenerationDone,
		generationBoundary: replacementSnapshot.generationBoundary,
	}
	replacement.sourceCursor.Store(replacementSnapshot.SourceCursor)
	replacementDone := make(chan struct{})
	go func() {
		defer close(replacementDone)
		tm.transcodeLoop(ctx, replacement, replacementSnapshot.SourceCursor, 0, false)
	}()
	replacementReader := replacement.ringBuffer.NewReader()
	defer replacementReader.Close()
	replacementFrame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		1000, 1000, []byte{0xaa},
	)
	stream.WriteFrame(replacementFrame)
	if output := readTranscodeOutput(t, replacementReader); output.frame != replacementFrame {
		t.Fatalf("replacement output frame = %p, want %p", output.frame, replacementFrame)
	}
	stream.RemovePublisher()
	select {
	case <-replacementDone:
	case <-time.After(time.Second):
		t.Fatal("replacement transcode producer did not terminate")
	}
	if cause := replacement.terminationCause(); !errors.Is(cause, errTranscodeGenerationComplete) {
		t.Fatalf("replacement termination cause = %v, want clean generation completion", cause)
	}
}

func readTranscodeOutput(t *testing.T, reader *util.RingReader[transcodeOutput]) transcodeOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, ok := reader.ReadContext(ctx)
	if !ok {
		t.Fatal("transcode output closed before expected record")
	}
	return output
}

// Catches forwarding the retained envelope returned with an internal bridge
// overwrite or closing the shared track used by an unrelated peer reader.
func TestTranscodeBridgeOverwriteClosesOnlyThatReader(t *testing.T) {
	tm := &TranscodeManager{bufSize: 4}
	track := &TranscodedTrack{
		targetCodec: avframe.CodecG711A,
		ringBuffer:  util.NewRingBuffer[transcodeOutput](2),
	}
	outputs := []transcodeOutput{
		transcodeMediaOutput(1, 0, []byte{0}, audiocodec.SourceSpan{Begin: 0, End: 1}),
		transcodeMediaOutput(1, 1, []byte{1}, audiocodec.SourceSpan{Begin: 1, End: 2}),
		transcodeMediaOutput(1, 2, []byte{2}, audiocodec.SourceSpan{Begin: 2, End: 3}),
	}
	for i := range outputs {
		outputs[i].frame.Codec = avframe.CodecG711A
		track.ringBuffer.Write(outputs[i])
	}

	lappedShared := track.ringBuffer.NewReaderAt(0)
	lapped, stopLapped := tm.bridgeTrackReader(track, lappedShared, 0, 0)
	defer stopLapped()
	peerShared := track.ringBuffer.NewReader()
	peer, stopPeer := tm.bridgeTrackReader(track, peerShared, 0, 0)
	defer stopPeer()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := lapped.ReadResultContext(ctx)
	if result.OK {
		t.Fatalf("lapped bridge forwarded retained post-gap frame %p", result.Value)
	}
	if ctx.Err() != nil {
		t.Fatal("lapped bridge did not close after overwrite")
	}
	if got := readTranscodeFrame(t, peer); got != outputs[1].frame {
		t.Fatalf("peer first retained frame = %p, want %p", got, outputs[1].frame)
	}
	if track.ringBuffer.IsClosed() {
		t.Fatal("one bridge overwrite closed the shared track")
	}
}

// Catches collapsing last-subscriber release and manager Reset into one
// indistinguishable cancellation cause.
func TestTranscodeTrackCancellationDistinguishesReleaseAndReset(t *testing.T) {
	releaseStream := newTranscodeTestStream(avframe.CodecG711U)
	releaseReader, release, err := releaseStream.TranscodeManager().GetOrCreateReader(avframe.CodecG711A)
	if err != nil {
		t.Fatal(err)
	}
	releaseStream.TranscodeManager().mu.Lock()
	releasedTrack := releaseStream.TranscodeManager().tracks[avframe.CodecG711A]
	releaseStream.TranscodeManager().mu.Unlock()
	release()
	release()
	waitForTranscodeTrackClose(t, releasedTrack)
	releaseReader.Close()
	if cause := releasedTrack.terminationCause(); !errors.Is(cause, errTranscodeSubscriberReleased) {
		t.Fatalf("last-subscriber cause = %v, want subscriber release", cause)
	}
	releaseStream.Close()

	resetStream := newTranscodeTestStream(avframe.CodecG711U)
	resetReader, resetRelease, err := resetStream.TranscodeManager().GetOrCreateReader(avframe.CodecG711A)
	if err != nil {
		t.Fatal(err)
	}
	defer resetRelease()
	resetStream.TranscodeManager().mu.Lock()
	resetTrack := resetStream.TranscodeManager().tracks[avframe.CodecG711A]
	resetStream.TranscodeManager().mu.Unlock()
	resetStream.TranscodeManager().Reset()
	waitForTranscodeTrackClose(t, resetTrack)
	resetReader.Close()
	if cause := resetTrack.terminationCause(); !errors.Is(cause, errTranscodeManagerReset) {
		t.Fatalf("manager-reset cause = %v, want manager reset", cause)
	}
	resetStream.Close()
}

func waitForTranscodeTrackClose(t *testing.T, track *TranscodedTrack) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader := track.ringBuffer.NewReaderAt(track.ringBuffer.WriteCursor())
	defer reader.Close()
	for {
		if _, ok := reader.ReadContext(ctx); !ok {
			break
		}
	}
	if ctx.Err() != nil {
		t.Fatal("transcode track did not close after termination")
	}
}
