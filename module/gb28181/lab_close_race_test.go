package gb28181

import (
	"context"
	"sync"
	"testing"
	"time"
)

type blockedGBLabBYEDialog struct {
	inviteDialog
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockedGBLabBYEDialog) SendBYE(ctx context.Context) error {
	d.once.Do(func() { close(d.entered) })
	select {
	case <-d.release:
		return d.inviteDialog.SendBYE(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestGBLabCleanupWaitsForInFlightSessionBYE(t *testing.T) {
	h := newRealGBLabHarness(t)
	active, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode: LabModePublish, DeviceID: "34020000001320000141",
		ChannelID: "34020000001320000142", StreamKey: "gb28181/close-order",
	})
	if err != nil {
		t.Fatal(err)
	}
	lab := h.module.labs.session(active.ID)
	lab.mu.RLock()
	session, peerDone := lab.moduleSession, lab.peerDone
	lab.mu.RUnlock()
	dialog := &blockedGBLabBYEDialog{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	session.mu.Lock()
	dialog.inviteDialog = session.InviteTx
	session.InviteTx = dialog
	session.mu.Unlock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(dialog.release) }) }
	defer release()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		h.module.handler.closeSession(session, "")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case <-dialog.entered:
	case <-ctx.Done():
		t.Fatal("session cleanup did not reach BYE")
	}
	// The receiver is already closed, so its Lab watcher also starts cleanup.
	// Keep the real dialog in flight while checking that the peer stays alive.
	select {
	case <-peerDone:
		t.Fatal("Lab closed its SIP peer before concurrent session BYE completed")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("session cleanup did not finish after BYE was released")
	}
	waitForGBLab(t, lab.byeReceived.Load)
	waitForGBLabSnapshot(t, h.module, active.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateFailed
	})
	if got := len(h.module.sessions.All()); got != 0 {
		t.Fatalf("completed cleanup left %d media sessions", got)
	}
}
