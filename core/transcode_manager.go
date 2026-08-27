package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// TranscodedTrack holds a ring buffer for a specific target codec.
type TranscodedTrack struct {
	targetCodec avframe.CodecType
	ringBuffer  *util.RingBuffer[*avframe.AVFrame]
	sourceStart int64
	subCount    int
	cancel      context.CancelFunc
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

// GetOrCreateReader returns a reader for the given target codec.
// If the publisher's codec matches, it returns the original ring buffer reader (zero overhead).
// Otherwise it creates or reuses a shared TranscodedTrack.
// The returned func must be called to release the subscription.
func (tm *TranscodeManager) GetOrCreateReader(targetCodec avframe.CodecType) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, tm.stream.RingBuffer().WriteCursor(), 0, false, false, false)
}

// GetOrCreateReaderAt returns a reader whose newly-created transcode track
// starts transcoding at sourceStart. The returned reader begins at the current
// output cursor; use GetOrCreateReaderAtFromHistory when a snapshot subscriber
// must consume already-produced output as well.
func (tm *TranscodeManager) GetOrCreateReaderAt(targetCodec avframe.CodecType, sourceStart int64) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, sourceStart, 0, false, false, false)
}

// GetOrCreateReaderAtFromHistory returns a combined audio/video transcode
// reader that includes retained source video and target audio no older than
// snapshot's audio codec epoch. HLS, LL-HLS, and DASH use this compatibility
// path to transform the captured GOP without replaying stale audio epochs.
func (tm *TranscodeManager) GetOrCreateReaderAtFromHistory(targetCodec avframe.CodecType, snapshot StreamStartupSnapshot) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, snapshot.SourceCursor, snapshot.audioCodecEpoch, false, true, false)
}

// GetOrCreateAudioReaderAt returns a target-codec audio-only reader sourced
// from snapshot. RTMP and WHEP use it when direct video has a separate cursor.
// The reader cannot emit audio older than snapshot's current codec epoch.
func (tm *TranscodeManager) GetOrCreateAudioReaderAt(targetCodec avframe.CodecType, snapshot StreamStartupSnapshot) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, snapshot.SourceCursor, snapshot.audioCodecEpoch, true, false, true)
}

// GetOrCreateAudioReaderAtFromHistory returns an audio-only target-codec
// reader at retained output from snapshot's current audio epoch. Snapshot
// muxers use it to transform cached audio without pulling direct video behind
// the snapshot's live cursor.
func (tm *TranscodeManager) GetOrCreateAudioReaderAtFromHistory(targetCodec avframe.CodecType, snapshot StreamStartupSnapshot) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, snapshot.SourceCursor, snapshot.audioCodecEpoch, true, true, true)
}

