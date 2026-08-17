package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
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
// access token OR an "X-API-Key" header, resolving whichever is present into
// a common AuthContext. Exactly one credential type is required; requests
// with neither are rejected with 401.
//
// The tenant is re-read from the database on every request rather than
// trusted from the token. A JWT is valid for its full TTL, so a tenant that
// was suspended (or whose plan was downgraded) five minutes ago would keep
// its old entitlements until every outstanding token expired. Reading the
// tenant row makes suspension take effect on the next request.
func Auth(
	jwtManager *jwt.Manager,
	apiKeys *service.APIKeyService,
	tenants TenantLookup,
	audit *service.AuditService,
	log *zap.Logger,
) func(http.Handler) http.Handler {
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
					response.Fail(w, r, apierr.Unauthenticated("invalid or revoked API key"))
					return
				}

				tenant, err := tenants.Get(ctx, key.TenantID)
				if err != nil {
					log.Error("failed to resolve tenant for API key",
						zap.Error(err), zap.String("tenant_id", key.TenantID))
					response.Fail(w, r, apierr.Internal("failed to resolve tenant").Wrap(err))
					return
				}
				if !tenant.IsActive {
					response.Fail(w, r, apierr.Forbidden("tenant account is inactive"))
					return
				}

				ac := AuthContext{
					TenantID:   key.TenantID,
					Role:       models.RoleDeveloper, // machine credentials act with developer-level permissions
					TenantPlan: tenant.Plan,
					AuthMethod: "api_key",
					APIKeyID:   key.ID,
				}
				next.ServeHTTP(w, r.WithContext(withAuth(ctx, ac)))
				return
			}

			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				response.Fail(w, r, apierr.Unauthenticated("a bearer token or API key is required"))
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))

			claims, err := jwtManager.VerifyAccessToken(token)
			if err != nil {
				response.Fail(w, r, apierr.Unauthenticated("invalid or expired access token"))
				return
			}

			tenant, err := tenants.Get(ctx, claims.TenantID)
			if err != nil {
				// A token signed for a tenant that no longer exists is not a
				// server error — it is an unusable credential.
				response.Fail(w, r, apierr.Unauthenticated("the tenant for this token no longer exists"))
				return
			}
			if !tenant.IsActive {
				response.Fail(w, r, apierr.Forbidden("tenant account is inactive"))
				return
			}

			role := models.Role(claims.Role)
			if !role.Valid() {
				response.Fail(w, r, apierr.Unauthenticated("this token carries an unrecognised role"))
				return
			}

			ac := AuthContext{
				UserID:     claims.UserID,
				TenantID:   claims.TenantID,
				Role:       role,
				TenantPlan: tenant.Plan,
				AuthMethod: "jwt",
			}
			next.ServeHTTP(w, r.WithContext(withAuth(ctx, ac)))
		})
	}
}

// RequirePlatformToken authenticates the platform control plane.
//
// Cross-tenant operations (provisioning a tenant, changing a plan, granting
// an entitlement) are reachable only with this operator credential, which is
// deliberately not derivable from any customer's login. Without this split,
// "admin" would have to mean both "administers their own tenant" and
// "administers the platform", and every cross-tenant endpoint would be one
// forgotten check away from being self-service.
//
// The comparison is constant-time so the endpoint cannot be used to recover
// the token a byte at a time.
func RequirePlatformToken(expected string, audit *service.AuditService) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := []byte(r.Header.Get("X-Platform-Token"))

			if len(expectedBytes) == 0 || subtle.ConstantTimeCompare(presented, expectedBytes) != 1 {
				audit.Record(r.Context(), service.RecordInput{
					Event:     models.EventUnauthorizedRequest,
					Endpoint:  r.URL.Path,
					IPAddress: clientIP(r),
					UserAgent: r.UserAgent(),
					Metadata:  map[string]any{"reason": "invalid_platform_token"},
				})
				// A 404 rather than a 401: the platform control plane does not
				// advertise its own existence to unauthenticated callers.
				response.Fail(w, r, apierr.NotFound("not found"))
				return
			}

			next.ServeHTTP(w, r.WithContext(withPlatform(r.Context())))
		})
	}
}
