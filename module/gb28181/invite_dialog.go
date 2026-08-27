package gb28181

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	sipmod "github.com/im-pingo/liveforge/module/sip"
)

const dialogTerminationTimeout = 2 * time.Second

type inviteDialog interface {
	Done() <-chan struct{}
	Response() *sip.Response
	SendACK(context.Context) error
	SendBYE(context.Context) error
	Close()
}

type inviteSender func(context.Context, *sip.Request) (inviteDialog, error)

type managedInviteDialog struct {
	inviteDialog
	byeOnce   sync.Once
	closeOnce sync.Once
	byeErr    error
}

func newManagedInviteDialog(dialog inviteDialog) *managedInviteDialog {
	return &managedInviteDialog{inviteDialog: dialog}
}

func (d *managedInviteDialog) SendBYE(ctx context.Context) error {
	d.byeOnce.Do(func() {
		d.byeErr = d.inviteDialog.SendBYE(ctx)
	})
	return d.byeErr
}

func (d *managedInviteDialog) Close() {
	d.closeOnce.Do(d.inviteDialog.Close)
}

func sendInvite(ctx context.Context, service sipmod.SIPService, override inviteSender, req *sip.Request) (inviteDialog, error) {
	if override != nil {
		return override(ctx, req)
	}
	return service.SendInvite(ctx, req)
}

func terminateAcceptedDialog(dialog inviteDialog) {
	ctx, cancel := context.WithTimeout(context.Background(), dialogTerminationTimeout)
	defer cancel()
	if err := dialog.SendBYE(ctx); err != nil {
		slog.Warn("failed to terminate accepted GB28181 dialog", "module", "gb28181", "error", err)
	}
	dialog.Close()
}

func cleanupInviteDialog(dialog inviteDialog) {
	if dialog == nil {
		return
	}
	response := dialog.Response()
	if response != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		terminateAcceptedDialog(dialog)
		return
	}
	dialog.Close()
	// Closing a transaction can publish a final response that raced the
	// initial response probe. Re-check before deciding no dialog exists.
	response = dialog.Response()
	if response != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		terminateAcceptedDialog(dialog)
	}
}
