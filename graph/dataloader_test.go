package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/bridgecore/bridgecore/internal/models"
)

// countingTenantSource records how many round trips it serves, which is the
// property the N+1 fix is actually about.
type countingTenantSource struct {
	tenants     map[string]*models.Tenant
	batchCalls  int
	singleCalls int
}

func (c *countingTenantSource) ListByIDs(ctx context.Context, ids []string) ([]*models.Tenant, error) {
	c.batchCalls++
	var out []*models.Tenant
	for _, id := range ids {
		if t, ok := c.tenants[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (c *countingTenantSource) GetByID(ctx context.Context, id string) (*models.Tenant, error) {
	c.singleCalls++
	t, ok := c.tenants[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return t, nil
}

func newCountingSource(ids ...string) *countingTenantSource {
	src := &countingTenantSource{tenants: map[string]*models.Tenant{}}
	for _, id := range ids {
		src.tenants[id] = &models.Tenant{ID: id, Name: "Tenant " + id, Plan: models.PlanPro, IsActive: true}
	}
	return src
}

func TestBatchLoader_CollapsesNPlusOneIntoOneQuery(t *testing.T) {
	src := newCountingSource("t1", "t2", "t3")
	loaders := NewLoaders(src)
	ctx := context.Background()

	// A page of 100 users spread across three tenants: the parent resolver
	// primes once...
	keys := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		keys = append(keys, []string{"t1", "t2", "t3"}[i%3])
	}
	if err := loaders.Tenant.Prime(ctx, keys); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}

	// ...and the 100 child resolvers all hit the cache.
	for _, k := range keys {
		tenant, err := loaders.Tenant.Load(ctx, k)
		if err != nil {
			t.Fatalf("Load(%q) error = %v", k, err)
		}
		if tenant == nil || tenant.ID != k {
			t.Fatalf("Load(%q) returned %+v", k, tenant)
		}
	}

	if src.batchCalls != 1 {
		t.Errorf("expected exactly 1 batched query, got %d", src.batchCalls)
	}
	if src.singleCalls != 0 {
		t.Errorf("expected no per-row queries after priming, got %d", src.singleCalls)
	}
	if got := loaders.Tenant.Queries(); got != 1 {
		t.Errorf("loader reported %d queries, want 1", got)
	}
}

func TestBatchLoader_DeduplicatesKeysWhenPriming(t *testing.T) {
	src := newCountingSource("t1")
	loaders := NewLoaders(src)

	if err := loaders.Tenant.Prime(context.Background(), []string{"t1", "t1", "t1"}); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	if src.batchCalls != 1 {
		t.Errorf("expected 1 query for a repeated key, got %d", src.batchCalls)
	}
}

func TestBatchLoader_FallsBackWhenNotPrimed(t *testing.T) {
	// Correctness must not depend on the optimisation having been applied.
	src := newCountingSource("t1")
	loaders := NewLoaders(src)

	tenant, err := loaders.Tenant.Load(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if tenant == nil || tenant.ID != "t1" {
		t.Fatalf("expected the unprimed key to resolve, got %+v", tenant)
	}
	if src.singleCalls != 1 {
		t.Errorf("expected 1 fallback query, got %d", src.singleCalls)
	}
}

func TestBatchLoader_CachesRepeatLoads(t *testing.T) {
	src := newCountingSource("t1")
	loaders := NewLoaders(src)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := loaders.Tenant.Load(ctx, "t1"); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
	}
	if src.singleCalls != 1 {
		t.Errorf("expected repeat loads to be served from cache, got %d queries", src.singleCalls)
	}
}

func TestBatchLoader_RemembersMissesWithoutRequerying(t *testing.T) {
	src := newCountingSource("t1")
	loaders := NewLoaders(src)
	ctx := context.Background()

	if err := loaders.Tenant.Prime(ctx, []string{"t1", "missing"}); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}

	tenant, err := loaders.Tenant.Load(ctx, "missing")
	if err != nil {
		t.Fatalf("expected a known-absent key to resolve as nil, got error %v", err)
	}
	if tenant != nil {
		t.Errorf("expected nil for a missing tenant, got %+v", tenant)
	}
	if src.singleCalls != 0 {
		t.Errorf("a known miss must not trigger another query, got %d", src.singleCalls)
	}
}

func TestLoadersFrom_ReturnsNilWhenAbsent(t *testing.T) {
	if LoadersFrom(context.Background()) != nil {
		t.Error("expected no loaders on a bare context")
	}
	loaders := NewLoaders(newCountingSource())
	ctx := WithLoaders(context.Background(), loaders)
	if LoadersFrom(ctx) != loaders {
		t.Error("expected the attached loader set to be retrievable")
	}
}
