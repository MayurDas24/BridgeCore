package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
)

type fakeTenantStoreFull struct {
	byID   map[string]*models.Tenant
	bySlug map[string]*models.Tenant
	nextID int
}

func newFakeTenantStoreFull() *fakeTenantStoreFull {
	return &fakeTenantStoreFull{byID: map[string]*models.Tenant{}, bySlug: map[string]*models.Tenant{}}
}

func (f *fakeTenantStoreFull) Create(ctx context.Context, t *models.Tenant) error {
	if _, exists := f.bySlug[t.Slug]; exists {
		return repository.ErrConflict
	}
	f.nextID++
	t.ID = "tenant-" + itoa(f.nextID)
	t.CreatedAt = time.Now()
	t.UpdatedAt = time.Now()
	f.byID[t.ID] = t
	f.bySlug[t.Slug] = t
	return nil
}

func (f *fakeTenantStoreFull) GetByID(ctx context.Context, id string) (*models.Tenant, error) {
	t, ok := f.byID[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return t, nil
}

func (f *fakeTenantStoreFull) GetBySlug(ctx context.Context, slug string) (*models.Tenant, error) {
	t, ok := f.bySlug[slug]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return t, nil
}

func (f *fakeTenantStoreFull) List(ctx context.Context, search string, page, pageSize int) ([]*models.Tenant, int64, error) {
	var out []*models.Tenant
	for _, t := range f.byID {
		out = append(out, t)
	}
	return out, int64(len(out)), nil
}

func (f *fakeTenantStoreFull) Update(ctx context.Context, t *models.Tenant) error {
	if _, ok := f.byID[t.ID]; !ok {
		return repository.ErrNotFound
	}
	t.UpdatedAt = time.Now()
	f.byID[t.ID] = t
	f.bySlug[t.Slug] = t
	return nil
}

func (f *fakeTenantStoreFull) SoftDelete(ctx context.Context, id string) error {
	t, ok := f.byID[id]
	if !ok {
		return repository.ErrNotFound
	}
	now := time.Now()
	t.DeletedAt = &now
	delete(f.byID, id)
	return nil
}

func newTestTenantService() *TenantService {
	return NewTenantService(newFakeTenantStoreFull())
}

func TestTenantService_Create_DefaultsToFreePlan(t *testing.T) {
	svc := newTestTenantService()

	tenant, err := svc.Create(context.Background(), CreateTenantInput{Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if tenant.Plan != models.PlanFree {
		t.Fatalf("expected default plan to be free, got %s", tenant.Plan)
	}
	if !tenant.IsActive {
		t.Fatal("expected newly created tenant to be active")
	}
}

func TestTenantService_Create_RejectsInvalidPlan(t *testing.T) {
	svc := newTestTenantService()

	_, err := svc.Create(context.Background(), CreateTenantInput{Name: "Acme", Slug: "acme", Plan: models.Plan("unlimited")})
	if err == nil {
		t.Fatal("expected an error for an invalid plan value")
	}
}

func TestTenantService_Create_RejectsDuplicateSlug(t *testing.T) {
	svc := newTestTenantService()
	ctx := context.Background()

	if _, err := svc.Create(ctx, CreateTenantInput{Name: "Acme", Slug: "acme"}); err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, err := svc.Create(ctx, CreateTenantInput{Name: "Acme 2", Slug: "acme"})
	if !errors.Is(err, ErrTenantSlugTaken) {
		t.Fatalf("expected ErrTenantSlugTaken, got %v", err)
	}
}

func TestTenantService_Get_ReturnsNotFoundForMissingTenant(t *testing.T) {
	svc := newTestTenantService()

	_, err := svc.Get(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("expected ErrTenantNotFound, got %v", err)
	}
}

func TestTenantService_Update_ChangesPlanAndActiveStatus(t *testing.T) {
	svc := newTestTenantService()
	ctx := context.Background()

	tenant, err := svc.Create(ctx, CreateTenantInput{Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	newPlan := models.PlanEnterprise
	inactive := false
	updated, err := svc.Update(ctx, tenant.ID, UpdateTenantInput{Plan: &newPlan, IsActive: &inactive})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Plan != models.PlanEnterprise {
		t.Fatalf("expected plan to be updated to enterprise, got %s", updated.Plan)
	}
	if updated.IsActive {
		t.Fatal("expected tenant to be marked inactive")
	}
}

func TestTenantService_Update_RejectsInvalidPlan(t *testing.T) {
	svc := newTestTenantService()
	ctx := context.Background()

	tenant, err := svc.Create(ctx, CreateTenantInput{Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	badPlan := models.Plan("ultra")
	_, err = svc.Update(ctx, tenant.ID, UpdateTenantInput{Plan: &badPlan})
	if err == nil {
		t.Fatal("expected an error for an invalid plan on update")
	}
}

func TestTenantService_Delete_SoftDeletesTenant(t *testing.T) {
	svc := newTestTenantService()
	ctx := context.Background()

	tenant, err := svc.Create(ctx, CreateTenantInput{Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := svc.Delete(ctx, tenant.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := svc.Get(ctx, tenant.ID); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("expected tenant to be unreachable after delete, got err=%v", err)
	}
}

func TestTenantService_Delete_ReturnsNotFoundForMissingTenant(t *testing.T) {
	svc := newTestTenantService()

	err := svc.Delete(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("expected ErrTenantNotFound, got %v", err)
	}
}
