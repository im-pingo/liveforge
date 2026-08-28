package gb28181

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

func TestChannelLiveAndPlaybackDeleteRoutesReachHandlers(t *testing.T) {
	server := core.NewServer(config.Defaults())
	module := &Module{sessions: NewSessionManager()}
	registerAPI(server, module)
	handler := server.APIHandlers()["DELETE "+apiPrefix+"/channels/"]
	if handler == nil {
		t.Fatal("DELETE channel API handler was not registered")
	}

	for _, action := range []string{"play", "playback"} {
		t.Run(action, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, apiPrefix+"/channels/channel-1/"+action, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no active session for channel") {
				t.Fatalf("DELETE %s status=%d body=%q; handler was not reached", action, w.Code, w.Body.String())
			}
		})
	}
}

func TestSelfTestRouteReturnsLocalProtocolReport(t *testing.T) {
	ports, err := portalloc.New(42120, 42140)
	if err != nil {
		t.Fatal(err)
	}
	server := core.NewServer(config.Defaults())
	module := &Module{handler: &handler{ports: ports}}
	registerAPI(server, module)
	handler := server.APIHandlers()["GET "+apiPrefix+"/test"]
	if handler == nil {
		t.Fatal("self-test API handler was not registered")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, apiPrefix+"/test", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var report struct {
		Protocol string `json:"protocol"`
		Passed   bool   `json:"passed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Protocol != "gb28181" || !report.Passed {
		t.Fatalf("report=%+v", report)
	}
}
