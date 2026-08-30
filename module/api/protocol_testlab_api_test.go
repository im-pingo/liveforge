package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/im-pingo/liveforge/core"
	gb28181mod "github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/module/sipgateway"
	"github.com/im-pingo/liveforge/pkg/protocoltest"
)

type protocolRunnerModule struct {
	name string
}

func (m *protocolRunnerModule) Name() string                   { return m.name }
func (m *protocolRunnerModule) Init(*core.Server) error        { return nil }
func (m *protocolRunnerModule) Hooks() []core.HookRegistration { return nil }
func (m *protocolRunnerModule) Close() error                   { return nil }
func (m *protocolRunnerModule) RunSelfTest(context.Context) (protocoltest.Report, error) {
	return protocoltest.New(m.name, []protocoltest.Check{{Name: "route", Passed: true}}), nil
}

func TestSIPGatewaySelfTestRouteReturnsReport(t *testing.T) {
	server := core.NewServer(newTestConfig())
	server.RegisterModule(&protocolRunnerModule{name: "sipgateway"})
	mux := http.NewServeMux()
	RegisterRoutes(mux, server)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sipgateway/test", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data protocoltest.Report `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Protocol != "sipgateway" || !response.Data.Passed {
		t.Fatalf("response=%+v", response.Data)
	}
}

func TestSIPGatewaySelfTestRouteReportsUnavailableWithoutModule(t *testing.T) {
	server := core.NewServer(newTestConfig())
	mux := http.NewServeMux()
	RegisterRoutes(mux, server)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sipgateway/test", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type protocolLabSIPStub struct {
	sessions []sipgateway.LabSessionSnapshot
	started  sipgateway.LabSessionRequest
	stopped  string
	err      error
}

func (m *protocolLabSIPStub) Name() string                         { return "sipgateway" }
func (m *protocolLabSIPStub) Init(*core.Server) error              { return nil }
func (m *protocolLabSIPStub) Hooks() []core.HookRegistration       { return nil }
func (m *protocolLabSIPStub) Close() error                         { return nil }
func (m *protocolLabSIPStub) ListCalls() []sipgateway.CallSnapshot { return nil }
func (m *protocolLabSIPStub) Call(string) (sipgateway.CallSnapshot, bool) {
	return sipgateway.CallSnapshot{}, false
}
func (m *protocolLabSIPStub) Dial(context.Context, string, string) (string, error) { return "", nil }
func (m *protocolLabSIPStub) Hangup(string) error                                  { return nil }
func (m *protocolLabSIPStub) StartLabSession(_ context.Context, request sipgateway.LabSessionRequest) (sipgateway.LabSessionSnapshot, error) {
	m.started = request
	if m.err != nil {
		return sipgateway.LabSessionSnapshot{}, m.err
	}
	snapshot := sipgateway.LabSessionSnapshot{ID: "sip-lab-1", DeviceID: request.DeviceID, StreamKey: request.StreamKey, Mode: request.Mode, State: sipgateway.LabSessionStateActive}
	m.sessions = []sipgateway.LabSessionSnapshot{snapshot}
	return snapshot, nil
}
func (m *protocolLabSIPStub) ListLabSessions() []sipgateway.LabSessionSnapshot {
	return append([]sipgateway.LabSessionSnapshot(nil), m.sessions...)
}
func (m *protocolLabSIPStub) StopLabSession(id string) error { m.stopped = id; return m.err }
func (m *protocolLabSIPStub) Metrics() sipgateway.MetricsSnapshot {
	return sipgateway.MetricsSnapshot{}
}

type protocolLabGBStub struct {
	sessions []gb28181mod.LabSessionSnapshot
	started  gb28181mod.LabSessionRequest
	stopped  string
	err      error
}

func (m *protocolLabGBStub) Name() string                   { return "gb28181" }
func (m *protocolLabGBStub) Init(*core.Server) error        { return nil }
func (m *protocolLabGBStub) Hooks() []core.HookRegistration { return nil }
func (m *protocolLabGBStub) Close() error                   { return nil }
func (m *protocolLabGBStub) StartLabSession(_ context.Context, request gb28181mod.LabSessionRequest) (gb28181mod.LabSessionSnapshot, error) {
	m.started = request
	if m.err != nil {
		return gb28181mod.LabSessionSnapshot{}, m.err
	}
	snapshot := gb28181mod.LabSessionSnapshot{ID: "gb-lab-1", DeviceID: request.DeviceID, ChannelID: request.ChannelID, StreamKey: request.StreamKey, Mode: request.Mode, State: gb28181mod.LabSessionStateActive}
	m.sessions = []gb28181mod.LabSessionSnapshot{snapshot}
	return snapshot, nil
}
func (m *protocolLabGBStub) ListLabSessions() []gb28181mod.LabSessionSnapshot {
	return append([]gb28181mod.LabSessionSnapshot(nil), m.sessions...)
}
func (m *protocolLabGBStub) StopLabSession(id string) error { m.stopped = id; return m.err }

func decodeManagementData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	return response.Data
}

