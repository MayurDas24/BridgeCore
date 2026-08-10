package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
)

var ErrTenantNotFound = errors.New("service: tenant not found")

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

func (s *TenantService) Create(ctx context.Context, in CreateTenantInput) (*models.Tenant, error) {
	plan := in.Plan
	if plan == "" {
		plan = models.PlanFree
	}
	if !plan.Valid() {
		return nil, fmt.Errorf("service: invalid plan %q", plan)
	}

	t := &models.Tenant{
		Name:     in.Name,
		Slug:     in.Slug,
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

func (s *TenantService) Get(ctx context.Context, id string) (*models.Tenant, error) {
	t, err := s.repo.GetByID(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrTenantNotFound
	}
	return t, err
}

func (s *TenantService) List(ctx context.Context, search string, page, pageSize int) ([]*models.Tenant, int64, error) {
	return s.repo.List(ctx, search, page, pageSize)
}

type UpdateTenantInput struct {
	Name     *string
	Plan     *models.Plan
	IsActive *bool
}

func (s *TenantService) Update(ctx context.Context, id string, in UpdateTenantInput) (*models.Tenant, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if in.Name != nil {
		t.Name = *in.Name
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

func (s *TenantService) Delete(ctx context.Context, id string) error {
	err := s.repo.SoftDelete(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrTenantNotFound
	}
	return err
}