func (tm *TranscodeManager) getOrCreateReaderAt(
	targetCodec avframe.CodecType,
	sourceStart int64,
	audioEpochFloor uint64,
	audioOnly bool,
	fromHistory bool,
	forceTrack bool,
) (*util.RingReader[*avframe.AVFrame], func(), error) {
	pub := tm.stream.Publisher()
	if pub == nil {
		return nil, func() {}, fmt.Errorf("no publisher on stream")
	}

	sourceCodec, _ := tm.stream.audioCodecState()

	// Zero-overhead path: target matches source, no transcoding needed.
	if targetCodec == sourceCodec && !forceTrack {
		rb := tm.stream.RingBuffer()
		reader := rb.NewReaderAt(sourceStart)
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
		reader, stopReader := tm.newTrackReader(track, fromHistory, audioEpochFloor)
		var once sync.Once
		release := func() {
			once.Do(func() {
				stopReader()
				tm.releaseTrack(targetCodec, audioOnly, track)
			})
		}
		return reader, release, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	track := &TranscodedTrack{
		targetCodec: targetCodec,
		ringBuffer:  util.NewRingBuffer[*avframe.AVFrame](tm.bufSize),
		sourceStart: sourceStart,
		subCount:    1,
		cancel:      cancel,
	}
	tracks[targetCodec] = track

	// Attach the first reader before starting the producer so a non-history
	// subscriber cannot race and miss the generated target sequence header.
	reader, stopReader := tm.newTrackReader(track, fromHistory, audioEpochFloor)
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

func (tm *TranscodeManager) newTrackReader(
	track *TranscodedTrack,
	fromHistory bool,
	audioEpochFloor uint64,
) (*util.RingReader[*avframe.AVFrame], func()) {
	sharedReader := track.ringBuffer.NewReaderAt(track.ringBuffer.WriteCursor())
	if fromHistory {
		sharedReader = track.ringBuffer.NewReader()
	}
	if audioEpochFloor == 0 {
		return sharedReader, func() { sharedReader.Close() }
	}

	// A reader-local ring preserves shared producer ownership while enforcing
	// the snapshot's audio epoch floor. Source video remains unfiltered for the
	// legacy combined-track consumers.
	filtered := util.NewRingBuffer[*avframe.AVFrame](tm.bufSize)
	reader := filtered.NewReader()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer filtered.Close()
		defer sharedReader.Close()
		for {
			frame, ok := sharedReader.ReadContext(ctx)
			if !ok {
				return
			}
			if frame != nil && frame.MediaType.IsAudio() && frame.AudioCodecEpoch < audioEpochFloor {
				continue
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
		track.cancel()
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
	track.ringBuffer.Write(frame)
}

func (p *audioTranscodePipeline) writeEncoded(dts int64, payload []byte) {
	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, p.track.targetCodec,
		avframe.FrameTypeInterframe, dts, dts, payload,
	)
	frame.AudioCodecEpoch = p.sourceEpoch
	frame.AudioProvenance = avframe.FrameProvenanceTranscoded
	p.track.ringBuffer.Write(frame)
}

func (p *audioTranscodePipeline) encode(frame *avframe.AVFrame) {
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
		pcm = p.resampler.Resample(pcm)
	}

	frameSize := p.encoder.FrameSize() * p.encoder.Channels()
	if frameSize == 0 {
		encoded, encErr := p.encoder.Encode(&audiocodec.PCMFrame{
			Samples: pcm.Samples, SampleRate: p.encoder.SampleRate(), Channels: p.encoder.Channels(),
		})
		if encErr != nil {
			return
		}
		samplesPerChannel := len(pcm.Samples) / p.encoder.Channels()
		p.writeEncoded(p.ts.Next(samplesPerChannel), encoded)
		return
	}

	p.pcmBuf = append(p.pcmBuf, pcm.Samples...)
	const maxPCMBufSamples = 48000 * 2
	if len(p.pcmBuf) > maxPCMBufSamples {
		p.pcmBuf = p.pcmBuf[len(p.pcmBuf)-maxPCMBufSamples:]
	}
	for len(p.pcmBuf) >= frameSize {
		encoded, encErr := p.encoder.Encode(&audiocodec.PCMFrame{
			Samples: p.pcmBuf[:frameSize], SampleRate: p.encoder.SampleRate(), Channels: p.encoder.Channels(),
		})
		p.pcmBuf = p.pcmBuf[frameSize:]
		if encErr != nil {
			continue
		}
		p.writeEncoded(p.ts.Next(p.encoder.FrameSize()), encoded)
	}
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
	stopReader := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			reader.Close()
		case <-stopReader:
		}
	}()
	defer close(stopReader)

	for {
		// Inline processing: handle each frame as it arrives. Video passes
		// through immediately; audio encodes inline. This limits the maximum
		// video delivery delay to a single audio encode operation (~0.5ms)
		// rather than batching N audio encodes which would block video for
		// N × encode_time. Chrome's jitter estimator accumulates delivery
		// irregularities via EWMA, so even small periodic delays from batch
		// encoding compound over minutes into large jitter buffer growth.
		frame, ok := reader.Read()
		if !ok {
			return
		}
		for {
			if frame.MediaType.IsVideo() {
				if !audioOnly {
					// Legacy reader: pass video through without encoding.
					track.ringBuffer.Write(frame)
				}
			} else if frame.MediaType.IsAudio() {
				frameEpoch := frame.AudioCodecEpoch
				if frameEpoch == 0 {
					frameEpoch = sourceEpoch
				}
				if frameEpoch < audioEpochFloor {
					frame, ok = reader.TryRead()
					if !ok {
						break
					}
					continue
				}
				if frame.Codec != sourceCodec || frameEpoch != sourceEpoch {
					firstAudioEpoch := sourceCodec == 0
					if pipeline != nil {
						pipeline.close()
						pipeline = nil
					}
					sourceCodec = frame.Codec
					sourceEpoch = frameEpoch
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
					track.ringBuffer.Write(frame)
				} else if pipeline != nil && frame.FrameType == avframe.FrameTypeSequenceHeader {
					pipeline.decoder.SetExtradata(frame.Payload)
				} else if pipeline != nil {
					pipeline.encode(frame)
				}
			}

			frame, ok = reader.TryRead()
			if !ok {
				break
			}
		}
	}
}

// Reset cancels all active transcode goroutines and removes all tracks.
// Called when a new publisher replaces the old one.
func (tm *TranscodeManager) Reset() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for codec, track := range tm.tracks {
		if track.cancel != nil {
			track.cancel()
		}
		track.ringBuffer.Close()
		delete(tm.tracks, codec)
	}
	for codec, track := range tm.audioTracks {
		if track.cancel != nil {
			track.cancel()
		}
		track.ringBuffer.Close()
		delete(tm.audioTracks, codec)
	}
}

// SetTranscodeManagerForTest sets the TranscodeManager on a Stream (for integration tests).
func SetTranscodeManagerForTest(s *Stream, tm *TranscodeManager) {
	s.transcodeManager = tm
}
