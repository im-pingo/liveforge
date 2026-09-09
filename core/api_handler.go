package core

import "net/http"

// APIPermissionHandler declares a registered handler's management permission
// independently from its configurable URL. The API middleware enforces it.
type APIPermissionHandler interface {
	http.Handler
	APIPermission() string
}

type permissionedAPIHandler struct {
	http.Handler
	permission string
}

func (h permissionedAPIHandler) APIPermission() string { return h.permission }

// WithAPIPermission attaches authorization metadata without changing dispatch.
func WithAPIPermission(permission string, handler http.Handler) http.Handler {
	return permissionedAPIHandler{Handler: handler, permission: permission}
}
