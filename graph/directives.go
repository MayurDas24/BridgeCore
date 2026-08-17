package graph

import (
	"context"

	"github.com/graphql-go/graphql"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/internal/tenancy"
	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// Authorization for the GraphQL API is expressed as resolver decorators that
// wrap a field's resolve function, applied once where the schema is built:
//
//	"updateUserRole": &graphql.Field{
//	    Resolve: r.requiresRole(models.RoleAdmin, r.updateUserRole),
//	}
//
// This is the same idea as an SDL @requiresRole / @requiresFeature directive —
// authorization declared next to the field rather than repeated inside every
// resolver body — with two advantages. The rules are the *same code* the REST
// middleware uses (middleware.RoleAtLeast, the EntitlementChecker), so the two
// transports cannot drift into different authorization models; and a field
// whose decorator is missing is visible in one file rather than requiring a
// schema audit. The equivalent SDL declarations are documented in
// graph/schema/directives.graphqls.
//
// The decorators compose outside-in: requireAuth runs first, then the role
// check, then the entitlement check, mirroring the REST chain exactly.

// requireAuth rejects an unauthenticated request.
func (r *Resolver) requireAuth(next graphql.FieldResolveFn) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		if !middleware.ScopeFromContext(p.Context).Valid() {
			return nil, apierr.Unauthenticated("authentication is required")
		}
		return next(p)
	}
}

// requiresRole enforces the same role ordering as the REST RequireRole
// middleware (admin > developer > viewer).
func (r *Resolver) requiresRole(minRole models.Role, next graphql.FieldResolveFn) graphql.FieldResolveFn {
	return r.requireAuth(func(p graphql.ResolveParams) (interface{}, error) {
		scope := middleware.ScopeFromContext(p.Context)

		if !middleware.RoleAtLeast(models.Role(scope.Role), minRole) {
			r.auditDenial(p.Context, scope, models.EventFeatureAccessDenied, map[string]any{
				"transport": "graphql",
				"field":     fieldName(p),
				"reason":    "insufficient_role",
				"required":  string(minRole),
				"actual":    scope.Role,
			})
			return nil, apierr.Forbidden("this operation requires the %s role", minRole)
		}
		return next(p)
	})
}

// requiresFeature enforces a plan entitlement, reusing the exact
// EntitlementService the REST RequireFeature middleware calls.
func (r *Resolver) requiresFeature(featureKey string, next graphql.FieldResolveFn) graphql.FieldResolveFn {
	return r.requireAuth(func(p graphql.ResolveParams) (interface{}, error) {
		ac, ok := middleware.AuthFromContext(p.Context)
		if !ok {
			return nil, apierr.Unauthenticated("authentication is required")
		}

		entitled, err := r.Entitlements.HasFeature(p.Context, ac.TenantID, ac.TenantPlan, featureKey)
		if err != nil {
			return nil, apierr.Internal("failed to resolve feature entitlement").Wrap(err)
		}
		if !entitled {
			r.auditDenial(p.Context, middleware.ScopeFromContext(p.Context), models.EventFeatureAccessDenied, map[string]any{
				"transport": "graphql",
				"field":     fieldName(p),
				"feature":   featureKey,
				"plan":      string(ac.TenantPlan),
			})
			return nil, apierr.FeatureRequired(featureKey)
		}
		return next(p)
	})
}

// fieldName reports the field being resolved, for audit metadata. GraphQL has
// no URL path, so the field name is the closest equivalent to the REST
// endpoint recorded on a denial.
func fieldName(p graphql.ResolveParams) string {
	if p.Info.FieldName != "" {
		return p.Info.FieldName
	}
	return "unknown"
}

func (r *Resolver) auditDenial(ctx context.Context, scope tenancy.Scope, event string, metadata map[string]any) {
	tenantID := scope.TenantID
	var tenantPtr *string
	if tenantID != "" {
		tenantPtr = &tenantID
	}
	var actorPtr *string
	if scope.UserID != "" {
		userID := scope.UserID
		actorPtr = &userID
	}

	r.Audit.Record(ctx, service.RecordInput{
		TenantID: tenantPtr,
		ActorID:  actorPtr,
		Event:    event,
		Endpoint: "graphql",
		Metadata: metadata,
	})
}
