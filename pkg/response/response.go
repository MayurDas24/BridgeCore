// Package response provides the single JSON envelope shape used by every
// BridgeCore endpoint, so API consumers never have to branch on response
// structure.
package response

import (
	"encoding/json"
	"net/http"
)

// Envelope is the canonical response shape for every BridgeCore endpoint.
type Envelope struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
	Error   any    `json:"error,omitempty"`
}

// Meta carries pagination info and is embedded in Data for list endpoints.
type Meta struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	TotalCount int64 `json:"total_count"`
	TotalPages int   `json:"total_pages"`
}

// ListResponse is the standard shape for paginated collection endpoints.
type ListResponse struct {
	Items any  `json:"items"`
	Meta  Meta `json:"meta"`
}

func write(w http.ResponseWriter, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// OK writes a 200 success envelope.
func OK(w http.ResponseWriter, message string, data any) {
	write(w, http.StatusOK, Envelope{Success: true, Message: message, Data: data})
}

// Created writes a 201 success envelope.
func Created(w http.ResponseWriter, message string, data any) {
	write(w, http.StatusCreated, Envelope{Success: true, Message: message, Data: data})
}

// NoContent writes a 204 with no body.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Err writes an error envelope with the given HTTP status code.
func Err(w http.ResponseWriter, status int, message string, detail any) {
	write(w, status, Envelope{Success: false, Message: message, Error: detail})
}

// BadRequest writes a 400 error envelope.
func BadRequest(w http.ResponseWriter, message string, detail any) {
	Err(w, http.StatusBadRequest, message, detail)
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

// InternalError writes a 500 error envelope. The detail is intentionally
// generic — internal error detail belongs in logs, not API responses.
func InternalError(w http.ResponseWriter, message string) {
	Err(w, http.StatusInternalServerError, message, nil)
}
