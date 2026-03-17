package httpstream

import (
	"log"
	"net/http"
	"path"
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

	app, key, format, ok := parseStreamPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid path, expected /app/key.{flv,ts,mp4}", http.StatusBadRequest)
		return
	}

	// Validate format
	switch format {
	case "flv", "ts", "mp4":
	default:
		http.Error(w, "unsupported format: "+format, http.StatusBadRequest)
		return
	}

	// Look up stream
	streamKey := app + "/" + key
	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	log.Printf("[httpstream] %s subscriber for %s from %s", format, streamKey, r.RemoteAddr)
	m.serveStream(w, r, format, stream)
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

func (m *Module) setCORSHeaders(w http.ResponseWriter) {
	if m.server.Config().HTTP.CORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
}
