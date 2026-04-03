# Cluster Multi-Protocol Relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the cluster module with a plugin-based transport system supporting RTMP, SRT, RTSP, and RTP direct relay between nodes, selectable via URL scheme.

**Architecture:** A `RelayTransport` interface defines Push/Pull operations. Each protocol registers as a plugin in a `TransportRegistry`. `ForwardManager` and `OriginManager` become protocol-agnostic — they resolve the transport by URL scheme at runtime. RTP direct relay uses SDP-over-HTTP signaling (no ICE/DTLS) for lowest latency.

**Tech Stack:** Go 1.21+, `github.com/datarhei/gosrt`, `github.com/pion/rtp/v2`, existing `pkg/rtp/`, `pkg/sdp/`, `pkg/muxer/ts/`, `pkg/muxer/flv/`

**Spec:** `docs/superpowers/specs/2026-04-03-cluster-multi-protocol-relay-design.md`

---

## File Structure

```
module/cluster/
├── module.go              # MODIFY — add registry init, new config fields
├── transport.go           # CREATE — RelayTransport interface + ErrCodecMismatch
├── registry.go            # CREATE — TransportRegistry
├── forward.go             # MODIFY — use registry instead of direct RTMP
├── origin.go              # MODIFY — use registry instead of direct RTMP
├── transport_rtmp.go      # CREATE — RTMPTransport (extracted from rtmp_client.go)
├── transport_srt.go       # CREATE — SRTTransport
├── transport_rtsp.go      # CREATE — RTSPTransport
├── transport_rtp.go       # CREATE — RTPTransport + SDP signaling
├── rtmp_client.go         # DELETE — logic moves to transport_rtmp.go
├── registry_test.go       # CREATE
├── transport_rtmp_test.go # CREATE
├── transport_srt_test.go  # CREATE
├── transport_rtsp_test.go # CREATE
├── transport_rtp_test.go  # CREATE
├── forward_test.go        # MODIFY — test with registry
├── origin_test.go         # (part of module_test.go, modify)
├── module_test.go         # MODIFY — test new init path
├── integration_test.go    # MODIFY — update for transport interface
├── scheduler.go           # UNCHANGED
├── scheduler_test.go      # UNCHANGED
config/
├── config.go              # MODIFY — add ClusterSRTConfig, ClusterRTSPConfig, ClusterRTPConfig
```

---

## Phase 1: Interface + Registry + RTMP Refactor

### Task 1: RelayTransport Interface + Sentinel Errors

**Files:**
- Create: `module/cluster/transport.go`
- Test: `module/cluster/transport_test.go` (interface compile checks)

- [ ] **Step 1: Write the interface file**

```go
// module/cluster/transport.go
package cluster

import (
	"context"
	"errors"

	"github.com/im-pingo/liveforge/core"
)

// ErrCodecMismatch is returned when remote node rejects all offered codecs.
// This error is non-retryable.
var ErrCodecMismatch = errors.New("codec mismatch: remote rejected all offered codecs")

// RelayTransport is the plugin interface for cluster relay protocols.
// Each protocol (RTMP, SRT, RTSP, RTP) implements this interface and
// registers with the TransportRegistry.
type RelayTransport interface {
	// Scheme returns the URL scheme this transport handles ("rtmp", "srt", "rtsp", "rtp").
	Scheme() string

	// Push connects to a remote node and pushes frames from a local stream.
	// Returns nil on normal termination (stream ended, context cancelled).
	// Returns error on abnormal disconnection (network error, protocol error).
	// Callers use errors.Is(err, ErrCodecMismatch) to detect non-retryable errors.
	Push(ctx context.Context, targetURL string, stream *core.Stream) error

	// Pull connects to a remote node and pulls frames into a local stream.
	// stream.WriteFrame() returning false (bitrate-limited) is silently dropped.
	// Returns nil on normal termination, error on abnormal disconnection.
	Pull(ctx context.Context, sourceURL string, stream *core.Stream) error

	// Close releases any resources held by this transport.
	Close() error
}
```

- [ ] **Step 2: Write compile-check test**

```go
// module/cluster/transport_test.go
package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/im-pingo/liveforge/core"
)

// mockTransport verifies interface compliance at compile time.
type mockTransport struct{}

var _ RelayTransport = (*mockTransport)(nil)

func (m *mockTransport) Scheme() string                                              { return "mock" }
func (m *mockTransport) Push(ctx context.Context, url string, s *core.Stream) error  { return nil }
func (m *mockTransport) Pull(ctx context.Context, url string, s *core.Stream) error  { return nil }
func (m *mockTransport) Close() error                                                { return nil }

func TestErrCodecMismatchIsDetectable(t *testing.T) {
	wrapped := fmt.Errorf("push failed: %w", ErrCodecMismatch)
	if !errors.Is(wrapped, ErrCodecMismatch) {
		t.Error("wrapped ErrCodecMismatch should be detectable via errors.Is")
	}
}
```

- [ ] **Step 3: Run tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestErrCodecMismatch -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add module/cluster/transport.go module/cluster/transport_test.go
git commit -m "feat: add RelayTransport interface and ErrCodecMismatch sentinel"
```

---

### Task 2: TransportRegistry

**Files:**
- Create: `module/cluster/registry.go`
- Create: `module/cluster/registry_test.go`

- [ ] **Step 1: Write registry tests**

```go
// module/cluster/registry_test.go
package cluster

import (
	"testing"
)

func TestRegistryRegisterAndResolve(t *testing.T) {
	r := NewTransportRegistry()
	r.Register(&mockTransport{})

	tr, err := r.Resolve("mock://host/path")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tr.Scheme() != "mock" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "mock")
	}
}

func TestRegistryResolveUnknownScheme(t *testing.T) {
	r := NewTransportRegistry()

	_, err := r.Resolve("unknown://host/path")
	if err == nil {
		t.Fatal("expected error for unknown scheme")
	}
}

