package gb28181

import (
	"context"
	"errors"
	"testing"
)

func validGBLabRequest() LabSessionRequest {
	return LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000001",
		ChannelID: "34020000001320000002",
		StreamKey: "gb28181/lab",
	}
}

func TestLabManagerRejectsInvalidStartRequest(t *testing.T) {
	manager := NewLabManager()

	_, err := manager.Start(context.Background(), LabSessionRequest{})
	if !errors.Is(err, ErrLabInvalidRequest) {
		t.Fatalf("Start error = %v, want ErrLabInvalidRequest", err)
	}
}

func TestLabManagerListsStartedSession(t *testing.T) {
	manager := NewLabManager()
	want := validGBLabRequest()

	session, err := manager.Start(context.Background(), want)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	listed := manager.List()
	if len(listed) != 1 || listed[0] != session {
		t.Fatalf("List = %+v, want [%+v]", listed, session)
	}
	if listed[0].DeviceID != want.DeviceID || listed[0].ChannelID != want.ChannelID || listed[0].StreamKey != want.StreamKey {
		t.Fatalf("listed session identity = %+v, want device=%q channel=%q stream=%q", listed[0], want.DeviceID, want.ChannelID, want.StreamKey)
	}
}

func TestLabManagerRejectsDuplicateIdentity(t *testing.T) {
	manager := NewLabManager()
	want := validGBLabRequest()
	if _, err := manager.Start(context.Background(), want); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	_, err := manager.Start(context.Background(), want)
	if !errors.Is(err, ErrLabDuplicateIdentity) {
		t.Fatalf("duplicate Start error = %v, want ErrLabDuplicateIdentity", err)
	}
}

func TestLabManagerStopIsIdempotent(t *testing.T) {
	manager := NewLabManager()
	session, err := manager.Start(context.Background(), validGBLabRequest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := manager.Stop(session.ID); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := manager.Stop(session.ID); err != nil {
		t.Fatalf("second Stop: %v, want nil", err)
	}
}
