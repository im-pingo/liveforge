package record

import (
	"context"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleRecordingProviderLifecycle(t *testing.T) {
	cfg := &config.Config{Record: config.RecordConfig{Path: t.TempDir() + "/{stream_key}.flv", Format: "flv"}}
	m := NewModule()
	if err := m.Init(core.NewServer(cfg)); err != nil {
		t.Fatal(err)
	}
	runtime := m.runtime.Load()
	object, err := runtime.storage.Create(context.Background(), "live/provider.flv", RecordingInfo{StreamKey: "live/provider", Format: "flv"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Write([]byte("media")); err != nil {
		t.Fatal(err)
	}
	completed, err := object.Complete(context.Background(), RecordingInfo{})
	if err != nil {
		t.Fatal(err)
	}

	items, err := m.ListRecordings(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if _, err := m.Recording(context.Background(), completed.ID); err != nil {
		t.Fatal(err)
	}
	reader, _, err := m.OpenRecording(context.Background(), completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	if err := m.DeleteRecording(context.Background(), completed.ID); err != nil {
		t.Fatal(err)
	}
	if got := m.RecordingStatus(context.Background()).Metrics.FilesDeleted; got != 1 {
		t.Fatalf("deleted metric = %d", got)
	}
}
