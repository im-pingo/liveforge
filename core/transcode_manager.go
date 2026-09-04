package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// TranscodedTrack holds source-attributed output for a specific target codec.
type TranscodedTrack struct {
	targetCodec        avframe.CodecType
	sourceCodec        atomic.Uint32
	sourceEpoch        atomic.Uint64
	ringBuffer         *util.RingBuffer[transcodeOutput]
	sourceStart        int64
	sourceCursor       atomic.Int64
	sourceMu           sync.Mutex
	sourceAdvance      chan struct{}
	generationDone     <-chan struct{}
	generationBoundary *streamGenerationBoundary
	headerMu           sync.RWMutex
	sequenceHeaders    []transcodeOutput
	terminationMu      sync.Mutex
	termination        error
	subCount           int
	cancel             context.CancelCauseFunc
}

// TranscodeTaskSnapshot is a bounded point-in-time view of one on-demand
// audio conversion task. It is safe to expose through management APIs.
type TranscodeTaskSnapshot struct {
	SourceCodec avframe.CodecType
	TargetCodec avframe.CodecType
	AudioOnly   bool
	State       string
	Subscribers int
	LastError   string
}

const transcodeSequenceHeaderCacheLimit = 8

var (
	errTranscodeGenerationComplete = errors.New("transcode generation complete")
	errTranscodeSubscriberReleased = errors.New("transcode subscriber released")
	errTranscodeManagerReset       = errors.New("transcode manager reset")
	errTranscodeSourceClosed       = errors.New("transcode source closed")
)

type transcodeSourceOverwriteError struct {
	Overwritten int64
}

func (e *transcodeSourceOverwriteError) Error() string {
	return fmt.Sprintf("transcode source overwritten by %d frames", e.Overwritten)
}

func (track *TranscodedTrack) setTerminationCause(cause error) {
	if cause == nil {
		return
	}
	track.terminationMu.Lock()
	defer track.terminationMu.Unlock()
	if track.termination == nil {
		track.termination = cause
	}
}

func (track *TranscodedTrack) terminationCause() error {
	track.terminationMu.Lock()
	defer track.terminationMu.Unlock()
	return track.termination
}

func (track *TranscodedTrack) cacheSequenceHeader(output transcodeOutput) {
	if output.frame == nil || output.kind != transcodeOutputSequenceHeader {
		return
	}
	track.headerMu.Lock()
	defer track.headerMu.Unlock()
	for i := len(track.sequenceHeaders) - 1; i >= 0; i-- {
		if track.sequenceHeaders[i].audioEpoch == output.audioEpoch {
			track.sequenceHeaders[i] = output
			return
		}
	}
	track.sequenceHeaders = append(track.sequenceHeaders, output)
	if len(track.sequenceHeaders) > transcodeSequenceHeaderCacheLimit {
		copy(track.sequenceHeaders, track.sequenceHeaders[len(track.sequenceHeaders)-transcodeSequenceHeaderCacheLimit:])
		track.sequenceHeaders = track.sequenceHeaders[:transcodeSequenceHeaderCacheLimit]
	}
}

func (track *TranscodedTrack) sequenceHeaderForEpoch(epoch uint64) (transcodeOutput, bool) {
	track.headerMu.RLock()
	defer track.headerMu.RUnlock()
	for i := len(track.sequenceHeaders) - 1; i >= 0; i-- {
		if output := track.sequenceHeaders[i]; output.audioEpoch == epoch {
			return output, output.frame != nil
		}
	}
	return transcodeOutput{}, false
}

type transcodeOutputKind uint8

const (
	transcodeOutputMedia transcodeOutputKind = iota
	transcodeOutputSequenceHeader
)

// transcodeOutput keeps source identity beside a frame without copying its
// payload. Configuration records intentionally carry no media source span.
type transcodeOutput struct {
	frame      *avframe.AVFrame
	sourceSpan audiocodec.SourceSpan
	kind       transcodeOutputKind
	audioEpoch uint64
}

// TranscodeManager creates and manages on-demand audio transcoding goroutines.
// It is attached to a Stream and creates TranscodedTracks lazily when a subscriber
// requests conversion or needs to follow same-generation source codec epochs.
type TranscodeManager struct {
	mu          sync.Mutex
	tracks      map[avframe.CodecType]*TranscodedTrack // legacy audio + video tracks
	audioTracks map[avframe.CodecType]*TranscodedTrack // WebRTC audio-only tracks
	stream      *Stream
	registry    *audiocodec.Registry
	bufSize     int
}

