package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/prometheus/client_golang/prometheus"
)

type inertConfigSource struct{}

func (inertConfigSource) Load(context.Context, configruntime.Version) (configruntime.Snapshot, error) {
	return configruntime.Snapshot{}, nil
}
func (inertConfigSource) Close() error { return nil }

func testConfig() *config.Config {
	return &config.Config{
		Stream: config.StreamConfig{
			GOPCache:          true,
			GOPCacheNum:       1,
			GOPCacheMaxFrames: config.DefaultGOPCacheMaxFrames,
			GOPCacheMaxBytes:  32 * 1024 * 1024,
			RingBufferSize:    1024,
		},
		Metrics: config.MetricsConfig{
			Enabled:           true,
			Listen:            "127.0.0.1:0", // random loopback port
			Path:              "/metrics",
			StreamDetail:      true,
			StreamDetailLimit: 100,
		},
	}
}

func TestMetricsStreamDetailIsDisabledByDefault(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.StreamDetail = false
	s := core.NewServer(cfg)
	for _, key := range []string{"live/one", "live/two"} {
		if _, err := s.StreamHub().GetOrCreate(key); err != nil {
			t.Fatal(err)
		}
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(NewCollector(s))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "stream_key" {
					t.Fatalf("default metrics exposed unbounded stream label in %s", family.GetName())
				}
			}
		}
	}
}

func TestMetricsStreamDetailCollectorDefensivelyHidesLabelsForNonPositiveDirectConfig(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			cfg := testConfig()
			cfg.Metrics.StreamDetailLimit = limit
			s := core.NewServer(cfg)
			if _, err := s.StreamHub().GetOrCreate("live/hidden"); err != nil {
				t.Fatal(err)
			}
			registry := prometheus.NewRegistry()
			registry.MustRegister(NewCollector(s))
			keys, err := gatherStreamKeys(registry)
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 0 {
				t.Fatalf("stream detail labels = %v, want none", sortedKeys(keys))
			}
		})
	}
}

func TestMetricsStreamDetailAllowlistIsDeduplicatedSortedAndExact(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.StreamDetail = true
	cfg.Metrics.StreamDetailLimit = 2
	cfg.Metrics.StreamDetailAllowlist = []string{"live/z", "live/b", "live/a", "live/a"}
	s := core.NewServer(cfg)
	var firstA *core.Stream
	for _, key := range []string{"live/z", "live/a", "live/b", "live/not-allowed"} {
		stream, err := s.StreamHub().GetOrCreate(key)
		if err != nil {
			t.Fatal(err)
		}
		if key == "live/a" {
			firstA = stream
		}
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(NewCollector(s))
	for range 3 {
		keys, err := gatherSelectedStreamKeys(registry)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamSelection(t, keys, []string{"live/a", "live/b"}, cfg.Metrics.StreamDetailLimit)
	}

	s.StreamHub().Remove("live/a")
	for range 3 {
		keys, err := gatherSelectedStreamKeys(registry)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamSelection(t, keys, []string{"live/b", "live/z"}, cfg.Metrics.StreamDetailLimit)
	}

	recreatedA, err := s.StreamHub().GetOrCreate("live/a")
	if err != nil {
		t.Fatal(err)
	}
	if recreatedA == firstA {
		t.Fatal("same-key recreation reused the removed Stream instance")
	}
	for range 3 {
		keys, err := gatherSelectedStreamKeys(registry)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamSelection(t, keys, []string{"live/a", "live/b"}, cfg.Metrics.StreamDetailLimit)
	}
}

func TestMetricsStreamDetailUsesBoundedStableHubOrder(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.StreamDetailLimit = 2
	s := core.NewServer(cfg)
	for _, key := range []string{"live/z", "live/a", "live/m"} {
		if _, err := s.StreamHub().GetOrCreate(key); err != nil {
			t.Fatal(err)
		}
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(NewCollector(s))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]struct{})
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "stream_key" {
					keys[label.GetValue()] = struct{}{}
				}
			}
		}
	}
	for _, key := range []string{"live/z", "live/a"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("stable stream detail labels = %v, missing %q", keys, key)
		}
	}
	if _, ok := keys["live/m"]; ok {
		t.Fatalf("stable stream detail labels exceeded limit: %v", keys)
	}
}