func TestRegistryResolveNoScheme(t *testing.T) {
	r := NewTransportRegistry()

	_, err := r.Resolve("host/path")
	if err == nil {
		t.Fatal("expected error for URL without scheme")
	}
}

func TestRegistryResolveMultipleTransports(t *testing.T) {
	r := NewTransportRegistry()
	r.Register(&mockTransport{})

	mock2 := &mockTransportAlt{}
	r.Register(mock2)

	tr, err := r.Resolve("alt://host/path")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tr.Scheme() != "alt" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "alt")
	}
}

type mockTransportAlt struct{ mockTransport }

func (m *mockTransportAlt) Scheme() string { return "alt" }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestRegistry -v`
Expected: FAIL (NewTransportRegistry not defined)

- [ ] **Step 3: Write registry implementation**

```go
// module/cluster/registry.go
package cluster

import (
	"fmt"
	"strings"
	"sync"
)

// TransportRegistry manages protocol transport plugins.
// Transports are registered by URL scheme and resolved at runtime.
type TransportRegistry struct {
	mu         sync.RWMutex
	transports map[string]RelayTransport
}

// NewTransportRegistry creates an empty registry.
func NewTransportRegistry() *TransportRegistry {
	return &TransportRegistry{
		transports: make(map[string]RelayTransport),
	}
}

// Register adds a transport plugin. Overwrites any existing transport
// for the same scheme.
func (r *TransportRegistry) Register(t RelayTransport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transports[t.Scheme()] = t
}

// Resolve finds the transport for the given URL's scheme.
func (r *TransportRegistry) Resolve(rawURL string) (RelayTransport, error) {
	scheme := extractScheme(rawURL)
	if scheme == "" {
		return nil, fmt.Errorf("no scheme in URL: %q", rawURL)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	t, ok := r.transports[scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported relay scheme: %q", scheme)
	}
	return t, nil
}

// Close closes all registered transports.
func (r *TransportRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.transports {
		t.Close()
	}
}

// extractScheme returns the scheme portion of a URL (before "://").
func extractScheme(rawURL string) string {
	idx := strings.Index(rawURL, "://")
	if idx <= 0 {
		return ""
	}
	return rawURL[:idx]
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestRegistry -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add module/cluster/registry.go module/cluster/registry_test.go
git commit -m "feat: add TransportRegistry for protocol plugin resolution"
```

---

### Task 3: RTMPTransport Plugin (Extract from rtmp_client.go)

**Files:**
- Create: `module/cluster/transport_rtmp.go`
- Create: `module/cluster/transport_rtmp_test.go`
- Modify: `module/cluster/rtmp_client.go` — keep only low-level helpers (`rtmpConn`, `dialRTMP`, `clientHandshake`, `parseRTMPURL`, `parseVideoPayload`, `parseAudioPayload`, `buildRTMPPayload`)

- [ ] **Step 1: Write RTMPTransport test**

```go
// module/cluster/transport_rtmp_test.go
package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRTMPTransportScheme(t *testing.T) {
	tr := NewRTMPTransport()
	if tr.Scheme() != "rtmp" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "rtmp")
	}
}

func TestRTMPTransportPushToMockServer(t *testing.T) {
	ln, addr := mockRTMPServer(t)
	defer ln.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/rtmptest")
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
	}}
	stream.SetPublisher(pub)

	// Write sequence headers
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	})
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio, Codec: avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x12, 0x10},
	})

	tr := NewRTMPTransport()
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Write some frames in the background
	go func() {
		time.Sleep(100 * time.Millisecond)
		for i := 0; i < 5; i++ {
			stream.WriteFrame(&avframe.AVFrame{
				MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264,
				FrameType: avframe.FrameTypeKeyframe,
				DTS: int64((i + 1) * 33), PTS: int64((i + 1) * 33),
				Payload: []byte{0x65, 0x88, 0x00, 0x01},
			})
			time.Sleep(33 * time.Millisecond)
		}
		cancel()
	}()

	// Push should run until context cancels
	err := tr.Push(ctx, "rtmp://"+addr+"/live/rtmptest", stream)
	// Either nil (context cancelled) or an error (connection closed) is acceptable
	_ = err
}

func TestRTMPTransportPushBadURL(t *testing.T) {
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	tr := NewRTMPTransport()
	defer tr.Close()

	ctx := context.Background()
	err := tr.Push(ctx, "bad-url-no-scheme", stream)
	if err == nil {
		t.Error("expected error for bad URL")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestRTMPTransport -v`
Expected: FAIL (NewRTMPTransport not defined)

- [ ] **Step 3: Write RTMPTransport implementation**

Create `module/cluster/transport_rtmp.go` implementing `RelayTransport` with `Scheme() = "rtmp"`. The `Push()` method extracts the logic from `ForwardTarget.forwardOnce()`: parse URL, dial RTMP, handshake, connect, publish, send seq headers, read from ring buffer, send frames. The `Pull()` method extracts the logic from `OriginPull.pullOnce()`: parse URL, dial RTMP, connect, play, read messages, parse video/audio, write frames to stream. Both respect `ctx.Done()` for cancellation.

The existing helper functions in `rtmp_client.go` (`dialRTMP`, `rtmpConn`, `parseRTMPURL`, `buildRTMPPayload`, `parseVideoPayload`, `parseAudioPayload`, `clientHandshake`) stay in `rtmp_client.go` as shared utilities.

```go
// module/cluster/transport_rtmp.go
package cluster

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// RTMPTransport implements RelayTransport for RTMP protocol.
type RTMPTransport struct{}

// NewRTMPTransport creates a new RTMP relay transport.
func NewRTMPTransport() *RTMPTransport {
	return &RTMPTransport{}
}

func (t *RTMPTransport) Scheme() string { return "rtmp" }

func (t *RTMPTransport) Push(ctx context.Context, targetURL string, stream *core.Stream) error {
	host, app, streamName, err := parseRTMPURL(targetURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	rc, err := dialRTMP(host)
	if err != nil {
		return err
	}
	defer rc.conn.Close()

	if err := rc.setChunkSize(defaultChunkSize); err != nil {
		return fmt.Errorf("set chunk size: %w", err)
	}
	if err := rc.sendConnect(app); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := rc.readResponses(1); err != nil {
		return fmt.Errorf("connect response: %w", err)
	}
	if err := rc.sendPublish(streamName); err != nil {
		return fmt.Errorf("publish commands: %w", err)
	}
	if err := rc.readResponses(4); err != nil {
		return fmt.Errorf("createStream response: %w", err)
	}

	publishPayload, _ := rtmp.AMF0Encode("publish", float64(5), nil, streamName, "live")
	if err := rc.cw.WriteMessage(8, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(publishPayload)),
		StreamID: 1, Payload: publishPayload,
	}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if err := rc.readResponses(5); err != nil {
		return fmt.Errorf("publish response: %w", err)
	}

	slog.Info("rtmp relay push connected", "module", "cluster", "target", targetURL)

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		if err := rc.sendMediaFrame(vsh); err != nil {
			return fmt.Errorf("video seq header: %w", err)
		}
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		if err := rc.sendMediaFrame(ash); err != nil {
			return fmt.Errorf("audio seq header: %w", err)
		}
	}

	reader := stream.RingBuffer().NewReader()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			if stream.RingBuffer().IsClosed() {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-stream.RingBuffer().Signal():
			}
			continue
		}

		if err := rc.sendMediaFrame(frame); err != nil {
			return fmt.Errorf("send frame: %w", err)
		}
	}
}

