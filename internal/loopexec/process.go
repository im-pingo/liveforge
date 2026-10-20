package loopexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type processResult struct {
	ExecutableSHA256 string
	ExitCode         int
	Err              error
	Started          bool
	TimedOut         bool
	Canceled         bool
	Executable       string
}

func runProcess(ctx context.Context, root string, argv, env []string, stdout, stderr io.Writer) processResult {
	result := processResult{ExitCode: -1}
	canceled := func() {
		result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		result.Canceled = errors.Is(ctx.Err(), context.Canceled)
		result.Err = ctx.Err()
	}
	if ctx.Err() != nil {
		canceled()
		return result
	}
	if len(argv) == 0 {
		result.Err = errors.New("empty argv")
		return result
	}
	executable := argv[0]
	if strings.ContainsRune(executable, filepath.Separator) && !filepath.IsAbs(executable) {
		executable = filepath.Join(root, executable)
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		result.Err = err
		return result
	}
	result.Executable = resolved
	file, hashErr := os.Open(resolved)
	if hashErr != nil {
		result.Err = hashErr
		return result
	}
	h := sha256.New()
	_, hashErr = io.Copy(h, contextReader{ctx: ctx, Reader: file})
	_ = file.Close()
	if hashErr != nil {
		result.Err = hashErr
		if ctx.Err() != nil {
			canceled()
		}
		return result
	}
	result.ExecutableSHA256 = hex.EncodeToString(h.Sum(nil))
	cmd := exec.Command(resolved, argv[1:]...)
	cmd.Dir = root
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	if prepareErr := prepareProcess(cmd); prepareErr != nil {
		result.Err = prepareErr
		return result
	}
	if startErr := cmd.Start(); startErr != nil {
		result.Err = startErr
		return result
	}
	result.Started = true
	defer killProcessGroup(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		killProcessGroup(cmd)
		err = <-done
		canceled()
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if result.Err == nil {
		result.Err = err
	}
	return result
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type limitedLog struct {
	writer    io.Writer
	remaining int64
	truncated bool
}

func (w *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	keep := int64(n)
	if keep > w.remaining {
		keep = w.remaining
		w.truncated = true
	}
	if keep > 0 {
		wrote, err := w.writer.Write(p[:int(keep)])
		w.remaining -= int64(wrote)
		if err != nil {
			return wrote, err
		}
		if wrote != int(keep) {
			return wrote, io.ErrShortWrite
		}
	}
	return n, nil
}
