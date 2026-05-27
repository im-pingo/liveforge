package sip

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

func makeTestInviteAndResponse() (*sip.Request, *sip.Response) {
	invite := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "example.com"})
	invite.SipVersion = "SIP/2.0"

	fromHeader := &sip.FromHeader{
		DisplayName: "Alice",
		Address:     sip.Uri{User: "alice", Host: "example.com"},
		Params:      sip.NewParams(),
	}
	fromHeader.Params.Add("tag", "from-tag-123")
	invite.AppendHeader(fromHeader)

	toHeader := &sip.ToHeader{
		Address: sip.Uri{User: "bob", Host: "example.com"},
	}
	invite.AppendHeader(toHeader)

	callID := sip.CallIDHeader("call-id-abc@example.com")
	invite.AppendHeader(&callID)

	invite.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.INVITE})

	resp := sip.NewResponseFromRequest(invite, 200, "OK", nil)
	if to := resp.To(); to != nil {
		to.Params.Add("tag", "to-tag-456")
	}

	return invite, resp
}

func TestBuildACKHeaders(t *testing.T) {
	invite, resp := makeTestInviteAndResponse()
	ack := buildACK(invite, resp)

	if ack.Method != sip.ACK {
		t.Errorf("expected ACK method, got %s", ack.Method)
	}

	from := ack.From()
	if from == nil {
		t.Fatal("ACK missing From header")
	}
	tag, _ := from.Params.Get("tag")
	if tag != "from-tag-123" {
		t.Errorf("expected from-tag-123, got %s", tag)
	}

	to := ack.To()
	if to == nil {
		t.Fatal("ACK missing To header")
	}
	toTag, _ := to.Params.Get("tag")
	if toTag != "to-tag-456" {
		t.Errorf("expected to-tag-456, got %s", toTag)
	}

	if ack.CallID() == nil {
		t.Fatal("ACK missing Call-ID header")
	}
	if ack.CallID().Value() != "call-id-abc@example.com" {
		t.Errorf("expected call-id-abc@example.com, got %s", ack.CallID().Value())
	}

	cseq := ack.CSeq()
	if cseq == nil {
		t.Fatal("ACK missing CSeq header")
	}
	if cseq.SeqNo != 1 {
		t.Errorf("ACK CSeq should be 1, got %d", cseq.SeqNo)
	}
	if cseq.MethodName != sip.ACK {
		t.Errorf("ACK CSeq method should be ACK, got %s", cseq.MethodName)
	}
}

func TestBuildBYEHeaders(t *testing.T) {
	invite, resp := makeTestInviteAndResponse()
	bye := buildBYE(invite, resp)

	if bye.Method != sip.BYE {
		t.Errorf("expected BYE method, got %s", bye.Method)
	}

	cseq := bye.CSeq()
	if cseq == nil {
		t.Fatal("BYE missing CSeq header")
	}
	if cseq.SeqNo != 2 {
		t.Errorf("BYE CSeq should be INVITE CSeq+1=2, got %d", cseq.SeqNo)
	}
	if cseq.MethodName != sip.BYE {
		t.Errorf("BYE CSeq method should be BYE, got %s", cseq.MethodName)
	}
}

func TestInviteTransactionClose(t *testing.T) {
	tx := &InviteTransaction{
		done: make(chan struct{}),
	}

	select {
	case <-tx.Done():
		t.Fatal("done channel should not be closed yet")
	default:
	}

	tx.Close()

	select {
	case <-tx.Done():
	default:
		t.Fatal("done channel should be closed after Close()")
	}

	// Double close should not panic
	tx.Close()
}

func TestInviteTransactionResponseNilBeforeSet(t *testing.T) {
	tx := &InviteTransaction{
		done: make(chan struct{}),
	}
	if tx.Response() != nil {
		t.Error("response should be nil before being set")
	}
}

func TestInviteTransactionSendACKNoResponse(t *testing.T) {
	tx := &InviteTransaction{
		done: make(chan struct{}),
	}
	err := tx.SendACK(t.Context())
	if err == nil {
		t.Error("expected error when no response")
	}
}

func TestInviteTransactionSendBYENoResponse(t *testing.T) {
	tx := &InviteTransaction{
		done: make(chan struct{}),
	}
	err := tx.SendBYE(t.Context())
	if err == nil {
		t.Error("expected error when no dialog established")
	}
}
