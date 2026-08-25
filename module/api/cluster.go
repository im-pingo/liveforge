package api

import (
	"net/http"

	"github.com/im-pingo/liveforge/module/cluster"
)

func (h *Handlers) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	module := h.server.ModuleByName("cluster")
	provider, ok := module.(cluster.ClusterStatusProvider)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":         false,
			"active_forwards": 0,
			"active_origins":  0,
			"relays":          []any{},
			"peers":           []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, provider.ClusterStatus())
}