// NewTranscodeManager creates a TranscodeManager for the given stream.
func NewTranscodeManager(stream *Stream, registry *audiocodec.Registry, bufSize int) *TranscodeManager {
	return &TranscodeManager{
		tracks:      make(map[avframe.CodecType]*TranscodedTrack),
		audioTracks: make(map[avframe.CodecType]*TranscodedTrack),
		stream:      stream,
		registry:    registry,
		bufSize:     bufSize,
	}
}

// CanTranscode reports whether this manager's configured registry can convert
// one audio codec to another in the current build.
func (tm *TranscodeManager) CanTranscode(from, to avframe.CodecType) bool {
	return tm != nil && tm.registry != nil && tm.registry.CanTranscode(from, to)
}

// TranscodeTasks returns active tasks owned by this manager. The result
// contains no mutable internal references; tasks are removed when their last
// subscriber releases them or the publisher generation is reset.
func (tm *TranscodeManager) TranscodeTasks() []TranscodeTaskSnapshot {
	if tm == nil {
		return nil
	}
	tm.mu.Lock()
	type taskEntry struct {
		codec       avframe.CodecType
		audioOnly   bool
		track       *TranscodedTrack
		subscribers int
	}
	entries := make([]taskEntry, 0, len(tm.tracks)+len(tm.audioTracks))
	for codec, track := range tm.tracks {
		entries = append(entries, taskEntry{codec: codec, track: track, subscribers: track.subCount})
	}
	for codec, track := range tm.audioTracks {
		entries = append(entries, taskEntry{codec: codec, audioOnly: true, track: track, subscribers: track.subCount})
	}
	tm.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].audioOnly != entries[j].audioOnly {
			return !entries[i].audioOnly
		}
		return entries[i].codec < entries[j].codec
	})

	result := make([]TranscodeTaskSnapshot, 0, len(entries))
	for _, entry := range entries {
		track := entry.track
		if track == nil {
			continue
		}
		task := TranscodeTaskSnapshot{
			SourceCodec: avframe.CodecType(track.sourceCodec.Load()),
			TargetCodec: entry.codec,
			AudioOnly:   entry.audioOnly,
			State:       "running",
			Subscribers: entry.subscribers,
		}
		if cause := track.terminationCause(); cause != nil {
			task.State = transcodeTaskState(cause)
			task.LastError = boundedTranscodeError(cause)
		}
		result = append(result, task)
	}
	return result
}

func transcodeTaskState(cause error) string {
	switch {
	case errors.Is(cause, errTranscodeGenerationComplete):
		return "completed"
	case errors.Is(cause, errTranscodeSubscriberReleased), errors.Is(cause, errTranscodeManagerReset):
		return "stopped"
	default:
		return "failed"
	}
}

func boundedTranscodeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 256 {
		return message[:256]
	}
	return message
}

// GetOrCreateReader returns a reader for the given target codec.
// If the publisher's codec matches, it returns the original ring buffer reader (zero overhead).
// Otherwise it creates or reuses a shared TranscodedTrack.
// The returned func must be called to release the subscription.
func (tm *TranscodeManager) GetOrCreateReader(targetCodec avframe.CodecType) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, tm.stream.RingBuffer().WriteCursor(), 0, false, false, false, nil)
}

// GetOrCreateReaderAt returns a reader whose newly-created transcode track
// starts transcoding at sourceStart. The returned reader begins at the current
// output cursor; use GetOrCreateReaderAtFromHistory when a snapshot subscriber
// must consume already-produced output as well.
func (tm *TranscodeManager) GetOrCreateReaderAt(targetCodec avframe.CodecType, sourceStart int64) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, sourceStart, 0, false, false, false, nil)
}

// GetOrCreateReaderAtFromHistory returns a combined audio/video transcode
// reader that includes retained output whose source span begins at or after the
// snapshot source cursor and whose audio is no older than the snapshot codec
// epoch. HLS, LL-HLS, and DASH use this compatibility path.
func (tm *TranscodeManager) GetOrCreateReaderAtFromHistory(targetCodec avframe.CodecType, snapshot StreamStartupSnapshot) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, snapshot.SourceCursor, snapshot.audioCodecEpoch, false, true, false, &snapshot)
}

// GetOrCreateAudioReaderAt returns a target-codec audio-only reader sourced
// from snapshot. RTMP and WHEP use it when direct video has a separate cursor.
// The reader cannot emit media attributed before snapshot's source cursor or
// audio older than snapshot's current codec epoch.
func (tm *TranscodeManager) GetOrCreateAudioReaderAt(targetCodec avframe.CodecType, snapshot StreamStartupSnapshot) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, snapshot.SourceCursor, snapshot.audioCodecEpoch, true, false, true, &snapshot)
}