func (t *RTMPTransport) Pull(ctx context.Context, sourceURL string, stream *core.Stream) error {
	host, app, streamName, err := parseRTMPURL(sourceURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	rc, err := dialRTMP(host)
	if err != nil {
		return err
	}
	defer rc.conn.Close()

	if err := rc.sendConnect(app); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := rc.readResponses(1); err != nil {
		return fmt.Errorf("connect response: %w", err)
	}
	if err := rc.sendPlay(streamName); err != nil {
		return fmt.Errorf("play commands: %w", err)
	}
	if err := rc.readResponses(2); err != nil {
		return fmt.Errorf("createStream response: %w", err)
	}

	playPayload, _ := rtmp.AMF0Encode("play", float64(3), nil, streamName)
	if err := rc.cw.WriteMessage(8, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(playPayload)),
		StreamID: 1, Payload: playPayload,
	}); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	slog.Info("rtmp relay pull connected", "module", "cluster", "source", sourceURL)

	pub := &originPublisher{
		id:   fmt.Sprintf("rtmp-pull-%s", stream.Key()),
		info: &avframe.MediaInfo{},
	}
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}
	defer stream.RemovePublisher()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		msg, err := rc.cr.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		switch msg.TypeID {
		case rtmp.MsgSetChunkSize:
			if len(msg.Payload) >= 4 {
				size := int(binary.BigEndian.Uint32(msg.Payload))
				rc.cr.SetChunkSize(size)
			}
		case rtmp.MsgVideo:
			frame := parseVideoPayload(msg.Payload, int64(msg.Timestamp))
			if frame != nil {
				if pub.info.VideoCodec == 0 {
					pub.info.VideoCodec = frame.Codec
				}
				stream.WriteFrame(frame)
			}
		case rtmp.MsgAudio:
			frame := parseAudioPayload(msg.Payload, int64(msg.Timestamp))
			if frame != nil {
				if pub.info.AudioCodec == 0 {
					pub.info.AudioCodec = frame.Codec
				}
				stream.WriteFrame(frame)
			}
		case rtmp.MsgAMF0Command:
			vals, err := rtmp.AMF0Decode(msg.Payload)
			if err != nil || len(vals) < 1 {
				continue
			}
			cmd, _ := vals[0].(string)
			if cmd == "onStatus" && len(vals) >= 4 {
				if m, ok := vals[3].(map[string]any); ok {
					code, _ := m["code"].(string)
					if code == "NetStream.Play.UnpublishNotify" || code == "NetStream.Play.Stop" {
						slog.Info("rtmp relay stream ended", "module", "cluster", "code", code)
						return nil
					}
				}
			}
		}
	}
}

func (t *RTMPTransport) Close() error { return nil }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestRTMPTransport -v`
Expected: PASS

- [ ] **Step 5: Run all existing cluster tests to verify no regression**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All tests PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/transport_rtmp.go module/cluster/transport_rtmp_test.go
git commit -m "feat: add RTMPTransport plugin implementing RelayTransport"
```

---

### Task 3.5: Clean Up rtmp_client.go

**Files:**
- Modify: `module/cluster/rtmp_client.go` — remove the push/pull orchestration logic that moved to transport_rtmp.go, keep only shared utilities

- [ ] **Step 1: Verify no duplicate code**

Check that `rtmp_client.go` only contains:
- `rtmpConn` struct + `dialRTMP` + `clientHandshake`
- `sendConnect`, `sendPublish`, `sendPlay`, `readResponses`, `setChunkSize`, `sendMediaFrame`
- `buildRTMPPayload`, `parseRTMPURL`, `parseVideoPayload`, `parseAudioPayload`

Remove any push/pull orchestration logic that was moved to `transport_rtmp.go`. The `forwardOnce()` and `pullOnce()` methods from the old `ForwardTarget`/`OriginPull` are now in `RTMPTransport.Push()`/`Pull()`.

- [ ] **Step 2: Run all tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 3: Commit**

```bash
git add module/cluster/rtmp_client.go
git commit -m "refactor: rtmp_client.go retains only shared RTMP helpers"
```

---

### Task 4: Config Schema Extension

**Files:**
- Modify: `config/config.go:277-304` — add ClusterSRTConfig, ClusterRTSPConfig, ClusterRTPConfig to ClusterConfig

