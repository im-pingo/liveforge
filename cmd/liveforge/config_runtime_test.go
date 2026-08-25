package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

type rejectingRuntimeModule struct{}

func (rejectingRuntimeModule) Name() string                   { return "rejecting-runtime" }
func (rejectingRuntimeModule) Init(*core.Server) error        { return nil }
func (rejectingRuntimeModule) Hooks() []core.HookRegistration { return nil }
func (rejectingRuntimeModule) Close() error                   { return nil }
func (rejectingRuntimeModule) OnReload(*core.Server) error {
	return errors.New("reject runtime policy")
}

type lifecycleRuntimeModule struct {
	initialized      atomic.Bool
	reloadBeforeInit atomic.Bool
}

func (*lifecycleRuntimeModule) Name() string                   { return "lifecycle-runtime" }
func (m *lifecycleRuntimeModule) Init(*core.Server) error      { m.initialized.Store(true); return nil }
func (*lifecycleRuntimeModule) Hooks() []core.HookRegistration { return nil }
func (*lifecycleRuntimeModule) Close() error                   { return nil }
func (m *lifecycleRuntimeModule) OnReload(*core.Server) error {
	if !m.initialized.Load() {
		m.reloadBeforeInit.Store(true)
	}
	return nil
}

func TestNewRuntimeManagerUsesBootstrapFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "liveforge.yaml")
	if err := os.WriteFile(path, []byte("server:\n  name: bootstrap\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runtime.Source = "file"
	server := core.NewServer(cfg)
	manager, err := newRuntimeManager(cfg, path, server)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if got := manager.Snapshot().Config.Server.Name; got != "bootstrap" {
		t.Fatalf("bootstrap snapshot name = %q", got)
	}
	if server.ConfigManager() != manager {
		t.Fatal("server manager was not attached")
	}
}

func TestNewRuntimeManagerKeepsBootstrapActiveWhenServerRejectsCandidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "liveforge.yaml")
	if err := os.WriteFile(path, []byte("limits:\n  max_streams: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := core.NewServer(cfg)
	server.RegisterModule(rejectingRuntimeModule{})
	if err := server.Init(); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	manager, err := newRuntimeManager(cfg, path, server)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("limits:\n  max_streams: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	deadline := time.Now().Add(time.Second)
	for manager.Status().ConfigChangesApplicationFailed == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := manager.Snapshot().Config.Limits.MaxStreams; got != 1 {
		t.Fatalf("manager published rejected max_streams=%d", got)
	}
	if got := server.Config().Limits.MaxStreams; got != 1 {
		t.Fatalf("server published rejected max_streams=%d", got)
	}
}

func TestInitializeServerStartsRuntimeOnlyAfterModuleInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveforge.yaml")
	if err := os.WriteFile(path, []byte("limits:\n  max_streams: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := core.NewServer(cfg)
	module := &lifecycleRuntimeModule{}
	server.RegisterModule(module)
	manager, err := newRuntimeManager(cfg, path, server)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("limits:\n  max_streams: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := initializeServerAndRuntime(ctx, server, manager); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	defer manager.Close()

	deadline := time.Now().Add(time.Second)
	for manager.Snapshot().Config.Limits.MaxStreams != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := manager.Snapshot().Config.Limits.MaxStreams; got != 2 {
		t.Fatalf("immediate runtime candidate was not applied: max_streams=%d", got)
	}
	if module.reloadBeforeInit.Load() {
		t.Fatal("runtime reload ran before module initialization completed")
	}
}
