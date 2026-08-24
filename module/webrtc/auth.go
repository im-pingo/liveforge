package webrtc

import (
	"net/http"
	"strings"

	"github.com/im-pingo/liveforge/core"
)

func eventContextFromRequest(r *http.Request, streamKey string) *core.EventContext {
	params := make(map[string]string)
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}
	if params["token"] == "" {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			params["token"] = parts[1]
		}
	}
	return &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "webrtc",
		RemoteAddr: r.RemoteAddr,
		Params:     params,
	}
}

func rejectUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