// GetOrCreateAudioReaderAtFromHistory returns an audio-only target-codec
// reader at retained output from snapshot's source cursor and current audio
// epoch. Snapshot muxers use it to transform cached audio without pulling
// direct video behind the snapshot's live cursor.
func (tm *TranscodeManager) GetOrCreateAudioReaderAtFromHistory(targetCodec avframe.CodecType, snapshot StreamStartupSnapshot) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, snapshot.SourceCursor, snapshot.audioCodecEpoch, true, true, true, &snapshot)
}

func (tm *TranscodeManager) getOrCreateReaderAt(
	targetCodec avframe.CodecType,
	sourceStart int64,
	audioEpochFloor uint64,
	audioOnly bool,
	fromHistory bool,
	forceTrack bool,
	snapshot *StreamStartupSnapshot,
) (*util.RingReader[*avframe.AVFrame], func(), error) {
	// SetPublisher takes these locks in the same order while Reset removes old
	// tracks. Holding the stream sequence and lifecycle locks through track
	// lookup/creation makes the snapshot generation check and map mutation one
	// atomic acquisition.
	tm.stream.writeMu.Lock()
	defer tm.stream.writeMu.Unlock()
	tm.stream.mu.RLock()
	defer tm.stream.mu.RUnlock()
	activeGeneration := tm.stream.state == StreamStatePublishing && !isNilPublisher(tm.stream.publisher)
	if snapshot == nil && !activeGeneration {
		return nil, func() {}, fmt.Errorf("no publisher on stream")
	}
	if snapshot != nil && (tm.stream.instanceID != snapshot.StreamInstanceID ||
		tm.stream.publisherGeneration != snapshot.Generation ||
		tm.stream.generationBoundary != snapshot.generationBoundary) {
		return nil, func() {}, fmt.Errorf(
			"stale stream startup snapshot generation %d (active %d)",
			snapshot.Generation,
			tm.stream.publisherGeneration,
		)
	}
	if snapshot != nil && !activeGeneration {
		if _, ended := snapshot.GenerationEndCursor(); !ended {
			return nil, func() {}, fmt.Errorf("no publisher on stream")
		}
	}

	sourceCodec := tm.stream.mediaInfo.AudioCodec
	generationDone := (<-chan struct{})(tm.stream.generationDone)
	generationBoundary := tm.stream.generationBoundary
	if snapshot != nil {
		sourceCodec = snapshot.MediaInfo.AudioCodec
		generationDone = snapshot.GenerationDone
		generationBoundary = snapshot.generationBoundary
	}

	// Zero-overhead path: target matches source, no transcoding needed.
	if targetCodec == sourceCodec && !forceTrack {
		reader := tm.stream.ringBuffer.NewReaderAt(sourceStart)
		return reader, func() {}, nil
	}

	// Transcode path: create or reuse a shared TranscodedTrack.
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tracks := tm.tracks
	if audioOnly {
		tracks = tm.audioTracks
	}

	if track, ok := tracks[targetCodec]; ok {
		track.subCount++
		sourceFloor := int64(0)
		if snapshot != nil {
			sourceFloor = snapshot.SourceCursor
		}
		reader, stopReader := tm.newTrackReader(track, fromHistory, audioEpochFloor, sourceFloor)
		var once sync.Once
		release := func() {
			once.Do(func() {
				stopReader()
				tm.releaseTrack(targetCodec, audioOnly, track)
			})
		}
		return reader, release, nil
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	track := &TranscodedTrack{
		targetCodec:        targetCodec,
		ringBuffer:         util.NewRingBuffer[transcodeOutput](tm.bufSize),
		sourceStart:        sourceStart,
		subCount:           1,
		cancel:             cancel,
		sourceAdvance:      make(chan struct{}),
		generationDone:     generationDone,
		generationBoundary: generationBoundary,
	}
	track.sourceCodec.Store(uint32(sourceCodec))
	track.sourceEpoch.Store(audioEpochFloor)
	track.sourceCursor.Store(sourceStart)
	tracks[targetCodec] = track

	// Attach the first reader before starting the producer so a non-history
	// subscriber cannot race and miss the generated target sequence header.
	sourceFloor := int64(0)
	if snapshot != nil {
		sourceFloor = snapshot.SourceCursor
	}
	reader, stopReader := tm.newTrackReader(track, fromHistory, audioEpochFloor, sourceFloor)
	go tm.transcodeLoop(ctx, track, sourceStart, audioEpochFloor, audioOnly)
	var once sync.Once
	release := func() {
		once.Do(func() {
			stopReader()
			tm.releaseTrack(targetCodec, audioOnly, track)
		})
	}
	return reader, release, nil
}

// WaitForSourceCursor waits until a combined transcode track has consumed all
// source frames before sourceCursor. It is used by finite consumers, such as
// recording, when they need to drain generated output after stopping input.
func (tm *TranscodeManager) WaitForSourceCursor(targetCodec avframe.CodecType, sourceCursor int64, ctx context.Context) bool {
	if sourceCursor <= 0 {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		tm.mu.Lock()
		track := tm.tracks[targetCodec]
		tm.mu.Unlock()
		if track == nil {
			return false
		}
		track.sourceMu.Lock()
		current := track.sourceCursor.Load()
		advance := track.sourceAdvance
		track.sourceMu.Unlock()
		if current >= sourceCursor {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-advance:
		}
	}
}

func (track *TranscodedTrack) advanceSourceCursor(cursor int64) {
	track.sourceMu.Lock()
	defer track.sourceMu.Unlock()
	if cursor <= track.sourceCursor.Load() {
		return
	}
	track.sourceCursor.Store(cursor)
	close(track.sourceAdvance)
	track.sourceAdvance = make(chan struct{})
}

func (tm *TranscodeManager) newTrackReader(
	track *TranscodedTrack,
	fromHistory bool,
	audioEpochFloor uint64,
	sourceFloor int64,
) (*util.RingReader[*avframe.AVFrame], func()) {
	sharedReader := track.ringBuffer.NewReaderAt(track.ringBuffer.WriteCursor())
	if fromHistory {
		sharedReader = track.ringBuffer.NewReader()
	}
	return tm.bridgeTrackReader(track, sharedReader, audioEpochFloor, sourceFloor)
}

func (tm *TranscodeManager) bridgeTrackReader(
	track *TranscodedTrack,
	sharedReader *util.RingReader[transcodeOutput],
	audioEpochFloor uint64,
	sourceFloor int64,
) (*util.RingReader[*avframe.AVFrame], func()) {
	// A reader-local ring preserves the public RingReader[*AVFrame] contract
	// while the shared producer retains source attribution internally.
	filtered := util.NewRingBuffer[*avframe.AVFrame](tm.bufSize)
	reader := filtered.NewReader()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer filtered.Close()
		defer sharedReader.Close()
		var forwardedHeaderEpoch uint64
		for {
			result := sharedReader.ReadResultContext(ctx)
			if !result.OK || result.Overwritten > 0 {
				return
			}
			output := result.Value
			if output.kind == transcodeOutputSequenceHeader {
				if output.audioEpoch < audioEpochFloor {
					continue
				}
				filtered.Write(output.frame)
				forwardedHeaderEpoch = output.audioEpoch
				continue
			}
			if !output.sourceSpan.Valid() {
				return
			}
			if output.sourceSpan.Begin < sourceFloor {
				continue
			}
			frame := output.frame
			if frame == nil {
				return
			}
			if frame.MediaType.IsAudio() && output.audioEpoch < audioEpochFloor {
				continue
			}
			if frame.MediaType.IsAudio() {
				if frame.FrameType == avframe.FrameTypeSequenceHeader {
					forwardedHeaderEpoch = output.audioEpoch
				} else if track.targetCodec == avframe.CodecAAC && forwardedHeaderEpoch != output.audioEpoch {
					header, ok := track.sequenceHeaderForEpoch(output.audioEpoch)
					if !ok || header.audioEpoch < audioEpochFloor {
						continue
					}
					filtered.Write(header.frame)
					forwardedHeaderEpoch = header.audioEpoch
				}
			}
			filtered.Write(frame)
		}
	}()
	var once sync.Once
	return reader, func() {
		once.Do(func() {
			cancel()
			sharedReader.Close()
			<-done
		})
	}
}

