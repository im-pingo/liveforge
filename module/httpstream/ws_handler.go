package httpstream

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/im-pingo/liveforge/core"
)

const websocketContinuityLossReason = "stream continuity lost"

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

	streamKey := streamKeyFromPath(app, key)

	// Run authorization independently from lifecycle delivery.
	subscribeCtx := &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "ws-" + format,
		RemoteAddr: r.RemoteAddr,
		Params:     queryToMap(r.URL.Query()),
	}
	if err := m.server.Authorize(r.Context(), core.AuthorizationRequest{
		Action:     core.AuthorizationSubscribe,
		Stage:      core.AuthorizationPreSession,
		StreamKey:  subscribeCtx.StreamKey,
		Protocol:   subscribeCtx.Protocol,
		RemoteAddr: subscribeCtx.RemoteAddr,
		Params:     subscribeCtx.Params,
	}); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}
	startup := stream.StartupSnapshot()
	releaseSubscriber, err := stream.AddSubscriberForGeneration(subscribeCtx.Protocol, startup.Generation)
	if err != nil {
		http.Error(w, "publisher generation is no longer available", http.StatusServiceUnavailable)
		return
	}
	defer releaseSubscriber()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		slog.Error("websocket accept error", "module", "httpstream", "error", err)
		return
	}
	defer conn.CloseNow()

	lifecycleCtx := *subscribeCtx
	lifecycleCtx.SubscriberID = nextSubscriberID(lifecycleCtx.Protocol, streamKey)
	lifecycleCtx.StreamInstanceID = startup.StreamInstanceID
	lifecycleCtx.PublisherGeneration = startup.Generation
	lifecycleCtx.PublisherID = startup.PublisherID
	if err := m.server.GetEventBus().EmitAsync(core.EventSubscribe, &lifecycleCtx); err != nil {
		_ = conn.Close(websocket.StatusTryAgainLater, "subscriber lifecycle capacity exceeded")
		return
	}
	defer func() {
		if err := m.server.GetEventBus().EmitAsync(core.EventSubscribeStop, &lifecycleCtx); err != nil {
			slog.Error("subscriber terminal lifecycle admission failed", "module", "httpstream", "format", format, "stream", streamKey, "error", err)
		}
	}()

	slog.Info("ws subscriber connected", "module", "httpstream", "format", format, "stream", streamKey, "remote", r.RemoteAddr)
	m.serveWebSocket(r.Context(), conn, format, stream, startup.Generation)
}

func (m *Module) serveWebSocket(ctx context.Context, conn *websocket.Conn, format string, stream *core.Stream, generation uint64) {
	m.ensureMuxerCallbacks(stream)

	mm := stream.MuxerManager()
	reader, inst := mm.GetOrCreateMuxerForGeneration(format, generation)
	if reader == nil || inst == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "stream ended")
		return
	}
	defer mm.ReleaseMuxer(format, inst)

	// Send init data (FLV header / FMP4 init segment). TS doesn't need it.
	if format == "flv" || format == "mp4" {
		var initData []byte
		if waitForCondition(ctx, time.Second, 10*time.Millisecond, func() bool {
			initData = inst.InitData()
			return initData != nil
		}) {
			if err := writeWebSocketStreamChunk(ctx, conn, initData, httpStreamWriteTimeout); err != nil {
				return
			}
		} else {
			return
		}
	}

	serveWebSocketStreamReader(ctx, conn, format, stream.Key(), reader)
}

func serveWebSocketStreamReader(ctx context.Context, conn *websocket.Conn, format, streamKey string, reader *core.SharedBufferReader) {
	// Close and join the reader watcher on every handler exit, including a
	// WebSocket write failure.
	stopReaderWatch := watchReaderContext(ctx, reader)
	defer stopReaderWatch()

	for {
		result := reader.ReadResult()
		if ctx.Err() != nil {
			_ = conn.CloseNow()
			return
		}
		if result.Overwritten > 0 {
			logContinuousOutputOverwrite("websocket", format, streamKey, result.Overwritten)
			_ = conn.Close(websocket.StatusTryAgainLater, websocketContinuityLossReason)
			return
		}
		if !result.OK {
			conn.Close(websocket.StatusNormalClosure, "stream ended")
			return
		}

		if err := writeWebSocketStreamChunk(ctx, conn, result.Data, httpStreamWriteTimeout); err != nil {
			return
		}
	}
}
