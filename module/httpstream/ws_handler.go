package httpstream

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/im-pingo/liveforge/core"
)

func (m *Module) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !m.server.AcquireConn() {
		http.Error(w, "max connections reached", http.StatusServiceUnavailable)
		return
	}
	defer m.server.ReleaseConn()

	// Strip the "/ws/" prefix so parseStreamPath sees "app/key.format"
	wsPath := strings.TrimPrefix(r.URL.Path, "/ws")
	app, key, format, ok := parseStreamPath(wsPath)
	if !ok {
		http.Error(w, "invalid path, expected /ws/app/key.{flv,ts,mp4}", http.StatusBadRequest)
		return
	}

	switch format {
	case "flv", "ts", "mp4":
	default:
		http.Error(w, "unsupported format: "+format, http.StatusBadRequest)
		return
	}

	streamKey := app + "/" + key

	// Emit subscribe event (auth hooks can reject)
	if err := m.server.GetEventBus().Emit(core.EventSubscribe, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "ws-" + format,
		RemoteAddr: r.RemoteAddr,
		Params:     queryToMap(r.URL.Query()),
	}); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		log.Printf("[httpstream] websocket accept error: %v", err)
		return
	}
	defer conn.CloseNow()

	log.Printf("[httpstream] ws-%s subscriber for %s from %s", format, streamKey, r.RemoteAddr)
	m.serveWebSocket(r.Context(), conn, format, stream)

	m.server.GetEventBus().Emit(core.EventSubscribeStop, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "ws-" + format,
		RemoteAddr: r.RemoteAddr,
	}) //nolint:errcheck
}

func (m *Module) serveWebSocket(ctx context.Context, conn *websocket.Conn, format string, stream *core.Stream) {
	m.ensureMuxerCallbacks(stream)

	mm := stream.MuxerManager()
	reader, inst := mm.GetOrCreateMuxer(format)
	defer mm.ReleaseMuxer(format)

	// Send init data (FLV header / FMP4 init segment). TS doesn't need it.
	if format == "flv" || format == "mp4" {
		for i := 0; i < 100; i++ {
			if data := inst.InitData(); data != nil {
				if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
					return
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Read loop: pull muxed data from shared buffer, send as WS binary frames
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "client disconnected")
			return
		default:
		}

		data, ok := reader.Read()
		if !ok {
			conn.Close(websocket.StatusNormalClosure, "stream ended")
			return
		}

		if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
			return
		}
	}
}
