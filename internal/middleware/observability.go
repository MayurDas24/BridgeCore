package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/config"
)

// responseRecorder captures the status code and byte count written by a
// handler so the logging/metering middleware can observe them after the
// fact — http.ResponseWriter doesn't expose this natively.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, so
// streaming responses are not buffered by the recorder.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

// maxRequestIDLength bounds a propagated correlation ID. An inbound header
// is attacker-controlled and ends up in every log line for the request, so
// it is length-capped and character-filtered rather than trusted.
const maxRequestIDLength = 64

// RequestID generates (or safely propagates) a unique request correlation ID
// and attaches it to the request context and response headers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitizeRequestID keeps only characters that are safe in a log field,
// dropping anything that could forge a new log entry or break a JSON line.
func sanitizeRequestID(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > maxRequestIDLength {
		raw = raw[:maxRequestIDLength]
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			b.WriteByte(c)
		}
	}
	return b.String()
}

// RequestIDOf is the accessor the response package is wired to at boot, so
// every error envelope carries the correlation ID a user can quote in a
// support ticket.
func RequestIDOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return RequestIDFromContext(r.Context())
}

// Recovery catches panics anywhere downstream, logs them with the request's
// correlation ID, and returns a clean 500 instead of crashing the process.
func Recovery(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					requestID := RequestIDFromContext(r.Context())
					log.Error("panic recovered",
						zap.Any("error", err),
						zap.String("request_id", requestID),
						zap.String("method", r.Method),
						zap.String("path", r.URL.Path),
						zap.Stack("stack"),
					)
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"success":false,"message":"internal server error",` +
						`"error":{"code":"INTERNAL_ERROR","message":"internal server error"},` +
						`"request_id":"` + requestID + `"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logging emits one structured log line per completed request, including
// request ID, method, route, status, latency, and (if authenticated)
// tenant/user identity.
//
// The field set is chosen so that a CloudWatch Logs Insights query can
// answer the questions that actually come up during an incident: which
// tenant, which endpoint, what status, how slow.
func Logging(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := newRecorder(w)

			next.ServeHTTP(rec, r)

			latency := time.Since(start)
			fields := []zap.Field{
				zap.String("request_id", RequestIDFromContext(r.Context())),
				zap.String("method", r.Method),
				zap.String("route", r.URL.Path),
				zap.Int("status_code", rec.status),
				zap.Int64("latency_ms", latency.Milliseconds()),
				zap.Int("bytes", rec.bytes),
				zap.String("remote_ip", clientIP(r)),
				zap.String("user_agent", r.UserAgent()),
			}
			if ac, ok := AuthFromContext(r.Context()); ok {
				fields = append(fields,
					zap.String("tenant_id", ac.TenantID),
					zap.String("auth_method", ac.AuthMethod),
					zap.String("role", string(ac.Role)),
				)
				if ac.UserID != "" {
					fields = append(fields, zap.String("user_id", ac.UserID))
				}
			}

			switch {
			case rec.status >= 500:
				log.Error("request completed", fields...)
			case rec.status >= 400:
				log.Warn("request completed", fields...)
			default:
				log.Info("request completed", fields...)
			}
		})
	}
}

// CORS applies the configured cross-origin policy.
//
// The origin is echoed back only when it matches the allow-list, rather than
// answering every request with "*". A wildcard is fine for a public,
// credential-free API; BridgeCore is neither, and config.Validate refuses to
// start production with one.
func CORS(cfg config.CORSConfig) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[strings.ToLower(strings.TrimSpace(o))] = true
	}
	allowAll := allowed["*"]

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	maxAge := strconv.Itoa(int(cfg.MaxAge.Seconds()))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			switch {
			case origin == "":
				// Not a browser cross-origin request; nothing to negotiate.
			case allowAll:
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case allowed[strings.ToLower(origin)]:
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				// Responses vary by Origin, so caches must not serve one
				// origin's response to another.
				w.Header().Add("Vary", "Origin")
			}

			w.Header().Set("Access-Control-Allow-Methods", methods)
			w.Header().Set("Access-Control-Allow-Headers", headers)
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining")
			w.Header().Set("Access-Control-Max-Age", maxAge)

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets the baseline response headers appropriate for a JSON
// API. They matter here mainly because the same origin serves the Swagger UI
// and the GraphQL playground, which are HTML.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// clientIP resolves the caller's address, honouring the X-Forwarded-For
// chain the Application Load Balancer appends to. The left-most entry is the
// original client; entries beyond it are proxies.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if idx := strings.IndexByte(fwd, ','); idx > 0 {
			return strings.TrimSpace(fwd[:idx])
		}
		return strings.TrimSpace(fwd)
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		return strings.TrimSpace(realIP)
	}
	return r.RemoteAddr
}
