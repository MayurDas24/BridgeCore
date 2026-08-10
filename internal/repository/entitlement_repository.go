package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
)

// EntitlementRepository provides SQL data access for the feature catalog
// and per-tenant entitlements.
type EntitlementRepository struct {
	db *database.DB
}

func NewEntitlementRepository(db *database.DB) *EntitlementRepository {
	return &EntitlementRepository{db: db}
}

func (r *EntitlementRepository) CreateFeature(ctx context.Context, f *models.Feature) error {
	query := `
		INSERT INTO features (key, name, description)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, updated_at = now()
		RETURNING id, created_at, updated_at`
	return r.db.QueryRowContext(ctx, query, f.Key, f.Name, f.Description).Scan(&f.ID, &f.CreatedAt, &f.UpdatedAt)
}

func (r *EntitlementRepository) GetFeatureByKey(ctx context.Context, key string) (*models.Feature, error) {
	var f models.Feature
	err := r.db.QueryRowContext(ctx, `SELECT id, key, name, description, created_at, updated_at FROM features WHERE key = $1`, key).
		Scan(&f.ID, &f.Key, &f.Name, &f.Description, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &f, err
}

func (r *EntitlementRepository) ListFeatures(ctx context.Context) ([]*models.Feature, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, key, name, description, created_at, updated_at FROM features ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var features []*models.Feature
	for rows.Next() {
		var f models.Feature
		if err := rows.Scan(&f.ID, &f.Key, &f.Name, &f.Description, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		features = append(features, &f)
	}
	return features, rows.Err()
}

// GrantFeature entitles a tenant to a feature (idempotent upsert).
func (r *EntitlementRepository) GrantFeature(ctx context.Context, tenantID, featureID string, enabled bool) error {
	query := `
		INSERT INTO tenant_features (tenant_id, feature_id, enabled)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, feature_id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`
	_, err := r.db.ExecContext(ctx, query, tenantID, featureID, enabled)
	return err
}

// TenantHasFeature checks whether a tenant is entitled to a feature key,
// either via an explicit tenant_features grant or via its subscription
// plan's default feature set (see service/entitlement_service.go for the
// plan -> feature defaults).
func (r *EntitlementRepository) TenantHasFeature(ctx context.Context, tenantID, featureKey string) (bool, error) {
	query := `
		SELECT tf.enabled
		FROM tenant_features tf
		JOIN features f ON f.id = tf.feature_id
		WHERE tf.tenant_id = $1 AND f.key = $2`
	var enabled bool
	err := r.db.QueryRowContext(ctx, query, tenantID, featureKey).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled, nil
}

// ListTenantFeatures returns every feature key currently enabled for a tenant.
func (r *EntitlementRepository) ListTenantFeatures(ctx context.Context, tenantID string) ([]string, error) {
	query := `
		SELECT f.key
		FROM tenant_features tf
		JOIN features f ON f.id = tf.feature_id
		WHERE tf.tenant_id = $1 AND tf.enabled = TRUE
		ORDER BY f.key`
	rows, err := r.db.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