- [ ] **Step 0: Write config defaults test first (TDD)**

Write a test in an appropriate config test file to assert all new defaults:

```go
func TestClusterTransportConfigDefaults(t *testing.T) {
	cfg := &Config{}
	setDefaults(cfg) // or however defaults are applied

	if cfg.Cluster.SRT.Latency != 120*time.Millisecond {
		t.Errorf("SRT.Latency = %v, want 120ms", cfg.Cluster.SRT.Latency)
	}
	if cfg.Cluster.SRT.PBKeyLen != 16 {
		t.Errorf("SRT.PBKeyLen = %d, want 16", cfg.Cluster.SRT.PBKeyLen)
	}
	if cfg.Cluster.RTSP.Transport != "tcp" {
		t.Errorf("RTSP.Transport = %q, want tcp", cfg.Cluster.RTSP.Transport)
	}
	if cfg.Cluster.RTP.PortRange != "20000-20100" {
		t.Errorf("RTP.PortRange = %q, want 20000-20100", cfg.Cluster.RTP.PortRange)
	}
	if cfg.Cluster.RTP.SignalingPath != "/api/relay" {
		t.Errorf("RTP.SignalingPath = %q, want /api/relay", cfg.Cluster.RTP.SignalingPath)
	}
	if cfg.Cluster.RTP.RTCPInterval != 5*time.Second {
		t.Errorf("RTP.RTCPInterval = %v, want 5s", cfg.Cluster.RTP.RTCPInterval)
	}
	if cfg.Cluster.RTP.Timeout != 15*time.Second {
		t.Errorf("RTP.Timeout = %v, want 15s", cfg.Cluster.RTP.Timeout)
	}
}
```

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./config/ -run TestClusterTransportConfigDefaults -v`
Expected: FAIL (new structs not defined yet)

- [ ] **Step 1: Add new config structs**

In `config/config.go`, after the existing `ClusterConfig` struct, add:

```go
// ClusterConfig holds cluster settings.
type ClusterConfig struct {
	Forward ForwardConfig      `yaml:"forward"`
	Origin  OriginConfig       `yaml:"origin"`
	SRT     ClusterSRTConfig   `yaml:"srt"`
	RTSP    ClusterRTSPConfig  `yaml:"rtsp"`
	RTP     ClusterRTPConfig   `yaml:"rtp"`
}

// ClusterSRTConfig holds SRT-specific relay settings.
type ClusterSRTConfig struct {
	Latency    time.Duration `yaml:"latency"`
	Passphrase string        `yaml:"passphrase"`
	PBKeyLen   int           `yaml:"pbkeylen"`
}

// ClusterRTSPConfig holds RTSP-specific relay settings.
type ClusterRTSPConfig struct {
	Transport string `yaml:"transport"` // "tcp" or "udp"
}

// ClusterRTPConfig holds RTP direct relay settings.
type ClusterRTPConfig struct {
	PortRange     string        `yaml:"port_range"`
	SignalingPath string        `yaml:"signaling_path"`
	RTCPInterval  time.Duration `yaml:"rtcp_interval"`
	Timeout       time.Duration `yaml:"timeout"`
}
```

- [ ] **Step 2: Add defaults in the defaults function**

Find the defaults function (or wherever config defaults are set) and add:

```go
// In setDefaults or equivalent:
if cfg.Cluster.SRT.Latency == 0 {
	cfg.Cluster.SRT.Latency = 120 * time.Millisecond
}
if cfg.Cluster.SRT.PBKeyLen == 0 {
	cfg.Cluster.SRT.PBKeyLen = 16
}
if cfg.Cluster.RTSP.Transport == "" {
	cfg.Cluster.RTSP.Transport = "tcp"
}
if cfg.Cluster.RTP.PortRange == "" {
	cfg.Cluster.RTP.PortRange = "20000-20100"
}
if cfg.Cluster.RTP.SignalingPath == "" {
	cfg.Cluster.RTP.SignalingPath = "/api/relay"
}
if cfg.Cluster.RTP.RTCPInterval == 0 {
	cfg.Cluster.RTP.RTCPInterval = 5 * time.Second
}
if cfg.Cluster.RTP.Timeout == 0 {
	cfg.Cluster.RTP.Timeout = 15 * time.Second
}
```

- [ ] **Step 3: Run all tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./config/ -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add config/config.go
git commit -m "feat: add cluster SRT, RTSP, and RTP config structs with defaults"
```

---

### Task 5: Refactor ForwardManager to Use Registry

**Files:**
- Modify: `module/cluster/forward.go` — replace direct RTMP calls with registry
- Modify: `module/cluster/module_test.go` — update tests for new ForwardManager signature

- [ ] **Step 1: Update ForwardManager struct**

In `forward.go`, replace direct RTMP logic in `ForwardTarget` and `ForwardManager`:

1. Add `registry *TransportRegistry` field to `ForwardManager`
2. Add `transport RelayTransport` field to `ForwardTarget`
3. Update `NewForwardManager` to accept `*TransportRegistry`
4. Update `NewForwardTarget` to accept `RelayTransport`
5. Replace `forwardOnce()` with `transport.Push(ctx, ...)`
6. Remove `containsStreamPath` and the auto-append logic — all URLs must be full paths
7. In `onPublish`, resolve transport via `fm.registry.Resolve(targetURL)`
8. Use exponential backoff in ForwardTarget.Run() (cap at 30s)

