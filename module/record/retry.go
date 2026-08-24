package record

import (
	"io"
)

type writeSeeker interface {
	io.Writer
	io.Seeker
}

type retryWriteSeeker struct {
	writeSeeker
	attempts int
	onRetry  func()
}

func (w *retryWriteSeeker) Name() string {
	if named, ok := w.writeSeeker.(interface{ Name() string }); ok {
		return named.Name()
	}
	return ""
}

func newRetryWriteSeeker(w writeSeeker, attempts int, onRetry func()) *retryWriteSeeker {
	if attempts < 1 {
		attempts = 1
	}
	return &retryWriteSeeker{writeSeeker: w, attempts: attempts, onRetry: onRetry}
}

func (w *retryWriteSeeker) Write(p []byte) (int, error) {
	var lastErr error
	for attempt := 0; attempt < w.attempts; attempt++ {
		n, err := w.writeSeeker.Write(p)
		if err == nil && n == len(p) {
			return n, nil
		}
		if n > 0 {
			return n, io.ErrShortWrite
		}
		if err == nil {
			err = io.ErrShortWrite
		}
		lastErr = err
		if attempt+1 < w.attempts && w.onRetry != nil {
			w.onRetry()
		}
	}
	return 0, lastErr
}
