package gb28181

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
)

var errPublishUnauthorized = errors.New("gb28181 publish unauthorized")

func sipEventParams(req *sip.Request) map[string]string {
	params := make(map[string]string)
	if req.Recipient.UriParams != nil {
		if token, ok := req.Recipient.UriParams.Get("token"); ok {
			params["token"] = token
		}
	}
	if params["token"] == "" && req.Recipient.Headers != nil {
		if token, ok := req.Recipient.Headers.Get("token"); ok {
			params["token"] = token
		}
	}
	if params["token"] == "" {
		if token := req.GetHeader("token"); token != nil {
			params["token"] = token.Value()
		}
	}
	if params["token"] == "" {
		params["token"] = bearerToken(req.GetHeader("Authorization"))
	}
	return params
}

func httpEventParams(r *http.Request) map[string]string {
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
	return params
}

func bearerToken(header sip.Header) string {
	if header == nil {
		return ""
	}
	parts := strings.Fields(header.Value())
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func sipPublishContext(req *sip.Request, streamKey, publisherID string) *core.EventContext {
	return &core.EventContext{
		StreamKey:   streamKey,
		PublisherID: publisherID,
		Protocol:    "gb28181",
		RemoteAddr:  req.Source(),
		Params:      sipEventParams(req),
	}
}

func outboundPublishContext(device *Device, streamKey string, params map[string]string) *core.EventContext {
	paramsCopy := make(map[string]string, len(params))
	for key, value := range params {
		paramsCopy[key] = value
	}
	return &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "gb28181",
		RemoteAddr: device.RemoteAddr,
		Params:     paramsCopy,
	}
}

func authorizePublish(bus *core.EventBus, ctx *core.EventContext) error {
	if err := bus.EmitSync(core.EventPublish, ctx); err != nil {
		return fmt.Errorf("%w: %w", errPublishUnauthorized, err)
	}
	return nil
}
