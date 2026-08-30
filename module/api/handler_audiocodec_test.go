//go:build audiocodec

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleServerInfoReportsAudioTranscodingCapability(t *testing.T) {
	h, server := newTestHandlers(t)
	server.Config().AudioCodec.Enabled = true

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/info", nil)
	w := httptest.NewRecorder()
	h.handleServerInfo(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("server info status = %d, want 200", w.Code)
	}

	data := decodeAPIData(t, w.Body.Bytes())
	var info ServerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if !info.Capabilities.AudioTranscoding {
		t.Fatal("audio transcoding capability is false in the audiocodec build")
	}
}
