package audiocodec

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

// SequenceHeaderFunc returns an initial sequence header frame for the
// target codec, or nil if the codec does not use sequence headers.
type SequenceHeaderFunc func() []byte