Key changes in `ForwardTarget.Run()`:
```go
func (ft *ForwardTarget) Run() {
	defer slog.Info("forward target stopped", "module", "cluster",
		"stream", ft.streamKey, "target", ft.targetURL)

	for attempt := 0; ; attempt++ {
		select {
		case <-ft.closed:
			return
		default:
		}

		if ft.retryMax > 0 && attempt >= ft.retryMax {
			slog.Warn("forward max retries exceeded", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "attempts", attempt)
			return
		}

		if attempt > 0 {
			delay := ft.retryDelay * time.Duration(1<<min(attempt-1, 4))
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			slog.Info("forward reconnecting", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "attempt", attempt)
			select {
			case <-ft.closed:
				return
			case <-time.After(delay):
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-ft.closed:
				cancel()
			case <-ctx.Done():
			}
		}()

		err := ft.transport.Push(ctx, ft.targetURL, ft.stream)
		cancel()

		if err != nil {
			if errors.Is(err, ErrCodecMismatch) {
				slog.Warn("forward codec mismatch, not retrying", "module", "cluster",
					"stream", ft.streamKey, "target", ft.targetURL, "error", err)
				return
			}
			slog.Warn("forward connection error", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "error", err)
		}
	}
}
```

- [ ] **Step 2: Update ForwardManager.onPublish**

```go
func (fm *ForwardManager) onPublish(ctx *core.EventContext) error {
	stream, ok := fm.hub.Find(ctx.StreamKey)
	if !ok {
		return nil
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()

	if _, exists := fm.active[ctx.StreamKey]; exists {
		return nil
	}

	targets, err := fm.scheduler.Resolve("forward", ctx.StreamKey)
	if err != nil {
		slog.Warn("forward schedule resolve failed", "module", "cluster",
			"stream", ctx.StreamKey, "error", err)
		return nil
	}

	var fts []*ForwardTarget
	for _, targetURL := range targets {
		transport, err := fm.registry.Resolve(targetURL)
		if err != nil {
			slog.Warn("unsupported relay protocol", "module", "cluster",
				"url", targetURL, "error", err)
			continue
		}

		ft := NewForwardTarget(ctx.StreamKey, targetURL, stream, transport, fm.retryMax, fm.retryDel)
		fts = append(fts, ft)
		go ft.Run()
	}

	if len(fts) > 0 {
		fm.active[ctx.StreamKey] = fts
		fm.eventBus.Emit(core.EventForwardStart, &core.EventContext{
			StreamKey: ctx.StreamKey,
			Extra:     map[string]any{"target_count": len(fts)},
		})
	}

	return nil
}
```

- [ ] **Step 3: Update module_test.go**

Update `NewForwardManager` calls in tests to pass a `*TransportRegistry` with an RTMPTransport registered. For example:

```go
registry := NewTransportRegistry()
registry.Register(NewRTMPTransport())
fm := NewForwardManager(hub, bus, scheduler, registry, retryMax, retryDelay)
```

- [ ] **Step 4: Remove containsStreamPath and related helpers**

Delete `containsStreamPath`, `findSchemeEnd`, `splitPath` from `forward.go` and `TestContainsStreamPath` from `module_test.go`.

- [ ] **Step 5: Run all cluster tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/forward.go module/cluster/module_test.go
git commit -m "refactor: ForwardManager uses TransportRegistry instead of direct RTMP"
```

---

### Task 6: Refactor OriginManager to Use Registry

**Files:**
- Modify: `module/cluster/origin.go` — replace direct RTMP calls with registry
- Modify: `module/cluster/module_test.go` — update NewOriginManager calls

- [ ] **Step 1: Update OriginManager and OriginPull**

1. Add `registry *TransportRegistry` field to `OriginManager`
2. Update `NewOriginManager` to accept `*TransportRegistry`
3. Update `OriginPull` to hold `registry *TransportRegistry` instead of `servers []string`
4. Replace `pullOnce(url)` body with registry resolve + `transport.Pull(ctx, ...)`
5. URL construction: for origin servers, append stream key to base URL

Key changes in `OriginPull.pullOnce()`:
```go
func (op *OriginPull) pullOnce(sourceURL string) error {
	transport, err := op.registry.Resolve(sourceURL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-op.closed:
			cancel()
		case <-ctx.Done():
		}
	}()

	return transport.Pull(ctx, sourceURL, op.stream)
}
```

- [ ] **Step 2: Update tests in module_test.go**

Update `NewOriginManager` calls to pass registry. Update `NewOriginPull` calls similarly.

- [ ] **Step 3: Run all cluster tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 4: Commit**

```bash
git add module/cluster/origin.go module/cluster/module_test.go
git commit -m "refactor: OriginManager uses TransportRegistry instead of direct RTMP"
```

---

### Task 7: Update Module Init + Wiring

**Files:**
- Modify: `module/cluster/module.go` — create registry, register RTMP, pass to managers

- [ ] **Step 1: Update Module struct and Init**

```go
type Module struct {
	forward  *ForwardManager
	origin   *OriginManager
	registry *TransportRegistry
}

func (m *Module) Init(s *core.Server) error {
	cfg := s.Config().Cluster
	hub := s.StreamHub()
	bus := s.GetEventBus()

	m.registry = NewTransportRegistry()
	m.registry.Register(NewRTMPTransport())
	// SRT, RTSP, RTP transports will be added in Phase 2 and 3

	if cfg.Forward.Enabled && (len(cfg.Forward.Targets) > 0 || cfg.Forward.ScheduleURL != "") {
		fwdScheduler := NewScheduler(
			cfg.Forward.ScheduleURL, cfg.Forward.Targets,
			cfg.Forward.SchedulePriority, cfg.Forward.ScheduleTimeout,
		)
		m.forward = NewForwardManager(hub, bus, fwdScheduler, m.registry,
			cfg.Forward.RetryMax, cfg.Forward.RetryInterval)
		slog.Info("cluster forward enabled", "module", "cluster",
			"static_targets", len(cfg.Forward.Targets),
			"schedule_url", cfg.Forward.ScheduleURL)
	}

	if cfg.Origin.Enabled && (len(cfg.Origin.Servers) > 0 || cfg.Origin.ScheduleURL != "") {
		origScheduler := NewScheduler(
			cfg.Origin.ScheduleURL, cfg.Origin.Servers,
			cfg.Origin.SchedulePriority, cfg.Origin.ScheduleTimeout,
		)
		m.origin = NewOriginManager(hub, bus, origScheduler, m.registry,
			cfg.Origin.RetryMax, cfg.Origin.RetryDelay, cfg.Origin.IdleTimeout)
		slog.Info("cluster origin pull enabled", "module", "cluster",
			"static_servers", len(cfg.Origin.Servers),
			"schedule_url", cfg.Origin.ScheduleURL)
	}

	return nil
}

