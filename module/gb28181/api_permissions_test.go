package gb28181

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestRegisteredGBAPIPermissionsPreserveReadControlAndDeleteRoles(t *testing.T) {
	server := core.NewServer(config.Defaults())
	registerAPI(server, &Module{})
	mux := http.NewServeMux()
	for pattern, handler := range server.APIHandlers() {
		mux.Handle(pattern, handler)
	}
	for _, tc := range []struct {
		method, path, permission string
	}{
		{http.MethodGet, "/devices", "gb28181:read"},
		{http.MethodGet, "/devices/device-1", "gb28181:read"},
		{http.MethodDelete, "/devices/device-1", "gb28181:delete"},
		{http.MethodGet, "/channels", "gb28181:read"},
		{http.MethodPost, "/channels/channel-1/play", "gb28181:control"},
		{http.MethodDelete, "/channels/channel-1/play", "gb28181:control"},
		{http.MethodDelete, "/channels/channel-1/playback", "gb28181:control"},
		{http.MethodDelete, "/channels/channel-1/unknown", "gb28181:manage"},
		{http.MethodGet, "/sessions", "gb28181:read"},
		{http.MethodDelete, "/sessions/session-1", "gb28181:delete"},
		{http.MethodGet, "/test", "gb28181:read"},
	} {
		handler, _ := mux.Handler(httptest.NewRequest(tc.method, apiPrefix+tc.path, nil))
		declared, ok := handler.(core.APIPermissionHandler)
		if !ok || declared.APIPermission() != tc.permission {
			t.Errorf("%s %s does not declare %s", tc.method, tc.path, tc.permission)
		}
	}
}
