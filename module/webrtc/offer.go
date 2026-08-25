package webrtc

import (
	"errors"
	"io"
	"net/http"
)

const maxSDPOfferBytes int64 = 1 << 20

func readSDPOffer(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSDPOfferBytes))
	if err == nil {
		return offer, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "SDP offer exceeds 1048576 bytes", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	http.Error(w, "failed to read offer", http.StatusBadRequest)
	return nil, false
}
