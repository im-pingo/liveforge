package audiocodec

import "errors"

// ErrInvalidSourceSpan reports an invalid half-open source cursor interval.
var ErrInvalidSourceSpan = errors.New("invalid source span")

// SourceSpan is a half-open interval in source-ring cursor space.
// The zero value is invalid.
type SourceSpan struct {
	Begin int64
	End   int64
}

// Valid reports whether the span contains source media.
func (s SourceSpan) Valid() bool {
	return s.Begin < s.End
}

// Union returns the smallest span covering both inputs. If either input is
// invalid, the result is invalid rather than attributing unrelated media.
func (s SourceSpan) Union(other SourceSpan) SourceSpan {
	if !s.Valid() || !other.Valid() {
		return SourceSpan{}
	}
	if other.Begin < s.Begin {
		s.Begin = other.Begin
	}
	if other.End > s.End {
		s.End = other.End
	}
	return s
}

// PCMFrame is the universal exchange format between all audio codecs.
type PCMFrame struct {
	Samples    []int16 // interleaved samples (L,R,L,R... or mono)
	SampleRate int     // 8000, 16000, 44100, 48000
	Channels   int     // 1 or 2
}

// Decoder decodes compressed audio into PCM.
// Instances are NOT safe for concurrent use.
type Decoder interface {
	SetExtradata(data []byte)
	Decode(payload []byte) (*PCMFrame, error)
	SampleRate() int
	Channels() int
	Close()
}

// Encoder encodes PCM into compressed audio.
// Instances are NOT safe for concurrent use.
type Encoder interface {
	Encode(pcm *PCMFrame) ([]byte, error)
	SampleRate() int
	Channels() int
	FrameSize() int
	Close()
}

// AttributedPacket adds by-value source provenance to a compressed payload.
// Payload retains the ownership and backing storage of the encoder result.
type AttributedPacket struct {
	Payload    []byte
	SourceSpan SourceSpan
}

// AttributedEncoder is an opt-in source provenance extension for Encoder.
// One input may produce zero, one, or multiple packets.
type AttributedEncoder interface {
	Encoder
	EncodeAttributed(pcm *PCMFrame, sourceSpan SourceSpan) ([]AttributedPacket, error)
}

// DrainingEncoder exposes delayed packets held by an encoder at a finite
// stream boundary. Drain is idempotent and returns each delayed packet once.
type DrainingEncoder interface {
	Encoder
	Drain() ([][]byte, error)
}

// AttributedDrainingEncoder exposes source provenance on delayed packets.
type AttributedDrainingEncoder interface {
	AttributedEncoder
	DrainingEncoder
	DrainAttributed() ([]AttributedPacket, error)
}

// SequenceHeaderFunc returns an initial sequence header frame for the
// target codec, or nil if the codec does not use sequence headers.
type SequenceHeaderFunc func() []byte

// Resampler converts PCM between different sample-rates and channel counts.
// Instances are NOT safe for concurrent use.
type Resampler interface {
	Resample(pcm *PCMFrame) *PCMFrame
	Close()
}

// AttributedPCMFrame adds by-value source provenance to resampled PCM. An
// empty frame has an invalid SourceSpan.
type AttributedPCMFrame struct {
	PCMFrame
	SourceSpan SourceSpan
}

// AttributedResampler is an opt-in source provenance extension for Resampler.
type AttributedResampler interface {
	Resampler
	ResampleAttributed(pcm *PCMFrame, sourceSpan SourceSpan) (*AttributedPCMFrame, error)
}

// DrainingResampler exposes samples retained by a streaming resampler at a
// finite input boundary. Drain is idempotent and returns the terminal samples
// exactly once.
type DrainingResampler interface {
	Resampler
	Drain() *PCMFrame
}

// AttributedDrainingResampler exposes source provenance on terminal samples.
type AttributedDrainingResampler interface {
	AttributedResampler
	DrainingResampler
	DrainAttributed() (*AttributedPCMFrame, error)
}

type sourceSpanSegment struct {
	samples int64
	span    SourceSpan
}

// sourceSpanQueue tracks input samples per channel that may still contribute
// to future output.
type sourceSpanQueue struct {
	segments []sourceSpanSegment
	samples  int64
}

func (q *sourceSpanQueue) append(samples int64, span SourceSpan) bool {
	if samples <= 0 || !span.Valid() {
		return false
	}
	q.segments = append(q.segments, sourceSpanSegment{samples: samples, span: span})
	q.samples += samples
	return true
}

func (q *sourceSpanQueue) span() SourceSpan {
	if len(q.segments) == 0 {
		return SourceSpan{}
	}
	span := q.segments[0].span
	for _, segment := range q.segments[1:] {
		span = span.Union(segment.span)
		if !span.Valid() {
			return SourceSpan{}
		}
	}
	return span
}

func (q *sourceSpanQueue) retainTail(samples int64) {
	if samples <= 0 {
		q.clear()
		return
	}
	if samples >= q.samples {
		return
	}

	drop := q.samples - samples
	droppedSegments := 0
	for droppedSegments < len(q.segments) && drop >= q.segments[droppedSegments].samples {
		drop -= q.segments[droppedSegments].samples
		droppedSegments++
	}
	if droppedSegments > 0 {
		copy(q.segments, q.segments[droppedSegments:])
		q.segments = q.segments[:len(q.segments)-droppedSegments]
	}
	if drop > 0 {
		q.segments[0].samples -= drop
	}
	q.samples = samples
}

func (q *sourceSpanQueue) clear() {
	q.segments = nil
	q.samples = 0
}

// ceilRetainedInputSamples converts an exact resampler delay to the number of
// whole input samples whose provenance must be retained. exactBase must be a
// common multiple of the input and output sample rates.
func ceilRetainedInputSamples(delay, exactBase int64, inputRate int) int64 {
	if delay <= 0 || exactBase <= 0 || inputRate <= 0 {
		return 0
	}
	ticksPerInputSample := exactBase / int64(inputRate)
	if ticksPerInputSample <= 0 {
		return 0
	}
	return 1 + (delay-1)/ticksPerInputSample
}

func attributePackets(payloads [][]byte, span SourceSpan) ([]AttributedPacket, error) {
	if len(payloads) == 0 {
		return nil, nil
	}
	if !span.Valid() {
		return nil, ErrInvalidSourceSpan
	}
	packets := make([]AttributedPacket, len(payloads))
	for i, payload := range payloads {
		packets[i] = AttributedPacket{Payload: payload, SourceSpan: span}
	}
	return packets, nil
}

func attributePCMFrame(frame *PCMFrame, span SourceSpan) (*AttributedPCMFrame, error) {
	attributed := &AttributedPCMFrame{}
	if frame != nil {
		attributed.PCMFrame = *frame
	}
	if len(attributed.Samples) == 0 {
		return attributed, nil
	}
	if !span.Valid() {
		return nil, ErrInvalidSourceSpan
	}
	attributed.SourceSpan = span
	return attributed, nil
}
