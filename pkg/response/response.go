// Package response provides the single JSON envelope shape used by every
// BridgeCore REST endpoint, so API consumers never have to branch on
// response structure.
//
// Errors are always rendered as a structured object with a stable code:
//
//	{
//	  "success": false,
//	  "message": "you do not have permission to perform this operation",
//	  "error": { "code": "FORBIDDEN", "message": "..." },
//	  "request_id": "b1c9..."
//	}
//
// The code is the contract; the message is for humans and may be reworded.
package response

import (
	"encoding/json"
	"net/http"

	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// Envelope is the canonical response shape for every BridgeCore endpoint.
type Envelope struct {
	Success   bool       `json:"success"`
	Message   string     `json:"message"`
	Data      any        `json:"data,omitempty"`
	Error     *ErrorBody `json:"error,omitempty"`
	RequestID string     `json:"request_id,omitempty"`
}

// ErrorBody is the structured error payload. Details is optional, always
// caller-safe context (never internal diagnostics).
type ErrorBody struct {
	Code    apierr.Code `json:"code"`
	Message string      `json:"message"`
	Details any         `json:"details,omitempty"`
}

// Meta carries pagination info and is embedded in Data for list endpoints.
type Meta struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	TotalCount int64 `json:"total_count"`
	TotalPages int   `json:"total_pages"`
	HasNext    bool  `json:"has_next"`
}

// NewMeta builds pagination metadata, deriving total pages and the
// has-next hint so clients do not have to compute them.
func NewMeta(page, pageSize int, totalCount int64) Meta {
	pages := 0
	if pageSize > 0 {
		pages = int(totalCount) / pageSize
		if int(totalCount)%pageSize != 0 {
			pages++
		}
	}
	return Meta{
		Page:       page,
		PageSize:   pageSize,
		TotalCount: totalCount,
		TotalPages: pages,
		HasNext:    page < pages,
	}
}

// ListResponse is the standard shape for paginated collection endpoints.
type ListResponse struct {
	Items any  `json:"items"`
	Meta  Meta `json:"meta"`
}

// RequestIDFunc resolves the correlation ID for the request being answered.
// It is wired once at boot (to the middleware's context accessor) so this
// package does not import middleware and create an import cycle.
var RequestIDFunc func(r *http.Request) string

func requestID(r *http.Request) string {
	if RequestIDFunc == nil || r == nil {
		return ""
	}
	return RequestIDFunc(r)
}

func write(w http.ResponseWriter, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// JSON writes an arbitrary success envelope with an explicit status code.
func JSON(w http.ResponseWriter, r *http.Request, status int, message string, data any) {
	write(w, status, Envelope{Success: true, Message: message, Data: data, RequestID: requestID(r)})
}

// OK writes a 200 success envelope.
func OK(w http.ResponseWriter, message string, data any) {
	write(w, http.StatusOK, Envelope{Success: true, Message: message, Data: data})
}

// OKWithRequest writes a 200 success envelope including the correlation ID.
func OKWithRequest(w http.ResponseWriter, r *http.Request, message string, data any) {
	JSON(w, r, http.StatusOK, message, data)
}

// Created writes a 201 success envelope.
func Created(w http.ResponseWriter, message string, data any) {
	write(w, http.StatusCreated, Envelope{Success: true, Message: message, Data: data})
}

// Accepted writes a 202 envelope, used when work has been queued rather
// than completed (for example an asynchronous usage export).
func Accepted(w http.ResponseWriter, message string, data any) {
	write(w, http.StatusAccepted, Envelope{Success: true, Message: message, Data: data})
}

// NoContent writes a 204 with no body.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Fail renders any error using its stable code and status. This is the only
// error path handlers should use: it guarantees the envelope shape and that
// internal causes stay out of the response body.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	e := apierr.From(err)
	write(w, e.Status, Envelope{
		Success: false,
		Message: e.Message,
		Error: &ErrorBody{
			Code:    e.Code,
			Message: e.Message,
			Details: e.Details,
		},
		RequestID: requestID(r),
	})
}

// Err writes an error envelope with an explicit status code, inferring a
// code from the status. Prefer Fail with a typed *apierr.Error.
func Err(w http.ResponseWriter, status int, message string, details any) {
	write(w, status, Envelope{
		Success: false,
		Message: message,
		Error: &ErrorBody{
			Code:    codeForStatus(status),
			Message: message,
			Details: details,
		},
	})
}

// BadRequest writes a 400 error envelope.
func BadRequest(w http.ResponseWriter, message string, details any) {
	Err(w, http.StatusBadRequest, message, details)
}

// Unauthorized writes a 401 error envelope.
func Unauthorized(w http.ResponseWriter, message string) {
	Err(w, http.StatusUnauthorized, message, nil)
}

// Forbidden writes a 403 error envelope.
func Forbidden(w http.ResponseWriter, message string) {
	Err(w, http.StatusForbidden, message, nil)
}

// NotFound writes a 404 error envelope.
func NotFound(w http.ResponseWriter, message string) {
	Err(w, http.StatusNotFound, message, nil)
}

// Conflict writes a 409 error envelope.
func Conflict(w http.ResponseWriter, message string) {
	Err(w, http.StatusConflict, message, nil)
}

// TooManyRequests writes a 429 error envelope.
func TooManyRequests(w http.ResponseWriter, message string) {
	Err(w, http.StatusTooManyRequests, message, nil)
}

// InternalError writes a 500 error envelope. Detail is intentionally
// generic — internal error detail belongs in logs, not API responses.
func InternalError(w http.ResponseWriter, message string) {
	Err(w, http.StatusInternalServerError, message, nil)
}

func codeForStatus(status int) apierr.Code {
	switch status {
	case http.StatusBadRequest:
		return apierr.CodeBadRequest
	case http.StatusUnauthorized:
		return apierr.CodeUnauthenticated
	case http.StatusForbidden:
		return apierr.CodeForbidden
	case http.StatusNotFound:
		return apierr.CodeNotFound
	case http.StatusConflict:
		return apierr.CodeConflict
	case http.StatusUnprocessableEntity:
		return apierr.CodeValidation
	case http.StatusTooManyRequests:
		return apierr.CodeRateLimited
	case http.StatusServiceUnavailable:
		return apierr.CodeUnavailable
	default:
		if status >= 500 {
			return apierr.CodeInternal
		}
		return apierr.CodeBadRequest
	}
}
