// Package middleware implements BridgeCore's HTTP middleware chain:
// request ID injection, panic recovery, structured request logging, CORS,
// JWT/API-key authentication, RBAC, feature entitlement checks, usage
// metering, and rate limiting.
package middleware

import (
	"context"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/tenancy"
)

type contextKey string

const (
	ctxKeyRequestID  contextKey = "request_id"
	ctxKeyUserID     contextKey = "user_id"
	ctxKeyTenantID   contextKey = "tenant_id"
	ctxKeyRole       contextKey = "role"
	ctxKeyTenantPlan contextKey = "tenant_plan"
	ctxKeyAuthMethod contextKey = "auth_method" // "jwt" or "api_key"
	ctxKeyAPIKeyID   contextKey = "api_key_id"
	ctxKeyPlatform   contextKey = "platform_operator"
)

// AuthContext is the resolved identity attached to an authenticated request.
type AuthContext struct {
	UserID     string // empty for API-key-authenticated requests
	TenantID   string
	Role       models.Role
	TenantPlan models.Plan
	AuthMethod string
	APIKeyID   string
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

func withAuth(ctx context.Context, ac AuthContext) context.Context {
	ctx = context.WithValue(ctx, ctxKeyUserID, ac.UserID)
	ctx = context.WithValue(ctx, ctxKeyTenantID, ac.TenantID)
	ctx = context.WithValue(ctx, ctxKeyRole, ac.Role)
	ctx = context.WithValue(ctx, ctxKeyTenantPlan, ac.TenantPlan)
	ctx = context.WithValue(ctx, ctxKeyAuthMethod, ac.AuthMethod)
	ctx = context.WithValue(ctx, ctxKeyAPIKeyID, ac.APIKeyID)
	return ctx
}

// AuthFromContext extracts the resolved AuthContext set by the
// authentication middleware. ok is false if the request was never
// authenticated (e.g. a public endpoint).
func AuthFromContext(ctx context.Context) (AuthContext, bool) {
	tenantID, ok := ctx.Value(ctxKeyTenantID).(string)
	if !ok || tenantID == "" {
		return AuthContext{}, false
	}
	userID, _ := ctx.Value(ctxKeyUserID).(string)
	role, _ := ctx.Value(ctxKeyRole).(models.Role)
	plan, _ := ctx.Value(ctxKeyTenantPlan).(models.Plan)
	method, _ := ctx.Value(ctxKeyAuthMethod).(string)
	apiKeyID, _ := ctx.Value(ctxKeyAPIKeyID).(string)
	return AuthContext{
		UserID:     userID,
		TenantID:   tenantID,
		Role:       role,
		TenantPlan: plan,
		AuthMethod: method,
		APIKeyID:   apiKeyID,
	}, true
}

// ScopeFromContext converts the authenticated identity into the tenancy
// scope every service method takes.
//
// This is the single conversion point between "who authenticated" and "what
// this request is allowed to touch", and it is why no handler or resolver
// ever constructs a scope from a request body: there is nowhere else for one
// to come from.
func ScopeFromContext(ctx context.Context) tenancy.Scope {
	ac, ok := AuthFromContext(ctx)
	if !ok {
		return tenancy.Scope{}
	}
	return tenancy.Scope{
		TenantID: ac.TenantID,
		UserID:   ac.UserID,
		Role:     string(ac.Role),
	}
}

// PlatformFromContext reports whether the request was authenticated as a
// platform operator rather than as a tenant.
func PlatformFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyPlatform).(bool)
	return v
}

func withPlatform(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyPlatform, true)
}
