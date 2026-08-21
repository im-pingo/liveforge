package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cancelAfterFirstCheckContext struct{ checks int }

func (*cancelAfterFirstCheckContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*cancelAfterFirstCheckContext) Done() <-chan struct{}       { return nil }
func (*cancelAfterFirstCheckContext) Value(any) any               { return nil }
func (c *cancelAfterFirstCheckContext) Err() error {
	c.checks++
	if c.checks > 1 {
		return context.Canceled
	}
	return nil
}

func TestFileSourceMergesRuntimeOverrideWithoutChangingBase(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "liveforge.yaml")
	overridePath := filepath.Join(dir, "liveforge.runtime.yaml")
	base := "server:\n  name: base\n  log_level: info\nstream:\n  ring_buffer_size: 1024\n"
	if err := os.WriteFile(basePath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}

	source := NewFileSource(basePath, overridePath)
	initial, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial.Config.Server.Name != "base" {
		t.Fatalf("initial server name = %q", initial.Config.Server.Name)
	}

	newRevision, err := source.Store(context.Background(), Patch{
		"server": map[string]any{"log_level": "debug"},
	}, initial.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if newRevision == initial.Revision {
		t.Fatal("revision did not change after override update")
	}

	got, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Server.Name != "base" || got.Config.Server.LogLevel != "debug" {
		t.Fatalf("merged config = name %q level %q", got.Config.Server.Name, got.Config.Server.LogLevel)
	}
	baseAfter, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseAfter) != base {
		t.Fatalf("base file changed:\n%s", baseAfter)
	}
	overrideInfo, err := os.Stat(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if overrideInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("override permissions = %o, want no group/world access", overrideInfo.Mode().Perm())
	}
}

func TestFileSourceRejectsStaleRevision(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "liveforge.yaml")
	if err := os.WriteFile(basePath, []byte("stream:\n  ring_buffer_size: 1024\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := NewFileSource(basePath, filepath.Join(dir, "runtime.yaml"))
	doc, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Store(context.Background(), Patch{"server": map[string]any{"name": "one"}}, doc.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = source.Store(context.Background(), Patch{"server": map[string]any{"name": "two"}}, doc.Revision)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Store stale revision error = %v", err)
	}
}

func TestFileSourceTransactionalWriteRollsBackCancellationBeforeApply(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "liveforge.yaml")
	overridePath := filepath.Join(dir, "runtime.yaml")
	if err := os.WriteFile(basePath, []byte("server:\n  name: original\nstream:\n  ring_buffer_size: 1024\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := NewFileSource(basePath, overridePath)
	initial, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := &cancelAfterFirstCheckContext{}
	applyCalled := false
	_, err = source.StoreAndApply(ctx, Patch{
		"server": map[string]any{"name": "must-roll-back"},
	}, initial.Revision, func(Document) error {
		applyCalled = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StoreAndApply error = %v, want context canceled", err)
	}
	if applyCalled {
		t.Fatal("apply called after context cancellation")
	}
	loaded, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != initial.Revision || loaded.Config.Server.Name != "original" {
		t.Fatalf("source after cancellation = revision %q name %q, want %q/original", loaded.Revision, loaded.Config.Server.Name, initial.Revision)
	}
}

func TestFileSourceExpandsEnvironmentAfterMerge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LIVEFORGE_TEST_SECRET", "expanded")
	basePath := filepath.Join(dir, "liveforge.yaml")
	overridePath := filepath.Join(dir, "runtime.yaml")
	if err := os.WriteFile(basePath, []byte("stream:\n  ring_buffer_size: 1024\nauth:\n  publish:\n    token:\n      secret: base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, []byte("auth:\n  publish:\n    token:\n      secret: ${LIVEFORGE_TEST_SECRET}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := NewFileSource(basePath, overridePath).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Config.Auth.Publish.Token.Secret != "expanded" {
		t.Fatalf("secret = %q", doc.Config.Auth.Publish.Token.Secret)
	}
	if !strings.HasPrefix(doc.Revision, "sha256:") {
		t.Fatalf("revision = %q", doc.Revision)
	}
}

func TestFileSourcePreservesBcryptPasswordHash(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "liveforge.yaml")
	const passwordHash = "$2a$04$kVNzL2JmbM5pHjtDwvdfIuP7Kf0v6g3LDFnVdk0m1gQ5RZLHYQyMu"
	base := "stream:\n  ring_buffer_size: 1024\napi:\n  console:\n    username: admin\n    password_hash: '" + passwordHash + "'\n"
	if err := os.WriteFile(basePath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := NewFileSource(basePath, "").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Config.API.Console.PasswordHash; got != passwordHash {
		t.Fatalf("password hash = %q, want %q", got, passwordHash)
	}
}
