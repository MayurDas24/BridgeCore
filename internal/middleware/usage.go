package middleware

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/service"
)

// UsageRecorder records one usage_logs row per completed request.
type UsageRecorder interface {
	Record(ctx context.Context, in service.RecordUsageInput) error
}

// UsageMetering wraps every request, timing it and recording tenant,
// endpoint, method, latency, and status code to the usage log — entirely
// transparent to handlers. Recording happens asynchronously (fire-and-
// forget with a bounded timeout) so a slow metering write never adds
// latency to the actual API response.
func UsageMetering(usage UsageRecorder, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := newRecorder(w)

			next.ServeHTTP(rec, r)

			latency := time.Since(start)
			requestID := RequestIDFromContext(r.Context())

			var tenantIDPtr *string
			if ac, ok := AuthFromContext(r.Context()); ok {
				tenantID := ac.TenantID
				tenantIDPtr = &tenantID
			}

			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := usage.Record(ctx, service.RecordUsageInput{
					TenantID:   tenantIDPtr,
					Endpoint:   r.URL.Path,
					Method:     r.Method,
					StatusCode: rec.status,
					LatencyMS:  int(latency.Milliseconds()),
					RequestID:  requestID,
				}); err != nil {
					log.Warn("failed to record usage log", zap.Error(err), zap.String("request_id", requestID))
				}
			}()
		})
	}
}
