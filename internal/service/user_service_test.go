package service

import (
	"context"
	"testing"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/tenancy"
	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// fakeUserStoreFull is an in-memory UserStoreFull that enforces tenant
// scoping the same way the SQL implementation does, so a test that passes
// here would also pass against PostgreSQL.
type fakeUserStoreFull struct {
	users map[string]*models.User
}

func newFakeUserStoreFull(users ...*models.User) *fakeUserStoreFull {
	f := &fakeUserStoreFull{users: map[string]*models.User{}}
	for _, u := range users {
		f.users[u.ID] = u
	}
	return f
}

// detach returns a copy, mirroring the SQL implementation, which scans each
// row into a fresh struct. A double that handed back its own live pointer
// would let a service mutate the "database" by accident and would hide the
// class of aliasing bug this fidelity is here to catch.
func detach(u *models.User) *models.User {
	copied := *u
	return &copied
}

func (f *fakeUserStoreFull) GetByID(ctx context.Context, id string) (*models.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return detach(u), nil
}

func (f *fakeUserStoreFull) GetByIDInTenant(ctx context.Context, tenantID, id string) (*models.User, error) {
	u, ok := f.users[id]
	if !ok || u.TenantID != tenantID {
		return nil, repository.ErrNotFound
	}
	return detach(u), nil
}

func (f *fakeUserStoreFull) ListByTenant(ctx context.Context, tenantID string, page, pageSize int) ([]*models.User, int64, error) {
	var out []*models.User
	for _, u := range f.users {
		if u.TenantID == tenantID {
			out = append(out, detach(u))
		}
	}
	return out, int64(len(out)), nil
}

func (f *fakeUserStoreFull) UpdateRoleInTenant(ctx context.Context, tenantID, id string, role models.Role) error {
	u, ok := f.users[id]
	if !ok || u.TenantID != tenantID {
		return repository.ErrNotFound
	}
	u.Role = role
	return nil
}

func (f *fakeUserStoreFull) SetActiveInTenant(ctx context.Context, tenantID, id string, active bool) error {
	u, ok := f.users[id]
	if !ok || u.TenantID != tenantID {
		return repository.ErrNotFound
	}
	u.IsActive = active
	return nil
}

func (f *fakeUserStoreFull) CountAdminsInTenant(ctx context.Context, tenantID string) (int, error) {
	n := 0
	for _, u := range f.users {
		if u.TenantID == tenantID && u.Role == models.RoleAdmin && u.IsActive {
			n++
		}
	}
	return n, nil
}

func adminScope(tenantID, userID string) tenancy.Scope {
	return tenancy.Scope{TenantID: tenantID, UserID: userID, Role: string(models.RoleAdmin)}
}

func user(id, tenantID string, role models.Role) *models.User {
	return &models.User{ID: id, TenantID: tenantID, Role: role, IsActive: true, Email: id + "@example.test"}
}

func TestUserService_Get_HidesUsersFromOtherTenants(t *testing.T) {
	store := newFakeUserStoreFull(
		user("admin-a", "tenant-a", models.RoleAdmin),
		user("victim-b", "tenant-b", models.RoleViewer),
	)
	svc := NewUserService(store)

	_, err := svc.Get(context.Background(), adminScope("tenant-a", "admin-a"), "victim-b")
	if !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("expected a cross-tenant read to look like NOT_FOUND, got %v", err)
	}
}

func TestUserService_List_OnlyReturnsOwnTenant(t *testing.T) {
	store := newFakeUserStoreFull(
		user("admin-a", "tenant-a", models.RoleAdmin),
		user("dev-a", "tenant-a", models.RoleDeveloper),
		user("admin-b", "tenant-b", models.RoleAdmin),
	)
	svc := NewUserService(store)

	users, total, err := svc.List(context.Background(), adminScope("tenant-a", "admin-a"), 1, 20)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if total != 2 || len(users) != 2 {
		t.Fatalf("expected exactly the 2 users in tenant-a, got %d (total %d)", len(users), total)
	}
	for _, u := range users {
		if u.TenantID != "tenant-a" {
			t.Fatalf("user %s from tenant %s leaked into the listing", u.ID, u.TenantID)
		}
	}
}

