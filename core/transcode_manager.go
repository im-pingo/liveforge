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
// requests a codec different from the publisher's.
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
	return tm.getOrCreateReaderAt(targetCodec, tm.stream.RingBuffer().WriteCursor(), false, false)
}

// GetOrCreateReaderAt returns a reader whose newly-created transcode track
// starts transcoding at sourceStart. The returned reader begins at the current
// output cursor; use GetOrCreateReaderAtFromHistory when a snapshot subscriber
// must consume already-produced output as well.
func (tm *TranscodeManager) GetOrCreateReaderAt(targetCodec avframe.CodecType, sourceStart int64) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, sourceStart, false, false)
}

// GetOrCreateReaderAtFromHistory returns a combined audio/video transcode
// reader that includes the oldest output retained by the shared track. HTTP
// muxers use this to replay converted audio from the cached GOP before they
// continue with live frames; ordinary subscribers should use
// GetOrCreateReaderAt and start at the current output cursor.
func (tm *TranscodeManager) GetOrCreateReaderAtFromHistory(targetCodec avframe.CodecType, sourceStart int64) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, sourceStart, false, true)
}

// GetOrCreateAudioReaderAt returns a target-codec audio-only reader starting
// at sourceStart. WHEP uses it when source video is read from a separate ring
// cursor and the negotiated audio track needs a different codec.
func (tm *TranscodeManager) GetOrCreateAudioReaderAt(targetCodec avframe.CodecType, sourceStart int64) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, sourceStart, true, false)
}

// GetOrCreateAudioReaderAtFromHistory returns an audio-only target-codec
// reader at the oldest retained output while sourcing a new track at
// sourceStart. Snapshot muxers use it to transform cached audio without
// pulling direct video behind the snapshot's live cursor.
func (tm *TranscodeManager) GetOrCreateAudioReaderAtFromHistory(targetCodec avframe.CodecType, sourceStart int64) (*util.RingReader[*avframe.AVFrame], func(), error) {
	return tm.getOrCreateReaderAt(targetCodec, sourceStart, true, true)
}

