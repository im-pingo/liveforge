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

func TestLabManagerRejectsValidStartAsUnavailable(t *testing.T) {
	manager := NewLabManager()

	_, err := manager.Start(context.Background(), validGBLabRequest())
	if !errors.Is(err, ErrLabManagerUnimplemented) {
		t.Fatalf("valid Start error = %v, want ErrLabManagerUnimplemented", err)
	}
	if listed := manager.List(); len(listed) != 0 {
		t.Fatalf("List = %+v, want no transportless sessions", listed)
	}
}

func TestLabManagerDoesNotReserveUnavailableIdentity(t *testing.T) {
	manager := NewLabManager()
	want := validGBLabRequest()
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := manager.Start(context.Background(), want); !errors.Is(err, ErrLabManagerUnimplemented) {
			t.Fatalf("Start attempt %d error = %v, want ErrLabManagerUnimplemented", attempt+1, err)
		}
	}
}

func TestLabManagerStopReportsUnavailable(t *testing.T) {
	manager := NewLabManager()
	if err := manager.Stop("unavailable"); !errors.Is(err, ErrLabManagerUnimplemented) {
		t.Fatalf("Stop error = %v, want ErrLabManagerUnimplemented", err)
	}
}