func TestMetricsStreamDetailLimitBoundsLifetimeLabelsAcrossChurn(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.StreamDetailLimit = 1
	s := core.NewServer(cfg)
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewCollector(s))

	lifetimeKeys := make(map[string]struct{})
	for _, key := range []string{"live/first", "live/second", "live/third"} {
		if _, err := s.StreamHub().GetOrCreate(key); err != nil {
			t.Fatal(err)
		}

		gatherKeys, err := gatherStreamKeys(registry)
		if err != nil {
			t.Fatal(err)
		}
		for gatherKey := range gatherKeys {
			lifetimeKeys[gatherKey] = struct{}{}
		}
		if len(gatherKeys) > cfg.Metrics.StreamDetailLimit {
			t.Fatalf("gather stream detail labels = %v, want at most %d", sortedKeys(gatherKeys), cfg.Metrics.StreamDetailLimit)
		}
		s.StreamHub().Remove(key)
	}

	if len(lifetimeKeys) > cfg.Metrics.StreamDetailLimit {
		t.Fatalf("lifetime stream detail labels = %v, want at most %d", sortedKeys(lifetimeKeys), cfg.Metrics.StreamDetailLimit)
	}
}

func TestMetricsConcurrentGatherProgressesWhileAdmissionLockHeld(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.StreamDetailLimit = 32
	server := core.NewServer(cfg)
	for i := range 64 {
		if _, err := server.StreamHub().GetOrCreate(fmt.Sprintf("live/gather/%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	collector := NewCollector(server)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	if _, err := registry.Gather(); err != nil {
		t.Fatal(err)
	}

	collector.streamDetailMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := registry.Gather()
		done <- err
	}()
	select {
	case err := <-done:
		collector.streamDetailMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		collector.streamDetailMu.Unlock()
		t.Fatal("stable gather waited on the admission mutex")
	}
}

func TestMetricsLargeAllowlistRemainsBounded(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.StreamDetailLimit = 25
	cfg.Metrics.StreamDetailAllowlist = make([]string, 0, 1100)
	for i := 0; i < 1000; i++ {
		cfg.Metrics.StreamDetailAllowlist = append(cfg.Metrics.StreamDetailAllowlist, fmt.Sprintf("live/allow/%04d", i))
		if i%10 == 0 {
			cfg.Metrics.StreamDetailAllowlist = append(cfg.Metrics.StreamDetailAllowlist, fmt.Sprintf("live/allow/%04d", i))
		}
	}
	server := core.NewServer(cfg)
	for i := 0; i < 40; i++ {
		if _, err := server.StreamHub().GetOrCreate(fmt.Sprintf("live/allow/%04d", i)); err != nil {
			t.Fatal(err)
		}
	}
	destroying, err := server.StreamHub().GetOrCreate("live/allow/0000-destroying")
	if err != nil {
		t.Fatal(err)
	}
	destroying.Close()

	registry := prometheus.NewRegistry()
	registry.MustRegister(NewCollector(server))
	keys, err := gatherSelectedStreamKeys(registry)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, cfg.Metrics.StreamDetailLimit)
	for i := 0; i < cfg.Metrics.StreamDetailLimit; i++ {
		want = append(want, fmt.Sprintf("live/allow/%04d", i))
	}
	requireStreamSelection(t, keys, want, cfg.Metrics.StreamDetailLimit)
}

