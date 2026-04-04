package gb28181

import (
	"testing"
)

func TestMediaSessionStateTransitions(t *testing.T) {
	tests := []struct {
		name        string
		transitions []SessionState
		wantFinal   SessionState
	}{
		{
			name:        "idle to inviting to streaming to closed",
			transitions: []SessionState{SessionStateInviting, SessionStateStreaming, SessionStateClosed},
			wantFinal:   SessionStateClosed,
		},
		{
			name:        "idle to streaming directly",
			transitions: []SessionState{SessionStateStreaming},
			wantFinal:   SessionStateStreaming,
		},
		{
			name:        "idle to closed directly",
			transitions: []SessionState{SessionStateClosed},
			wantFinal:   SessionStateClosed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &MediaSession{State: SessionStateIdle}

			if s.GetState() != SessionStateIdle {
				t.Fatalf("initial state = %v, want idle", s.GetState())
			}

			for _, state := range tt.transitions {
				s.SetState(state)
			}

			if got := s.GetState(); got != tt.wantFinal {
				t.Errorf("final state = %v, want %v", got, tt.wantFinal)
			}
		})
	}
}

func TestMediaSessionClose(t *testing.T) {
	closed := false
	pub := NewPublisher("test", nil)
	// Verify publisher Close is called via session Close
	s := &MediaSession{
		State:     SessionStateStreaming,
		Publisher: pub,
	}

	s.Close()

	if s.State != SessionStateClosed {
		t.Errorf("state = %v, want closed", s.State)
	}
	// Publisher should be marked as closed
	pub.mu.Lock()
	closed = pub.closed
	pub.mu.Unlock()
	if !closed {
		t.Error("publisher not closed after session close")
	}
}

func TestMediaSessionCloseIdempotent(t *testing.T) {
	s := &MediaSession{State: SessionStateStreaming}
	s.Close()
	s.Close() // should not panic
	if s.State != SessionStateClosed {
		t.Errorf("state = %v, want closed", s.State)
	}
}

func TestSessionManagerAddGetRemove(t *testing.T) {
	m := NewSessionManager()

	s := &MediaSession{ID: "call-001", DeviceID: "d1", ChannelID: "ch1", StreamKey: "gb28181/ch1"}
	m.Add(s)

	got := m.Get("call-001")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.StreamKey != "gb28181/ch1" {
		t.Errorf("StreamKey = %q", got.StreamKey)
	}

	if m.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent")
	}

	m.Remove("call-001")
	if m.Get("call-001") != nil {
		t.Error("session not removed")
	}
}

func TestSessionManagerGetByStreamKey(t *testing.T) {
	m := NewSessionManager()
	m.Add(&MediaSession{ID: "c1", StreamKey: "gb28181/ch001"})
	m.Add(&MediaSession{ID: "c2", StreamKey: "gb28181/ch002"})

	got := m.GetByStreamKey("gb28181/ch001")
	if got == nil || got.ID != "c1" {
		t.Errorf("GetByStreamKey returned %v", got)
	}

	if m.GetByStreamKey("missing") != nil {
		t.Error("expected nil for missing key")
	}
}

func TestSessionManagerGetByChannel(t *testing.T) {
	m := NewSessionManager()
	m.Add(&MediaSession{ID: "c1", ChannelID: "ch1", State: SessionStateStreaming})
	m.Add(&MediaSession{ID: "c2", ChannelID: "ch1", State: SessionStateClosed})
	m.Add(&MediaSession{ID: "c3", ChannelID: "ch2", State: SessionStateStreaming})

	sessions := m.GetByChannel("ch1")
	if len(sessions) != 1 {
		t.Errorf("GetByChannel(ch1) len = %d, want 1 (excludes closed)", len(sessions))
	}
	if sessions[0].ID != "c1" {
		t.Errorf("ID = %q, want c1", sessions[0].ID)
	}
}

func TestSessionManagerAll(t *testing.T) {
	m := NewSessionManager()
	m.Add(&MediaSession{ID: "c1"})
	m.Add(&MediaSession{ID: "c2"})
	m.Add(&MediaSession{ID: "c3"})

	if got := len(m.All()); got != 3 {
		t.Errorf("All() len = %d, want 3", got)
	}
}

func TestSessionManagerCloseByDevice(t *testing.T) {
	m := NewSessionManager()
	m.Add(&MediaSession{ID: "c1", DeviceID: "d1", State: SessionStateStreaming})
	m.Add(&MediaSession{ID: "c2", DeviceID: "d1", State: SessionStateStreaming})
	m.Add(&MediaSession{ID: "c3", DeviceID: "d2", State: SessionStateStreaming})

	m.CloseByDevice("d1")

	if m.Get("c1") != nil {
		t.Error("c1 should be removed")
	}
	if m.Get("c2") != nil {
		t.Error("c2 should be removed")
	}
	if m.Get("c3") == nil {
		t.Error("c3 (different device) should remain")
	}
}

func TestSessionStateString(t *testing.T) {
	tests := []struct {
		state SessionState
		want  string
	}{
		{SessionStateIdle, "idle"},
		{SessionStateInviting, "inviting"},
		{SessionStateStreaming, "streaming"},
		{SessionStateClosed, "closed"},
		{SessionState(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("SessionState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}
