package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStringSliceSetAndString(t *testing.T) {
	var values stringSlice
	if err := values.Set("video.fps>=25"); err != nil {
		t.Fatal(err)
	}
	if err := values.Set("audio.codec==aac"); err != nil {
		t.Fatal(err)
	}
	if got := values.String(); got != "video.fps>=25, audio.codec==aac" {
		t.Fatalf("String()=%q", got)
	}
}

func TestOutputFormatHonorsExplicitValue(t *testing.T) {
	if got := outputFormat("human"); got != "human" {
		t.Fatalf("human output=%q", got)
	}
	if got := outputFormat("json"); got != "json" {
		t.Fatalf("json output=%q", got)
	}
}

func TestBuildPushConfigPreservesRealtimeMode(t *testing.T) {
	cfg := buildPushConfig("rtmp", "rtmp://127.0.0.1:1935/live/test", 5*time.Second, "token", true)
	if !cfg.Realtime {
		t.Fatal("realtime push mode was not preserved")
	}
	if cfg.Duration != 5*time.Second || cfg.Token != "token" {
		t.Fatalf("push config = %+v", cfg)
	}
}

func TestErrorClassifiers(t *testing.T) {
	other := errors.New("connection failed")
	for _, tc := range []struct {
		name     string
		classify func(error) string
		fallback string
	}{
		{name: "push", classify: classifyError, fallback: "CONNECT_FAILED"},
		{name: "play", classify: classifyPlayError, fallback: "CONNECT_FAILED"},
		{name: "auth", classify: classifyAuthError, fallback: "AUTH_ERROR"},
		{name: "cluster", classify: classifyClusterError, fallback: "CLUSTER_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.classify(context.DeadlineExceeded); got != "TIMEOUT" {
				t.Fatalf("deadline code=%q want=TIMEOUT", got)
			}
			if got := tc.classify(other); got != tc.fallback {
				t.Fatalf("other error code=%q want=%q", got, tc.fallback)
			}
		})
	}
}

func TestFindLiveforBinaryHonorsEnvironmentAndRejectsMissingPaths(t *testing.T) {
	tempDir := t.TempDir()
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	t.Setenv("GOPATH", filepath.Join(tempDir, "empty-gopath"))

	configured := filepath.Join(tempDir, "custom-liveforge")
	if err := os.WriteFile(configured, []byte("test binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LF_BINARY", configured)
	if got, err := findLiveforBinary(); err != nil || got != configured {
		t.Fatalf("find configured binary=(%q, %v) want=(%q, nil)", got, err, configured)
	}

	t.Setenv("LF_BINARY", filepath.Join(tempDir, "missing-liveforge"))
	if got, err := findLiveforBinary(); err == nil {
		t.Fatalf("missing binary resolved to %q", got)
	}
}
