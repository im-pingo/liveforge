package audiocodec

import (
	"fmt"
	"sync"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// DecoderFactory creates a new Decoder instance.
type DecoderFactory func() Decoder

// EncoderFactory creates a new Encoder instance.
type EncoderFactory func() Encoder

// SeqHeaderFactory creates a SequenceHeaderFunc for a codec.
type SeqHeaderFactory func() SequenceHeaderFunc

// Registry manages available audio codecs.
type Registry struct {
	mu       sync.RWMutex
	decoders map[avframe.CodecType]DecoderFactory
	encoders map[avframe.CodecType]EncoderFactory
	seqHdrs  map[avframe.CodecType]SeqHeaderFactory
}

var (
	globalRegistry     *Registry
	globalRegistryOnce sync.Once
)

// Global returns the process-wide codec registry.
func Global() *Registry {
	globalRegistryOnce.Do(func() {
		globalRegistry = &Registry{
			decoders: make(map[avframe.CodecType]DecoderFactory),
			encoders: make(map[avframe.CodecType]EncoderFactory),
			seqHdrs:  make(map[avframe.CodecType]SeqHeaderFactory),
		}
	})
	return globalRegistry
}

func (r *Registry) RegisterDecoder(codec avframe.CodecType, f DecoderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decoders[codec] = f
}

func (r *Registry) RegisterEncoder(codec avframe.CodecType, f EncoderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.encoders[codec] = f
}

func (r *Registry) RegisterSequenceHeader(codec avframe.CodecType, fn SeqHeaderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seqHdrs[codec] = fn
}

func (r *Registry) NewDecoder(codec avframe.CodecType) (Decoder, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.decoders[codec]
	if !ok {
		return nil, fmt.Errorf("audiocodec: no decoder registered for %s", codec)
	}
	return f(), nil
}

func (r *Registry) NewEncoder(codec avframe.CodecType) (Encoder, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.encoders[codec]
	if !ok {
		return nil, fmt.Errorf("audiocodec: no encoder registered for %s", codec)
	}
	return f(), nil
}

func (r *Registry) CanTranscode(from, to avframe.CodecType) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, hasDec := r.decoders[from]
	_, hasEnc := r.encoders[to]
	return hasDec && hasEnc
}

func (r *Registry) SequenceHeader(codec avframe.CodecType) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.seqHdrs[codec]
	if !ok {
		return nil
	}
	shf := fn()
	if shf == nil {
		return nil
	}
	return shf()
}