// releaseTrack decrements the subscriber count for a track and cleans it up when empty.
func (tm *TranscodeManager) releaseTrack(targetCodec avframe.CodecType, audioOnly bool, acquired *TranscodedTrack) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tracks := tm.tracks
	if audioOnly {
		tracks = tm.audioTracks
	}
	track, ok := tracks[targetCodec]
	if !ok || track != acquired {
		return
	}
	track.subCount--
	if track.subCount <= 0 {
		track.cancel(errTranscodeSubscriberReleased)
		delete(tracks, targetCodec)
	}
}

type audioTranscodePipeline struct {
	registry    *audiocodec.Registry
	track       *TranscodedTrack
	sourceCodec avframe.CodecType
	sourceEpoch uint64
	decoder     audiocodec.Decoder
	encoder     audiocodec.Encoder
	resampler   audiocodec.Resampler
	resampled   bool
	ts          audiocodec.TsTracker
	tsInited    bool
	pcmBuf      []int16
	sourceSpan  audiocodec.SourceSpan
	pcmSpans    pcmSourceSpanQueue
	submitted   bool
	finalized   bool
}

type pcmSourceSpanSegment struct {
	samples int
	span    audiocodec.SourceSpan
}

// pcmSourceSpanQueue accounts provenance in samples per channel, matching the
// unit used by Encoder.FrameSize.
type pcmSourceSpanQueue struct {
	segments []pcmSourceSpanSegment
}

