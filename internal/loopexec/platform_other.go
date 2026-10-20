//go:build !linux && !darwin

package loopexec

import (
	"context"
	"errors"
	"os/exec"
)

func prepareProcess(_ *exec.Cmd) error {
	return errors.New("lf-loop process control requires Linux or macOS")
}
func killProcessGroup(_ *exec.Cmd) {}
func acquireLock(_ context.Context, _ string) (func(), error) {
	return nil, errors.New("lf-loop locking requires Linux or macOS")
}