func TestMetricsStreamDetailAdmissionStaysStickyDuringConcurrentMutationAndGathers(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.StreamDetailLimit = 2
	s := core.NewServer(cfg)
	firstAdmitted, err := s.StreamHub().GetOrCreate("live/admitted/one")
	if err != nil {
		t.Fatal(err)
	}
	if _, createErr := s.StreamHub().GetOrCreate("live/admitted/two"); createErr != nil {
		t.Fatal(createErr)
	}
	collector := NewCollector(s)
	barrier := newRegistryGatherBarrier()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector, barrier)

	lifetimeKeys := make(map[string]struct{})
	recordSelection := func(keys []string) {
		for _, key := range keys {
			lifetimeKeys[key] = struct{}{}
		}
	}

	initial, err := gatherSelectedStreamKeys(registry)
	if err != nil {
		t.Fatal(err)
	}
	requireStreamSelection(t, initial, []string{"live/admitted/one", "live/admitted/two"}, cfg.Metrics.StreamDetailLimit)
	recordSelection(initial)

	laterKeys := []string{"live/later/one", "live/later/two", "live/later/three", "live/later/four", "live/later/five"}
	absentSelections := gatherConcurrentlyWithMutation(t, collector, barrier, registry, 8, func() error {
		s.StreamHub().Remove("live/admitted/one")
		for _, key := range laterKeys {
			if _, createErr := s.StreamHub().GetOrCreate(key); createErr != nil {
				return createErr
			}
		}
		return nil
	})
	for _, selection := range absentSelections {
		requireAdmittedStreamSelection(t, selection, []string{"live/admitted/one", "live/admitted/two"}, cfg.Metrics.StreamDetailLimit)
		recordSelection(selection)
	}

	reappearedSelections := gatherConcurrentlyWithMutation(t, collector, barrier, registry, 8, func() error {
		recreated, createErr := s.StreamHub().GetOrCreate("live/admitted/one")
		if createErr != nil {
			return createErr
		}
		if recreated == firstAdmitted {
			return fmt.Errorf("same-key recreation reused the removed Stream instance")
		}
		return nil
	})
	for _, selection := range reappearedSelections {
		requireAdmittedStreamSelection(t, selection, []string{"live/admitted/one", "live/admitted/two"}, cfg.Metrics.StreamDetailLimit)
		recordSelection(selection)
	}

	finalSelection, err := gatherSelectedStreamKeys(registry)
	if err != nil {
		t.Fatal(err)
	}
	requireStreamSelection(t, finalSelection, []string{"live/admitted/one", "live/admitted/two"}, cfg.Metrics.StreamDetailLimit)
	recordSelection(finalSelection)

	if got := sortedKeys(lifetimeKeys); !slices.Equal(got, []string{"live/admitted/one", "live/admitted/two"}) {
		t.Fatalf("lifetime stream detail labels = %v, want only the two admitted keys", got)
	}
	for _, key := range laterKeys {
		if _, exposed := lifetimeKeys[key]; exposed {
			t.Fatalf("later key %q entered lifetime stream detail labels: %v", key, sortedKeys(lifetimeKeys))
		}
	}
}

type streamGatherResult struct {
	keys []string
	err  error
}

type registryGatherBarrier struct {
	desc *prometheus.Desc
	mu   sync.RWMutex

	entered chan<- struct{}
	release <-chan struct{}
}

func newRegistryGatherBarrier() *registryGatherBarrier {
	return &registryGatherBarrier{
		desc: prometheus.NewDesc("liveforge_test_gather_barrier", "Test-only gather synchronization.", nil, nil),
	}
}

func (b *registryGatherBarrier) Describe(ch chan<- *prometheus.Desc) { ch <- b.desc }

func (b *registryGatherBarrier) Collect(ch chan<- prometheus.Metric) {
	b.mu.RLock()
	entered, release := b.entered, b.release
	b.mu.RUnlock()
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
	ch <- prometheus.MustNewConstMetric(b.desc, prometheus.GaugeValue, 1)
}

func (b *registryGatherBarrier) begin(entered chan<- struct{}, release <-chan struct{}) {
	b.mu.Lock()
	b.entered = entered
	b.release = release
	b.mu.Unlock()
}

func (b *registryGatherBarrier) end() {
	b.mu.Lock()
	b.entered = nil
	b.release = nil
	b.mu.Unlock()
}

func gatherConcurrentlyWithMutation(
	t *testing.T,
	collector *Collector,
	barrier *registryGatherBarrier,
	registry *prometheus.Registry,
	gatherCount int,
	mutate func() error,
) [][]string {
	t.Helper()

	// Keep detail admission blocked, and use a registered collector as an
	// explicit signal that every real Registry.Gather is in flight.
	collector.streamDetailMu.Lock()
	gatherStart := make(chan struct{})
	gatherEntered := make(chan struct{}, gatherCount)
	gatherRelease := make(chan struct{})
	barrier.begin(gatherEntered, gatherRelease)
	results := make(chan streamGatherResult, gatherCount)
	var wait sync.WaitGroup
	for range gatherCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-gatherStart
			keys, err := gatherSelectedStreamKeys(registry)
			results <- streamGatherResult{keys: keys, err: err}
		}()
	}
	close(gatherStart)
	for range gatherCount {
		<-gatherEntered
	}

	mutationDone := make(chan error, 1)
	go func() { mutationDone <- mutate() }()
	mutationErr := <-mutationDone
	collector.streamDetailMu.Unlock()
	close(gatherRelease)
	wait.Wait()
	barrier.end()
	close(results)
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}

	selections := make([][]string, 0, gatherCount)
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		selections = append(selections, result.keys)
	}
	return selections
}