func (q *pcmSourceSpanQueue) append(samples int, span audiocodec.SourceSpan) bool {
	if samples <= 0 || !span.Valid() {
		return false
	}
	q.segments = append(q.segments, pcmSourceSpanSegment{samples: samples, span: span})
	return true
}

func (q *pcmSourceSpanQueue) consume(samples int) audiocodec.SourceSpan {
	var span audiocodec.SourceSpan
	for samples > 0 && len(q.segments) > 0 {
		segment := &q.segments[0]
		take := samples
		if take > segment.samples {
			take = segment.samples
		}
		if !span.Valid() {
			span = segment.span
		} else {
			span = span.Union(segment.span)
		}
		segment.samples -= take
		samples -= take
		if segment.samples == 0 {
			q.segments = q.segments[1:]
		}
	}
	if samples != 0 {
		return audiocodec.SourceSpan{}
	}
	return span
}

func (q *pcmSourceSpanQueue) discard(samples int) bool {
	return q.consume(samples).Valid()
}

func (tm *TranscodeManager) newAudioTranscodePipeline(
	track *TranscodedTrack,
	sourceCodec avframe.CodecType,
	sourceEpoch uint64,
) (*audioTranscodePipeline, error) {
	decoder, err := tm.registry.NewDecoder(sourceCodec)
	if err != nil {
		return nil, err
	}
	encoder, err := tm.registry.NewEncoder(track.targetCodec)
	if err != nil {
		decoder.Close()
		return nil, err
	}
	pipeline := &audioTranscodePipeline{
		registry:    tm.registry,
		track:       track,
		sourceCodec: sourceCodec,
		sourceEpoch: sourceEpoch,
		decoder:     decoder,
		encoder:     encoder,
	}
	if seqHeader := tm.stream.AudioSeqHeader(); seqHeader != nil &&
		seqHeader.Codec == sourceCodec &&
		(seqHeader.AudioCodecEpoch == 0 || seqHeader.AudioCodecEpoch == sourceEpoch) {
		decoder.SetExtradata(seqHeader.Payload)
	}
	return pipeline, nil
}

func (p *audioTranscodePipeline) close() {
	if !p.finalized {
		p.finalized = true
		p.pcmBuf = nil
		p.drainEncoder(false)
	}
	if p.resampler != nil {
		p.resampler.Close()
	}
	p.decoder.Close()
	p.encoder.Close()
}

func (p *audioTranscodePipeline) writeSequenceHeader(dts int64) {
	writeTranscodeSequenceHeader(p.registry, p.track, p.sourceEpoch, dts)
}

func writeTranscodeSequenceHeader(registry *audiocodec.Registry, track *TranscodedTrack, epoch uint64, dts int64) {
	seqHdr := registry.SequenceHeader(track.targetCodec)
	if seqHdr == nil {
		return
	}
	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, track.targetCodec,
		avframe.FrameTypeSequenceHeader, dts, dts, seqHdr,
	)
	frame.AudioCodecEpoch = epoch
	frame.AudioProvenance = avframe.FrameProvenanceTranscoded
	output := transcodeOutput{
		frame: frame, kind: transcodeOutputSequenceHeader, audioEpoch: epoch,
	}
	track.cacheSequenceHeader(output)
	track.ringBuffer.Write(output)
}

func (p *audioTranscodePipeline) writeEncoded(dts int64, payload []byte, sourceSpan audiocodec.SourceSpan) {
	if len(payload) == 0 || p.track.ringBuffer.IsClosed() {
		return
	}
	if !sourceSpan.Valid() {
		return
	}
	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, p.track.targetCodec,
		avframe.FrameTypeInterframe, dts, dts, payload,
	)
	frame.AudioCodecEpoch = p.sourceEpoch
	frame.AudioProvenance = avframe.FrameProvenanceTranscoded
	p.track.ringBuffer.Write(transcodeOutput{
		frame: frame, sourceSpan: sourceSpan, kind: transcodeOutputMedia, audioEpoch: p.sourceEpoch,
	})
}

