package dvr

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestDVRModuleInitShutdownBounded(t *testing.T) {
	_, _, stop := startDVRRouteServer(t)
	stop()
}

func TestDVRMediaRoutesRealServer(t *testing.T) {
	server, module, stop := startDVRRouteServer(t)
	defer stop()

	segmentBody := "dvr-route-segment"
	segmentPath := filepath.Join(resolvePath(module.Policy().Path, "live/camera"), "seg_000001.ts")
	if err := os.MkdirAll(filepath.Dir(segmentPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segmentPath, []byte(segmentBody), 0644); err != nil {
		t.Fatal(err)
	}
	stream, err := server.StreamHub().GetOrCreate("live/camera")
	if err != nil {
		t.Fatal(err)
	}
	session, err := newSessionWithStorage("live/camera", stream, module.Policy(), nil, 0, &module.metrics, module.storage, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	session.finish()
	module.mu.Lock()
	module.sessions["live/camera"] = session
	module.mu.Unlock()

	baseURL := "http://" + module.listener.Addr().String()
	client := &http.Client{
		Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	playlist := getDVRRoute(t, client, baseURL+"/dvr/live/camera.m3u8")
	if playlist.status != http.StatusOK || !strings.Contains(playlist.body, "camera/seg_000001.ts") {
		t.Fatalf("playlist status=%d body=%q", playlist.status, playlist.body)
	}
	segment := getDVRRoute(t, client, baseURL+"/dvr/live/camera/seg_000001.ts")
	if segment.status != http.StatusOK || segment.body != segmentBody {
		t.Fatalf("segment status=%d body=%q", segment.status, segment.body)
	}

	var authorizationCalls atomic.Int64
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventSubscribe,
		Mode:  core.HookSync,
		Handler: func(*core.EventContext) error {
			authorizationCalls.Add(1)
			return errors.New("denied")
		},
	})
	for _, requestPath := range []string{
		"/dvr/live/camera.m3u",
		"/dvr/live/.m3u8",
		"/dvr/%2e%2e/camera.m3u8",
		"/dvr/live%2fevil/camera.m3u8",
		"/dvr/live/camera%2fescape.m3u8",
		"/dvr/live/camera/segment.ts",
		"/dvr/live/camera/seg_000001.ts/extra",
		"/dvr/live/camera/segments/seg_000001.ts",
		"/dvr/live/camera/%2e%2e%2fseg_000001.ts",
	} {
		response := getDVRRoute(t, client, baseURL+requestPath)
		if response.status != http.StatusNotFound {
			t.Errorf("GET %s status=%d want=%d body=%q", requestPath, response.status, http.StatusNotFound, response.body)
		}
	}
	if got := authorizationCalls.Load(); got != 0 {
		t.Fatalf("malformed routes reached authorization %d time(s)", got)
	}
}

type dvrRouteResponse struct {
	status int
	body   string
}

func getDVRRoute(t *testing.T, client *http.Client, url string) dvrRouteResponse {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read GET %s: %v", url, err)
	}
	return dvrRouteResponse{status: response.StatusCode, body: string(body)}
}

func startDVRRouteServer(t *testing.T) (*core.Server, *Module, func()) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.DrainTimeout = time.Second
	cfg.DVR.Enabled = true
	cfg.DVR.Listen = "127.0.0.1:0"
	cfg.DVR.Path = filepath.Join(t.TempDir(), "{stream_key}")
	server := core.NewServer(cfg)
	module := NewModule()
	server.RegisterModule(module)
	if err := initDVRRouteServer(server); err != nil {
		shutdownDVRRouteServer(t, server)
		t.Fatalf("core.Server.Init must not panic or fail: %v", err)
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { shutdownDVRRouteServer(t, server) })
	}
	t.Cleanup(stop)
	return server, module, stop
}

func initDVRRouteServer(server *core.Server) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return server.Init()
}

func shutdownDVRRouteServer(t *testing.T, server *core.Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		server.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("core.Server.Shutdown exceeded one second")
	}
}
