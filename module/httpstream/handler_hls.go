package httpstream

import (
	"net/http"
	"strconv"
	"time"

	"github.com/im-pingo/liveforge/core"
)

// serveHLSPlaylist serves the m3u8 playlist for a stream.
func (m *Module) serveHLSPlaylist(w http.ResponseWriter, r *http.Request, streamKey string) {
	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	// LL-HLS takes precedence when enabled
	if m.server.Config().HTTP.LLHLS.Enabled {
		m.serveLLHLSPlaylist(w, r, streamKey, stream)
		return
	}

	mgr := m.getOrCreateHLS(streamKey, stream)

	// Wait for at least one segment before serving playlist.
	// An empty playlist causes ffplay to give up immediately.
	for i := 0; i < 100 && mgr.SegmentCount() == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}

	playlist := mgr.GenerateM3U8()

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write([]byte(playlist))
}

// serveLLHLSPlaylist serves the LL-HLS m3u8 playlist with blocking reload support.
func (m *Module) serveLLHLSPlaylist(w http.ResponseWriter, r *http.Request, streamKey string, stream *core.Stream) {
	mgr := m.getOrCreateLLHLS(streamKey, stream)

	// Parse blocking reload params
	targetMSN := -1
	targetPart := -1
	skip := false

	if msn := r.URL.Query().Get("_HLS_msn"); msn != "" {
		if n, err := strconv.Atoi(msn); err == nil {
			targetMSN = n
		}
	}
	if part := r.URL.Query().Get("_HLS_part"); part != "" {
		if n, err := strconv.Atoi(part); err == nil {
			targetPart = n
		}
	}
	if r.URL.Query().Get("_HLS_skip") == "YES" {
		skip = true
	}

	// For non-blocking requests (legacy players like ffplay that don't support
	// LL-HLS blocking reload), wait for at least 3 completed segments.
	// FFmpeg's HLS demuxer uses live_start_index=-3, meaning it starts
	// playback from n_segments-3. With fewer than 3 segments, the player
	// buffer drains before reloads complete, causing periodic stutter.
	if targetMSN < 0 {
		const minSegmentsForLegacy = 3
		for i := 0; i < 300 && mgr.SegmentCount() < minSegmentsForLegacy; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}

	playlist, _ := mgr.GeneratePlaylist(r.Context(), targetMSN, targetPart, skip)

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write([]byte(playlist))
}

// serveHLSSegment serves a single TS segment by sequence number.
func (m *Module) serveHLSSegment(w http.ResponseWriter, r *http.Request, streamKey string, seqNum int) {
	m.hlsMu.Lock()
	mgr, ok := m.hlsManagers[streamKey]
	m.hlsMu.Unlock()

	if !ok {
		http.Error(w, "no HLS session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetSegment(seqNum)
	if !found {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=10")
	w.Write(data)
}

// serveLLHLSPartialSegment serves a partial segment by MSN and part index.
func (m *Module) serveLLHLSPartialSegment(w http.ResponseWriter, _ *http.Request, streamKey string, msn, partIdx int) {
	m.llhlsMu.Lock()
	mgr, ok := m.llhlsManagers[streamKey]
	m.llhlsMu.Unlock()

	if !ok {
		http.Error(w, "no LL-HLS session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetPartialSegment(msn, partIdx)
	if !found {
		http.Error(w, "partial segment not found", http.StatusNotFound)
		return
	}

	contentType := "video/mp4"
	if m.server.Config().HTTP.LLHLS.Container == "ts" {
		contentType = "video/mp2t"
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// serveLLHLSFullSegment serves a completed full segment by MSN.
func (m *Module) serveLLHLSFullSegment(w http.ResponseWriter, _ *http.Request, streamKey string, msn int) {
	m.llhlsMu.Lock()
	mgr, ok := m.llhlsManagers[streamKey]
	m.llhlsMu.Unlock()

	if !ok {
		http.Error(w, "no LL-HLS session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetFullSegment(msn)
	if !found {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	contentType := "video/mp4"
	if m.server.Config().HTTP.LLHLS.Container == "ts" {
		contentType = "video/mp2t"
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(data)
}

// serveLLHLSInit serves the fMP4 init segment from the LL-HLS manager.
func (m *Module) serveLLHLSInit(w http.ResponseWriter, _ *http.Request, streamKey string) {
	m.llhlsMu.Lock()
	mgr, ok := m.llhlsManagers[streamKey]
	m.llhlsMu.Unlock()

	if !ok {
		http.Error(w, "no LL-HLS session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetInitSegment()
	if !found {
		for range 50 {
			time.Sleep(100 * time.Millisecond)
			data, found = mgr.GetInitSegment()
			if found {
				break
			}
		}
	}
	if !found {
		http.Error(w, "init segment not ready", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}