func gatherSelectedStreamKeys(registry *prometheus.Registry) ([]string, error) {
	families, err := registry.Gather()
	if err != nil {
		return nil, err
	}
	for _, family := range families {
		if family.GetName() != "liveforge_stream_bytes_in_total" {
			continue
		}
		keys := make([]string, 0, len(family.GetMetric()))
		seen := make(map[string]struct{}, len(family.GetMetric()))
		for _, metric := range family.GetMetric() {
			key := ""
			for _, label := range metric.GetLabel() {
				if label.GetName() == "stream_key" {
					key = label.GetValue()
					break
				}
			}
			if key == "" {
				return nil, fmt.Errorf("stream bytes metric is missing stream_key")
			}
			if _, duplicate := seen[key]; duplicate {
				return nil, fmt.Errorf("stream bytes metric contains duplicate stream_key %q", key)
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys, nil
	}
	return nil, nil
}

func requireStreamSelection(t *testing.T, got, want []string, limit int) {
	t.Helper()
	if len(got) > limit {
		t.Fatalf("stream detail labels = %v, want at most %d", got, limit)
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("stream detail labels contain duplicate key %q: %v", got[i], got)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("stream detail labels = %v, want %v", got, want)
	}
}

func requireAdmittedStreamSelection(t *testing.T, got, allowed []string, limit int) {
	t.Helper()
	if len(got) > limit {
		t.Fatalf("stream detail labels = %v, want at most %d", got, limit)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(got))
	for _, key := range got {
		if _, ok := allowedSet[key]; !ok {
			t.Fatalf("stream detail labels contain non-admitted key %q: %v", key, got)
		}
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("stream detail labels contain duplicate key %q: %v", key, got)
		}
		seen[key] = struct{}{}
	}
}

func gatherStreamKeys(registry *prometheus.Registry) (map[string]struct{}, error) {
	families, err := registry.Gather()
	if err != nil {
		return nil, err
	}
	keys := make(map[string]struct{})
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "stream_key" {
					keys[label.GetValue()] = struct{}{}
				}
			}
		}
	}
	return keys, nil
}

func sortedKeys(keys map[string]struct{}) []string {
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func TestMetricsModuleStartStop(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	addr := m.Addr()
	if addr == nil {
		t.Fatal("expected listener address")
	}

	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Verify server-level metrics are present
	if !strings.Contains(content, "liveforge_server_streams_active") {
		t.Error("missing liveforge_server_streams_active metric")
	}
	if !strings.Contains(content, "liveforge_server_connections_active") {
		t.Error("missing liveforge_server_connections_active metric")
	}
	if !strings.Contains(content, "liveforge_server_uptime_seconds") {
		t.Error("missing liveforge_server_uptime_seconds metric")
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)
	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	if got := m.httpSrv.ReadHeaderTimeout; got != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want %v", got, 5*time.Second)
	}
	if got := m.httpSrv.IdleTimeout; got != 2*time.Minute {
		t.Errorf("IdleTimeout = %v, want %v", got, 2*time.Minute)
	}
	if got := m.httpSrv.WriteTimeout; got != 0 {
		t.Errorf("WriteTimeout = %v, want unchanged zero value", got)
	}
}

