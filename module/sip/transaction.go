package sip

import (
	"context"
	"fmt"
	"sync"

	"github.com/emiago/sipgo/sip"
)

// InviteTransaction wraps a SIP client INVITE transaction providing
// simplified access to the INVITE/ACK/BYE lifecycle.
type InviteTransaction struct {
	mu       sync.Mutex
	clientTx sip.ClientTransaction
	response *sip.Response
	client   *sipClient
	request  *sip.Request
	done     chan struct{}
	closed   bool
}

// Response returns the final response (200 OK) received for the INVITE.
func (t *InviteTransaction) Response() *sip.Response {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.response
}

// SendACK sends an ACK for the INVITE transaction.
func (t *InviteTransaction) SendACK(ctx context.Context) error {
	t.mu.Lock()
	resp := t.response
	t.mu.Unlock()

	if resp == nil {
		return fmt.Errorf("no response to ACK")
	}

	ack := buildACK(t.request, resp)
	return t.client.writeRequest(ctx, ack)
}

// SendBYE sends a BYE to terminate the dialog.
func (t *InviteTransaction) SendBYE(ctx context.Context) error {
	t.mu.Lock()
	resp := t.response
	t.mu.Unlock()

	if resp == nil {
		return fmt.Errorf("no dialog established")
	}

	bye := buildBYE(t.request, resp)
	return t.client.writeRequest(ctx, bye)
}

// Done returns a channel that is closed when the transaction completes.
func (t *InviteTransaction) Done() <-chan struct{} {
	return t.done
}

// Close terminates the transaction.
func (t *InviteTransaction) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.done)
		if t.clientTx != nil {
			t.clientTx.Terminate()
		}
	}
}

// buildACK creates an ACK request for a 2xx response.
func buildACK(invite *sip.Request, resp *sip.Response) *sip.Request {
	ack := sip.NewRequest(sip.ACK, invite.Recipient)
	ack.SipVersion = invite.SipVersion

	copyDialogHeaders(ack, invite, resp, sip.ACK)
	return ack
}

// buildBYE creates a BYE request for an established dialog.
func buildBYE(invite *sip.Request, resp *sip.Response) *sip.Request {
	bye := sip.NewRequest(sip.BYE, invite.Recipient)
	bye.SipVersion = invite.SipVersion

	copyDialogHeaders(bye, invite, resp, sip.BYE)
	return bye
}

// copyDialogHeaders copies From, To, Call-ID and CSeq into a new in-dialog request.
func copyDialogHeaders(dst, invite *sip.Request, resp *sip.Response, method sip.RequestMethod) {
	// From (same as INVITE)
	if from := invite.From(); from != nil {
		dst.AppendHeader(sip.HeaderClone(from))
	}
	// To (from response, includes tag)
	if to := resp.To(); to != nil {
		dst.AppendHeader(sip.HeaderClone(to))
	}
	// Call-ID
	if callID := invite.CallID(); callID != nil {
		dst.AppendHeader(sip.HeaderClone(callID))
	}
	// CSeq
	cseq := invite.CSeq()
	seqNo := uint32(1)
	if cseq != nil {
		seqNo = cseq.SeqNo
		if method == sip.BYE {
			seqNo++
		}
	}
	dst.AppendHeader(&sip.CSeqHeader{
		SeqNo:      seqNo,
		MethodName: method,
	})
}
