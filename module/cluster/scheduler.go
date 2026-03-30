package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// scheduleRequest is the JSON body sent to the schedule_url endpoint.
type scheduleRequest struct {
	Action    string `json:"action"`
	StreamKey string `json:"stream_key"`
}

// scheduleResponse is the JSON body returned by the schedule_url endpoint.
type scheduleResponse struct {
	Targets []string `json:"targets"`
}

// Scheduler resolves target lists for cluster forwarding and origin pull.
// It supports dynamic resolution via HTTP callback, static fallback lists,
// and priority-based selection between the two.
type Scheduler struct {
	url      string
	fallback []string
	priority string
	client   *http.Client
}

// NewScheduler creates a scheduler.
// If url is empty, only static fallback is used.
// If fallback is empty/nil, only schedule_url is used.
// priority is "schedule_first" (default) or "static_first".
func NewScheduler(url string, fallback []string, priority string, timeout time.Duration) *Scheduler {
	if priority == "" {
		priority = "schedule_first"
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Scheduler{
		url:      url,
		fallback: fallback,
		priority: priority,
		client:   &http.Client{Timeout: timeout},
	}
}

// Resolve returns the target list for a given stream key.
// action is "forward" or "origin".
func (s *Scheduler) Resolve(action, streamKey string) ([]string, error) {
	// No URL configured -> static only
	if s.url == "" {
		if len(s.fallback) > 0 {
			return s.fallback, nil
		}
		return nil, errors.New("no schedule_url and no static targets configured")
	}

	// No static fallback -> dynamic only
	if len(s.fallback) == 0 {
		targets, err := s.httpResolve(action, streamKey)
		if err != nil {
			return nil, fmt.Errorf("schedule_url failed: %w", err)
		}
		if len(targets) == 0 {
			return nil, errors.New("schedule_url returned empty targets")
		}
		return targets, nil
	}

	// Both configured -> use priority
	if s.priority == "static_first" {
		return s.fallback, nil
	}

	// schedule_first (default)
	targets, err := s.httpResolve(action, streamKey)
	if err != nil || len(targets) == 0 {
		if err != nil {
			slog.Warn("schedule_url failed, using static fallback",
				"module", "cluster", "url", s.url, "error", err)
		}
		return s.fallback, nil
	}
	return targets, nil
}

// httpResolve sends a POST request to the schedule_url and parses the response.
func (s *Scheduler) httpResolve(action, streamKey string) ([]string, error) {
	body, err := json.Marshal(scheduleRequest{
		Action:    action,
		StreamKey: streamKey,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := s.client.Post(s.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var result scheduleResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result.Targets, nil
}
