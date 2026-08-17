package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/exports"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/tenancy"
	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// ExportJobStore is the persistence contract ExportService needs.
type ExportJobStore interface {
	Create(ctx context.Context, j *models.ExportJob) error
	GetByIDInTenant(ctx context.Context, tenantID, id string) (*models.ExportJob, error)
	ListByTenant(ctx context.Context, tenantID string, page, pageSize int) ([]*models.ExportJob, int64, error)
	CountQueued(ctx context.Context) (int64, error)
}

// ExportService turns a synchronous, unbounded CSV download into a tracked
// asynchronous job.
//
// The endpoint it replaces held an HTTP connection open while it paged
// through the whole usage table and serialized it into the response: a
// large tenant's export could exceed the load balancer's idle timeout, and
// the work was lost on any retry. Here the request is durable, the response
// is immediate, and the result is a private object plus a short-lived
// download URL.
type ExportService struct {
	jobs     ExportJobStore
	store    exports.ObjectStore
	notifier exports.Notifier

	downloadTTL time.Duration
	maxRows     int
	log         *zap.Logger
}

func NewExportService(
	jobs ExportJobStore,
	store exports.ObjectStore,
	notifier exports.Notifier,
	downloadTTL time.Duration,
	maxRows int,
	log *zap.Logger,
) *ExportService {
	return &ExportService{
		jobs:        jobs,
		store:       store,
		notifier:    notifier,
		downloadTTL: downloadTTL,
		maxRows:     maxRows,
		log:         log,
	}
}

// RequestExportInput describes a requested export.
type RequestExportInput struct {
	Endpoint string
	Method   string
	From     *time.Time
	To       *time.Time
}

// Request enqueues an export for the caller's tenant.
//
// The tenant ID comes from the verified scope, never from the request, which
// is what makes it impossible to request an export of another tenant's usage
// data — the most valuable thing an attacker could ask this endpoint for.
func (s *ExportService) Request(ctx context.Context, scope tenancy.Scope, in RequestExportInput) (*models.ExportJob, error) {
	if !scope.Valid() {
		return nil, apierr.Unauthenticated("authentication is required")
	}

	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method != "" && !isHTTPMethod(method) {
		return nil, apierr.Validation("method must be a valid HTTP method")
	}
	if len(in.Endpoint) > 255 {
		return nil, apierr.Validation("endpoint filter must be at most 255 characters")
	}
	if in.From != nil && in.To != nil && in.To.Before(*in.From) {
		return nil, apierr.Validation("the 'to' timestamp must not be before 'from'")
	}

	job := &models.ExportJob{
		TenantID: scope.TenantID,
		Endpoint: strings.TrimSpace(in.Endpoint),
		Method:   method,
		From:     in.From,
		To:       in.To,
	}
	if scope.UserID != "" {
		userID := scope.UserID
		job.RequestedBy = &userID
	}

	if err := s.jobs.Create(ctx, job); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apierr.NotFound("tenant not found")
		}
		return nil, apierr.Internal("failed to enqueue the export").Wrap(err)
	}

	// Notification is best-effort: the durable job row is the source of
	// truth and the worker polls it, so a queue outage delays the export
	// rather than losing it.
	if err := s.notifier.Publish(ctx, exports.JobNotification{JobID: job.ID, TenantID: job.TenantID}); err != nil {
		s.log.Warn("failed to publish export job notification; the worker will pick it up by polling",
			zap.Error(err),
			zap.String("export_job_id", job.ID),
		)
	}

	return job, nil
}

// Get returns one export job belonging to the caller's tenant.
func (s *ExportService) Get(ctx context.Context, scope tenancy.Scope, id string) (*models.ExportJob, error) {
	if !scope.Valid() {
		return nil, apierr.Unauthenticated("authentication is required")
	}
	job, err := s.jobs.GetByIDInTenant(ctx, scope.TenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, apierr.NotFound("export job not found")
	}
	if err != nil {
		return nil, apierr.Internal("failed to load the export job").Wrap(err)
	}
	return job, nil
}

// List returns a page of the caller tenant's export jobs.
func (s *ExportService) List(ctx context.Context, scope tenancy.Scope, page, pageSize int) ([]*models.ExportJob, int64, error) {
	if !scope.Valid() {
		return nil, 0, apierr.Unauthenticated("authentication is required")
	}
	jobs, total, err := s.jobs.ListByTenant(ctx, scope.TenantID, page, pageSize)
	if err != nil {
		return nil, 0, apierr.Internal("failed to list export jobs").Wrap(err)
	}
	return jobs, total, nil
}

// Download is a completed export's short-lived download URL.
type Download struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	RowCount  int       `json:"row_count"`
	SizeBytes int64     `json:"size_bytes"`
}

// DownloadURL mints a fresh, expiring URL for a completed export.
//
// The URL is generated per request rather than stored on the job, so it can
// never outlive its TTL, revoking access is a matter of waiting rather than
// of cleaning up a database column, and a leaked API response from an hour
// ago is not a usable capability.
func (s *ExportService) DownloadURL(ctx context.Context, scope tenancy.Scope, id string) (*Download, error) {
	job, err := s.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}

	switch job.Status {
	case models.ExportStatusCompleted:
	case models.ExportStatusFailed:
		return nil, apierr.Conflict("this export failed and has no downloadable result")
	default:
		return nil, apierr.Conflict("this export is not ready yet").
			WithDetails(map[string]any{"status": string(job.Status)})
	}

	if job.ObjectKey == "" {
		return nil, apierr.Internal("the export is marked complete but has no stored object")
	}

	url, err := s.store.PresignGet(ctx, job.ObjectKey, s.downloadTTL)
	if err != nil {
		return nil, apierr.Internal("failed to create a download URL").Wrap(err)
	}

	return &Download{
		URL:       url,
		ExpiresAt: time.Now().Add(s.downloadTTL),
		RowCount:  job.RowCount,
		SizeBytes: job.SizeBytes,
	}, nil
}

// QueueDepth reports how many exports are waiting platform-wide. Surfaced
// on /health so a CloudWatch alarm can fire on a growing backlog.
func (s *ExportService) QueueDepth(ctx context.Context) (int64, error) {
	return s.jobs.CountQueued(ctx)
}

// MaxRows is the per-export row cap, echoed to clients so they know when a
// result was truncated and should be split into narrower time windows.
func (s *ExportService) MaxRows() int { return s.maxRows }

// Backend names the configured object store, for health output.
func (s *ExportService) Backend() string { return s.store.Backend() }

func isHTTPMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}
