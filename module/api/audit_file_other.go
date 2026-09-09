//go:build !linux && !darwin

package api

import (
	"errors"
	"os"
)

func auditPersistenceSupported() error {
	return errors.New("audit persistence is unavailable on this platform")
}

func auditOpenFlags() int { return 0 }

func lockAuditFile(*os.File) error { return auditPersistenceSupported() }

func validateAuditLinks(*os.File) error { return auditPersistenceSupported() }
