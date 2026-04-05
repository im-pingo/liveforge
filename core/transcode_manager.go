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
	if tm.stream.Publisher() == nil {
		return nil, func() {}, fmt.Errorf("no publisher on stream")
	}

	sourceCodec := tm.stream.Publisher().MediaInfo().AudioCodec

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

	reader := tm.stream.RingBuffer().NewReaderAt(tm.stream.RingBuffer().WriteCursor())
	for {
		select {
		case <-ctx.Done():
			track.ringBuffer.Close()
			return
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			select {
			case <-ctx.Done():
				track.ringBuffer.Close()
				return
			case <-tm.stream.RingBuffer().Signal():
				continue
			}
		}

		// Video: passthrough unchanged
		if frame.MediaType.IsVideo() {
			track.ringBuffer.Write(frame)
			continue
		}

		// Skip source sequence headers
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		// Anchor timestamp to first audio frame
		if !tsInited {
			ts.Init(frame.DTS, encoder.SampleRate())
			tsInited = true
		}

		// Decode
		pcm, err := decoder.Decode(frame.Payload)
		if err != nil {
			continue
		}

		// Lazy resampler init using actual decoded params
		if !resamplerInited {
			if pcm.SampleRate != encoder.SampleRate() ||
				pcm.Channels != encoder.Channels() {
				resampler = audiocodec.NewFFmpegResampler(
					pcm.SampleRate, pcm.Channels,
					encoder.SampleRate(), encoder.Channels(),
				)
				defer resampler.Close()
			}
			resamplerInited = true
		}

		// Resample if needed
		if resampler != nil {
			pcm = resampler.Resample(pcm)
		}

		// Accumulate and encode in encoder-sized chunks
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
