package repository

import (
	"context"
	"time"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
)

// UsageRepository provides SQL data access for usage metering records.
type UsageRepository struct {
	db *database.DB
}

func NewUsageRepository(db *database.DB) *UsageRepository {
	return &UsageRepository{db: db}
}

// Create inserts one usage record. This is called from the metering
// middleware on every single request, so it is intentionally a single,
// minimal INSERT with no joins.
func (r *UsageRepository) Create(ctx context.Context, u *models.UsageLog) error {
	query := `
		INSERT INTO usage_logs (tenant_id, endpoint, method, status_code, latency_ms, request_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`
	return r.db.QueryRowContext(ctx, query, u.TenantID, u.Endpoint, u.Method, u.StatusCode, u.LatencyMS, u.RequestID).
		Scan(&u.ID, &u.CreatedAt)
}

// ListByTenant returns paginated, optionally filtered usage records for a tenant.
func (r *UsageRepository) ListByTenant(ctx context.Context, tenantID, endpointFilter, methodFilter string, from, to *time.Time, page, pageSize int) ([]*models.UsageLog, int64, error) {
	offset := (page - 1) * pageSize

	countQuery := `
		SELECT COUNT(*) FROM usage_logs
		WHERE tenant_id = $1
		  AND ($2 = '' OR endpoint ILIKE '%' || $2 || '%')
		  AND ($3 = '' OR method = $3)
		  AND ($4::timestamptz IS NULL OR created_at >= $4)
		  AND ($5::timestamptz IS NULL OR created_at <= $5)`
	var total int64
	if err := r.db.QueryRowContext(ctx, countQuery, tenantID, endpointFilter, methodFilter, from, to).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT id, tenant_id, endpoint, method, status_code, latency_ms, request_id, created_at
		FROM usage_logs
		WHERE tenant_id = $1
		  AND ($2 = '' OR endpoint ILIKE '%' || $2 || '%')
		  AND ($3 = '' OR method = $3)
		  AND ($4::timestamptz IS NULL OR created_at >= $4)
		  AND ($5::timestamptz IS NULL OR created_at <= $5)
		ORDER BY created_at DESC
		LIMIT $6 OFFSET $7`
	rows, err := r.db.QueryContext(ctx, query, tenantID, endpointFilter, methodFilter, from, to, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []*models.UsageLog
	for rows.Next() {
		var u models.UsageLog
		if err := rows.Scan(&u.ID, &u.TenantID, &u.Endpoint, &u.Method, &u.StatusCode, &u.LatencyMS, &u.RequestID, &u.CreatedAt); err != nil {
			return nil, 0, err
		}
		logs = append(logs, &u)
	}
	return logs, total, rows.Err()
}

// EndpointSummary aggregates request counts, error counts, and average
// latency per endpoint for a tenant over a time window.
type EndpointSummary struct {
	Endpoint     string  `json:"endpoint"`
	Method       string  `json:"method"`
	RequestCount int64   `json:"request_count"`
	ErrorCount   int64   `json:"error_count"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

func (r *UsageRepository) SummaryByTenant(ctx context.Context, tenantID string, from, to *time.Time) ([]EndpointSummary, error) {
	query := `
		SELECT endpoint, method,
		       COUNT(*) AS request_count,
		       COUNT(*) FILTER (WHERE status_code >= 400) AS error_count,
		       COALESCE(AVG(latency_ms), 0) AS avg_latency_ms
		FROM usage_logs
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR created_at >= $2)
		  AND ($3::timestamptz IS NULL OR created_at <= $3)
		GROUP BY endpoint, method
		ORDER BY request_count DESC`
	rows, err := r.db.QueryContext(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []EndpointSummary
	for rows.Next() {
		var s EndpointSummary
		if err := rows.Scan(&s.Endpoint, &s.Method, &s.RequestCount, &s.ErrorCount, &s.AvgLatencyMS); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}