func (tm *TranscodeManager) getOrCreateReaderAt(targetCodec avframe.CodecType, sourceStart int64, audioOnly, fromHistory bool) (*util.RingReader[*avframe.AVFrame], func(), error) {
	pub := tm.stream.Publisher()
	if pub == nil {
		return nil, func() {}, fmt.Errorf("no publisher on stream")
	}

	sourceCodec := pub.MediaInfo().AudioCodec

	// Zero-overhead path: target matches source, no transcoding needed.
	if targetCodec == sourceCodec {
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
		reader := track.ringBuffer.NewReaderAt(track.ringBuffer.WriteCursor())
		if fromHistory {
			reader = track.ringBuffer.NewReader()
		}
		var once sync.Once
		release := func() { once.Do(func() { tm.releaseTrack(targetCodec, audioOnly) }) }
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

	go tm.transcodeLoop(ctx, track, sourceCodec, sourceStart, audioOnly)

	reader := track.ringBuffer.NewReaderAt(track.ringBuffer.WriteCursor())
	if fromHistory {
		// A newly-created track may produce the sequence header and cached source
		// frames before the caller starts consuming it. Start at the oldest output
		// still retained by the track so snapshot subscribers do not lose the
		// cached audio/video needed for their first segment.
		reader = track.ringBuffer.NewReader()
	}
	var once sync.Once
	release := func() { once.Do(func() { tm.releaseTrack(targetCodec, audioOnly) }) }
	return reader, release, nil
}

// releaseTrack decrements the subscriber count for a track and cleans it up when empty.
func (tm *TranscodeManager) releaseTrack(targetCodec avframe.CodecType, audioOnly bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tracks := tm.tracks
	if audioOnly {
		tracks = tm.audioTracks
	}
	track, ok := tracks[targetCodec]
	if !ok {
		return
	}
	track.subCount--
	if track.subCount <= 0 {
		track.cancel()
		delete(tracks, targetCodec)
	}
}

// transcodeLoop is the core decode-resample-encode pipeline for a single target codec.
// sourceCodec is passed in to avoid a TOCTOU race on Publisher().
//
// Architecture: inline processing to minimize audio delivery jitter. Each
// source audio frame is decoded/resampled/encoded inline. Combined tracks
// retain source video passthrough for container muxers and RTMP subscribers;
// audio-only tracks are reserved for WebRTC feeds whose video comes from a
// separate source-ring reader.
//
// This limits the maximum video delivery delay to one audio encode
// operation (~0.5ms) rather than batching N encodes which blocks video
// for N × encode_time. Chrome's jitter estimator accumulates delivery
// irregularities via EWMA, so even small periodic delays compound over
// minutes into large jitter buffer growth. The ring buffer remains
// single-producer (this goroutine only), avoiding data races.
func (tm *TranscodeManager) transcodeLoop(ctx context.Context, track *TranscodedTrack, sourceCodec avframe.CodecType, sourceStart int64, audioOnly bool) {
	decoder, err := tm.registry.NewDecoder(sourceCodec)
	if err != nil {
		slog.Error("transcode: decoder unavailable", "from", sourceCodec, "error", err)
		track.ringBuffer.Close()
		return
	}
	encoder, err := tm.registry.NewEncoder(track.targetCodec)
	if err != nil {
		slog.Error("transcode: encoder unavailable", "to", track.targetCodec, "error", err)
		decoder.Close()
		track.ringBuffer.Close()
		return
	}
	defer decoder.Close()
	defer encoder.Close()
	defer track.ringBuffer.Close()

	// Set extradata for codecs that need it (e.g. AAC AudioSpecificConfig)
	if seqHeader := tm.stream.AudioSeqHeader(); seqHeader != nil {
		decoder.SetExtradata(seqHeader.Payload)
	}

	// Resampler is created lazily after the first successful decode
	var resampler audiocodec.Resampler
	resamplerInited := false
	defer func() {
		if resampler != nil {
			resampler.Close()
		}
	}()

	// Emit sequence header for target codec
	if seqHdr := tm.registry.SequenceHeader(track.targetCodec); seqHdr != nil {
		track.ringBuffer.Write(avframe.NewAVFrame(
			avframe.MediaTypeAudio, track.targetCodec,
			avframe.FrameTypeSequenceHeader, 0, 0, seqHdr,
		))
	}

	var ts audiocodec.TsTracker
	tsInited := false
	var pcmBuf []int16
	frameSize := encoder.FrameSize() * encoder.Channels()
	const maxPCMBufSamples = 48000 * 2 // cap at ~1s of 48kHz stereo

	// encodeAudio processes a single audio frame through the decode-resample-encode pipeline.
	encodeAudio := func(frame *avframe.AVFrame) {
		if !tsInited {
			ts.Init(frame.DTS, encoder.SampleRate())
			tsInited = true
		}

		pcm, decErr := decoder.Decode(frame.Payload)
		if decErr != nil {
			return
		}

		if !resamplerInited {
			if pcm.SampleRate != encoder.SampleRate() ||
				pcm.Channels != encoder.Channels() {
				resampler = tm.registry.NewResampler(
					pcm.SampleRate, pcm.Channels,
					encoder.SampleRate(), encoder.Channels(),
				)
			}
			resamplerInited = true
		}

		if resampler != nil {
			pcm = resampler.Resample(pcm)
		}

		if frameSize == 0 {
			chunk := &audiocodec.PCMFrame{
				Samples:    pcm.Samples,
				SampleRate: encoder.SampleRate(),
				Channels:   encoder.Channels(),
			}
			encoded, encErr := encoder.Encode(chunk)
			if encErr != nil {
				return
			}
			samplesPerChannel := len(pcm.Samples) / encoder.Channels()
			dts := ts.Next(samplesPerChannel)
			track.ringBuffer.Write(avframe.NewAVFrame(
				avframe.MediaTypeAudio, track.targetCodec,
				avframe.FrameTypeInterframe,
				dts, dts,
				encoded,
			))
		} else {
			pcmBuf = append(pcmBuf, pcm.Samples...)
			if len(pcmBuf) > maxPCMBufSamples {
				pcmBuf = pcmBuf[len(pcmBuf)-maxPCMBufSamples:]
			}
			for len(pcmBuf) >= frameSize {
				chunk := &audiocodec.PCMFrame{
					Samples:    pcmBuf[:frameSize],
					SampleRate: encoder.SampleRate(),
					Channels:   encoder.Channels(),
				}
				encoded, encErr := encoder.Encode(chunk)
				if encErr != nil {
					pcmBuf = pcmBuf[frameSize:]
					continue
				}
				dts := ts.Next(encoder.FrameSize())
				track.ringBuffer.Write(avframe.NewAVFrame(
					avframe.MediaTypeAudio, track.targetCodec,
					avframe.FrameTypeInterframe,
					dts, dts,
					encoded,
				))
				pcmBuf = pcmBuf[frameSize:]
			}
		}
	}

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
			} else if frame.Codec != sourceCodec {
				// A same-generation source can switch codecs. This decoder is bound
				// to the codec captured when the track started, so stop before it
				// interprets bytes from the replacement codec.
				return
			} else if frame.FrameType == avframe.FrameTypeSequenceHeader {
				// Skip source audio sequence headers.
			} else {
				encodeAudio(frame)
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
