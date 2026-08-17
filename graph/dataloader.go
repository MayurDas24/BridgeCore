package graph

import (
	"context"
	"sync"

	"github.com/bridgecore/bridgecore/graph/model"
	"github.com/bridgecore/bridgecore/internal/models"
)

// loadersKey is the context key the per-request loaders are stored under.
type loadersKey struct{}

// BatchLoader eliminates the N+1 query problem for a nested GraphQL field.
//
// The problem it solves: resolving `users(pageSize: 100) { tenant { name } }`
// runs the tenant resolver once per user. Naively that is one query for the
// users plus one hundred point reads for their tenants — 101 round trips for
// data that fits in two.
//
// The strategy here is prime-then-load, rather than the deferred-dispatch
// batching a JavaScript DataLoader uses. The parent resolver already knows
// every key its children will ask for, so it issues one `WHERE id = ANY($1)`
// query up front and primes the cache; the child resolvers then hit the cache.
// A key that was somehow not primed still resolves correctly by falling back
// to a single fetch, so correctness never depends on the optimisation having
// been applied — it just costs more.
//
// Deferred dispatch would be the more general answer, but it requires the
// executor to resolve sibling fields concurrently so the batch window can
// fill. Prime-then-load gets the same query count with no concurrency
// assumptions and no risk of a resolver blocking forever waiting for a batch
// that will never fill.
type BatchLoader[K comparable, V any] struct {
	batch  func(ctx context.Context, keys []K) (map[K]V, error)
	single func(ctx context.Context, key K) (V, error)

	mu     sync.Mutex
	cache  map[K]V
	missed map[K]bool

	// queries counts round trips, which is what the N+1 test asserts on.
	queries int
}

// NewBatchLoader builds a loader from a batch function and a single-key
// fallback.
func NewBatchLoader[K comparable, V any](
	batch func(ctx context.Context, keys []K) (map[K]V, error),
	single func(ctx context.Context, key K) (V, error),
) *BatchLoader[K, V] {
	return &BatchLoader[K, V]{
		batch:  batch,
		single: single,
		cache:  map[K]V{},
		missed: map[K]bool{},
	}
}

// Prime loads every key that is not already cached in one round trip.
func (l *BatchLoader[K, V]) Prime(ctx context.Context, keys []K) error {
	l.mu.Lock()
	pending := make([]K, 0, len(keys))
	seen := map[K]bool{}
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, cached := l.cache[k]; !cached && !l.missed[k] {
			pending = append(pending, k)
		}
	}
	l.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	loaded, err := l.batch(ctx, pending)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries++
	for _, k := range pending {
		if v, ok := loaded[k]; ok {
			l.cache[k] = v
		} else {
			// Remember the miss so a later Load does not re-query for a key
			// that is known not to exist.
			l.missed[k] = true
		}
	}
	return nil
}

// Load returns a value for one key, using the primed cache when possible.
func (l *BatchLoader[K, V]) Load(ctx context.Context, key K) (V, error) {
	l.mu.Lock()
	if v, ok := l.cache[key]; ok {
		l.mu.Unlock()
		return v, nil
	}
	if l.missed[key] {
		l.mu.Unlock()
		var zero V
		return zero, nil
	}
	l.mu.Unlock()

	v, err := l.single(ctx, key)
	if err != nil {
		var zero V
		return zero, err
	}

	l.mu.Lock()
	l.queries++
	l.cache[key] = v
	l.mu.Unlock()
	return v, nil
}

// Queries reports how many round trips the loader has made, exposed so the
// N+1 behaviour can be asserted in a test rather than assumed.
func (l *BatchLoader[K, V]) Queries() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.queries
}

// TenantSource is the persistence contract the tenant loader needs.
type TenantSource interface {
	ListByIDs(ctx context.Context, ids []string) ([]*models.Tenant, error)
	GetByID(ctx context.Context, id string) (*models.Tenant, error)
}

// Loaders holds the per-request loader set.
//
// They are per request, never global: a loader is a request-scoped cache, and
// a cache of tenant rows shared across requests would happily serve one
// tenant a stale row that another request loaded — or, far worse, keep
// serving a suspended tenant as active.
type Loaders struct {
	Tenant *BatchLoader[string, *model.Tenant]
}

// NewLoaders builds a fresh loader set for one request.
func NewLoaders(tenants TenantSource) *Loaders {
	return &Loaders{
		Tenant: NewBatchLoader(
			func(ctx context.Context, ids []string) (map[string]*model.Tenant, error) {
				rows, err := tenants.ListByIDs(ctx, ids)
				if err != nil {
					return nil, err
				}
				out := make(map[string]*model.Tenant, len(rows))
				for _, t := range rows {
					out[t.ID] = model.NewTenant(t)
				}
				return out, nil
			},
			func(ctx context.Context, id string) (*model.Tenant, error) {
				t, err := tenants.GetByID(ctx, id)
				if err != nil {
					return nil, err
				}
				return model.NewTenant(t), nil
			},
		),
	}
}

// WithLoaders attaches a loader set to a request context.
func WithLoaders(ctx context.Context, l *Loaders) context.Context {
	return context.WithValue(ctx, loadersKey{}, l)
}

// LoadersFrom retrieves the loader set, returning nil when absent so callers
// can degrade to direct reads rather than panic.
func LoadersFrom(ctx context.Context) *Loaders {
	l, _ := ctx.Value(loadersKey{}).(*Loaders)
	return l
}
