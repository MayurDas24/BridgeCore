package middleware

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
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

func newRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

// RequestID generates (or propagates) a unique request correlation ID and
// attaches it to the request context and response headers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Recovery catches panics anywhere downstream, logs them with a stack
// trace, and returns a clean 500 instead of crashing the process.
func Recovery(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					log.Error("panic recovered",
						zap.Any("error", err),
						zap.String("request_id", RequestIDFromContext(r.Context())),
						zap.String("path", r.URL.Path),
					)
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"success":false,"message":"internal server error","error":null}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logging emits one structured log line per completed request, including
// request ID, method, route, status, latency, and (if authenticated)
// tenant/user identity.
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
				zap.Int("status", rec.status),
				zap.Duration("latency", latency),
				zap.String("remote_ip", clientIP(r)),
				zap.String("user_agent", r.UserAgent()),
			}
			if ac, ok := AuthFromContext(r.Context()); ok {
				fields = append(fields, zap.String("tenant_id", ac.TenantID))
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

// CORS applies a permissive-by-default cross-origin policy suitable for a
// platform API consumed by many first- and third-party frontends. In a
// stricter deployment, allowedOrigins would be sourced from config.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, X-Request-ID")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	return r.RemoteAddr
}
