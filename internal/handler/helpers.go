package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// decodeJSON decodes the request body into dst, rejecting unknown fields so
// typos in client payloads surface as 400s instead of being silently
// ignored.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// paginationParams reads page/page_size query params with sane bounds.
func paginationParams(r *http.Request) (page, pageSize int) {
	page = 1
	pageSize = 20

	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			pageSize = n
		}
	}
	return page, pageSize
}

func totalPages(total int64, pageSize int) int {
	if pageSize <= 0 {
		return 0
	}
	pages := int(total) / pageSize
	if int(total)%pageSize != 0 {
		pages++
	}
	return pages
}

// strPtrOrNil returns nil for an empty string, otherwise a pointer to it.
// Used when an optional foreign-key-like field (e.g. tenant_id on a failed
// login where the tenant couldn't be resolved) may legitimately be absent.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseTimeParam parses an RFC3339 query parameter, returning nil if absent
// or unparsable (callers treat nil as "no bound").
func parseTimeParam(r *http.Request, key string) *time.Time {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil
	}
	return &t
}
