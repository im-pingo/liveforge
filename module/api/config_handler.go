package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/im-pingo/liveforge/config"
	"golang.org/x/crypto/bcrypt"
)

type configPatchRequest struct {
	CurrentPassword string             `json:"current_password"`
	Server          *serverConfigPatch `json:"server,omitempty"`
	Auth            *mediaAuthPatch    `json:"auth,omitempty"`
	API             *apiConfigPatch    `json:"api,omitempty"`
}

type serverConfigPatch struct {
	Name     *string `json:"name,omitempty"`
	LogLevel *string `json:"log_level,omitempty"`
}

type mediaAuthPatch struct {
	Enabled   *bool          `json:"enabled,omitempty"`
	Publish   *authRulePatch `json:"publish,omitempty"`
	Subscribe *authRulePatch `json:"subscribe,omitempty"`
}

type authRulePatch struct {
	Mode     *string            `json:"mode,omitempty"`
	Stage    *string            `json:"stage,omitempty"`
	Token    *authTokenPatch    `json:"token,omitempty"`
	Callback *authCallbackPatch `json:"callback,omitempty"`
}

type authTokenPatch struct {
	Secret    *string `json:"secret,omitempty"`
	Algorithm *string `json:"algorithm,omitempty"`
}

type authCallbackPatch struct {
	URL     *string `json:"url,omitempty"`
	Timeout *string `json:"timeout,omitempty"`
}

type apiConfigPatch struct {
	PprofEnabled *bool               `json:"pprof_enabled,omitempty"`
	Auth         *apiAuthConfigPatch `json:"auth,omitempty"`
	Console      *consoleConfigPatch `json:"console,omitempty"`
}

type apiAuthConfigPatch struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	BearerToken *string `json:"bearer_token,omitempty"`
}

type consoleConfigPatch struct {
	Username                        *string `json:"username,omitempty"`
	AllowInsecureDefaultCredentials *bool   `json:"allow_insecure_default_credentials,omitempty"`
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *Handlers) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	setConfigResponseHeaders(w)
	updater := h.server.ConfigUpdater()
	if updater == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime configuration is unavailable")
		return
	}
	snapshot := updater.Current()
	if snapshot.Effective == nil || snapshot.Revision == "" {
		writeError(w, http.StatusServiceUnavailable, "runtime configuration is not loaded")
		return
	}
	w.Header().Set("ETag", strconv.Quote(snapshot.Revision))
	console := h.server.RuntimeConfig().API().Console
	writeJSON(w, http.StatusOK, buildConfigView(snapshot, h.sessions.csrfToken(r, console)))
}

func (h *Handlers) handleConfigPatch(w http.ResponseWriter, r *http.Request) {
	setConfigResponseHeaders(w)
	updater := h.server.ConfigUpdater()
	if updater == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime configuration is unavailable")
		return
	}
	snapshot := updater.Current()
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "If-Match revision is required")
		return
	}
	if expected != snapshot.Revision {
		writeError(w, http.StatusPreconditionFailed, "configuration revision is stale")
		return
	}
	var request configPatchRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid configuration patch")
		return
	}
	patch := request.configPatch()
	if len(patch) == 0 {
		writeError(w, http.StatusBadRequest, "configuration patch is empty")
		return
	}
	if request.changesCredentials() && !verifyConsolePassword(snapshot.Effective.API.Console, request.CurrentPassword) {
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	result, err := updater.Update(r.Context(), patch, expected)
	if err != nil {
		switch {
		case errors.Is(err, config.ErrRevisionConflict):
			writeError(w, http.StatusPreconditionFailed, "configuration revision is stale")
		case errors.Is(err, config.ErrSourceReadOnly):
			writeError(w, http.StatusConflict, "configuration source is read-only")
		default:
			writeError(w, http.StatusBadRequest, "configuration update was rejected")
		}
		return
	}
	updated := result.Snapshot
	w.Header().Set("ETag", strconv.Quote(updated.Revision))
	writeJSON(w, http.StatusOK, buildConfigView(updated, ""))
}

