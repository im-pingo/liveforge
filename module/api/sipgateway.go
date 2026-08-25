package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/im-pingo/liveforge/module/sipgateway"
)

type sipGatewayCallsResponse struct {
	Calls   []sipgateway.CallSnapshot  `json:"calls"`
	Metrics sipgateway.MetricsSnapshot `json:"metrics"`
}

type sipGatewayDialRequest struct {
	TargetURI string `json:"target_uri"`
	StreamKey string `json:"stream_key"`
}

func (h *Handlers) sipGatewayProvider() (sipgateway.SIPGatewayProvider, bool) {
	module := h.server.ModuleByName("sipgateway")
	provider, ok := module.(sipgateway.SIPGatewayProvider)
	return provider, ok
}

func (h *Handlers) handleSIPGatewayCalls(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.sipGatewayProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SIP gateway unavailable")
		return
	}
	writeJSON(w, http.StatusOK, sipGatewayCallsResponse{Calls: provider.ListCalls(), Metrics: provider.Metrics()})
}

func (h *Handlers) handleSIPGatewayDial(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.sipGatewayProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SIP gateway unavailable")
		return
	}
	var input sipGatewayDialRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	callID, err := provider.Dial(r.Context(), input.TargetURI, input.StreamKey)
	if err != nil {
		writeSIPGatewayError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"call_id": callID})
}

func (h *Handlers) handleSIPGatewayCall(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.sipGatewayProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SIP gateway unavailable")
		return
	}
	callID := r.PathValue("call_id")
	if callID == "" {
		writeError(w, http.StatusBadRequest, "missing call ID")
		return
	}
	if r.Method == http.MethodDelete {
		if err := provider.Hangup(callID); err != nil {
			writeSIPGatewayError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, nil)
		return
	}
	call, found := provider.Call(callID)
	if !found {
		writeError(w, http.StatusNotFound, "call not found")
		return
	}
	writeJSON(w, http.StatusOK, call)
}

func writeSIPGatewayError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sipgateway.ErrCallNotFound), errors.Is(err, sipgateway.ErrStreamNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, sipgateway.ErrCallCapacity), errors.Is(err, sipgateway.ErrPortExhausted):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, sipgateway.ErrInvalidTargetURI):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sipgateway.ErrTargetRequired), errors.Is(err, sipgateway.ErrCodecMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, sipgateway.ErrGatewayDisabled), errors.Is(err, sipgateway.ErrGatewayClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusBadGateway, "SIP call setup failed")
	}
}
