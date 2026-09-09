package core

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
)

func TestDiscardFailedAdmissionPreservesReplacementAndConsumers(t *testing.T) {
	for _, scenario := range []string{"empty", "failed", "replacement", "retired replacement", "subscriber"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := newTestStreamConfig()
			cfg.NoPublisherTimeout, cfg.IdleTimeout = 0, 0
			hub := NewStreamHub(cfg, config.LimitsConfig{MaxStreams: 1}, NewEventBus())
			stream, created, err := hub.GetOrCreateWithCreated("test/admission")
			if err != nil || !created {
				t.Fatalf("create=%v err=%v", created, err)
			}
			t.Cleanup(stream.Close)
			if again, newlyCreated, err := hub.GetOrCreateWithCreated(stream.Key()); err != nil || newlyCreated || again != stream {
				t.Fatal("existing stream was reported as newly created")
			}
			failed := &testPublisher{id: "failed"}
			if scenario != "empty" {
				if err := stream.SetPublisher(failed); err != nil {
					t.Fatal(err)
				}
				stream.RemovePublisherIf(failed)
			}
			if scenario == "replacement" || scenario == "retired replacement" {
				next := &testPublisher{id: "replacement"}
				if err := stream.SetPublisher(next); err != nil {
					t.Fatal(err)
				}
				if scenario == "retired replacement" {
					stream.RemovePublisherIf(next)
				}
			}
			if scenario == "subscriber" {
				if err := stream.AddSubscriber("test"); err != nil {
					t.Fatal(err)
				}
			}
			wantDiscard := scenario == "empty" || scenario == "failed"
			if discarded := stream.DiscardFailedAdmission(failed.ID()); discarded != wantDiscard {
				t.Fatalf("discarded=%v want%v", discarded, wantDiscard)
			}
			if wantDiscard {
				if hub.Count() != 0 {
					t.Fatal("discarded stream retained hub capacity")
				}
				replacement, created, err := hub.GetOrCreateWithCreated(stream.Key())
				if err != nil || !created || replacement == stream {
					t.Fatalf("recreate=%v err=%v", created, err)
				}
				t.Cleanup(replacement.Close)
				stream.DiscardFailedAdmission(failed.ID())
				if current, _ := hub.Find(stream.Key()); current != replacement {
					t.Fatal("old cleanup removed replacement instance")
				}
			} else if current, _ := hub.Find(stream.Key()); current != stream {
				t.Fatal("candidate cleanup removed another owner or subscriber")
			}
		})
	}
}