func TestUserService_UpdateRole_PromotesWithinTenant(t *testing.T) {
	store := newFakeUserStoreFull(
		user("admin-a", "tenant-a", models.RoleAdmin),
		user("viewer-a", "tenant-a", models.RoleViewer),
	)
	svc := NewUserService(store)

	res, err := svc.UpdateRole(context.Background(), adminScope("tenant-a", "admin-a"), "viewer-a", models.RoleDeveloper)
	if err != nil {
		t.Fatalf("UpdateRole() error = %v", err)
	}
	if res.PreviousRole != models.RoleViewer || res.NewRole != models.RoleDeveloper {
		t.Fatalf("unexpected transition %s -> %s", res.PreviousRole, res.NewRole)
	}
	if store.users["viewer-a"].Role != models.RoleDeveloper {
		t.Fatal("expected the role change to be persisted")
	}
}

func TestUserService_UpdateRole_RejectsNonAdminCaller(t *testing.T) {
	store := newFakeUserStoreFull(
		user("dev-a", "tenant-a", models.RoleDeveloper),
		user("viewer-a", "tenant-a", models.RoleViewer),
	)
	svc := NewUserService(store)

	scope := tenancy.Scope{TenantID: "tenant-a", UserID: "dev-a", Role: string(models.RoleDeveloper)}
	_, err := svc.UpdateRole(context.Background(), scope, "viewer-a", models.RoleAdmin)
	if !apierr.Is(err, apierr.CodeForbidden) {
		t.Fatalf("expected FORBIDDEN for a non-admin caller, got %v", err)
	}
}

func TestUserService_UpdateRole_RejectsSelfChange(t *testing.T) {
	store := newFakeUserStoreFull(user("admin-a", "tenant-a", models.RoleAdmin))
	svc := NewUserService(store)

	_, err := svc.UpdateRole(context.Background(), adminScope("tenant-a", "admin-a"), "admin-a", models.RoleViewer)
	if !apierr.Is(err, apierr.CodeForbidden) {
		t.Fatalf("expected FORBIDDEN for a self role change, got %v", err)
	}
}

func TestUserService_UpdateRole_RefusesToRemoveLastAdmin(t *testing.T) {
	// Two admins: demoting one is fine.
	store := newFakeUserStoreFull(
		user("admin-1", "tenant-a", models.RoleAdmin),
		user("admin-2", "tenant-a", models.RoleAdmin),
	)
	svc := NewUserService(store)
	ctx := context.Background()

	if _, err := svc.UpdateRole(ctx, adminScope("tenant-a", "admin-1"), "admin-2", models.RoleViewer); err != nil {
		t.Fatalf("expected demoting the second admin to succeed, got %v", err)
	}

	// Now only admin-1 remains, and it cannot be demoted by anyone.
	store.users["operator"] = user("operator", "tenant-a", models.RoleAdmin)
	store.users["operator"].Role = models.RoleAdmin
	// Deactivate the helper so admin-1 really is the last active admin.
	store.users["operator"].IsActive = false

	_, err := svc.UpdateRole(ctx, adminScope("tenant-a", "operator"), "admin-1", models.RoleViewer)
	if !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("expected CONFLICT when removing the last admin, got %v", err)
	}
}

func TestUserService_UpdateRole_RejectsUnknownRole(t *testing.T) {
	store := newFakeUserStoreFull(
		user("admin-a", "tenant-a", models.RoleAdmin),
		user("viewer-a", "tenant-a", models.RoleViewer),
	)
	svc := NewUserService(store)

	_, err := svc.UpdateRole(context.Background(), adminScope("tenant-a", "admin-a"), "viewer-a", models.Role("superuser"))
	if !apierr.Is(err, apierr.CodeValidation) {
		t.Fatalf("expected VALIDATION_FAILED for an unknown role, got %v", err)
	}
}

func TestUserService_Me_RejectsAPIKeyCredentials(t *testing.T) {
	svc := NewUserService(newFakeUserStoreFull())

	// An API key authenticates a tenant, not a person: UserID is empty.
	_, err := svc.Me(context.Background(), tenancy.Scope{TenantID: "tenant-a", Role: string(models.RoleDeveloper)})
	if !apierr.Is(err, apierr.CodeForbidden) {
		t.Fatalf("expected FORBIDDEN for an API-key credential, got %v", err)
	}
}