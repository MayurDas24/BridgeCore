package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/bridgecore/bridgecore/pkg/response"
)

// RateLimit implements a fixed-window rate limiter backed by Redis, keyed
// per-tenant (falling back to per-IP for unauthenticated requests). This
// protects the platform from a single noisy tenant degrading service for
// everyone else — essential for a shared, multi-tenant control plane.
func RateLimit(rdb *redis.Client, requestsPerMinute int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := clientIP(r)
			if ac, ok := AuthFromContext(r.Context()); ok {
				identity = "tenant:" + ac.TenantID
			}

			window := time.Now().Unix() / 60
			key := fmt.Sprintf("ratelimit:%s:%d", identity, window)

			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()

			count, err := rdb.Incr(ctx, key).Result()
			if err != nil {
				// Fail open: if Redis is unavailable, don't block traffic —
				// availability of the core API takes precedence over the
				// rate limiter itself.
				next.ServeHTTP(w, r)
				return
			}
			if count == 1 {
				rdb.Expire(ctx, key, 90*time.Second)
			}

			w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", requestsPerMinute))
			remaining := requestsPerMinute - int(count)
			if remaining < 0 {
				remaining = 0
			}
			w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))

			if int(count) > requestsPerMinute {
				response.TooManyRequests(w, "rate limit exceeded, please slow down")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
