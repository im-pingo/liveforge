package dvr

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/im-pingo/liveforge/core"
)

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

	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, seg.DiskPath)
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
	err := m.server.GetEventBus().Emit(core.EventSubscribe, &core.EventContext{
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
	name := strings.TrimSuffix(filename, ".ts")
	if !strings.HasPrefix(name, "seg_") {
		return -1
	}
	numStr := name[4:]
	n := 0
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
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
