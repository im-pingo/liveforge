package gb28181

import (
	"context"
	"log/slog"
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
}
