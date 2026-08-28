package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/sipgateway"
)

type sipGatewayStub struct {
	calls   map[string]sipgateway.CallSnapshot
	dialed  string
	dialErr error
	hungup  string
}

func (m *sipGatewayStub) Name() string                   { return "sipgateway" }
func (m *sipGatewayStub) Init(*core.Server) error        { return nil }
func (m *sipGatewayStub) Hooks() []core.HookRegistration { return nil }
func (m *sipGatewayStub) Close() error                   { return nil }
func (m *sipGatewayStub) ListCalls() []sipgateway.CallSnapshot {
	out := make([]sipgateway.CallSnapshot, 0, len(m.calls))
	for _, call := range m.calls {
		out = append(out, call)
	}
	return out
}
func (m *sipGatewayStub) Call(id string) (sipgateway.CallSnapshot, bool) {
	call, ok := m.calls[id]
	return call, ok
}
func (m *sipGatewayStub) Dial(_ context.Context, target, stream string) (string, error) {
	m.dialed = target + "|" + stream
	if m.dialErr != nil {
		return "", m.dialErr
	}
	return "outbound-1", nil
}
func (m *sipGatewayStub) Hangup(id string) error { m.hungup = id; delete(m.calls, id); return nil }
func (m *sipGatewayStub) StartLabSession(context.Context, sipgateway.LabSessionRequest) (sipgateway.LabSessionSnapshot, error) {
	return sipgateway.LabSessionSnapshot{}, nil
}
func (m *sipGatewayStub) ListLabSessions() []sipgateway.LabSessionSnapshot { return nil }
func (m *sipGatewayStub) StopLabSession(string) error                      { return nil }
func (m *sipGatewayStub) Metrics() sipgateway.MetricsSnapshot {
	return sipgateway.MetricsSnapshot{ActiveCalls: len(m.calls)}
}

func TestSIPGatewayControlPlaneHandlers(t *testing.T) {
	cfg := newTestConfig()
	server := core.NewServer(cfg)
	stub := &sipGatewayStub{calls: map[string]sipgateway.CallSnapshot{"call-1": {CallID: "call-1", StreamKey: "live/audio", Codec: "opus"}}}
	server.RegisterModule(stub)
	h := NewHandlers(server)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sipgateway/calls", nil)
	w := httptest.NewRecorder()
	h.handleSIPGatewayCalls(w, req)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("call-1")) {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}

	body, _ := json.Marshal(map[string]string{"target_uri": "1001", "stream_key": "live/audio"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sipgateway/calls", bytes.NewReader(body))
	w = httptest.NewRecorder()
	h.handleSIPGatewayDial(w, req)
	if w.Code != http.StatusCreated || stub.dialed != "1001|live/audio" {
		t.Fatalf("dial status=%d called=%q body=%s", w.Code, stub.dialed, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/sipgateway/calls/call-1", nil)
	req.SetPathValue("call_id", "call-1")
	w = httptest.NewRecorder()
	h.handleSIPGatewayCall(w, req)
	if w.Code != http.StatusOK || stub.hungup != "call-1" {
		t.Fatalf("hangup status=%d id=%q", w.Code, stub.hungup)
	}
}

func TestSIPGatewayMissingModuleAndCall(t *testing.T) {
	h, _ := newTestHandlers(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sipgateway/calls", nil)
	w := httptest.NewRecorder()
	h.handleSIPGatewayCalls(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing module status=%d", w.Code)
	}
}

func TestSIPGatewayMalformedTargetIsBadRequest(t *testing.T) {
	cfg := newTestConfig()
	server := core.NewServer(cfg)
	stub := &sipGatewayStub{dialErr: sipgateway.ErrInvalidTargetURI}
	server.RegisterModule(stub)
	h := NewHandlers(server)

	for _, target := range []string{"sip:alice\r\nInjected@example.com", "sip:alice@example.com:0"} {
		t.Run(target, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"target_uri": target, "stream_key": "live/audio"})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sipgateway/calls", bytes.NewReader(body))
			w := httptest.NewRecorder()

			h.handleSIPGatewayDial(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("malformed target status = %d body=%s, want 400", w.Code, w.Body.String())
			}
		})
	}
}
