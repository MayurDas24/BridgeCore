// Package tenancy holds BridgeCore's tenant-isolation primitives.
//
// Multi-tenant isolation is the single most consequential invariant in a
// shared SaaS control plane: one leak across the boundary is a breach, not
// a bug. BridgeCore enforces it in three independent layers, so that no
// single mistake is sufficient to cross it:
//
//  1. SQL — every tenant-scoped query carries `WHERE tenant_id = $n`. A row
//     belonging to another tenant is never returned, so the application
//     cannot leak what it never loaded.
//  2. Service — every service method that accepts a resource ID also
//     accepts the caller's tenant scope, and calls Guard before acting.
//  3. Transport — handlers and resolvers derive the scope from the
//     authenticated identity only. A tenant ID in a request body, query
//     string, or GraphQL argument is never trusted.
//
// Guard deliberately reports a not-found rather than a forbidden. Telling
// an attacker "that resource exists, but it isn't yours" is itself an
// information leak: it turns a blind ID-guessing attempt into a working
// resource-existence oracle. From outside the boundary, another tenant's
// data does not exist.
package tenancy

import (
	"errors"

	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// ErrCrossTenant is the internal marker for a blocked cross-tenant access.
// It is wrapped as the (never serialized) cause of the public not-found
// error, so the attempt is visible in logs and audit records while the
// caller learns nothing.
var ErrCrossTenant = errors.New("tenancy: cross-tenant access blocked")

// Scope is the tenant boundary a request is permitted to operate within.
// It is always derived from verified credentials, never from client input.
type Scope struct {
	TenantID string
	UserID   string
	Role     string
}

// Valid reports whether the scope identifies a tenant.
func (s Scope) Valid() bool { return s.TenantID != "" }

// Guard verifies that a resource belongs to the caller's tenant. resource
// names the resource kind and is used only for the caller-facing message
// ("user not found"), never to reveal whether the ID exists elsewhere.
func Guard(scope Scope, resourceTenantID, resource string) error {
	if !scope.Valid() {
		return apierr.Unauthenticated("authentication is required")
	}
	if resourceTenantID == "" || resourceTenantID != scope.TenantID {
		return apierr.NotFound("%s not found", resource).Wrap(ErrCrossTenant)
	}
	return nil
}

// GuardPtr is Guard for nullable tenant columns (usage and audit rows may
// have a NULL tenant_id when they record pre-authentication activity).
// A NULL owner is never visible to a tenant.
func GuardPtr(scope Scope, resourceTenantID *string, resource string) error {
	if resourceTenantID == nil {
		if !scope.Valid() {
			return apierr.Unauthenticated("authentication is required")
		}
		return apierr.NotFound("%s not found", resource).Wrap(ErrCrossTenant)
	}
	return Guard(scope, *resourceTenantID, resource)
}

// IsCrossTenant reports whether an error was produced by a blocked
// cross-tenant access, so callers can audit the attempt.
func IsCrossTenant(err error) bool {
	return errors.Is(err, ErrCrossTenant)
}
