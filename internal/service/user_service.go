package service

import (
	"context"
	"errors"
	"strings"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/tenancy"
	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// UserStoreFull is the set of user persistence operations UserService needs.
// Every method that reaches a specific row takes a tenant ID, so tenant
// isolation is expressed in the interface itself and a non-isolating
// implementation would not satisfy it.
type UserStoreFull interface {
	GetByID(ctx context.Context, id string) (*models.User, error)
	GetByIDInTenant(ctx context.Context, tenantID, id string) (*models.User, error)
	ListByTenant(ctx context.Context, tenantID string, page, pageSize int) ([]*models.User, int64, error)
	UpdateRoleInTenant(ctx context.Context, tenantID, id string, role models.Role) error
	SetActiveInTenant(ctx context.Context, tenantID, id string, active bool) error
	CountAdminsInTenant(ctx context.Context, tenantID string) (int, error)
}

// UserService owns user and RBAC business logic.
//
// It exists so that the rules below are stated exactly once, and both the
// REST handler and the GraphQL resolver inherit them. Before this layer, the
// role-change rules lived in the HTTP handler, which meant a second
// transport could not reuse them — and would have had to reimplement them,
// which is how two transports drift into two different authorization
// models.
type UserService struct {
	repo UserStoreFull
}

func NewUserService(repo UserStoreFull) *UserService {
	return &UserService{repo: repo}
}

// Me returns the authenticated user's own record.
func (s *UserService) Me(ctx context.Context, scope tenancy.Scope) (*models.User, error) {
	if !scope.Valid() {
		return nil, apierr.Unauthenticated("authentication is required")
	}
	if scope.UserID == "" {
		// API-key credentials authenticate a tenant, not a person, so there
		// is no "me" to return for them.
		return nil, apierr.Forbidden("this endpoint requires a user credential, not an API key")
	}
	return s.Get(ctx, scope, scope.UserID)
}

// Get loads one user from the caller's own tenant.
func (s *UserService) Get(ctx context.Context, scope tenancy.Scope, id string) (*models.User, error) {
	if !scope.Valid() {
		return nil, apierr.Unauthenticated("authentication is required")
	}
	if strings.TrimSpace(id) == "" {
		return nil, apierr.BadRequest("a user id is required")
	}

	user, err := s.repo.GetByIDInTenant(ctx, scope.TenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		// Covers both "no such user" and "exists in another tenant". The
		// caller cannot tell the difference, which is the point.
		return nil, apierr.NotFound("user not found")
	}
	if err != nil {
		return nil, apierr.Internal("failed to load user").Wrap(err)
	}
	return user, nil
}

// List returns a page of users in the caller's tenant.
func (s *UserService) List(ctx context.Context, scope tenancy.Scope, page, pageSize int) ([]*models.User, int64, error) {
	if !scope.Valid() {
		return nil, 0, apierr.Unauthenticated("authentication is required")
	}

	users, total, err := s.repo.ListByTenant(ctx, scope.TenantID, page, pageSize)
	if err != nil {
		return nil, 0, apierr.Internal("failed to list users").Wrap(err)
	}
	return users, total, nil
}

// UpdateRoleResult describes a completed role change, so callers can write
// an accurate audit record without re-reading the user.
type UpdateRoleResult struct {
	User         *models.User
	PreviousRole models.Role
	NewRole      models.Role
}

// UpdateRole changes a user's RBAC role within the caller's tenant.
//
// Three rules are enforced here rather than at the transport:
//
//  1. The target must be in the caller's tenant.
//  2. A caller may not change their own role. Self-service role changes are
//     either privilege escalation (viewer promoting themselves) or an
//     accidental lockout (the only admin demoting themselves), and neither
//     has a legitimate use.
//  3. A tenant's last active admin may not be demoted. A tenant with no
//     admin cannot manage its own users, invite anyone, or restore itself —
//     it needs platform-operator intervention to recover.
func (s *UserService) UpdateRole(ctx context.Context, scope tenancy.Scope, targetID string, role models.Role) (*UpdateRoleResult, error) {
	if !role.Valid() {
		return nil, apierr.Validation("role must be one of admin, developer, viewer")
	}
	if scope.Role != string(models.RoleAdmin) {
		return nil, apierr.Forbidden("only a tenant admin may change user roles")
	}
	if scope.UserID != "" && scope.UserID == targetID {
		return nil, apierr.Forbidden("you cannot change your own role")
	}

	target, err := s.Get(ctx, scope, targetID)
	if err != nil {
		return nil, err
	}

	// Captured before the write, not after. Reading target.Role once the
	// repository has been called would silently depend on the repository
	// having handed back a detached copy rather than a live row — a property
	// the interface does not promise. The SQL implementation happens to scan
	// into a fresh struct, so the bug would not show in production, which is
	// exactly what makes it worth removing: the audit record would report
	// "developer -> developer" against any implementation that aliases.
	previous := target.Role

	if target.Role == role {
		return &UpdateRoleResult{User: target, PreviousRole: role, NewRole: role}, nil
	}

	if target.Role == models.RoleAdmin && role != models.RoleAdmin {
		admins, err := s.repo.CountAdminsInTenant(ctx, scope.TenantID)
		if err != nil {
			return nil, apierr.Internal("failed to verify remaining tenant admins").Wrap(err)
		}
		if admins <= 1 {
			return nil, apierr.Conflict("this tenant would be left without an admin; promote another user first")
		}
	}

	if err := s.repo.UpdateRoleInTenant(ctx, scope.TenantID, targetID, role); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apierr.NotFound("user not found")
		}
		return nil, apierr.Internal("failed to update user role").Wrap(err)
	}

	updated := *target
	updated.Role = role

	return &UpdateRoleResult{User: &updated, PreviousRole: previous, NewRole: role}, nil
}

// SetActive enables or disables a user in the caller's tenant. The same
// last-admin and no-self-service rules apply as for role changes: a
// deactivated admin is as absent as a demoted one.
func (s *UserService) SetActive(ctx context.Context, scope tenancy.Scope, targetID string, active bool) (*models.User, error) {
	if scope.Role != string(models.RoleAdmin) {
		return nil, apierr.Forbidden("only a tenant admin may activate or deactivate users")
	}
	if scope.UserID != "" && scope.UserID == targetID {
		return nil, apierr.Forbidden("you cannot change your own account status")
	}

	target, err := s.Get(ctx, scope, targetID)
	if err != nil {
		return nil, err
	}

	if !active && target.Role == models.RoleAdmin {
		admins, err := s.repo.CountAdminsInTenant(ctx, scope.TenantID)
		if err != nil {
			return nil, apierr.Internal("failed to verify remaining tenant admins").Wrap(err)
		}
		if admins <= 1 {
			return nil, apierr.Conflict("this tenant would be left without an active admin")
		}
	}

	if err := s.repo.SetActiveInTenant(ctx, scope.TenantID, targetID, active); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apierr.NotFound("user not found")
		}
		return nil, apierr.Internal("failed to update user status").Wrap(err)
	}

	updated := *target
	updated.IsActive = active
	return &updated, nil
}
