package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/tenancy"
	"github.com/bridgecore/bridgecore/pkg/apierr"
)

var ErrTenantNotFound = errors.New("service: tenant not found")

// slugPattern constrains tenant slugs to lowercase alphanumerics and
// hyphens. Slugs appear in URLs and log lines, so they are validated at the
// service boundary rather than trusted from a request body.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	maxTenantNameLength = 255
	minSlugLength       = 3
	maxSlugLength       = 63
)

// TenantStoreFull is the full set of tenant persistence operations
// TenantService depends on (a superset of AuthService's TenantStore).
type TenantStoreFull interface {
	Create(ctx context.Context, t *models.Tenant) error
	GetByID(ctx context.Context, id string) (*models.Tenant, error)
	GetBySlug(ctx context.Context, slug string) (*models.Tenant, error)
	List(ctx context.Context, search string, page, pageSize int) ([]*models.Tenant, int64, error)
	Update(ctx context.Context, t *models.Tenant) error
	SoftDelete(ctx context.Context, id string) error
}

// TenantService implements tenant management business logic.
//
// The method set is split deliberately into two groups:
//
//   - Platform operations (Create, List, Get, Update, Delete) act across
//     tenants. They are reachable only from the platform control-plane
//     routes, which authenticate with a separate operator credential.
//   - Tenant-scoped operations (the *ForScope methods) take the caller's
//     verified scope and refuse to act outside it.
//
// Keeping them apart in the type itself means "which of these is safe to
// expose to a customer token?" is answerable by reading the signature,
// instead of by auditing every call site.
type TenantService struct {
	repo TenantStoreFull
}

func NewTenantService(repo TenantStoreFull) *TenantService {
	return &TenantService{repo: repo}
}

type CreateTenantInput struct {
	Name string
	Slug string
	Plan models.Plan
}

// Create provisions a new tenant. Platform operation.
func (s *TenantService) Create(ctx context.Context, in CreateTenantInput) (*models.Tenant, error) {
	name := strings.TrimSpace(in.Name)
	slug := strings.ToLower(strings.TrimSpace(in.Slug))

	if err := validateTenantName(name); err != nil {
		return nil, err
	}
	if err := validateSlug(slug); err != nil {
		return nil, err
	}

	plan := in.Plan
	if plan == "" {
		plan = models.PlanFree
	}
	if !plan.Valid() {
		return nil, fmt.Errorf("service: invalid plan %q", plan)
	}

	t := &models.Tenant{
		Name:     name,
		Slug:     slug,
		Plan:     plan,
		IsActive: true,
	}
	if err := s.repo.Create(ctx, t); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrTenantSlugTaken
		}
		return nil, fmt.Errorf("service: create tenant: %w", err)
	}
	return t, nil
}

// Get loads a tenant by ID with no tenant scoping. Platform operation, and
// the lookup the authentication middleware uses to resolve the caller's own
// tenant plan. Never call it from a handler with a client-supplied ID —
// use GetForScope.
func (s *TenantService) Get(ctx context.Context, id string) (*models.Tenant, error) {
	t, err := s.repo.GetByID(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrTenantNotFound
	}
	return t, err
}

// List returns tenants across the whole platform. Platform operation.
func (s *TenantService) List(ctx context.Context, search string, page, pageSize int) ([]*models.Tenant, int64, error) {
	return s.repo.List(ctx, search, page, pageSize)
}

type UpdateTenantInput struct {
	Name     *string
	Plan     *models.Plan
	IsActive *bool
}

// Update mutates any tenant, including its plan. Platform operation.
func (s *TenantService) Update(ctx context.Context, id string, in UpdateTenantInput) (*models.Tenant, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if err := validateTenantName(name); err != nil {
			return nil, err
		}
		t.Name = name
	}
	if in.Plan != nil {
		if !in.Plan.Valid() {
			return nil, fmt.Errorf("service: invalid plan %q", *in.Plan)
		}
		t.Plan = *in.Plan
	}
	if in.IsActive != nil {
		t.IsActive = *in.IsActive
	}

	if err := s.repo.Update(ctx, t); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("service: update tenant: %w", err)
	}
	return t, nil
}

