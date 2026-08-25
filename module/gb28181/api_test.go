package gb28181

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	apiModule "github.com/im-pingo/liveforge/module/api"
)

func TestChannelLiveAndPlaybackDeleteRoutesReachHandlers(t *testing.T) {
	server := core.NewServer(config.Defaults())
	module := &Module{sessions: NewSessionManager()}
	registerAPI(server, module)
	mux := http.NewServeMux()
	apiModule.RegisterRoutes(mux, server)

	for _, action := range []string{"play", "playback"} {
		t.Run(action, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, apiPrefix+"/channels/channel-1/"+action, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no active session for channel") {
				t.Fatalf("DELETE %s status=%d body=%q; handler was not reached", action, w.Code, w.Body.String())
			}
		})
	}
}
