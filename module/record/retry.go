package record

import (
	"errors"
	"io"
	"os"
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

func (w *retryWriteSeeker) CreateSidecar(base string, perm os.FileMode) (sidecarWriteObject, error) {
	provider, ok := w.writeSeeker.(sidecarMediaFile)
	if !ok {
		return nil, errors.New("record storage does not support sidecar objects")
	}
	return provider.CreateSidecar(base, perm)
}

func (w *retryWriteSeeker) WriteSidecarAtomic(base string, data []byte, perm os.FileMode) error {
	provider, ok := w.writeSeeker.(sidecarMediaFile)
	if !ok {
		return errors.New("record storage does not support sidecar objects")
	}
	return provider.WriteSidecarAtomic(base, data, perm)
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
