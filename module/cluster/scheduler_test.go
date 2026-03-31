package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSchedulerStaticOnly(t *testing.T) {
	// No URL, only static fallback
	s := NewScheduler("", []string{"rtmp://static/live"}, "schedule_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://static/live" {
		t.Errorf("targets = %v, want [rtmp://static/live]", targets)
	}
}

func TestSchedulerDynamicOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req scheduleRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Action != "forward" || req.StreamKey != "live/test" {
			t.Errorf("unexpected request: %+v", req)
		}
		json.NewEncoder(w).Encode(scheduleResponse{
			Targets: []string{"rtmp://dynamic1/live/test", "rtmp://dynamic2/live/test"},
		})
	}))
	defer srv.Close()

	s := NewScheduler(srv.URL, nil, "schedule_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 2 {
		t.Errorf("targets count = %d, want 2", len(targets))
	}
}

func TestSchedulerScheduleFirstFallback(t *testing.T) {
	// Dynamic returns error -> fallback to static
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewScheduler(srv.URL, []string{"rtmp://fallback/live"}, "schedule_first", 3*time.Second)
	targets, err := s.Resolve("origin", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://fallback/live" {
		t.Errorf("targets = %v, want [rtmp://fallback/live]", targets)
	}
}

func TestSchedulerStaticFirstFallback(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		json.NewEncoder(w).Encode(scheduleResponse{
			Targets: []string{"rtmp://dynamic/live/test"},
		})
	}))
	defer srv.Close()

	// Static has entries -> should use static, never call HTTP
	s := NewScheduler(srv.URL, []string{"rtmp://static/live"}, "static_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://static/live" {
		t.Errorf("targets = %v, want [rtmp://static/live]", targets)
	}
	if called {
		t.Error("HTTP should not be called when static_first has entries")
	}
}

func TestSchedulerStaticFirstEmptyFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(scheduleResponse{
			Targets: []string{"rtmp://dynamic/live/test"},
		})
	}))
	defer srv.Close()

	// Static is empty -> should fall through to HTTP
	s := NewScheduler(srv.URL, nil, "static_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://dynamic/live/test" {
		t.Errorf("targets = %v, want [rtmp://dynamic/live/test]", targets)
	}
}

func TestSchedulerTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // longer than timeout
	}))
	defer srv.Close()

	s := NewScheduler(srv.URL, []string{"rtmp://fallback/live"}, "schedule_first", 100*time.Millisecond)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://fallback/live" {
		t.Errorf("targets = %v, want [rtmp://fallback/live]", targets)
	}
}

func TestSchedulerEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(scheduleResponse{Targets: []string{}})
	}))
	defer srv.Close()

	// Empty targets + no fallback -> error
	s := NewScheduler(srv.URL, nil, "schedule_first", 3*time.Second)
	_, err := s.Resolve("forward", "live/test")
	if err == nil {
		t.Error("expected error for empty targets with no fallback")
	}
}

func TestSchedulerBothEmpty(t *testing.T) {
	s := NewScheduler("", nil, "schedule_first", 3*time.Second)
	_, err := s.Resolve("forward", "live/test")
	if err == nil {
		t.Error("expected error when both url and fallback are empty")
	}
}
