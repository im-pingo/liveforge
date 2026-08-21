package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/im-pingo/liveforge/config"
)

func TestInitializeConfigManagerLoadsRuntimeOverride(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "liveforge.yaml")
	overridePath := config.RuntimeOverridePath(basePath)
	if err := os.WriteFile(basePath, []byte("server:\n  name: base\nstream:\n  ring_buffer_size: 16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, []byte("server:\n  name: runtime\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager, cfg, err := initializeConfigManager(context.Background(), basePath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Name != "runtime" {
		t.Fatalf("effective server name = %q, want runtime", cfg.Server.Name)
	}
	if got := manager.Current(); got.Effective == nil || got.Effective.Server.Name != cfg.Server.Name || got.Source == "" || got.Revision == "" {
		t.Fatalf("initial snapshot = %#v", got)
	}
}

type fakeRuntimeConfigManager struct {
	runStarted chan struct{}
	runStopped chan struct{}
	refreshes  chan struct{}
	refreshErr error
}

func (m *fakeRuntimeConfigManager) Run(ctx context.Context, _ func(error)) {
	close(m.runStarted)
	<-ctx.Done()
	close(m.runStopped)
}

func (m *fakeRuntimeConfigManager) Refresh(context.Context) (config.ApplyResult, error) {
	m.refreshes <- struct{}{}
	return config.ApplyResult{}, m.refreshErr
}

func TestWaitForShutdownRunsPollingAndRefreshesOnSIGHUP(t *testing.T) {
	manager := &fakeRuntimeConfigManager{
		runStarted: make(chan struct{}),
		runStopped: make(chan struct{}),
		refreshes:  make(chan struct{}, 1),
		refreshErr: errors.New("invalid refresh"),
	}
	signals := make(chan os.Signal, 2)
	stopped := make(chan os.Signal, 1)
	go func() {
		stopped <- waitForShutdown(context.Background(), manager, signals)
	}()

	<-manager.runStarted
	signals <- syscall.SIGHUP
	<-manager.refreshes
	signals <- syscall.SIGTERM
	if got := <-stopped; got != syscall.SIGTERM {
		t.Fatalf("shutdown signal = %v, want SIGTERM", got)
	}
	<-manager.runStopped
}
