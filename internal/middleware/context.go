// Package middleware implements BridgeCore's HTTP middleware chain:
// request ID injection, panic recovery, structured request logging, CORS,
// JWT/API-key authentication, RBAC, feature entitlement checks, usage
// metering, and rate limiting.
package middleware

import (
	"context"

	"github.com/bridgecore/bridgecore/internal/models"
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
