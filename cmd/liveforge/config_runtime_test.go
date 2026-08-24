package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

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