func TestMetricsExposeRuntimeConfigHealth(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	manager, err := configruntime.NewManager(configruntime.Options{Source: inertConfigSource{}, SourceName: "test", Initial: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	s.SetConfigManager(manager)
	m := NewModule()
	s.RegisterModule(m)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	resp, err := http.Get("http://" + m.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, name := range []string{"liveforge_config_consecutive_failures", "liveforge_config_pending_restart", "liveforge_config_callback_failures"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("missing %s", name)
		}
	}
	for _, result := range []string{"accepted", "rejected", "application_failed"} {
		metric := `liveforge_config_changes_total{result="` + result + `"}`
		if !strings.Contains(string(body), metric) {
			t.Errorf("missing bounded config change metric %s", metric)
		}
	}
}

type securityMetricStub struct{}

func (securityMetricStub) Name() string                   { return "api" }
func (securityMetricStub) Init(*core.Server) error        { return nil }
func (securityMetricStub) Hooks() []core.HookRegistration { return nil }
func (securityMetricStub) Close() error                   { return nil }
func (securityMetricStub) SecurityMetricValues() map[string]float64 {
	return map[string]float64{"authentication_failures": 2, "authorization_failures": 3, "rate_limit_denials": 4, "audit_events": 5}
}

type collectorStub struct{ gauge prometheus.Gauge }

func newCollectorStub() *collectorStub {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "liveforge_test_module_value", Help: "test"})
	gauge.Set(7)
	return &collectorStub{gauge: gauge}
}
func (*collectorStub) Name() string                   { return "collector-stub" }
func (*collectorStub) Init(*core.Server) error        { return nil }
func (*collectorStub) Hooks() []core.HookRegistration { return nil }
func (*collectorStub) Close() error                   { return nil }
func (s *collectorStub) PrometheusCollectors() []prometheus.Collector {
	return []prometheus.Collector{s.gauge}
}

func TestMetricsRegistersModuleCollectors(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	s.RegisterModule(newCollectorStub())
	m := NewModule()
	s.RegisterModule(m)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	resp, err := http.Get("http://" + m.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "liveforge_test_module_value 7") {
		t.Fatalf("module collector missing from %s", body)
	}
}

func TestMetricsExposeManagementSecurityCounters(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	s.RegisterModule(securityMetricStub{})
	m := NewModule()
	s.RegisterModule(m)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	resp, err := http.Get("http://" + m.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, metric := range []string{
		"liveforge_api_authentication_failures_total 2",
		"liveforge_api_authorization_failures_total 3",
		"liveforge_api_rate_limit_denials_total 4",
		"liveforge_api_audit_events_total 5",
	} {
		if !strings.Contains(string(body), metric) {
			t.Errorf("missing %q in metrics output", metric)
		}
	}
}

type stubPublisher struct {
	id        string
	mediaInfo *avframe.MediaInfo
}

func (p *stubPublisher) ID() string                    { return p.id }
func (p *stubPublisher) MediaInfo() *avframe.MediaInfo { return p.mediaInfo }
func (p *stubPublisher) Close() error                  { return nil }

func TestMetricsWithStreams(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	// Create a stream with a publisher
	stream, err := s.StreamHub().GetOrCreate("live/test")
	if err != nil {
		t.Fatalf("create stream failed: %v", err)
	}

	pub := &stubPublisher{
		id: "rtmp-pub-1",
		mediaInfo: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("set publisher failed: %v", err)
	}

	// Write some frames to generate stats
	videoFrame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeKeyframe,
		Codec:     avframe.CodecH264,
		DTS:       0,
		PTS:       0,
		Payload:   make([]byte, 1000),
	}
	stream.WriteFrame(videoFrame)

	audioFrame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		FrameType: avframe.FrameTypeInterframe,
		Codec:     avframe.CodecAAC,
		DTS:       0,
		PTS:       0,
		Payload:   make([]byte, 200),
	}
	stream.WriteFrame(audioFrame)

	// Give a moment for stats to propagate
	time.Sleep(10 * time.Millisecond)

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Verify stream-level metrics
	if !strings.Contains(content, `liveforge_stream_bytes_in_total{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_bytes_in_total for live/test")
	}
	if !strings.Contains(content, `liveforge_stream_video_frames_total{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_video_frames_total for live/test")
	}
	if !strings.Contains(content, `liveforge_stream_audio_frames_total{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_audio_frames_total for live/test")
	}
	if !strings.Contains(content, `liveforge_stream_gop_cache_frames{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_gop_cache_frames for live/test")
	}

	// Verify Go runtime metrics are present
	if !strings.Contains(content, "go_goroutines") {
		t.Error("missing go_goroutines metric")
	}
}

func TestMetricsModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "metrics" {
		t.Errorf("expected name 'metrics', got '%s'", m.Name())
	}
}

