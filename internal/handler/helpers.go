package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// maxRequestBodyBytes caps how much of a request body will be read. Without
// it, any JSON endpoint is an unauthenticated memory-exhaustion primitive:
// a client can stream gigabytes into a decoder that is happy to keep going.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// Pagination bounds are resolved from configuration once at boot, so the
// same ceiling applies to every REST list endpoint and to GraphQL without
// each handler carrying a copy.
var (
	defaultPageSize = 20
	maxPageSize     = 100
)

// ConfigurePagination is called once from main with the resolved config.
func ConfigurePagination(defaultSize, maxSize int) {
	if defaultSize > 0 {
		defaultPageSize = defaultSize
	}
	if maxSize > 0 {
		maxPageSize = maxSize
	}
	if defaultPageSize > maxPageSize {
		defaultPageSize = maxPageSize
	}
}

// MaxPageSize exposes the configured ceiling (used by the GraphQL layer and
// reported in API docs).
func MaxPageSize() int { return maxPageSize }

// DefaultPageSize exposes the configured default.
func DefaultPageSize() int { return defaultPageSize }

// decodeJSON decodes the request body into dst. Unknown fields are rejected
// so typos in client payloads surface as 400s instead of being silently
// ignored, and the body is length-limited before it reaches the decoder.
func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxRequestBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Reject trailing content: a body of two concatenated JSON objects is
	// ambiguous, and silently honouring only the first is how request
	// smuggling bugs start.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

// decodeOptionalJSON decodes a body that may legitimately be absent.
func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	err := decodeJSON(r, dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// badBody converts a decode failure into a caller-safe validation error.
// The decoder's message names the offending field, which is genuinely
// useful to an API consumer and reveals nothing internal.
func badBody(err error) error {
	return apierr.BadRequest("the request body could not be parsed").
		WithDetails(map[string]any{"reason": err.Error()})
}

// paginationParams reads page/page_size query parameters, clamping page_size
// to the configured maximum rather than rejecting an oversized request.
//
// Clamping instead of erroring is a deliberate choice: a client asking for
// 10,000 rows gets 100 and a meta block telling it there are more pages,
// which is a usable answer. The ceiling still holds, so no single request
// can ask the database for an unbounded result set.
func paginationParams(r *http.Request) (page, pageSize int) {
	page = 1
	pageSize = defaultPageSize

	q := r.URL.Query()
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	// Accept both spellings: page_size for REST convention, pageSize for
	// clients sharing code with the GraphQL API.
	sizeRaw := q.Get("page_size")
	if sizeRaw == "" {
		sizeRaw = q.Get("pageSize")
	}
	if sizeRaw != "" {
		if n, err := strconv.Atoi(sizeRaw); err == nil && n > 0 {
			pageSize = n
		}
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
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
// Used when an optional foreign-key-like field (e.g. actor_id on a failed
// login, where no user was ever resolved) may legitimately be absent.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseTimeParam parses an RFC3339 query parameter. An unparsable value is
// reported rather than silently ignored: quietly treating a malformed `from`
// as "no lower bound" turns a client typo into a full-table export.
func parseTimeParam(r *http.Request, key string) (*time.Time, error) {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, apierr.BadRequest("%s must be an RFC3339 timestamp, e.g. 2026-01-31T00:00:00Z", key)
	}
	return &t, nil
}

// timeWindow reads the from/to pair every usage and audit query accepts.
func timeWindow(r *http.Request) (from, to *time.Time, err error) {
	if from, err = parseTimeParam(r, "from"); err != nil {
		return nil, nil, err
	}
	if to, err = parseTimeParam(r, "to"); err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && to.Before(*from) {
		return nil, nil, apierr.BadRequest("'to' must not be earlier than 'from'")
	}
	return from, to, nil
}

// errorsIs is a thin alias so the mapping helpers read cleanly without each
// file importing errors purely for one call.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// parseRFC3339Field parses an optional RFC3339 timestamp from a JSON body
// field, reporting a validation error rather than silently ignoring it.
func parseRFC3339Field(name, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, apierr.Validation("%s must be an RFC3339 timestamp, e.g. 2026-01-31T00:00:00Z", name)
	}
	return &t, nil
}
