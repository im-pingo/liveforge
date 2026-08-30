package httpstream

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const httpStreamWriteTimeout = 10 * time.Second

type webSocketMessageWriter interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

type closeableReader interface {
	Close()
}

// watchReaderContext closes a blocking reader when the request ends and also
// provides synchronous cleanup for writer errors and normal handler returns.
func watchReaderContext(ctx context.Context, reader closeableReader) func() {
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			reader.Close()
		case <-stop:
			reader.Close()
		}
	}()
	var stopOnce sync.Once
	return func() {
		reader.Close()
		stopOnce.Do(func() { close(stop) })
		<-watcherDone
	}
}

// waitForCondition waits for a bounded condition while honoring request or
// manager cancellation. It avoids keeping a handler asleep after a client
// has already disconnected.
func waitForCondition(ctx context.Context, timeout, interval time.Duration, ready func() bool) bool {
	if ready() {
		return true
	}
	if timeout <= 0 {
		return false
	}
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return ready()
		case <-ticker.C:
			if ready() {
				return true
			}
		}
	}
}

func setHTTPWriteDeadline(w http.ResponseWriter, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func writeHTTPStreamChunk(w http.ResponseWriter, payload []byte, timeout time.Duration) error {
	if err := writeHTTPResponse(w, payload, timeout); err != nil {
		return err
	}
	err := http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func writeHTTPResponse(w http.ResponseWriter, payload []byte, timeout time.Duration) error {
	if err := setHTTPWriteDeadline(w, timeout); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil { //nolint:gosec // payload is muxed binary media, never rendered as HTML
		return err
	}
	return nil
}

func writeHTTPError(w http.ResponseWriter, message string, status int) {
	if err := setHTTPWriteDeadline(w, httpStreamWriteTimeout); err != nil {
		return
	}
	http.Error(w, message, status)
}

func writeWebSocketStreamChunk(ctx context.Context, writer webSocketMessageWriter, payload []byte, timeout time.Duration) error {
	if timeout <= 0 {
		return writer.Write(ctx, websocket.MessageBinary, payload)
	}
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return writer.Write(writeCtx, websocket.MessageBinary, payload)
}
