package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/cluster"
)

type clusterStatusStub struct{ status cluster.ClusterStatus }

func (m clusterStatusStub) Name() string                         { return "cluster" }
func (m clusterStatusStub) Init(*core.Server) error              { return nil }
func (m clusterStatusStub) Hooks() []core.HookRegistration       { return nil }
func (m clusterStatusStub) Close() error                         { return nil }
func (m clusterStatusStub) ClusterStatus() cluster.ClusterStatus { return m.status }

func TestClusterStatusHandler(t *testing.T) {
	server := core.NewServer(newTestConfig())
	server.RegisterModule(clusterStatusStub{status: cluster.ClusterStatus{ActiveForwards: 2}})
	h := NewHandlers(server)
	w := httptest.NewRecorder()
	h.handleClusterStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/cluster/status", nil))
	var got cluster.ClusterStatus
	if err := json.Unmarshal(decodeAPIData(t, w.Body.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || got.ActiveForwards != 2 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
