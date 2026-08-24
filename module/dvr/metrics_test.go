package dvr

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPrometheusCollectorExportsFixedDVRMetrics(t *testing.T) {
	m := NewModule()
	m.metrics.segmentsWritten.Store(3)
	m.metrics.cleanupDeleted.Store(2)
	m.metrics.cleanupFailures.Store(1)
	collectors := m.PrometheusCollectors()
	if len(collectors) != 1 {
		t.Fatalf("collectors=%d want=1", len(collectors))
	}
	want := `
# HELP liveforge_dvr_cleanup_deleted_total Total expired DVR segments deleted.
# TYPE liveforge_dvr_cleanup_deleted_total counter
liveforge_dvr_cleanup_deleted_total 2
# HELP liveforge_dvr_cleanup_failures_total Total DVR segment cleanup failures.
# TYPE liveforge_dvr_cleanup_failures_total counter
liveforge_dvr_cleanup_failures_total 1
# HELP liveforge_dvr_segments_written_total Total DVR segments finalized successfully.
# TYPE liveforge_dvr_segments_written_total counter
liveforge_dvr_segments_written_total 3
`
	if err := testutil.CollectAndCompare(collectors[0], strings.NewReader(want),
		"liveforge_dvr_cleanup_deleted_total", "liveforge_dvr_cleanup_failures_total", "liveforge_dvr_segments_written_total"); err != nil {
		t.Fatal(err)
	}
}
