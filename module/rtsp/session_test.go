package rtsp

import (
	"testing"
	"time"
)

func TestSessionStateTransitions(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	if s.State != StateInit {
		t.Fatalf("initial state = %d", s.State)
	}

	// DESCRIBE → Described
	if err := s.Transition(StateDescribed); err != nil {
		t.Fatalf("Transition to Described: %v", err)
	}

	// SETUP → Ready
	if err := s.Transition(StateReady); err != nil {
		t.Fatalf("Transition to Ready: %v", err)
	}

	// PLAY → Playing
	if err := s.Transition(StatePlaying); err != nil {
		t.Fatalf("Transition to Playing: %v", err)
	}

	// TEARDOWN → Closed
	if err := s.Transition(StateClosed); err != nil {
		t.Fatalf("Transition to Closed: %v", err)
	}
}

func TestSessionPublishPath(t *testing.T) {
	s := NewRTSPSession("pub-id", "live/room1")

	// ANNOUNCE → Announced
	if err := s.Transition(StateAnnounced); err != nil {
		t.Fatalf("Transition to Announced: %v", err)
	}

	// SETUP → Ready
	if err := s.Transition(StateReady); err != nil {
		t.Fatalf("Transition to Ready: %v", err)
	}

	// RECORD → Recording
	if err := s.Transition(StateRecording); err != nil {
		t.Fatalf("Transition to Recording: %v", err)
	}

	// TEARDOWN → Closed
	if err := s.Transition(StateClosed); err != nil {
		t.Fatalf("Transition to Closed: %v", err)
	}
}

func TestSessionInvalidTransition(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	// Cannot go directly to Playing from Init
	err := s.Transition(StatePlaying)
	if err == nil {
		t.Fatal("expected error for invalid transition")
	}
}

func TestSessionTimeout(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	s.Timeout = 10 * time.Millisecond
	s.Touch()
	time.Sleep(20 * time.Millisecond)
	if !s.IsExpired() {
		t.Error("session should be expired")
	}
}

func TestSessionNotExpired(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	s.Timeout = 1 * time.Second
	s.Touch()
	if s.IsExpired() {
		t.Error("session should not be expired yet")
	}
}

func TestSessionClosedNoTransition(t *testing.T) {
	s := NewRTSPSession("test-id", "live/room1")
	s.Transition(StateClosed)
	err := s.Transition(StateDescribed)
	if err == nil {
		t.Fatal("expected error for transition from Closed")
	}
}