func TestSIPLabAPIListsSessionsAndPlaybackMetadata(t *testing.T) {
	cfg := newTestConfig()
	cfg.HTTP.Enabled = true
	cfg.RTMP.Enabled = true
	cfg.RTSP.Enabled = true
	cfg.WebRTC.Enabled = true
	server := core.NewServer(cfg)
	stub := &protocolLabSIPStub{sessions: []sipgateway.LabSessionSnapshot{{ID: "sip-lab-1", StreamKey: "sip/lab", State: sipgateway.LabSessionStateFailed, LastError: "INVITE rejected"}}}
	server.RegisterModule(stub)
	mux := http.NewServeMux()
	RegisterRoutes(mux, server)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sipgateway/lab/sessions", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	data := decodeManagementData(t, recorder)
	if _, ok := data["sessions"]; !ok {
		t.Fatalf("list data=%v, want sessions", data)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"stream_key":"sip/lab"`)) {
		t.Fatalf("list body=%s, want stream key", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"last_error":"INVITE rejected"`)) {
		t.Fatalf("list body=%s, want terminal error", recorder.Body.String())
	}
	for _, field := range []string{"http_flv", "hls", "dash", "whep"} {
		if !bytes.Contains(recorder.Body.Bytes(), []byte(`"`+field+`"`)) {
			t.Fatalf("list body=%s, want playback field %q", recorder.Body.String(), field)
		}
	}
}

func TestProtocolLabPlaybackEscapesStreamPathAndUsesBoundWildcardListeners(t *testing.T) {
	cfg := newTestConfig()
	cfg.HTTP.Enabled = true
	cfg.RTMP.Enabled = true
	cfg.RTMP.Listen = "127.0.0.1:1935"
	cfg.RTSP.Enabled = true
	cfg.RTSP.Listen = "127.0.0.1:8554"
	cfg.WebRTC.Enabled = true
	server := core.NewServer(cfg)
	const streamKey = "tenant/cam?variant#one%raw"
	server.RegisterModule(&protocolLabSIPStub{sessions: []sipgateway.LabSessionSnapshot{{
		ID: "escaped-playback", StreamKey: streamKey, State: sipgateway.LabSessionStateActive,
	}}})
	server.RegisterModule(&endpointTestModule{name: "rtmp", addr: &net.TCPAddr{IP: net.IPv4zero, Port: 41935}})
	server.RegisterModule(&endpointTestModule{name: "rtsp", addr: &net.TCPAddr{IP: net.IPv6unspecified, Port: 48554}})
	mux := http.NewServeMux()
	RegisterRoutes(mux, server)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/sipgateway/lab/sessions", nil)
	request.Host = "console.example:7443"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	data := decodeManagementData(t, recorder)
	sessions, ok := data["sessions"].([]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("sessions=%v, want one", data["sessions"])
	}
	view := sessions[0].(map[string]any)
	playback := view["playback"].(map[string]any)
	escaped := "tenant/cam%3Fvariant%23one%25raw"
	want := map[string]string{
		"rtmp":          "rtmp://console.example:41935/" + escaped,
		"rtsp":          "rtsp://console.example:48554/" + escaped,
		"http_flv":      "/" + escaped + ".flv",
		"ws_flv":        "/ws/" + escaped + ".flv",
		"whep":          "/webrtc/whep/" + escaped + "?mode=live",
		"whep_live":     "/webrtc/whep/" + escaped + "?mode=live",
		"whep_realtime": "/webrtc/whep/" + escaped + "?mode=realtime",
	}
	for field, expected := range want {
		if playback[field] != expected {
			t.Errorf("playback[%q] = %q, want %q", field, playback[field], expected)
		}
	}
}

func TestSIPLabAPIStartsAndStopsWithStrictBody(t *testing.T) {
	server := core.NewServer(newTestConfig())
	stub := &protocolLabSIPStub{}
	server.RegisterModule(stub)
	mux := http.NewServeMux()
	RegisterRoutes(mux, server)

	recorder := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"mode":"publish","device_id":"device-1","stream_key":"sip/lab","codec":"PCMA"}`)
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/sipgateway/lab/sessions", body))
	if recorder.Code != http.StatusCreated || stub.started.DeviceID != "device-1" {
		t.Fatalf("start status=%d body=%s request=%+v", recorder.Code, recorder.Body.String(), stub.started)
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/sipgateway/lab/sessions", bytes.NewBufferString(`{"mode":"publish","device_id":"device-1","stream_key":"sip/lab","unknown":true}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/sipgateway/lab/sessions/sip-lab-1", nil))
	if recorder.Code != http.StatusOK || stub.stopped != "sip-lab-1" {
		t.Fatalf("stop status=%d body=%s stopped=%q", recorder.Code, recorder.Body.String(), stub.stopped)
	}
}

func TestGBLabAPIStartsAndMapsUnavailable(t *testing.T) {
	server := core.NewServer(newTestConfig())
	stub := &protocolLabGBStub{}
	server.RegisterModule(stub)
	mux := http.NewServeMux()
	RegisterRoutes(mux, server)

	recorder := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"mode":"receive","device_id":"device-1","channel_id":"channel-1","stream_key":"gb28181/lab"}`)
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/gb28181/lab/sessions", body))
	if recorder.Code != http.StatusCreated || stub.started.ChannelID != "channel-1" {
		t.Fatalf("start status=%d body=%s request=%+v", recorder.Code, recorder.Body.String(), stub.started)
	}

	stub.err = gb28181mod.ErrLabManagerUnimplemented
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/gb28181/lab/sessions", bytes.NewBufferString(`{"mode":"publish","device_id":"device-2","channel_id":"channel-2","stream_key":"gb28181/lab-2"}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !errors.Is(stub.err, gb28181mod.ErrLabManagerUnimplemented) {
		t.Fatal("test setup error")
	}
}
