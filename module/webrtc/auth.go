package webrtc

import (
	"net/http"

	"github.com/im-pingo/liveforge/core"
)

func (m *Module) authorizeRequest(r *http.Request, action core.AuthorizationAction, stage core.AuthorizationStage, streamKey string) error {
	params := make(map[string]string)
	for name, values := range r.URL.Query() {
		if len(values) > 0 {
			params[name] = values[0]
		}
	}
	if len(params) == 0 {
		params = nil
	}
	return m.server.Authorize(r.Context(), core.AuthorizationRequest{
		Action: action, Stage: stage, StreamKey: streamKey,
		Protocol: "webrtc", RemoteAddr: r.RemoteAddr, Params: params,
	})
}