func (m *Module) Close() error {
	if m.forward != nil {
		m.forward.Close()
	}
	if m.origin != nil {
		m.origin.Close()
	}
	if m.registry != nil {
		m.registry.Close()
	}
	return nil
}
```

- [ ] **Step 2: Run all cluster tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 3: Run full test suite**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./... 2>&1 | tail -30`
Expected: All PASS

- [ ] **Step 4: Commit**

```bash
git add module/cluster/module.go
git commit -m "feat: wire TransportRegistry into cluster Module init"
```

---

## Phase 2: SRT + RTSP Transports

### Task 8: SRTTransport Plugin

**Files:**
- Create: `module/cluster/transport_srt.go`
- Create: `module/cluster/transport_srt_test.go`

- [ ] **Step 1: Write SRTTransport test**

```go
// module/cluster/transport_srt_test.go
package cluster

import "testing"

func TestSRTTransportScheme(t *testing.T) {
	cfg := defaultClusterSRTConfig()
	tr := NewSRTTransport(cfg)
	if tr.Scheme() != "srt" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "srt")
	}
}

func TestSRTTransportPushBadURL(t *testing.T) {
	cfg := defaultClusterSRTConfig()
	tr := NewSRTTransport(cfg)
	defer tr.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	ctx := context.Background()
	err := tr.Push(ctx, "srt://127.0.0.1:19999/live/test", stream)
	// Should fail to connect (no SRT server at :19999)
	if err == nil {
		t.Error("expected error for connection to non-existent server")
	}
}

func TestSRTTransportRoundTrip(t *testing.T) {
	// Start a local SRT listener using gosrt on loopback
	// Push: publish test frames (H264 keyframe + AAC) → SRT → TS mux → listener
	// Pull: SRT dial → TS demux → verify AVFrame codec, DTS, payload match originals
	// This verifies the TS mux/demux round-trip is lossless
	//
	// Use gosrt.Listen on 127.0.0.1:0 for dynamic port allocation.
	// Sender goroutine: stream.WriteFrame() → SRTTransport.Push()
	// Receiver goroutine: SRTTransport.Pull() → verify stream has frames
}
```

- [ ] **Step 2: Write SRTTransport implementation**

`SRTTransport.Push()` flow:
1. Parse SRT URL: extract host:port, stream path (from `streamid` query param or path)
2. `gosrt.Dial()` with configured latency/passphrase
3. Create `ts.NewMuxer()` from stream's publisher MediaInfo
4. Send sequence headers first via TS mux
5. Read from ring buffer → TS mux → SRT write
6. Respect ctx.Done()

`SRTTransport.Pull()` flow:
1. Parse SRT URL, `gosrt.Dial()` in caller mode
2. Create `ts.NewDemuxer()` with callback writing to `stream.WriteFrame()`
3. Read SRT → feed to demuxer
4. Respect ctx.Done()

Reuse existing `pkg/muxer/ts/` muxer/demuxer, same pattern as `module/srt/publisher.go` and `module/srt/subscriber.go`.

- [ ] **Step 3: Run tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestSRTTransport -v`
Expected: PASS

- [ ] **Step 4: Register in module.go**

Add `m.registry.Register(NewSRTTransport(cfg.SRT))` in `Module.Init()`.

- [ ] **Step 5: Run all tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/transport_srt.go module/cluster/transport_srt_test.go module/cluster/module.go
git commit -m "feat: add SRTTransport plugin for cluster relay"
```

---

### Task 9: RTSPTransport Plugin

**Files:**
- Create: `module/cluster/transport_rtsp.go`
- Create: `module/cluster/transport_rtsp_test.go`

- [ ] **Step 1: Write RTSPTransport test**

```go
// module/cluster/transport_rtsp_test.go
package cluster

import "testing"

func TestRTSPTransportScheme(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	tr := NewRTSPTransport(cfg)
	if tr.Scheme() != "rtsp" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "rtsp")
	}
}

func TestRTSPTransportPushBadURL(t *testing.T) {
	cfg := defaultClusterRTSPConfig()
	tr := NewRTSPTransport(cfg)
	defer tr.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	ctx := context.Background()
	err := tr.Push(ctx, "rtsp://127.0.0.1:19999/live/test", stream)
	if err == nil {
		t.Error("expected error for connection to non-existent server")
	}
}

func TestRTSPTransportRoundTrip(t *testing.T) {
	// Start a mock RTSP server on loopback:
	// - Accept ANNOUNCE → parse SDP → SETUP → RECORD → read interleaved RTP
	// - Accept DESCRIBE → return SDP → SETUP → PLAY → send interleaved RTP
	//
	// Push: publish test frames → RTSPTransport.Push() → RTP pack → interleaved → mock server
	// Pull: mock server sends RTP → RTSPTransport.Pull() → depack → verify AVFrame matches
	//
	// This verifies the RTP pack/depack round-trip through RTSP interleaved framing.
}
```

- [ ] **Step 2: Write RTSPTransport implementation**

`RTSPTransport.Push()` flow:
1. Parse RTSP URL (host:port, path)
2. TCP connect
3. ANNOUNCE with SDP from `stream.Publisher().MediaInfo()`
4. SETUP for each media track (using configured transport mode: TCP interleaved or UDP)
5. RECORD
6. Read from ring buffer → RTP pack via `pkg/rtp/` → interleaved write
7. Respect ctx.Done()

