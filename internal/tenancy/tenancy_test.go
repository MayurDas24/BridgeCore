package tenancy

import (
	"testing"

	"github.com/bridgecore/bridgecore/pkg/apierr"
)

func TestGuardAllowsSameTenant(t *testing.T) {
	scope := Scope{TenantID: "tenant-a", UserID: "user-1", Role: "admin"}
	if err := Guard(scope, "tenant-a", "user"); err != nil {
		t.Fatalf("expected same-tenant access to be allowed, got %v", err)
	}
}

func TestGuardBlocksCrossTenantAsNotFound(t *testing.T) {
	scope := Scope{TenantID: "tenant-a"}

	err := Guard(scope, "tenant-b", "user")
	if err == nil {
		t.Fatal("expected cross-tenant access to be blocked")
	}
	if !IsCrossTenant(err) {
		t.Errorf("expected the error to be identifiable as cross-tenant, got %v", err)
	}
	// Crucially a 404, not a 403: a 403 would confirm the resource exists.
	if !apierr.Is(err, apierr.CodeNotFound) {
		t.Errorf("expected NOT_FOUND to avoid leaking resource existence, got %s", apierr.CodeOf(err))
	}
}

func TestGuardRejectsMissingScope(t *testing.T) {
	err := Guard(Scope{}, "tenant-a", "user")
	if !apierr.Is(err, apierr.CodeUnauthenticated) {
		t.Errorf("expected UNAUTHENTICATED for an empty scope, got %s", apierr.CodeOf(err))
	}
}

func TestGuardRejectsEmptyResourceOwner(t *testing.T) {
	// A resource with no owner must never fall through as "matches".
	if err := Guard(Scope{TenantID: "tenant-a"}, "", "user"); err == nil {
		t.Fatal("expected an unowned resource to be rejected")
	}
}

func TestGuardPtrBlocksNullOwner(t *testing.T) {
	scope := Scope{TenantID: "tenant-a"}

	if err := GuardPtr(scope, nil, "usage record"); err == nil {
		t.Fatal("expected a NULL tenant_id to be invisible to a tenant")
	}

	owner := "tenant-a"
	if err := GuardPtr(scope, &owner, "usage record"); err != nil {
		t.Fatalf("expected same-tenant access to be allowed, got %v", err)
	}

	other := "tenant-b"
	if err := GuardPtr(scope, &other, "usage record"); !IsCrossTenant(err) {
		t.Fatalf("expected cross-tenant block, got %v", err)
	}
}
