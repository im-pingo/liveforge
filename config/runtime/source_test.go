package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestFileSourceReturnsDocumentAndModificationMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "liveforge.yaml")
	if err := os.WriteFile(path, []byte("server:\n  name: file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewFileSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshot, err := source.Load(context.Background(), Version{})
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Data) != "server:\n  name: file\n" {
		t.Fatalf("unexpected file data: %q", snapshot.Data)
	}
	if snapshot.LastModified.IsZero() || snapshot.Version == "" {
		t.Fatalf("missing file metadata: %+v", snapshot)
	}
}

func TestHTTPSourceUsesETagAndAcceptsNotModified(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("ETag", "v1")
			_, _ = fmt.Fprint(w, "server:\n  name: http\n")
			return
		}
		if got := r.Header.Get("If-None-Match"); got != "v1" {
			t.Errorf("If-None-Match = %q", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	source, err := NewHTTPSource(HTTPSourceOptions{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	first, err := source.Load(context.Background(), Version{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Load(context.Background(), Version{Value: first.Version})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Data) != 0 || second.Version != first.Version {
		t.Fatalf("304 snapshot = %+v", second)
	}
}

func TestConsulSourceDecodesKVPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/kv/liveforge" || r.URL.Query().Get("recurse") != "true" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		w.Header().Set("X-Consul-Index", "17")
		_, _ = fmt.Fprintf(w, `[{"Key":"liveforge/server/name","Value":%q,"ModifyIndex":17}]`, base64.StdEncoding.EncodeToString([]byte("consul")))
	}))
	defer server.Close()
	source, err := NewConsulSource(ConsulSourceOptions{Address: server.URL, Prefix: "liveforge"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshot, err := source.Load(context.Background(), Version{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseDocument(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Name != "consul" || snapshot.Version != "17" {
		t.Fatalf("consul snapshot = %+v cfg=%+v", snapshot, cfg.Server)
	}
}

func TestRedisSourceBuildsNestedDocumentFromKeys(t *testing.T) {
	doc, err := documentFromKeyValues(map[string]string{
		"server.name":         "redis",
		"http_stream.enabled": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Name != "redis" || !cfg.HTTP.Enabled {
		t.Fatalf("redis document = %q cfg=%+v", doc, cfg)
	}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	source, err := NewRedisSource(RedisSourceOptions{Client: client, Hash: "config"})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
}