func TestMetricsDefaultPath(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.Path = "" // empty should default to /metrics
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// TestPerStreamGaugesCreated verifies that per-stream gauges (bitrate, fps, uptime,
// gop_cache_frames) are emitted for each active stream.
func TestPerStreamGaugesCreated(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	stream, err := s.StreamHub().GetOrCreate("live/gauges")
	if err != nil {
		t.Fatalf("create stream failed: %v", err)
	}

	pub := &stubPublisher{
		id:        "pub-gauges",
		mediaInfo: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("set publisher failed: %v", err)
	}

	// Write a keyframe to populate GOP cache and stats
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeKeyframe,
		Codec:     avframe.CodecH264,
		DTS:       0, PTS: 0,
		Payload: make([]byte, 2000),
	})

	time.Sleep(10 * time.Millisecond)

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Per-stream gauges that should be present
	expectedMetrics := []string{
		`liveforge_stream_bitrate_kbps{stream_key="live/gauges"}`,
		`liveforge_stream_fps{stream_key="live/gauges"}`,
		`liveforge_stream_uptime_seconds{stream_key="live/gauges"}`,
		`liveforge_stream_gop_cache_frames{stream_key="live/gauges"}`,
	}
	for _, metric := range expectedMetrics {
		if !strings.Contains(content, metric) {
			t.Errorf("missing per-stream gauge: %s", metric)
		}
	}
}

// TestCollectMultipleStreams verifies that metrics are emitted for multiple
// concurrently active streams.
func TestCollectMultipleStreams(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	streamKeys := []string{"live/stream_a", "live/stream_b", "live/stream_c"}
	for i, key := range streamKeys {
		stream, err := s.StreamHub().GetOrCreate(key)
		if err != nil {
			t.Fatalf("create stream %s failed: %v", key, err)
		}
		pub := &stubPublisher{
			id:        fmt.Sprintf("pub-%d", i),
			mediaInfo: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
		}
		if err := stream.SetPublisher(pub); err != nil {
			t.Fatalf("set publisher for %s failed: %v", key, err)
		}
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			FrameType: avframe.FrameTypeKeyframe,
			Codec:     avframe.CodecH264,
			DTS:       int64(i * 1000), PTS: int64(i * 1000),
			Payload: make([]byte, 500*(i+1)),
		})
	}

	time.Sleep(10 * time.Millisecond)

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Verify active stream count
	if !strings.Contains(content, "liveforge_server_streams_active 3") {
		t.Error("expected liveforge_server_streams_active to be 3")
	}

	// Verify per-stream metrics for each stream
	for _, key := range streamKeys {
		label := fmt.Sprintf(`stream_key="%s"`, key)
		if !strings.Contains(content, label) {
			t.Errorf("missing metrics for stream %s", key)
		}
	}
}

// TestCollectCleanupRemovedStreams verifies that after a stream is removed from the
// hub, its metrics are no longer emitted.
func TestCollectCleanupRemovedStreams(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	// Create two streams
	for _, key := range []string{"live/keep", "live/remove"} {
		stream, err := s.StreamHub().GetOrCreate(key)
		if err != nil {
			t.Fatalf("create stream %s failed: %v", key, err)
		}
		pub := &stubPublisher{
			id:        "pub-" + key,
			mediaInfo: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
		}
		if err := stream.SetPublisher(pub); err != nil {
			t.Fatalf("set publisher for %s failed: %v", key, err)
		}
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			FrameType: avframe.FrameTypeKeyframe,
			Codec:     avframe.CodecH264,
			DTS:       0, PTS: 0,
			Payload: make([]byte, 500),
		})
	}

	time.Sleep(10 * time.Millisecond)

	// Verify both streams appear before removal
	addr := m.Addr()
	resp1, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	content1 := string(body1)

	if !strings.Contains(content1, `stream_key="live/keep"`) {
		t.Error("live/keep should be present before removal")
	}
	if !strings.Contains(content1, `stream_key="live/remove"`) {
		t.Error("live/remove should be present before removal")
	}

	// Remove the stream from the hub
	s.StreamHub().Remove("live/remove")

	time.Sleep(10 * time.Millisecond)

	// Verify only the kept stream remains
	resp2, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed after removal: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	content2 := string(body2)

	if !strings.Contains(content2, `stream_key="live/keep"`) {
		t.Error("live/keep should still be present after removing live/remove")
	}
	if strings.Contains(content2, `stream_key="live/remove"`) {
		t.Error("live/remove should not appear after being removed from the hub")
	}

	// Stream count should now be 1
	if !strings.Contains(content2, "liveforge_server_streams_active 1") {
		t.Error("expected liveforge_server_streams_active to be 1 after removal")
	}
}
