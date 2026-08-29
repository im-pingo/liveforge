package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
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

func TestFileSourceWriteReplacesDocumentAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveforge.yaml")
	if err := os.WriteFile(path, []byte("server:\n  name: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewFileSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Write(context.Background(), []byte("server:\n  name: new\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "server:\n  name: new\n" {
		t.Fatalf("written config=%q err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode=%v err=%v", info.Mode().Perm(), err)
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
	second, err := source.Load(context.Background(), Version{
		Value:        first.Version,
		ETag:         first.ETag,
		LastModified: first.LastModified,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Data) != 0 || second.Version != first.Version {
		t.Fatalf("304 snapshot = %+v", second)
	}
}

func TestHTTPSourceWriteUsesAuthenticatedPut(t *testing.T) {
	var method, auth, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, auth = r.Method, r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	source, err := NewHTTPSource(HTTPSourceOptions{URL: server.URL, Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Write(context.Background(), []byte("server:\n  name: http\n")); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || auth != "Bearer secret" || body == "" {
		t.Fatalf("write request method=%q auth=%q body=%q", method, auth, body)
	}
}

func TestHTTPSourceRejectedValidatorsAreNotReusedByManager(t *testing.T) {
	const (
		acceptedETag     = `"accepted"`
		rejectedETag     = `"rejected"`
		acceptedModified = "Mon, 24 Aug 2026 01:02:03 GMT"
		rejectedModified = "Mon, 24 Aug 2026 02:03:04 GMT"
	)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			w.Header().Set("ETag", acceptedETag)
			w.Header().Set("Last-Modified", acceptedModified)
			w.Header().Set("X-Config-Version", "accepted-source-version")
			_, _ = fmt.Fprint(w, "limits:\n  max_streams: 10\n")
		case 2:
			if got := r.Header.Get("If-None-Match"); got != acceptedETag {
				t.Errorf("second If-None-Match = %q, want %q", got, acceptedETag)
			}
			if got := r.Header.Get("If-Modified-Since"); got != acceptedModified {
				t.Errorf("second If-Modified-Since = %q, want %q", got, acceptedModified)
			}
			w.Header().Set("ETag", rejectedETag)
			w.Header().Set("Last-Modified", rejectedModified)
			w.Header().Set("X-Config-Version", "rejected-source-version")
			_, _ = fmt.Fprint(w, "api:\n  console:\n    username: incomplete\n")
		case 3:
			if got := r.Header.Get("If-None-Match"); got != acceptedETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			if got := r.Header.Get("If-Modified-Since"); got != acceptedModified {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"accepted-next"`)
			w.Header().Set("X-Config-Version", "accepted-source-version-next")
			_, _ = fmt.Fprint(w, "limits:\n  max_streams: 20\n")
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	source, err := NewHTTPSource(HTTPSourceOptions{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Options{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	if err := manager.load(context.Background()); err != nil {
		t.Fatalf("accepted load: %v", err)
	}
	if got := manager.Snapshot().Version.Value; got != "accepted-source-version" {
		t.Fatalf("accepted source version = %q", got)
	}
	if err := manager.load(context.Background()); err == nil {
		t.Fatal("invalid response was accepted")
	}
	failed := manager.Status()
	if failed.ConsecutiveFailures != 1 || failed.LastError == "" {
		t.Fatalf("failure status after invalid response = %+v", failed)
	}
	if err := manager.load(context.Background()); err != nil {
		t.Fatalf("later valid load: %v", err)
	}
	if got := manager.Snapshot().Config.Limits.MaxStreams; got != 20 {
		t.Fatalf("max_streams = %d, want later valid value 20", got)
	}
	if got := manager.Status().ConsecutiveFailures; got != 0 {
		t.Fatalf("consecutive failures after later valid response = %d", got)
	}
}

func TestHTTPSourceVersionDoesNotConflateConfigVersionWithETag(t *testing.T) {
	const modified = "Mon, 24 Aug 2026 01:02:03 GMT"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"document-validator"`)
		w.Header().Set("Last-Modified", modified)
		w.Header().Set("X-Config-Version", "deployment-42")
		_, _ = fmt.Fprint(w, "server:\n  name: http\n")
	}))
	defer server.Close()

	source, err := NewHTTPSource(HTTPSourceOptions{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Load(context.Background(), Version{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != "deployment-42" {
		t.Fatalf("source version = %q, want X-Config-Version", snapshot.Version)
	}
	wantModified, err := http.ParseTime(modified)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.LastModified.Equal(wantModified) {
		t.Fatalf("last modified = %s, want %s", snapshot.LastModified, wantModified)
	}
}

func TestHTTPSourceRejectsRedirects(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect target received Authorization %q", got)
		}
		_, _ = fmt.Fprint(w, "server:\n  name: redirected\n")
	}))
	defer target.Close()

	crossHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/config", http.StatusFound)
	}))
	defer crossHost.Close()

	sameHostRequests := 0
	var sameHost *httptest.Server
	sameHost = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sameHostRequests++
		if r.URL.Path == "/config" {
			http.Redirect(w, r, sameHost.URL+"/next", http.StatusFound)
			return
		}
		_, _ = fmt.Fprint(w, "server:\n  name: redirected\n")
	}))
	defer sameHost.Close()

	downgrade := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/downgraded", http.StatusFound)
	}))
	defer downgrade.Close()

	tests := []struct {
		name   string
		url    string
		client *http.Client
	}{
		{name: "cross-host", url: crossHost.URL},
		{name: "same-host-scheme", url: sameHost.URL + "/config"},
		{name: "https-downgrade", url: downgrade.URL, client: downgrade.Client()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := NewHTTPSource(HTTPSourceOptions{URL: tt.url, Token: "source-secret", Client: tt.client})
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.Load(context.Background(), Version{})
			if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
				t.Fatalf("redirect load error = %v, want HTTP 302 rejection", err)
			}
		})
	}
	if targetRequests != 0 {
		t.Fatalf("cross-origin redirect target received %d request(s)", targetRequests)
	}
	if sameHostRequests != 1 {
		t.Fatalf("same-host redirect dispatched %d request(s), want only original request", sameHostRequests)
	}
}