func (p *audioTranscodePipeline) drainEncoder(publish bool) {
	frameSamples := p.encoder.FrameSize()
	if frameSamples <= 0 || !p.submitted {
		return
	}
	if drainer, ok := p.encoder.(audiocodec.AttributedDrainingEncoder); ok {
		packets, err := drainer.DrainAttributed()
		if err != nil {
			slog.Warn("transcode: encoder drain failed", "codec", p.track.targetCodec, "epoch", p.sourceEpoch, "error", err)
		}
		if !publish {
			return
		}
		for _, packet := range packets {
			p.writeEncoded(p.ts.Next(frameSamples), packet.Payload, packet.SourceSpan)
		}
		return
	}
	drainer, ok := p.encoder.(audiocodec.DrainingEncoder)
	if !ok {
		return
	}
	packets, err := drainer.Drain()
	if err != nil {
		slog.Warn("transcode: encoder drain failed", "codec", p.track.targetCodec, "epoch", p.sourceEpoch, "error", err)
	}
	if !publish {
		return
	}
	for _, packet := range packets {
		p.writeEncoded(p.ts.Next(frameSamples), packet, p.sourceSpan)
	}
}

func (p *audioTranscodePipeline) finalize() {
	if p.finalized {
		return
	}
	p.finalized = true
	if drainer, ok := p.resampler.(audiocodec.AttributedDrainingResampler); ok {
		pcm, err := drainer.DrainAttributed()
		if err == nil && pcm != nil {
			p.encodePCM(&pcm.PCMFrame, pcm.SourceSpan)
		}
	} else if drainer, ok := p.resampler.(audiocodec.DrainingResampler); ok {
		p.encodePCM(drainer.Drain(), p.sourceSpan)
	}

	frameSamples := p.encoder.FrameSize()
	channels := p.encoder.Channels()
	frameSize := frameSamples * channels
	if frameSize > 0 && len(p.pcmBuf) > 0 {
		padded := make([]int16, frameSize)
		copy(padded, p.pcmBuf)
		span := p.pcmSpans.consume((len(p.pcmBuf) + channels - 1) / channels)
		p.pcmBuf = nil
		p.encodePackets(&audiocodec.PCMFrame{
			Samples: padded, SampleRate: p.encoder.SampleRate(), Channels: channels,
		}, span, frameSamples)
	}
	p.drainEncoder(true)
}

func (p *audioTranscodePipeline) encode(frame *avframe.AVFrame, sourceSpan audiocodec.SourceSpan) {
	p.sourceSpan = p.sourceSpan.Union(sourceSpan)
	if !p.sourceSpan.Valid() {
		p.sourceSpan = sourceSpan
	}
	if !p.tsInited {
		p.ts.Init(frame.DTS, p.encoder.SampleRate())
		p.tsInited = true
	}

	pcm, err := p.decoder.Decode(frame.Payload)
	if err != nil {
		return
	}
	if !p.resampled {
		if pcm.SampleRate != p.encoder.SampleRate() || pcm.Channels != p.encoder.Channels() {
			p.resampler = p.registry.NewResampler(
				pcm.SampleRate, pcm.Channels,
				p.encoder.SampleRate(), p.encoder.Channels(),
			)
		}
		p.resampled = true
	}
	if p.resampler != nil {
		if resampler, ok := p.resampler.(audiocodec.AttributedResampler); ok {
			attributed, resampleErr := resampler.ResampleAttributed(pcm, sourceSpan)
			if resampleErr != nil || attributed == nil {
				return
			}
			p.encodePCM(&attributed.PCMFrame, attributed.SourceSpan)
			return
		}
		pcm = p.resampler.Resample(pcm)
		p.encodePCM(pcm, p.sourceSpan)
		return
	}
	p.encodePCM(pcm, sourceSpan)
}

func (p *audioTranscodePipeline) encodePCM(pcm *audiocodec.PCMFrame, sourceSpan audiocodec.SourceSpan) {
	if pcm == nil || len(pcm.Samples) == 0 {
		return
	}
	channels := p.encoder.Channels()
	if channels <= 0 || len(pcm.Samples)%channels != 0 || !sourceSpan.Valid() {
		return
	}
	if !p.sourceSpan.Valid() {
		p.sourceSpan = sourceSpan
	} else {
		p.sourceSpan = p.sourceSpan.Union(sourceSpan)
	}
	frameSize := p.encoder.FrameSize() * p.encoder.Channels()
	if frameSize == 0 {
		p.encodePackets(&audiocodec.PCMFrame{
			Samples: pcm.Samples, SampleRate: p.encoder.SampleRate(), Channels: p.encoder.Channels(),
		}, sourceSpan, len(pcm.Samples)/channels)
		return
	}

	p.pcmBuf = append(p.pcmBuf, pcm.Samples...)
	p.pcmSpans.append(len(pcm.Samples)/channels, sourceSpan)
	const maxPCMBufSamples = 48000 * 2
	if len(p.pcmBuf) > maxPCMBufSamples {
		drop := len(p.pcmBuf) - maxPCMBufSamples
		drop -= drop % channels
		p.pcmBuf = p.pcmBuf[drop:]
		p.pcmSpans.discard(drop / channels)
	}
	for len(p.pcmBuf) >= frameSize {
		span := p.pcmSpans.consume(p.encoder.FrameSize())
		p.encodePackets(&audiocodec.PCMFrame{
			Samples: p.pcmBuf[:frameSize], SampleRate: p.encoder.SampleRate(), Channels: p.encoder.Channels(),
		}, span, p.encoder.FrameSize())
		p.pcmBuf = p.pcmBuf[frameSize:]
	}
}

