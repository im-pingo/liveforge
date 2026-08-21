package dvr

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/im-pingo/liveforge/core"
)

func (m *Module) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	key := r.PathValue("key")
	streamKey := app + "/" + key

	m.mu.Lock()
	session := m.sessions[streamKey]
	m.mu.Unlock()

	if session == nil {
		http.NotFound(w, r)
		return
	}
	if err := m.authorizeSubscribe(r, streamKey); err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	playlist := GeneratePlaylist(session.Index(), streamKey, session.IsLive())
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

	m.mu.Lock()
	session := m.sessions[streamKey]
	m.mu.Unlock()

	if session == nil {
		http.NotFound(w, r)
		return
	}
	if err := m.authorizeSubscribe(r, streamKey); err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
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

func (m *Module) authorizeSubscribe(r *http.Request, streamKey string) error {
	if m.server == nil {
		return nil
	}
	request := core.AuthorizationRequest{
		Action:     core.AuthorizationSubscribe,
		StreamKey:  streamKey,
		Protocol:   "dvr",
		RemoteAddr: r.RemoteAddr,
		Params:     queryParams(r),
	}
	request.Stage = core.AuthorizationPreSession
	if err := m.server.Authorize(context.Background(), request); err != nil {
		return err
	}
	request.Stage = core.AuthorizationPostConnect
	return m.server.Authorize(context.Background(), request)
}

func queryParams(r *http.Request) map[string]string {
	values := r.URL.Query()
	if len(values) == 0 {
		return nil
	}
	params := make(map[string]string, len(values))
	for key, items := range values {
		if len(items) > 0 {
			params[key] = items[0]
		}
	}
	return params
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
