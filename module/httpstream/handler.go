package httpstream

import (
	"log/slog"
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

	slog.Debug("request", "module", "httpstream", "path", r.URL.Path)

	// Try segment path first: /app/key/seg.ext
	if app, key, segName, ext, ok := parseSegmentPath(r.URL.Path); ok {
		switch ext {
		case "ts":
			streamKey := app + "/" + key
			// LL-HLS TS partial segment: segName = "MSN.partIdx"
			if m.server.Config().HTTP.LLHLS.Enabled && m.server.Config().HTTP.LLHLS.Container == "ts" {
				if strings.Contains(segName, ".") {
					tsParts := strings.SplitN(segName, ".", 2)
					msn, err1 := strconv.Atoi(tsParts[0])
					partIdx, err2 := strconv.Atoi(tsParts[1])
					if err1 != nil || err2 != nil {
						http.Error(w, "invalid partial segment path", http.StatusBadRequest)
						return
					}
					m.serveLLHLSPartialSegment(w, r, streamKey, msn, partIdx)
					return
				}
				if msn, err := strconv.Atoi(segName); err == nil {
					m.llhlsMu.Lock()
					_, hasLLHLS := m.llhlsManagers[streamKey]
					m.llhlsMu.Unlock()
					if hasLLHLS {
						m.serveLLHLSFullSegment(w, r, streamKey, msn)
						return
					}
				}
			}
			// Regular HLS segment: /app/key/N.ts
			seqNum, err := strconv.Atoi(segName)
			if err != nil {
				http.Error(w, "invalid segment number", http.StatusBadRequest)
				return
			}
			m.serveHLSSegment(w, r, streamKey, seqNum)
			return
		case "m4s":
			streamKey := app + "/" + key
			// LL-HLS partial segment: segName = "MSN.partIdx"
			if strings.Contains(segName, ".") {
				m4sParts := strings.SplitN(segName, ".", 2)
				msn, err1 := strconv.Atoi(m4sParts[0])
				partIdx, err2 := strconv.Atoi(m4sParts[1])
				if err1 != nil || err2 != nil {
					http.Error(w, "invalid partial segment path", http.StatusBadRequest)
					return
				}
				m.serveLLHLSPartialSegment(w, r, streamKey, msn, partIdx)
				return
			}
			// LL-HLS full segment: segName is numeric with no prefix
			m.llhlsMu.Lock()
			_, hasLLHLS := m.llhlsManagers[streamKey]
			m.llhlsMu.Unlock()
			if hasLLHLS {
				if msn, err := strconv.Atoi(segName); err == nil {
					m.serveLLHLSFullSegment(w, r, streamKey, msn)
					return
				}
			}
			// DASH media segments:
			//   /app/key/v$Number$.m4s (video)
			//   /app/key/a$Number$.m4s (audio)
			// MPD uses 1-based numbering; internal SeqNum is 0-based
			if len(segName) > 1 && segName[0] == 'v' {
				num, err := strconv.Atoi(segName[1:])
				if err != nil {
					http.Error(w, "invalid segment number", http.StatusBadRequest)
					return
				}
				m.serveDASHSegment(w, r, streamKey, num-1)
				return
			}
			if len(segName) > 1 && segName[0] == 'a' {
				num, err := strconv.Atoi(segName[1:])
				if err != nil {
					http.Error(w, "invalid segment number", http.StatusBadRequest)
					return
				}
				m.serveDASHAudioSegment(w, r, streamKey, num-1)
				return
			}
			http.Error(w, "invalid segment path", http.StatusBadRequest)
			return
		case "mp4":
			// Init segments:
			//   /app/key/init.mp4       — LL-HLS combined (video+audio) init, or DASH video-only fallback
			//   /app/key/vinit.mp4      — DASH video-only init
			//   /app/key/audio_init.mp4 — DASH audio-only init
			streamKey := app + "/" + key
			if segName == "vinit" {
				m.serveDASHInit(w, r, streamKey)
				return
			}
			if segName == "init" {
				// LL-HLS init segment takes precedence if manager exists
				m.llhlsMu.Lock()
				_, hasLLHLS := m.llhlsManagers[streamKey]
				m.llhlsMu.Unlock()
				if hasLLHLS && m.server.Config().HTTP.LLHLS.Container == "fmp4" {
					m.serveLLHLSInit(w, r, streamKey)
					return
				}
				m.serveDASHInit(w, r, streamKey)
				return
			}
			if segName == "audio_init" {
				m.serveDASHAudioInit(w, r, streamKey)
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

	slog.Info("subscriber connected", "module", "httpstream", "format", format, "stream", streamKey, "remote", r.RemoteAddr)
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

// serveDASHManifest serves the MPD manifest for a stream.
func (m *Module) serveDASHManifest(w http.ResponseWriter, r *http.Request, streamKey string) {
	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	mgr := m.getOrCreateDASH(streamKey, stream)

	// Wait for at least 3 segments before serving MPD. FFmpeg's dashdec.c
	// caches the SegmentTemplate @duration from the first MPD response and
	// never updates it on refresh. With fewer than 3 segments the computed
	// duration is wrong (dominated by short GOP-cache edge-case segments).
	for i := 0; i < 150 && mgr.SegmentCount() < 3; i++ {
		time.Sleep(100 * time.Millisecond)
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
//
// Production hold: if the segment hasn't been produced yet (future segment),
// wait up to ~6s for it to appear. ffmpeg's dashdec.c has no backoff on 404.
//
// No availability hold is needed because the MPD uses SegmentTimeline with
// explicit per-segment timing, so ffmpeg advances segment numbers from the
// timeline rather than computing them from wall-clock time.
func (m *Module) serveDASHSegment(w http.ResponseWriter, r *http.Request, streamKey string, seqNum int) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	// Production hold — wait for the segment to be produced.
	data, found := mgr.GetSegment(seqNum)
	if !found {
		_, hi := mgr.SegmentRange()
		if seqNum > hi {
			for i := 0; i < 60; i++ {
				select {
				case <-r.Context().Done():
					return
				default:
				}
				time.Sleep(100 * time.Millisecond)
				data, found = mgr.GetSegment(seqNum)
				if found {
					break
				}
			}
		}
	}

	if !found {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=10")
	w.Write(data)
}

// serveDASHAudioInit serves the audio-only fMP4 init segment.
func (m *Module) serveDASHAudioInit(w http.ResponseWriter, r *http.Request, streamKey string) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetAudioInitSegment()
	if !found {
		http.Error(w, "audio init segment not ready", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

// serveDASHAudioSegment serves a single audio-only fMP4 segment by sequence number.
func (m *Module) serveDASHAudioSegment(w http.ResponseWriter, r *http.Request, streamKey string, seqNum int) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	// Production hold — wait for the segment to be produced.
	data, found := mgr.GetAudioSegment(seqNum)
	if !found {
		_, hi := mgr.SegmentRange()
		if seqNum > hi {
			for i := 0; i < 60; i++ {
				select {
				case <-r.Context().Done():
					return
				default:
				}
				time.Sleep(100 * time.Millisecond)
				data, found = mgr.GetAudioSegment(seqNum)
				if found {
					break
				}
			}
		}
	}

	if !found {
		http.Error(w, "audio segment not found", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
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

func (m *Module) setCORSHeaders(w http.ResponseWriter) {
	if m.server.Config().HTTP.CORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
}
