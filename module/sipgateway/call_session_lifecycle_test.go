package sipgateway

import (
	"testing"

	"github.com/im-pingo/liveforge/core"
)

func TestCallSessionPublishLifecycleRejectsAdmissionError(t *testing.T) {
	session := &CallSession{state: CallStateActive}
	if session.startPublishLifecycle(func() error { return core.ErrAsyncBackpressure }) {
		t.Fatal("publish lifecycle started after admission error")
	}
	if session.publishLifecycleStarted() {
		t.Fatal("failed admission was retained as a started publish lifecycle")
	}
}
