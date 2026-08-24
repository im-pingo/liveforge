package record

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPrometheusCollectorExportsFixedRecordingMetrics(t *testing.T) {
	m := NewModule()
	m.metrics.completed.Store(2)
	m.metrics.failed.Store(1)
	m.metrics.bytesWritten.Store(42)
	collectors := m.PrometheusCollectors()
	if len(collectors) != 1 {
		t.Fatalf("collectors=%d want=1", len(collectors))
	}
	want := `
# HELP liveforge_record_bytes_written_total Total bytes written to recordings.
# TYPE liveforge_record_bytes_written_total counter
liveforge_record_bytes_written_total 42
# HELP liveforge_record_files_completed_total Total recordings completed successfully.
# TYPE liveforge_record_files_completed_total counter
liveforge_record_files_completed_total 2
# HELP liveforge_record_files_failed_total Total recordings preserved in failed state.
# TYPE liveforge_record_files_failed_total counter
liveforge_record_files_failed_total 1
`
	if err := testutil.CollectAndCompare(collectors[0], strings.NewReader(want),
		"liveforge_record_bytes_written_total", "liveforge_record_files_completed_total", "liveforge_record_files_failed_total"); err != nil {
		t.Fatal(err)
	}
}