`RTSPTransport.Pull()` flow:
1. Parse RTSP URL, TCP connect
2. DESCRIBE → parse SDP response
3. SETUP for each track
4. PLAY
5. Read interleaved RTP → depacketize via `pkg/rtp/` → `stream.WriteFrame()`
6. Respect ctx.Done()

Reuse `module/rtsp/` patterns for RTSP message formatting, `pkg/rtp/` for RTP pack/depack, `pkg/sdp/` for SDP generation.

- [ ] **Step 3: Run tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestRTSPTransport -v`
Expected: PASS

- [ ] **Step 4: Register in module.go**

Add `m.registry.Register(NewRTSPTransport(cfg.RTSP))` in `Module.Init()`.

- [ ] **Step 5: Run all tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/transport_rtsp.go module/cluster/transport_rtsp_test.go module/cluster/module.go
git commit -m "feat: add RTSPTransport plugin for cluster relay"
```

---

## Phase 3: RTP Direct Transport

### Task 10: RTPTransport Plugin + SDP Signaling

**Files:**
- Create: `module/cluster/transport_rtp.go`
- Create: `module/cluster/transport_rtp_test.go`

- [ ] **Step 1: Write RTPTransport tests (scheme, port allocator, auth, RTCP BYE, 406)**

```go
// module/cluster/transport_rtp_test.go
package cluster

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRTPTransportScheme(t *testing.T) {
	cfg := defaultClusterRTPConfig()
	srv := newTestServer()
	tr := NewRTPTransport(cfg, srv)
	defer tr.Close()

	if tr.Scheme() != "rtp" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "rtp")
	}
}

func TestPortAllocator(t *testing.T) {
	pa, err := NewPortAllocator("20000-20005")
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	// Should have 6 ports available (20000-20005 inclusive)
	var ports []int
	for i := 0; i < 6; i++ {
		p, err := pa.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
		ports = append(ports, p)
		if p < 20000 || p > 20005 {
			t.Errorf("port %d out of range", p)
		}
	}

	// Should be exhausted now
	_, err = pa.Allocate()
	if err == nil {
		t.Error("expected error when port range exhausted")
	}

	// Free one port, should be allocatable again
	pa.Free(ports[0])
	p, err := pa.Allocate()
	if err != nil {
		t.Fatalf("Allocate after free: %v", err)
	}
	if p != ports[0] {
		t.Errorf("expected freed port %d, got %d", ports[0], p)
	}
}

func TestPortAllocatorBadRange(t *testing.T) {
	_, err := NewPortAllocator("invalid")
	if err == nil {
		t.Error("expected error for invalid port range")
	}

	_, err = NewPortAllocator("20005-20000") // inverted
	if err == nil {
		t.Error("expected error for inverted port range")
	}
}

func TestRTPSignalingAuthRequired(t *testing.T) {
	// Verify that unauthenticated POST to /api/relay/push returns 401/403
	// when API auth is configured. Use httptest.Server with the signaling handler
	// and verify that requests without bearer token are rejected.
}

func TestRTPSignaling406OnCodecMismatch(t *testing.T) {
	// POST an SDP offer with an unsupported codec (e.g., VP9 when only H264
	// depacketizers exist). Verify the signaling endpoint returns HTTP 406
	// and the caller wraps it as ErrCodecMismatch.
}

func TestRTPTransportClosesendsRTCPBYE(t *testing.T) {
	// Set up an active RTP session between two loopback UDP sockets.
	// Call Close() on the sender transport.
	// Read from the receiver socket and verify that an RTCP BYE packet
	// (packet type 203) is received before the socket closes.
}
```

- [ ] **Step 2: Write RTPTransport implementation**

The RTPTransport is the most complex plugin. Key components:

**SDP signaling handlers** (registered on API server at init, behind existing API auth middleware):
- `POST {signaling_path}/push` — receive SDP offer, allocate UDP port, return SDP answer
- `POST {signaling_path}/pull` — receive SDP offer, start sending RTP, return SDP answer
- Return HTTP 406 Not Acceptable if no offered codecs are supported → caller wraps as `ErrCodecMismatch`
- Unauthenticated requests rejected (401/403) per existing API auth config

**Port allocator**: Parse `port_range` (e.g. "20000-20100"), allocate/free UDP ports.

**Push flow**:
1. Build SDP offer from `stream.Publisher().MediaInfo()` (nil-guard)
2. HTTP POST to target's `{signaling_path}/push`
3. Parse SDP answer → target UDP address
4. Create RTP sessions (`pkg/rtp/session.go`) + packetizers (`pkg/rtp/packetizer.go`)
5. Read from ring buffer → RTP pack → UDP send
6. RTCP SR goroutine every `rtcp_interval`
7. RTCP RR monitor — 3 missed intervals = stop
8. On close: send RTCP BYE, release UDP socket

**Pull flow**:
1. HTTP POST SDP offer to source's `{signaling_path}/pull`
2. Parse SDP answer → source SSRC/codec info
3. Allocate local UDP port, listen for RTP
4. RTP depacketize → `stream.WriteFrame()` (false return silently dropped)
5. RTCP RR goroutine every `rtcp_interval`
6. No RTP/SR for 3 intervals = stop
7. On close: send RTCP BYE

- [ ] **Step 3: Write loopback integration test**

