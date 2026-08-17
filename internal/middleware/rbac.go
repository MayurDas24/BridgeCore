package middleware

import (
	"context"
	"net/http"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// roleRank gives each role a numeric level so RequireRole can express
// "at least developer" rather than needing an exact-match list everywhere.
var roleRank = map[models.Role]int{
	models.RoleViewer:    1,
	models.RoleDeveloper: 2,
	models.RoleAdmin:     3,
}

// RoleAtLeast reports whether role satisfies minRole. Exported so the
// GraphQL authorization directives apply exactly the same ordering as the
// REST middleware, rather than reimplementing it.
func RoleAtLeast(role, minRole models.Role) bool {
	return roleRank[role] >= roleRank[minRole]
}

// RequireRole builds middleware that only allows requests whose
// authenticated role is at least minRole (admin > developer > viewer).
// Must run after Auth middleware in the chain.
func RequireRole(audit *service.AuditService, minRole models.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ac, ok := AuthFromContext(r.Context())
			if !ok {
				response.Fail(w, r, apierr.Unauthenticated("authentication is required"))
				return
			}

			if !RoleAtLeast(ac.Role, minRole) {
				tenantID := ac.TenantID
				audit.Record(r.Context(), service.RecordInput{
					TenantID:  &tenantID,
					ActorID:   ptrOrNil(ac.UserID),
					Event:     models.EventFeatureAccessDenied,
					Endpoint:  r.URL.Path,
					IPAddress: clientIP(r),
					UserAgent: r.UserAgent(),
					Metadata: map[string]any{
						"reason":   "insufficient_role",
						"required": string(minRole),
						"actual":   string(ac.Role),
					},
				})
				response.Fail(w, r, apierr.Forbidden("this operation requires the %s role", minRole))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// EntitlementChecker resolves whether a tenant has access to a feature key.
type EntitlementChecker interface {
	HasFeature(ctx context.Context, tenantID string, tenantPlan models.Plan, featureKey string) (bool, error)
}

// RequireFeature builds middleware that blocks the request unless the
// authenticated tenant is entitled to featureKey.
//
// The check runs before the handler, so an unentitled tenant never reaches
// the code that would do the work — which means a plan boundary is enforced
// by the routing table rather than by remembering to check inside each
// handler. The same EntitlementChecker backs the GraphQL @requiresFeature
// directive, so REST and GraphQL cannot drift apart.
func RequireFeature(entitlements EntitlementChecker, audit *service.AuditService, featureKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ac, ok := AuthFromContext(r.Context())
			if !ok {
				response.Fail(w, r, apierr.Unauthenticated("authentication is required"))
				return
			}

			has, err := entitlements.HasFeature(r.Context(), ac.TenantID, ac.TenantPlan, featureKey)
			if err != nil {
				response.Fail(w, r, apierr.Internal("failed to resolve feature entitlement").Wrap(err))
				return
			}
			if !has {
				tenantID := ac.TenantID
				audit.Record(r.Context(), service.RecordInput{
					TenantID:  &tenantID,
					ActorID:   ptrOrNil(ac.UserID),
					Event:     models.EventFeatureAccessDenied,
					Endpoint:  r.URL.Path,
					IPAddress: clientIP(r),
					UserAgent: r.UserAgent(),
					Metadata:  map[string]any{"feature": featureKey, "plan": string(ac.TenantPlan)},
				})
				response.Fail(w, r, apierr.FeatureRequired(featureKey))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