func (h *Handlers) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	setConfigResponseHeaders(w)
	updater := h.server.ConfigUpdater()
	if updater == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime configuration is unavailable")
		return
	}
	snapshot := updater.Current()
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "If-Match revision is required")
		return
	}
	if expected != snapshot.Revision {
		writeError(w, http.StatusPreconditionFailed, "configuration revision is stale")
		return
	}
	var request passwordChangeRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "invalid password change")
		return
	}
	if !verifyConsolePassword(snapshot.Effective.API.Console, request.CurrentPassword) {
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(request.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid new password")
		return
	}
	patch := config.Patch{
		"api": map[string]any{
			"console": map[string]any{
				"password":      "",
				"password_hash": string(passwordHash),
			},
		},
	}
	result, err := updater.Update(r.Context(), patch, expected)
	if err != nil {
		switch {
		case errors.Is(err, config.ErrRevisionConflict):
			writeError(w, http.StatusPreconditionFailed, "configuration revision is stale")
		case errors.Is(err, config.ErrSourceReadOnly):
			writeError(w, http.StatusConflict, "configuration source is read-only")
		default:
			writeError(w, http.StatusBadRequest, "password change was rejected")
		}
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "lf_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
	updated := result.Snapshot
	w.Header().Set("ETag", strconv.Quote(updated.Revision))
	writeJSON(w, http.StatusOK, buildConfigView(updated, ""))
}

func (r configPatchRequest) changesCredentials() bool {
	if r.API != nil {
		if r.API.Auth != nil && r.API.Auth.BearerToken != nil {
			return true
		}
		if r.API.Console != nil && r.API.Console.Username != nil {
			return true
		}
	}
	return authRuleChangesCredentials(r.Auth)
}

func authRuleChangesCredentials(auth *mediaAuthPatch) bool {
	if auth == nil {
		return false
	}
	for _, rule := range []*authRulePatch{auth.Publish, auth.Subscribe} {
		if rule == nil {
			continue
		}
		if rule.Token != nil && rule.Token.Secret != nil {
			return true
		}
		if rule.Callback != nil && rule.Callback.URL != nil {
			return true
		}
	}
	return false
}

func setConfigResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie, Authorization")
}

func parseIfMatch(value string) (string, error) {
	if value == "" {
		return "", strconv.ErrSyntax
	}
	return strconv.Unquote(value)
}

func (r configPatchRequest) configPatch() config.Patch {
	patch := config.Patch{}
	if r.Server != nil {
		server := map[string]any{}
		if r.Server.Name != nil {
			server["name"] = *r.Server.Name
		}
		if r.Server.LogLevel != nil {
			server["log_level"] = *r.Server.LogLevel
		}
		if len(server) > 0 {
			patch["server"] = server
		}
	}
	if r.Auth != nil {
		auth := map[string]any{}
		if r.Auth.Enabled != nil {
			auth["enabled"] = *r.Auth.Enabled
		}
		if r.Auth.Publish != nil {
			if publish := r.Auth.Publish.configPatch(); len(publish) > 0 {
				auth["publish"] = publish
			}
		}
		if r.Auth.Subscribe != nil {
			if subscribe := r.Auth.Subscribe.configPatch(); len(subscribe) > 0 {
				auth["subscribe"] = subscribe
			}
		}
		if len(auth) > 0 {
			patch["auth"] = auth
		}
	}
	if r.API != nil {
		api := map[string]any{}
		if r.API.PprofEnabled != nil {
			api["pprof_enabled"] = *r.API.PprofEnabled
		}
		if r.API.Auth != nil {
			auth := map[string]any{}
			if r.API.Auth.Enabled != nil {
				auth["enabled"] = *r.API.Auth.Enabled
			}
			if r.API.Auth.BearerToken != nil {
				auth["bearer_token"] = *r.API.Auth.BearerToken
			}
			if len(auth) > 0 {
				api["auth"] = auth
			}
		}
		if r.API.Console != nil {
			console := map[string]any{}
			if r.API.Console.Username != nil {
				console["username"] = *r.API.Console.Username
			}
			if r.API.Console.AllowInsecureDefaultCredentials != nil {
				console["allow_insecure_default_credentials"] = *r.API.Console.AllowInsecureDefaultCredentials
			}
			if len(console) > 0 {
				api["console"] = console
			}
		}
		if len(api) > 0 {
			patch["api"] = api
		}
	}
	return patch
}

func (r authRulePatch) configPatch() map[string]any {
	rule := map[string]any{}
	if r.Mode != nil {
		rule["mode"] = *r.Mode
	}
	if r.Stage != nil {
		rule["stage"] = *r.Stage
	}
	if r.Token != nil {
		token := map[string]any{}
		if r.Token.Secret != nil {
			token["secret"] = *r.Token.Secret
		}
		if r.Token.Algorithm != nil {
			token["algorithm"] = *r.Token.Algorithm
		}
		if len(token) > 0 {
			rule["token"] = token
		}
	}
	if r.Callback != nil {
		callback := map[string]any{}
		if r.Callback.URL != nil {
			callback["url"] = *r.Callback.URL
		}
		if r.Callback.Timeout != nil {
			callback["timeout"] = *r.Callback.Timeout
		}
		if len(callback) > 0 {
			rule["callback"] = callback
		}
	}
	return rule
}
