package api

import (
	"testing"
	"time"
)

func TestManagementBodyTimeout(t *testing.T) {
	m, _, _ := newTestModule(t, nil)
	if got := m.httpSrv.ReadTimeout; got != 10*time.Second {
		t.Fatalf("management ReadTimeout=%s want=10s", got)
	}
}
