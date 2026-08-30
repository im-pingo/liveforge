package metrics

import (
	"fmt"
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/prometheus/client_golang/prometheus"
)

func BenchmarkCollectorGatherStreamDetails(b *testing.B) {
	cfg := testConfig()
	cfg.Metrics.StreamDetailLimit = 32
	server := core.NewServer(cfg)
	for i := range 128 {
		if _, err := server.StreamHub().GetOrCreate(fmt.Sprintf("live/benchmark/%03d", i)); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("first_gather_admission", func(b *testing.B) {
		b.ReportAllocs()
		b.StopTimer()
		for range b.N {
			registry := prometheus.NewRegistry()
			registry.MustRegister(NewCollector(server))
			b.StartTimer()
			if _, err := registry.Gather(); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
		}
	})

	b.Run("steady_gather", func(b *testing.B) {
		registry := prometheus.NewRegistry()
		registry.MustRegister(NewCollector(server))
		if _, err := registry.Gather(); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if _, err := registry.Gather(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
