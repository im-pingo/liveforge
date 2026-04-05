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
	subCount    int
	cancel      context.CancelFunc
}

// TranscodeManager creates and manages on-demand audio transcoding goroutines.
// It is attached to a Stream and creates TranscodedTracks lazily when a subscriber
// requests a codec different from the publisher's.
type TranscodeManager struct {
	mu       sync.Mutex
	tracks   map[avframe.CodecType]*TranscodedTrack
	stream   *Stream
	registry *audiocodec.Registry
	bufSize  int
}

// NewTranscodeManager creates a TranscodeManager for the given stream.
func NewTranscodeManager(stream *Stream, registry *audiocodec.Registry, bufSize int) *TranscodeManager {
	return &TranscodeManager{
		tracks:   make(map[avframe.CodecType]*TranscodedTrack),
		stream:   stream,
		registry: registry,
		bufSize:  bufSize,
	}
}

// GetOrCreateReader returns a reader for the given target codec.
// If the publisher's codec matches, it returns the original ring buffer reader (zero overhead).
// Otherwise it creates or reuses a shared TranscodedTrack.
// The returned func must be called to release the subscription.
func (tm *TranscodeManager) GetOrCreateReader(targetCodec avframe.CodecType) (*util.RingReader[*avframe.AVFrame], func(), error) {
	pub := tm.stream.Publisher()
	if pub == nil {
		return nil, func() {}, fmt.Errorf("no publisher on stream")
	}

	sourceCodec := pub.MediaInfo().AudioCodec

	// Zero-overhead path: target matches source, no transcoding needed.
	if targetCodec == sourceCodec {
		rb := tm.stream.RingBuffer()
		reader := rb.NewReaderAt(rb.WriteCursor())
		return reader, func() {}, nil
	}

	// Transcode path: create or reuse a shared TranscodedTrack.
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if track, ok := tm.tracks[targetCodec]; ok {
		track.subCount++
		reader := track.ringBuffer.NewReaderAt(track.ringBuffer.WriteCursor())
		var once sync.Once
		release := func() { once.Do(func() { tm.releaseTrack(targetCodec) }) }
		return reader, release, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	track := &TranscodedTrack{
		targetCodec: targetCodec,
		ringBuffer:  util.NewRingBuffer[*avframe.AVFrame](tm.bufSize),
		subCount:    1,
		cancel:      cancel,
	}
	tm.tracks[targetCodec] = track

	go tm.transcodeLoop(ctx, track, sourceCodec)

	reader := track.ringBuffer.NewReaderAt(track.ringBuffer.WriteCursor())
	var once sync.Once
	release := func() { once.Do(func() { tm.releaseTrack(targetCodec) }) }
	return reader, release, nil
}

// releaseTrack decrements the subscriber count for a track and cleans it up when empty.
func (tm *TranscodeManager) releaseTrack(targetCodec avframe.CodecType) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	track, ok := tm.tracks[targetCodec]
	if !ok {
		return
	}
	track.subCount--
	if track.subCount <= 0 {
		track.cancel()
		delete(tm.tracks, targetCodec)
	}
}

// transcodeLoop is the core decode-resample-encode pipeline for a single target codec.
// sourceCodec is passed in to avoid a TOCTOU race on Publisher().
//
// Architecture: inline processing to minimize video delivery jitter.
// Each frame is handled as it arrives from the source ring buffer:
//   - Video frames: pass through to transcoded buffer immediately.
//   - Audio frames: decode/resample/encode inline (single frame at a time).
//
// This limits the maximum video delivery delay to one audio encode
// operation (~0.5ms) rather than batching N encodes which blocks video
// for N × encode_time. Chrome's jitter estimator accumulates delivery
// irregularities via EWMA, so even small periodic delays compound over
// minutes into large jitter buffer growth. The ring buffer remains
// single-producer (this goroutine only), avoiding data races.
func (tm *TranscodeManager) transcodeLoop(ctx context.Context, track *TranscodedTrack, sourceCodec avframe.CodecType) {
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

	// Set extradata for codecs that need it (e.g. AAC AudioSpecificConfig)
	if seqHeader := tm.stream.AudioSeqHeader(); seqHeader != nil {
		decoder.SetExtradata(seqHeader.Payload)
	}

	// Resampler is created lazily after the first successful decode
	var resampler *audiocodec.FFmpegResampler
	resamplerInited := false

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
				resampler = audiocodec.NewFFmpegResampler(
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

	reader := tm.stream.RingBuffer().NewReaderAt(tm.stream.RingBuffer().WriteCursor())

	for {
		select {
		case <-ctx.Done():
			if resampler != nil {
				resampler.Close()
			}
			track.ringBuffer.Close()
			return
		default:
		}

		// Inline processing: handle each frame as it arrives. Video passes
		// through immediately; audio encodes inline. This limits the maximum
		// video delivery delay to a single audio encode operation (~0.5ms)
		// rather than batching N audio encodes which would block video for
		// N × encode_time. Chrome's jitter estimator accumulates delivery
		// irregularities via EWMA, so even small periodic delays from batch
		// encoding compound over minutes into large jitter buffer growth.
		drained := false
		for {
			frame, ok := reader.TryRead()
			if !ok {
				break
			}
			drained = true

			if frame.MediaType.IsVideo() {
				// Video passthrough: zero encoding delay.
				track.ringBuffer.Write(frame)
			} else if frame.FrameType == avframe.FrameTypeSequenceHeader {
				// Skip source audio sequence headers.
			} else {
				encodeAudio(frame)
			}
		}

		if drained {
			// More frames may have arrived during encoding; loop back.
			continue
		}

		// No frames available — wait for signal.
		select {
		case <-ctx.Done():
			if resampler != nil {
				resampler.Close()
			}
			track.ringBuffer.Close()
			return
		case <-tm.stream.RingBuffer().Signal():
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
}

// SetTranscodeManagerForTest sets the TranscodeManager on a Stream (for integration tests).
func SetTranscodeManagerForTest(s *Stream, tm *TranscodeManager) {
	s.transcodeManager = tm
}
