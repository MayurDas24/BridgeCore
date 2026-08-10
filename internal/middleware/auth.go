package middleware

import (
	"context"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/jwt"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// TenantLookup resolves a tenant's current plan, used by auth middleware to
// stamp the tenant's plan onto the request context so downstream
// entitlement checks never need a second DB round trip.
type TenantLookup interface {
	Get(ctx context.Context, id string) (*models.Tenant, error)
}

// Auth builds authentication middleware that accepts EITHER a Bearer JWT
// access token OR an "X-API-Key" header, resolving whichever is present
// into a common AuthContext. Exactly one credential type is required;
// requests with neither are rejected with 401.
func Auth(jwtManager *jwt.Manager, apiKeys *service.APIKeyService, tenants TenantLookup, audit *service.AuditService, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
				key, err := apiKeys.Authenticate(ctx, apiKey)
				if err != nil {
					audit.Record(ctx, service.RecordInput{
						Event:     models.EventUnauthorizedRequest,
						Endpoint:  r.URL.Path,
						IPAddress: clientIP(r),
						UserAgent: r.UserAgent(),
						Metadata:  map[string]any{"reason": "invalid_api_key"},
					})
					response.Unauthorized(w, "invalid or revoked API key")
					return
				}

				tenant, err := tenants.Get(ctx, key.TenantID)
				if err != nil {
					response.InternalError(w, "failed to resolve tenant")
					return
				}
				if !tenant.IsActive {
					response.Forbidden(w, "tenant account is inactive")
					return
				}

				ac := AuthContext{
					TenantID:   key.TenantID,
					Role:       models.RoleDeveloper, // API keys act with developer-level machine permissions
					TenantPlan: tenant.Plan,
					AuthMethod: "api_key",
					APIKeyID:   key.ID,
				}
				next.ServeHTTP(w, r.WithContext(withAuth(ctx, ac)))
				return
			}

			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				response.Unauthorized(w, "missing bearer token or API key")
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")

			claims, err := jwtManager.VerifyAccessToken(token)
			if err != nil {
				response.Unauthorized(w, "invalid or expired access token")
				return
			}

			tenant, err := tenants.Get(ctx, claims.TenantID)
			if err != nil {
				response.Unauthorized(w, "tenant not found for token")
				return
			}
			if !tenant.IsActive {
				response.Forbidden(w, "tenant account is inactive")
				return
			}

			ac := AuthContext{
				UserID:     claims.UserID,
				TenantID:   claims.TenantID,
				Role:       models.Role(claims.Role),
				TenantPlan: tenant.Plan,
				AuthMethod: "jwt",
			}
			next.ServeHTTP(w, r.WithContext(withAuth(ctx, ac)))
		})
	}
}
