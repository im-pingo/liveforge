package webrtc

import (
	"errors"
	"sync"

	"github.com/pion/webrtc/v4/pkg/media"
)

var errWHEPSendGateClosed = errors.New("whep media send gate closed")

// whepSendGate serializes session teardown with media admission. Close waits
// for an already-admitted WriteSample to finish, while new samples are
// rejected once teardown begins.
type whepSendGate struct {
	mu     sync.RWMutex
	closed bool
}

func newWHEPSendGate() *whepSendGate {
	return &whepSendGate{}
}

func (g *whepSendGate) write(fn func() error) error {
	if g == nil {
		return fn()
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return errWHEPSendGateClosed
	}
	return fn()
}

func (g *whepSendGate) close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

func writeWHEPSample(gate *whepSendGate, sender *TrackSender, sample media.Sample) error {
	if sender == nil {
		return nil
	}
	if gate == nil {
		return sender.WriteSample(sample)
	}
	return gate.write(func() error {
		return sender.WriteSample(sample)
	})
}
