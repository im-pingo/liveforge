//go:build linux || darwin

package api

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func auditPersistenceSupported() error { return nil }

func auditOpenFlags() int { return unix.O_NOFOLLOW | unix.O_NONBLOCK }

func lockAuditFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func validateAuditLinks(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Nlink != 1 {
		return errors.New("audit files must not have additional hard links")
	}
	return nil
}
