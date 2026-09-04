package metrics

import (
	"fmt"
	"sync"
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

func BenchmarkMetricsCardinalityMatrix(b *testing.B) {
	const streamCount = 1000
	for _, limit := range []int{32, 512} {
		b.Run(fmt.Sprintf("admission_limit=%d", limit), func(b *testing.B) {
			cfg := testConfig()
			cfg.Metrics.StreamDetailLimit = limit
			server := core.NewServer(cfg)
			for i := 0; i < streamCount; i++ {
				if _, err := server.StreamHub().GetOrCreate(fmt.Sprintf("live/cardinality/%04d", i)); err != nil {
					b.Fatal(err)
				}
			}

			b.Run("first_admission", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					registry := prometheus.NewRegistry()
					registry.MustRegister(NewCollector(server))
					if err := gatherStreamKeysBounded(registry, limit); err != nil {
						b.Fatal(err)
					}
				}
			})

			b.Run("stable_gather", func(b *testing.B) {
				registry := prometheus.NewRegistry()
				registry.MustRegister(NewCollector(server))
				if err := gatherStreamKeysBounded(registry, limit); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := gatherStreamKeysBounded(registry, limit); err != nil {
						b.Fatal(err)
					}
				}
			})

			b.Run("concurrent_gather", func(b *testing.B) {
				registry := prometheus.NewRegistry()
				registry.MustRegister(NewCollector(server))
				if err := gatherStreamKeysBounded(registry, limit); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					errs := make(chan error, 8)
					var wait sync.WaitGroup
					for range 8 {
						wait.Add(1)
						go func() {
							defer wait.Done()
							errs <- gatherStreamKeysBounded(registry, limit)
						}()
					}
					wait.Wait()
					close(errs)
					for err := range errs {
						if err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		})
	}

	b.Run("allowlist_1000", func(b *testing.B) {
		cfg := testConfig()
		cfg.Metrics.StreamDetailLimit = streamCount
		cfg.Metrics.StreamDetailAllowlist = make([]string, 0, streamCount+100)
		for i := 0; i < streamCount; i++ {
			key := fmt.Sprintf("live/allow-benchmark/%04d", i)
			cfg.Metrics.StreamDetailAllowlist = append(cfg.Metrics.StreamDetailAllowlist, key)
			if i%10 == 0 {
				cfg.Metrics.StreamDetailAllowlist = append(cfg.Metrics.StreamDetailAllowlist, key)
			}
		}
		server := core.NewServer(cfg)
		for i := 0; i < streamCount; i++ {
			key := fmt.Sprintf("live/allow-benchmark/%04d", i)
			if _, err := server.StreamHub().GetOrCreate(key); err != nil {
				b.Fatal(err)
			}
		}
		collector := NewCollector(server)
		registry := prometheus.NewRegistry()
		registry.MustRegister(collector)
		if err := gatherStreamKeysBounded(registry, streamCount); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if err := gatherStreamKeysBounded(registry, streamCount); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func gatherStreamKeysBounded(registry *prometheus.Registry, limit int) error {
	keys, err := gatherStreamKeys(registry)
	if err != nil {
		return err
	}
	if len(keys) > limit {
		return fmt.Errorf("gather exposed %d stream keys, limit %d", len(keys), limit)
	}
	return nil
}
