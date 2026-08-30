package webrtc

import (
	"errors"
	"testing"
	"time"
)

func TestWHEPSendGateCloseWaitsForActiveWrite(t *testing.T) {
	gate := newWHEPSendGate()
	entered := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- gate.write(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("send gate write did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		gate.close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("send gate closed while a sample write was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("active gated write failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active gated write did not finish")
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("send gate did not close after the active write finished")
	}
	if err := gate.write(func() error { return nil }); !errors.Is(err, errWHEPSendGateClosed) {
		t.Fatalf("write after gate close = %v, want %v", err, errWHEPSendGateClosed)
	}
}
