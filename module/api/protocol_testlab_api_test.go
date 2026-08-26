package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/im-pingo/liveforge/core"
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
