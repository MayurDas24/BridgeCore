package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
)

// ErrNotFound is returned by repository methods when a row doesn't exist.
var ErrNotFound = errors.New("repository: record not found")

// ErrConflict is returned when a unique constraint would be violated.
var ErrConflict = errors.New("repository: unique constraint conflict")

// TenantRepository provides SQL data access for tenants.
type TenantRepository struct {
	db *database.DB
}

func NewTenantRepository(db *database.DB) *TenantRepository {
	return &TenantRepository{db: db}
}

func (r *TenantRepository) Create(ctx context.Context, t *models.Tenant) error {
	query := `
		INSERT INTO tenants (name, slug, plan, is_active)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at, updated_at`
	err := r.db.QueryRowContext(ctx, query, t.Name, t.Slug, t.Plan, t.IsActive).
		Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (r *TenantRepository) GetByID(ctx context.Context, id string) (*models.Tenant, error) {
	query := `
		SELECT id, name, slug, plan, is_active, created_at, updated_at, deleted_at
		FROM tenants WHERE id = $1 AND deleted_at IS NULL`
	return r.scanOne(r.db.QueryRowContext(ctx, query, id))
}

func (r *TenantRepository) GetBySlug(ctx context.Context, slug string) (*models.Tenant, error) {
	query := `
		SELECT id, name, slug, plan, is_active, created_at, updated_at, deleted_at
		FROM tenants WHERE slug = $1 AND deleted_at IS NULL`
	return r.scanOne(r.db.QueryRowContext(ctx, query, slug))
}

func (r *TenantRepository) List(ctx context.Context, search string, page, pageSize int) ([]*models.Tenant, int64, error) {
	offset := (page - 1) * pageSize

	countQuery := `SELECT COUNT(*) FROM tenants WHERE deleted_at IS NULL AND ($1 = '' OR name ILIKE '%' || $1 || '%' OR slug ILIKE '%' || $1 || '%')`
	var total int64
	if err := r.db.QueryRowContext(ctx, countQuery, search).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT id, name, slug, plan, is_active, created_at, updated_at, deleted_at
		FROM tenants
		WHERE deleted_at IS NULL AND ($1 = '' OR name ILIKE '%' || $1 || '%' OR slug ILIKE '%' || $1 || '%')
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.QueryContext(ctx, query, search, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var tenants []*models.Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, 0, err
		}
		tenants = append(tenants, t)
	}
	return tenants, total, rows.Err()
}

func (r *TenantRepository) Update(ctx context.Context, t *models.Tenant) error {
	query := `
		UPDATE tenants SET name = $1, plan = $2, is_active = $3, updated_at = now()
		WHERE id = $4 AND deleted_at IS NULL
		RETURNING updated_at`
	err := r.db.QueryRowContext(ctx, query, t.Name, t.Plan, t.IsActive, t.ID).Scan(&t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (r *TenantRepository) SoftDelete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (r *TenantRepository) scanOne(row rowScanner) (*models.Tenant, error) {
	t, err := scanTenant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func scanTenant(row rowScanner) (*models.Tenant, error) {
	var t models.Tenant
	err := row.Scan(&t.ID, &t.Name, &t.Slug, &t.Plan, &t.IsActive, &t.CreatedAt, &t.UpdatedAt, &t.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return containsAny(err.Error(), "unique constraint", "duplicate key")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
