package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/dvr"
	"github.com/im-pingo/liveforge/module/record"
)

type recordingReadSeekCloser struct{ *bytes.Reader }

func (recordingReadSeekCloser) Close() error { return nil }

type recordingProviderStub struct {
	items   []record.RecordingInfo
	content []byte
	deleted string
}

func (*recordingProviderStub) Name() string                   { return "record" }
func (*recordingProviderStub) Init(*core.Server) error        { return nil }
func (*recordingProviderStub) Hooks() []core.HookRegistration { return nil }
func (*recordingProviderStub) Close() error                   { return nil }
func (m *recordingProviderStub) ListRecordings(context.Context) ([]record.RecordingInfo, error) {
	return append([]record.RecordingInfo(nil), m.items...), nil
}
func (m *recordingProviderStub) Recording(_ context.Context, id string) (record.RecordingInfo, error) {
	for _, item := range m.items {
		if item.ID == id {
			return item, nil
		}
	}
	return record.RecordingInfo{}, record.ErrRecordingNotFound
}
func (m *recordingProviderStub) OpenRecording(ctx context.Context, id string) (record.ReadSeekCloser, record.RecordingInfo, error) {
	info, err := m.Recording(ctx, id)
	if err != nil {
		return nil, record.RecordingInfo{}, err
	}
	return recordingReadSeekCloser{bytes.NewReader(m.content)}, info, nil
}
func (m *recordingProviderStub) DeleteRecording(_ context.Context, id string) error {
	if _, err := m.Recording(context.Background(), id); err != nil {
		return err
	}
	m.deleted = id
	return nil
}
func (m *recordingProviderStub) RecordingStatus(context.Context) record.RecordingStatusSnapshot {
	return record.RecordingStatusSnapshot{Metrics: record.RecordingMetricsSnapshot{FilesCompleted: 1}}
}

type dvrStatusProviderStub struct {
	status  dvr.DVRStatusSnapshot
	details map[string]dvr.DVRSessionStatus
}

func (*dvrStatusProviderStub) Name() string                       { return "dvr" }
func (*dvrStatusProviderStub) Init(*core.Server) error            { return nil }
func (*dvrStatusProviderStub) Hooks() []core.HookRegistration     { return nil }
func (*dvrStatusProviderStub) Close() error                       { return nil }
func (m *dvrStatusProviderStub) DVRStatus() dvr.DVRStatusSnapshot { return m.status }
func (m *dvrStatusProviderStub) DVRSession(key string) (dvr.DVRSessionStatus, bool) {
	status, ok := m.details[key]
	return status, ok
}

