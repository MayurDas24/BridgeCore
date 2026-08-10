package service

import (
	"context"
	"time"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
)

// UsageStore is the subset of usage persistence operations UsageService
// depends on.
type UsageStore interface {
	Create(ctx context.Context, u *models.UsageLog) error
	ListByTenant(ctx context.Context, tenantID, endpointFilter, methodFilter string, from, to *time.Time, page, pageSize int) ([]*models.UsageLog, int64, error)
	SummaryByTenant(ctx context.Context, tenantID string, from, to *time.Time) ([]repository.EndpointSummary, error)
}

// UsageService records per-request metering data and serves aggregated
// usage queries.
type UsageService struct {
	repo UsageStore
}

func NewUsageService(repo UsageStore) *UsageService {
	return &UsageService{repo: repo}
}

type RecordUsageInput struct {
	TenantID   *string
	Endpoint   string
	Method     string
	StatusCode int
	LatencyMS  int
	RequestID  string
}

// Record persists a single request's metering data. Called from the usage
// metering middleware after every request completes.
func (s *UsageService) Record(ctx context.Context, in RecordUsageInput) error {
	return s.repo.Create(ctx, &models.UsageLog{
		TenantID:   in.TenantID,
		Endpoint:   in.Endpoint,
		Method:     in.Method,
		StatusCode: in.StatusCode,
		LatencyMS:  in.LatencyMS,
		RequestID:  in.RequestID,
	})
}

func (s *UsageService) List(ctx context.Context, tenantID, endpoint, method string, from, to *time.Time, page, pageSize int) ([]*models.UsageLog, int64, error) {
	return s.repo.ListByTenant(ctx, tenantID, endpoint, method, from, to, page, pageSize)
}

func (s *UsageService) Summary(ctx context.Context, tenantID string, from, to *time.Time) ([]repository.EndpointSummary, error) {
	return s.repo.SummaryByTenant(ctx, tenantID, from, to)
}
