package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPSourceOptions controls an HTTP configuration endpoint.
type HTTPSourceOptions struct {
	URL      string
	Token    string
	Scheme   string
	Client   *http.Client
	MaxBytes int64
}

// HTTPSource loads a complete document over HTTP with conditional requests.
type HTTPSource struct {
	url      string
	token    string
	client   *http.Client
	maxBytes int64
	name     string
}

func NewHTTPSource(opts HTTPSourceOptions) (*HTTPSource, error) {
	u, err := requireURL(opts.URL, "http source url")
	if err != nil {
		return nil, err
	}
	expectedScheme := strings.ToLower(strings.TrimSpace(opts.Scheme))
	if expectedScheme == "" {
		expectedScheme = strings.ToLower(u.Scheme)
	}
	if expectedScheme != "http" && expectedScheme != "https" {
		return nil, fmt.Errorf("http source scheme must be http or https")
	}
	if !strings.EqualFold(u.Scheme, expectedScheme) {
		return nil, fmt.Errorf("http source URL scheme %q does not match runtime source %q", u.Scheme, expectedScheme)
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 10 * time.Second}
	}
	client := *opts.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 4 << 20
	}
	return &HTTPSource{url: opts.URL, token: opts.Token, client: &client, maxBytes: opts.MaxBytes, name: expectedScheme}, nil
}

func (s *HTTPSource) Name() string { return s.name }

func (s *HTTPSource) Load(ctx context.Context, previous Version) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("create config request: %w", err)
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if previous.ETag != "" {
		req.Header.Set("If-None-Match", previous.ETag)
	}
	if !previous.LastModified.IsZero() {
		req.Header.Set("If-Modified-Since", previous.LastModified.UTC().Format(http.TimeFormat))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("fetch config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return Snapshot{Version: previous.Value, ETag: previous.ETag, LastModified: previous.LastModified}, nil
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
	etag := resp.Header.Get("ETag")
	lastModified, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	version := resp.Header.Get("X-Config-Version")
	if version == "" {
		version = etag
	}
	return Snapshot{Data: append([]byte(nil), data...), Version: version, ETag: etag, LastModified: lastModified}, nil
}

func (s *HTTPSource) Close() error { return nil }
