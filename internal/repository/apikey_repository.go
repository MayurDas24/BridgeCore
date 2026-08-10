package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
)

// APIKeyRepository provides SQL data access for API keys.
type APIKeyRepository struct {
	db *database.DB
}

func NewAPIKeyRepository(db *database.DB) *APIKeyRepository {
	return &APIKeyRepository{db: db}
}

func (r *APIKeyRepository) Create(ctx context.Context, k *models.APIKey) error {
	query := `
		INSERT INTO api_keys (tenant_id, created_by, name, prefix, key_hash, last_four, is_active, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`
	return r.db.QueryRowContext(ctx, query, k.TenantID, k.CreatedBy, k.Name, k.Prefix, k.KeyHash, k.LastFour, k.IsActive, k.ExpiresAt).
		Scan(&k.ID, &k.CreatedAt, &k.UpdatedAt)
}

// ListActiveByPrefix returns all active keys sharing a prefix, which is the
// candidate set the auth middleware bcrypt-compares against when
// authenticating an incoming API key. Prefix alone isn't unique, so every
// active key must be checked (bounded, cheap set in practice).
func (r *APIKeyRepository) ListActiveByPrefix(ctx context.Context, prefix string) ([]*models.APIKey, error) {
	query := `
		SELECT id, tenant_id, created_by, name, prefix, key_hash, last_four, is_active, last_used_at, expires_at, created_at, updated_at, revoked_at
		FROM api_keys
		WHERE prefix = $1 AND is_active = TRUE AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`
	rows, err := r.db.QueryContext(ctx, query, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*models.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (r *APIKeyRepository) ListByTenant(ctx context.Context, tenantID string) ([]*models.APIKey, error) {
	query := `
		SELECT id, tenant_id, created_by, name, prefix, key_hash, last_four, is_active, last_used_at, expires_at, created_at, updated_at, revoked_at
		FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*models.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (r *APIKeyRepository) GetByID(ctx context.Context, id string) (*models.APIKey, error) {
	query := `
		SELECT id, tenant_id, created_by, name, prefix, key_hash, last_four, is_active, last_used_at, expires_at, created_at, updated_at, revoked_at
		FROM api_keys WHERE id = $1`
	k, err := scanAPIKey(r.db.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)
	return err
}

func (r *APIKeyRepository) Deactivate(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE api_keys SET is_active = FALSE, revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanAPIKey(row rowScanner) (*models.APIKey, error) {
	var k models.APIKey
	err := row.Scan(
		&k.ID, &k.TenantID, &k.CreatedBy, &k.Name, &k.Prefix, &k.KeyHash, &k.LastFour,
		&k.IsActive, &k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt, &k.UpdatedAt, &k.RevokedAt,
	)
	if err != nil {
		return nil, err
	}
	return &k, nil
}
