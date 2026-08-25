package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ConsulSourceOptions configures the Consul KV HTTP API.
type ConsulSourceOptions struct {
	Address  string
	Prefix   string
	Token    string
	Client   *http.Client
	MaxBytes int64
}

type ConsulSource struct {
	address  string
	prefix   string
	token    string
	client   *http.Client
	maxBytes int64
}

type consulKV struct {
	Key         string `json:"Key"`
	Value       string `json:"Value"`
	ModifyIndex uint64 `json:"ModifyIndex"`
}

func NewConsulSource(opts ConsulSourceOptions) (*ConsulSource, error) {
	if _, err := requireURL(opts.Address, "consul address"); err != nil {
		return nil, err
	}
	if strings.Trim(opts.Prefix, "/") == "" {
		return nil, fmt.Errorf("consul prefix is required")
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 4 << 20
	}
	return &ConsulSource{address: strings.TrimRight(opts.Address, "/"), prefix: strings.Trim(opts.Prefix, "/"), token: opts.Token, client: opts.Client, maxBytes: opts.MaxBytes}, nil
}

func (s *ConsulSource) Name() string { return "consul" }

func (s *ConsulSource) Load(ctx context.Context, previous Version) (Snapshot, error) {
	endpoint := s.address + "/v1/kv/" + s.prefix + "?recurse=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("create consul request: %w", err)
	}
	if s.token != "" {
		req.Header.Set("X-Consul-Token", s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("fetch consul config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("consul returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBytes+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read consul response: %w", err)
	}
	if int64(len(body)) > s.maxBytes {
		return Snapshot{}, fmt.Errorf("consul response exceeds %d bytes", s.maxBytes)
	}
	var entries []consulKV
	if err := json.Unmarshal(body, &entries); err != nil {
		return Snapshot{}, fmt.Errorf("decode consul response: %w", err)
	}
	values := make(map[string]string, len(entries))
	var maxIndex uint64
	for _, entry := range entries {
		value, err := base64.StdEncoding.DecodeString(entry.Value)
		if err != nil {
			return Snapshot{}, fmt.Errorf("decode consul value: %w", err)
		}
		key := strings.TrimPrefix(entry.Key, s.prefix+"/")
		values[key] = string(value)
		if entry.ModifyIndex > maxIndex {
			maxIndex = entry.ModifyIndex
		}
	}
	data, err := documentFromKeyValues(values)
	if err != nil {
		return Snapshot{}, err
	}
	version := resp.Header.Get("X-Consul-Index")
	if version == "" && maxIndex > 0 {
		version = fmt.Sprintf("%d", maxIndex)
	}
	return Snapshot{Data: data, Version: version}, nil
}

func (s *ConsulSource) Close() error { return nil }