```go
func TestRTPTransportLoopback(t *testing.T) {
	// Simulates two nodes: node1 pushes RTP to node2 via SDP signaling.
	hub1, _ := newTestHub()
	hub2, _ := newTestHub()

	// Node2: create RTPTransport with signaling handler
	cfg2 := defaultClusterRTPConfig()
	srv2 := newTestServer()
	tr2 := NewRTPTransport(cfg2, srv2)
	defer tr2.Close()

	// Start httptest.Server exposing node2's signaling handlers
	mux := http.NewServeMux()
	tr2.RegisterHandlers(mux) // register /api/relay/push and /api/relay/pull
	signalingServer := httptest.NewServer(mux)
	defer signalingServer.Close()

	// Node1: create stream with test frames
	stream1, _ := hub1.GetOrCreate("live/rtptest")
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC,
	}}
	stream1.SetPublisher(pub)
	stream1.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	})

	// Node1: push to node2 via RTP signaling URL
	cfg1 := defaultClusterRTPConfig()
	tr1 := NewRTPTransport(cfg1, newTestServer())
	defer tr1.Close()

	// Node2: prepare to receive into a stream
	stream2, _ := hub2.GetOrCreate("live/rtptest")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Push frames in background
	go func() {
		time.Sleep(100 * time.Millisecond)
		for i := 0; i < 10; i++ {
			stream1.WriteFrame(&avframe.AVFrame{
				MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264,
				FrameType: avframe.FrameTypeKeyframe,
				DTS: int64((i + 1) * 33), PTS: int64((i + 1) * 33),
				Payload: []byte{0x65, 0x88, 0x00, 0x01},
			})
			time.Sleep(33 * time.Millisecond)
		}
		cancel()
	}()

	// Push from node1 to node2
	signalingURL := "rtp://" + signalingServer.Listener.Addr().String() + "/live/rtptest"
	err := tr1.Push(ctx, signalingURL, stream1)
	// nil or context cancelled is expected
	_ = err

	// Verify frames arrived on node2
	if stream2.VideoSeqHeader() == nil {
		t.Error("expected video sequence header on node2")
	}
}
```

- [ ] **Step 4: Register in module.go**

Add `m.registry.Register(NewRTPTransport(cfg.RTP, s))` in `Module.Init()`.

- [ ] **Step 5: Run all tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/transport_rtp.go module/cluster/transport_rtp_test.go module/cluster/module.go
git commit -m "feat: add RTPTransport plugin with SDP signaling for direct relay"
```

---

### Task 11: Update Default Config + Sample Config

**Files:**
- Modify: `configs/` — update sample YAML with new cluster transport options

- [ ] **Step 1: Update sample config**

Add the new cluster transport sections to the sample YAML config file in `configs/`.

- [ ] **Step 2: Verify config loads**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./config/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add configs/
git commit -m "docs: add SRT, RTSP, RTP cluster transport options to sample config"
```

---

### Task 12: Prometheus Metrics for Cluster Relay

**Files:**
- Create: `module/cluster/relay_metrics.go`
- Create: `module/cluster/relay_metrics_test.go`
- Modify: `module/cluster/transport.go` — add optional metrics hook to interface or wrap transports

- [ ] **Step 1: Write metrics test**

```go
// module/cluster/relay_metrics_test.go
package cluster

import "testing"

func TestRelayMetricsIncrement(t *testing.T) {
	m := NewRelayMetrics()

	m.RecordPush("rtmp", 1024, nil)
	m.RecordPush("srt", 512, fmt.Errorf("connection refused"))

	// Verify counters via prometheus client_golang testutil
	// cluster_relay_bytes_total{direction="forward",protocol="rtmp"} == 1024
	// cluster_relay_errors_total{direction="forward",protocol="srt"} == 1
}

func TestRelayMetricsActive(t *testing.T) {
	m := NewRelayMetrics()
	m.SetActive("forward", "rtmp", 2)
	m.SetActive("origin", "srt", 1)

	// cluster_relay_active{direction="forward",protocol="rtmp"} == 2
	// cluster_relay_active{direction="origin",protocol="srt"} == 1
}
```

- [ ] **Step 2: Implement RelayMetrics**

Define Prometheus metrics per spec:
- `cluster_relay_active` (Gauge) — labels: direction, protocol
- `cluster_relay_errors_total` (Counter) — labels: direction, protocol, error_type
- `cluster_relay_bytes_total` (Counter) — labels: direction, protocol
- `cluster_relay_latency_seconds` (Histogram) — labels: protocol (RTP only)
- `cluster_rtp_packet_loss_ratio` (Gauge) — labels: stream, direction

Register metrics with the existing Prometheus registry. Instrument ForwardManager/OriginManager to call metrics on transport start/stop/error/bytes.

- [ ] **Step 3: Run tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -run TestRelayMetrics -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add module/cluster/relay_metrics.go module/cluster/relay_metrics_test.go
git commit -m "feat: add Prometheus metrics for cluster relay transports"
```

---

### Task 13: Integration Tests

**Files:**
- Modify: `module/cluster/integration_test.go` — update existing tests, add multi-protocol tests

- [ ] **Step 1: Update existing integration tests**

Update `TestForwardToMockRTMPServer` and `TestOriginPullFromMockServer` to work with the new `ForwardTarget`/`OriginPull` signatures (transport-based).

- [ ] **Step 2: Add multi-protocol forward test**

Test that ForwardManager correctly routes to different transports based on URL scheme:
```go
func TestForwardManagerMultiProtocol(t *testing.T) {
	// Register mock transports for "mock1" and "mock2" schemes
	// Configure targets with mixed schemes
	// Verify each target resolved to correct transport
}
```

- [ ] **Step 3: Run all tests**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./module/cluster/ -v`
Expected: All PASS

- [ ] **Step 4: Run full test suite**

Run: `cd /Users/pingo-macmini/Documents/liveforge && go test -race ./... 2>&1 | tail -30`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add module/cluster/integration_test.go
git commit -m "test: update integration tests for multi-protocol relay"
```

---

### Task 14: Update PROGRESS.md

**Files:**
- Modify: `docs/PROGRESS.md`

- [ ] **Step 1: Add Phase 11 — Multi-Protocol Cluster Relay section**

Add a new section documenting the completed multi-protocol relay feature with:
- All new transport plugins
- TransportRegistry architecture
- Config changes
- Test coverage

- [ ] **Step 2: Commit**

```bash
git add docs/PROGRESS.md
git commit -m "docs: add Phase 11 multi-protocol cluster relay to PROGRESS.md"
```
