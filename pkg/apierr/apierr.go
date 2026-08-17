// Package apierr defines BridgeCore's single error vocabulary.
//
// Both transports (REST and GraphQL) surface the same stable, machine-
// readable Code, so a client that integrates against REST and later moves
// to GraphQL does not have to relearn the platform's failure modes. Services
// return *apierr.Error (or a sentinel that From knows how to map); the
// transport layer is responsible only for rendering.
//
// The public Message is always safe to show a caller. Internal detail
// belongs on the wrapped cause, which is logged and never serialized.
package apierr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Codes are part of
// the public API contract: they may be added, but never renamed.
type Code string

const (
	CodeBadRequest      Code = "BAD_REQUEST"
	CodeValidation      Code = "VALIDATION_FAILED"
	CodeUnauthenticated Code = "UNAUTHENTICATED"
	CodeForbidden       Code = "FORBIDDEN"
	CodeNotFound        Code = "NOT_FOUND"
	CodeConflict        Code = "CONFLICT"
	CodeRateLimited     Code = "RATE_LIMITED"
	CodeFeatureRequired Code = "FEATURE_NOT_ENTITLED"
	CodeQueryRejected   Code = "QUERY_REJECTED"
	CodeInternal        Code = "INTERNAL_ERROR"
	CodeUnavailable     Code = "SERVICE_UNAVAILABLE"
)

// Error is a transport-agnostic application error.
type Error struct {
	Code    Code
	Message string
	Status  int
	Details map[string]any

	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the internal cause to errors.Is/errors.As without ever
// exposing it to an API consumer.
func (e *Error) Unwrap() error { return e.cause }

// Wrap attaches an internal cause for logging. The cause is never
// serialized into a response body.
func (e *Error) Wrap(cause error) *Error {
	clone := *e
	clone.cause = cause
	return &clone
}

// WithDetails attaches structured, caller-safe context (for example which
// field failed validation, or which feature key is missing).
func (e *Error) WithDetails(details map[string]any) *Error {
	clone := *e
	clone.Details = details
	return &clone
}

// WithMessage overrides the public message, keeping code and status.
func (e *Error) WithMessage(message string) *Error {
	clone := *e
	clone.Message = message
	return &clone
}

func newf(code Code, status int, format string, args ...any) *Error {
	return &Error{Code: code, Status: status, Message: fmt.Sprintf(format, args...)}
}

// Constructors for each supported failure mode.

func BadRequest(format string, args ...any) *Error {
	return newf(CodeBadRequest, http.StatusBadRequest, format, args...)
}

func Validation(format string, args ...any) *Error {
	return newf(CodeValidation, http.StatusUnprocessableEntity, format, args...)
}

func Unauthenticated(format string, args ...any) *Error {
	return newf(CodeUnauthenticated, http.StatusUnauthorized, format, args...)
}

func Forbidden(format string, args ...any) *Error {
	return newf(CodeForbidden, http.StatusForbidden, format, args...)
}

func NotFound(format string, args ...any) *Error {
	return newf(CodeNotFound, http.StatusNotFound, format, args...)
}

func Conflict(format string, args ...any) *Error {
	return newf(CodeConflict, http.StatusConflict, format, args...)
}

func RateLimited(format string, args ...any) *Error {
	return newf(CodeRateLimited, http.StatusTooManyRequests, format, args...)
}

// FeatureRequired reports that the tenant's plan does not include a feature.
// It is a 403 rather than a 402 because the request was well-formed and
// authenticated; the tenant simply is not entitled to it.
func FeatureRequired(featureKey string) *Error {
	return (&Error{
		Code:    CodeFeatureRequired,
		Status:  http.StatusForbidden,
		Message: "your plan does not include this feature",
	}).WithDetails(map[string]any{"feature": featureKey})
}

// QueryRejected reports that a GraphQL document was refused before
// execution (too deep, too costly, or too large).
func QueryRejected(format string, args ...any) *Error {
	return newf(CodeQueryRejected, http.StatusBadRequest, format, args...)
}

func Internal(format string, args ...any) *Error {
	return newf(CodeInternal, http.StatusInternalServerError, format, args...)
}

func Unavailable(format string, args ...any) *Error {
	return newf(CodeUnavailable, http.StatusServiceUnavailable, format, args...)
}

// From normalizes any error into an *Error. Errors that are already
// *Error pass through unchanged; anything else becomes a generic internal
// error with the original preserved as the (unexported, unserialized)
// cause. This is the single choke point that guarantees an unexpected
// error can never leak a SQL string or hostname to a caller.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return Internal("an unexpected error occurred").Wrap(err)
}

// StatusOf reports the HTTP status an error should be rendered with.
func StatusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return From(err).Status
}

// CodeOf reports the stable error code for an error.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	return From(err).Code
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool {
	if err == nil {
		return false
	}
	return From(err).Code == code
}

// Extensions implements the GraphQL "extended error" contract, so a resolver
// failure surfaces the same stable code in the GraphQL errors array that a
// REST caller receives in the error envelope.
//
// This is what stops the two transports from developing separate error
// vocabularies: a client that already handles FEATURE_NOT_ENTITLED from REST
// handles it identically over GraphQL.
func (e *Error) Extensions() map[string]interface{} {
	ext := map[string]interface{}{
		"code":   string(e.Code),
		"status": e.Status,
	}
	if len(e.Details) > 0 {
		ext["details"] = e.Details
	}
	return ext
}
