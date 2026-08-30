package httpstream

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type testCloseableReader struct {
	once   sync.Once
	closed chan struct{}
}

type deadlineResponseWriter struct {
	header   http.Header
	deadline time.Time
	payload  []byte
	flushed  bool
}

func (w *deadlineResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*deadlineResponseWriter) WriteHeader(int) {}

func (w *deadlineResponseWriter) Write(payload []byte) (int, error) {
	w.payload = append(w.payload, payload...)
	return len(payload), nil
}

func (w *deadlineResponseWriter) Flush() { w.flushed = true }

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

type blockingWebSocketWriter struct{}

func (blockingWebSocketWriter) Write(ctx context.Context, _ websocket.MessageType, _ []byte) error {
	<-ctx.Done()
	return ctx.Err()
}

func (r *testCloseableReader) Close() {
	r.once.Do(func() { close(r.closed) })
}

func TestWaitForConditionReturnsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	if waitForCondition(ctx, time.Second, time.Second, func() bool { return false }) {
		t.Fatal("waitForCondition returned ready after cancellation")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("canceled wait took %s", elapsed)
	}
}

func TestWatchReaderContextClosesReaderAndWaitsForWatcher(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &testCloseableReader{closed: make(chan struct{})}
		stop := watchReaderContext(ctx, reader)
		cancel()
		select {
		case <-reader.closed:
		case <-time.After(time.Second):
			t.Fatal("context cancellation did not close reader")
		}
		stop()
	})

	t.Run("normal handler exit", func(t *testing.T) {
		reader := &testCloseableReader{closed: make(chan struct{})}
		stop := watchReaderContext(context.Background(), reader)
		stop()
		select {
		case <-reader.closed:
		default:
			t.Fatal("normal cleanup did not close reader")
		}
	})
}

func TestWriteHTTPStreamChunkSetsPerWriteDeadlineAndFlushes(t *testing.T) {
	writer := &deadlineResponseWriter{}
	timeout := 200 * time.Millisecond
	started := time.Now()
	if err := writeHTTPStreamChunk(writer, []byte("media"), timeout); err != nil {
		t.Fatal(err)
	}
	if writer.deadline.Before(started.Add(timeout)) || writer.deadline.After(time.Now().Add(timeout)) {
		t.Fatalf("write deadline = %s, want a fresh %s deadline", writer.deadline, timeout)
	}
	if got := string(writer.payload); got != "media" {
		t.Fatalf("payload = %q, want media", got)
	}
	if !writer.flushed {
		t.Fatal("stream chunk was not flushed")
	}
}

func TestWriteWebSocketStreamChunkTimesOutBlockedWrite(t *testing.T) {
	started := time.Now()
	err := writeWebSocketStreamChunk(context.Background(), blockingWebSocketWriter{}, []byte("media"), 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("blocked WebSocket write took %s", elapsed)
	}
}
