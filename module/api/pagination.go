package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

type pagination struct {
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	Total   int  `json:"total"`
	HasMore bool `json:"has_more"`
}

func readPagination(r *http.Request) (*pagination, error) {
	query := r.URL.Query()
	if !query.Has("limit") && !query.Has("offset") {
		return nil, nil
	}
	page := &pagination{Limit: 100}
	for name, target := range map[string]*int{"limit": &page.Limit, "offset": &page.Offset} {
		if !query.Has(name) {
			continue
		}
		values := query[name]
		if len(values) != 1 {
			return nil, fmt.Errorf("%s must occur once", name)
		}
		value, err := strconv.Atoi(values[0])
		if err != nil || value < 0 || (name == "limit" && (value < 1 || value > 500)) {
			return nil, fmt.Errorf("limit must be 1..500 and offset must be a non-negative integer")
		}
		*target = value
	}
	return page, nil
}

func pageItems[T any](items []T, page *pagination) []T {
	if page == nil {
		return items
	}
	page.Total = len(items)
	start := min(page.Offset, len(items))
	end := start + min(page.Limit, len(items)-start)
	page.HasMore = end < len(items)
	return items[start:end]
}

func writePageJSON(w http.ResponseWriter, data any, page *pagination) {
	w.Header().Set("Cache-Control", "no-store")
	if page != nil {
		w.Header().Set("X-Total-Count", strconv.Itoa(page.Total))
		w.Header().Set("X-Limit", strconv.Itoa(page.Limit))
		w.Header().Set("X-Offset", strconv.Itoa(page.Offset))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		apiResponse
		Pagination *pagination `json:"pagination,omitempty"`
	}{apiResponse: apiResponse{Code: 0, Message: "ok", Data: data}, Pagination: page})
}