func (p *audioTranscodePipeline) encodePackets(pcm *audiocodec.PCMFrame, sourceSpan audiocodec.SourceSpan, samplesPerPacket int) {
	if !sourceSpan.Valid() || samplesPerPacket <= 0 {
		return
	}
	if encoder, ok := p.encoder.(audiocodec.AttributedEncoder); ok {
		packets, err := encoder.EncodeAttributed(pcm, sourceSpan)
		if err != nil {
			return
		}
		p.submitted = true
		for _, packet := range packets {
			if !packet.SourceSpan.Valid() {
				continue
			}
			p.writeEncoded(p.ts.Next(samplesPerPacket), packet.Payload, packet.SourceSpan)
		}
		return
	}
	encoded, err := p.encoder.Encode(pcm)
	if err != nil {
		return
	}
	p.submitted = true
	p.writeEncoded(p.ts.Next(samplesPerPacket), encoded, sourceSpan)
}

// transcodeLoop is the core decode-resample-encode pipeline for a single target codec.
// Each source codec epoch owns fresh decoder, encoder, resampler, timestamp,
// and PCM state while the shared track itself remains reference counted.
//
// Architecture: inline processing to minimize audio delivery jitter. Each
// source audio frame is decoded/resampled/encoded inline. Combined tracks
// retain source video passthrough for the segmenter compatibility paths;
// audio-only tracks serve subscribers and shared muxers whose video comes from
// a separate source-ring reader.
//
// This limits the maximum video delivery delay to one audio encode
// operation (~0.5ms) rather than batching N encodes which blocks video
// for N × encode_time. Chrome's jitter estimator accumulates delivery
// irregularities via EWMA, so even small periodic delays compound over
// minutes into large jitter buffer growth. The ring buffer remains
// single-producer (this goroutine only), avoiding data races.
func (tm *TranscodeManager) transcodeLoop(
	ctx context.Context,
	track *TranscodedTrack,
	sourceStart int64,
	audioEpochFloor uint64,
	audioOnly bool,
) {
	var pipeline *audioTranscodePipeline
	var sourceCodec avframe.CodecType
	var sourceEpoch uint64
	writeTranscodeSequenceHeader(tm.registry, track, audioEpochFloor, 0)
	defer func() {
		if pipeline != nil {
			pipeline.close()
		}
	}()
	defer track.ringBuffer.Close()

	reader := tm.stream.RingBuffer().NewReaderAt(sourceStart)
	// RingBuffer.Signal is a single legacy notification channel shared by all
	// readers. The transcoder must not consume it, or a live playback reader
	// can miss wakeups and emit video in bursts. RingReader.Read blocks on the
	// ring condition variable instead; close the reader when this track is
	// cancelled so the blocking read remains interruptible.
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	stopReader := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancelRead()
		case <-track.generationDone:
			cancelRead()
		case <-stopReader:
		}
	}()
	defer close(stopReader)
	defer reader.Close()

	for {
		// Inline processing: handle each frame as it arrives. Video passes
		// through immediately; audio encodes inline. This limits the maximum
		// video delivery delay to a single audio encode operation (~0.5ms)
		// rather than batching N audio encodes which would block video for
		// N × encode_time. Chrome's jitter estimator accumulates delivery
		// irregularities via EWMA, so even small periodic delays from batch
		// encoding compound over minutes into large jitter buffer growth.
		if ctx.Err() != nil {
			track.setTerminationCause(context.Cause(ctx))
			return
		}
		endCursor, generationEnded := track.generationBoundary.end()
		if generationEnded && reader.ReadCursor() >= endCursor {
			if pipeline != nil {
				pipeline.finalize()
			}
			track.advanceSourceCursor(endCursor)
			track.setTerminationCause(errTranscodeGenerationComplete)
			return
		}

		var read util.RingReadResult[*avframe.AVFrame]
		if generationEnded {
			read = reader.TryReadResult()
		} else {
			read = reader.ReadResultContext(readCtx)
		}
		if !read.OK {
			if ctx.Err() != nil {
				track.setTerminationCause(context.Cause(ctx))
				return
			}
			// A generation close cancels the blocking read. Re-evaluate its
			// now-published finite boundary and drain retained source frames.
			if _, ended := track.generationBoundary.end(); ended {
				continue
			}
			track.setTerminationCause(errTranscodeSourceClosed)
			return
		}
		if read.Overwritten > 0 {
			cause := &transcodeSourceOverwriteError{Overwritten: read.Overwritten}
			track.setTerminationCause(cause)
			slog.Warn("transcode: source ring overwritten", "codec", track.targetCodec, "overwritten", read.Overwritten)
			return
		}
		frame := read.Value
		consumedCursor := reader.ReadCursor()
		sourceSpan := audiocodec.SourceSpan{Begin: consumedCursor - 1, End: consumedCursor}
		if endCursor, ended := track.generationBoundary.end(); ended && consumedCursor > endCursor {
			// The shared source ring may already contain a replacement. Never
			// emit a frame beyond this track's publisher-generation boundary.
			if pipeline != nil {
				pipeline.finalize()
			}
			track.advanceSourceCursor(endCursor)
			track.setTerminationCause(errTranscodeGenerationComplete)
			return
		}

		if frame.MediaType.IsVideo() {
			if !audioOnly {
				// Legacy reader: pass video through without encoding.
				track.ringBuffer.Write(transcodeOutput{
					frame: frame, sourceSpan: sourceSpan, kind: transcodeOutputMedia,
				})
			}
		} else if frame.MediaType.IsAudio() {
			frameEpoch := frame.AudioCodecEpoch
			if frameEpoch == 0 {
				frameEpoch = sourceEpoch
			}
			if frameEpoch >= audioEpochFloor {
				if frame.Codec != sourceCodec || frameEpoch != sourceEpoch {
					firstAudioEpoch := sourceCodec == 0
					if pipeline != nil {
						pipeline.close()
						pipeline = nil
					}
					sourceCodec = frame.Codec
					sourceEpoch = frameEpoch
					track.sourceCodec.Store(uint32(sourceCodec))
					track.sourceEpoch.Store(sourceEpoch)
					if sourceCodec != track.targetCodec {
						var pipelineErr error
						pipeline, pipelineErr = tm.newAudioTranscodePipeline(track, sourceCodec, sourceEpoch)
						if pipelineErr != nil {
							slog.Warn("transcode: codec epoch unavailable", "from", sourceCodec, "to", track.targetCodec, "epoch", sourceEpoch, "error", pipelineErr)
						} else if !firstAudioEpoch || sourceEpoch != audioEpochFloor {
							pipeline.writeSequenceHeader(frame.DTS)
						}
					}
				}

				if frame.Codec == track.targetCodec {
					output := transcodeOutput{
						frame: frame, sourceSpan: sourceSpan, kind: transcodeOutputMedia, audioEpoch: frameEpoch,
					}
					if frame.FrameType == avframe.FrameTypeSequenceHeader {
						track.cacheSequenceHeader(transcodeOutput{
							frame: frame, kind: transcodeOutputSequenceHeader, audioEpoch: frameEpoch,
						})
					}
					track.ringBuffer.Write(output)
				} else if pipeline != nil && frame.FrameType == avframe.FrameTypeSequenceHeader {
					pipeline.decoder.SetExtradata(frame.Payload)
				} else if pipeline != nil {
					pipeline.encode(frame, sourceSpan)
				}
			}
		}
		track.advanceSourceCursor(consumedCursor)
	}
}

// Reset removes track mappings before a replacement publisher starts. Active
// generations are canceled; ended-generation producers retain ownership of
// their output rings until finite readers have consumed their terminal output.
func (tm *TranscodeManager) Reset() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for codec, track := range tm.tracks {
		if _, ended := track.generationBoundary.end(); !ended && track.cancel != nil {
			track.cancel(errTranscodeManagerReset)
		}
		delete(tm.tracks, codec)
	}
	for codec, track := range tm.audioTracks {
		if _, ended := track.generationBoundary.end(); !ended && track.cancel != nil {
			track.cancel(errTranscodeManagerReset)
		}
		delete(tm.audioTracks, codec)
	}
}

// SetTranscodeManagerForTest sets the TranscodeManager on a Stream (for integration tests).
func SetTranscodeManagerForTest(s *Stream, tm *TranscodeManager) {
	s.transcodeManager = tm
}
