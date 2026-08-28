package api

import (
	"context"
	"net/http"

	"github.com/im-pingo/liveforge/pkg/protocoltest"
)

type protocolSelfTestRunner interface {
	RunSelfTest(context.Context) (protocoltest.Report, error)
}

func (h *Handlers) handleSIPGatewaySelfTest(w http.ResponseWriter, r *http.Request) {
	module := h.server.ModuleByName("sipgateway")
	runner, ok := module.(protocolSelfTestRunner)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SIP gateway self-test unavailable")
		return
	}
	report, err := runner.RunSelfTest(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}
