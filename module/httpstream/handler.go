package httpstream

import (
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/core"
)

// parseStreamPath parses "/app/key.format" from the URL path.
// Returns app, key, format, and whether the parse succeeded.
func parseStreamPath(urlPath string) (app, key, format string, ok bool) {
	// Clean the path
	urlPath = path.Clean(urlPath)
	urlPath = strings.TrimPrefix(urlPath, "/")

	// Split into segments: app/key.format
	parts := strings.SplitN(urlPath, "/", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}

	app = parts[0]
	rest := parts[1]

	// Extract format extension
	dotIdx := strings.LastIndex(rest, ".")
	if dotIdx < 0 {
		return "", "", "", false
	}

	key = rest[:dotIdx]
	format = rest[dotIdx+1:]

	if app == "" || key == "" || format == "" {
		return "", "", "", false
	}

	return app, key, format, true
}

// parseSegmentPath parses "/app/key/seg.ext" from the URL path.
// Used for HLS segments (/app/key/N.ts) and DASH segments (/app/key/N.m4s, /app/key/init.mp4).
func parseSegmentPath(urlPath string) (app, key, segName, ext string, ok bool) {
	urlPath = path.Clean(urlPath)
	urlPath = strings.TrimPrefix(urlPath, "/")

	// Need at least 3 parts: app/key/seg.ext
	parts := strings.SplitN(urlPath, "/", 3)
	if len(parts) != 3 {
		return "", "", "", "", false
	}

	app = parts[0]
	key = parts[1]
	seg := parts[2]

	dotIdx := strings.LastIndex(seg, ".")
	if dotIdx < 0 {
		return "", "", "", "", false
	}

	segName = seg[:dotIdx]
	ext = seg[dotIdx+1:]

	if app == "" || key == "" || segName == "" || ext == "" {
		return "", "", "", "", false
	}

	return app, key, segName, ext, true
}

func (m *Module) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		m.setCORSHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !m.server.AcquireConn() {
		http.Error(w, "max connections reached", http.StatusServiceUnavailable)
		return
	}
	defer m.server.ReleaseConn()

	log.Printf("[httpstream] request: %s", r.URL.Path)

	// Try segment path first: /app/key/seg.ext
	if app, key, segName, ext, ok := parseSegmentPath(r.URL.Path); ok {
		switch ext {
		case "ts":
			// HLS segment: /app/key/N.ts
			seqNum, err := strconv.Atoi(segName)
			if err != nil {
				http.Error(w, "invalid segment number", http.StatusBadRequest)
				return
			}
			m.serveHLSSegment(w, r, app+"/"+key, seqNum)
			return
		case "m4s":
			// DASH media segment: /app/key/$Number$.m4s
			// MPD uses 1-based numbering; internal SeqNum is 0-based
			num, err := strconv.Atoi(segName)
			if err != nil {
				http.Error(w, "invalid segment number", http.StatusBadRequest)
				return
			}
			m.serveDASHSegment(w, r, app+"/"+key, num-1)
			return
		case "mp4":
			// DASH init segment: /app/key/init.mp4
			if segName == "init" {
				m.serveDASHInit(w, r, app+"/"+key)
				return
			}
		}
	}

	// Standard path: /app/key.format
	app, key, format, ok := parseStreamPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid path, expected /app/key.{flv,ts,mp4,m3u8,mpd}", http.StatusBadRequest)
		return
	}

	streamKey := app + "/" + key

	switch format {
	case "m3u8":
		m.serveHLSPlaylist(w, r, streamKey)
		return
	case "mpd":
		m.serveDASHManifest(w, r, streamKey)
		return
	case "flv", "ts", "mp4":
		// Continue to chunked streaming below
	default:
		http.Error(w, "unsupported format: "+format, http.StatusBadRequest)
		return
	}

	// Emit subscribe event (auth hooks can reject)
	if err := m.server.GetEventBus().Emit(core.EventSubscribe, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "http-" + format,
		RemoteAddr: r.RemoteAddr,
		Params:     queryToMap(r.URL.Query()),
	}); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Look up stream
	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	log.Printf("[httpstream] %s subscriber for %s from %s", format, streamKey, r.RemoteAddr)
	m.serveStream(w, r, format, stream)

	m.server.GetEventBus().Emit(core.EventSubscribeStop, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "http-" + format,
		RemoteAddr: r.RemoteAddr,
	}) //nolint:errcheck
}

// queryToMap converts url.Values to a flat map (first value wins).
func queryToMap(vals map[string][]string) map[string]string {
	if len(vals) == 0 {
		return nil
	}
	m := make(map[string]string, len(vals))
	for k, v := range vals {
		if len(v) > 0 {
			m[k] = v[0]
		}
	}
	return m
}

func (m *Module) serveStream(w http.ResponseWriter, r *http.Request, format string, stream *core.Stream) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Register muxer callbacks for this stream if not already done
	m.ensureMuxerCallbacks(stream)

	mm := stream.MuxerManager()
	reader, inst := mm.GetOrCreateMuxer(format)
	defer mm.ReleaseMuxer(format)

	// Set response headers
	switch format {
	case "flv":
		w.Header().Set("Content-Type", "video/x-flv")
	case "ts":
		w.Header().Set("Content-Type", "video/mp2t")
	case "mp4":
		w.Header().Set("Content-Type", "video/mp4")
	}
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "close")
	m.setCORSHeaders(w)

	// Wait for init data (FLV header, FMP4 init segment)
	// TS format doesn't need init data.
	if format == "flv" || format == "mp4" {
		for i := 0; i < 100; i++ {
			if data := inst.InitData(); data != nil {
				w.Write(data)
				flusher.Flush()
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Read loop
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		data, ok := reader.Read()
		if !ok {
			return
		}
		if _, err := w.Write(data); err != nil {
			return
		}
		flusher.Flush()
	}
}

// serveHLSPlaylist serves the m3u8 playlist for a stream.
func (m *Module) serveHLSPlaylist(w http.ResponseWriter, r *http.Request, streamKey string) {
	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	mgr := m.getOrCreateHLS(streamKey, stream)
	playlist := mgr.GenerateM3U8()

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

// serveDASHManifest serves the MPD manifest for a stream.
func (m *Module) serveDASHManifest(w http.ResponseWriter, r *http.Request, streamKey string) {
	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	mgr := m.getOrCreateDASH(streamKey, stream)

	// Wait for at least one segment before serving MPD.
	// An empty SegmentTimeline causes ffplay to compute invalid segment numbers.
	for i := 0; i < 50 && mgr.SegmentCount() == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if mgr.SegmentCount() == 0 {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "segments not ready yet", http.StatusServiceUnavailable)
		return
	}

	manifest := mgr.GenerateMPD()

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write([]byte(manifest))
}

// serveDASHInit serves the fMP4 init segment.
func (m *Module) serveDASHInit(w http.ResponseWriter, r *http.Request, streamKey string) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetInitSegment()
	if !found {
		http.Error(w, "init segment not ready", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

// serveDASHSegment serves a single fMP4 media segment by sequence number.
func (m *Module) serveDASHSegment(w http.ResponseWriter, r *http.Request, streamKey string, seqNum int) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetSegment(seqNum)
	if !found {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=10")
	w.Write(data)
}

func (m *Module) setCORSHeaders(w http.ResponseWriter) {
	if m.server.Config().HTTP.CORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
}