// Delete soft-deletes any tenant. Platform operation.
func (s *TenantService) Delete(ctx context.Context, id string) error {
	err := s.repo.SoftDelete(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrTenantNotFound
	}
	return err
}

// ---------------------------------------------------------------------
// Tenant-scoped operations
// ---------------------------------------------------------------------

// Current returns the caller's own tenant.
func (s *TenantService) Current(ctx context.Context, scope tenancy.Scope) (*models.Tenant, error) {
	if !scope.Valid() {
		return nil, apierr.Unauthenticated("authentication is required")
	}
	t, err := s.repo.GetByID(ctx, scope.TenantID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, apierr.NotFound("tenant not found")
	}
	if err != nil {
		return nil, apierr.Internal("failed to load tenant").Wrap(err)
	}
	return t, nil
}

// GetForScope loads a tenant by ID on behalf of a tenant-scoped caller.
// Any ID other than the caller's own resolves to a not-found, so the
// endpoint cannot be used to probe which tenant IDs exist.
func (s *TenantService) GetForScope(ctx context.Context, scope tenancy.Scope, id string) (*models.Tenant, error) {
	if err := tenancy.Guard(scope, id, "tenant"); err != nil {
		return nil, err
	}
	return s.Current(ctx, scope)
}

// UpdateTenantSelfInput is the strictly narrower set of fields a tenant may
// change about itself.
type UpdateTenantSelfInput struct {
	Name *string
}

// UpdateForScope lets a tenant admin update their own tenant's profile.
//
// Plan is intentionally not settable here. A tenant's plan is what drives
// its feature entitlements, so allowing a tenant admin to write it would
// make every entitlement check self-service: any admin could grant
// themselves Enterprise features by editing their own record. Plan changes
// belong to billing, and are exposed only on the platform control plane.
func (s *TenantService) UpdateForScope(ctx context.Context, scope tenancy.Scope, id string, in UpdateTenantSelfInput) (*models.Tenant, error) {
	if err := tenancy.Guard(scope, id, "tenant"); err != nil {
		return nil, err
	}
	if scope.Role != string(models.RoleAdmin) {
		return nil, apierr.Forbidden("only a tenant admin may update the tenant profile")
	}

	t, err := s.Current(ctx, scope)
	if err != nil {
		return nil, err
	}

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if err := validateTenantName(name); err != nil {
			return nil, err
		}
		t.Name = name
	}

	if err := s.repo.Update(ctx, t); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apierr.NotFound("tenant not found")
		}
		return nil, apierr.Internal("failed to update tenant").Wrap(err)
	}
	return t, nil
}

// ListForScope returns exactly the caller's own tenant.
//
// The collection endpoint is kept for API compatibility, but a
// tenant-scoped credential can only ever see one row through it. The
// cross-tenant listing lives on the platform control plane.
func (s *TenantService) ListForScope(ctx context.Context, scope tenancy.Scope) ([]*models.Tenant, int64, error) {
	t, err := s.Current(ctx, scope)
	if err != nil {
		return nil, 0, err
	}
	return []*models.Tenant{t}, 1, nil
}

func validateTenantName(name string) error {
	if name == "" {
		return apierr.Validation("tenant name is required")
	}
	if len(name) > maxTenantNameLength {
		return apierr.Validation("tenant name must be at most %d characters", maxTenantNameLength)
	}
	return nil
}

func validateSlug(slug string) error {
	if len(slug) < minSlugLength || len(slug) > maxSlugLength {
		return apierr.Validation("tenant slug must be between %d and %d characters", minSlugLength, maxSlugLength)
	}
	if !slugPattern.MatchString(slug) {
		return apierr.Validation("tenant slug may contain only lowercase letters, digits and single hyphens")
	}
	return nil
}
