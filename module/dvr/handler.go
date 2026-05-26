package dvr

import (
	"net/http"
	"path"
	"strings"
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
