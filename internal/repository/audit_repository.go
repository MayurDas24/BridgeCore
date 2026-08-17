package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
)

// AuditRepository provides SQL data access for the audit trail.
type AuditRepository struct {
	db *database.DB
}

func NewAuditRepository(db *database.DB) *AuditRepository {
	return &AuditRepository{db: db}
}

// Create inserts one immutable audit record.
func (r *AuditRepository) Create(ctx context.Context, a *models.AuditLog) error {
	metadataJSON, err := json.Marshal(a.Metadata)
	if err != nil {
		return err
	}
	query := `
		INSERT INTO audit_logs (tenant_id, actor_id, event, metadata, endpoint, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`
	return r.db.QueryRowContext(ctx, query, a.TenantID, a.ActorID, a.Event, metadataJSON, a.Endpoint, a.IPAddress, a.UserAgent).
		Scan(&a.ID, &a.CreatedAt)
}

func (r *AuditRepository) GetByID(ctx context.Context, id string) (*models.AuditLog, error) {
	query := `
		SELECT id, tenant_id, actor_id, event, metadata, endpoint, ip_address, user_agent, created_at
		FROM audit_logs WHERE id = $1`
	a, err := scanAudit(r.db.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func (r *AuditRepository) ListByTenant(ctx context.Context, tenantID, eventFilter string, page, pageSize int) ([]*models.AuditLog, int64, error) {
	offset := (page - 1) * pageSize

	var total int64
	countQuery := `SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND ($2 = '' OR event = $2)`
	if err := r.db.QueryRowContext(ctx, countQuery, tenantID, eventFilter).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT id, tenant_id, actor_id, event, metadata, endpoint, ip_address, user_agent, created_at
		FROM audit_logs
		WHERE tenant_id = $1 AND ($2 = '' OR event = $2)
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4`
	rows, err := r.db.QueryContext(ctx, query, tenantID, eventFilter, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []*models.AuditLog
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return nil, 0, err
		}
		logs = append(logs, a)
	}
	return logs, total, rows.Err()
}

func scanAudit(row rowScanner) (*models.AuditLog, error) {
	var a models.AuditLog
	var metadataJSON []byte
	err := row.Scan(&a.ID, &a.TenantID, &a.ActorID, &a.Event, &metadataJSON, &a.Endpoint, &a.IPAddress, &a.UserAgent, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &a.Metadata); err != nil {
			return nil, err
		}
	}
	return &a, nil
}

// GetByIDInTenant is the tenant-scoped read for a single audit record.
func (r *AuditRepository) GetByIDInTenant(ctx context.Context, tenantID, id string) (*models.AuditLog, error) {
	query := `
		SELECT id, tenant_id, actor_id, event, metadata, endpoint, ip_address, user_agent, created_at
		FROM audit_logs WHERE id = $1 AND tenant_id = $2`
	a, err := scanAudit(r.db.QueryRowContext(ctx, query, id, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}
