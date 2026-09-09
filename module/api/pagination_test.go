package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamPaginationFiltersBeforeSlicingAndPreservesLegacy(t *testing.T) {
	h, server := newTestHandlers(t)
	for _, key := range []string{"test/c", "other/x", "test/a", "test/b"} {
		if _, err := server.StreamHub().GetOrCreate(key); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query     string
		count     int
		first     string
		paginated bool
	}{
		{"", 4, "other/x", false},
		{"?q=test&limit=1&offset=1", 1, "test/b", true},
		{"?q=test&limit=1&offset=99", 0, "", true},
	} {
		w := httptest.NewRecorder()
		h.handleStreams(w, httptest.NewRequest(http.MethodGet, "/api/v1/streams"+tc.query, nil))
		var response struct {
			Data       StreamsResponse `json:"data"`
			Pagination *struct {
				Total int `json:"total"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Data.Streams) != tc.count {
			t.Fatalf("query%s count%d want%d", tc.query, len(response.Data.Streams), tc.count)
		}
		if tc.first != "" && response.Data.Streams[0].Key != tc.first {
			t.Fatalf("query%s first=%s", tc.query, response.Data.Streams[0].Key)
		}
		if (response.Pagination != nil) != tc.paginated {
			t.Fatalf("query%s missing/unexpected pagination", tc.query)
		}
		if tc.paginated && response.Pagination.Total != 3 {
			t.Fatalf("total=%d", response.Pagination.Total)
		}
	}
}

func TestListPaginationRejectsInvalidBounds(t *testing.T) {
	h, _ := newTestHandlers(t)
	for _, query := range []string{"?limit=0", "?limit=501", "?limit=no", "?offset=-1", "?offset=99999999999999999999999", "?limit=1&limit=2"} {
		for _, handler := range []http.HandlerFunc{h.handleStreams, h.handleRecordings, h.handleAudit} {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodGet, "/list"+query, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("query%s status%d", query, w.Code)
			}
		}
	}
}

func TestAuditFiltersPaginationAndExport(t *testing.T) {
	h, _ := newTestHandlers(t)
	h.audit.Record(AuditEntry{Principal: "alice", Action: "config:reload", Result: "success"})
	h.audit.Record(AuditEntry{Principal: "bob", Action: "config:reload", Result: "failed"})
	h.audit.Record(AuditEntry{Principal: "alice", Action: "config:reload", Result: "failed"})
	w := httptest.NewRecorder()
	h.handleAudit(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit?principal=alice&result=failed&limit=1", nil))
	var entries []AuditEntry
	if err := json.Unmarshal(decodeAPIData(t, w.Body.Bytes()), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Principal != "alice" || entries[0].Result != "failed" {
		t.Fatalf("filtered entries=%+v", entries)
	}
	w = httptest.NewRecorder()
	h.handleAudit(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit?principal=bob&format=ndjson", nil))
	if w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("export content-type=%s", w.Header().Get("Content-Type"))
	}
	var entry AuditEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entry); err != nil || entry.Principal != "bob" {
		t.Fatalf("export err=%v principal=%s", err, entry.Principal)
	}
}