func TestRecordingManagementHandlers(t *testing.T) {
	completedAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	provider := &recordingProviderStub{
		items:   []record.RecordingInfo{{ID: "live/cam.flv", State: record.RecordingCompleted, CompletedAt: completedAt}},
		content: []byte("recording-data"),
	}
	server := core.NewServer(newTestConfig())
	server.RegisterModule(provider)
	h := NewHandlers(server)

	list := httptest.NewRecorder()
	h.handleRecordings(list, httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "live/cam.flv") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}

	status := httptest.NewRecorder()
	h.handleRecordingStatus(status, httptest.NewRequest(http.MethodGet, "/api/v1/recordings/status", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"files_completed":1`) {
		t.Fatalf("status=%d body=%s", status.Code, status.Body.String())
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/recordings/live/cam.flv", nil)
	detailReq.SetPathValue("id", "live/cam.flv")
	detail := httptest.NewRecorder()
	h.handleRecording(detail, detailReq)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "live/cam.flv") {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}

	downloadReq := httptest.NewRequest(http.MethodGet, "/api/v1/recordings/live/cam.flv/download", nil)
	downloadReq.SetPathValue("id", "live/cam.flv")
	download := httptest.NewRecorder()
	h.handleRecordingDownload(download, downloadReq)
	body, err := io.ReadAll(download.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if download.Code != http.StatusOK || string(body) != "recording-data" || !strings.Contains(download.Header().Get("Content-Disposition"), "cam.flv") {
		t.Fatalf("download status=%d headers=%v body=%q", download.Code, download.Header(), body)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/recordings/live/cam.flv", nil)
	deleteReq.SetPathValue("id", "live/cam.flv")
	deleted := httptest.NewRecorder()
	h.handleRecording(deleted, deleteReq)
	if deleted.Code != http.StatusOK || provider.deleted != "live/cam.flv" {
		t.Fatalf("delete status=%d id=%q", deleted.Code, provider.deleted)
	}
}

func TestRecordingHandlerMapsProviderErrors(t *testing.T) {
	provider := &recordingProviderStub{}
	server := core.NewServer(newTestConfig())
	server.RegisterModule(provider)
	h := NewHandlers(server)

	for _, test := range []struct {
		id   string
		want int
	}{
		{"missing.flv", http.StatusNotFound},
		{"../escape.flv", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/recordings/"+test.id, nil)
		req.SetPathValue("id", test.id)
		w := httptest.NewRecorder()
		if test.id == "../escape.flv" {
			writeRecordingError(w, record.ErrInvalidRecordingID)
		} else {
			h.handleRecording(w, req)
		}
		if w.Code != test.want {
			t.Errorf("id=%q status=%d want=%d body=%s", test.id, w.Code, test.want, w.Body.String())
		}
	}
	if got := recordingHTTPStatus(errors.New("storage unavailable")); got != http.StatusInternalServerError {
		t.Fatalf("unexpected storage error status=%d", got)
	}
}

func TestDVRStatusAndDetailHandlers(t *testing.T) {
	provider := &dvrStatusProviderStub{
		status:  dvr.DVRStatusSnapshot{Sessions: []dvr.DVRSessionStatus{{StreamKey: "live/cam", Segments: 3}}},
		details: map[string]dvr.DVRSessionStatus{"live/cam": {StreamKey: "live/cam", Segments: 3}},
	}
	server := core.NewServer(newTestConfig())
	server.RegisterModule(provider)
	h := NewHandlers(server)

	status := httptest.NewRecorder()
	h.handleDVRStatus(status, httptest.NewRequest(http.MethodGet, "/api/v1/dvr/status", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"segments":3`) {
		t.Fatalf("status=%d body=%s", status.Code, status.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dvr/sessions/live/cam", nil)
	req.SetPathValue("stream_key", "live/cam")
	detail := httptest.NewRecorder()
	h.handleDVRSession(detail, req)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "live/cam") {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}

	missingReq := httptest.NewRequest(http.MethodGet, "/api/v1/dvr/sessions/missing", nil)
	missingReq.SetPathValue("stream_key", "missing")
	missing := httptest.NewRecorder()
	h.handleDVRSession(missing, missingReq)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestRecordingAndDVRRoutesUseRegisteredProviders(t *testing.T) {
	recordProvider := &recordingProviderStub{
		items:   []record.RecordingInfo{{ID: "live/cam.flv", State: record.RecordingCompleted}},
		content: []byte("media"),
	}
	dvrProvider := &dvrStatusProviderStub{
		status:  dvr.DVRStatusSnapshot{Sessions: []dvr.DVRSessionStatus{{StreamKey: "live/cam"}}},
		details: map[string]dvr.DVRSessionStatus{"live/cam": {StreamKey: "live/cam"}},
	}
	server := core.NewServer(newTestConfig())
	server.RegisterModule(recordProvider)
	server.RegisterModule(dvrProvider)
	mux := http.NewServeMux()
	RegisterRoutes(mux, server)

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v1/recordings", http.StatusOK},
		{http.MethodGet, "/api/v1/recordings/status", http.StatusOK},
		{http.MethodGet, "/api/v1/recordings/live/cam.flv", http.StatusOK},
		{http.MethodGet, "/api/v1/recordings/live/cam.flv/download", http.StatusOK},
		{http.MethodDelete, "/api/v1/recordings/live/cam.flv", http.StatusOK},
		{http.MethodGet, "/api/v1/dvr/status", http.StatusOK},
		{http.MethodGet, "/api/v1/dvr/sessions/live/cam", http.StatusOK},
	} {
		req := httptest.NewRequest(test.method, test.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != test.want {
			t.Errorf("%s %s status=%d want=%d body=%s", test.method, test.path, w.Code, test.want, w.Body.String())
		}
	}
}
