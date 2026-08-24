package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPSourceOptions controls an HTTP configuration endpoint.
type HTTPSourceOptions struct {
	URL      string
	Token    string
	Client   *http.Client
	MaxBytes int64
}

// HTTPSource loads a complete document over HTTP with conditional requests.
type HTTPSource struct {
	url      string
	token    string
	client   *http.Client
	maxBytes int64
	etag     string
	modified string
}

func NewHTTPSource(opts HTTPSourceOptions) (*HTTPSource, error) {
	if _, err := requireURL(opts.URL, "http source url"); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 4 << 20
	}
	return &HTTPSource{url: opts.URL, token: opts.Token, client: opts.Client, maxBytes: opts.MaxBytes}, nil
}

func (s *HTTPSource) Name() string { return "http" }

func (s *HTTPSource) Load(ctx context.Context, previous Version) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("create config request: %w", err)
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if s.etag != "" {
		req.Header.Set("If-None-Match", s.etag)
	}
	if s.modified != "" {
		req.Header.Set("If-Modified-Since", s.modified)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("fetch config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return Snapshot{Version: previous.Value}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("config endpoint returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBytes+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read config response: %w", err)
	}
	if int64(len(data)) > s.maxBytes {
		return Snapshot{}, fmt.Errorf("config response exceeds %d bytes", s.maxBytes)
	}
	if len(data) == 0 {
		return Snapshot{}, fmt.Errorf("config response is empty")
	}
	s.etag = resp.Header.Get("ETag")
	s.modified = resp.Header.Get("Last-Modified")
	lastModified, _ := http.ParseTime(s.modified)
	version := s.etag
	if version == "" {
		version = resp.Header.Get("X-Config-Version")
	}
	return Snapshot{Data: append([]byte(nil), data...), Version: version, LastModified: lastModified}, nil
}

func (s *HTTPSource) Close() error { return nil }
