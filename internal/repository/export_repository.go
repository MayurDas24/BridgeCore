package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
)

// ExportRepository provides SQL data access for asynchronous usage-export
// jobs, including the queue semantics the worker relies on.
type ExportRepository struct {
	db *database.DB
}

func NewExportRepository(db *database.DB) *ExportRepository {
	return &ExportRepository{db: db}
}

const exportJobColumns = `
	id, tenant_id, requested_by, status, endpoint, method, from_ts, to_ts,
	object_key, row_count, size_bytes, attempts, error, started_at, finished_at,
	created_at, updated_at`

// Create enqueues a new export job.
func (r *ExportRepository) Create(ctx context.Context, j *models.ExportJob) error {
	query := `
		INSERT INTO export_jobs (tenant_id, requested_by, status, endpoint, method, from_ts, to_ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, status, attempts, created_at, updated_at`
	err := r.db.QueryRowContext(ctx, query,
		j.TenantID, j.RequestedBy, models.ExportStatusQueued, j.Endpoint, j.Method, j.From, j.To,
	).Scan(&j.ID, &j.Status, &j.Attempts, &j.CreatedAt, &j.UpdatedAt)
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}

// GetByIDInTenant is the tenant-scoped read.
func (r *ExportRepository) GetByIDInTenant(ctx context.Context, tenantID, id string) (*models.ExportJob, error) {
	query := `SELECT ` + exportJobColumns + ` FROM export_jobs WHERE id = $1 AND tenant_id = $2`
	j, err := scanExportJob(r.db.QueryRowContext(ctx, query, id, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// GetByID is the unscoped read, for the worker only. Workers act on behalf
// of the platform rather than a tenant, so they legitimately need to load
// any job — which is exactly why this method is never reachable from an
// HTTP handler or a GraphQL resolver.
func (r *ExportRepository) GetByID(ctx context.Context, id string) (*models.ExportJob, error) {
	query := `SELECT ` + exportJobColumns + ` FROM export_jobs WHERE id = $1`
	j, err := scanExportJob(r.db.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// ListByTenant returns a tenant's export jobs, newest first.
func (r *ExportRepository) ListByTenant(ctx context.Context, tenantID string, page, pageSize int) ([]*models.ExportJob, int64, error) {
	offset := (page - 1) * pageSize

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM export_jobs WHERE tenant_id = $1`, tenantID).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT ` + exportJobColumns + `
		FROM export_jobs WHERE tenant_id = $1
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`
	rows, err := r.db.QueryContext(ctx, query, tenantID, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var jobs []*models.ExportJob
	for rows.Next() {
		j, err := scanExportJob(rows)
		if err != nil {
			return nil, 0, err
		}
		jobs = append(jobs, j)
	}
	return jobs, total, rows.Err()
}

// ClaimQueued atomically transitions up to limit queued jobs to
// "processing" and returns them.
//
// The claim is a single statement built on `FOR UPDATE SKIP LOCKED`, which
// is what makes the worker horizontally scalable: several workers can poll
// concurrently and each row is handed to exactly one of them, with no
// advisory locks, no leader election, and no window where two workers
// generate the same export. SKIP LOCKED means a worker never blocks waiting
// for a row another worker already holds — it just takes the next one.
func (r *ExportRepository) ClaimQueued(ctx context.Context, limit int) ([]*models.ExportJob, error) {
	if limit <= 0 {
		return nil, nil
	}

	query := `
		WITH claimed AS (
			SELECT id
			FROM export_jobs
			WHERE status = 'queued'
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE export_jobs j
		SET status = 'processing',
		    attempts = j.attempts + 1,
		    started_at = now(),
		    updated_at = now()
		FROM claimed
		WHERE j.id = claimed.id
		RETURNING j.id, j.tenant_id, j.requested_by, j.status, j.endpoint, j.method,
		          j.from_ts, j.to_ts, j.object_key, j.row_count, j.size_bytes,
		          j.attempts, j.error, j.started_at, j.finished_at, j.created_at, j.updated_at`

	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*models.ExportJob
	for rows.Next() {
		j, err := scanExportJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// MarkCompleted records a successful export.
func (r *ExportRepository) MarkCompleted(ctx context.Context, id, objectKey string, rowCount int, sizeBytes int64) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE export_jobs
		SET status = 'completed', object_key = $2, row_count = $3, size_bytes = $4,
		    error = '', finished_at = now(), updated_at = now()
		WHERE id = $1`, id, objectKey, rowCount, sizeBytes)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkFailed records a permanent failure once retries are exhausted.
func (r *ExportRepository) MarkFailed(ctx context.Context, id, reason string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE export_jobs
		SET status = 'failed', error = $2, finished_at = now(), updated_at = now()
		WHERE id = $1`, id, truncate(reason, 2000))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Requeue returns a job to the queue after a transient failure, recording
// why. attempts has already been incremented by the claim, so the worker's
// retry budget is enforced without a second write.
func (r *ExportRepository) Requeue(ctx context.Context, id, reason string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE export_jobs
		SET status = 'queued', error = $2, started_at = NULL, updated_at = now()
		WHERE id = $1`, id, truncate(reason, 2000))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReleaseStale returns jobs that have been "processing" for longer than
// olderThan back to the queue.
//
// This is the recovery path for a worker that died mid-job: without it, a
// crashed task would leave rows stuck in processing forever. Jobs that have
// already consumed their retry budget are failed instead of looping.
func (r *ExportRepository) ReleaseStale(ctx context.Context, olderThan time.Duration, maxAttempts int) (int64, error) {
	cutoff := time.Now().Add(-olderThan)

	res, err := r.db.ExecContext(ctx, `
		UPDATE export_jobs
		SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END,
		    error = 'worker did not finish the job before the visibility timeout expired',
		    finished_at = CASE WHEN attempts >= $2 THEN now() ELSE NULL END,
		    started_at = NULL,
		    updated_at = now()
		WHERE status = 'processing' AND started_at < $1`, cutoff, maxAttempts)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountQueued reports the current queue depth, exported as a CloudWatch
// metric so alarms can fire on a backlog.
func (r *ExportRepository) CountQueued(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM export_jobs WHERE status = 'queued'`).Scan(&n)
	return n, err
}

func scanExportJob(row rowScanner) (*models.ExportJob, error) {
	var j models.ExportJob
	err := row.Scan(
		&j.ID, &j.TenantID, &j.RequestedBy, &j.Status, &j.Endpoint, &j.Method,
		&j.From, &j.To, &j.ObjectKey, &j.RowCount, &j.SizeBytes, &j.Attempts,
		&j.Error, &j.StartedAt, &j.FinishedAt, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
