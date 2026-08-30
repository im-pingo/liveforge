package dvr

import (
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/im-pingo/liveforge/core"
)

// dvrMediaCORS permits browser consoles on a separate management port to
// fetch HLS playlists and segments. Authentication remains enforced by the
// synchronous subscribe hook for every media request; this does not enable
// credentialed cross-origin cookies.
func dvrMediaCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Range")
		w.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Length, Content-Range, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func strictDVRMediaRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath := r.URL.Path
		if requestPath != "/dvr" && !strings.HasPrefix(requestPath, "/dvr/") {
			next.ServeHTTP(w, r)
			return
		}
		// URL.Path is already unescaped by net/http. Reject encoded path
		// separators before the decoded path can turn one route segment into
		// another stream key or resource component.
		if hasEscapedPathSeparator(r.URL.EscapedPath()) {
			http.NotFound(w, r)
			return
		}

		remainder := strings.TrimPrefix(requestPath, "/dvr/")
		app, resource, hasResource := strings.Cut(remainder, "/")
		if requestPath == "/dvr" || path.Clean(requestPath) != requestPath || !hasResource || app == "" || resource == "" {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasEscapedPathSeparator(escapedPath string) bool {
	for i := 0; i+2 < len(escapedPath); i++ {
		if escapedPath[i] != '%' {
			continue
		}
		hi, okHi := fromHex(escapedPath[i+1])
		lo, okLo := fromHex(escapedPath[i+2])
		if okHi && okLo {
			decoded := hi<<4 | lo
			if decoded == '/' || decoded == '\\' {
				return true
			}
		}
		i += 2
	}
	return false
}

func fromHex(value byte) (byte, bool) {
	switch {
	case '0' <= value && value <= '9':
		return value - '0', true
	case 'a' <= value && value <= 'f':
		return value - 'a' + 10, true
	case 'A' <= value && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func (m *Module) handleMedia(w http.ResponseWriter, r *http.Request) {
	if m.server != nil && !m.server.AcquireConn() {
		http.Error(w, "max connections reached", http.StatusServiceUnavailable)
		return
	}
	if m.server != nil {
		defer m.server.ReleaseConn()
	}

	app := r.PathValue("app")
	resource := r.PathValue("resource")
	if !validMediaPathPart(app) {
		http.NotFound(w, r)
		return
	}

	if key, ok := strings.CutSuffix(resource, ".m3u8"); ok && validMediaKey(key) {
		r.SetPathValue("key", key)
		m.handlePlaylist(w, r)
		return
	}

	separator := strings.LastIndexByte(resource, '/')
	if separator <= 0 || separator == len(resource)-1 {
		http.NotFound(w, r)
		return
	}
	key, filename := resource[:separator], resource[separator+1:]
	if !validMediaKey(key) || strings.ContainsAny(filename, "/\\") || parseSeqNum(filename) < 0 {
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("key", key)
	r.SetPathValue("filename", filename)
	m.handleSegment(w, r)
}

func validMediaPathPart(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\")
}

func validMediaKey(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if !validMediaPathPart(part) {
			return false
		}
	}
	return true
}

func (m *Module) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	key := r.PathValue("key")
	streamKey := app + "/" + key
	if !m.authorizePlayback(w, r, streamKey) {
		return
	}

	m.mu.Lock()
	session := m.sessions[streamKey]
	m.mu.Unlock()

	if session == nil {
		http.NotFound(w, r)
		return
	}

	query := url.Values{}
	if token := r.URL.Query().Get("token"); token != "" {
		query.Set("token", token)
	}
	playlist := GeneratePlaylistWithQuery(session.Index(), streamKey, session.IsLive(), query.Encode())
	if playlist == "" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write([]byte(playlist))
}

func (m *Module) handleSegment(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	key := r.PathValue("key")
	filename := r.PathValue("filename")
	streamKey := app + "/" + key
	if !m.authorizePlayback(w, r, streamKey) {
		return
	}

	m.mu.Lock()
	session := m.sessions[streamKey]
	m.mu.Unlock()

	if session == nil {
		http.NotFound(w, r)
		return
	}

	seqNum := parseSeqNum(filename)
	if seqNum < 0 {
		http.NotFound(w, r)
		return
	}

	seg, ok := session.Index().SegmentBySeqNum(seqNum)
	if !ok {
		http.NotFound(w, r)
		return
	}
	file, info, err := session.openIndexedSegment(seg)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, seg.Filename, info.ModTime(), file)
}

func (m *Module) authorizePlayback(w http.ResponseWriter, r *http.Request, streamKey string) bool {
	params := make(map[string]string, len(r.URL.Query()))
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}
	if params["token"] == "" {
		if authorization := r.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
			params["token"] = strings.TrimPrefix(authorization, "Bearer ")
		}
	}
	err := m.server.Authorize(r.Context(), core.AuthorizationRequest{
		Action:     core.AuthorizationSubscribe,
		Stage:      core.AuthorizationPreSession,
		StreamKey:  streamKey,
		Protocol:   "dvr",
		RemoteAddr: r.RemoteAddr,
		Params:     params,
	})
	if err == nil {
		return true
	}
	if params["token"] == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	http.Error(w, "forbidden", http.StatusForbidden)
	return false
}

func parseSeqNum(filename string) int {
	// Expected format: seg_000042.ts
	if !strings.HasPrefix(filename, "seg_") || !strings.HasSuffix(filename, ".ts") {
		return -1
	}
	numStr := filename[4 : len(filename)-3]
	if len(numStr) < 6 || (len(numStr) > 6 && numStr[0] == '0') {
		return -1
	}
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return -1
		}
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return -1
	}
	return n
}

// matchPattern checks if a stream key matches a glob pattern.
func matchPattern(pattern, key string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	matched, _ := path.Match(pattern, key)
	return matched
}