func TestBuildSourceRejectsHTTPURLSchemeMismatchBeforeDispatch(t *testing.T) {
	tests := []struct {
		name   string
		source string
		url    string
	}{
		{name: "http-source-with-https-url", source: "http", url: "https://config.example.test/liveforge.yaml"},
		{name: "https-source-with-http-url", source: "https", url: "http://config.example.test/liveforge.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildSource(config.RuntimeConfig{
				Source: tt.source,
				HTTP:   config.RuntimeHTTPSourceConfig{URL: tt.url},
			}, "")
			if err == nil || !strings.Contains(err.Error(), "scheme") {
				t.Fatalf("BuildSource error = %v, want scheme mismatch", err)
			}
		})
	}
}

func TestHTTPSourceLastModifiedConditionalRequest(t *testing.T) {
	const modified = "Mon, 24 Aug 2026 01:02:03 GMT"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Last-Modified", modified)
			w.Header().Set("X-Config-Version", "revision-1")
			_, _ = fmt.Fprint(w, "server:\n  name: http\n")
			return
		}
		if got := r.Header.Get("If-Modified-Since"); got != modified {
			t.Fatalf("If-Modified-Since = %q, want %q", got, modified)
		}
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Fatalf("If-None-Match = %q, X-Config-Version must not be used as an ETag", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	source, err := NewHTTPSource(HTTPSourceOptions{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.Load(context.Background(), Version{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Load(context.Background(), Version{
		Value:        first.Version,
		ETag:         first.ETag,
		LastModified: first.LastModified,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != first.Version || len(second.Data) != 0 {
		t.Fatalf("304 snapshot = %+v", second)
	}
	if first.LastModified.IsZero() {
		t.Fatalf("invalid Last-Modified metadata: %s", first.LastModified)
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

func TestConsulSourceWriteUsesCompleteConfigKey(t *testing.T) {
	var path, token, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token = r.URL.Path, r.Header.Get("X-Consul-Token")
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	source, err := NewConsulSource(ConsulSourceOptions{Address: server.URL, Prefix: "liveforge", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Write(context.Background(), []byte("server:\n  name: consul\n")); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/kv/liveforge/config.yaml" || token != "secret" || body == "" {
		t.Fatalf("consul write path=%q token=%q body=%q", path, token, body)
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

func TestParseFlattenedScalar(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  any
	}{
		{name: "positive integer", value: "100", want: int64(100)},
		{name: "negative integer", value: "-42", want: int64(-42)},
		{name: "decimal float", value: "1.25", want: 1.25},
		{name: "exponent float", value: "6.25e-2", want: 0.0625},
		{name: "true", value: "true", want: true},
		{name: "false", value: "FALSE", want: false},
		{name: "null", value: "null", want: nil},
		{name: "ordinary string", value: "liveforge", want: "liveforge"},
		{name: "duration", value: "15s", want: "15s"},
		{name: "leading zero identifier", value: "00123", want: "00123"},
		{name: "negative leading zero identifier", value: "-01", want: "-01"},
		{name: "leading zero float identifier", value: "01.5", want: "01.5"},
		{name: "plus-prefixed integer", value: "+12", want: "+12"},
		{name: "underscored integer", value: "1_000", want: "1_000"},
		{name: "hexadecimal integer", value: "0x10", want: "0x10"},
		{name: "integer overflow", value: "9223372036854775808", want: "9223372036854775808"},
		{name: "float overflow", value: "1e309", want: "1e309"},
		{name: "not a number", value: "NaN", want: "NaN"},
		{name: "infinity", value: ".inf", want: ".inf"},
		{name: "YAML sequence", value: "[one, two]", want: "[one, two]"},
		{name: "YAML mapping", value: "{key: value}", want: "{key: value}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseScalar(test.value); got != test.want {
				t.Fatalf("parseScalar(%q) = %#v (%T), want %#v (%T)", test.value, got, got, test.want, test.want)
			}
		})
	}
}

func TestFlattenedKeyValueDocumentsDecodeTypedNumericConfig(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
	}{
		{
			name: "consul slash paths",
			values: map[string]string{
				"limits/max_connections":             "100",
				"limits/max_streams":                 "25",
				"http_stream/llhls/segment_duration": "1.25",
				"http_stream/llhls/part_duration":    "0.2",
				"http_stream/llhls/segment_count":    "6",
				"http_stream/llhls/enabled":          "true",
			},
		},
		{
			name: "redis dotted paths",
			values: map[string]string{
				"limits.max_connections":             "100",
				"limits.max_streams":                 "25",
				"http_stream.llhls.segment_duration": "1.25",
				"http_stream.llhls.part_duration":    "0.2",
				"http_stream.llhls.segment_count":    "6",
				"http_stream.llhls.enabled":          "true",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := documentFromKeyValues(test.values)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseDocument(document)
			if err != nil {
				t.Fatalf("parse generated document: %v\n%s", err, document)
			}
			if cfg.Limits.MaxConnections != 100 || cfg.Limits.MaxStreams != 25 {
				t.Fatalf("typed limits = %+v, want max_connections=100 max_streams=25", cfg.Limits)
			}
			if cfg.HTTP.LLHLS.SegmentDuration != 1.25 || cfg.HTTP.LLHLS.PartDuration != 0.2 || cfg.HTTP.LLHLS.SegmentCount != 6 {
				t.Fatalf("typed LL-HLS config = %+v", cfg.HTTP.LLHLS)
			}
		})
	}
}

func TestFlattenedKeyValueDocumentSerializationIsDeterministic(t *testing.T) {
	first, err := documentFromKeyValues(map[string]string{
		"limits.max_connections":             "100",
		"http_stream.llhls.segment_duration": "1.25",
		"server.name":                        "liveforge",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := documentFromKeyValues(map[string]string{
		"server.name":                        "liveforge",
		"http_stream.llhls.segment_duration": "1.25",
		"limits.max_connections":             "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("flattened documents differ:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
